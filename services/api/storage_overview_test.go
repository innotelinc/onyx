package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// fakeCapacity describes what the local namespace can see: path -> capacity.
// A path absent from the map is invisible (not a mount, or not reachable).
func fakeCapacity(m map[string][2]int64) capacityFn {
	return func(path string) (int64, int64, bool) {
		v, ok := m[path]
		if !ok {
			return 0, 0, false
		}
		return v[0], v[1], true
	}
}

func pool(name, uuid, fs string, total, used uint64) *onyxv1.Pool {
	return &onyxv1.Pool{Name: name, Uuid: uuid, FsType: fs, TotalBytes: total, UsedBytes: used, State: "online"}
}

func dev(kname, label, uuid, mountpoint, state string) *onyxv1.Device {
	return &onyxv1.Device{Kname: kname, Name: label, Path: "/dev/" + kname, Type: "disk", Label: label, Uuid: uuid, Mountpoint: mountpoint, State: state, SizeBytes: 1000}
}

func TestStorageOverviewVisiblePool(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "main-pool")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	pools := []*onyxv1.Pool{pool("main-pool", "uuid-1", "btrfs", 1000, 400)}
	devices := []*onyxv1.Device{dev("sdc", "main-pool", "uuid-1", mount, "mounted")}
	// The live filesystem disagrees with the registry numbers: statfs wins.
	capacity := fakeCapacity(map[string][2]int64{mount: {8000, 6000}})

	got := buildStorageOverview(root, pools, devices, capacity)
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(got.Entries), got.Entries)
	}
	e := got.Entries[0]
	if !e.Mounted || !e.Visible || e.Mountpoint != mount {
		t.Errorf("entry = %+v, want mounted+visible at %s", e, mount)
	}
	if e.TotalBytes != 8000 || e.FreeBytes != 6000 || e.UsedBytes != 2000 {
		t.Errorf("capacity = %d/%d/%d, want 8000/6000/2000", e.TotalBytes, e.FreeBytes, e.UsedBytes)
	}
	if got.Primary == nil || got.Primary.Name != "main-pool" {
		t.Fatalf("primary = %+v, want main-pool", got.Primary)
	}
	if got.TotalBytes != 8000 || got.FreeBytes != 6000 {
		t.Errorf("totals = %d/%d, want 8000/6000", got.TotalBytes, got.FreeBytes)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", got.Warnings)
	}
}

// The headline bug: a pool the data plane says is mounted, which this process
// cannot see, must not read as "no storage" — it is a deployment fault and it
// has a fix.
func TestStorageOverviewInvisibleMountIsReported(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "main-pool")
	pools := []*onyxv1.Pool{pool("main-pool", "uuid-1", "btrfs", 1000, 400)}
	devices := []*onyxv1.Device{dev("sdc", "main-pool", "uuid-1", mount, "mounted")}

	got := buildStorageOverview(root, pools, devices, fakeCapacity(nil))
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(got.Entries), got.Entries)
	}
	e := got.Entries[0]
	if !e.Mounted || e.Visible {
		t.Errorf("entry = %+v, want mounted but not visible", e)
	}
	if e.TotalBytes != 1000 || e.UsedBytes != 400 {
		t.Errorf("capacity = %d/%d, want the data plane's 1000/400", e.TotalBytes, e.UsedBytes)
	}
	if got.Primary != nil {
		t.Errorf("primary = %+v, want nil (nothing usable is visible)", got.Primary)
	}
	if got.TotalBytes != 0 || got.FreeBytes != 0 {
		t.Errorf("totals = %d/%d, want 0/0 — the host disk must never stand in", got.TotalBytes, got.FreeBytes)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "mount --make-shared") {
		t.Fatalf("warnings = %v, want the propagation hint", got.Warnings)
	}
}

func TestStorageOverviewEmpty(t *testing.T) {
	got := buildStorageOverview(t.TempDir(), nil, nil, fakeCapacity(nil))
	if len(got.Entries) != 0 || got.Primary != nil {
		t.Fatalf("entries = %+v, primary = %+v", got.Entries, got.Primary)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "No storage is mounted") {
		t.Fatalf("warnings = %v, want the empty-storage note", got.Warnings)
	}
}

func TestStorageOverviewUnclaimedMountIsListed(t *testing.T) {
	root := t.TempDir()
	hotplug := filepath.Join(root, "usb-data")
	if err := os.Mkdir(hotplug, 0o755); err != nil {
		t.Fatal(err)
	}
	// A mount the pool registry does not list (a hotplug drive, or a volume
	// mounted by the host before any onyx service started).
	capacity := fakeCapacity(map[string][2]int64{hotplug: {2000, 500}})

	got := buildStorageOverview(root, nil, nil, capacity)
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1: %+v", len(got.Entries), got.Entries)
	}
	if got.Entries[0].Source != "device" || got.Entries[0].Name != "usb-data" {
		t.Errorf("entry = %+v, want a device-sourced usb-data", got.Entries[0])
	}
	if got.Primary == nil || got.FreeBytes != 500 {
		t.Errorf("primary = %+v, totals free = %d, want usb-data with 500 free", got.Primary, got.FreeBytes)
	}
}

// A single-pool install mounts the pool at the storage root itself.
func TestStorageOverviewStorageRootIsTheMount(t *testing.T) {
	root := t.TempDir()
	capacity := fakeCapacity(map[string][2]int64{root: {4000, 1000}})
	got := buildStorageOverview(root, nil, nil, capacity)
	if len(got.Entries) != 1 || got.Entries[0].Source != "storage-root" {
		t.Fatalf("entries = %+v, want one storage-root entry", got.Entries)
	}
	if got.Primary == nil || got.FreeBytes != 1000 {
		t.Fatalf("primary = %+v, want the root mount", got.Primary)
	}
}

func TestStorageOverviewOrdersVisibleFirst(t *testing.T) {
	root := t.TempDir()
	visible := filepath.Join(root, "main-pool")
	invisible := filepath.Join(root, "archive")
	pools := []*onyxv1.Pool{
		pool("main-pool", "uuid-1", "btrfs", 100, 10),
		pool("archive", "uuid-2", "ext4", 200, 20),
	}
	devices := []*onyxv1.Device{
		dev("sdc", "main-pool", "uuid-1", visible, "mounted"),
		dev("sdd", "archive", "uuid-2", invisible, "mounted"),
	}
	capacity := fakeCapacity(map[string][2]int64{visible: {100, 50}})

	got := buildStorageOverview(root, pools, devices, capacity)
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(got.Entries))
	}
	if !got.Entries[0].Visible || got.Entries[0].Name != "main-pool" {
		t.Errorf("first entry = %+v, want the visible main-pool", got.Entries[0])
	}
	// Totals only ever sum what can actually serve files.
	if got.TotalBytes != 100 || got.FreeBytes != 50 {
		t.Errorf("totals = %d/%d, want 100/50", got.TotalBytes, got.FreeBytes)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "archive") {
		t.Errorf("warnings = %v, want one about archive", got.Warnings)
	}
}

func TestDeviceForPoolMatching(t *testing.T) {
	devices := []*onyxv1.Device{
		dev("sdb", "other", "uuid-other", "", "attached"),
		dev("sdc", "renamed", "uuid-1", "", "attached"),
		dev("sdd", "by-label", "", "", "attached"),
	}
	if got := deviceForPool(pool("main-pool", "uuid-1", "btrfs", 1, 0), devices); got == nil || got.Kname != "sdc" {
		t.Errorf("uuid match = %+v, want sdc", got)
	}
	if got := deviceForPool(pool("by-label", "uuid-missing", "btrfs", 1, 0), devices); got == nil || got.Kname != "sdd" {
		t.Errorf("label match = %+v, want sdd", got)
	}
	if got := deviceForPool(pool("nobody", "", "btrfs", 1, 0), devices); got != nil {
		t.Errorf("unmatched pool = %+v, want nil", got)
	}
}
