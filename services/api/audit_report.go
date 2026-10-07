package main

import (
	"context"
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// The audit report (docs/design/10 Quartz, "audit reporting"). The trail itself
// already exists — `access_events` in onyx-core, written when a grant changes
// and when onyx-davd refuses a request (docs/design/08#2) — and
// GET /api/v1/audit/access returns it as a raw list. What an operator actually
// asks is a question *about* the trail: how many refusals, which share is being
// refused the most, which names are hitting the wall. This endpoint answers that
// from the same rows rather than making the console aggregate a page of JSON.
//
// It is a report over the newest N events, not a scan of every row ever
// written: the trail is bounded in core (maxAccessEventLimit), so the report
// says which window it covered instead of pretending to be exhaustive.

// The report summarizes one core page at most, which is also core's published
// maximum, so a report never asks for more than the trail can return.
const (
	defaultAuditReportSize = 500
	maxAuditReportSize     = 500
)

type auditTotals struct {
	Grant  int `json:"grant"`
	Revoke int `json:"revoke"`
	Denied int `json:"denied"`
}

type auditShareRow struct {
	Share  string `json:"share"`
	Grant  int    `json:"grant"`
	Revoke int    `json:"revoke"`
	Denied int    `json:"denied"`
}

type auditUserRow struct {
	Username string `json:"username"`
	Denied   int    `json:"denied"`
}

type auditReport struct {
	GeneratedAt string          `json:"generated_at"`
	SampleSize  int             `json:"sample_size"`
	Events      int             `json:"events"`
	FirstTS     string          `json:"first_ts,omitempty"`
	LastTS      string          `json:"last_ts,omitempty"`
	Totals      auditTotals     `json:"totals"`
	Shares      []auditShareRow `json:"shares"`
	DeniedUsers []auditUserRow  `json:"denied_users"`
}

// handleAuditReport serves GET /api/v1/audit — the report, or the same events
// as CSV when `format=csv` is asked for. `share` narrows it to one share, which
// is what the Shares page wants.
func (s *server) handleAuditReport(w http.ResponseWriter, r *http.Request) {
	sampleSize := defaultAuditReportSize
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "limit must be a positive integer"})
			return
		}
		if n > maxAuditReportSize {
			n = maxAuditReportSize
		}
		sampleSize = n
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format != "" && format != "json" && format != "csv" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "format must be json or csv"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	resp, err := s.core.ListAccessEvents(ctx, &onyxv1.ListAccessEventsRequest{
		Share: strings.TrimSpace(r.URL.Query().Get("share")),
		Limit: int32(sampleSize),
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	events := resp.GetEvents()

	if format == "csv" {
		writeAuditCSV(w, events)
		return
	}
	writeJSON(w, http.StatusOK, buildAuditReport(events, sampleSize))
}

// buildAuditReport folds the events into counts. The rows arrive newest-first
// from core, so the first and last seen give the window without a second query.
func buildAuditReport(events []*onyxv1.AccessEvent, sampleSize int) auditReport {
	report := auditReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		SampleSize:  sampleSize,
		Events:      len(events),
		Shares:      []auditShareRow{},
		DeniedUsers: []auditUserRow{},
	}

	byShare := map[string]*auditShareRow{}
	deniedByUser := map[string]int{}

	for _, e := range events {
		switch e.GetKind() {
		case "grant":
			report.Totals.Grant++
		case "revoke":
			report.Totals.Revoke++
		case "denied":
			report.Totals.Denied++
			// A denial names the person refused; a name with no denials has no
			// row here, which is the point of the report.
			if user := strings.TrimSpace(e.GetUsername()); user != "" {
				deniedByUser[user]++
			}
		}

		share := strings.TrimSpace(e.GetShare())
		if share == "" {
			continue
		}
		row, ok := byShare[share]
		if !ok {
			row = &auditShareRow{Share: share}
			byShare[share] = row
		}
		switch e.GetKind() {
		case "grant":
			row.Grant++
		case "revoke":
			row.Revoke++
		case "denied":
			row.Denied++
		}
	}

	if len(events) > 0 {
		report.LastTS = events[0].GetTs()
		report.FirstTS = events[len(events)-1].GetTs()
	}

	for _, row := range byShare {
		report.Shares = append(report.Shares, *row)
	}
	// Most-refused share first — the reason to open a report at all — then by
	// name so two shares with equal counts do not reorder between calls.
	sort.Slice(report.Shares, func(i, j int) bool {
		a, b := report.Shares[i], report.Shares[j]
		if a.Denied != b.Denied {
			return a.Denied > b.Denied
		}
		return a.Share < b.Share
	})

	for user, count := range deniedByUser {
		report.DeniedUsers = append(report.DeniedUsers, auditUserRow{Username: user, Denied: count})
	}
	sort.Slice(report.DeniedUsers, func(i, j int) bool {
		a, b := report.DeniedUsers[i], report.DeniedUsers[j]
		if a.Denied != b.Denied {
			return a.Denied > b.Denied
		}
		return a.Username < b.Username
	})

	return report
}

// writeAuditCSV renders the same sample as CSV, for the export the design doc
// promises (docs/design/07#9). The columns are the trail's own, in the order the
// table stores them, so a reader can diff it against the database.
func writeAuditCSV(w http.ResponseWriter, events []*onyxv1.AccessEvent) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="onyx-audit-access.csv"`)
	writer := csv.NewWriter(w)
	defer writer.Flush()

	_ = writer.Write([]string{"id", "ts", "kind", "share", "username", "mode", "actor", "detail"})
	for _, e := range events {
		_ = writer.Write([]string{
			strconv.FormatInt(e.GetId(), 10),
			e.GetTs(),
			e.GetKind(),
			e.GetShare(),
			e.GetUsername(),
			e.GetMode(),
			e.GetActor(),
			e.GetDetail(),
		})
	}
}
