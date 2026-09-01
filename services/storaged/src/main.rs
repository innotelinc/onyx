//! onyx-storaged — the data plane for pools, datasets, quotas and scrub
//! (docs/design/05-storage-subsystem.md).
//!
//! Rules this skeleton upholds from the start (docs/design/02#1):
//! * no network listeners — only a unix socket in `/run/onyx`;
//! * privileged operations (mount, btrfs ioctls) go through onyx-privd, never
//!   directly — pool enumeration runs `btrfs filesystem show` inside privd and
//!   the hotplug watcher mounts devices through privd's allowlisted ops;
//! * owns one SQLite DB (pool + device registries) under
//!   `/var/lib/onyx/onyx-storaged/`.

mod devices;
mod discovery;
mod registry;
mod uevent;

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use tokio::sync::{broadcast, mpsc, Mutex as AsyncMutex};

use hyper_util::rt::TokioIo;
use tokio::net::UnixListener;
use tokio::net::UnixStream;
use tokio_stream::wrappers::{ReceiverStream, UnixListenerStream};
use tonic::transport::{Channel, Endpoint};
use tonic::{Request, Response, Status};
use tower::service_fn;

pub mod onyx {
    tonic::include_proto!("onyx.v1");
}

use devices::DeviceManager;
use uevent::UeventMonitor;
use onyx::health_server::{Health, HealthServer};
use onyx::health_check_response::ServingStatus as StatusValue;
use onyx::privd_client::PrivdClient;
use onyx::storaged_server::{Storaged, StoragedServer};
use onyx::{
    Device, DeviceEvent, GetDeviceRequest, GetPoolRequest, HealthCheckRequest, HealthCheckResponse,
    ListDevicesRequest, ListDevicesResponse, ListEventsRequest, ListEventsResponse,
    ListPoolsRequest, ListPoolsResponse, MountDeviceRequest, Pool, UnmountDeviceRequest,
    WatchDevicesRequest,
};
use registry::Registry;

const VERSION: &str = "0.1.0-dev";

/// Retries when connecting to onyx-privd at startup (dev.sh starts privd first,
/// but the socket can become writable a moment before privd accepts).
const PRIVD_CONNECT_ATTEMPTS: u32 = 10;
const PRIVD_CONNECT_DELAY: Duration = Duration::from_millis(200);

/// Re-scan the pool registry at most this often; keeps results fresh without
/// hammering btrfs on every list call.
const POOL_REFRESH_TTL: Duration = Duration::from_secs(60);

/// Kernel view of block devices (attach/detach detection).
const SYSFS_BLOCK: &str = "/sys/class/block";

/// Forget detached device records after this long (minutes).
const DEFAULT_DETACHED_TTL_MINUTES: i64 = 10;

/// Periodic rescan cadence when no uevent arrived first. Block uevents make
/// hotplug reaction instant; this slow scan is the safety net for missed or
/// kernel-complex events (and for environments without netlink).
const DEFAULT_DEVICE_WATCH_INTERVAL: Duration = Duration::from_secs(5);

/// Minimum gap between event-triggered scans: one USB plug bursts dozens of
/// uevents; scanning more often than this is pure overhead.
const EVENT_SCAN_MIN_INTERVAL: Duration = Duration::from_millis(250);

/// How often the SMART health sweep revisits every attached drive.
const DEFAULT_HEALTH_INTERVAL: Duration = Duration::from_secs(300);

/// Broadcast buffer for the live device event stream (WatchDevices).
const EVENT_BROADCAST_CAPACITY: usize = 512;

/// Minimal argument parsing so the binary behaves like the Go services
/// (--socket-dir / --state-dir). Prefer structured config in /etc/onyx later.
struct Args {
    socket_path: PathBuf,
    state_dir: PathBuf,
    privd_socket: PathBuf,
    mount_root: PathBuf,
    sysfs_root: PathBuf,
    auto_attach: String,
    watch_interval: Duration,
    detached_ttl_minutes: i64,
    mount_uid: u32,
    mount_gid: u32,
    fat_umask: u32,
    health_interval: Duration,
}

fn parse_args() -> Args {
    let mut socket_path = PathBuf::from("/run/onyx/onyx-storaged.sock");
    let mut state_dir = PathBuf::from("/var/lib/onyx/onyx-storaged");
    let mut privd_socket = PathBuf::from("/run/onyx/onyx-privd.sock");
    let mut mount_root = PathBuf::from("/mnt/onyx");
    let mut sysfs_root = PathBuf::from(SYSFS_BLOCK);
    let mut auto_attach = "removable".to_string();
    let mut watch_interval = DEFAULT_DEVICE_WATCH_INTERVAL;
    let mut detached_ttl_minutes = DEFAULT_DETACHED_TTL_MINUTES;
    let mut mount_uid: u32 = 1000;
    let mut mount_gid: u32 = 100;
    let mut fat_umask: u32 = 0o002;
    let mut health_interval = DEFAULT_HEALTH_INTERVAL;
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
            "--mount-root" => {
                mount_root = PathBuf::from(it.next().expect("--mount-root requires a value"));
            }
            "--sysfs-root" => {
                sysfs_root = PathBuf::from(it.next().expect("--sysfs-root requires a value"));
            }
            "--auto-attach" => {
                auto_attach = it.next().expect("--auto-attach requires a value");
                match auto_attach.as_str() {
                    "removable" | "all" | "none" => {}
                    other => {
                        eprintln!("onyx-storaged: --auto-attach must be removable|all|none, got {other}");
                        std::process::exit(2);
                    }
                }
            }
            "--device-watch-interval-ms" => {
                let ms: u64 = it
                    .next()
                    .expect("--device-watch-interval-ms requires a value")
                    .parse()
                    .expect("--device-watch-interval-ms must be a number");
                watch_interval = Duration::from_millis(ms);
            }
            "--device-detached-ttl-minutes" => {
                detached_ttl_minutes = it
                    .next()
                    .expect("--device-detached-ttl-minutes requires a value")
                    .parse()
                    .expect("--device-detached-ttl-minutes must be a number");
            }
            "--mount-uid" => {
                mount_uid = it
                    .next()
                    .expect("--mount-uid requires a value")
                    .parse()
                    .expect("--mount-uid must be a number");
            }
            "--mount-gid" => {
                mount_gid = it
                    .next()
                    .expect("--mount-gid requires a value")
                    .parse()
                    .expect("--mount-gid must be a number");
            }
            "--fat-umask" => {
                let v = it.next().expect("--fat-umask requires a value");
                fat_umask = u32::from_str_radix(&v, 8)
                    .unwrap_or_else(|_| panic!("--fat-umask must be octal (e.g. 002): {v}"));
            }
            "--device-health-interval-ms" => {
                let ms: u64 = it
                    .next()
                    .expect("--device-health-interval-ms requires a value")
                    .parse()
                    .expect("--device-health-interval-ms must be a number");
                health_interval = Duration::from_millis(ms);
            }
            other => {
                eprintln!("onyx-storaged: unknown argument: {other}");
                std::process::exit(2);
            }
        }
    }
    Args {
        socket_path,
        state_dir,
        privd_socket,
        mount_root,
        sysfs_root,
        auto_attach,
        watch_interval,
        detached_ttl_minutes,
        mount_uid,
        mount_gid,
        fat_umask,
        health_interval,
    }
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
    let privd = Arc::new(AsyncMutex::new(PrivdClient::new(channel)));

    let (events_tx, _) = broadcast::channel::<DeviceEvent>(EVENT_BROADCAST_CAPACITY);
    let manager = Arc::new(DeviceManager::new(
        registry.clone(),
        privd.clone(),
        args.mount_root.clone(),
        &args.auto_attach,
        args.detached_ttl_minutes,
        args.mount_uid,
        args.mount_gid,
        args.fat_umask,
        events_tx,
    ));
    let sysfs_root = args.sysfs_root.clone();
    let health_interval = args.health_interval;

    let backend = RegistryBackend {
        registry,
        privd,
        manager: manager.clone(),
        last_refresh: Mutex::new(Instant::now() - POOL_REFRESH_TTL),
    };

    // First refresh eagerly so we start with a warm registry. Failure is
    // logged, not fatal — list_pools retries on TTL.
    if let Err(e) = backend.refresh_locked().await {
        tracing::warn!(error = %e, "initial pool scan failed (will retry on next list)");
    }

    // Kernel-uevent monitor: block add/remove/change wakes the watcher
    // instantly (no polling on real hardware). Some containers block netlink
    // — then the periodic scan below is the only trigger.
    let monitor = match UeventMonitor::open() {
        Ok(m) => {
            tracing::info!("udev/netlink uevent monitor active: instant hotplug detection");
            Some(m)
        }
        Err(e) => {
            tracing::warn!(error = %e, "netlink uevent monitor unavailable; periodic scans only");
            None
        }
    };

    // Background hotplug watcher: reacts to uevents instantly, auto-mounts
    // eligible removable devices and reacts to removals — all through
    // onyx-privd, with a slow periodic scan as the safety net.
    tokio::spawn(watch_loop(
        manager,
        args.watch_interval,
        sysfs_root,
        monitor,
        health_interval,
    ));

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

/// The hotplug watcher. Primary trigger: kernel uevents (add/remove/change of
/// block devices) — one uevent wakes a full reconcile scan immediately. The
/// periodic scan is the safety net for anything uevents miss (or for hosts
/// where netlink is blocked). All scanning runs through onyx-privd and the
/// registry, so a wedged lsblk can never break polling.
async fn watch_loop(
    manager: Arc<DeviceManager>,
    interval: Duration,
    sysfs_root: PathBuf,
    monitor: Option<UeventMonitor>,
    health_interval: Duration,
) {
    let mut prev: HashSet<String> = HashSet::new();
    let mut last_scan = Instant::now();
    loop {
        let sleep = tokio::time::sleep(interval);
        tokio::pin!(sleep);
        let health = tokio::time::sleep(health_interval);
        tokio::pin!(health);
        let uevent_wait = wait_uevent(monitor.as_ref());
        tokio::pin!(uevent_wait);
        tokio::select! {
            _ = &mut sleep => {
                prev = manager.tick(&prev, &sysfs_root).await;
                last_scan = Instant::now();
            }
            _ = uevent_wait => {
                // A block uevent just landed: react now, but never faster
                // than the throttle (a single plug fires many uevents).
                if last_scan.elapsed() >= EVENT_SCAN_MIN_INTERVAL {
                    prev = manager.tick(&prev, &sysfs_root).await;
                    last_scan = Instant::now();
                }
            }
            _ = &mut health => {
                // Periodic SMART sweep so health stays fresh on idle hosts.
                manager.health_sweep().await;
            }
        }
    }
}

/// Resolves to () when the monitor delivers a block uevent; never resolves
/// when there is no monitor (so the periodic scan stays the only trigger).
async fn wait_uevent(monitor: Option<&UeventMonitor>) {
    match monitor {
        Some(m) => {
            if let Some(ev) = m.next().await {
                tracing::debug!(action = %ev.action, kname = %ev.kname, devtype = %ev.devtype, "block uevent");
            }
        }
        None => std::future::pending().await,
    }
}

/// Build a tonic channel that connects to a unix socket.
async fn connect_privd(socket: &Path) -> Result<Channel, Box<dyn std::error::Error>> {
    let socket_target = socket.to_path_buf();
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

/// Backend served by the SQLite registries, refreshed from btrfs (via privd)
/// on a TTL and from lsblk (via privd) continuously, so the UI/API always see
/// live-but-cheap state.
struct RegistryBackend {
    registry: Arc<Registry>,
    privd: Arc<AsyncMutex<PrivdClient<Channel>>>,
    manager: Arc<DeviceManager>,
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
        }
        let mut privd = self.privd.lock().await;
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
        request: Request<GetPoolRequest>,
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

    async fn list_devices(
        &self,
        _request: Request<ListDevicesRequest>,
    ) -> Result<Response<ListDevicesResponse>, Status> {
        let devices = self
            .registry
            .list_devices()
            .map_err(|e| Status::internal(format!("registry read failed: {e}")))?;
        Ok(Response::new(ListDevicesResponse { devices }))
    }

    async fn get_device(
        &self,
        request: Request<GetDeviceRequest>,
    ) -> Result<Response<Device>, Status> {
        let name = request.into_inner().name;
        self.manager
            .registry
            .get_device(&name)
            .map_err(|e| Status::internal(format!("registry read failed: {e}")))?
            .map(Response::new)
            .ok_or_else(|| Status::not_found(format!("device '{name}' not found")))
    }

    async fn mount_device(
        &self,
        request: Request<MountDeviceRequest>,
    ) -> Result<Response<Device>, Status> {
        let name = request.into_inner().name;
        self.manager
            .attach(&name)
            .await
            .map(Response::new)
            .map_err(|e| {
                if e.contains("not found") {
                    Status::not_found(e)
                } else {
                    Status::failed_precondition(e)
                }
            })
    }

    async fn unmount_device(
        &self,
        request: Request<UnmountDeviceRequest>,
    ) -> Result<Response<Device>, Status> {
        let name = request.into_inner().name;
        self.manager
            .detach(&name)
            .await
            .map(Response::new)
            .map_err(|e| {
                if e.contains("not found") {
                    Status::not_found(e)
                } else {
                    Status::failed_precondition(e)
                }
            })
    }

    async fn list_events(
        &self,
        request: Request<ListEventsRequest>,
    ) -> Result<Response<ListEventsResponse>, Status> {
        let req = request.into_inner();
        let events = self
            .registry
            .list_events(req.limit, req.after_id, &req.kname)
            .map_err(|e| Status::internal(format!("event registry read failed: {e}")))?;
        Ok(Response::new(ListEventsResponse { events }))
    }

    type WatchDevicesStream =
        Pin<Box<dyn tokio_stream::Stream<Item = Result<DeviceEvent, Status>> + Send>>;

    async fn watch_devices(
        &self,
        _request: Request<WatchDevicesRequest>,
    ) -> Result<Response<Self::WatchDevicesStream>, Status> {
        // One subscriber task per client; the broadcast covers every event
        // emitted by the DeviceManager (attach/detach/health/error). A small
        // bounded mpsc protects slow clients from unbounded buffering.
        let (tx, rx) = mpsc::channel::<Result<DeviceEvent, Status>>(128);
        let mut sub = self.manager.events.subscribe();
        tokio::spawn(async move {
            loop {
                match sub.recv().await {
                    Ok(ev) => {
                        if tx.send(Ok(ev)).await.is_err() {
                            break; // client closed the stream
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(_)) => continue,
                    Err(broadcast::error::RecvError::Closed) => break,
                }
            }
        });
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }
}