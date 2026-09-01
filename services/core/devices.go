package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	onyxv1 "onyx.dev/onyx/proto/gen/go/onyx/v1"
)

// deviceShareSourcePrefix marks shares created automatically by the hotplug
// reconciler. The full source value embeds the device kname
// ("device:sdb1"), so automatic cleanup never touches user-created shares,
// even one that happens to share the name.
const deviceShareSourcePrefix = "device:"

// deviceNameRe mirrors the share name rule: device names come from volume
// labels / short UUIDs / kernel names and are sanitized by storaged, but the
// reconciler still validates before inserting anything into shares.
var deviceNameRe = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// shareRow is the minimal share record the reconciler reasons about.
type shareRow struct {
	name, path, source string
}

// reconcilePlan is the pure diff the reconciler applies: which mounted
// devices need an auto-share, and which auto-shares must go because their
// device is no longer attached on the onyx mount root. "Mounted" only
// counts when the mountpoint lives under mountRoot — a device mounted by the
// OS (/, /boot, ...) is the host system, not a hotplug drive, and is never
// shared. Pure so the behaviour is unit-testable.
func reconcilePlan(devices []*onyxv1.Device, existing []shareRow, mountRoot string) (toCreate []*onyxv1.Device, toDelete []string) {
	byName := map[string]shareRow{}
	for _, s := range existing {
		byName[s.name] = s
	}
	for _, d := range devices {
		cur, seen := byName[d.Name]
		switch {
		case seen && cur.source == deviceShareSourcePrefix+d.Kname:
			// Our auto-share: keep it while the device is mounted by onyx,
			// drop it the moment it isn't (detached or unmounted).
			if onyxMounted(mountRoot, d) {
				continue
			}
			toDelete = append(toDelete, d.Name)
		case seen:
			// A user/CLI share with this name — never touched automatically.
			continue
		default:
			if onyxMounted(mountRoot, d) {
				toCreate = append(toCreate, d)
			}
		}
	}
	return toCreate, toDelete
}

// onyxMounted reports whether the device is currently mounted as an onyx
// hotplug drive: state mounted AND mountpoint strictly inside mountRoot
// (a mountpoint equal to the root itself is not a share).
func onyxMounted(mountRoot string, d *onyxv1.Device) bool {
	if d.State != "mounted" || d.Mountpoint == "" {
		return false
	}
	root := strings.TrimSuffix(mountRoot, "/")
	rel, ok := strings.CutPrefix(d.Mountpoint, root)
	return ok && strings.HasPrefix(rel, "/") && len(rel) > 1
}

// deviceReconciler keeps the logical share model in lockstep with the data
// plane: when storaged mounts a hotplugged drive, core makes it a *share*
// (SMB + NFS) so clients can reach it immediately; when the drive is removed,
// the auto-share disappears with it. It only ever touches shares it created.
// Every mutation ends with a config sync so the daemons serve the new state.
type deviceReconciler struct {
	db        *sql.DB
	storaged  onyxv1.StoragedClient
	config    *configApplier
	mountRoot string
}

func (r *deviceReconciler) run(ctx context.Context, interval time.Duration) {
	// First pass immediately so drives already attached at boot become
	// shares without waiting for the ticker.
	r.reconcileOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileOnce(ctx)
		}
	}
}

func (r *deviceReconciler) reconcileOnce(ctx context.Context) {
	resp, err := r.storaged.ListDevices(ctx, &onyxv1.ListDevicesRequest{})
	if err != nil {
		if status.Code(err) == codes.Unavailable {
			slogWarn("device reconcile: storaged unavailable", "error", err)
		} else {
			slogWarn("device reconcile: list devices", "error", err)
		}
		return
	}

	existing := r.listShareRows()
	toCreate, toDelete := reconcilePlan(resp.Devices, existing, r.mountRoot)

	dirty := false
	for _, d := range toCreate {
		if r.createAutoShare(ctx, d) {
			dirty = true
		}
	}
	for _, name := range toDelete {
		dirty = r.deleteAutoShare(name) || dirty
	}
	if dirty && r.config != nil {
		// Rewrite smb.conf/exports and reload so the new share set is live.
		if err := r.config.apply(ctx); err != nil {
			slogWarn("device reconcile: apply daemon config", "error", err)
		}
	}
}

func (r *deviceReconciler) listShareRows() []shareRow {
	rows, err := r.db.Query(`SELECT name, path, source FROM shares`)
	if err != nil {
		slogWarn("device reconcile: query shares", "error", err)
		return nil
	}
	defer rows.Close()
	var out []shareRow
	for rows.Next() {
		var s shareRow
		if err := rows.Scan(&s.name, &s.path, &s.source); err != nil {
			slogWarn("device reconcile: scan share", "error", err)
			continue
		}
		out = append(out, s)
	}
	return out
}

// createAutoShare records the mounted device as a share (SMB + NFS). Returns
// true when a new share was actually inserted (so the caller knows the
// daemon config needs applying).
func (r *deviceReconciler) createAutoShare(ctx context.Context, d *onyxv1.Device) bool {
	if !deviceNameRe.MatchString(d.Name) || !onyxMounted(r.mountRoot, d) {
		slogWarn("device reconcile: skipping unshareable device", "name", d.Name, "state", d.State, "mountpoint", d.Mountpoint)
		return false
	}
	comment := fmt.Sprintf("Auto-attached %s (%s)", d.Path, orLabel(d))
	res, err := r.db.Exec(
		`INSERT OR IGNORE INTO shares (name, path, comment, readonly, protocols, source)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		d.Name, d.Mountpoint, comment, "smb,nfs", deviceShareSourcePrefix+d.Kname,
	)
	if err != nil {
		slogWarn("device reconcile: insert share", "device", d.Name, "error", err)
		return false
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false // already shared
	}
	slog.Info("device reconciled into share", "name", d.Name, "path", d.Mountpoint, "kname", d.Kname)
	return true
}

// deleteAutoShare removes exactly the share the reconciler created for a
// device; user shares are never touched. Returns true when a share was
// actually removed (daemon config needs applying).
func (r *deviceReconciler) deleteAutoShare(name string) bool {
	res, err := r.db.Exec(`DELETE FROM shares WHERE name = ? AND source LIKE 'device:%'`, name)
	if err != nil {
		slogWarn("device reconcile: delete share", "share", name, "error", err)
		return false
	}
	if n, _ := res.RowsAffected(); n > 0 {
		slog.Info("device detach reconciled: share removed", "name", name)
		return true
	}
	return false
}

func orLabel(d *onyxv1.Device) string {
	if d.Label != "" {
		return "label " + d.Label
	}
	if d.FsType != "" {
		return d.FsType + " filesystem"
	}
	return "block device"
}

// --- Core passthrough (the gateway only talks to onyx-core) ---

func (s *server) ListDevices(ctx context.Context, req *onyxv1.ListDevicesRequest) (*onyxv1.ListDevicesResponse, error) {
	return s.storaged.ListDevices(ctx, req)
}

func (s *server) GetDevice(ctx context.Context, req *onyxv1.GetDeviceRequest) (*onyxv1.Device, error) {
	return s.storaged.GetDevice(ctx, req)
}

func (s *server) MountDevice(ctx context.Context, req *onyxv1.MountDeviceRequest) (*onyxv1.Device, error) {
	return s.storaged.MountDevice(ctx, req)
}

func (s *server) UnmountDevice(ctx context.Context, req *onyxv1.UnmountDeviceRequest) (*onyxv1.Device, error) {
	return s.storaged.UnmountDevice(ctx, req)
}

// ListEvents pages the device audit trail, forwarded to onyx-storaged.
func (s *server) ListEvents(ctx context.Context, req *onyxv1.ListEventsRequest) (*onyxv1.ListEventsResponse, error) {
	return s.storaged.ListEvents(ctx, req)
}

// WatchDevices tunnels the live device event stream (attach/detach/health)
// from onyx-storaged to the HTTP gateway. The gateway client owns the
// cancellation, so when it disconnects the upstream stream ends too.
func (s *server) WatchDevices(req *onyxv1.WatchDevicesRequest, stream onyxv1.Core_WatchDevicesServer) error {
	sub, err := s.storaged.WatchDevices(stream.Context(), req)
	if err != nil {
		return err
	}
	for {
		ev, err := sub.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
}
