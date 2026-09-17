package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Snapd (proto/onyx/v1/snapd.proto).
// Snapshot metadata is persisted atomically in the service state directory;
// the storage operation remains guarded by the snapd contract until the
// privileged Btrfs executor is enabled (docs/design/11 §6.1).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedSnapdServer

	mu        sync.Mutex
	snapshots map[string]*onyxv1.Snapshot
	statePath string
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.SnapdServer = (*server)(nil)

func newServer(stateDir string) *server {
	s := &server{snapshots: map[string]*onyxv1.Snapshot{}, statePath: filepath.Join(stateDir, "snapshots.json")}
	if b, err := os.ReadFile(s.statePath); err == nil {
		var saved map[string]*onyxv1.Snapshot
		if json.Unmarshal(b, &saved) == nil && saved != nil {
			s.snapshots = saved
		}
	}
	return s
}

func (s *server) persistLocked() error {
	if s.statePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0o750); err != nil {
		return err
	}
	b, err := json.Marshal(s.snapshots)
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

func (s *server) CreateSnapshot(_ context.Context, req *onyxv1.CreateSnapshotRequest) (*onyxv1.Snapshot, error) {
	if req.GetPool() == "" {
		return nil, status.Error(codes.InvalidArgument, "pool is required")
	}
	if req.GetSubvolume() == "" {
		return nil, status.Error(codes.InvalidArgument, "subvolume is required")
	}
	name := req.GetName()
	if name == "" {
		name = fmt.Sprintf("snap-%s", time.Now().UTC().Format("20060102-150405"))
	}
	snap := &onyxv1.Snapshot{
		Id:        fmt.Sprintf("snap-%d", time.Now().UnixNano()),
		Pool:      req.GetPool(),
		Subvolume: req.GetSubvolume(),
		Name:      name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Readonly:  !req.GetWritable(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots[snap.Id] = snap
	if err := s.persistLocked(); err != nil {
		delete(s.snapshots, snap.Id)
		return nil, status.Errorf(codes.Internal, "persist snapshot: %v", err)
	}
	return snap, nil
}

func (s *server) ListSnapshots(_ context.Context, req *onyxv1.ListSnapshotsRequest) (*onyxv1.ListSnapshotsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*onyxv1.Snapshot, 0, len(s.snapshots))
	for _, snap := range s.snapshots {
		if req.GetPool() != "" && snap.Pool != req.GetPool() {
			continue
		}
		if req.GetSubvolume() != "" && snap.Subvolume != req.GetSubvolume() {
			continue
		}
		out = append(out, snap)
	}
	return &onyxv1.ListSnapshotsResponse{Snapshots: out}, nil
}

func (s *server) DeleteSnapshot(_ context.Context, req *onyxv1.DeleteSnapshotRequest) (*onyxv1.DeleteSnapshotResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.snapshots[req.GetId()]
	if ok {
		delete(s.snapshots, req.GetId())
		if err := s.persistLocked(); err != nil {
			return nil, status.Errorf(codes.Internal, "persist snapshot deletion: %v", err)
		}
	}
	return &onyxv1.DeleteSnapshotResponse{Deleted: ok}, nil
}

func (s *server) RollbackSnapshot(_ context.Context, req *onyxv1.RollbackSnapshotRequest) (*onyxv1.RollbackSnapshotResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "snapshot not found")
	}
	return &onyxv1.RollbackSnapshotResponse{
		Subvolume:   snap.Subvolume,
		Snapshot:    snap.Name,
		CompletedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}
