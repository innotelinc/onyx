package main

import (
	"testing"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

func dev(name, kname, state, mountpoint string) *onyxv1.Device {
	return &onyxv1.Device{
		Name:       name,
		Kname:      kname,
		Path:       "/dev/" + kname,
		FsType:     "vfat",
		Label:      "USB",
		Mountpoint: mountpoint,
		State:      state,
		Auto:       "removable",
	}
}

const testMountRoot = "/mnt/onyx"

func TestReconcilePlanCreatesSharesForOnyxMountedDevices(t *testing.T) {
	toCreate, toDelete := reconcilePlan(
		[]*onyxv1.Device{
			dev("usb-stick", "sdb1", "mounted", "/mnt/onyx/usb-stick"),
			dev("sdc1", "sdc1", "attached", ""), // attached but unmounted: no share
			dev("sdd1", "sdd1", "detached", ""),
		},
		nil, testMountRoot,
	)
	if len(toCreate) != 1 || len(toDelete) != 0 {
		t.Fatalf("expected 1 create / 0 deletes, got %d / %d", len(toCreate), len(toDelete))
	}
	if toCreate[0].Name != "usb-stick" {
		t.Fatalf("expected create for usb-stick, got %s", toCreate[0].Name)
	}
}

func TestReconcilePlanNeverSharesTheOSFilesystem(t *testing.T) {
	// The host root fs and /boot are "mounted" but outside the onyx root —
	// sharing them would be a security hole. Only onyx-mounted drives count.
	toCreate, toDelete := reconcilePlan(
		[]*onyxv1.Device{
			dev("root", "sda1", "mounted", "/"),
			dev("boot", "sda2", "mounted", "/boot"),
			dev("data", "sdb1", "mounted", "/srv/elsewhere"), // user mount, not onyx
		},
		nil, testMountRoot,
	)
	if len(toCreate) != 0 {
		t.Fatalf("expected no creates for non-onyx mounts, got %v", toCreate)
	}
	if len(toDelete) != 0 {
		t.Fatalf("expected no deletes, got %v", toDelete)
	}
}

func TestReconcilePlanDeletesAutoShareWhenDeviceUnmountedOrDetached(t *testing.T) {
	existing := []shareRow{
		{name: "usb-stick", path: "/mnt/onyx/usb-stick", source: "device:sdb1"},
		{name: "photos", path: "/mnt/onyx/pool1/@data/photos", source: "manual"},
	}
	// Detached-after-unplug AND manually unmounted-but-still-attached must
	// both remove the auto share. The user share is untouched.
	toCreate, toDelete := reconcilePlan(
		[]*onyxv1.Device{
			dev("usb-stick", "sdb1", "detached", ""),
			dev("manual-usb", "sdc1", "attached", ""), // attached, no mountpoint
		},
		existing, testMountRoot,
	)
	if len(toDelete) != 1 || toDelete[0] != "usb-stick" {
		t.Fatalf("expected delete of auto share usb-stick, got %v", toDelete)
	}
	if len(toCreate) != 0 {
		t.Fatalf("expected no creates, got %v", toCreate)
	}
}

func TestReconcilePlanAutoShareRemovedOnManualUnmount(t *testing.T) {
	existing := []shareRow{{name: "usb-stick", path: "/mnt/onyx/usb-stick", source: "device:sdb1"}}
	// Device still attached (drive still plugged) but no longer mounted:
	// the auto share follows the mount, so it must go.
	toCreate, toDelete := reconcilePlan(
		[]*onyxv1.Device{dev("usb-stick", "sdb1", "attached", "")},
		existing, testMountRoot,
	)
	if len(toDelete) != 1 || toDelete[0] != "usb-stick" {
		t.Fatalf("expected delete on manual unmount, got %v", toDelete)
	}
	if len(toCreate) != 0 {
		t.Fatalf("expected no creates, got %v", toCreate)
	}
}

func TestReconcilePlanNeverStepsOnUserShares(t *testing.T) {
	existing := []shareRow{{name: "media", path: "/srv/media", source: "manual"}}
	toCreate, toDelete := reconcilePlan(
		[]*onyxv1.Device{
			dev("media", "sdb1", "mounted", "/mnt/onyx/media"),
			dev("media", "sdb1", "detached", ""), // source mismatch guards against deletion too
		},
		existing, testMountRoot,
	)
	if len(toCreate) != 0 {
		t.Fatalf("expected no create over a user share, got %v", toCreate)
	}
	if len(toDelete) != 0 {
		t.Fatalf("expected no delete of a user share, got %v", toDelete)
	}
}

func TestReconcilePlanIdempotent(t *testing.T) {
	existing := []shareRow{{name: "usb-stick", path: "/mnt/onyx/usb-stick", source: "device:sdb1"}}
	toCreate, toDelete := reconcilePlan(
		[]*onyxv1.Device{dev("usb-stick", "sdb1", "mounted", "/mnt/onyx/usb-stick")},
		existing, testMountRoot,
	)
	if len(toCreate) != 0 || len(toDelete) != 0 {
		t.Fatalf("reconcile must be a no-op for an already-synced device, got create=%v delete=%v", toCreate, toDelete)
	}
}

func TestOnyxMounted(t *testing.T) {
	ok := func(state, mountpoint string) *onyxv1.Device {
		return dev("x", "sdb1", state, mountpoint)
	}
	cases := []struct {
		d    *onyxv1.Device
		want bool
	}{
		{ok("mounted", "/mnt/onyx/usb-stick"), true},
		{ok("mounted", "/mnt/onyx"), false},          // the root itself
		{ok("mounted", "/mnt/onyx/"), false},         // root with trailing slash
		{ok("mounted", "/"), false},                  // OS root
		{ok("mounted", "/mnt/onyxx"), false},         // sibling of root
		{ok("mounted", "/boot"), false},              // OS boot
		{ok("attached", "/mnt/onyx/usb-stick"), false}, // not mounted
		{ok("mounted", ""), false},                   // no mountpoint
	}
	for _, c := range cases {
		if got := onyxMounted(testMountRoot, c.d); got != c.want {
			t.Errorf("onyxMounted(%q, %q) = %v, want %v", c.d.State, c.d.Mountpoint, got, c.want)
		}
	}
}