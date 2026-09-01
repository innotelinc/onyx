//! Hotplug detection and auto-attach (docs/design/05#7-disk-management).
//!
//! onyx-storaged never touches privileged operations directly: filesystem
//! probing (`lsblk`) and mounting (`mount`/`umount`) run *inside onyx-privd*
//! (the one root process) through allowlisted ops. This module owns the
//! policy — which devices to track, what to name them, when to attach them —
//! and keeps the SQLite `devices` registry in sync with the kernel's view
//! (/sys/class/block for attach/detach, lsblk for enrichment).

use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use tokio::sync::{broadcast, Mutex as AsyncMutex};
use tonic::transport::Channel;

use crate::onyx::privd_client::PrivdClient;
use crate::onyx::{Device, DeviceEvent, PrivOp, PrivRequest, PrivResponse};
use crate::registry::Registry;

/// Filesystem types we never attach: they are not user-visible storage.
/// Compared lowercased (blkid reports "Linux_raid_member" / "crypto_LUKS"
/// with unpredictable case).
const EXCLUDED_FS: &[&str] = &[
    "swap",
    "lvm2_member",
    "lvm_member",
    "linux_raid_member",
    "crypto_luks",
    "luks",
    "dm_crypt",
    "squashfs",
    "iso9660",
    "drbd",
    "zfs_member",
];

const ATTACHABLE_TYPES: &[&str] = &["part", "disk"];

/// Is this (type, filesystem) pair something onyx will ever attach?
/// Everything else (loop, dm, swapped, unformatted, ...) is ignored.
pub fn attachable(device_type: &str, fs_type: &str) -> bool {
    if !ATTACHABLE_TYPES.contains(&device_type) {
        return false;
    }
    let fs = fs_type.to_ascii_lowercase();
    if fs.is_empty() || EXCLUDED_FS.contains(&fs.as_str()) {
        return false;
    }
    true
}

/// One parsed `lsblk -n -P -b -o KNAME,TYPE,FSTYPE,LABEL,UUID,SIZE,MOUNTPOINT`
/// record.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DeviceInfo {
    pub kname: String,
    pub device_type: String,
    pub fs_type: String,
    pub label: String,
    pub uuid: String,
    pub size_bytes: u64,
    pub mountpoint: String,
}

/// Parse lsblk pairs output (`KNAME="sda" TYPE="disk" ...`). lsblk quotes
/// every value and escapes `"` and `\`; this parser mirrors that format
/// without pulling in a JSON dep.
pub fn parse_lsblk(output: &str) -> Vec<DeviceInfo> {
    let mut out = Vec::new();
    for line in output.lines() {
        let map = parse_pairs(line);
        let Some(kname) = map.get("KNAME") else { continue };
        out.push(DeviceInfo {
            kname: kname.to_string(),
            device_type: map.get("TYPE").cloned().unwrap_or_default(),
            fs_type: map.get("FSTYPE").cloned().unwrap_or_default(),
            label: map.get("LABEL").cloned().unwrap_or_default(),
            uuid: map.get("UUID").cloned().unwrap_or_default(),
            size_bytes: map.get("SIZE").and_then(|s| s.parse().ok()).unwrap_or(0),
            mountpoint: map.get("MOUNTPOINT").cloned().unwrap_or_default(),
        });
    }
    out
}

fn parse_pairs(line: &str) -> HashMap<String, String> {
    let mut map = HashMap::new();
    let b = line.as_bytes();
    let mut i = 0;
    let len = b.len();
    while i < len {
        while i < len && (b[i] == b' ' || b[i] == b'\t') {
            i += 1;
        }
        if i >= len {
            break;
        }
        let key_start = i;
        while i < len && b[i] != b'=' {
            i += 1;
        }
        if i >= len {
            break;
        }
        let key = &line[key_start..i];
        i += 1; // consume '='
        if i >= len || b[i] != b'"' {
            // malformed; skip to end of line start of next token
            while i < len && !(b[i] == b' ' && i + 1 < len && b[i + 1] != b'=') {
                i += 1;
            }
            continue;
        }
        i += 1; // consume opening quote
        let mut val = String::new();
        loop {
            if i >= len {
                break;
            }
            let c = b[i];
            if c == b'\\' && i + 1 < len {
                val.push(b[i + 1] as char);
                i += 2;
                continue;
            }
            if c == b'"' {
                i += 1;
                break;
            }
            val.push(c as char);
            i += 1;
        }
        map.insert(key.to_string(), val);
    }
    map
}

/// Slugify a fs label into a safe share/component name: lowercase ascii
/// alphanumerics, everything else becomes a separator, collapses runs.
/// ("USB STICK" -> "usb-stick"; "Data 1!" -> "data-1"; "ПРИВЕТ" -> "")
pub fn slugify(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_lowercase()
            } else {
                '-'
            }
        })
        .collect::<String>()
        .split('-')
        .filter(|p| !p.is_empty())
        .collect::<Vec<_>>()
        .join("-")
}

/// Stable display/share name for a device: label slug, else short UUID,
/// else the kernel name.
pub fn device_name(label: &str, uuid: &str, kname: &str) -> String {
    let slug = slugify(label);
    if !slug.is_empty() {
        return slug.chars().take(64).collect();
    }
    if !uuid.is_empty() {
        return uuid.chars().take(8).collect();
    }
    slugify(kname)
}

/// Make `base` unique against currently-used names: append -2, -3, ...
pub fn make_unique_name(used: &HashSet<String>, base: &str) -> String {
    if !used.contains(base) {
        return base.to_string();
    }
    let mut i = 2;
    loop {
        let candidate = format!("{base}-{i}");
        if !used.contains(&candidate) {
            return candidate;
        }
        i += 1;
    }
}

/// Whole-disk kname for a partition: "sdb1" -> "sdb", "nvme0n1p2" -> "nvme0n1",
/// "mmcblk0p1" -> "mmcblk0". Whole disks (and oddballs like "sr0") -> None.
pub fn parent_disk(kname: &str) -> Option<String> {
    let digits = kname.chars().rev().take_while(|c| c.is_ascii_digit()).count();
    if digits == 0 || digits == kname.len() {
        return None;
    }
    let mut base: String = kname.chars().take(kname.len() - digits).collect();
    if base.ends_with('p') && base.len() > 1 {
        base.pop();
    }
    Some(base)
}

/// Snapshot of /sys/class/block: kname -> removable flag (whole disks carry
/// the flag; partitions report false and inherit via parent_disk).
pub fn sysfs_removable(root: &Path) -> HashMap<String, bool> {
    let mut map = HashMap::new();
    let Ok(entries) = std::fs::read_dir(root) else {
        return map;
    };
    for entry in entries.flatten() {
        if !entry.path().is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let removable = std::fs::read_to_string(entry.path().join("removable"))
            .map(|s| s.trim() == "1")
            .unwrap_or(false);
        map.insert(name, removable);
    }
    map
}

/// True when a device (or its parent whole disk) is removable.
pub fn is_removable(snapshot: &HashMap<String, bool>, kname: &str) -> bool {
    // Whole disks carry the flag; partitions report false in sysfs and
    // inherit their parent disk's flag.
    if snapshot.get(kname) == Some(&true) {
        return true;
    }
    parent_disk(kname)
        .and_then(|p| snapshot.get(&p).copied())
        .unwrap_or(false)
}

/// Parse `smartctl -H -A` text output into (status, temperature_celsius).
/// Status: "ok" (PASSED), "degraded" (FAILED/FAILING), else "unknown"
/// (unsupported device, no SMART data, or smartctl error).
pub fn parse_smartctl(out: &str, exit_code: i32) -> (String, Option<u32>) {
    let lower = out.to_ascii_lowercase();
    let status = if lower.contains("failed") || lower.contains("failing") {
        "degraded"
    } else if lower.contains("passed") {
        "ok"
    } else if exit_code == 0 && !out.trim().is_empty() && !lower.contains("unavailable") {
        "unknown" // exit 0 but no verdict line (e.g. odd device); still record
    } else {
        "unknown"
    };
    let mut temp = None;
    for line in out.lines() {
        let toks: Vec<&str> = line.split_whitespace().collect();
        if toks.len() >= 10 && toks[1] == "Temperature_Celsius" {
            for t in toks.iter().skip(9) {
                // RAW_VALUE starts at index 9; some drives annotate it,
                // e.g. "38 (Min/Max 24/40)" — take the first plain number.
                if let Ok(v) = t.parse::<u32>() {
                    temp = Some(v);
                    break;
                }
            }
            break;
        }
    }
    (status.to_string(), temp)
}

/// DeviceManager owns the hotplug policy: it scans, names, auto-attaches
/// eligible devices via onyx-privd, and reacts to removals. It is shared by
/// the background watcher loop and the MountDevice/UnmountDevice RPCs, so
/// explicit user actions and automatic detection serialize on the same code.
pub struct DeviceManager {
    pub registry: Arc<Registry>,
    privd: Arc<AsyncMutex<PrivdClient<Channel>>>,
    pub mount_root: PathBuf,
    /// removable | all | none
    pub auto_attach: String,
    /// Forget detached records older than this (minutes).
    pub detached_ttl_minutes: i64,
    /// uid/gid/umask for fat-family mounts (vfat/exfat/... have no real
    /// ownership; without these every file ends up root-only and SMB/NFS
    /// write access is impossible). Configurable so deployments can point at
    /// the onyx share user/group.
    pub mount_uid: u32,
    pub mount_gid: u32,
    pub fat_umask: u32,
    /// Live audit trail fan-out: every attach/detach/health/error event is
    /// persisted in the registry AND broadcast to WatchDevices subscribers.
    pub events: broadcast::Sender<DeviceEvent>,
    ops: Mutex<HashSet<String>>,
}

/// Filesystem types that carry no POSIX ownership on disk; they are mounted
/// with uid/gid/umask so the share's users actually own the files.
fn requires_ownership_opts(fs_type: &str) -> bool {
    matches!(
        fs_type.to_ascii_lowercase().as_str(),
        "vfat" | "exfat" | "msdos" | "fat" | "fat16" | "fat32"
    )
}

impl DeviceManager {
    pub fn new(
        registry: Arc<Registry>,
        privd: Arc<AsyncMutex<PrivdClient<Channel>>>,
        mount_root: PathBuf,
        auto_attach: &str,
        detached_ttl_minutes: i64,
        mount_uid: u32,
        mount_gid: u32,
        fat_umask: u32,
        events: broadcast::Sender<DeviceEvent>,
    ) -> Self {
        DeviceManager {
            registry,
            privd,
            mount_root,
            auto_attach: auto_attach.to_string(),
            detached_ttl_minutes,
            mount_uid,
            mount_gid,
            fat_umask,
            events,
            ops: Mutex::new(HashSet::new()),
        }
    }

    /// Persist an audit event and broadcast it to live subscribers.
    fn emit(&self, kname: &str, name: &str, event: &str, detail: &str) {
        let ev = DeviceEvent {
            id: 0,
            ts: String::new(),
            kname: kname.to_string(),
            name: name.to_string(),
            event: event.to_string(),
            detail: detail.to_string(),
        };
        match self.registry.push_event(&ev) {
            Ok(live) => {
                let _ = self.events.send(live); // no subscribers is fine
            }
            Err(e) => tracing::warn!(kname, event, error = %e, "audit event persist failed"),
        }
    }

    /// Mount options for one filesystem. Fat-family filesystems get
    /// uid/gid/umask (privd allowlists exactly these tokens); anything with
    /// real on-disk ownership (ext4/btrfs/xfs/...) is mounted plain so its
    /// own permissions keep working.
    pub fn fat_mount_options(&self, fs_type: &str) -> Vec<String> {
        if !requires_ownership_opts(fs_type) {
            return Vec::new();
        }
        vec![
            format!("uid={}", self.mount_uid),
            format!("gid={}", self.mount_gid),
            format!("umask={:03o}", self.fat_umask),
        ]
    }

    // --- privd plumbing ---

    async fn run_op(&self, op: PrivOp, args: Vec<String>) -> Result<PrivResponse, String> {
        let mut privd = self.privd.lock().await;
        privd
            .run(PrivRequest { op: op as i32, args })
            .await
            .map(|r| r.into_inner())
            .map_err(|e| format!("privd {op:?}: {e}"))
    }

    async fn lsblk(&self) -> Result<String, String> {
        let resp = self.run_op(PrivOp::LsblkRaw, Vec::new()).await?;
        if resp.exit_code != 0 {
            return Err(format!("lsblk exited {}", resp.exit_code));
        }
        Ok(String::from_utf8_lossy(&resp.stdout).into_owned())
    }

    async fn mount_block(&self, device: &str, mountpoint: &str, options: &[String]) -> Result<(), String> {
        let mut args = vec![device.to_string(), mountpoint.to_string()];
        args.extend(options.iter().cloned());
        let resp = self.run_op(PrivOp::MountBlock, args).await?;
        if resp.exit_code != 0 {
            let stderr = String::from_utf8_lossy(&resp.stderr);
            return Err(format!(
                "mount {device} at {mountpoint} ({}) failed: {}",
                options.join(","),
                stderr.trim()
            ));
        }
        Ok(())
    }

    async fn unmount_block(&self, mountpoint: &str) -> Result<(), String> {
        let resp = self
            .run_op(PrivOp::UnmountBlock, vec![mountpoint.to_string()])
            .await?;
        if resp.exit_code != 0 {
            let stderr = String::from_utf8_lossy(&resp.stderr);
            return Err(format!("umount {mountpoint} failed: {}", stderr.trim()));
        }
        Ok(())
    }

    // --- attach/unmount primitives ---

    fn begin_attach(&self, kname: &str) -> bool {
        self.ops.lock().unwrap().insert(kname.to_string())
    }

    fn end_attach(&self, kname: &str) {
        self.ops.lock().unwrap().remove(kname);
    }

    /// The auto-attach reason for a device under the current policy:
    /// "all", "removable" (whole-disk flag), or "manual" (not eligible).
    fn auto_reason(&self, removable: bool) -> String {
        match self.auto_attach.as_str() {
            "all" => "all".to_string(),
            a if a == "removable" && removable => "removable".to_string(),
            _ => "manual".to_string(),
        }
    }

    /// Mount one device at <mount_root>/<name> through privd and record it.
    /// The caller holds the attach-op guard.
    async fn mount_and_record(&self, dev: &Device) -> Result<(), String> {
        let mountpoint = self.mount_root.join(&dev.name);
        let options = self.fat_mount_options(&dev.fs_type);
        self.mount_block(&dev.path, &mountpoint.display().to_string(), &options)
            .await?;
        let mountpoint_str = mountpoint.display().to_string();
        self.registry
            .set_mounted(&dev.kname, &mountpoint_str)
            .map_err(|e| format!("registry: {e}"))?;
        tracing::info!(
            kname = %dev.kname,
            mountpoint = %mountpoint_str,
            options = %options.join(","),
            "device attached"
        );
        self.emit(&dev.kname, &dev.name, "attach", &mountpoint_str);
        // Quick SMART probe right away so health is visible immediately.
        self.check_health(&dev.kname).await;
        Ok(())
    }

    /// Run one SMART health probe for a device and record + broadcast the
    /// result. Non-fatal: unsupported devices end up "unknown" and the
    /// periodic sweep retries later.
    pub async fn check_health(&self, name_or_kname: &str) {
        let dev = match self.registry.get_device(name_or_kname) {
            Ok(Some(d)) => d,
            _ => return,
        };
        if dev.state == "detached" {
            return;
        }
        let resp = match self.run_op(PrivOp::SmartInfoRaw, vec![dev.path.clone()]).await {
            Ok(r) => r,
            Err(e) => {
                tracing::warn!(kname = %dev.kname, error = %e, "smartctl via privd failed");
                return;
            }
        };
        let (status, temp) =
            parse_smartctl(&String::from_utf8_lossy(&resp.stdout), resp.exit_code);
        if let Err(e) = self.registry.set_health(&dev.kname, &status, temp) {
            tracing::warn!(kname = %dev.kname, error = %e, "health record failed");
            return;
        }
        let detail = match temp {
            Some(t) => format!("{status} temp={t}C"),
            None => status.clone(),
        };
        self.emit(&dev.kname, &dev.name, "health", &detail);
    }

    /// Probe every attached device (periodic sweep from the watch loop).
    pub async fn health_sweep(&self) {
        let devices = match self.registry.list_devices() {
            Ok(d) => d,
            Err(e) => {
                tracing::warn!(error = %e, "health sweep: registry read failed");
                return;
            }
        };
        for d in devices {
            if d.state != "detached" {
                self.check_health(&d.kname).await;
            }
        }
    }

    /// Attach a device by stable name or kernel name. Idempotent: already
    /// mounted devices are returned as-is.
    pub async fn attach(&self, name_or_kname: &str) -> Result<Device, String> {
        let dev = self
            .registry
            .get_device(name_or_kname)
            .map_err(|e| format!("registry: {e}"))?
            .ok_or_else(|| format!("device {name_or_kname} not found"))?;
        if dev.state == "detached" {
            return Err(format!("device {} is detached", dev.name));
        }
        // A manual attach reverses a previous `device detach`: clear the
        // opt-out and restore the fresh auto-attach policy so a future replug
        // mounts the drive again.
        let auto = self.auto_reason(dev.removable);
        if let Err(e) = self.registry.mark_attached_by_user(&dev.kname, &auto) {
            return Err(format!("registry: {e}"));
        }
        if !dev.mountpoint.is_empty() {
            return Ok(dev); // already mounted
        }
        if !self.begin_attach(&dev.kname) {
            return Err(format!("attach already in progress for {}", dev.kname));
        }
        let result = self.mount_and_record(&dev).await;
        self.end_attach(&dev.kname);
        if let Err(e) = &result {
            self.emit(&dev.kname, &dev.name, "error", &e.clone());
        }
        result?;
        let updated = self
            .registry
            .get_device(name_or_kname)
            .map_err(|e| format!("registry: {e}"))?
            .ok_or_else(|| format!("device {name_or_kname} vanished"))?;
        Ok(updated)
    }

    /// Detach a device onyx mounted (or is about to mount). Devices mounted
    /// elsewhere are left alone. Idempotent for already-detached devs.
    ///
    /// Detaching also pins the device out of auto-attach (`auto = 'manual'`)
    /// for as long as it is known: a still-plugged-in drive must not be
    /// re-mounted by the next watcher tick. `attach()` reverses the pin.
    pub async fn detach(&self, name_or_kname: &str) -> Result<Device, String> {
        let dev = self
            .registry
            .get_device(name_or_kname)
            .map_err(|e| format!("registry: {e}"))?
            .ok_or_else(|| format!("device {name_or_kname} not found"))?;
        if dev.state == "detached" {
            // Already physically unplugged: still record the intent so a
            // future replug does not auto-attach until an explicit attach.
            self.registry
                .mark_detached_by_user(&dev.kname)
                .map_err(|e| format!("registry: {e}"))?;
            return Ok(dev);
        }
        if !dev.mountpoint.is_empty() && !Path::new(&dev.mountpoint).starts_with(self.mount_root.as_path()) {
            return Err(format!(
                "device {} is mounted at {} (not by onyx); unmount manually",
                dev.name, dev.mountpoint
            ));
        }
        if dev.mountpoint.is_empty() {
            // Not mounted: nothing to unmount, but record the user's intent so
            // the watcher keeps the drive detached while it is plugged in.
            self.registry
                .mark_detached_by_user(&dev.kname)
                .map_err(|e| format!("registry: {e}"))?;
        } else {
            if let Err(e) = self.unmount_block(&dev.mountpoint).await {
                tracing::warn!(kname = %dev.kname, mountpoint = %dev.mountpoint, error = %e, "unmount failed");
                return Err(e);
            }
            self.registry
                .set_unmounted(&dev.kname)
                .map_err(|e| format!("registry: {e}"))?;
            self.registry
                .mark_detached_by_user(&dev.kname)
                .map_err(|e| format!("registry: {e}"))?;
        }
        tracing::info!(kname = %dev.kname, "device detached");
        self.emit(&dev.kname, &dev.name, "detach", "detached (auto off)");
        let updated = self
            .registry
            .get_device(name_or_kname)
            .map_err(|e| format!("registry: {e}"))?
            .ok_or_else(|| format!("device {name_or_kname} vanished"))?;
        Ok(updated)
    }

    /// A device disappeared from /sys: unmount ours (best effort) and mark it
    /// detached so the control plane can remove its share.
    pub async fn on_removal(&self, kname: &str) {
        let prev = match self.registry.mark_detached(kname) {
            Ok(Some(dev)) => dev,
            Ok(None) => return, // never tracked (loop, swap, ...)
            Err(e) => {
                tracing::warn!(kname, error = %e, "registry mark_detached failed");
                return;
            }
        };
        if !prev.mountpoint.is_empty() && Path::new(&prev.mountpoint).starts_with(self.mount_root.as_path()) {
            if let Err(e) = self.unmount_block(&prev.mountpoint).await {
                tracing::warn!(kname, mountpoint = %prev.mountpoint, error = %e, "unmount after unplug failed");
            } else {
                tracing::info!(kname, mountpoint = %prev.mountpoint, "unmounted removed device");
            }
        }
        tracing::info!(kname, "device removed");
        self.emit(kname, &prev.name, "detach", "unplugged");
    }

    /// One watch tick. Returns the new /sys snapshot for the next diff.
    /// Failures are logged, never fatal: the registry keeps its last view.
    pub async fn tick(&self, prev: &HashSet<String>, sysfs_root: &Path) -> HashSet<String> {
        let snapshot = sysfs_removable(sysfs_root);
        // lsblk is the live kernel view of devices (presence AND mountpoints);
        // sysfs only carries the removable flag for whole disks.
        let mut present: Option<HashSet<String>> = None;
        match self.lsblk().await {
            Ok(out) => {
                let mut seen = HashSet::new();
                for info in parse_lsblk(&out) {
                    seen.insert(info.kname.clone());
                    if !attachable(&info.device_type, &info.fs_type) {
                        continue;
                    }
                    let removable = is_removable(&snapshot, &info.kname);
                    let mountpoint = info.mountpoint.trim().to_string();
                    // Keep the device's own existing name (label changes or a
                    // replug must not churn the mountpoint/share); only avoid
                    // colliding with *other* devices' names.
                    let name = {
                        let used: HashSet<String> = self
                            .registry
                            .device_names()
                            .map(|v| {
                                v.into_iter()
                                    .filter(|(k, _)| *k != info.kname)
                                    .map(|(_, n)| n)
                                    .collect()
                            })
                            .unwrap_or_default();
                        make_unique_name(&used, &device_name(&info.label, &info.uuid, &info.kname))
                    };
                    let auto = self.auto_reason(removable);
                    let auto_attach_eligible = auto != "manual";
                    let dev = Device {
                        name,
                        kname: info.kname.clone(),
                        path: format!("/dev/{}", info.kname),
                        r#type: info.device_type.clone(),
                        fs_type: info.fs_type.clone(),
                        label: info.label.clone(),
                        uuid: info.uuid.clone(),
                        size_bytes: info.size_bytes,
                        mountpoint,
                        removable,
                        state: if info.mountpoint.trim().is_empty() { "attached".to_string() } else { "mounted".to_string() },
                        auto,
                        health_status: String::new(),
                        temperature_c: 0,
                    };
                    if let Err(e) = self.registry.upsert_device(&dev) {
                        tracing::warn!(kname = %info.kname, error = %e, "device registry upsert failed");
                        continue;
                    }
                    // Auto-attach: eligible under policy, not mounted — and the
                    // user must not have pinned the device out (`device detach`
                    // sets user_detached, which the upsert keeps as auto='manual'
                    // and so never mounts). Fresh-scan opinions don't override
                    // that persisted intent.
                    let user_detached = self.registry.is_user_detached(&info.kname).unwrap_or(false);
                    if auto_attach_eligible && !user_detached && dev.state != "mounted" && self.begin_attach(&dev.kname) {
                        let res = self.mount_and_record(&dev).await;
                        self.end_attach(&dev.kname);
                        match res {
                            Ok(()) => {}
                            Err(e) => {
                                tracing::warn!(kname = %info.kname, error = %e, "auto-attach failed");
                                self.emit(&dev.kname, &dev.name, "error", &e);
                            }
                        }
                    }
                }
                present = Some(seen);
            }
            Err(e) => tracing::warn!(error = %e, "lsblk scan failed; falling back to sysfs diff"),
        }

        // Removal detection. Primary signal: the lsblk listing — a device
        // unplugged from the kernel is gone from both /sys and lsblk, so any
        // tracked device missing from lsblk is gone. When lsblk is
        // unavailable, fall back to the previous /sys snapshot diff.
        match &present {
            Some(seen) => {
                if let Ok(devices) = self.registry.list_devices() {
                    for d in devices {
                        if d.state != "detached" && !seen.contains(&d.kname) {
                            self.on_removal(&d.kname).await;
                        }
                    }
                }
            }
            None => {
                let sysfs_keys: HashSet<String> = snapshot.keys().cloned().collect();
                for gone in prev.difference(&sysfs_keys) {
                    self.on_removal(gone).await;
                }
            }
        }
        if let Ok(n) = self.registry.prune_detached(self.detached_ttl_minutes) {
            if n > 0 {
                tracing::info!(pruned = n, "pruned old detached device records");
            }
        }
        snapshot.keys().cloned().collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_lsblk_pairs() {
        let out = "KNAME=\"sda\" TYPE=\"disk\" FSTYPE=\"\" LABEL=\"\" UUID=\"\" SIZE=\"53687091200\" MOUNTPOINT=\"\"\n\
                   KNAME=\"sda1\" TYPE=\"part\" FSTYPE=\"ext4\" LABEL=\"root\" UUID=\"abcdef12-3456-7890-abcd-ef1234567890\" SIZE=\"1127219200\" MOUNTPOINT=\"/\"\n\
                   KNAME=\"sdb1\" TYPE=\"part\" FSTYPE=\"vfat\" LABEL=\"USB STICK\" UUID=\"1234-5678\" SIZE=\"12884901888\" MOUNTPOINT=\"/mnt/onyx/usb-stick\"\n\
                   KNAME=\"nvme0n1p1\" TYPE=\"part\" FSTYPE=\"ntfs\" LABEL=\"CAKE\\\"BOX\\\\1\" UUID=\"ABC123DEF\" SIZE=\"4096\" MOUNTPOINT=\"\"\n";
        let devs = parse_lsblk(out);
        assert_eq!(devs.len(), 4);
        assert_eq!(devs[0].kname, "sda");
        assert_eq!(devs[0].fs_type, "");
        assert_eq!(devs[1].label, "root");
        assert_eq!(devs[1].mountpoint, "/");
        assert_eq!(devs[2].label, "USB STICK");
        assert_eq!(devs[2].mountpoint, "/mnt/onyx/usb-stick");
        assert_eq!(devs[2].size_bytes, 12884901888);
        assert_eq!(devs[3].label, "CAKE\"BOX\\1");
        assert_eq!(devs[3].uuid, "ABC123DEF");
    }

    #[test]
    fn parses_empty_and_garbage() {
        assert!(parse_lsblk("").is_empty());
        assert!(parse_lsblk("KNAME sda no-equals").is_empty());
    }

    #[test]
    fn slugifies_labels() {
        assert_eq!(slugify("USB STICK"), "usb-stick");
        assert_eq!(slugify("Data 1!"), "data-1");
        assert_eq!(slugify("Café"), "caf");
        assert_eq!(slugify("привет"), "");
        assert_eq!(slugify(""), "");
        assert_eq!(slugify("a--b__c"), "a-b-c");
    }

    #[test]
    fn device_names_prefer_label_then_uuid_then_kname() {
        assert_eq!(device_name("USB STICK", "1234-5678", "sdb1"), "usb-stick");
        assert_eq!(device_name("", "1234-5678", "sdb1"), "1234-567"); // first 8 chars
        assert_eq!(device_name("", "", "sdb1"), "sdb1");
        assert_eq!(device_name("", "1234-5678-9abc", "sdb1"), "1234-567");
        // overlong labels truncate to share-name limit
        let long = "x".repeat(100);
        assert_eq!(device_name(&long, "", "sdb1").len(), 64);
    }

    #[test]
    fn unique_names_append_suffix() {
        let used: HashSet<String> = ["usb-stick".into(), "usb-stick-2".into()].into_iter().collect();
        assert_eq!(make_unique_name(&used, "usb-stick"), "usb-stick-3");
        assert_eq!(make_unique_name(&used, "other"), "other");
    }

    #[test]
    fn attachability_policy() {
        assert!(attachable("part", "vfat"));
        assert!(attachable("disk", "ext4"));
        assert!(attachable("part", "exfat"));
        assert!(attachable("part", "btrfs"));
        assert!(!attachable("part", "")); // unformatted
        assert!(!attachable("disk", "")); // unformatted whole disk
        assert!(!attachable("part", "swap"));
        assert!(!attachable("part", "LVM2_member"));
        assert!(!attachable("part", "linux_raid_member"));
        assert!(!attachable("loop", "ext4")); // loopback
        assert!(!attachable("rom", "iso9660")); // cdrom
        assert!(!attachable("dm", "ext4")); // device mapper
    }

    #[tokio::test]
    async fn fat_mount_options_applied_only_to_ownership_less_fs() {
        let m = |uid: u32, gid: u32, umask: u32| -> DeviceManager {
            // registry/privd are not touched by fat_mount_options; dummy arcs
            // typed from a never-opened registry.
            let reg = registry_for_test();
            DeviceManager {
                registry: reg,
                privd: Arc::new(AsyncMutex::new(privd_dummy())),
                mount_root: PathBuf::from("/mnt/onyx"),
                auto_attach: "removable".into(),
                detached_ttl_minutes: 10,
                mount_uid: uid,
                mount_gid: gid,
                fat_umask: umask,
                events: broadcast::channel(1).0,
                ops: Mutex::new(HashSet::new()),
            }
        };
        let mgr = m(1000, 100, 0o002);
        assert_eq!(
            mgr.fat_mount_options("vfat"),
            vec!["uid=1000".to_string(), "gid=100".to_string(), "umask=002".to_string()]
        );
        assert_eq!(
            mgr.fat_mount_options("exfat"),
            vec!["uid=1000".to_string(), "gid=100".to_string(), "umask=002".to_string()]
        );
        assert_eq!(mgr.fat_mount_options("msdos").len(), 3);
        // ownership-carrying filesystems get no magic options
        assert_eq!(mgr.fat_mount_options("ext4"), Vec::<String>::new() as Vec<String>);
        assert_eq!(mgr.fat_mount_options("btrfs"), Vec::<String>::new() as Vec<String>);
        assert_eq!(mgr.fat_mount_options("xfs"), Vec::<String>::new() as Vec<String>);
        assert_eq!(mgr.fat_mount_options("ntfs"), Vec::<String>::new() as Vec<String>);
        assert_eq!(mgr.fat_mount_options(""), Vec::<String>::new() as Vec<String>);
        // custom deployment config shows up in the options
        let custom = m(1500, 2000, 0o007);
        assert_eq!(
            custom.fat_mount_options("vfat"),
            vec!["uid=1500".to_string(), "gid=2000".to_string(), "umask=007".to_string()]
        );
    }

    fn registry_for_test() -> Arc<Registry> {
        let dir = std::env::temp_dir().join(format!(
            "onyx-reg-test-{}-{}",
            std::process::id(),
            std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_nanos()
        ));
        let reg = Registry::open(&dir).expect("open registry");
        let _ = std::fs::remove_dir_all(&dir);
        reg
    }

    fn privd_dummy() -> PrivdClient<Channel> {
        // Parsed but never dialed during the test (fat_mount_options does not
        // touch the channel); connect_lazy returns a channel without
        // performing any I/O.
        let endpoint = Channel::from_shared("http://127.0.0.1:1").expect("parses");
        PrivdClient::new(endpoint.connect_lazy())
    }

    #[test]
    fn parses_smartctl_output() {
        let (status, temp) = parse_smartctl(
            "SMART overall-health self-assessment test result: PASSED\n\
             194 Temperature_Celsius 0x0022 038 038 000 - - - 38\n\
             SMART Error Log Version: 1\n",
            0,
        );
        assert_eq!(status, "ok");
        assert_eq!(temp, Some(38));

        let (status, temp) = parse_smartctl(
            "SMART overall-health self-assessment test result: FAILED!\n\
             SMART Error Log Version: 1\n",
            1,
        );
        assert_eq!(status, "degraded");
        assert!(temp.is_none());

        // unsupported device: smartctl exits non-zero and reports it
        let (status, _) = parse_smartctl("Device does not support SMART\n", 2);
        assert_eq!(status, "unknown");

        // PASSED wins over exit-code quirks; annotated raw values parse
        let (status, temp) = parse_smartctl(
            "SMART overall-health self-assessment test result: PASSED\n\
             194 Temperature_Celsius 0x0022 038 038 000 - - - 38 (Min/Max 24/40)\n",
            1,
        );
        assert_eq!(status, "ok");
        assert_eq!(temp, Some(38));
    }

    #[test]
    fn parent_disk_mapping() {
        assert_eq!(parent_disk("sdb1"), Some("sdb".to_string()));
        assert_eq!(parent_disk("nvme0n1p2"), Some("nvme0n1".to_string()));
        assert_eq!(parent_disk("mmcblk0p1"), Some("mmcblk0".to_string()));
        assert_eq!(parent_disk("sdb"), None);
        assert_eq!(parent_disk("sda1s"), None); // trailing letter, not partition
    }

    #[test]
    fn removable_inherited_from_parent_disk() {
        let root = std::env::temp_dir().join(format!(
            "onyx-sysfs-test-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(root.join("sda")).unwrap();
        std::fs::create_dir_all(root.join("sdb")).unwrap();
        std::fs::create_dir_all(root.join("sdb1")).unwrap(); // partition: no removable file
        std::fs::write(root.join("sda/removable"), "0").unwrap();
        std::fs::write(root.join("sdb/removable"), "1").unwrap();

        let map = sysfs_removable(&root);
        assert_eq!(map.get("sda"), Some(&false));
        assert_eq!(map.get("sdb"), Some(&true));
        assert_eq!(map.get("sdb1"), Some(&false));
        assert!(!is_removable(&map, "sda"));
        assert!(is_removable(&map, "sdb1"));
        assert!(is_removable(&map, "sdb"));
        assert!(!is_removable(&map, "nvme0n1"));
        let _ = std::fs::remove_dir_all(&root);
    }
}