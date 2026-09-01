package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Backupd (proto/onyx/v1/backupd.proto).
// Jobs and runs live in memory at v0.1; the execution engine + persistence
// land with v0.3 (docs/design/11 §6.2).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedBackupdServer

	mu   sync.Mutex
	jobs map[string]*onyxv1.BackupJob
	runs map[string][]*onyxv1.BackupRun // job_id → history (newest last)
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.BackupdServer = (*server)(nil)

func newServer() *server {
	return &server{jobs: map[string]*onyxv1.BackupJob{}, runs: map[string][]*onyxv1.BackupRun{}}
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
	}
	return &onyxv1.DeleteBackupJobResponse{Deleted: ok}, nil
}

// RunBackup executes a job. At v0.1 the engine simulates the run so the
// contract, history, retention and report pipeline are exercisable end to end;
// the real copy engine (snapshot → target via onyx-snapd/onyx-objectstore)
// lands with v0.3.
func (s *server) RunBackup(_ context.Context, req *onyxv1.RunBackupRequest) (*onyxv1.BackupRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[req.GetJobId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "job not found")
	}
	now := time.Now().UTC()
	run := &onyxv1.BackupRun{
		Id:        fmt.Sprintf("run-%d", now.UnixNano()),
		JobId:     job.Id,
		Status:    "succeeded",
		StartedAt: now.Format(time.RFC3339),
	}
	if job.SnapshotBefore {
		// Snapshot hand-off to onyx-snapd happens here in v0.3.
	}
	s.runs[job.Id] = append(s.runs[job.Id], run)
	s.applyRetention(job)
	return run, nil
}

func (s *server) applyRetention(job *onyxv1.BackupJob) {
	runs := s.runs[job.Id]
	if job.Retention <= 0 || len(runs) <= int(job.Retention) {
		return
	}
	s.runs[job.Id] = runs[len(runs)-int(job.Retention):]
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
