package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// fakeHypervisor records the actions taken and serves a controllable state, so
// the lifecycle policy can be checked without libvirt or /dev/kvm.
type fakeHypervisor struct {
	specs  []vmSpec
	starts []string
	stops  []struct {
		name     string
		graceful bool
	}
	removes []struct {
		name       string
		disk       string
		deleteDisk bool
	}
	state     string
	createErr error
	startErr  error
	stopErr   error
	removeErr error
	stateErr  error
}

func (f *fakeHypervisor) Create(_ context.Context, spec vmSpec) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.specs = append(f.specs, spec)
	return spec.DiskPath, nil
}

func (f *fakeHypervisor) Start(_ context.Context, name string) error {
	f.starts = append(f.starts, name)
	if f.startErr != nil {
		return f.startErr
	}
	f.state = "running"
	return nil
}

func (f *fakeHypervisor) Stop(_ context.Context, name string, graceful bool) error {
	f.stops = append(f.stops, struct {
		name     string
		graceful bool
	}{name, graceful})
	if f.stopErr != nil {
		return f.stopErr
	}
	f.state = "stopped"
	return nil
}

func (f *fakeHypervisor) Remove(_ context.Context, name, diskPath string, deleteDisk bool) error {
	f.removes = append(f.removes, struct {
		name       string
		disk       string
		deleteDisk bool
	}{name, diskPath, deleteDisk})
	return f.removeErr
}

func (f *fakeHypervisor) State(_ context.Context, _ string) (string, error) {
	if f.stateErr != nil {
		return "", f.stateErr
	}
	if f.state == "" {
		return "stopped", nil
	}
	return f.state, nil
}

func testVMSServer(t *testing.T, hyper Hypervisor) (*server, string) {
	t.Helper()
	stateDir := t.TempDir()
	st, err := openStore(stateDir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	diskRoot := filepath.Join(stateDir, "disks")
	return newServer(st, hyper, diskRoot), diskRoot
}

func createVM(t *testing.T, s *server, req *onyxv1.CreateVMRequest) *onyxv1.VM {
	t.Helper()
	vm, err := s.CreateVM(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	return vm
}

func TestCreateVMDefinesStoppedMachine(t *testing.T) {
	hyper := &fakeHypervisor{}
	s, diskRoot := testVMSServer(t, hyper)
	ctx := context.Background()

	vm := createVM(t, s, &onyxv1.CreateVMRequest{Name: "debian", Vcpus: 2, MemoryMb: 2048, DiskMb: 8192, Os: "debian-12"})
	if vm.GetStatus() != "stopped" {
		t.Errorf("status = %q, want stopped (creation must not boot)", vm.GetStatus())
	}
	if vm.GetId() != "vm-1" {
		t.Errorf("id = %q, want vm-1", vm.GetId())
	}
	if want := filepath.Join(diskRoot, "debian.qcow2"); vm.GetDisk() != want {
		t.Errorf("disk = %q, want %q", vm.GetDisk(), want)
	}
	if len(hyper.specs) != 1 || hyper.specs[0].VCPUs != 2 || hyper.specs[0].MemoryMB != 2048 || hyper.specs[0].DiskMB != 8192 {
		t.Fatalf("specs = %+v", hyper.specs)
	}
	if len(hyper.starts) != 0 {
		t.Errorf("starts = %v, want none", hyper.starts)
	}

	// The definition persists: a restarted service still lists the machine.
	records, err := s.store.list()
	if err != nil || len(records) != 1 || records[0].Name != "debian" {
		t.Fatalf("store = %+v, %v", records, err)
	}

	list, err := s.ListVMs(ctx, &onyxv1.ListVMsRequest{})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(list.GetVms()) != 1 || list.GetVms()[0].GetName() != "debian" {
		t.Fatalf("vms = %+v", list.GetVms())
	}
}

func TestCreateVMDefaultDiskAndValidation(t *testing.T) {
	hyper := &fakeHypervisor{}
	s, _ := testVMSServer(t, hyper)
	ctx := context.Background()

	createVM(t, s, &onyxv1.CreateVMRequest{Name: "default-disk", Vcpus: 1, MemoryMb: 512})
	if len(hyper.specs) != 1 || hyper.specs[0].DiskMB != 20480 {
		t.Errorf("spec = %+v, want the 20 GiB default", hyper.specs)
	}

	cases := []struct {
		name string
		req  *onyxv1.CreateVMRequest
		code codes.Code
	}{
		{"empty name", &onyxv1.CreateVMRequest{Name: "", Vcpus: 1, MemoryMb: 512}, codes.InvalidArgument},
		{"unsafe name", &onyxv1.CreateVMRequest{Name: "../etc/passwd", Vcpus: 1, MemoryMb: 512}, codes.InvalidArgument},
		{"space in name", &onyxv1.CreateVMRequest{Name: "my vm", Vcpus: 1, MemoryMb: 512}, codes.InvalidArgument},
		{"no vcpus", &onyxv1.CreateVMRequest{Name: "vm", Vcpus: 0, MemoryMb: 512}, codes.InvalidArgument},
		{"too little memory", &onyxv1.CreateVMRequest{Name: "vm", Vcpus: 1, MemoryMb: 32}, codes.InvalidArgument},
		{"tiny disk", &onyxv1.CreateVMRequest{Name: "vm", Vcpus: 1, MemoryMb: 512, DiskMb: 100}, codes.InvalidArgument},
	}
	for _, c := range cases {
		if _, err := s.CreateVM(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("%s: %v, want %v", c.name, err, c.code)
		}
	}
	if _, err := s.CreateVM(ctx, &onyxv1.CreateVMRequest{Name: "default-disk", Vcpus: 1, MemoryMb: 512}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("duplicate name: %v, want AlreadyExists", err)
	}
}

// A hypervisor failure at create time must not leave a record behind.
func TestCreateVMFailureLeavesNoRecord(t *testing.T) {
	hyper := &fakeHypervisor{createErr: errors.New("qemu-img create: permission denied")}
	s, _ := testVMSServer(t, hyper)

	_, err := s.CreateVM(context.Background(), &onyxv1.CreateVMRequest{Name: "broken", Vcpus: 1, MemoryMb: 512})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the backend's message", err)
	}
	records, listErr := s.store.list()
	if listErr != nil || len(records) != 0 {
		t.Fatalf("store = %+v, want empty", records)
	}
}

func TestLifecycleFollowsTheLiveState(t *testing.T) {
	hyper := &fakeHypervisor{}
	s, _ := testVMSServer(t, hyper)
	ctx := context.Background()
	vm := createVM(t, s, &onyxv1.CreateVMRequest{Name: "web", Vcpus: 1, MemoryMb: 1024})

	started, err := s.StartVM(ctx, &onyxv1.StartVMRequest{Id: vm.GetId()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if started.GetStatus() != "running" || len(hyper.starts) != 1 {
		t.Fatalf("started = %+v, starts = %v", started, hyper.starts)
	}
	// Starting an already-running machine is a policy violation, not a no-op.
	if _, err := s.StartVM(ctx, &onyxv1.StartVMRequest{Id: vm.GetId()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second start: %v, want FailedPrecondition", err)
	}

	stopped, err := s.StopVM(ctx, &onyxv1.StopVMRequest{Id: vm.GetId(), Graceful: true})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.GetStatus() != "stopped" || len(hyper.stops) != 1 || !hyper.stops[0].graceful {
		t.Fatalf("stopped = %+v, stops = %+v", stopped, hyper.stops)
	}
	if _, err := s.StopVM(ctx, &onyxv1.StopVMRequest{Id: vm.GetId()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second stop: %v, want FailedPrecondition", err)
	}

	// A guest that shut itself down while the store still says running: the
	// live state is what allows the next start.
	if err := s.store.updateStatus(vm.GetId(), "running"); err != nil {
		t.Fatalf("store: %v", err)
	}
	hyper.state = "stopped"
	if _, err := s.StartVM(ctx, &onyxv1.StartVMRequest{Id: vm.GetId()}); err != nil {
		t.Fatalf("start after a guest-initiated shutdown: %v", err)
	}
}

func TestStopHonoursGracefulFlag(t *testing.T) {
	hyper := &fakeHypervisor{state: "running"}
	s, _ := testVMSServer(t, hyper)
	ctx := context.Background()
	vm := createVM(t, s, &onyxv1.CreateVMRequest{Name: "hard", Vcpus: 1, MemoryMb: 512})
	if err := s.store.updateStatus(vm.GetId(), "running"); err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := s.StopVM(ctx, &onyxv1.StopVMRequest{Id: vm.GetId(), Graceful: false}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(hyper.stops) != 1 || hyper.stops[0].graceful {
		t.Fatalf("stops = %+v, want a hard power-off", hyper.stops)
	}
}

func TestDeleteRequiresStoppedAndHonoursDiskFlag(t *testing.T) {
	hyper := &fakeHypervisor{state: "running"}
	s, _ := testVMSServer(t, hyper)
	ctx := context.Background()
	vm := createVM(t, s, &onyxv1.CreateVMRequest{Name: "doomed", Vcpus: 1, MemoryMb: 512})

	if _, err := s.DeleteVM(ctx, &onyxv1.DeleteVMRequest{Id: vm.GetId()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("delete while running: %v, want FailedPrecondition", err)
	}
	if len(hyper.removes) != 0 {
		t.Errorf("removes = %+v, want none", hyper.removes)
	}

	hyper.state = "stopped"
	if _, err := s.DeleteVM(ctx, &onyxv1.DeleteVMRequest{Id: vm.GetName(), DeleteDisk: true}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(hyper.removes) != 1 || !hyper.removes[0].deleteDisk || hyper.removes[0].disk != vm.GetDisk() {
		t.Fatalf("removes = %+v, want a disk-deleting removal of %s", hyper.removes, vm.GetDisk())
	}
	if _, err := s.DeleteVM(ctx, &onyxv1.DeleteVMRequest{Id: vm.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("delete after delete: %v, want NotFound", err)
	}
}

// An unreachable hypervisor must not blank the inventory: the recorded state is
// still the operator's only view of their machines.
func TestStateUnavailableFallsBackToRecorded(t *testing.T) {
	hyper := &fakeHypervisor{stateErr: errors.New("Failed to connect socket to '/var/run/libvirt/libvirt-sock'")}
	s, _ := testVMSServer(t, hyper)
	vm := createVM(t, s, &onyxv1.CreateVMRequest{Name: "quiet", Vcpus: 1, MemoryMb: 512})

	list, err := s.ListVMs(context.Background(), &onyxv1.ListVMsRequest{})
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(list.GetVms()) != 1 || list.GetVms()[0].GetStatus() != "stopped" || list.GetVms()[0].GetId() != vm.GetId() {
		t.Fatalf("vms = %+v", list.GetVms())
	}
}

func TestTransitionsOnUnknownVM(t *testing.T) {
	s, _ := testVMSServer(t, &fakeHypervisor{})
	if _, err := s.StartVM(context.Background(), &onyxv1.StartVMRequest{Id: "vm-9"}); status.Code(err) != codes.NotFound {
		t.Errorf("start unknown: %v, want NotFound", err)
	}
	if _, err := s.StartVM(context.Background(), &onyxv1.StartVMRequest{}); status.Code(err) != codes.NotFound {
		t.Errorf("start with no id: %v, want NotFound", err)
	}
}
