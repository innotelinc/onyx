package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// server implements Health and Vmm (proto/onyx/v1/vmm.proto). VM definitions
// live in SQLite, their disks and domains are real libvirt domains, and every
// lifecycle action goes through the Hypervisor seam (docs/design/11 §6.3).
type server struct {
	onyxv1.UnimplementedHealthServer
	onyxv1.UnimplementedVmmServer

	store    *vmStore
	hyper    Hypervisor
	diskRoot string
}

var _ onyxv1.HealthServer = (*server)(nil)
var _ onyxv1.VmmServer = (*server)(nil)

func newServer(st *vmStore, hyper Hypervisor, diskRoot string) *server {
	return &server{store: st, hyper: hyper, diskRoot: diskRoot}
}

func (s *server) Check(_ context.Context, _ *onyxv1.HealthCheckRequest) (*onyxv1.HealthCheckResponse, error) {
	return &onyxv1.HealthCheckResponse{
		Status:  onyxv1.HealthCheckResponse_SERVING,
		Version: version,
	}, nil
}

// vmNameRe keeps a VM name safe as a libvirt domain name, a file stem and an
// SMB/NFS-visible path: libvirt rejects most punctuation anyway, and a name we
// cannot turn into a filename is a name we must refuse early.
var vmNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

func (s *server) ListVMs(ctx context.Context, _ *onyxv1.ListVMsRequest) (*onyxv1.ListVMsResponse, error) {
	records, err := s.store.list()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*onyxv1.VM, 0, len(records))
	for _, rec := range records {
		// The hypervisor is the authority on what a machine is doing: a VM
		// stopped from inside the guest must read as stopped here too.
		live, err := s.hyper.State(ctx, rec.Name)
		if err != nil {
			slog.Warn("vm state unavailable, using the recorded state", "vm", rec.Name, "error", err)
			live = rec.Status
		} else if live != rec.Status {
			if err := s.store.updateStatus(rec.ID, live); err != nil {
				slog.Warn("vm state update failed", "vm", rec.ID, "error", err)
			}
			rec.Status = live
		}
		out = append(out, recordToProto(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return &onyxv1.ListVMsResponse{Vms: out}, nil
}

// CreateVM creates the disk image and defines the domain, leaving the machine
// stopped: an operator boots it deliberately (docs/design/11 §6.3).
func (s *server) CreateVM(ctx context.Context, req *onyxv1.CreateVMRequest) (*onyxv1.VM, error) {
	name := strings.TrimSpace(req.GetName())
	if !vmNameRe.MatchString(name) {
		return nil, status.Errorf(codes.InvalidArgument, "name %q must be 1-63 characters of letters, digits, dot, dash or underscore", req.GetName())
	}
	if req.GetVcpus() < 1 {
		return nil, status.Error(codes.InvalidArgument, "vcpus must be at least 1")
	}
	if req.GetMemoryMb() < 64 {
		return nil, status.Error(codes.InvalidArgument, "memory_mb must be at least 64")
	}
	diskMB := req.GetDiskMb()
	if diskMB == 0 {
		diskMB = 20480 // 20 GiB: a bootable default rather than a surprise
	}
	if diskMB < 1024 {
		return nil, status.Error(codes.InvalidArgument, "disk_mb must be at least 1024")
	}
	taken, err := s.store.nameTaken(name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if taken {
		return nil, status.Errorf(codes.AlreadyExists, "a vm named %s already exists", name)
	}
	id, err := s.store.nextID()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	diskPath := filepath.Join(s.diskRoot, name+".qcow2")
	created, err := s.hyper.Create(ctx, vmSpec{
		Name:     name,
		VCPUs:    req.GetVcpus(),
		MemoryMB: req.GetMemoryMb(),
		DiskMB:   diskMB,
		DiskPath: diskPath,
		OS:       req.GetOs(),
		ISO:      req.GetIso(),
	})
	if err != nil {
		slog.Error("vm create failed", "vm", name, "error", err)
		return nil, status.Errorf(codes.FailedPrecondition, "create %s: %v", name, err)
	}
	rec := vmRecord{
		ID: id, Name: name, Status: "stopped",
		VCPUs: req.GetVcpus(), MemoryMB: req.GetMemoryMb(), DiskMB: diskMB,
		Disk: created, OS: req.GetOs(), ISO: req.GetIso(),
	}
	if err := s.store.insert(rec); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Read back for the store's timestamp, so the caller sees what was stored.
	stored, _, err := s.store.byID(id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	slog.Info("vm created", "vm", name, "vcpus", rec.VCPUs, "memory_mb", rec.MemoryMB, "disk_mb", diskMB)
	return recordToProto(stored), nil
}

func (s *server) StartVM(ctx context.Context, req *onyxv1.StartVMRequest) (*onyxv1.VM, error) {
	return s.transition(ctx, req.GetId(), "stopped", func(rec vmRecord) error {
		return s.hyper.Start(ctx, rec.Name)
	}, func(rec vmRecord) string { return "running" })
}

func (s *server) StopVM(ctx context.Context, req *onyxv1.StopVMRequest) (*onyxv1.VM, error) {
	graceful := req.GetGraceful()
	return s.transition(ctx, req.GetId(), "running", func(rec vmRecord) error {
		return s.hyper.Stop(ctx, rec.Name, graceful)
	}, func(rec vmRecord) string { return "stopped" })
}

// transition applies the lifecycle rule — a machine may only move from the
// expected state — then runs the hypervisor action and re-reads the live state.
func (s *server) transition(ctx context.Context, idOrName, wantFrom string, action func(vmRecord) error, wantTo func(vmRecord) string) (*onyxv1.VM, error) {
	rec, ok, err := s.store.byID(idOrName)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "vm %q not found", idOrName)
	}
	// The live state decides, not the recorded one: a guest that shut itself
	// down must be startable, and one already running must not be started twice.
	current := rec.Status
	if live, err := s.hyper.State(ctx, rec.Name); err == nil {
		current = live
		if current != rec.Status {
			if err := s.store.updateStatus(rec.ID, current); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
	}
	if current != wantFrom {
		return nil, status.Errorf(codes.FailedPrecondition, "cannot act on vm %s in state %q (expected %q)", rec.Name, current, wantFrom)
	}
	if err := action(rec); err != nil {
		slog.Error("vm transition failed", "vm", rec.Name, "from", wantFrom, "error", err)
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	next := wantTo(rec)
	if live, err := s.hyper.State(ctx, rec.Name); err == nil {
		next = live
	} else {
		slog.Warn("vm state refresh failed after a successful action", "vm", rec.Name, "error", err)
	}
	if err := s.store.updateStatus(rec.ID, next); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	rec.Status = next
	return recordToProto(rec), nil
}

// DeleteVM undefines the domain; the disk image is deleted only when the caller
// asks for it, because that is irreversible (docs/design/11 §6.3).
func (s *server) DeleteVM(ctx context.Context, req *onyxv1.DeleteVMRequest) (*onyxv1.DeleteVMResponse, error) {
	rec, ok, err := s.store.byID(req.GetId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "vm %q not found", req.GetId())
	}
	// The live state decides, as everywhere else in the lifecycle: a machine the
	// store still has as stopped may be running in a hypervisor that was managed
	// while this service was down.
	current := rec.Status
	if live, err := s.hyper.State(ctx, rec.Name); err == nil {
		current = live
	} else {
		slog.Warn("vm state unavailable, using the recorded state", "vm", rec.Name, "error", err)
	}
	if current == "running" {
		return nil, status.Errorf(codes.FailedPrecondition, "stop vm %s before deleting it", rec.Name)
	}
	if err := s.hyper.Remove(ctx, rec.Name, rec.Disk, req.GetDeleteDisk()); err != nil {
		slog.Error("vm delete failed", "vm", rec.Name, "error", err)
		return nil, status.Errorf(codes.FailedPrecondition, "delete %s: %v", rec.Name, err)
	}
	if err := s.store.delete(rec.ID); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	slog.Info("vm deleted", "vm", rec.Name, "deleted_disk", req.GetDeleteDisk())
	return &onyxv1.DeleteVMResponse{Deleted: true}, nil
}

func recordToProto(rec vmRecord) *onyxv1.VM {
	return &onyxv1.VM{
		Id:        rec.ID,
		Name:      rec.Name,
		Status:    orDefault(rec.Status, "stopped"),
		Vcpus:     rec.VCPUs,
		MemoryMb:  rec.MemoryMB,
		Disk:      rec.Disk,
		Os:        rec.OS,
		CreatedAt: rec.CreatedAt,
	}
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
