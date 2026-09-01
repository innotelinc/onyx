package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Snapd (proto/onyx/v1/snapd.proto).
// Snapshot metadata lives in memory at v0.1; the btrfs-backed registry lands
// with v0.3 (docs/design/11 §6.1).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedSnapdServer

	mu        sync.Mutex
	snapshots map[string]*onyxv1.Snapshot
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.SnapdServer = (*server)(nil)

func newServer() *server {
	return &server{snapshots: map[string]*onyxv1.Snapshot{}}
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
