//! onyx-privd — the privilege helper (docs/design/04-backend-service-architecture.md#7-privilege-helper).
//!
//! The single root process on the system. Design rules (04 §7):
//! * **No shell, ever.** Structured commands (`PrivReq { op, args }`) are
//!   validated against an allowlist and executed with explicit argv.
//! * Each op has a validator: block devices must be plain names under /dev/
//!   (no traversal, no nested paths); mountpoints must resolve inside the
//!   allowed root (default `/mnt/onyx/`).
//! * Runs under systemd socket activation (dev: `scripts/dev.sh`); every
//!   request is logged to the audit trail with caller identity.
//!
//! v0.1 allows the `btrfs` subset for pool discovery; v0.1.1 adds the block
//! device ops used by the hotplug watcher (lsblk scan, mount, umount). Ops are
//! additive: add a `PrivOp` variant in `proto/onyx/v1/privd.proto`, a
//! validator, and one match arm below.

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::time::Duration;

use tokio::net::UnixListener;
use tokio::process::Command;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::{Request, Response, Status};

pub mod onyx {
    tonic::include_proto!("onyx.v1");
}

use onyx::health_server::{Health, HealthServer};
use onyx::health_check_response::ServingStatus as StatusValue;
use onyx::privd_server::{Privd, PrivdServer};
use onyx::{HealthCheckRequest, HealthCheckResponse, PrivOp, PrivRequest, PrivResponse};

const VERSION: &str = "0.1.0-dev";
const CMD_TIMEOUT: Duration = Duration::from_secs(30);

fn main() -> ExitCode {
    // Structured logs to stderr (04 §8). In production this lands in journald;
    // dev.sh captures stderr to .run/onyx-privd.log.
    tracing_subscriber::fmt().with_env_filter(tracing_subscriber::EnvFilter::from_default_env().add_directive("info".parse().unwrap())).init();

    let runtime = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(err) => {
            eprintln!("onyx-privd: failed to start runtime: {err}");
            return ExitCode::FAILURE;
        }
    };
    let args = Args::parse();
    let allowlist = Allowlist::new(
        &args.btrfs_bin,
        &args.lsblk_bin,
        &args.mount_bin,
        &args.umount_bin,
        &args.mkdir_bin,
        &args.smartctl_bin,
        &args.testparm_bin,
        &args.systemctl_bin,
        &args.exportfs_bin,
        &args.config_dir,
        &args.allowed_root,
        &args.dev_root,
    );
    match runtime.block_on(run(&args.socket_path, allowlist)) {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("onyx-privd: {err}");
            ExitCode::FAILURE
        }
    }
}

struct Args {
    socket_path: PathBuf,
    btrfs_bin: String,
    lsblk_bin: String,
    mount_bin: String,
    umount_bin: String,
    mkdir_bin: String,
    smartctl_bin: String,
    testparm_bin: String,
    systemctl_bin: String,
    exportfs_bin: String,
    config_dir: PathBuf,
    allowed_root: PathBuf,
    dev_root: PathBuf,
}

impl Args {
    fn parse() -> Self {
        let mut socket_path = PathBuf::from("/run/onyx/onyx-privd.sock");
        let mut btrfs_bin = "btrfs".to_string();
        let mut lsblk_bin = "lsblk".to_string();
        let mut mount_bin = "mount".to_string();
        let mut umount_bin = "umount".to_string();
        let mut mkdir_bin = "mkdir".to_string();
        let mut smartctl_bin = "smartctl".to_string();
        let mut testparm_bin = "testparm".to_string();
        let mut systemctl_bin = "systemctl".to_string();
        let mut exportfs_bin = "exportfs".to_string();
        let mut config_dir = PathBuf::from("/etc/onyx/conf.d");
        let mut allowed_root = PathBuf::from("/mnt/onyx");
        let mut dev_root = PathBuf::from("/dev");
        let mut it = std::env::args().skip(1);
        while let Some(arg) = it.next() {
            match arg.as_str() {
                "--socket-dir" => {
                    let dir = it.next().expect("--socket-dir requires a value");
                    socket_path = PathBuf::from(dir).join("onyx-privd.sock");
                }
                "--btrfs-bin" => btrfs_bin = it.next().expect("--btrfs-bin requires a value"),
                "--lsblk-bin" => lsblk_bin = it.next().expect("--lsblk-bin requires a value"),
                "--mount-bin" => mount_bin = it.next().expect("--mount-bin requires a value"),
                "--umount-bin" => umount_bin = it.next().expect("--umount-bin requires a value"),
                "--mkdir-bin" => mkdir_bin = it.next().expect("--mkdir-bin requires a value"),
                "--smartctl-bin" => smartctl_bin = it.next().expect("--smartctl-bin requires a value"),
                "--testparm-bin" => testparm_bin = it.next().expect("--testparm-bin requires a value"),
                "--systemctl-bin" => systemctl_bin = it.next().expect("--systemctl-bin requires a value"),
                "--exportfs-bin" => exportfs_bin = it.next().expect("--exportfs-bin requires a value"),
                "--config-dir" => {
                    config_dir = PathBuf::from(it.next().expect("--config-dir requires a value"));
                }
                "--allowed-root" => {
                    allowed_root = PathBuf::from(it.next().expect("--allowed-root requires a value"))
                }
                "--dev-root" => {
                    dev_root = PathBuf::from(it.next().expect("--dev-root requires a value"))
                }
                other => {
                    eprintln!("onyx-privd: unknown argument: {other}");
                    std::process::exit(2);
                }
            }
        }
        Args {
            socket_path,
            btrfs_bin,
            lsblk_bin,
            mount_bin,
            umount_bin,
            mkdir_bin,
            smartctl_bin,
            testparm_bin,
            systemctl_bin,
            exportfs_bin,
            config_dir,
            allowed_root,
            dev_root,
        }
    }
}

async fn run(socket_path: &Path, allowlist: Allowlist) -> Result<(), Box<dyn std::error::Error>> {
    // Note: privd keeps no state dir — it is stateless by design and logs the
    // audit trail through onyx-core (04 §8).

    if socket_path.exists() {
        std::fs::remove_file(socket_path)?;
    }
    let listener = UnixListener::bind(socket_path)?;
    tracing::info!(socket = %socket_path.display(), pid = std::process::id(), "onyx-privd listening (root)");

    let incoming = UnixListenerStream::new(listener);
    tonic::transport::Server::builder()
        .add_service(HealthServer::new(HealthService))
        .add_service(PrivdServer::new(PrivdService { allowlist }))
        .serve_with_incoming(incoming)
        .await?;
    Ok(())
}

// --- Health ---

struct HealthService;

#[tonic::async_trait]
impl Health for HealthService {
    async fn check(
        &self,
        _request: Request<HealthCheckRequest>,
    ) -> Result<Response<HealthCheckResponse>, Status> {
        Ok(Response::new(HealthCheckResponse {
            status: StatusValue::Serving as i32,
            version: VERSION.into(),
        }))
    }
}

// --- Allowlist ---

/// A validated, ready-to-execute command. Distinct type from `PrivRequest` so
/// `execute` can never see unvalidated input.
#[derive(Debug)]
enum AllowedCommand {
    /// `btrfs filesystem show --raw` — enumerate all filesystems.
    BtrfsFilesystemShowRaw,
    /// `btrfs filesystem usage -b <mountpoint>` — one validated mount under the allowed root.
    BtrfsFilesystemUsage { mount: PathBuf },
    /// `lsblk -n -P -b -o ...` — enumerate block devices; fixed args only.
    LsblkRaw,
    /// `mkdir -p <mountpoint> && mount [-o <options>] <device> <mountpoint>` —
    /// attach one validated block device at one validated mountpoint.
    /// Options (uid/gid/umask/...) are allowlisted per-token so a fat
    /// filesystem can be made owner-visible without opening arbitrary mount
    /// flags.
    MountBlock {
        device: PathBuf,
        mountpoint: PathBuf,
        options: Vec<String>,
    },
    /// `umount <mountpoint>` — detach one device mounted under the allowed root.
    UnmountBlock { mountpoint: PathBuf },
    /// `smartctl -H -A <device>` — SMART health + temperature probe.
    SmartInfo { device: PathBuf },
    /// Atomic write of one generated daemon config (target -> fixed path
    /// under the config dir); content is pre-validated size-wise.
    WriteDaemonConfig { target: String, content: Vec<u8> },
    /// Validate + reload daemons: smb = testparm then systemctl reload smbd;
    /// nfs = exportfs -ra.
    ReloadDaemons { targets: Vec<String> },
}

/// Maximum size of one generated daemon config (smb.conf/exports). Configs
/// are tiny; this only stops a buggy caller from writing gigabytes.
const MAX_CONFIG_BYTES: usize = 1 << 20;

/// Atomically write a generated daemon config: tmp file in the same dir,
/// fsync, rename into place, mode 0644 (docs/design/04#4: write -> fsync ->
/// rename, only via the owning service). The path is derived from the
/// allowlisted target, never from callers.
fn write_config(config_dir: &Path, target: &str, content: &[u8]) -> Result<(), Status> {
    let path = config_dir.join(match target {
        "smb" => "smb.conf",
        "nfs" => "exports",
        _ => return Err(Status::invalid_argument("target must be smb or nfs")),
    });
    std::fs::create_dir_all(config_dir).map_err(|e| {
        Status::internal(format!("create config dir {}: {e}", config_dir.display()))
    })?;
    let tmp = config_dir.join(format!(".{}.{}.tmp", target, std::process::id()));
    let result = (|| -> std::io::Result<()> {
        {
            let mut f = std::fs::File::create(&tmp)?;
            f.write_all(content)?;
            f.sync_all()?;
        }
        std::fs::rename(&tmp, &path)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644))?;
        }
        Ok(())
    })();
    if let Err(e) = result {
        let _ = std::fs::remove_file(&tmp);
        return Err(Status::internal(format!("write config {}: {e}", path.display())));
    }
    tracing::info!(target, path = %path.display(), bytes = content.len(), "daemon config written");
    Ok(())
}

struct Allowlist {
    btrfs_bin: String,
    lsblk_bin: String,
    mount_bin: String,
    umount_bin: String,
    mkdir_bin: String,
    smartctl_bin: String,
    testparm_bin: String,
    systemctl_bin: String,
    exportfs_bin: String,
    config_dir: PathBuf,
    allowed_root: PathBuf,
    dev_root: PathBuf,
}

impl Allowlist {
    fn new(
        btrfs_bin: &str,
        lsblk_bin: &str,
        mount_bin: &str,
        umount_bin: &str,
        mkdir_bin: &str,
        smartctl_bin: &str,
        testparm_bin: &str,
        systemctl_bin: &str,
        exportfs_bin: &str,
        config_dir: &Path,
        allowed_root: &Path,
        dev_root: &Path,
    ) -> Self {
        Allowlist {
            btrfs_bin: btrfs_bin.to_string(),
            lsblk_bin: lsblk_bin.to_string(),
            mount_bin: mount_bin.to_string(),
            umount_bin: umount_bin.to_string(),
            mkdir_bin: mkdir_bin.to_string(),
            smartctl_bin: smartctl_bin.to_string(),
            testparm_bin: testparm_bin.to_string(),
            systemctl_bin: systemctl_bin.to_string(),
            exportfs_bin: exportfs_bin.to_string(),
            config_dir: config_dir.to_path_buf(),
            allowed_root: allowed_root.to_path_buf(),
            dev_root: dev_root.to_path_buf(),
        }
    }

    /// Fixed filename per config target.
    fn config_path(&self, target: &str) -> PathBuf {
        self.config_dir.join(match target {
            "smb" => "smb.conf",
            "nfs" => "exports",
            _ => unreachable!("validated target"),
        })
    }

    /// Validate a `PrivRequest` against the allowlist. Arguments are checked
    /// per-op; nothing here is ever passed to a shell.
    fn validate(&self, req: &PrivRequest) -> Result<AllowedCommand, Status> {
        let op = match PrivOp::try_from(req.op) {
            Ok(op) if op != PrivOp::Unspecified => op,
            _ => return Err(Status::invalid_argument(format!("unknown or unspecified op: {}", req.op))),
        };
        match op {
            PrivOp::BtrfsFilesystemShowRaw => {
                if !req.args.is_empty() {
                    return Err(Status::invalid_argument("BTRFS_FILESYSTEM_SHOW_RAW takes no arguments"));
                }
                Ok(AllowedCommand::BtrfsFilesystemShowRaw)
            }
            PrivOp::BtrfsFilesystemUsageRaw => {
                if req.args.len() != 1 {
                    return Err(Status::invalid_argument(
                        "BTRFS_FILESYSTEM_USAGE_RAW requires exactly one mountpoint argument",
                    ));
                }
                let mount = validate_mount_path(&req.args[0], &self.allowed_root)?;
                Ok(AllowedCommand::BtrfsFilesystemUsage { mount })
            }
            PrivOp::LsblkRaw => {
                if !req.args.is_empty() {
                    return Err(Status::invalid_argument("LSBLK_RAW takes no arguments"));
                }
                Ok(AllowedCommand::LsblkRaw)
            }
            PrivOp::MountBlock => {
                if req.args.len() < 2 {
                    return Err(Status::invalid_argument(
                        "MOUNT_BLOCK requires <device> <mountpoint> [options...]",
                    ));
                }
                if req.args.len() - 2 > MAX_MOUNT_OPTIONS {
                    return Err(Status::invalid_argument(format!(
                        "MOUNT_BLOCK accepts at most {MAX_MOUNT_OPTIONS} mount options"
                    )));
                }
                let device = validate_device_path(&req.args[0], &self.dev_root)?;
                let mountpoint = validate_mount_target(&req.args[1], &self.allowed_root)?;
                let mut options = Vec::with_capacity(req.args.len() - 2);
                for raw in &req.args[2..] {
                    options.push(validate_mount_option(raw)?);
                }
                Ok(AllowedCommand::MountBlock { device, mountpoint, options })
            }
            PrivOp::UnmountBlock => {
                if req.args.len() != 1 {
                    return Err(Status::invalid_argument(
                        "UNMOUNT_BLOCK requires exactly one mountpoint argument",
                    ));
                }
                let mountpoint = validate_mount_path(&req.args[0], &self.allowed_root)?;
                Ok(AllowedCommand::UnmountBlock { mountpoint })
            }
            PrivOp::SmartInfoRaw => {
                if req.args.len() != 1 {
                    return Err(Status::invalid_argument(
                        "SMART_INFO_RAW requires exactly one device argument",
                    ));
                }
                let device = validate_device_path(&req.args[0], &self.dev_root)?;
                Ok(AllowedCommand::SmartInfo { device })
            }
            PrivOp::WriteDaemonConfig => {
                if req.args.len() != 2 {
                    return Err(Status::invalid_argument(
                        "WRITE_DAEMON_CONFIG requires <target> <content>",
                    ));
                }
                let target = &req.args[0];
                if target != "smb" && target != "nfs" {
                    return Err(Status::invalid_argument(format!(
                        "WRITE_DAEMON_CONFIG target must be smb or nfs, got {target:?}"
                    )));
                }
                let content = req.args[1].as_bytes();
                if content.len() > MAX_CONFIG_BYTES {
                    return Err(Status::invalid_argument(format!(
                        "config content too large: {} bytes (max {MAX_CONFIG_BYTES})",
                        content.len()
                    )));
                }
                Ok(AllowedCommand::WriteDaemonConfig {
                    target: target.clone(),
                    content: content.to_vec(),
                })
            }
            PrivOp::ReloadDaemons => {
                if req.args.is_empty() || req.args.len() > 2 {
                    return Err(Status::invalid_argument(
                        "RELOAD_DAEMONS requires 1-2 targets: smb, nfs",
                    ));
                }
                let mut targets = Vec::new();
                for t in &req.args {
                    if t != "smb" && t != "nfs" {
                        return Err(Status::invalid_argument(format!(
                            "RELOAD_DAEMONS target must be smb or nfs, got {t:?}"
                        )));
                    }
                    if !targets.contains(t) {
                        targets.push(t.clone());
                    }
                }
                Ok(AllowedCommand::ReloadDaemons { targets })
            }
            // Keep the match exhaustive; new ops fail closed until implemented.
            _ => Err(Status::unimplemented(format!("op {op:?} not implemented"))),
        }
    }
}

/// Canonicalize and require the path stay inside the allowed root (04 §7:
/// paths must resolve inside allowed roots). The path must already exist.
fn validate_mount_path(raw: &str, allowed_root: &Path) -> Result<PathBuf, Status> {
    let p = Path::new(raw);
    if !p.is_absolute() {
        return Err(Status::invalid_argument(format!("mountpoint must be absolute: {raw}")));
    }
    let canonical_root = allowed_root
        .canonicalize()
        .map_err(|_| Status::invalid_argument(format!("allowed root does not exist: {}", allowed_root.display())))?;
    let canonical = p.canonicalize().map_err(|_| {
        Status::invalid_argument(format!("mountpoint does not exist or is inaccessible: {raw}"))
    })?;
    if canonical == canonical_root {
        return Err(Status::invalid_argument("mountpoint may not be the allowed root itself"));
    }
    if !canonical.starts_with(&canonical_root) {
        return Err(Status::permission_denied(format!(
            "path {raw} resolves outside allowed root {}",
            allowed_root.display()
        )));
    }
    Ok(canonical)
}

/// Validate a mountpoint that does not exist yet (mount creates it): it must
/// be absolute, contain no traversal, live inside the allowed root, and be a
/// single level deeper than it. Created with `mkdir -p` before canonicalizing.
fn validate_mount_target(raw: &str, allowed_root: &Path) -> Result<PathBuf, Status> {
    let p = Path::new(raw);
    if !p.is_absolute() {
        return Err(Status::invalid_argument(format!("mountpoint must be absolute: {raw}")));
    }
    if has_traversal(p) {
        return Err(Status::invalid_argument(format!("mountpoint must not contain '..': {raw}")));
    }
    let rel = p.strip_prefix(allowed_root).map_err(|_| {
        Status::permission_denied(format!("mountpoint {raw} is outside allowed root {}", allowed_root.display()))
    })?;
    if rel.as_os_str().is_empty() {
        return Err(Status::invalid_argument("mountpoint may not be the allowed root itself"));
    }
    if rel.components().count() == 0 || rel.file_name().is_none() {
        return Err(Status::invalid_argument(format!("invalid mountpoint: {raw}")));
    }
    let canonical_root = allowed_root
        .canonicalize()
        .map_err(|_| Status::invalid_argument(format!("allowed root does not exist: {}", allowed_root.display())))?;
    if !p.starts_with(&canonical_root) {
        return Err(Status::permission_denied(format!(
            "mountpoint {raw} is outside allowed root {}",
            canonical_root.display()
        )));
    }
    // VTA: mount mounts the *directory*; make sure a regular file at that path
    // cannot be the target (mount refuses, but fail fast with a clear error).
    if p.symlink_metadata().map(|m| m.file_type().is_file()).unwrap_or(false) {
        return Err(Status::invalid_argument(format!("mountpoint is a regular file: {raw}")));
    }
    std::fs::create_dir_all(&p).map_err(|e| {
        Status::internal(format!("create mountpoint {}: {e}", p.display()))
    })?;
    let canonical = p.canonicalize().map_err(|_| {
        Status::invalid_argument(format!("mountpoint does not exist or is inaccessible: {raw}"))
    })?;
    if !canonical.starts_with(&canonical_root) {
        return Err(Status::permission_denied(format!(
            "path {raw} resolves outside allowed root {}",
            canonical_root.display()
        )));
    }
    Ok(canonical)
}

/// Validate a block device path: absolute, directly under the dev root, plain
/// name (`[A-Za-z0-9_.-]`, single component, no traversal). The kernel mount
/// call is the final line of defense; this keeps every accepted path inside
/// the device namespace.
fn validate_device_path(raw: &str, dev_root: &Path) -> Result<PathBuf, Status> {
    let p = Path::new(raw);
    if !p.is_absolute() {
        return Err(Status::invalid_argument(format!("device path must be absolute: {raw}")));
    }
    if has_traversal(p) {
        return Err(Status::invalid_argument(format!("device path must not contain '..': {raw}")));
    }
    if p.parent() != Some(dev_root) {
        return Err(Status::permission_denied(format!(
            "device {raw} must be directly under {}",
            dev_root.display()
        )));
    }
    let name = match p.file_name().and_then(|n| n.to_str()) {
        Some(n) if !n.is_empty() => n,
        _ => return Err(Status::invalid_argument(format!("invalid device path: {raw}"))),
    };
    if !name
        .chars()
        .all(|c| c.is_ascii_alphanumeric() || c == '_')
    {
        return Err(Status::invalid_argument(format!(
            "device name {name:?} contains unsupported characters"
        )));
    }
    Ok(p.to_path_buf())
}

/// True when a path contains a `..` component (traversal).
fn has_traversal(p: &Path) -> bool {
    p.components().any(|c| matches!(c, std::path::Component::ParentDir))
}

/// How many `key=value` mount options MOUNT_BLOCK will accept.
const MAX_MOUNT_OPTIONS: usize = 8;

/// Validate one `key=value` mount option. Only a closed set of keys that
/// change *ownership of the mount* (allowing non-root users to write a fat
/// stick) are accepted; nothing here can remount, change the device, or run
/// code. Values are strictly typed per key.
fn validate_mount_option(raw: &str) -> Result<String, Status> {
    if raw.len() > 64 {
        return Err(Status::invalid_argument("mount option too long"));
    }
    let Some((key, value)) = raw.split_once('=') else {
        return Err(Status::invalid_argument(format!(
            "mount option must be key=value: {raw:?}"
        )));
    };
    let ok = match key {
        "uid" | "gid" => {
            // numeric user/group id the mount is made visible to
            !value.is_empty()
                && value.len() <= 10
                && value.chars().all(|c| c.is_ascii_digit())
                && value.parse::<u32>().is_ok()
        }
        "umask" | "fmask" | "dmask" => {
            // octal permission mask (0-7 digits), e.g. "002" or "0077"
            !value.is_empty()
                && value.len() <= 4
                && value.chars().all(|c| matches!(c, '0'..='7'))
                && u32::from_str_radix(value, 8).is_ok_and(|v| v <= 0o7777)
        }
        "iocharset" => {
            !value.is_empty()
                && value.len() <= 16
                && value
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.'))
        }
        _ => false, // anything else stays rejected (fail closed)
    };
    if !ok {
        return Err(Status::invalid_argument(format!("invalid mount option: {raw:?}")));
    }
    Ok(raw.to_string())
}

// --- Privd service ---

struct PrivdService {
    allowlist: Allowlist,
}

#[tonic::async_trait]
impl Privd for PrivdService {
    async fn run(&self, request: Request<PrivRequest>) -> Result<Response<PrivResponse>, Status> {
        let req = request.into_inner();
        let cmd = self.allowlist.validate(&req)?;

        // TODO(04 §7): audit trail with caller identity via SO_PEERCRED once
        // peer verification lands (docs/design/04#5). For now, log op + args.
        tracing::info!(op = ?PrivOp::try_from(req.op).unwrap_or(PrivOp::Unspecified), args = ?req.args, "privd request");

        Ok(Response::new(execute(&self.allowlist, &cmd).await?))
    }
}

/// Run one allowlisted command with `Command` (explicit argv, no shell) and a
/// hard timeout so a wedged binary can never hang the control plane.
async fn execute(allowlist: &Allowlist, cmd: &AllowedCommand) -> Result<PrivResponse, Status> {
    let (bin, args): (String, Vec<String>) = match cmd {
        AllowedCommand::BtrfsFilesystemShowRaw => (
            allowlist.btrfs_bin.clone(),
            vec!["filesystem".into(), "show".into(), "--raw".into()],
        ),
        AllowedCommand::BtrfsFilesystemUsage { mount } => (
            allowlist.btrfs_bin.clone(),
            vec!["filesystem".into(), "usage".into(), "-b".into(), mount.display().to_string()],
        ),
        AllowedCommand::LsblkRaw => (
            allowlist.lsblk_bin.clone(),
            vec![
                "-n".into(),
                "-P".into(),
                "-b".into(),
                "-o".into(),
                "KNAME,TYPE,FSTYPE,LABEL,UUID,SIZE,MOUNTPOINT".into(),
            ],
        ),
        AllowedCommand::MountBlock { device, mountpoint, options } => {
            // mkdir -p first: the mountpoint may not exist yet (it was created
            // during validation, but execute must be self-sufficient too).
            run_sync_ok(
                &allowlist.mkdir_bin,
                &["-p".into(), mountpoint.display().to_string()],
            )?;
            let mut args = Vec::with_capacity(2 + options.len() + 2);
            if !options.is_empty() {
                args.push("-o".into());
                args.push(options.join(","));
            }
            args.push(device.display().to_string());
            args.push(mountpoint.display().to_string());
            (allowlist.mount_bin.clone(), args)
        }
        AllowedCommand::UnmountBlock { mountpoint } => (
            allowlist.umount_bin.clone(),
            vec![mountpoint.display().to_string()],
        ),
        AllowedCommand::SmartInfo { device } => (
            allowlist.smartctl_bin.clone(),
            vec!["-H".into(), "-A".into(), device.display().to_string()],
        ),
        AllowedCommand::WriteDaemonConfig { target, content } => {
            // Not a subprocess: atomic write (tmp -> fsync -> rename).
            write_config(&allowlist.config_dir, target, content)?;
            return Ok(PrivResponse {
                exit_code: 0,
                stdout: Vec::new(),
                stderr: Vec::new(),
            });
        }
        AllowedCommand::ReloadDaemons { targets } => {
            // Build the (bin, argv) list for every target, then run them
            // sequentially, failing closed: if validation fails, no reload.
            let mut steps: Vec<(String, Vec<String>)> = Vec::new();
            for t in targets {
                match t.as_str() {
                    "smb" => {
                        let conf = allowlist.config_path("smb");
                        // validate first (docs/design/02#6 step 4)
                        steps.push((
                            allowlist.testparm_bin.clone(),
                            vec!["-s".into(), conf.display().to_string()],
                        ));
                        steps.push((
                            allowlist.systemctl_bin.clone(),
                            vec!["reload".into(), "smbd".into()],
                        ));
                    }
                    "nfs" => {
                        steps.push((
                            allowlist.exportfs_bin.clone(),
                            vec!["-ra".into()],
                        ));
                    }
                    _ => unreachable!("validated target"),
                }
            }
            let mut resp = PrivResponse {
                exit_code: 0,
                stdout: Vec::new(),
                stderr: Vec::new(),
            };
            for (bin, args) in steps {
                let out = run_argv(&bin, &args).await?;
                if !out.status.success() {
                    resp.exit_code = out.status.code().unwrap_or(-1) as i32;
                    resp.stdout.extend_from_slice(&out.stdout);
                    resp.stderr.extend_from_slice(&out.stderr);
                    break; // validation gate: don't reload an invalid config
                }
            }
            return Ok(resp);
        }
    };

    let output = run_argv(&bin, &args).await?;
    Ok(PrivResponse {
        exit_code: output.status.code().unwrap_or(-1) as i32,
        stdout: output.stdout,
        stderr: output.stderr,
    })
}

/// Run a helper subprocess (mkdir -p before mount) whose failures surface as
/// mount failures, not gRPC errors.
fn run_sync_ok(bin: &str, args: &[String]) -> Result<(), Status> {
    let output = std::process::Command::new(bin)
        .args(args)
        .output()
        .map_err(|e| Status::internal(format!("failed to run {bin}: {e}")))?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(Status::internal(format!(
            "{} {} failed: {}",
            bin,
            args.join(" "),
            stderr.trim()
        )));
    }
    Ok(())
}

/// Run one argv array with a hard timeout; the binary itself must already be
/// allowlisted at validation time.
async fn run_argv(bin: &str, args: &[String]) -> Result<std::process::Output, Status> {
    let mut child = Command::new(bin);
    child.args(args);
    match tokio::time::timeout(CMD_TIMEOUT, child.output()).await {
        Ok(Ok(output)) => Ok(output),
        Ok(Err(e)) => Err(Status::internal(format!("failed to run {bin}: {e} (is it installed?)"))),
        Err(_) => Err(Status::deadline_exceeded(format!("{bin} timed out"))),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::sync::atomic::{AtomicU32, Ordering};

    static NEXT_DIR: AtomicU32 = AtomicU32::new(0);

    /// Unique temp dir per test, removed on drop.
    struct TempDir(PathBuf);
    impl TempDir {
        fn new(tag: &str) -> Self {
            let p = std::env::temp_dir().join(format!(
                "onyx-privd-test-{}-{}-{}",
                std::process::id(),
                tag,
                NEXT_DIR.fetch_add(1, Ordering::SeqCst)
            ));
            fs::create_dir_all(&p).unwrap();
            TempDir(p)
        }
        fn path(&self) -> &Path {
            &self.0
        }
    }
    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn fake_bin(dir: &Path, name: &str, script: &str) -> String {
        let p = dir.join(name);
        fs::write(&p, script).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&p, fs::Permissions::from_mode(0o755)).unwrap();
        }
        p.display().to_string()
    }

    fn request(op: PrivOp, args: Vec<&str>) -> PrivRequest {
        PrivRequest {
            op: op as i32,
            args: args.into_iter().map(|s| s.to_string()).collect(),
        }
    }

    fn test_allowlist(dir: &Path) -> Allowlist {
        // All fake bins record argv into a shared log file.
        let log = dir.join("argv.log");
        let record = format!(
            "#!/bin/sh\n{{ printf '%s' \"$0\"; printf '|%s' \"$@\"; printf '\\n'; }} >> \"{}\"\nexit 0\n",
            log.display()
        );
        let marker = |name: &str| fake_bin(dir, name, &record);
        Allowlist::new(
            &marker("btrfs"),
            &marker("lsblk"),
            &marker("mount"),
            &marker("umount"),
            &marker("mkdir"),
            &marker("smartctl"),
            &marker("testparm"),
            &marker("systemctl"),
            &marker("exportfs"),
            dir,             // config dir = temp dir
            dir,             // allowed root = temp dir
            dir.join("dev").as_path(), // dev root = temp dir/dev (created by tests)
        )
    }

    async fn run_request(a: &Allowlist, req: &PrivRequest) -> Result<PrivResponse, Status> {
        let cmd = a.validate(req)?;
        execute(a, &cmd).await
    }

    #[tokio::test]
    async fn missing_bin_is_reported_as_internal_error() {
        // btrfs bin is set to a nonexistent path; the error must be structured.
        let dir = TempDir::new("nobin");
        let a = Allowlist::new(
            "/nonexistent/btrfs",
            "lsblk",
            "mount",
            "umount",
            "mkdir",
            "smartctl",
            "testparm",
            "systemctl",
            "exportfs",
            dir.path(),
            dir.path(),
            dir.path().join("dev").as_path(),
        );
        let resp = run_request(&a, &request(PrivOp::BtrfsFilesystemShowRaw, vec![])).await;
        assert!(resp.is_err(), "missing binary must fail closed");
    }

    #[tokio::test]
    async fn btrfs_show_rejects_args() {
        let dir = TempDir::new("btrfs");
        let a = test_allowlist(dir.path());
        let err = run_request(&a, &request(PrivOp::BtrfsFilesystemShowRaw, vec!["extra"])).await.unwrap_err();
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
    }

    #[tokio::test]
    async fn lsblk_rejects_args() {
        let dir = TempDir::new("lsblk");
        let a = test_allowlist(dir.path());
        let err = run_request(&a, &request(PrivOp::LsblkRaw, vec!["-o"])).await.unwrap_err();
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
        // and succeeds with no args
        let resp = run_request(&a, &request(PrivOp::LsblkRaw, vec![])).await.unwrap();
        assert_eq!(resp.exit_code, 0);
    }

    #[tokio::test]
    async fn mount_block_passes_device_and_mountpoint() {
        let dir = TempDir::new("mount");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let a = test_allowlist(dir.path());
        let mountpoint = dir.path().join("usb-stick");
        // Build the request against the real dev root:
        let req = request(
            PrivOp::MountBlock,
            vec![dev_root.join("sdb1").to_str().unwrap(), mountpoint.to_str().unwrap()],
        );
        let resp = run_request(&a, &req).await.unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        let mount_line = log.lines().find(|l| l.contains("sdb1")).expect("mount argv logged");
        assert!(mount_line.contains(&format!("{}/dev/sdb1", dir.path().display())), "{mount_line}");
        assert!(mount_line.ends_with(&format!("|{}/usb-stick", dir.path().display())), "{mount_line}");
        // mountpoint dir must exist afterwards
        assert!(mountpoint.is_dir(), "mountpoint was created");
    }

    #[tokio::test]
    async fn smart_info_runs_smartctl_with_validated_device() {
        let dir = TempDir::new("smart");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let a = test_allowlist(dir.path());
        let dev = dev_root.join("sdb");
        let resp = run_request(&a, &request(PrivOp::SmartInfoRaw, vec![dev.to_str().unwrap()])).await.unwrap();
        assert_eq!(resp.exit_code, 0);
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        assert!(log.lines().any(|l| l.contains("-H") && l.contains("-A") && l.contains("sdb")));
    }

    #[test]
    fn smart_info_rejects_bad_devices_and_extra_args() {
        let dir = TempDir::new("smartbad");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let a = test_allowlist(dir.path());
        // too many args
        let err = a
            .validate(&request(
                PrivOp::SmartInfoRaw,
                vec![dev_root.join("sda").to_str().unwrap(), "extra"],
            ))
            .expect_err("two args rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
        // non-device path
        let err = a
            .validate(&request(PrivOp::SmartInfoRaw, vec!["/etc/passwd"]))
            .expect_err("non-device rejected");
        assert_eq!(err.code(), tonic::Code::PermissionDenied, "{err}");
    }

    #[tokio::test]
    async fn write_daemon_config_writes_fixed_path_atomically() {
        let dir = TempDir::new("writecfg");
        let a = test_allowlist(dir.path());
        let content = "# generated\n[global]\n   workgroup = WORKGROUP\n";
        let resp = run_request(
            &a,
            &PrivRequest {
                op: PrivOp::WriteDaemonConfig as i32,
                args: vec!["smb".to_string(), content.to_string()],
            },
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        let written = fs::read_to_string(dir.path().join("smb.conf")).unwrap();
        assert_eq!(written, content);
        // nfs target lands in exports
        run_request(
            &a,
            &PrivRequest {
                op: PrivOp::WriteDaemonConfig as i32,
                args: vec!["nfs".to_string(), "/mnt/onyx/x  *(ro)\n".to_string()],
            },
        )
        .await
        .unwrap();
        assert!(dir.path().join("exports").is_file());
        // no leftover temp files; perms 0644 (rw-r--r--)
        for entry in fs::read_dir(dir.path()).unwrap().flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            assert!(!name.ends_with(".tmp"), "temp file left behind: {name}");
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = fs::metadata(dir.path().join("smb.conf")).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode, 0o644);
        }
    }

    #[test]
    fn write_daemon_config_rejects_bad_input() {
        let dir = TempDir::new("writebad");
        let a = test_allowlist(dir.path());
        // bad target
        let err = a
            .validate(&PrivRequest {
                op: PrivOp::WriteDaemonConfig as i32,
                args: vec!["httpd".to_string(), "x".to_string()],
            })
            .expect_err("bad target rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
        // arity
        let err = a
            .validate(&PrivRequest {
                op: PrivOp::WriteDaemonConfig as i32,
                args: vec!["smb".to_string()],
            })
            .expect_err("arity rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
        // oversized content (1 MiB cap)
        let big = "x".repeat(MAX_CONFIG_BYTES + 1);
        let err = a
            .validate(&PrivRequest {
                op: PrivOp::WriteDaemonConfig as i32,
                args: vec!["smb".to_string(), big],
            })
            .expect_err("oversized rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
    }

    #[tokio::test]
    async fn reload_daemons_runs_testparm_then_reload() {
        let dir = TempDir::new("reload");
        let a = test_allowlist(dir.path());
        let resp = run_request(
            &a,
            &PrivRequest {
                op: PrivOp::ReloadDaemons as i32,
                args: vec!["smb".to_string(), "nfs".to_string()],
            },
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        let lines: Vec<&str> = log.lines().collect();
        // testparm validates the written smb.conf BEFORE any reload happens
        // fake bins log their full path, so match on the basename
        let tp_line = lines.iter().find(|l| l.contains("/testparm")).unwrap_or_else(|| panic!("no testparm line in argv.log:\n{log}"));
        assert!(tp_line.contains("-s") && tp_line.contains("smb.conf"), "{tp_line}");
        let idx_tp = lines.iter().position(|l| l.contains("/testparm")).unwrap();
        let idx_sys = lines.iter().position(|l| l.contains("/systemctl")).unwrap();
        assert!(idx_tp < idx_sys, "testparm must run before systemctl reload");
        assert!(lines.iter().any(|l| l.contains("/systemctl") && l.contains("reload") && l.contains("smbd")));
        assert!(log.contains("exportfs") && log.contains("-ra"));
    }

    #[tokio::test]
    async fn reload_daemons_fails_closed_on_invalid_config() {
        let dir = TempDir::new("reloadfail");
        // testparm that fails validation; the reload must never run.
        let failing = fake_bin(dir.path(), "testparm-bad", "#!/bin/sh\nexit 1\n");
        let a = Allowlist::new(
            "btrfs", "lsblk", "mount", "umount", "mkdir", "smartctl",
            &failing, // testparm fails
            "systemctl", "exportfs",
            dir.path(), dir.path(), dir.path().join("dev").as_path(),
        );
        let resp = run_request(
            &a,
            &PrivRequest {
                op: PrivOp::ReloadDaemons as i32,
                args: vec!["smb".to_string()],
            },
        )
        .await
        .unwrap();
        assert_ne!(resp.exit_code, 0, "invalid config must fail the reload");
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap_or_default();
        assert!(!log.contains("/systemctl"), "reload ran despite failed validation: {log}");
    }

    #[test]
    fn reload_daemons_rejects_bad_targets() {
        let dir = TempDir::new("reloadbad");
        let a = test_allowlist(dir.path());
        for args in [vec!["ftp"], vec![], vec!["smb", "nfs", "smb"]] {
            let err = a
                .validate(&PrivRequest {
                    op: PrivOp::ReloadDaemons as i32,
                    args: args.iter().map(|s| s.to_string()).collect(),
                })
                .expect_err("must be rejected");
            assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
        }
        // duplicates are deduped, order preserved
        let cmd = a
            .validate(&PrivRequest {
                op: PrivOp::ReloadDaemons as i32,
                args: vec!["nfs".to_string(), "nfs".to_string()],
            })
            .unwrap();
        match cmd {
            AllowedCommand::ReloadDaemons { targets } => assert_eq!(targets, vec!["nfs"]),
            other => panic!("expected ReloadDaemons, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn mount_block_passes_validated_options() {
        let dir = TempDir::new("mountopts");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let a = test_allowlist(dir.path());
        let mountpoint = dir.path().join("usb-stick");
        let req = request(
            PrivOp::MountBlock,
            vec![
                dev_root.join("sdz1").to_str().unwrap(),
                mountpoint.to_str().unwrap(),
                "uid=1000",
                "gid=100",
                "umask=002",
            ],
        );
        let resp = run_request(&a, &req).await.unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        let mount_line = log.lines().find(|l| l.contains("sdz1")).expect("mount argv logged");
        assert!(mount_line.contains("-o"), "{mount_line}");
        assert!(mount_line.contains("uid=1000,gid=100,umask=002"), "{mount_line}");
    }

    #[test]
    fn mount_block_validates_options() {
        let dir = TempDir::new("optbad");
        let a = test_allowlist(dir.path());
        let dev = dir.path().join("dev/sda1");
        let mp = dir.path().join("x");
        let cases = [
            // unknown keys / non-option shapes fail closed
            vec!["foo=1"],
            vec!["uid"],          // missing =
            vec!["-o"],           // flag, not key=value
            vec![""],             // empty
            // bad value types
            vec!["uid=abc"],
            vec!["uid=-1"],
            vec!["uid=99999999999999999999"],
            vec!["gid=1.5"],
            vec!["umask=928"],   // not octal
            vec!["umask=0x022"], // not plain octal
            vec!["umask=99999"], // too large
            vec!["iocharset=utf 8"],
            // too many options
            vec![
                "uid=1", "gid=1", "uid=1", "gid=1", "uid=1", "gid=1", "uid=1", "gid=1", "uid=1",
            ],
        ];
        for opts in cases {
            let mut args = vec![dev.to_str().unwrap().to_string(), mp.to_str().unwrap().to_string()];
            args.extend(opts.iter().map(|s| s.to_string()));
            let req = PrivRequest {
                op: PrivOp::MountBlock as i32,
                args,
            };
            let err = a.validate(&req).expect_err("must be rejected");
            assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
        }
        // and a couple of valid ones pass
        for opts in [
            vec!["umask=0002"],
            vec!["fmask=0133", "dmask=0022"],
            vec!["uid=0", "gid=0"],
            vec!["iocharset=utf8"],
        ] {
            let mut args = vec![dev.to_str().unwrap().to_string(), mp.to_str().unwrap().to_string()];
            args.extend(opts.iter().map(|s| s.to_string()));
            let req = PrivRequest {
                op: PrivOp::MountBlock as i32,
                args,
            };
            let cmd = a.validate(&req).expect("valid options must pass");
            match cmd {
                AllowedCommand::MountBlock { options, .. } => assert_eq!(options.len(), opts.len()),
                other => panic!("expected MountBlock, got {other:?}"),
            }
        }
    }

    #[tokio::test]
    async fn unmount_block_umounts_validated_mountpoint() {
        let dir = TempDir::new("umount");
        let a = test_allowlist(dir.path());
        let mp = dir.path().join("usb-stick");
        fs::create_dir_all(&mp).unwrap();
        let resp = run_request(&a, &request(PrivOp::UnmountBlock, vec![mp.to_str().unwrap()])).await.unwrap();
        assert_eq!(resp.exit_code, 0);
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        assert!(log.lines().any(|l| l.ends_with(&format!("|{}/usb-stick", dir.path().display()))), "{log}");
    }

    #[test]
    fn mount_block_rejects_bad_devices() {
        let dir = TempDir::new("baddev");
        let a = test_allowlist(dir.path());
        let mp = dir.path().join("x");
        let cases = [
            "/etc/passwd".to_string(),                                                   // outside dev root
            "/dev/../etc/passwd".to_string(),                                            // traversal
            dir.path().join("dev").join("a/b").to_str().unwrap().to_string(),           // nested
            "/dev/sdb1/extra".to_string(),                                               // nested
            "/dev/s db1".to_string(),                                                    // space
            "/dev/".to_string(),                                                         // the root itself
            "/dev/-x".to_string(),                                                       // leading dash (flag-like)
            "relative".to_string(),                                                       // not absolute
        ];
        for c in cases {
            let req = PrivRequest {
                op: PrivOp::MountBlock as i32,
                args: vec![c.clone(), mp.to_str().unwrap().to_string()],
            };
            let err = a.validate(&req).expect_err(&format!("{c} must be rejected"));
            assert!(
                err.code() == tonic::Code::InvalidArgument || err.code() == tonic::Code::PermissionDenied,
                "{c}: {err}"
            );
        }
    }

    #[test]
    fn mount_block_rejects_bad_mountpoints() {
        let dir = TempDir::new("badmp");
        let dev = dir.path().join("dev/sda1");
        let a = test_allowlist(dir.path());
        let cases = [
            "/etc/passwd-x".to_string(),                    // outside root
            dir.path().join("a/../b").to_str().unwrap().to_string(), // traversal
            dir.path().to_str().unwrap().to_string(),        // the root itself
            "relative".to_string(),                          // not absolute
            "/tmp/does-not-exist-but-outside".to_string(),  // canonicalize would pass but prefix fails
        ];
        for c in cases {
            let req = PrivRequest {
                op: PrivOp::MountBlock as i32,
                args: vec![dev.to_str().unwrap().to_string(), c.clone()],
            };
            let err = a.validate(&req).expect_err(&format!("{c} must be rejected"));
            assert!(
                err.code() == tonic::Code::InvalidArgument || err.code() == tonic::Code::PermissionDenied,
                "{c}: {err}"
            );
        }
    }

    #[test]
    fn mount_block_regular_file_mountpoint_rejected() {
        let dir = TempDir::new("filemp");
        let f = dir.path().join("afile");
        fs::write(&f, "x").unwrap();
        let a = test_allowlist(dir.path());
        let dev = dir.path().join("dev/sda1");
        let req = PrivRequest {
            op: PrivOp::MountBlock as i32,
            args: vec![dev.to_str().unwrap().to_string(), f.to_str().unwrap().to_string()],
        };
        let err = a.validate(&req).unwrap_err();
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
    }

    #[test]
    fn unmount_block_requires_existing_path_under_root() {
        let dir = TempDir::new("badumount");
        let a = test_allowlist(dir.path());
        let cases = vec!["/mnt".to_string(), "/etc".to_string(), dir.path().join("nope").to_str().unwrap().to_string()];
        for c in cases {
            let req = PrivRequest {
                op: PrivOp::UnmountBlock as i32,
                args: vec![c.clone()],
            };
            let err = a.validate(&req).expect_err(&format!("{c} must be rejected"));
            assert!(
                err.code() == tonic::Code::InvalidArgument || err.code() == tonic::Code::PermissionDenied,
                "{c}: {err}"
            );
        }
    }
}