//! onyx-privd — the privilege helper (docs/design/04-backend-service-architecture.md#7-privilege-helper).
//!
//! The single root process on the system. Design rules (04 §7):
//! * **No shell, ever.** Structured commands (`PrivReq { op, args }`) are
//!   validated against an allowlist and executed with explicit argv.
//! * Each op has a validator: paths are canonicalized and must stay inside the
//!   allowed root (`/mnt/onyx/`); datasets must match a safe charset.
//! * Runs under systemd socket activation (dev: `scripts/dev.sh`); every
//!   request is logged to the audit trail with caller identity.
//!
//! v0.1 allows the `btrfs` subset needed for pool discovery. Ops are additive:
//! add a `PrivOp` variant in `proto/onyx/v1/privd.proto`, a validator, and one
//! match arm below.

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
const ALLOWED_ROOT: &str = "/mnt/onyx";
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
    match runtime.block_on(run(&args.socket_path, &args.btrfs_bin)) {
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
}

impl Args {
    fn parse() -> Self {
        let mut socket_path = PathBuf::from("/run/onyx/onyx-privd.sock");
        let mut btrfs_bin = "btrfs".to_string();
        let mut it = std::env::args().skip(1);
        while let Some(arg) = it.next() {
            match arg.as_str() {
                "--socket-dir" => {
                    let dir = it.next().expect("--socket-dir requires a value");
                    socket_path = PathBuf::from(dir).join("onyx-privd.sock");
                }
                "--btrfs-bin" => {
                    btrfs_bin = it.next().expect("--btrfs-bin requires a value");
                }
                other => {
                    eprintln!("onyx-privd: unknown argument: {other}");
                    std::process::exit(2);
                }
            }
        }
        Args { socket_path, btrfs_bin }
    }
}

async fn run(socket_path: &Path, btrfs_bin: &str) -> Result<(), Box<dyn std::error::Error>> {
    // Note: privd keeps no state dir — it is stateless by design and logs the
    // audit trail through onyx-core (04 §8).

    let allowlist = Allowlist::new(btrfs_bin);

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
    /// `btrfs filesystem usage -b <mountpoint>` — one validated mount under /mnt/onyx/.
    BtrfsFilesystemUsage { mount: PathBuf },
}

struct Allowlist {
    btrfs_bin: String,
}

impl Allowlist {
    fn new(btrfs_bin: &str) -> Self {
        Allowlist { btrfs_bin: btrfs_bin.to_string() }
    }

    /// Validate a `PrivRequest` against the allowlist. Arguments are checked
    /// per-op; nothing here is ever passed to a shell.
    fn validate(&self, req: &PrivRequest) -> Result<AllowedCommand, Status> {		let op = match PrivOp::try_from(req.op) {
			Ok(op) if op != PrivOp::Unspecified => op,
			_ => return Err(Status::invalid_argument(format!("unknown or unspecified op: {}", req.op))),
		};
        match op {
            PrivOp::BtrfsFilesystemShowRaw => {
                if !req.args.is_empty() {
                    return Err(Status::invalid_argument(
                        "BTRFS_FILESYSTEM_SHOW_RAW takes no arguments",
                    ));
                }
                Ok(AllowedCommand::BtrfsFilesystemShowRaw)
            }
            PrivOp::BtrfsFilesystemUsageRaw => {
                if req.args.len() != 1 {
                    return Err(Status::invalid_argument(
                        "BTRFS_FILESYSTEM_USAGE_RAW requires exactly one mountpoint argument",
                    ));
                }
                let mount = validate_mount_path(&req.args[0])?;
                Ok(AllowedCommand::BtrfsFilesystemUsage { mount })
            }
            // Keep the match exhaustive; new ops fail closed until implemented.
            _ => Err(Status::unimplemented(format!("op {op:?} not implemented"))),
        }
    }
}

/// Canonicalize and require the path stay inside `/mnt/onyx` (04 §7: paths
/// must resolve inside allowed roots).
fn validate_mount_path(raw: &str) -> Result<PathBuf, Status> {
    let p = Path::new(raw);
    if !p.is_absolute() {
        return Err(Status::invalid_argument(format!("mountpoint must be absolute: {raw}")));
    }
    let canonical = p.canonicalize().map_err(|_| {
        Status::invalid_argument(format!("mountpoint does not exist or is inaccessible: {raw}"))
    })?;
    let allowed = Path::new(ALLOWED_ROOT);
    if !canonical.starts_with(allowed) {
        return Err(Status::permission_denied(format!(
            "path {raw} resolves outside allowed root {ALLOWED_ROOT}"
        )));
    }
    Ok(canonical)
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
        // peer verification lands (docs/design/04#5). For now, log op + args.		tracing::info!(op = ?PrivOp::try_from(req.op).unwrap_or(PrivOp::Unspecified), args = ?req.args, "privd request");

        match execute(&self.allowlist.btrfs_bin, &cmd).await {
            Ok(resp) => Ok(Response::new(resp)),
            Err(err) => Err(err),
        }
    }
}

/// Run one allowlisted command with `Command` (explicit argv, no shell) and a
/// hard timeout so a wedged btrfs can never hang the control plane.
async fn execute(btrfs_bin: &str, cmd: &AllowedCommand) -> Result<PrivResponse, Status> {
    let mut child = Command::new(btrfs_bin);
    match cmd {
        AllowedCommand::BtrfsFilesystemShowRaw => {
            child.args(["filesystem", "show", "--raw"]);
        }
        AllowedCommand::BtrfsFilesystemUsage { mount } => {
            child.args(["filesystem", "usage", "-b"]);
            child.arg(mount);
        }
    }

    let timed = match tokio::time::timeout(CMD_TIMEOUT, child.output()).await {
        Ok(Ok(output)) => output,
        Ok(Err(e)) => {
            return Err(Status::internal(format!(
                "failed to run {btrfs_bin}: {e} (is btrfs-progs installed?)"
            )))
        }
        Err(_) => return Err(Status::deadline_exceeded(format!("{btrfs_bin} timed out"))),
    };

    Ok(PrivResponse {
        exit_code: timed.status.code().unwrap_or(-1) as i32,
        stdout: timed.stdout,
        stderr: timed.stderr,
    })
}