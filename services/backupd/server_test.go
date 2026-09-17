package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

func TestRunBackupPersistsSuccessfulLocalRun(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "photo.jpg"), []byte("pixels"), 0o640); err != nil {
		t.Fatal(err)
	}

	s := newServer(filepath.Join(root, "state"))
	job, err := s.CreateBackupJob(context.Background(), &onyxv1.CreateBackupJobRequest{Name: "photos", Source: source, TargetKind: "local", Target: target, Retention: 2})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.RunBackup(context.Background(), &onyxv1.RunBackupRequest{JobId: job.Id})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "succeeded" || run.BytesWritten != 6 {
		t.Fatalf("unexpected run: %+v", run)
	}
	if _, err := os.Stat(filepath.Join(target, run.Id, "photo.jpg")); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	reloaded := newServer(filepath.Join(root, "state"))
	runs, err := reloaded.ListBackups(context.Background(), &onyxv1.ListBackupsRequest{JobId: job.Id})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].Status != "succeeded" {
		t.Fatalf("persisted runs: %+v", runs.Runs)
	}
}

func TestRunBackupRecordsFailure(t *testing.T) {
	root := t.TempDir()
	s := newServer(filepath.Join(root, "state"))
	job, err := s.CreateBackupJob(context.Background(), &onyxv1.CreateBackupJobRequest{Name: "missing", Source: filepath.Join(root, "nope"), TargetKind: "local", Target: filepath.Join(root, "target")})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.RunBackup(context.Background(), &onyxv1.RunBackupRequest{JobId: job.Id})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || run.Error == "" {
		t.Fatalf("expected failed run with error: %+v", run)
	}
}
