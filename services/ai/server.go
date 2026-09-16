package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Ai (proto/onyx/v1/ai.proto).
//
// The advisor runs deterministic heuristics in-process (no network, no
// telemetry). When a model plane is configured — the shared OmniRoute gateway
// (`OMNIROUTE_BASE_URL` + key), or the documented BYO-key hook
// (`AI_PROVIDER`/`AI_API_KEY`/`AI_MODEL`) — the same findings are rewritten as
// advice by that model, with the local summary as the fallback whenever the
// call fails (docs/design/11 §6.5).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedAiServer

	// provider is the raw BYO-key hook, kept for the startup log: it says which
	// documented hook a deployment set, independent of resolution order.
	provider string
	// model is the AI plane this process will actually call, or the absence of
	// one.
	model modelConfig
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.AiServer = (*server)(nil)

func newServer() *server {
	s := &server{
		provider: strings.TrimSpace(os.Getenv("AI_PROVIDER")),
		model:    readModelConfig(),
	}
	if s.model.configured() {
		// Name and source only — the base URL and the key are never logged.
		slog.Info("advisor model plane configured", "source", s.model.source, "model", s.model.model)
	} else {
		slog.Info("no model plane configured — local heuristics only")
	}
	return s
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

// AnalyzeStorage turns pool telemetry into findings + a 0..1 health score.
func (s *server) AnalyzeStorage(ctx context.Context, req *onyxv1.AnalyzeStorageRequest) (*onyxv1.AnalyzeStorageResponse, error) {
	if len(req.GetPools()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one pool is required")
	}

	var findings []*onyxv1.StorageFinding
	score := 1.0
	for _, p := range req.GetPools() {
		if p.GetTotalBytes() <= 0 {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "warning",
				Code:     "unknown_pool_capacity",
				Summary:  "Pool capacity unknown",
				Detail:   "onyx-" + p.GetPool() + " reported no capacity; the advisor cannot estimate runway.",
				Action:   "Confirm the pool is mounted and onyx-storaged is healthy.",
			})
			score *= 0.8
			continue
		}
		freeRatio := float64(p.GetFreeBytes()) / float64(p.GetTotalBytes())
		switch {
		case freeRatio < 0.10:
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "critical",
				Code:     "low_free_space",
				Summary:  "Pool critically low on free space",
				Detail:   "onyx-" + p.GetPool() + " has under 10% free — writes may start failing.",
				Action:   "Add capacity, enable compression, or move cold data to the object store / cloud tier.",
			})
			score *= 0.4
		case freeRatio < 0.25:
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "warning",
				Code:     "low_free_space",
				Summary:  "Pool running low on free space",
				Detail:   "onyx-" + p.GetPool() + " is below 25% free.",
				Action:   "Plan capacity, review snapshots, or tier cold data to the cloud.",
			})
			score *= 0.7
		}
		if p.GetSnapshotCount() == 0 {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "info",
				Code:     "snapshot_cadence",
				Summary:  "No snapshots on pool",
				Detail:   "onyx-" + p.GetPool() + " has no snapshots; there is no point-in-time safety net.",
				Action:   "Enable onyx-snapd with a daily snapshot schedule.",
			})
			score *= 0.95
		}
		if p.GetScrubStatus() != "" && !strings.EqualFold(p.GetScrubStatus(), "healthy") {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "warning",
				Code:     "scrub_health",
				Summary:  "Scrub did not complete cleanly",
				Detail:   "onyx-" + p.GetPool() + " last scrub status: " + p.GetScrubStatus(),
				Action:   "Run a scrub and inspect the pool log.",
			})
			score *= 0.7
		}
	}

	return &onyxv1.AnalyzeStorageResponse{
		Findings:  findings,
		Health:    score,
		Narrative: s.narrate(ctx, "storage", findings),
	}, nil
}

// AnalyzeBackups reviews the onyx-backupd report (docs/design/11 §6.2).
func (s *server) AnalyzeBackups(ctx context.Context, req *onyxv1.AnalyzeBackupsRequest) (*onyxv1.AnalyzeBackupsResponse, error) {
	if req.GetReport() == nil {
		return nil, status.Error(codes.InvalidArgument, "report is required")
	}

	var findings []*onyxv1.StorageFinding
	for _, j := range req.GetReport().GetJobs() {
		if j.GetTotalRuns() == 0 {
			continue
		}
		if j.GetFailedRuns() > 0 {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "warning",
				Code:     "backup_failures",
				Summary:  "Backup job has failures",
				Detail:   j.GetName() + " failed " + strconv.FormatInt(int64(j.GetFailedRuns()), 10) + " of " + strconv.FormatInt(int64(j.GetTotalRuns()), 10) + " runs.",
				Action:   "Review the job's last failed run and the target connectivity.",
			})
		}
		if j.GetSuccessRatio() > 0 && j.GetSuccessRatio() < 0.8 {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "critical",
				Code:     "low_success_ratio",
				Summary:  "Backup success ratio is low",
				Detail:   j.GetName() + " succeeds less than 80% of the time.",
				Action:   "Investigate target reliability; consider a second target for redundancy.",
			})
		}
		if j.GetTrend() < -0.2 {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "warning",
				Code:     "degrading_trend",
				Summary:  "Backup reliability is degrading",
				Detail:   j.GetName() + " has a downward success trend over recent history.",
				Action:   "Check the target and retention before the next planned restore.",
			})
		}
		if j.GetEstimatedRpoSeconds() > 86400 {
			findings = append(findings, &onyxv1.StorageFinding{
				Severity: "warning",
				Code:     "stale_recovery_point",
				Summary:  "Recovery point is stale",
				Detail:   j.GetName() + " newest successful backup is older than 24h.",
				Action:   "Check the schedule and whether runs are being skipped.",
			})
		}
	}

	return &onyxv1.AnalyzeBackupsResponse{
		Findings:  findings,
		Health:    req.GetReport().GetOverallHealth(),
		Narrative: s.narrate(ctx, "backup", findings),
	}, nil
}

// narrate is the one place the two sources are ordered: the local sentence is
// computed first and is always the answer unless a configured model plane
// improves on it. A model that is unreachable, slow, rate-limited or inventing
// nothing is not an error the caller has to handle — the findings and the score
// are the substance of the response, and the narrative is commentary on them.
func (s *server) narrate(ctx context.Context, kind string, findings []*onyxv1.StorageFinding) string {
	local := s.narrative(kind, findings)
	if !s.model.configured() || len(findings) == 0 {
		return local
	}

	advice, err := s.model.advise(ctx, kind, findings)
	if err != nil {
		// Never fatal, always logged: an operator needs to know that the advice
		// is local rather than newly missing.
		slog.Warn("advisor: using the local summary", "kind", kind, "source", s.model.source, "error", err)
		return local
	}
	return advice
}

// narrative summarizes findings in plain language, deterministically and
// without a network call — the local-first answer, and the fallback whenever
// the model plane cannot be reached (docs/design/07 §Privacy).
func (s *server) narrative(kind string, findings []*onyxv1.StorageFinding) string {
	if len(findings) == 0 {
		return "No " + kind + " concerns detected."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s finding(s): ", len(findings), kind)
	for i, f := range findings {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(f.GetSummary())
	}
	return b.String()
}
