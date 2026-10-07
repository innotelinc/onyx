package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// sampleAuditEvents is newest-first, the order core returns.
func sampleAuditEvents() []*onyxv1.AccessEvent {
	return []*onyxv1.AccessEvent{
		{Id: 5, Ts: "2026-10-07T12:00:00Z", Kind: "denied", Share: "media", Username: "dana", Detail: "GET not granted"},
		{Id: 4, Ts: "2026-10-07T11:00:00Z", Kind: "denied", Share: "media", Username: "dana"},
		{Id: 3, Ts: "2026-10-07T10:00:00Z", Kind: "denied", Share: "docs", Username: "erin"},
		{Id: 2, Ts: "2026-10-07T09:00:00Z", Kind: "grant", Share: "media", Username: "dana", Mode: "read", Actor: "ada"},
		{Id: 1, Ts: "2026-10-07T08:00:00Z", Kind: "revoke", Share: "docs", Username: "frank", Actor: "console"},
	}
}

// The report answers the question the raw list does not: how many, and which
// share or name is worst. The most-refused share leads, so opening the report
// shows what to fix first.
func TestAuditReportAggregatesTheTrail(t *testing.T) {
	s, _, core, _ := accessFixture(t)
	core.events = sampleAuditEvents()

	code, decoded, text := getJSON(t, s, http.MethodGet, "/api/v1/audit", "", s.handleAuditReport)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if core.eventReq.GetLimit() != defaultAuditReportSize {
		t.Errorf("core saw limit=%d, want the default sample size", core.eventReq.GetLimit())
	}

	totals, _ := decoded["totals"].(map[string]any)
	if totals["grant"] != float64(1) || totals["revoke"] != float64(1) || totals["denied"] != float64(3) {
		t.Errorf("totals = %v, want grant 1 revoke 1 denied 3", totals)
	}

	shares, _ := decoded["shares"].([]any)
	if len(shares) != 2 {
		t.Fatalf("shares = %v, want two", decoded["shares"])
	}
	first, _ := shares[0].(map[string]any)
	if first["share"] != "media" || first["denied"] != float64(2) {
		t.Errorf("first share = %v, want media with 2 denials (the worst first)", first)
	}

	users, _ := decoded["denied_users"].([]any)
	if len(users) != 2 {
		t.Fatalf("denied_users = %v, want two", decoded["denied_users"])
	}
	top, _ := users[0].(map[string]any)
	if top["username"] != "dana" || top["denied"] != float64(2) {
		t.Errorf("top denied user = %v, want dana 2", top)
	}

	// The window is the sample's own first and last, newest first from core.
	if decoded["last_ts"] != "2026-10-07T12:00:00Z" || decoded["first_ts"] != "2026-10-07T08:00:00Z" {
		t.Errorf("window = %v..%v", decoded["first_ts"], decoded["last_ts"])
	}
	if decoded["events"] != float64(5) {
		t.Errorf("events = %v, want 5", decoded["events"])
	}
}

// An empty trail is a report of nothing, not an error, and the lists are [] so
// the console does not special-case null.
func TestAuditReportOnAnEmptyTrail(t *testing.T) {
	s, _, core, _ := accessFixture(t)
	core.events = nil

	code, decoded, text := getJSON(t, s, http.MethodGet, "/api/v1/audit", "", s.handleAuditReport)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if decoded["events"] != float64(0) {
		t.Errorf("events = %v, want 0", decoded["events"])
	}
	if shares, ok := decoded["shares"].([]any); !ok || len(shares) != 0 {
		t.Errorf("shares = %v, want []", decoded["shares"])
	}
	if _, ok := decoded["first_ts"]; ok {
		t.Error("an empty sample has no window; first_ts must be omitted")
	}
}

// The export is the rows themselves: the trail's own columns, in table order,
// so a reader can diff it against the database.
func TestAuditReportCSVExportsTheEvents(t *testing.T) {
	s, _, core, _ := accessFixture(t)
	core.events = sampleAuditEvents()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/audit?format=csv", nil)
	rec := httptest.NewRecorder()
	s.handleAuditReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("csv has %d lines, want a header + 5 rows:\n%s", len(lines), rec.Body.String())
	}
	if lines[0] != "id,ts,kind,share,username,mode,actor,detail" {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "denied,media,dana") {
		t.Errorf("first row = %q, want the newest denial", lines[1])
	}
}

// Input is validated before core is asked anything, and the filter passes
// through — the Shares page asks for one share at a time.
func TestAuditReportValidatesAndFilters(t *testing.T) {
	s, _, core, _ := accessFixture(t)
	core.events = sampleAuditEvents()

	if code, _, text := getJSON(t, s, http.MethodGet, "/api/v1/audit?share=media&limit=25", "", s.handleAuditReport); code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if core.eventReq.GetShare() != "media" || core.eventReq.GetLimit() != 25 {
		t.Errorf("core saw share=%q limit=%d", core.eventReq.GetShare(), core.eventReq.GetLimit())
	}

	for _, target := range []string{"/api/v1/audit?limit=0", "/api/v1/audit?limit=abc", "/api/v1/audit?format=xml"} {
		if code, _, _ := getJSON(t, s, http.MethodGet, target, "", s.handleAuditReport); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, code)
		}
	}

	// The sample size is capped, so one report can never ask core for more than
	// it can return.
	core.eventReq = nil
	if code, _, text := getJSON(t, s, http.MethodGet, "/api/v1/audit?limit=100000", "", s.handleAuditReport); code != http.StatusOK {
		t.Fatalf("status = %d (%s)", code, text)
	}
	if core.eventReq.GetLimit() != maxAuditReportSize {
		t.Errorf("core saw limit=%d, want the cap %d", core.eventReq.GetLimit(), maxAuditReportSize)
	}
}
