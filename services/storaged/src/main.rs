//! onyx-storaged — the data plane for pools, datasets, quotas and scrub
//! (docs/design/05-storage-subsystem.md).
//!
//! Rules this skeleton upholds from the start (docs/design/02#1):
//! * no network listeners — only a unix socket in `/run/onyx`;
//! * privileged operations (mount, btrfs ioctls) go through onyx-privd, never
//!   directly — pool enumeration runs `btrfs filesystem show` inside privd;
//! * owns one SQLite DB (the pool registry) under `/var/lib/onyx/onyx-storaged/`.

mod discovery;
mod registry;

use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tokio::sync::Mutex as AsyncMutex;

use hyper_util::rt::TokioIo;
use tokio::net::UnixListener;
use tokio::net::UnixStream;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Channel, Endpoint};
use tonic::{Request, Response, Status};
use tower::service_fn;

pub mod onyx {
    tonic::include_proto!("onyx.v1");
}

use onyx::health_server::{Health, HealthServer};
use onyx::health_check_response::ServingStatus as StatusValue;
use onyx::privd_client::PrivdClient;
use onyx::storaged_server::{Storaged, StoragedServer};
use onyx::{HealthCheckRequest, HealthCheckResponse, ListPoolsRequest, ListPoolsResponse, Pool};
use registry::Registry;

const VERSION: &str = "0.1.0-dev";

/// Retries when connecting to onyx-privd at startup (dev.sh starts privd first,
/// but the socket can become writable a moment before privd accepts).
const PRIVD_CONNECT_ATTEMPTS: u32 = 10;
const PRIVD_CONNECT_DELAY: Duration = Duration::from_millis(200);

/// Re-scan the pool registry at most this often; keeps results fresh without
/// hammering btrfs on every list call.
const POOL_REFRESH_TTL: Duration = Duration::from_secs(60);

/// Minimal argument parsing so the binary behaves like the Go services
/// (--socket-dir / --state-dir). Prefer structured config in /etc/onyx later.
struct Args {
    socket_path: PathBuf,
    state_dir: PathBuf,
    privd_socket: PathBuf,
}

fn parse_args() -> Args {
    let mut socket_path = PathBuf::from("/run/onyx/onyx-storaged.sock");
    let mut state_dir = PathBuf::from("/var/lib/onyx/onyx-storaged");
    let mut privd_socket = PathBuf::from("/run/onyx/onyx-privd.sock");
    let mut it = std::env::args().skip(1);
    while let Some(arg) = it.next() {
        match arg.as_str() {
            "--socket-dir" => {
                let dir = PathBuf::from(it.next().expect("--socket-dir requires a value"));
                socket_path = dir.join("onyx-storaged.sock");
                privd_socket = dir.join("onyx-privd.sock");
            }
            "--state-dir" => {
                state_dir = PathBuf::from(it.next().expect("--state-dir requires a value"));
            }
            "--privd-socket" => {
                privd_socket = PathBuf::from(it.next().expect("--privd-socket requires a value"));
            }
            other => {
                eprintln!("onyx-storaged: unknown argument: {other}");
                std::process::exit(2);
            }
        }
    }
    Args { socket_path, state_dir, privd_socket }
}

fn main() -> ExitCode {
    // Structured logs to stderr (04 §8: journald at runtime; stderr in dev).
    tracing_subscriber::fmt().with_env_filter(tracing_subscriber::EnvFilter::from_default_env().add_directive("info".parse().unwrap())).init();

    let runtime = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(err) => {
            eprintln!("onyx-storaged: failed to start runtime: {err}");
            return ExitCode::FAILURE;
        }
    };
    let args = parse_args();
    match runtime.block_on(run(args)) {
        Ok(()) => ExitCode::SUCCESS,
        Err(err) => {
            eprintln!("onyx-storaged: {err}");
            ExitCode::FAILURE
        }
    }
}

async fn run(args: Args) -> Result<(), Box<dyn std::error::Error>> {
    let registry = Registry::open(&args.state_dir)?;

    // Dial onyx-privd over its unix socket (only privileged ops reach it).
    let channel = connect_privd(&args.privd_socket).await?;
    let privd_client = PrivdClient::new(channel);	let backend = RegistryBackend {
		registry,
		privd: AsyncMutex::new(privd_client),
		last_refresh: Mutex::new(Instant::now() - POOL_REFRESH_TTL),
	};

	// First refresh eagerly so we start with a warm registry. Failure is
	// logged, not fatal — list_pools retries on TTL.
	if let Err(e) = backend.refresh_locked().await {
		tracing::warn!(error = %e, "initial pool scan failed (will retry on next list)");
	}

	// Remove a stale socket from a previous run, then bind.
	if args.socket_path.exists() {
		std::fs::remove_file(&args.socket_path)?;
	}
	let listener = UnixListener::bind(&args.socket_path)?;
	tracing::info!(socket = %args.socket_path.display(), "onyx-storaged listening");

	let incoming = UnixListenerStream::new(listener);
	tonic::transport::Server::builder()
		.add_service(HealthServer::new(HealthService))
		.add_service(StoragedServer::new(backend))
		.serve_with_incoming(incoming)
		.await?;
	Ok(())
}

/// Build a tonic channel that connects to a unix socket.
async fn connect_privd(socket: &Path) -> Result<Channel, Box<dyn std::error::Error>> {    let socket_target = socket.to_path_buf();
    let mut last_err = None;
    for _ in 0..PRIVD_CONNECT_ATTEMPTS {
        let endpoint = Endpoint::try_from("http://[::]:50051")?;
        let socket = socket_target.clone();
        let chan = endpoint
            .connect_with_connector(service_fn(move |_| {
                let socket = socket.clone();
                async move {
                    Ok::<_, std::io::Error>(TokioIo::new(UnixStream::connect(socket).await?))
                }
            }))
            .await;
        match chan {
            Ok(c) => return Ok(c),
            Err(e) => last_err = Some(e),
        }
        tokio::time::sleep(PRIVD_CONNECT_DELAY).await;
    }
    Err(format!("could not connect to onyx-privd at {}: {:?}", socket.display(), last_err).into())
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

// --- Storaged ---

/// Backend served by the SQLite registry, refreshed from btrfs (via privd) on
/// a TTL so the UI/API always see live-but-cheap pool state.
struct RegistryBackend {
    registry: Arc<Registry>,	privd: AsyncMutex<PrivdClient<Channel>>,
	last_refresh: Mutex<Instant>,
}

impl RegistryBackend {
    /// Refresh if the TTL has expired. Errors surface on the RPC so clients
    /// see a structured failure instead of silently stale data.
    async fn refresh_locked(&self) -> Result<(), String> {
        let due = {
            let last = self.last_refresh.lock().unwrap();
            last.elapsed() >= POOL_REFRESH_TTL
        };
        if !due {
            return Ok(());
        }		let mut privd = self.privd.lock().await;
		let res = discovery::refresh_pools(&mut privd, &self.registry).await;
		*self.last_refresh.lock().unwrap() = Instant::now();
		res.map(|_| ())
    }
}

#[tonic::async_trait]
impl Storaged for RegistryBackend {
    async fn list_pools(
        &self,
        _request: Request<ListPoolsRequest>,
    ) -> Result<Response<ListPoolsResponse>, Status> {
        // Best-effort refresh; serve the registry either way.
        if let Err(e) = self.refresh_locked().await {
            tracing::warn!(error = %e, "pool refresh failed, serving cached registry");
        }
        let pools = self
            .registry
            .list_pools()
            .map_err(|e| Status::internal(format!("registry read failed: {e}")))?;
        Ok(Response::new(ListPoolsResponse { pools }))
    }

    async fn get_pool(
        &self,
        request: Request<onyx::GetPoolRequest>,
    ) -> Result<Response<Pool>, Status> {
        let name = request.into_inner().name;
        if let Err(e) = self.refresh_locked().await {
            tracing::warn!(error = %e, "pool refresh failed, serving cached registry");
        }
        let pools = self
            .registry
            .list_pools()
            .map_err(|e| Status::internal(format!("registry read failed: {e}")))?;
        match pools.into_iter().find(|p| p.name == name) {
            Some(pool) => Ok(Response::new(pool)),
            None => Err(Status::not_found(format!("pool '{name}' not found"))),
        }
    }
}