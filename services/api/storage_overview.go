package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	onyxv1 "github.com/innotelinc/onyx/proto/gen/go/onyx/v1"
)

// Storage capacity reporting (docs/design/05#5). The Files page used to stat
// the storage *root*, which on a normal install is the host's system disk — it
// reported the system disk's capacity as if it were the pool's, and said
// nothing at all when a pool was mounted somewhere the API could not see.
//
// The data plane is the authority on what is mounted (storaged's registry is
// populated from `lsblk` and from every mount it performs), so the overview is
// built by joining that view with what this process can actually reach. When
// the two disagree the difference is reported instead of being swallowed: an
// unshared mount namespace means the pool is mounted but invisible here, and
// that is a container/deployment fault an operator has to fix.

// mountCapacity reports the capacity of one mounted path as (total, free, ok).
// ok is false when the path is missing, is not a directory, or is not a mount
// point — a plain directory under the storage root is not storage.
func mountCapacity(path string) (int64, int64, bool) {
	if path == "" {
		return 0, 0, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return 0, 0, false
	}
	if !isMountPoint(path, filepath.Dir(path)) {
		return 0, 0, false
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, false
	}
	block := int64(fs.Bsize)
	return int64(fs.Blocks) * block, int64(fs.Bavail) * block, true
}

// storageEntry is one storage location as the overview reports it.
type storageEntry struct {
	Name       string `json:"name"`
	FSType     string `json:"fs_type,omitempty"`
	State      string `json:"state,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	// Mounted is what the data plane says; Visible is what this process can
	// reach. Only a mounted-and-visible entry can serve files.
	Mounted    bool  `json:"mounted"`
	Visible    bool  `json:"visible"`
	TotalBytes int64 `json:"total_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	// Source says where the entry came from: "pool" (storaged's pool registry),
	// "device" (a mounted device the pool registry does not list) or
	// "storage-root" (the storage root itself is the mount).
	Source string `json:"source"`
}

type storageOverview struct {
	StorageRoot string         `json:"storage_root"`
	Entries     []storageEntry `json:"entries"`
	// Primary is the entry the capacity headline describes: the first visible
	// mount, else nil when nothing is visible.
	Primary    *storageEntry `json:"primary"`
	TotalBytes int64         `json:"total_bytes"`
	FreeBytes  int64         `json:"free_bytes"`
	// Warnings are operator-actionable: each one is something this deployment
	// is doing wrong, not a condition the user can fix from the UI.
	Warnings []string `json:"warnings"`
}

// capacityFn is mountCapacity, injected so the join can be tested without real
// mounts.
type capacityFn func(path string) (int64, int64, bool)

// reconcilePoolState corrects one pool's state from the mount this process can
// actually reach, so `GET /pools` agrees with `GET /storage/overview` and the
// dashboard instead of contradicting them.
//
// The registry's `state` is a snapshot of the last scan: `btrfs filesystem
// show` marks a btrfs pool offline the moment it stops listing it, which happens
// whenever the scanner cannot see the device (a pool mounted by the host before
// Onyx started, a container without the block node, a scan that raced a
// remount). The mount is the reality the user sees in Files, so a pool whose
// mountpoint this process can stat is online whatever the snapshot concluded.
// An unreachable pool keeps the registry's verdict — a missing mount is exactly
// when "offline" is the truth.
func reconcilePoolState(pool *onyxv1.Pool, devices []*onyxv1.Device, capacity capacityFn) {
	if pool == nil || strings.EqualFold(pool.GetState(), "online") {
		return
	}
	mount := ""
	if dev := deviceForPool(pool, devices); dev != nil {
		mount = dev.GetMountpoint()
	}
	if mount == "" {
		// The stale-registry case the Remove action exists for: the device row is
		// gone but the pool remembers where it was mounted.
		mount = pool.GetMountpoint()
	}
	if _, _, visible := capacity(mount); visible {
		pool.State = "online"
	}
}

// reconcilePools applies reconcilePoolState across a pool list in place.
func reconcilePools(pools []*onyxv1.Pool, devices []*onyxv1.Device, capacity capacityFn) {
	for _, pool := range pools {
		reconcilePoolState(pool, devices, capacity)
	}
}

// dataPlaneMountsRoot reports whether the data plane says the storage root *is*
// a filesystem of its own: a pool (through its backing device) or a device
// whose mountpoint is the root itself.
//
// This is the difference between the classic single-pool install — where
// /mnt/onyx is the pool rather than a parent of it, mounted by onyx-pool before
// any daemon starts — and a normal install, where the root sits on whatever the
// host gave it (its system disk, or a bind mount of one). Only the data plane's
// answer can tell them apart, and statfs'ing the second is how a 220 GB root
// disk came to be reported as "Storage" with every pool offline.
func dataPlaneMountsRoot(root string, pools []*onyxv1.Pool, devices []*onyxv1.Device) bool {
	for _, pool := range pools {
		if dev := deviceForPool(pool, devices); dev != nil && dev.GetMountpoint() == root {
			return true
		}
	}
	for _, dev := range devices {
		if dev.GetMountpoint() == root {
			return true
		}
	}
	return false
}

// deviceForPool finds the device backing a pool: by filesystem UUID first (the
// stable identity), then by label, then by the pool's display name.
func deviceForPool(pool *onyxv1.Pool, devices []*onyxv1.Device) *onyxv1.Device {
	for _, d := range devices {
		if pool.GetUuid() != "" && d.GetUuid() == pool.GetUuid() {
			return d
		}
	}
	for _, d := range devices {
		if pool.GetName() != "" && (d.GetLabel() == pool.GetName() || d.GetName() == pool.GetName()) {
			return d
		}
	}
	return nil
}

// buildStorageOverview joins the data plane's view of storage with what this
// process can reach, under one storage root.
func buildStorageOverview(root string, pools []*onyxv1.Pool, devices []*onyxv1.Device, capacity capacityFn) storageOverview {
	out := storageOverview{StorageRoot: root, Entries: []storageEntry{}, Warnings: []string{}}
	claimed := map[string]bool{}

	addMount := func(mountpoint string) {
		total, free, visible := capacity(mountpoint)
		if !visible {
			return // not a mount this process can reach
		}
		claimed[mountpoint] = true
		out.Entries = append(out.Entries, storageEntry{
			Name:       filepath.Base(mountpoint),
			Mountpoint: mountpoint,
			Mounted:    true,
			Visible:    true,
			TotalBytes: total,
			FreeBytes:  free,
			UsedBytes:  total - free,
			Source:     "device",
		})
	}

	// 1. Pools the data plane knows about, with their backing device's mount.
	for _, pool := range pools {
		dev := deviceForPool(pool, devices)
		entry := storageEntry{
			Name:       pool.GetName(),
			FSType:     pool.GetFsType(),
			State:      pool.GetState(),
			TotalBytes: int64(pool.GetTotalBytes()),
			UsedBytes:  int64(pool.GetUsedBytes()),
			Source:     "pool",
		}
		entry.FreeBytes = max64(entry.TotalBytes-entry.UsedBytes, 0)
		deviceMount := ""
		if dev != nil {
			deviceMount = dev.GetMountpoint()
			entry.State = orString(entry.State, dev.GetState())
		}
		// A pool remembers where it was mounted even when its device row is gone
		// (the stale-registry case the Remove action exists for). Falling back to
		// that is what keeps one pool from being reported twice — once as the
		// pool, once as an unclaimed mount under the root — and it is how the
		// path gets claimed below, so the mount is listed once rather than twice.
		entry.Mountpoint = deviceMount
		if entry.Mountpoint == "" {
			entry.Mountpoint = pool.GetMountpoint()
		}
		// Mounted means the data plane says a device is mounted there. A merely
		// remembered path is not evidence of a live mount, so it does not raise
		// the mount-namespace warning below.
		entry.Mounted = deviceMount != "" || dev.GetState() == "mounted"
		// A live filesystem is more accurate than a registry snapshot (btrfs
		// usage and ext4 used-bytes are both approximations there), so the
		// statfs numbers win whenever this process can reach the mount. When it
		// cannot, the data plane's own totals are all there is to report.
		if total, free, visible := capacity(entry.Mountpoint); visible {
			entry.Visible = true
			claimed[entry.Mountpoint] = true
			entry.TotalBytes, entry.FreeBytes, entry.UsedBytes = total, free, total-free
			// A filesystem this process can stat is not offline, whatever the
			// last registry scan concluded before the device record went away.
			entry.State = "online"
		}
		out.Entries = append(out.Entries, entry)
	}

	// 2. Mounts under the root that no pool claims — a hotplug device storaged
	// mounted, or a volume mounted by the host before Onyx started. A mount a
	// pool already accounts for is not listed twice.
	entries, err := os.ReadDir(root)
	if err != nil {
		out.Warnings = append(out.Warnings, "storage root "+root+" is not readable: "+err.Error())
	}
	for _, dir := range entries {
		if !dir.IsDir() {
			continue
		}
		path := filepath.Join(root, dir.Name())
		if claimed[path] {
			continue
		}
		addMount(path)
	}

	// 3. The root itself may be the mount: the classic single-pool install,
	// where /mnt/onyx is the filesystem rather than a parent of it. It is only
	// storage when the data plane says so — the root is a mount point in almost
	// every deployment (a system disk, a dataset, or the API container's bind
	// of the host path) and reporting its capacity is what put the host's root
	// filesystem behind the "Storage" headline while the pools were offline.
	// Whether the root is a mount at all is still capacity()'s call.
	if !claimed[root] && dataPlaneMountsRoot(root, pools, devices) {
		if total, free, visible := capacity(root); visible {
			out.Entries = append(out.Entries, storageEntry{
				Name: filepath.Base(root), Mountpoint: root, Mounted: true, Visible: true,
				TotalBytes: total, FreeBytes: free, UsedBytes: total - free, Source: "storage-root",
			})
		}
	}

	// Deterministic order: visible entries first (they are the usable ones),
	// then by name, so the headline is stable across requests.
	sort.SliceStable(out.Entries, func(i, j int) bool {
		if out.Entries[i].Visible != out.Entries[j].Visible {
			return out.Entries[i].Visible
		}
		return out.Entries[i].Name < out.Entries[j].Name
	})

	for i := range out.Entries {
		e := out.Entries[i]
		if !e.Visible {
			continue
		}
		if out.Primary == nil {
			primary := e
			out.Primary = &primary
		}
		out.TotalBytes += e.TotalBytes
		out.FreeBytes += e.FreeBytes
	}

	// Warnings last, so they read as the conclusion of the join.
	for _, e := range out.Entries {
		if e.Mounted && !e.Visible {
			out.Warnings = append(out.Warnings, e.Name+" is mounted at "+e.Mountpoint+
				" by the data plane but is not visible to onyx-api: the mount namespace is not shared. "+
				"Run `mount --make-shared "+root+"` on the host and restart the stack.")
		}
	}
	if len(out.Entries) == 0 {
		out.Warnings = append(out.Warnings, "No storage is mounted under "+root+". Create a pool from the Storage page or attach a device.")
	}
	return out
}

// handleStorageOverview serves GET /api/v1/storage/overview — the capacity and
// mount reality behind the Files page's storage card.
func (s *server) handleStorageOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	overview, err := s.storageOverview(ctx)
	if err != nil {
		s.writeGRPCError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

// storageOverview reads the data plane's storage view through onyx-core and
// joins it with this process's own filesystem view.
func (s *server) storageOverview(ctx context.Context) (storageOverview, error) {
	pools, err := s.core.ListPools(ctx, &onyxv1.ListPoolsRequest{})
	if err != nil {
		return storageOverview{}, err
	}
	devices, err := s.core.ListDevices(ctx, &onyxv1.ListDevicesRequest{})
	if err != nil {
		return storageOverview{}, err
	}
	return buildStorageOverview(s.filesRoot, pools.GetPools(), devices.GetDevices(), mountCapacity), nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func orString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
