package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// This file is the gateway's v0.4 platform surface (docs/design/11 §6.3-§6.6):
// apps/containers (onyx-appd), virtual machines (onyx-vmm), object storage
// (onyx-objectstore) and the AI advisor (onyx-ai). Each route forwards to the
// daemon that owns the resource and maps its gRPC status onto the error
// envelope, exactly like the storage and backup routes do. Nothing here
// touches the filesystem, and every list route degrades to a 503 when the
// owning daemon is not deployed — never a fabricated empty result.

// queryBool reads an optional boolean query parameter, applying def when the
// parameter is absent. An unparsable value is reported as an error so a typo
// such as ?force=yes is not silently treated as false.
func queryBool(r *http.Request, name string, def bool) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	return strconv.ParseBool(raw)
}

func badQueryBool(w http.ResponseWriter, name string) {
	writeEnvelope(w, http.StatusBadRequest, apiError{
		Code:    "invalid_argument",
		Message: name + " must be a boolean (true or false)",
	})
}

// --- Apps and containers (onyx-appd, docs/design/09) ---

// handleApps serves GET /api/v1/apps and GET /api/v1/app-store. onyx-appd keeps
// one catalog with a per-app status, so the store view and the installed view
// are the same list filtered by status: `?status=installed` is the installed
// apps, an empty status is the whole store.
func (s *server) handleApps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.appd.ListApps(ctx, &onyxv1.ListAppsRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	want := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	if want == "" {
		writeJSON(w, http.StatusOK, protoMessage(resp))
		return
	}
	if want != "installed" && want != "not_installed" && want != "updating" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "status must be installed, not_installed or updating"})
		return
	}
	out := &onyxv1.ListAppsResponse{}
	for _, a := range resp.GetApps() {
		if a.GetStatus() == want {
			out.Apps = append(out.Apps, a)
		}
	}
	writeJSON(w, http.StatusOK, protoMessage(out))
}

func (s *server) handleInstallApp(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AppID string `json:"app_id"`
		// Optional pinned version; empty = the catalog's current version.
		Version string `json:"version"`
		// Per-installation settings (ports, paths) injected into the manifest.
		Config map[string]string `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.AppID) == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "app_id is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	app, err := s.appd.InstallApp(ctx, &onyxv1.InstallAppRequest{AppId: body.AppID, Version: body.Version, Config: body.Config})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(app))
}

func (s *server) handleUninstallApp(w http.ResponseWriter, r *http.Request) {
	purge, err := queryBool(r, "purge_data", false)
	if err != nil {
		badQueryBool(w, "purge_data")
		return
	}
	force, err := queryBool(r, "force", false)
	if err != nil {
		badQueryBool(w, "force")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	resp, err := s.appd.UninstallApp(ctx, &onyxv1.UninstallAppRequest{AppId: r.PathValue("id"), PurgeData: purge, Force: force})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleContainers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.appd.ListContainers(ctx, &onyxv1.ListContainersRequest{AppId: r.URL.Query().Get("app_id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleContainerStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := s.appd.StartContainer(ctx, &onyxv1.StartContainerRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleContainerStop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := s.appd.StopContainer(ctx, &onyxv1.StopContainerRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleContainerRestart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := s.appd.RestartContainer(ctx, &onyxv1.RestartContainerRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// --- Virtual machines (onyx-vmm, docs/design/11 §6.3) ---

func (s *server) handleVMs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.vmm.ListVMs(ctx, &onyxv1.ListVMsRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Vcpus    int32  `json:"vcpus"`
		MemoryMB int64  `json:"memory_mb"`
		DiskMB   int64  `json:"disk_mb"`
		OS       string `json:"os"`
		ISO      string `json:"iso"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "name is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	vm, err := s.vmm.CreateVM(ctx, &onyxv1.CreateVMRequest{
		Name: body.Name, Vcpus: body.Vcpus, MemoryMb: body.MemoryMB, DiskMb: body.DiskMB, Os: body.OS, Iso: body.ISO,
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(vm))
}

func (s *server) handleVMStart(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	vm, err := s.vmm.StartVM(ctx, &onyxv1.StartVMRequest{Id: r.PathValue("id")})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(vm))
}

func (s *server) handleVMStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Graceful *bool `json:"graceful"`
	}
	// A body is optional here; only a malformed one is an error.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	graceful := true
	if body.Graceful != nil {
		graceful = *body.Graceful
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	vm, err := s.vmm.StopVM(ctx, &onyxv1.StopVMRequest{Id: r.PathValue("id"), Graceful: graceful})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(vm))
}

func (s *server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	// Deleting the disk image is irreversible, so it is opt-in.
	deleteDisk, err := queryBool(r, "delete_disk", false)
	if err != nil {
		badQueryBool(w, "delete_disk")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	resp, err := s.vmm.DeleteVM(ctx, &onyxv1.DeleteVMRequest{Id: r.PathValue("id"), DeleteDisk: deleteDisk})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// --- Object storage (onyx-objectstore, docs/design/11 §6.6) ---

// parseBucketTier maps the tier names the UI sends onto the contract, treating
// an empty value as LOCAL — the default for a new bucket on the pool. The
// bucket tiers are named without a prefix in objectstore.proto (LOCAL, CLOUD,
// TIERED), so the friendly names and the enum names are the same words; the
// real bucket tiers are 1..3, never the zero value.
func parseBucketTier(raw string) (onyxv1.BucketTier, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "local":
		return onyxv1.BucketTier_LOCAL, true
	case "cloud":
		return onyxv1.BucketTier_CLOUD, true
	case "tiered":
		return onyxv1.BucketTier_TIERED, true
	}
	return 0, false
}

func (s *server) handleBuckets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp, err := s.objects.ListBuckets(ctx, &onyxv1.ListBucketsRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Tier        string `json:"tier"`
		CloudTarget string `json:"cloud_target"`
		// EvictAfterDays applies to TIERED buckets (0 = keep every hot copy).
		EvictAfterDays int32 `json:"evict_after_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "invalid JSON body: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "name is required"})
		return
	}
	tier, ok := parseBucketTier(body.Tier)
	if !ok {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "tier must be local, cloud or tiered"})
		return
	}
	// A cloud-backed bucket without a target would silently write locally, so
	// the requirement is enforced here rather than discovered later.
	if tier != onyxv1.BucketTier_LOCAL && strings.TrimSpace(body.CloudTarget) == "" {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: "cloud_target is required for a cloud or tiered bucket"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	bucket, err := s.objects.CreateBucket(ctx, &onyxv1.CreateBucketRequest{
		Name: body.Name, Tier: tier, CloudTarget: body.CloudTarget, EvictAfterDays: body.EvictAfterDays,
	})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, protoMessage(bucket))
}

// handleSyncBucket serves POST /api/v1/buckets/{name}/sync — mirror a cloud or
// tiered bucket into its target and, when asked, release local copies that the
// cloud has been verified to hold. Eviction destroys local data if it is wrong,
// so it is opt-in per call (`?evict=1`) and refused outright by the data plane
// when the whole-bucket check does not pass.
func (s *server) handleSyncBucket(w http.ResponseWriter, r *http.Request) {
	evict, err := queryBool(r, "evict", false)
	if err != nil {
		badQueryBool(w, "evict")
		return
	}
	// A sync moves real data, so the request is allowed the full upload window
	// rather than the usual seconds: a large bucket's first sync is the slow one.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	resp, err := s.objects.SyncBucket(ctx, &onyxv1.SyncBucketRequest{Name: r.PathValue("name"), Evict: evict})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

func (s *server) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	// Purging a non-empty bucket destroys objects, so it is opt-in.
	force, err := queryBool(r, "force", false)
	if err != nil {
		badQueryBool(w, "force")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	resp, err := s.objects.DeleteBucket(ctx, &onyxv1.DeleteBucketRequest{Name: r.PathValue("name"), Force: force})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// --- AI advisor (onyx-ai, docs/design/11 §6.5) ---

// unknownSnapshotCount marks a pool whose snapshot cadence the advisor could
// not learn because onyx-snapd was unreachable. It is a negative sentinel on
// purpose: the advisor reports "no snapshots" only for a genuine zero, so an
// outage produces no finding rather than a false one.
const unknownSnapshotCount int32 = -1

// handleAdvisor serves GET /api/v1/ai/advisor. The advisor is only as good as
// its telemetry, so the gateway assembles the observation the data plane
// cannot: capacity from onyx-storaged (via core), snapshot cadence from
// onyx-snapd, scrub status from the schedule store. Missing telemetry is
// reported in `warnings` instead of being silently assumed.
func (s *server) handleAdvisor(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	pools, err := s.core.ListPools(ctx, &onyxv1.ListPoolsRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	only := strings.TrimSpace(r.URL.Query().Get("pool"))
	telemetry := []*onyxv1.PoolTelemetry{}
	found := only == ""
	for _, p := range pools.GetPools() {
		if only != "" && p.GetName() != only {
			continue
		}
		found = true
		telemetry = append(telemetry, &onyxv1.PoolTelemetry{
			Pool:       p.GetName(),
			TotalBytes: int64(p.GetTotalBytes()),
			FreeBytes:  int64(p.GetTotalBytes() - p.GetUsedBytes()),
		})
	}
	if !found {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: "pool not found: " + only})
		return
	}
	if len(telemetry) == 0 {
		// No pools yet: there is nothing to advise on, and onyx-ai rejects an
		// empty request. Answer plainly rather than borrowing an error.
		writeJSON(w, http.StatusOK, map[string]any{
			"findings":  []any{},
			"health":    1.0,
			"narrative": "No pools configured yet.",
			"pools":     []string{},
			"warnings":  []string{},
		})
		return
	}

	var warnings []string
	snapshots, err := s.snapshotCountsByPool(ctx)
	if err != nil {
		warnings = append(warnings, "onyx-snapd unavailable: snapshot cadence unknown ("+err.Error()+")")
		for _, t := range telemetry {
			t.SnapshotCount = unknownSnapshotCount
		}
	} else {
		for _, t := range telemetry {
			t.SnapshotCount = snapshots[t.GetPool()]
		}
	}
	for _, t := range telemetry {
		last, status := s.scrubStatus(t.GetPool())
		t.LastScrubAt = last
		t.ScrubStatus = status
	}

	resp, err := s.ai.AnalyzeStorage(ctx, &onyxv1.AnalyzeStorageRequest{Pools: telemetry})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	out := protoMessage(resp)
	out["pools"] = poolNames(telemetry)
	out["warnings"] = warningsOrEmpty(warnings)
	writeJSON(w, http.StatusOK, out)
}

// handleBackupAdvice serves GET /api/v1/ai/backup-advice: the backup report
// from onyx-backupd, reviewed by the advisor. Pulling the report here rather
// than accepting it from the client keeps the analysis grounded in real runs.
func (s *server) handleBackupAdvice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	report, err := s.backupd.GetBackupReport(ctx, &onyxv1.GetBackupReportRequest{})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	resp, err := s.ai.AnalyzeBackups(ctx, &onyxv1.AnalyzeBackupsRequest{Report: report})
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, protoMessage(resp))
}

// snapshotCountsByPool learns the current snapshot count per pool in a single
// onyx-snapd call and buckets the result by the pool each snapshot names.
// Pools with no snapshots are absent from the map, which callers read as zero.
func (s *server) snapshotCountsByPool(ctx context.Context) (map[string]int32, error) {
	resp, err := s.snapd.ListSnapshots(ctx, &onyxv1.ListSnapshotsRequest{})
	if err != nil {
		return nil, err
	}
	counts := map[string]int32{}
	for _, snap := range resp.GetSnapshots() {
		counts[snap.GetPool()]++
	}
	return counts, nil
}

// scrubStatus reports the last scrub of one pool as ("<timestamp>", "<status>").
// A pool with no schedule reads as "never" / "not-configured" so the advisor
// distinguishes "no scrub yet" from "scrub failed".
func (s *server) scrubStatus(pool string) (string, string) {
	s.scrub.mu.RLock()
	defer s.scrub.mu.RUnlock()
	sched, ok := s.scrub.schedules[pool]
	if !ok {
		return "", "not-configured"
	}
	return sched.LastRun, sched.Status
}

func poolNames(telemetry []*onyxv1.PoolTelemetry) []string {
	names := make([]string, 0, len(telemetry))
	for _, t := range telemetry {
		names = append(names, t.GetPool())
	}
	return names
}

// warningsOrEmpty keeps the JSON shape stable: a caller always gets an array,
// never null.
func warningsOrEmpty(warnings []string) []string {
	if warnings == nil {
		return []string{}
	}
	return warnings
}
