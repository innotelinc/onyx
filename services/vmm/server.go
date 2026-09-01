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

// server implements Health and Vmm (proto/onyx/v1/vmm.proto). The registry
// lives in memory at v0.1 with a strict lifecycle state machine; the
// libvirt/QEMU backend lands with v0.4 (docs/design/11 §6.3).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedVmmServer

	mu   sync.Mutex
	vms  map[string]*onyxv1.VM
	next int
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.VmmServer = (*server)(nil)

func newServer() *server {
	return &server{vms: map[string]*onyxv1.VM{}}
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

func (s *server) ListVMs(_ context.Context, _ *onyxv1.ListVMsRequest) (*onyxv1.ListVMsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*onyxv1.VM, 0, len(s.vms))
	for _, vm := range s.vms {
		out = append(out, vm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListVMsResponse{Vms: out}, nil
}

func (s *server) CreateVM(_ context.Context, req *onyxv1.CreateVMRequest) (*onyxv1.VM, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.GetVcpus() < 1 || req.GetMemoryMb() < 64 {
		return nil, status.Error(codes.InvalidArgument, "vcpus >= 1 and memory_mb >= 64 are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	vm := &onyxv1.VM{
		Id:        fmt.Sprintf("vm-%d", s.next),
		Name:      req.GetName(),
		Status:    "stopped",
		Vcpus:     req.GetVcpus(),
		MemoryMb:  req.GetMemoryMb(),
		Disk:      fmt.Sprintf("pool1/@apps/vms/%s.qcow2", req.GetName()),
		Os:        req.GetOs(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.vms[vm.Id] = vm
	return vm, nil
}

func (s *server) StartVM(_ context.Context, req *onyxv1.StartVMRequest) (*onyxv1.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, err := s.getLocked(req.GetId())
	if err != nil {
		return nil, err
	}
	if vm.Status != "stopped" {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot start vm in state %q", vm.Status)
	}
	vm.Status = "running"
	return vm, nil
}

func (s *server) StopVM(_ context.Context, req *onyxv1.StopVMRequest) (*onyxv1.VM, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, err := s.getLocked(req.GetId())
	if err != nil {
		return nil, err
	}
	if vm.Status != "running" {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot stop vm in state %q", vm.Status)
	}
	vm.Status = "stopped"
	return vm, nil
}

func (s *server) DeleteVM(_ context.Context, req *onyxv1.DeleteVMRequest) (*onyxv1.DeleteVMResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, err := s.getLocked(req.GetId())
	if err != nil {
		return nil, err
	}
	if vm.Status == "running" {
		return nil, status.Error(codes.FailedPrecondition, "stop the vm before deleting it")
	}
	delete(s.vms, vm.Id)
	return &onyxv1.DeleteVMResponse{Deleted: true}, nil
}

func (s *server) getLocked(id string) (*onyxv1.VM, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	vm, ok := s.vms[id]
	if !ok {
		return nil, status.Error(codes.NotFound, "vm not found")
	}
	return vm, nil
}
