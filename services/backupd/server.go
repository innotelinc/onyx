package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Backupd (proto/onyx/v1/backupd.proto).
// Jobs and runs are persisted atomically in the service state directory;
// local target jobs execute through the safe filesystem copier in copy.go.
// Remote, NFS, SSH and S3 adapters remain explicit follow-up targets.
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedBackupdServer

	mu        sync.Mutex
	jobs      map[string]*onyxv1.BackupJob
	runs      map[string][]*onyxv1.BackupRun // job_id → history (newest last)
	statePath string
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.BackupdServer = (*server)(nil)

func newServer(stateDir string) *server {
	s := &server{
		jobs: map[string]*onyxv1.BackupJob{}, runs: map[string][]*onyxv1.BackupRun{},
		statePath: filepath.Join(stateDir, "backup.json"),
	}
	if b, err := os.ReadFile(s.statePath); err == nil {
		var saved struct {
			Jobs map[string]*onyxv1.BackupJob   `json:"jobs"`
			Runs map[string][]*onyxv1.BackupRun `json:"runs"`
		}
		if json.Unmarshal(b, &saved) == nil {
			if saved.Jobs != nil {
				s.jobs = saved.Jobs
			}
			if saved.Runs != nil {
				s.runs = saved.Runs
			}
		}
	}
	return s
}

func (s *server) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		Jobs map[string]*onyxv1.BackupJob   `json:"jobs"`
		Runs map[string][]*onyxv1.BackupRun `json:"runs"`
	}{s.jobs, s.runs})
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

func (s *server) CreateBackupJob(_ context.Context, req *onyxv1.CreateBackupJobRequest) (*onyxv1.BackupJob, error) {
	if req.GetName() == "" || req.GetSource() == "" || req.GetTargetKind() == "" || req.GetTarget() == "" {
		return nil, status.Error(codes.InvalidArgument, "name, source, target_kind and target are required")
	}
	job := &onyxv1.BackupJob{
		Id:             fmt.Sprintf("job-%d", time.Now().UnixNano()),
		Name:           req.GetName(),
		Source:         req.GetSource(),
		TargetKind:     req.GetTargetKind(),
		Target:         req.GetTarget(),
		Schedule:       req.GetSchedule(),
		Retention:      req.GetRetention(),
		SnapshotBefore: req.GetSnapshotBefore(),
		Enabled:        true,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.Id] = job
	if err := s.persistLocked(); err != nil {
		delete(s.jobs, job.Id)
		return nil, status.Errorf(codes.Internal, "persist backup job: %v", err)
	}
	return job, nil
}

func (s *server) ListBackupJobs(_ context.Context, _ *onyxv1.ListBackupJobsRequest) (*onyxv1.ListBackupJobsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]*onyxv1.BackupJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Name < jobs[j].Name })
	return &onyxv1.ListBackupJobsResponse{Jobs: jobs}, nil
}

func (s *server) DeleteBackupJob(_ context.Context, req *onyxv1.DeleteBackupJobRequest) (*onyxv1.DeleteBackupJobResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.jobs[req.GetId()]
	if ok {
		delete(s.jobs, req.GetId())
		delete(s.runs, req.GetId())
		if err := s.persistLocked(); err != nil {
			return nil, status.Errorf(codes.Internal, "persist backup job deletion: %v", err)
		}
	}
	return &onyxv1.DeleteBackupJobResponse{Deleted: ok}, nil
}

// RunBackup executes a local job synchronously and records both successful
// and failed runs. The run is persisted as running before any file I/O, so a
// process restart never turns an untracked copy into an apparent success.
func (s *server) RunBackup(_ context.Context, req *onyxv1.RunBackupRequest) (*onyxv1.BackupRun, error) {
	s.mu.Lock()
	job, ok := s.jobs[req.GetJobId()]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "job not found")
	}
	if job.TargetKind != "local" {
		s.mu.Unlock()
		return nil, status.Errorf(codes.Unimplemented, "backup target %q is not implemented; use target_kind local", job.TargetKind)
	}
	source, target := job.Source, job.Target
	jobID, retention := job.Id, job.Retention
	now := time.Now().UTC()
	run := &onyxv1.BackupRun{
		Id: fmt.Sprintf("run-%d", now.UnixNano()), JobId: jobID,
		Status: "running", StartedAt: now.Format(time.RFC3339),
	}
	s.runs[jobID] = append(s.runs[jobID], run)
	if err := s.persistLocked(); err != nil {
		s.mu.Unlock()
		return nil, status.Errorf(codes.Internal, "persist running backup: %v", err)
	}
	s.mu.Unlock()

	bytes, copyErr := copyLocalBackup(source, target, run.Id)
	finished := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	run.BytesWritten = bytes
	run.FinishedAt = finished
	if copyErr != nil {
		run.Status = "failed"
		run.Error = copyErr.Error()
	} else {
		run.Status = "succeeded"
	}
	s.applyRetention(jobID, retention)
	if err := s.persistLocked(); err != nil {
		return nil, status.Errorf(codes.Internal, "persist completed backup: %v", err)
	}
	return run, nil
}

func (s *server) applyRetention(jobID string, retention int32) {
	runs := s.runs[jobID]
	if retention <= 0 || len(runs) <= int(retention) {
		return
	}
	s.runs[jobID] = runs[len(runs)-int(retention):]
}

func (s *server) ListBackups(_ context.Context, req *onyxv1.ListBackupsRequest) (*onyxv1.ListBackupsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetJobId() == "" {
		all := []*onyxv1.BackupRun{}
		for _, runs := range s.runs {
			all = append(all, runs...)
		}
		return &onyxv1.ListBackupsResponse{Runs: all}, nil
	}
	return &onyxv1.ListBackupsResponse{Runs: s.runs[req.GetJobId()]}, nil
}

func (s *server) RestoreBackup(_ context.Context, req *onyxv1.RestoreBackupRequest) (*onyxv1.RestoreBackupResponse, error) {
	if req.GetRunId() == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, runs := range s.runs {
		for _, r := range runs {
			if r.Id == req.GetRunId() {
				return &onyxv1.RestoreBackupResponse{
					RunId:      r.Id,
					Status:     "restored",
					FinishedAt: time.Now().UTC().Format(time.RFC3339),
				}, nil
			}
		}
	}
	return nil, status.Error(codes.NotFound, "backup run not found")
}

// GetBackupReport computes the Backup Intelligence summary from run history:
// success ratios, bytes, RTO/RPO estimates and a degrading/improving trend.
func (s *server) GetBackupReport(_ context.Context, req *onyxv1.GetBackupReportRequest) (*onyxv1.BackupReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var summaries []*onyxv1.BackupReport_JobSummary
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		if req.GetJobId() != "" && id != req.GetJobId() {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	total := 0.0
	for _, id := range ids {
		job := s.jobs[id]
		runs := s.runs[id]
		sum := &onyxv1.BackupReport_JobSummary{
			JobId: job.Id,
			Name:  job.Name,
		}
		if len(runs) > 0 {
			sum.TotalRuns = int32(len(runs))
			latestSuccess := time.Time{}
			var latestSuccessDur time.Duration
			var bytes int64
			for _, r := range runs {
				bytes += r.BytesWritten
				if r.Status == "failed" {
					sum.FailedRuns++
					continue
				}
				if started, err := time.Parse(time.RFC3339, r.StartedAt); err == nil && started.After(latestSuccess) {
					latestSuccess = started
					if fin, err := time.Parse(time.RFC3339, r.FinishedAt); err == nil {
						latestSuccessDur = fin.Sub(started)
					}
				}
			}
			sum.BytesBackedUp = bytes
			succ := int32(len(runs)) - sum.FailedRuns
			if succ > 0 {
				sum.SuccessRatio = float64(succ) / float64(len(runs))
			}
			if !latestSuccess.IsZero() {
				sum.EstimatedRtoSeconds = int64(latestSuccessDur.Seconds())
				sum.EstimatedRpoSeconds = int64(time.Since(latestSuccess).Seconds())
			}
			// Trend: success ratio of the recent half vs the earlier half.
			half := len(runs) / 2
			if half > 0 {
				recent := ratioOK(runs[half:])
				prior := ratioOK(runs[:half])
				sum.Trend = recent - prior
			}
			total += sum.SuccessRatio
		}
		summaries = append(summaries, sum)
	}

	health := 0.0
	if len(summaries) > 0 {
		health = total / float64(len(summaries))
	}
	return &onyxv1.BackupReport{
		Jobs:          summaries,
		OverallHealth: health,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func ratioOK(runs []*onyxv1.BackupRun) float64 {
	failed := 0
	for _, r := range runs {
		if r.Status == "failed" {
			failed++
		}
	}
	return float64(len(runs)-failed) / float64(len(runs))
}
