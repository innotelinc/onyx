package main

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingRunner struct {
	calls [][]string
	out   string
	err   error
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out, r.err
}

func (r *recordingRunner) find(bin string) []string {
	for _, c := range r.calls {
		if c[0] == bin {
			return c
		}
	}
	return nil
}

func TestRenderDomainXML(t *testing.T) {
	doc, err := renderDomainXML(vmSpec{
		Name: "debian", VCPUs: 2, MemoryMB: 2048, DiskPath: "/mnt/onyx/main-pool/@apps/vms/debian.qcow2",
	}, "default")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var parsed domain
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("the generated XML does not parse: %v\n%s", err, doc)
	}
	if parsed.Type != "kvm" || parsed.Name != "debian" || parsed.VCPU != 2 || parsed.Memory.Value != 2048 {
		t.Errorf("domain = %+v", parsed)
	}
	if parsed.Memory.Unit != "MiB" {
		t.Errorf("memory unit = %q, want MiB (a bare number means KiB to libvirt)", parsed.Memory.Unit)
	}
	if parsed.OS.Boot.Dev != "hd" {
		t.Errorf("boot = %q, want hd without install media", parsed.OS.Boot.Dev)
	}
	if len(parsed.Devices.Disks) != 1 || parsed.Devices.Disks[0].Target.Bus != "virtio" {
		t.Fatalf("disks = %+v", parsed.Devices.Disks)
	}
	if parsed.Devices.Interface.Source.Network != "default" {
		t.Errorf("network = %q", parsed.Devices.Interface.Source.Network)
	}
	// The console/VNC surface is loopback-only: an unattended VM must not expose
	// a graphical console on the LAN.
	if parsed.Devices.Graphics.Listen != "127.0.0.1" || parsed.Devices.Graphics.AutoPort != "yes" {
		t.Errorf("graphics = %+v, want loopback autoport", parsed.Devices.Graphics)
	}
}

func TestRenderDomainXMLWithISO(t *testing.T) {
	doc, err := renderDomainXML(vmSpec{
		Name: "installer", VCPUs: 1, MemoryMB: 1024,
		DiskPath: "/mnt/onyx/main-pool/@apps/vms/installer.qcow2",
		ISO:      "/mnt/onyx/main-pool/@apps/iso/debian.iso",
	}, "default")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var parsed domain
	if err := xml.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.OS.Boot.Dev != "cdrom" {
		t.Errorf("boot = %q, want cdrom when install media is attached", parsed.OS.Boot.Dev)
	}
	if len(parsed.Devices.Disks) != 2 {
		t.Fatalf("disks = %+v, want the image plus the cdrom", parsed.Devices.Disks)
	}
	cd := parsed.Devices.Disks[1]
	if cd.Device != "cdrom" || cd.Readonly == nil || !strings.HasSuffix(cd.Source.File, "debian.iso") {
		t.Errorf("cdrom = %+v", cd)
	}
}

// Names and paths are XML-escaped by the marshaller rather than concatenated,
// so a hostile name cannot break out of the document.
func TestRenderDomainXMLEscapesValues(t *testing.T) {
	doc, err := renderDomainXML(vmSpec{Name: `a&b<c>"d`, VCPUs: 1, MemoryMB: 512, DiskPath: "/tmp/x.qcow2"}, "default")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(doc), "<c>") {
		t.Errorf("unescaped XML in output:\n%s", doc)
	}
	if !strings.Contains(string(doc), "a&amp;b") {
		t.Errorf("name not escaped:\n%s", doc)
	}
}

func TestDomainStateMapping(t *testing.T) {
	cases := map[string]string{
		"running":     "running",
		"shut off":    "stopped",
		"paused":      "paused",
		"pmsuspended": "paused",
		"in shutdown": "stopped",
		"crashed":     "error",
		"  Running  ": "running",
		"weird":       "error",
		"":            "error",
	}
	for raw, want := range cases {
		if got := domainState(raw); got != want {
			t.Errorf("domainState(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCreateUsesQemuImgThenVirshDefine(t *testing.T) {
	root := t.TempDir()
	r := &recordingRunner{out: "Domain 'debian' defined from /tmp/x.xml"}
	hyper := newLibvirtHypervisor("virsh", "qemu-img", filepath.Join(root, "vms"), filepath.Join(root, "domains"), "default")
	hyper.run = r.run

	disk, err := hyper.Create(context.Background(), vmSpec{Name: "debian", VCPUs: 1, MemoryMB: 512, DiskMB: 4096})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %v, want qemu-img then virsh define", r.calls)
	}
	qemu := r.find("qemu-img")
	if qemu == nil || qemu[1] != "create" || qemu[2] != "-f" || qemu[3] != "qcow2" || qemu[5] != "4096M" {
		t.Fatalf("qemu-img argv = %v", qemu)
	}
	if qemu[4] != disk {
		t.Errorf("qemu-img target = %q, want %q", qemu[4], disk)
	}
	virsh := r.find("virsh")
	if virsh == nil || virsh[1] != "define" {
		t.Fatalf("virsh argv = %v, want a define", virsh)
	}
	if _, err := os.Stat(virsh[2]); err != nil {
		t.Errorf("domain XML %s was not written: %v", virsh[2], err)
	}
}

// An existing image is never overwritten: creating a VM over a machine's disk
// would destroy it.
func TestCreateRefusesExistingDisk(t *testing.T) {
	root := t.TempDir()
	diskRoot := filepath.Join(root, "vms")
	if err := os.MkdirAll(diskRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(diskRoot, "debian.qcow2")
	if err := os.WriteFile(existing, []byte("qcow2"), 0o640); err != nil {
		t.Fatal(err)
	}
	hyper := newLibvirtHypervisor("virsh", "qemu-img", diskRoot, filepath.Join(root, "domains"), "default")
	hyper.run = (&recordingRunner{}).run
	if _, err := hyper.Create(context.Background(), vmSpec{Name: "debian", VCPUs: 1, MemoryMB: 512, DiskMB: 1024}); err == nil {
		t.Fatal("expected an error for an existing image")
	}
	body, err := os.ReadFile(existing)
	if err != nil || string(body) != "qcow2" {
		t.Errorf("existing image was modified: %q %v", body, err)
	}
}

// A failed define must not leave the freshly created image behind.
func TestCreateCleansUpAfterAFailedDefine(t *testing.T) {
	root := t.TempDir()
	r := &recordingRunner{}
	hyper := newLibvirtHypervisor("virsh", "qemu-img", filepath.Join(root, "vms"), filepath.Join(root, "domains"), "default")
	hyper.run = func(ctx context.Context, name string, args ...string) (string, error) {
		r.calls = append(r.calls, append([]string{name}, args...))
		if name == "virsh" {
			return "", os.ErrPermission
		}
		return "", nil
	}
	disk, err := hyper.Create(context.Background(), vmSpec{Name: "debian", VCPUs: 1, MemoryMB: 512, DiskMB: 1024})
	if err == nil {
		t.Fatal("expected the define failure to surface")
	}
	if _, statErr := os.Stat(disk); !os.IsNotExist(statErr) {
		t.Errorf("orphan disk left at %s", disk)
	}
}

func TestStopAndRemoveArgv(t *testing.T) {
	root := t.TempDir()
	r := &recordingRunner{out: "shut off"}
	hyper := newLibvirtHypervisor("virsh", "qemu-img", filepath.Join(root, "vms"), filepath.Join(root, "domains"), "default")
	hyper.run = r.run
	ctx := context.Background()

	if err := hyper.Stop(ctx, "debian", true); err != nil {
		t.Fatalf("graceful stop: %v", err)
	}
	if got := r.find("virsh"); got[1] != "shutdown" {
		t.Errorf("graceful stop argv = %v, want virsh shutdown", got)
	}
	if err := hyper.Stop(ctx, "debian", false); err != nil {
		t.Fatalf("hard stop: %v", err)
	}
	if got := r.calls[len(r.calls)-1]; got[1] != "destroy" {
		t.Errorf("hard stop argv = %v, want virsh destroy", got)
	}
	if state, err := hyper.State(ctx, "debian"); err != nil || state != "stopped" {
		t.Errorf("state = %q, %v, want stopped", state, err)
	}
	if err := hyper.Remove(ctx, "debian", "", false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := r.calls[len(r.calls)-1]; got[1] != "undefine" || len(got) != 3 {
		t.Errorf("remove argv = %v, want a plain undefine", got)
	}
}

// Removing with deleteDisk actually deletes the image — the one irreversible
// VM action, so it is asserted rather than assumed.
func TestRemoveDeletesTheDiskOnlyWhenAsked(t *testing.T) {
	root := t.TempDir()
	diskRoot := filepath.Join(root, "vms")
	if err := os.MkdirAll(diskRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(diskRoot, "debian.qcow2")
	if err := os.WriteFile(disk, []byte("qcow2"), 0o640); err != nil {
		t.Fatal(err)
	}
	r := &recordingRunner{}
	hyper := newLibvirtHypervisor("virsh", "qemu-img", diskRoot, filepath.Join(root, "domains"), "default")
	hyper.run = r.run

	if err := hyper.Remove(context.Background(), "debian", disk, false); err != nil {
		t.Fatalf("remove keeping the disk: %v", err)
	}
	if _, err := os.Stat(disk); err != nil {
		t.Fatalf("disk was deleted without being asked: %v", err)
	}
	if err := hyper.Remove(context.Background(), "debian", disk, true); err != nil {
		t.Fatalf("remove deleting the disk: %v", err)
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Error("disk still present after a delete request")
	}
}
