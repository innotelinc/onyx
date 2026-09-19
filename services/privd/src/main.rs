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
use std::os::unix::fs::FileTypeExt;
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

const VERSION: &str = "0.3.0-dev";
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
        &args.mkfs_btrfs_bin,
        &args.mkfs_ext4_bin,
        &args.wipefs_bin,
        &args.testparm_bin,
        &args.systemctl_bin,
        &args.exportfs_bin,
        &args.sshd_bin,
        &args.config_dir,
        &args.allowed_root,
        &args.dev_root,
    )
    .with_holder_tools(&args.swapoff_bin, &args.dmsetup_bin, &args.mdadm_bin, &args.losetup_bin)
    .with_pool_mode(args.pool_mode);
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
    mkfs_btrfs_bin: String,
    mkfs_ext4_bin: String,
    wipefs_bin: String,
    testparm_bin: String,
    systemctl_bin: String,
    exportfs_bin: String,
    sshd_bin: String,
    swapoff_bin: String,
    dmsetup_bin: String,
    mdadm_bin: String,
    losetup_bin: String,
    /// Permissions applied to the root of a freshly mounted pool. Defaults to
    /// 0777: a pool is a *shared* volume whose writers are the unprivileged
    /// protocol daemons (davd/SFTP/FTP/rsync), containerised apps and the
    /// object-store tier target, all of which run as ordinary uids — a fresh
    /// ext4/btrfs root is root:root 0755, so without this a freshly created
    /// pool is read-only for everything that is supposed to use it.
    pool_mode: u32,
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
        let mut mkfs_btrfs_bin = "mkfs.btrfs".to_string();
        let mut mkfs_ext4_bin = "mkfs.ext4".to_string();
        let mut wipefs_bin = "wipefs".to_string();
        let mut testparm_bin = "testparm".to_string();
        let mut systemctl_bin = "systemctl".to_string();
        let mut exportfs_bin = "exportfs".to_string();
        let mut sshd_bin = "sshd".to_string();
        let mut swapoff_bin = "swapoff".to_string();
        let mut dmsetup_bin = "dmsetup".to_string();
        let mut mdadm_bin = "mdadm".to_string();
        let mut losetup_bin = "losetup".to_string();
        let mut pool_mode = 0o777;
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
                "--mkfs-btrfs-bin" => mkfs_btrfs_bin = it.next().expect("--mkfs-btrfs-bin requires a value"),
                "--mkfs-ext4-bin" => mkfs_ext4_bin = it.next().expect("--mkfs-ext4-bin requires a value"),
                "--wipefs-bin" => wipefs_bin = it.next().expect("--wipefs-bin requires a value"),
                "--testparm-bin" => testparm_bin = it.next().expect("--testparm-bin requires a value"),
                "--systemctl-bin" => systemctl_bin = it.next().expect("--systemctl-bin requires a value"),
                "--exportfs-bin" => exportfs_bin = it.next().expect("--exportfs-bin requires a value"),
                "--sshd-bin" => sshd_bin = it.next().expect("--sshd-bin requires a value"),
                "--swapoff-bin" => swapoff_bin = it.next().expect("--swapoff-bin requires a value"),
                "--dmsetup-bin" => dmsetup_bin = it.next().expect("--dmsetup-bin requires a value"),
                "--mdadm-bin" => mdadm_bin = it.next().expect("--mdadm-bin requires a value"),
                "--losetup-bin" => losetup_bin = it.next().expect("--losetup-bin requires a value"),
                "--pool-mode" => {
                    let raw = it.next().expect("--pool-mode requires a value");
                    match parse_mode(&raw) {
                        Some(mode) => pool_mode = mode,
                        None => {
                            eprintln!("onyx-privd: --pool-mode must be an octal mode up to 07777, got {raw:?}");
                            std::process::exit(2);
                        }
                    }
                }
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
            mkfs_btrfs_bin,
            mkfs_ext4_bin,
            wipefs_bin,
            testparm_bin,
            systemctl_bin,
            exportfs_bin,
            sshd_bin,
            swapoff_bin,
            dmsetup_bin,
            mdadm_bin,
            losetup_bin,
            pool_mode,
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
    /// `umount [-f|-l] <mountpoint>` — detach one device mounted under the
    /// allowed root. With force=true a busy mount is retried with `-f`
    /// (force) and then `-l` (lazy detach) so a lingering handle cannot block
    /// an operation the user already confirmed (pool creation).
    UnmountBlock { mountpoint: PathBuf, force: bool },
    /// `smartctl -H -A <device>` — SMART health + temperature probe.
    SmartInfo { device: PathBuf },
    /// `rmdir <mountpoint>` — remove an empty mountpoint directory left behind
    /// after an unmount. rmdir semantics: never removes a directory that still
    /// holds anything.
    RemoveMountpoint { mountpoint: PathBuf },
    /// `mkfs.btrfs -f -L <label> <device>` — format one verified device.
    FormatFilesystem { device: PathBuf, fs_type: String, label: String, force: bool },
    CreateBtrfsPool { device: PathBuf, label: String },
    /// Atomic write of one generated daemon config (target -> fixed path
    /// under the config dir); content is pre-validated size-wise.
    WriteDaemonConfig { target: String, content: Vec<u8> },
    /// Validate + reload daemons after a config write. smb = testparm then
    /// systemctl reload smbd; nfs = exportfs -ra; sftp = sshd -t then systemctl
    /// reload onyx-sftp; ftp/webdav/rsync = systemctl reload of their daemon.
    ReloadDaemons { targets: Vec<String> },
}

/// Maximum size of one generated daemon config. Configs are tiny; this only
/// stops a buggy caller from writing gigabytes.
const MAX_CONFIG_BYTES: usize = 1 << 20;

/// The closed set of generated daemon config files (docs/design/05#6): one per
/// sharing protocol. Each maps to a fixed filename under the config dir, so a
/// caller never passes a path.
const CONFIG_TARGETS: &[&str] = &["smb", "nfs", "ftp", "sftp", "webdav", "rsync"];

fn is_config_target(target: &str) -> bool {
    CONFIG_TARGETS.contains(&target)
}

/// Fixed filename for one config target, or None for an unknown target.
fn config_filename(target: &str) -> Option<&'static str> {
    Some(match target {
        "smb" => "smb.conf",
        "nfs" => "exports",
        "ftp" => "vsftpd.conf",
        "sftp" => "sshd_config",
        "webdav" => "davd.conf",
        "rsync" => "rsyncd.conf",
        _ => return None,
    })
}

/// Atomically write a generated daemon config: tmp file in the same dir,
/// fsync, rename into place, mode 0644 (docs/design/04#4: write -> fsync ->
/// rename, only via the owning service). The path is derived from the
/// allowlisted target, never from callers.
fn write_config(config_dir: &Path, target: &str, content: &[u8]) -> Result<(), Status> {
    let filename = config_filename(target)
        .ok_or_else(|| Status::invalid_argument(format!("unknown config target {target:?}")))?;
    let path = config_dir.join(filename);
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
    mkfs_btrfs_bin: String,
    mkfs_ext4_bin: String,
    wipefs_bin: String,
    testparm_bin: String,
    systemctl_bin: String,
    exportfs_bin: String,
    sshd_bin: String,
    /// Kernel stacks that keep a disk open — an active swap area, a
    /// device-mapper mapping, an md array, a loop device. Releasing them is
    /// what "force erase" means for a disk that was used before; each is
    /// optional, so a host without (say) mdraid simply has nothing to stop.
    swapoff_bin: String,
    dmsetup_bin: String,
    mdadm_bin: String,
    losetup_bin: String,
    /// Mode applied to the root of a freshly mounted pool (docs/design/05 §2).
    pool_mode: u32,
    config_dir: PathBuf,
    allowed_root: PathBuf,
    dev_root: PathBuf,
    /// How a device path is checked for a node this process can open. It is a
    /// field so the argv-construction tests can drive synthetic device paths
    /// (a unit test cannot create a block device node); production always uses
    /// [`device_node_error`].
    device_check: fn(&Path) -> Option<String>,
    /// How the holder-release path finds active swap areas and the devices
    /// stacked on a disk. Fields for the same reason as `device_check`: a unit
    /// test cannot activate swap or create a dm mapping, so it injects the
    /// kernel's answer; production reads /proc/swaps and /sys.
    swap_probe: fn(&Path, &str) -> Vec<PathBuf>,
    holders_probe: fn(&str) -> Vec<String>,
}

impl Allowlist {
    fn new(
        btrfs_bin: &str,
        lsblk_bin: &str,
        mount_bin: &str,
        umount_bin: &str,
        mkdir_bin: &str,
        smartctl_bin: &str,
        mkfs_btrfs_bin: &str,
        mkfs_ext4_bin: &str,
        wipefs_bin: &str,
        testparm_bin: &str,
        systemctl_bin: &str,
        exportfs_bin: &str,
        sshd_bin: &str,
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
            mkfs_btrfs_bin: mkfs_btrfs_bin.to_string(),
            mkfs_ext4_bin: mkfs_ext4_bin.to_string(),
            wipefs_bin: wipefs_bin.to_string(),
            testparm_bin: testparm_bin.to_string(),
            systemctl_bin: systemctl_bin.to_string(),
            exportfs_bin: exportfs_bin.to_string(),
            sshd_bin: sshd_bin.to_string(),
            swapoff_bin: "swapoff".to_string(),
            dmsetup_bin: "dmsetup".to_string(),
            mdadm_bin: "mdadm".to_string(),
            losetup_bin: "losetup".to_string(),
            pool_mode: 0o777,
            config_dir: config_dir.to_path_buf(),
            allowed_root: allowed_root.to_path_buf(),
            dev_root: dev_root.to_path_buf(),
            device_check: device_node_error,
            swap_probe: swap_areas_for,
            holders_probe: stacked_holders,
        }
    }

    /// Point the holder-release tools at explicit binaries. Tests use this to
    /// drive fake ones; production passes the `--*-bin` flags through.
    fn with_holder_tools(mut self, swapoff: &str, dmsetup: &str, mdadm: &str, losetup: &str) -> Self {
        self.swapoff_bin = swapoff.to_string();
        self.dmsetup_bin = dmsetup.to_string();
        self.mdadm_bin = mdadm.to_string();
        self.losetup_bin = losetup.to_string();
        self
    }

    /// Permissions applied to a freshly mounted pool root.
    fn with_pool_mode(mut self, mode: u32) -> Self {
        self.pool_mode = mode & 0o7777;
        self
    }

    /// Fixed filename per config target (validated before use).
    fn config_path(&self, target: &str) -> PathBuf {
        self.config_dir
            .join(config_filename(target).expect("validated config target"))
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
                if req.args.is_empty() || req.args.len() > 2 {
                    return Err(Status::invalid_argument(
                        "UNMOUNT_BLOCK requires <mountpoint> [force]",
                    ));
                }
                let mountpoint = validate_mount_path(&req.args[0], &self.allowed_root)?;
                let force = match req.args.get(1).map(String::as_str) {
                    None | Some("false") => false,
                    Some("true") => true,
                    Some(other) => {
                        return Err(Status::invalid_argument(format!(
                            "force must be true or false, got {other:?}"
                        )))
                    }
                };
                Ok(AllowedCommand::UnmountBlock { mountpoint, force })
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
            PrivOp::RemoveMountpoint => {
                if req.args.len() != 1 {
                    return Err(Status::invalid_argument(
                        "REMOVE_MOUNTPOINT requires exactly one mountpoint argument",
                    ));
                }
                let mountpoint = validate_mount_path(&req.args[0], &self.allowed_root)?;
                Ok(AllowedCommand::RemoveMountpoint { mountpoint })
            }
            PrivOp::FormatFilesystem => {
                if req.args.len() != 4 {
                    return Err(Status::invalid_argument("FORMAT_FILESYSTEM requires <device> <fs_type> <label> <force>"));
                }
                let device = validate_device_path(&req.args[0], &self.dev_root)?;
                let fs_type = req.args[1].to_ascii_lowercase();
                if fs_type != "btrfs" && fs_type != "ext4" {
                    return Err(Status::invalid_argument("filesystem must be btrfs or ext4"));
                }
                let label = &req.args[2];
                if label.is_empty() || label.len() > 32 || !label.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.')) {
                    return Err(Status::invalid_argument("filesystem label must be 1-32 characters: letters, numbers, _, -, ."));
                }
                let force = match req.args[3].as_str() {
                    "true" => true,
                    "false" => false,
                    _ => return Err(Status::invalid_argument("force must be true or false")),
                };
                Ok(AllowedCommand::FormatFilesystem { device, fs_type, label: label.clone(), force })
            }
            PrivOp::CreateBtrfsPool => {
                if req.args.len() != 2 {
                    return Err(Status::invalid_argument("CREATE_BTRFS_POOL requires <device> <label>"));
                }
                let device = validate_device_path(&req.args[0], &self.dev_root)?;
                let label = &req.args[1];
                Ok(AllowedCommand::CreateBtrfsPool { device, label: label.clone() })
            }
            PrivOp::WriteDaemonConfig => {
                if req.args.len() != 2 {
                    return Err(Status::invalid_argument(
                        "WRITE_DAEMON_CONFIG requires <target> <content>",
                    ));
                }
                let target = &req.args[0];
                if !is_config_target(target) {
                    return Err(Status::invalid_argument(format!(
                        "WRITE_DAEMON_CONFIG target must be one of {CONFIG_TARGETS:?}, got {target:?}"
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
                if req.args.is_empty() || req.args.len() > CONFIG_TARGETS.len() {
                    return Err(Status::invalid_argument(format!(
                        "RELOAD_DAEMONS requires 1-{} targets: {CONFIG_TARGETS:?}",
                        CONFIG_TARGETS.len()
                    )));
                }
                let mut targets = Vec::new();
                for t in &req.args {
                    if !is_config_target(t) {
                        return Err(Status::invalid_argument(format!(
                            "RELOAD_DAEMONS target must be one of {CONFIG_TARGETS:?}, got {t:?}"
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

/// The reason a device path cannot be opened here, or None when it can.
///
/// The kernel's device list (`/sys`) and this process's `/dev` can disagree:
/// `lsblk` reports a disk that has no node in this mount namespace, and the
/// tools then fail with their own wording — `wipefs: error: /dev/sda: probing
/// initialization failed: No such file or directory` followed by `mke2fs: The
/// file /dev/sda does not exist and no size was specified`. Neither tells an
/// operator that the disk is simply not visible inside the container, so the
/// check runs before any tool is invoked (and before the destructive path).
fn device_node_error(device: &Path) -> Option<String> {
    match std::fs::metadata(device) {
        Ok(meta) if meta.file_type().is_block_device() => None,
        Ok(_) => Some(format!(
            "{} is not a block device node; nothing can be formatted or mounted on it",
            device.display()
        )),
        Err(_) => Some(format!(
            "{} does not exist in this container: the disk is visible to the kernel but has no device node here. \
             Block devices have to be passed into onyx-privd (compose mounts `- /dev:/dev`; a nested or restricted runtime can still hide them)",
            device.display()
        )),
    }
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
            // Same visibility check as the format path: mounting a node that is
            // not here would fail with the generic mount error after the
            // mountpoint had already been created.
            if let Some(reason) = (allowlist.device_check)(device) {
                // Validation created the mountpoint on the way in; a mount that
                // never happens must not leave an empty directory for the file
                // explorer to list as a stale pool.
                let _ = std::fs::remove_dir(mountpoint);
                return Ok(PrivResponse {
                    exit_code: 1,
                    stdout: Vec::new(),
                    stderr: reason.into_bytes(),
                });
            }
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
            let out = run_argv(&allowlist.mount_bin, &args).await?;
            if !out.status.success() {
                // A failed mount must not leave its empty mountpoint directory
                // behind — the file explorer would list it as a stale pool.
                // Best effort: a directory that already held something stays.
                let _ = std::fs::remove_dir(mountpoint);
                return Ok(PrivResponse {
                    exit_code: out.status.code().unwrap_or(-1) as i32,
                    stdout: out.stdout,
                    stderr: out.stderr,
                });
            }
            // A freshly formatted pool mounts as root:root 0755. Everything
            // that is supposed to write into it runs unprivileged, so the pool
            // root is opened up here, once, instead of failing every protocol
            // and app later with a bare "permission denied".
            let mut stderr = out.stderr;
            if let Some(note) = apply_pool_mode(mountpoint, allowlist.pool_mode) {
                tracing::warn!(mountpoint = %mountpoint.display(), mode = format!("{:04o}", allowlist.pool_mode), "{note}");
                stderr.extend_from_slice(note.as_bytes());
                stderr.push(b'\n');
            } else {
                tracing::info!(mountpoint = %mountpoint.display(), mode = format!("{:04o}", allowlist.pool_mode), "pool root mode set");
            }
            return Ok(PrivResponse {
                exit_code: 0,
                stdout: out.stdout,
                stderr,
            });
        }
        AllowedCommand::UnmountBlock { mountpoint, force } => {
            // Escalating unmount: a plain `umount` first; when the caller asked
            // for force, a busy mount is retried with `-f` (force) and then
            // `-l` (lazy detach) so a stale handle cannot block a destructive
            // operation the user already confirmed.
            let out = umount_escalating(allowlist, &mountpoint.display().to_string(), *force).await?;
            return Ok(PrivResponse {
                exit_code: out.status.code().unwrap_or(-1) as i32,
                stdout: out.stdout,
                stderr: out.stderr,
            });
        }
        AllowedCommand::SmartInfo { device } => (
            allowlist.smartctl_bin.clone(),
            vec!["-H".into(), "-A".into(), device.display().to_string()],
        ),
        AllowedCommand::FormatFilesystem { device, fs_type, label, force } => {
            // A device nothing can be opened on is refused before wipefs runs,
            // so the failure is a plain-language one instead of two tools'
            // variants of "No such file or directory".
            if let Some(reason) = (allowlist.device_check)(device) {
                return Ok(PrivResponse {
                    exit_code: 1,
                    stdout: Vec::new(),
                    stderr: format!("format failed: {reason}").into_bytes(),
                });
            }
            // Formatting refuses a device that is still mounted — "apparently
            // in use by the system" (mke2fs) or "Resource busy" (mkfs.btrfs) —
            // and the control plane's registry can miss a mount it did not
            // create. So privd releases every kernel mount of the device (and
            // of its partitions) itself, then erases stale signatures, then
            // formats, retrying while the kernel finishes releasing the disk.
            let target = device.display().to_string();
            let kname = device
                .file_name()
                .map(|n| n.to_string_lossy().to_string())
                .unwrap_or_default();

            let mut stderr: Vec<u8> = Vec::new();
            // Stacked holders first: everything below them (a swap area, a dm
            // mapping, an md array) has to go before the disk can be erased,
            // and releasing the stack is what makes the mounts unmountable too.
            for note in release_device_holders(allowlist, &kname, &target, *force).await {
                tracing::info!(device = %target, "{note}");
                stderr.extend_from_slice(note.as_bytes());
                stderr.push(b'\n');
            }
            // Two sweeps: a nested mount only appears once its outer one is gone.
            for _ in 0..2 {
                let mounts = device_mounts(device, &allowlist.dev_root);
                if mounts.is_empty() {
                    break;
                }
                for mountpoint in mounts {
                    if !mountpoint.starts_with(&allowlist.allowed_root) {
                        // Never silently unmount a host or OS volume: report it
                        // so the operator decides.
                        return Ok(PrivResponse {
                            exit_code: 1,
                            stdout: Vec::new(),
                            stderr: format!(
                                "{target} is mounted at {} outside {}; unmount it manually before formatting",
                                mountpoint.display(),
                                allowlist.allowed_root.display()
                            )
                            .into_bytes(),
                        });
                    }
                    let out = umount_escalating(allowlist, &mountpoint.display().to_string(), *force).await?;
                    if out.status.success() {
                        tracing::info!(device = %target, mountpoint = %mountpoint.display(), "released before format");
                    } else {
                        stderr.extend_from_slice(&out.stderr);
                        stderr.push(b'\n');
                    }
                }
            }

            // Erase whatever signatures are left (the "force erase" the user
            // confirmed). Best effort: a missing wipefs is not fatal because
            // mkfs writes fresh signatures anyway, but a failed wipe is worth
            // reporting alongside a failed format.
            match run_argv(&allowlist.wipefs_bin, &["-a".into(), target.clone()]).await {
                Ok(out) if out.status.success() => {}
                Ok(out) => {
                    stderr.extend_from_slice(&out.stderr);
                    stderr.push(b'\n');
                }
                Err(_) => tracing::debug!(bin = %allowlist.wipefs_bin, "wipefs unavailable; skipping signature erase"),
            }

            let (bin, force_arg) = if fs_type == "ext4" {
                (allowlist.mkfs_ext4_bin.clone(), "-F")
            } else {
                (allowlist.mkfs_btrfs_bin.clone(), "-f")
            };
            let mut args = vec!["-L".into(), label.clone(), target.clone()];
            if *force { args.insert(0, force_arg.into()); }

            let mut last: Option<std::process::Output> = None;
            for attempt in 0..3 {
                let out = run_argv(&bin, &args).await?;
                let busy = !out.status.success() && is_busy_error(&out);
                // Give an asynchronously unmounting filesystem a moment to
                // release the device before the next attempt.
                if busy && attempt < 2 {
                    tracing::warn!(device = %target, attempt, "device still busy; retrying format");
                    tokio::time::sleep(std::time::Duration::from_millis(500)).await;
                    last = Some(out);
                    continue;
                }
                last = Some(out);
                break;
            }
            let out = last.expect("at least one format attempt");
            if !out.status.success() {
                // Say what is still holding the disk: a stacked holder (dm/md)
                // or a mount that survived the sweep is the whole difference
                // between "try again" and "this disk is in use by something else".
                if let Some(reason) = busy_reason(&kname, &target) {
                    stderr.extend_from_slice(reason.as_bytes());
                    stderr.push(b'\n');
                }
                stderr.extend_from_slice(&out.stderr);
                return Ok(PrivResponse {
                    exit_code: out.status.code().unwrap_or(-1) as i32,
                    stdout: out.stdout,
                    stderr,
                });
            }
            return Ok(PrivResponse {
                exit_code: 0,
                stdout: out.stdout,
                stderr,
            });
        }
        AllowedCommand::CreateBtrfsPool { device, label } => {
            if let Some(reason) = (allowlist.device_check)(device) {
                return Ok(PrivResponse {
                    exit_code: 1,
                    stdout: Vec::new(),
                    stderr: reason.into_bytes(),
                });
            }
            (
                allowlist.mkfs_btrfs_bin.clone(),
                vec!["-f".into(), "-L".into(), label.clone(), device.display().to_string()],
            )
        }
        AllowedCommand::RemoveMountpoint { mountpoint } => {
            // Not a subprocess: rmdir the empty mountpoint. A directory that
            // still holds anything stays (rmdir refuses non-empty), and the
            // failure is reported in-band like any other command failure.
            return match std::fs::remove_dir(mountpoint) {
                Ok(()) => Ok(PrivResponse {
                    exit_code: 0,
                    stdout: Vec::new(),
                    stderr: Vec::new(),
                }),
                Err(e) => Ok(PrivResponse {
                    exit_code: 1,
                    stdout: Vec::new(),
                    stderr: format!("rmdir {}: {e}", mountpoint.display()).into_bytes(),
                }),
            };
        }
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
            // Build one (bin, argv) plan per target, then run each plan in
            // order, failing closed *within* a target: a failed validation
            // stops that target's reload, not every other daemon's. A host
            // without samba has no testparm, and a container has no systemctl;
            // aborting the whole batch there would leave a WebDAV or rsync
            // change unwritten (docs/design/02#6 step 4).
            let mut plans: Vec<(String, Vec<(String, Vec<String>)>)> = Vec::new();
            for t in targets {
                let mut steps: Vec<(String, Vec<String>)> = Vec::new();
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
                    // The protocol daemons added in v0.4 are started by their
                    // first share: `reload-or-restart` brings a stopped daemon
                    // up and reloads a running one, so an operator does not have
                    // to enable each protocol's unit by hand before the first
                    // FTP/SFTP/WebDAV/rsync share exists.
                    "ftp" => {
                        // vsftpd reloads its config on SIGHUP (systemd reload).
                        steps.push((
                            allowlist.systemctl_bin.clone(),
                            vec!["reload-or-restart".into(), "vsftpd".into()],
                        ));
                    }
                    "sftp" => {
                        let conf = allowlist.config_path("sftp");
                        // Validate before reload: a broken sshd_config would
                        // otherwise take the dedicated sftp instance down.
                        steps.push((
                            allowlist.sshd_bin.clone(),
                            vec!["-t".into(), "-f".into(), conf.display().to_string()],
                        ));
                        steps.push((
                            allowlist.systemctl_bin.clone(),
                            vec!["reload-or-restart".into(), "onyx-sftp".into()],
                        ));
                    }
                    "webdav" => {
                        steps.push((
                            allowlist.systemctl_bin.clone(),
                            vec!["reload-or-restart".into(), "onyx-davd".into()],
                        ));
                    }
                    "rsync" => {
                        steps.push((
                            allowlist.systemctl_bin.clone(),
                            vec!["reload-or-restart".into(), "rsyncd".into()],
                        ));
                    }
                    _ => unreachable!("validated target"),
                }
                plans.push((t.clone(), steps));
            }
            let mut resp = PrivResponse {
                exit_code: 0,
                stdout: Vec::new(),
                stderr: Vec::new(),
            };
            for (target, steps) in plans {
                for (bin, args) in steps {
                    match run_argv(&bin, &args).await {
                        // The binary is not part of this deployment, so there is
                        // nothing here to reload. That is not a failure of the
                        // config: a missing testparm means no samba, and a
                        // missing systemctl means the daemon is reached another
                        // way. Say so and move on.
                        Err(status) if status.code() == tonic::Code::NotFound => {
                            resp.stdout.extend_from_slice(
                                format!("{target}: skipped {bin} (not installed)\n").as_bytes(),
                            );
                        }
                        Err(status) => return Err(status),
                        Ok(out) if !out.status.success() => {
                            resp.exit_code = out.status.code().unwrap_or(-1) as i32;
                            resp.stdout.extend_from_slice(&out.stdout);
                            resp.stderr.extend_from_slice(&out.stderr);
                            break; // this target's validation gate: don't reload an invalid config
                        }
                        Ok(_) => {}
                    }
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

/// Unmount one mountpoint, escalating a busy mount: plain `umount` first,
/// then (only when the caller asked for force) `umount -f` and finally
/// `umount -l`. The last attempt's output is returned either way.
async fn umount_escalating(
    allowlist: &Allowlist,
    target: &str,
    force: bool,
) -> Result<std::process::Output, Status> {
    let mut attempts: Vec<Vec<String>> = vec![vec![target.to_string()]];
    if force {
        attempts.push(vec!["-f".into(), target.to_string()]);
        attempts.push(vec!["-l".into(), target.to_string()]);
    }
    let mut last: Option<std::process::Output> = None;
    for args in attempts {
        let out = run_argv(&allowlist.umount_bin, &args).await?;
        if out.status.success() {
            return Ok(out);
        }
        last = Some(out);
    }
    Ok(last.expect("at least one umount attempt"))
}

/// True when `candidate` is a partition of disk `disk` (sdc/sdc1,
/// nvme0n1/nvme0n1p1) — the same grouping storaged uses, so a device mounted
/// through its partition still counts as busy.
fn is_partition_device(disk: &str, candidate: &str) -> bool {
    candidate.strip_prefix(disk).is_some_and(|suffix| {
        !suffix.is_empty()
            && (suffix.chars().all(|c| c.is_ascii_digit())
                || suffix
                    .strip_prefix('p')
                    .is_some_and(|s| !s.is_empty() && s.chars().all(|c| c.is_ascii_digit())))
    })
}

/// Undo the octal escaping the kernel applies to mount fields (\040 et al) so
/// a mountpoint with a space can still be unmounted by path.
fn unescape_mount_field(field: &str) -> String {
    let bytes = field.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'\\' && i + 3 < bytes.len() {
            let octal = &field[i + 1..i + 4];
            if let Ok(value) = u8::from_str_radix(octal, 8) {
                out.push(value);
                i += 4;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).to_string()
}

/// Every kernel mountpoint carrying `device` or one of its partitions, read
/// from /proc/self/mounts. Only sources under `dev_root` are considered: that
/// is the device tree this helper is allowed to act on, and it keeps a test's
/// fake device names from matching real host mounts.
fn device_mounts(device: &Path, dev_root: &Path) -> Vec<PathBuf> {
    let kname = match device.file_name().map(|n| n.to_string_lossy().to_string()) {
        Some(name) if !name.is_empty() => name,
        _ => return Vec::new(),
    };
    let Ok(content) = std::fs::read_to_string("/proc/self/mounts") else {
        return Vec::new();
    };
    let mut mounts = Vec::new();
    for line in content.lines() {
        let mut fields = line.split_whitespace();
        let (Some(source), Some(target)) = (fields.next(), fields.next()) else {
            continue;
        };
        let source = Path::new(source);
        if !source.starts_with(dev_root) {
            continue;
        }
        let Some(name) = source.file_name().map(|n| n.to_string_lossy().to_string()) else {
            continue;
        };
        if name == kname || is_partition_device(&kname, &name) {
            mounts.push(PathBuf::from(unescape_mount_field(target)));
        }
    }
    mounts
}

/// True when a failed command looks like the kernel refusing a busy device.
fn is_busy_error(output: &std::process::Output) -> bool {
    let stderr = String::from_utf8_lossy(&output.stderr).to_ascii_lowercase();
    stderr.contains("resource busy")
        || stderr.contains("device or resource busy")
        || stderr.contains("apparently in use by the system")
        || stderr.contains("is mounted")
}

/// Explain what still holds a busy device: remaining mounts, then stacked
/// holders from sysfs (device-mapper, mdraid, loop). Returns None when the
/// device looks free, so a failure that has nothing to do with busy-ness is
/// reported without a misleading note.
fn busy_reason(kname: &str, target: &str) -> Option<String> {
    if kname.is_empty() {
        return None;
    }
    let mut notes: Vec<String> = Vec::new();
    let mounts = device_mounts(Path::new(target), Path::new("/dev"));
    if !mounts.is_empty() {
        let list: Vec<String> = mounts.iter().map(|m| m.display().to_string()).collect();
        notes.push(format!("still mounted at {}", list.join(", ")));
    }
    let holders = std::fs::read_dir(format!("/sys/class/block/{kname}/holders"))
        .map(|entries| {
            entries
                .filter_map(|e| e.ok().map(|e| e.file_name().to_string_lossy().to_string()))
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    if !holders.is_empty() {
        notes.push(format!("held by {}", holders.join(", ")));
    }
    if notes.is_empty() {
        return None;
    }
    Some(format!("{target} is busy: {}", notes.join("; ")))
}

/// Parse an octal mode (`0777`, `0o777`, `777`). The leading digit is kept when
/// it is there — on a shared pool root `2775`/`1777` mean something (setgid for
/// group inheritance, sticky for delete protection) — and anything above 07777,
/// non-octal or empty is refused rather than guessed at.
fn parse_mode(raw: &str) -> Option<u32> {
    let trimmed = raw.trim();
    let digits = trimmed
        .strip_prefix("0o")
        .or_else(|| trimmed.strip_prefix("0O"))
        .unwrap_or(trimmed);
    if digits.is_empty() || !digits.chars().all(|c| matches!(c, '0'..='7')) {
        return None;
    }
    u32::from_str_radix(digits, 8).ok().filter(|v| *v <= 0o7777)
}

/// Active swap areas on a disk or any of its partitions, from /proc/swaps.
/// An active swap signature is the classic reason `mke2fs` refuses a disk with
/// "is apparently in use by the system": the kernel, not a mount, is holding
/// it — so unmounting alone can never release it.
fn swap_areas_for(dev_root: &Path, kname: &str) -> Vec<PathBuf> {
    if kname.is_empty() {
        return Vec::new();
    }
    let Ok(content) = std::fs::read_to_string("/proc/swaps") else {
        return Vec::new();
    };
    let mut areas = Vec::new();
    for line in content.lines().skip(1) {
        let Some(source) = line.split_whitespace().next() else {
            continue;
        };
        let path = Path::new(source);
        // Only the device tree this helper acts on (see `device_mounts`).
        if !path.starts_with(dev_root) {
            continue;
        }
        let Some(name) = path.file_name().map(|n| n.to_string_lossy().to_string()) else {
            continue;
        };
        if name == kname || is_partition_device(kname, &name) {
            areas.push(path.to_path_buf());
        }
    }
    areas
}

/// The devices stacked on top of `kname` (device-mapper, mdraid, loop), read
/// from sysfs. These are what keep the disk open once every mount is gone.
fn stacked_holders(kname: &str) -> Vec<String> {
    if kname.is_empty() {
        return Vec::new();
    }
    let Ok(entries) = std::fs::read_dir(format!("/sys/class/block/{kname}/holders")) else {
        return Vec::new();
    };
    let mut holders: Vec<String> = entries
        .filter_map(|e| e.ok().map(|e| e.file_name().to_string_lossy().to_string()))
        // Kernel-provided names still become argv, so only the shape the
        // kernel actually uses is accepted.
        .filter(|name| {
            !name.is_empty()
                && name.len() <= 64
                && name.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.'))
        })
        .collect();
    holders.sort();
    holders
}

/// Release everything that holds a disk open, so a format the user confirmed
/// can actually run (docs/design/05 §2 "force erase"). Covers the three
/// stacks that survive an unmount sweep: an active swap area, a device-mapper
/// mapping (LVM/bcache) and an md array, plus loop devices. Every step is best
/// effort and reported: a disk still busy afterwards fails with the tool's own
/// message *plus* a note saying what is left, which is the difference between
/// "try again" and "this disk belongs to something else".
///
/// Only reached when the caller set `force` — the confirmation the operator
/// gave for erasing the disk.
async fn release_device_holders(allowlist: &Allowlist, kname: &str, target: &str, force: bool) -> Vec<String> {
    let mut notes: Vec<String> = Vec::new();
    if !force || kname.is_empty() {
        return notes;
    }
    // Only the device tree this helper acts on (same guard as `device_mounts`):
    // it keeps a test's fake device names from ever matching a real host swap
    // area — swapoff on the wrong device would end the host's memory.
    let swap_areas: Vec<PathBuf> = (allowlist.swap_probe)(&allowlist.dev_root, kname)
        .into_iter()
        .filter(|area| area.starts_with(&allowlist.dev_root))
        .collect();
    for area in swap_areas {
        match run_argv(&allowlist.swapoff_bin, &[area.display().to_string()]).await {
            Ok(out) if out.status.success() => notes.push(format!("deactivated swap on {}", area.display())),
            Ok(out) => notes.push(format!(
                "could not deactivate swap on {}: {}",
                area.display(),
                String::from_utf8_lossy(&out.stderr).trim()
            )),
            Err(_) => notes.push(format!(
                "{} is active swap and {} is unavailable to deactivate it",
                area.display(),
                allowlist.swapoff_bin
            )),
        }
    }
    for holder in (allowlist.holders_probe)(kname) {
        let path = allowlist.dev_root.join(&holder).display().to_string();
        // dm-0 -> device mapper; md0 -> mdraid; loop0 -> loop. Anything else is
        // a stack this helper has no business tearing down.
        let released = if holder.starts_with("dm-") {
            run_argv(&allowlist.dmsetup_bin, &["remove".to_string(), path.clone()]).await
        } else if holder.starts_with("md") {
            run_argv(&allowlist.mdadm_bin, &["--stop".to_string(), path.clone()]).await
        } else if holder.starts_with("loop") {
            run_argv(&allowlist.losetup_bin, &["-d".to_string(), path.clone()]).await
        } else {
            notes.push(format!("{target} is held by {holder}, which Onyx does not manage"));
            continue;
        };
        match released {
            Ok(out) if out.status.success() => notes.push(format!("released {holder}")),
            Ok(out) => notes.push(format!(
                "could not release {holder}: {}",
                String::from_utf8_lossy(&out.stderr).trim()
            )),
            Err(_) => notes.push(format!("{target} is held by {holder}, which could not be released")),
        }
    }
    notes
}

/// Make a freshly mounted pool usable by the services that write to it: a new
/// ext4/btrfs root is root:root 0755, which leaves every unprivileged writer
/// (davd, SFTP/FTP/rsync daemons, apps, the object-store tier target) unable to
/// create anything. Returns a note when the mode could not be applied — the
/// mount itself succeeded, so this is a warning, not a failure.
fn apply_pool_mode(mountpoint: &Path, mode: u32) -> Option<String> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        match std::fs::set_permissions(mountpoint, std::fs::Permissions::from_mode(mode & 0o7777)) {
            Ok(()) => None,
            Err(e) => Some(format!(
                "mounted at {} but its mode could not be set to {:04o}: {e} (writes may fail)",
                mountpoint.display(),
                mode & 0o7777
            )),
        }
    }
    #[cfg(not(unix))]
    {
        let _ = (mountpoint, mode);
        None
    }
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
        // A missing binary is its own answer: callers that can carry on without
        // it (a reload on a host that does not run that daemon) need to tell it
        // apart from a command that ran and failed.
        Ok(Err(e)) if e.kind() == std::io::ErrorKind::NotFound => {
            Err(Status::not_found(format!("{bin} is not installed")))
        }
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
        // Write to a temp name, chmod, then atomically rename into place:
        // exec'ing a file while its overlayfs copy-up / write handle is still
        // settling races as ETXTBSY ("Text file busy") and flakes CI. A rename
        // gives the exec a fully settled dentry (the same pattern cargo and
        // rustup use when installing binaries).
        let tmp = dir.join(format!("{}.tmp", name));
        fs::write(&tmp, script).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(&tmp, fs::Permissions::from_mode(0o755)).unwrap();
        }
        fs::rename(&tmp, &p).unwrap();
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
        // The fake devices are plain paths under a temp "dev root": nothing on
        // disk backs them, which is exactly what the real check refuses. The
        // argv tests care about the command that would run, so the check is
        // stubbed here and exercised on its own below.
        let mut allowlist = Allowlist::new(
            &marker("btrfs"),
            &marker("lsblk"),
            &marker("mount"),
            &marker("umount"),
            &marker("mkdir"),
            &marker("smartctl"),
            &marker("mkfs.btrfs"),
            &marker("mkfs.ext4"),
            &marker("wipefs"),
            &marker("testparm"),
            &marker("systemctl"),
            &marker("exportfs"),
            &marker("sshd"),
            dir,             // config dir = temp dir
            dir,             // allowed root = temp dir
            dir.join("dev").as_path(), // dev root = temp dir/dev (created by tests)
        );
        allowlist.device_check = |_| None;
        // The holder-release path is exercised with explicit probes so a unit
        // test does not depend on the host's swap or dm state.
        allowlist.swap_probe = |_, _| Vec::new();
        allowlist.holders_probe = |_| Vec::new();
        allowlist.with_holder_tools(
            &marker("swapoff"),
            &marker("dmsetup"),
            &marker("mdadm"),
            &marker("losetup"),
        )
    }

    async fn run_request(a: &Allowlist, req: &PrivRequest) -> Result<PrivResponse, Status> {
        let cmd = a.validate(req)?;
        execute(a, &cmd).await
    }

    /// The real check, on a device the kernel may list but this namespace has no
    /// node for: the operator must get that sentence, not two tools' variants of
    /// "No such file or directory" — and nothing may be run against the path.
    #[tokio::test]
    async fn format_and_mount_refuse_a_device_with_no_node_here() {
        let dir = TempDir::new("missingnode");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let a = test_allowlist(dir.path()); // stub the check for other tests
        let real = Allowlist { device_check: device_node_error, ..test_allowlist(dir.path()) };

        let device = dev_root.join("sde");
        let format = run_request(
            &real,
            &request(
                PrivOp::FormatFilesystem,
                vec![device.to_str().unwrap(), "ext4", "main-pool", "true"],
            ),
        )
        .await
        .unwrap();
        assert_eq!(format.exit_code, 1);
        let stderr = String::from_utf8_lossy(&format.stderr);
        assert!(stderr.contains("does not exist in this container"), "{stderr}");
        assert!(stderr.starts_with("format failed:"), "{stderr}");

        let mountpoint = dir.path().join("pool");
        let mount = run_request(
            &real,
            &request(
                PrivOp::MountBlock,
                vec![device.to_str().unwrap(), mountpoint.to_str().unwrap()],
            ),
        )
        .await
        .unwrap();
        assert_eq!(mount.exit_code, 1);
        assert!(
            String::from_utf8_lossy(&mount.stderr).contains("does not exist in this container")
        );
        // Refused before anything ran: no mountpoint left behind, and no argv log.
        assert!(!mountpoint.exists(), "a refused mount must not create its mountpoint");
        assert!(!dir.path().join("argv.log").exists(), "no tool may run for an unusable device");
        drop(a);
    }

    /// "Force erase" has to mean something on a disk that was used before: the
    /// stacks that keep it open (active swap, dm mapping, md array, loop device)
    /// are released before wipefs/mkfs run, and each release is named in the
    /// output so the operator can see what the disk belonged to.
    #[tokio::test]
    async fn forced_format_releases_the_stacks_holding_the_disk() {
        let dir = TempDir::new("holders");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let log = dir.path().join("argv.log");
        let fake = |name: &str| {
            fake_bin(
                dir.path(),
                name,
                &format!(
                    "#!/bin/sh\n{{ printf '%s' \"$0\"; printf '|%s' \"$@\"; printf '\\n'; }} >> \"{}\"\nexit 0\n",
                    log.display()
                ),
            )
        };
        let mut a = test_allowlist(dir.path());
        // The probe answers from this temp dev root, so no real /dev path is
        // ever handed to swapoff/dmsetup/mdadm/losetup.
        let swap_path = dev_root.join("sdc1");
        a.swap_probe = |dev_root, _| vec![dev_root.join("sdc1")];
        a.holders_probe = |kname| {
            if kname == "sdc" {
                vec!["dm-0".to_string(), "md0".to_string(), "loop0".to_string()]
            } else {
                Vec::new()
            }
        };
        a = a.with_holder_tools(
            &fake("swapoff"),
            &fake("dmsetup"),
            &fake("mdadm"),
            &fake("losetup"),
        );

        let device = dev_root.join("sdc");
        let resp = run_request(
            &a,
            &request(
                PrivOp::FormatFilesystem,
                vec![device.to_str().unwrap(), "ext4", "data", "true"],
            ),
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0);

        let recorded = fs::read_to_string(&log).unwrap();
        let line = |needle: &str| {
            recorded
                .lines()
                .find(|l| l.contains(needle))
                .unwrap_or_else(|| panic!("no {needle} in argv.log:\n{recorded}"))
                .to_string()
        };
        assert!(line("/swapoff").ends_with(&swap_path.display().to_string()), "{recorded}");
        assert!(line("/dmsetup").contains(&format!("|remove|{}", dev_root.join("dm-0").display())), "{recorded}");
        assert!(line("/mdadm").contains(&format!("|--stop|{}", dev_root.join("md0").display())), "{recorded}");
        assert!(line("/losetup").contains(&format!("|-d|{}", dev_root.join("loop0").display())), "{recorded}");
        // The release is reported, so a partial one is visible to the operator.
        let stderr = String::from_utf8_lossy(&resp.stderr);
        assert!(stderr.contains("released dm-0"), "{stderr}");
        assert!(stderr.contains("deactivated swap"), "{stderr}");
    }

    /// Without the confirmation, nothing is torn down: the holder is reported
    /// and the disk is left alone.
    #[tokio::test]
    async fn unforced_format_does_not_touch_holders() {
        let dir = TempDir::new("noholders");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let mut a = test_allowlist(dir.path());
        a.holders_probe = |_| vec!["dm-0".to_string()];
        a.swap_probe = |dev_root, _| vec![dev_root.join("sdc1")];

        let device = dev_root.join("sdc");
        run_request(
            &a,
            &request(
                PrivOp::FormatFilesystem,
                vec![device.to_str().unwrap(), "ext4", "data", "false"],
            ),
        )
        .await
        .unwrap();
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap_or_default();
        assert!(!log.contains("dmsetup"), "no holder may be released unforced: {log}");
        assert!(!log.contains("swapoff"), "swap must stay for an unforced run: {log}");
    }

    /// A mounted pool root is opened up for the unprivileged writers that use
    /// it — a fresh filesystem is root:root 0755, which would make every share,
    /// app and tier target read-only.
    #[tokio::test]
    async fn mount_opens_up_the_pool_root_for_unprivileged_writers() {
        use std::os::unix::fs::PermissionsExt;
        let dir = TempDir::new("poolmode");
        let mountpoint = dir.path().join("mnt").join("main-pool");
        fs::create_dir_all(&mountpoint).unwrap();
        fs::set_permissions(&mountpoint, fs::Permissions::from_mode(0o755)).unwrap();

        let a = test_allowlist(dir.path()).with_pool_mode(0o777);
        let resp = run_request(
            &a,
            &request(
                PrivOp::MountBlock,
                vec![
                    dir.path().join("dev").join("sdb").to_str().unwrap(),
                    mountpoint.to_str().unwrap(),
                ],
            ),
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "{:?}", String::from_utf8_lossy(&resp.stderr));
        let mode = fs::metadata(&mountpoint).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o777, "the pool root must be writable by every service");
    }

    #[test]
    fn parse_mode_accepts_octal_and_rejects_the_rest() {
        assert_eq!(parse_mode("0777"), Some(0o777));
        assert_eq!(parse_mode("0o775"), Some(0o775));
        assert_eq!(parse_mode("775"), Some(0o775));
        assert_eq!(parse_mode("2775"), Some(0o2775));
        // Above 07777, non-octal, empty, or smuggled: refused outright.
        assert_eq!(parse_mode("017777"), None);
        assert_eq!(parse_mode("0888"), None);
        assert_eq!(parse_mode("rwx"), None);
        assert_eq!(parse_mode(""), None);
    }

    /// An unknown stacked holder is reported rather than torn down: privd only
    /// stops stacks it understands (dm, md, loop).
    #[tokio::test]
    async fn unknown_holders_are_reported_not_released() {
        let dir = TempDir::new("unknownholder");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let mut a = test_allowlist(dir.path());
        a.holders_probe = |_| vec!["bcache0".to_string()];

        let notes = release_device_holders(&a, "sdc", "/dev/sdc", true).await;
        assert_eq!(notes.len(), 1, "{notes:?}");
        assert!(notes[0].contains("bcache0"), "{notes:?}");
        assert!(notes[0].contains("does not manage"), "{notes:?}");
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap_or_default();
        assert!(log.is_empty(), "nothing may run for a stack Onyx does not manage: {log}");
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
            "mkfs.btrfs",
            "mkfs.ext4",
            "wipefs",
            "testparm",
            "systemctl",
            "exportfs",
            "sshd",
            dir.path(),
            dir.path(),
            dir.path().join("dev").as_path(),
        );
        let resp = run_request(&a, &request(PrivOp::BtrfsFilesystemShowRaw, vec![])).await;
        assert!(resp.is_err(), "missing binary must fail closed");
    }

    #[tokio::test]
    async fn format_filesystem_selects_ext4_and_force_flag() {
        let dir = TempDir::new("format");
        let dev_root = dir.path().join("dev");
        fs::create_dir_all(&dev_root).unwrap();
        let a = test_allowlist(dir.path());
        let resp = run_request(
            &a,
            &request(
                PrivOp::FormatFilesystem,
                vec![dev_root.join("sdb").to_str().unwrap(), "ext4", "data", "true"],
            ),
        ).await.unwrap();
        assert_eq!(resp.exit_code, 0);
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        let mkfs = log.lines().position(|line| line.contains("mkfs.ext4")).expect("mkfs ran");
        assert!(log.lines().nth(mkfs).unwrap().contains("|-F") && log.lines().nth(mkfs).unwrap().contains("|-L|data"), "{log}");
        // Force erase: stale signatures are wiped before the new filesystem is
        // written, so a surviving superblock cannot confuse the new pool.
        let wipe = log.lines().position(|line| line.contains("wipefs")).expect("wipefs ran");
        assert!(wipe < mkfs, "wipefs must run before mkfs: {log}");
        assert!(log.lines().nth(wipe).unwrap().contains("|-a|"), "wipefs -a: {log}");
    }

    #[test]
    fn device_mounts_matches_partitions_of_the_whole_disk() {
        assert!(is_partition_device("sdc", "sdc1"));
        assert!(is_partition_device("sdc", "sdc12"));
        assert!(is_partition_device("nvme0n1", "nvme0n1p1"));
        assert!(!is_partition_device("sdc", "sdc"));
        assert!(!is_partition_device("sdc", "sdd1"));
        assert!(!is_partition_device("sdc", "sdcx"));
        // Kernel escaping in /proc/self/mounts uses octal \040 for a space.
        assert_eq!(unescape_mount_field("/mnt/onyx/main\\040pool"), "/mnt/onyx/main pool");
        assert_eq!(unescape_mount_field("/mnt/onyx/main-pool"), "/mnt/onyx/main-pool");
    }

    #[tokio::test]
    async fn unmount_block_force_escalates_until_it_succeeds() {
        let dir = TempDir::new("umount-force");
        // Fake umount that only succeeds for the lazy `-l` variant, so the
        // escalating force path must try plain, `-f`, then `-l`.
        let log = dir.path().join("argv.log");
        let script = format!(
            "#!/bin/sh\n{{ printf '%s' \"$0\"; printf '|%s' \"$@\"; printf '\\n'; }} >> \"{}\"\n[ \"$1\" = \"-l\" ] && exit 0\nexit 1\n",
            log.display()
        );
        let umount = fake_bin(dir.path(), "umount", &script);
        let record = |name: &str| fake_bin(dir.path(), name, "#!/bin/sh\nexit 0\n");
        let a = Allowlist::new(
            &record("btrfs"),
            &record("lsblk"),
            &record("mount"),
            &umount,
            &record("mkdir"),
            &record("smartctl"),
            &record("mkfs.btrfs"),
            &record("mkfs.ext4"),
            &record("wipefs"),
            &record("testparm"),
            &record("systemctl"),
            &record("exportfs"),
            &record("sshd"),
            dir.path(),
            dir.path(),
            dir.path().join("dev").as_path(),
        );
        let mountpoint = dir.path().join("usb-stick");
        fs::create_dir_all(&mountpoint).unwrap();

        // Without force a busy mount is reported as-is, no retry.
        let resp = run_request(
            &a,
            &request(PrivOp::UnmountBlock, vec![mountpoint.to_str().unwrap()]),
        )
        .await
        .unwrap();
        assert_ne!(resp.exit_code, 0, "plain umount must fail");

        let resp = run_request(
            &a,
            &request(PrivOp::UnmountBlock, vec![mountpoint.to_str().unwrap(), "true"]),
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "forced unmount must fall back to umount -l");
        let log = fs::read_to_string(&log).unwrap();
        assert!(log.contains("|-f|"), "force attempt logged: {log}");
        assert!(log.contains("|-l|"), "lazy attempt logged: {log}");
    }

    #[test]
    fn unmount_block_validates_force_argument() {
        let dir = TempDir::new("umount-bad");
        let a = test_allowlist(dir.path());
        let mountpoint = dir.path().join("data");
        fs::create_dir_all(&mountpoint).unwrap();

        // force must be a boolean literal
        let err = a
            .validate(&request(
                PrivOp::UnmountBlock,
                vec![mountpoint.to_str().unwrap(), "yes"],
            ))
            .expect_err("bad force rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");

        // too many args
        let err = a
            .validate(&request(
                PrivOp::UnmountBlock,
                vec![mountpoint.to_str().unwrap(), "true", "extra"],
            ))
            .expect_err("extra arg rejected");
        assert_eq!(err.code(), tonic::Code::InvalidArgument, "{err}");
    }

    #[tokio::test]
    async fn remove_mountpoint_rmdirs_only_empty_directories() {
        let dir = TempDir::new("rmmount");
        let a = test_allowlist(dir.path());

        // An empty mountpoint directory (left over after an unmount) goes away.
        let empty = dir.path().join("stale-pool");
        fs::create_dir_all(&empty).unwrap();
        let resp = run_request(
            &a,
            &request(PrivOp::RemoveMountpoint, vec![empty.to_str().unwrap()]),
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        assert!(!empty.exists(), "empty mountpoint should be removed");

        // A directory that still holds data is never removed.
        let busy = dir.path().join("live-pool");
        fs::create_dir_all(&busy).unwrap();
        fs::write(busy.join("file.txt"), b"data").unwrap();
        let resp = run_request(
            &a,
            &request(PrivOp::RemoveMountpoint, vec![busy.to_str().unwrap()]),
        )
        .await
        .unwrap();
        assert_ne!(resp.exit_code, 0, "non-empty directory must not be removed");
        assert!(busy.join("file.txt").is_file(), "data must survive");

        // Outside the allowed root is rejected outright.
        let outside = TempDir::new("rmmount-outside");
        let err = a
            .validate(&request(PrivOp::RemoveMountpoint, vec![outside.path().to_str().unwrap()]))
            .expect_err("outside root rejected");
        assert_eq!(err.code(), tonic::Code::PermissionDenied, "{err}");
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

    #[tokio::test]
    async fn write_daemon_config_covers_protocol_surface() {
        let dir = TempDir::new("writeproto");
        let a = test_allowlist(dir.path());
        let cases = [
            ("ftp", "vsftpd.conf"),
            ("sftp", "sshd_config"),
            ("webdav", "davd.conf"),
            ("rsync", "rsyncd.conf"),
        ];
        for (target, filename) in cases {
            let content = format!("# {target}\n");
            let resp = run_request(
                &a,
                &PrivRequest {
                    op: PrivOp::WriteDaemonConfig as i32,
                    args: vec![target.to_string(), content.clone()],
                },
            )
            .await
            .unwrap();
            assert_eq!(resp.exit_code, 0, "{target}: {}", String::from_utf8_lossy(&resp.stderr));
            let written = fs::read_to_string(dir.path().join(filename))
                .unwrap_or_else(|e| panic!("{target} did not land in {filename}: {e}"));
            assert_eq!(written, content);
        }
    }

    /// A reload target whose tool is not installed here is skipped, and the
    /// other targets still reload: a container has no systemctl and a host
    /// without samba has no testparm, and aborting the batch would leave a
    /// WebDAV or rsync change unwritten.
    #[tokio::test]
    async fn reload_daemons_skips_a_binary_this_host_lacks() {
        let dir = TempDir::new("reloadskip");
        let mut a = test_allowlist(dir.path());
        a.testparm_bin = dir.path().join("no-such-testparm").display().to_string();

        let resp = run_request(
            &a,
            &PrivRequest {
                op: PrivOp::ReloadDaemons as i32,
                args: ["smb", "webdav"].iter().map(|s| s.to_string()).collect(),
            },
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        let stdout = String::from_utf8_lossy(&resp.stdout);
        assert!(stdout.contains("skipped"), "the skip must be reported: {stdout}");
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        assert!(
            log.contains("reload-or-restart|onyx-davd"),
            "webdav must still reload:\n{log}"
        );
    }

    /// A target that fails validation stops *its* reload and says so, but does
    /// not take the other targets down with it.
    #[tokio::test]
    async fn reload_daemons_one_targets_failure_does_not_stop_the_rest() {
        let dir = TempDir::new("reloadfail");
        let mut a = test_allowlist(dir.path());
        a.testparm_bin = fake_bin(dir.path(), "testparm-rejects", "#!/bin/sh\necho 'bad config' >&2\nexit 3\n");

        let resp = run_request(
            &a,
            &PrivRequest {
                op: PrivOp::ReloadDaemons as i32,
                args: ["smb", "webdav"].iter().map(|s| s.to_string()).collect(),
            },
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 3, "the failing target's code is reported");
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        assert!(!log.contains("smbd"), "an invalid smb config must not be reloaded:\n{log}");
        assert!(
            log.contains("reload-or-restart|onyx-davd"),
            "the healthy target must still reload:\n{log}"
        );
    }

    #[tokio::test]
    async fn reload_daemons_covers_protocol_surface() {
        let dir = TempDir::new("reloadproto");
        let a = test_allowlist(dir.path());
        let resp = run_request(
            &a,
            &PrivRequest {
                op: PrivOp::ReloadDaemons as i32,
                args: ["ftp", "sftp", "webdav", "rsync"].iter().map(|s| s.to_string()).collect(),
            },
        )
        .await
        .unwrap();
        assert_eq!(resp.exit_code, 0, "stderr: {}", String::from_utf8_lossy(&resp.stderr));
        let log = fs::read_to_string(dir.path().join("argv.log")).unwrap();
        // Every protocol daemon is started *or* reloaded (`reload-or-restart`):
        // the first share that enables a protocol must bring its daemon up,
        // not just poke one that was assumed to be running.
        for unit in ["vsftpd", "onyx-davd", "rsyncd"] {
            let want = format!("/systemctl|reload-or-restart|{unit}");
            assert!(log.contains(&want), "{unit} reload missing:\n{log}");
        }
        // SFTP validates the generated sshd_config before reloading.
        let idx_check = log.lines().position(|l| l.contains("/sshd") && l.contains("-t") && l.contains("sshd_config"));
        assert!(idx_check.is_some(), "sshd config check missing:\n{log}");
        let idx_reload = log
            .lines()
            .position(|l| l.contains("/systemctl") && l.contains("reload-or-restart") && l.contains("onyx-sftp"));
        assert!(idx_reload.is_some(), "sftp reload missing:\n{log}");
        assert!(idx_check < idx_reload, "sshd -t must run before the sftp reload");
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
            "btrfs", "lsblk", "mount", "umount", "mkdir", "smartctl", "mkfs.btrfs", "mkfs.ext4",
            "wipefs",
            &failing, // testparm fails
            "systemctl", "exportfs", "sshd",
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
        for args in [
            vec!["httpd"],
            vec![],
            vec!["smb", "nfs", "ftp", "sftp", "webdav", "rsync", "smb"],
        ] {
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