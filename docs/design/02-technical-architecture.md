# 02 — Technical Architecture

## 1. System overview

```
┌────────────────────────────────────────────────────────────────┐
│                        CLIENTS                                  │
│   Browser (SPA) · SMB/NFS/FTP/SFTP/WebDAV/Rsync clients · CLI   │
│   Mobile app · Backup peers · Remote admin (SSH, optional)      │
└───────────────────────────┬────────────────────────────────────┘
                            │ HTTPS (443) / protocol ports
┌───────────────────────────▼────────────────────────────────────┐
│                    CONTROL PLANE  (Go)                          │
│  ┌────────────┐  ┌───────────┐  ┌──────────┐  ┌──────────────┐  │
│  │ onyx-api   │  │ onyx-core │  │ onyx-appd│  │ onyx-ai      │  │
│  │ (gateway)  │──│ (orchestr)│──│ (docker) │  │ (assistant)  │  │
│  └─────┬──────┘  └─────┬─────┘  └──────────┘  └──────────────┘  │
│  ┌─────▼──────┐  ┌─────▼─────┐  ┌──────────┐  ┌──────────────┐  │
│  │ onyx-netd  │  │ onyx-bus  │  │ onyx-ai  │  │ onyx-updated │  │
│  └────────────┘  └───────────┘  └──────────┘  └──────────────┘  │
└───────────────────────────┬────────────────────────────────────┘
                            │ gRPC over unix sockets (local only)
┌───────────────────────────▼────────────────────────────────────┐
│                     DATA PLANE  (Rust)                          │
│  ┌────────────┐  ┌───────────┐  ┌──────────┐  ┌──────────────┐  │
│  │ onyx-storage│  │ onyx-snap │  │ onyx-    │  │ onyx-agent   │  │
│  │ (pools,fs) │  │ (snapshot)│  │ backupd  │  │ (SMART, HW)  │  │
│  └────────────┘  └───────────┘  └──────────┘  └──────────────┘  │
└───────────────────────────┬────────────────────────────────────┘
                            │ syscalls / mount / btrfs ioctl
┌───────────────────────────▼────────────────────────────────────┐
│                      KERNEL LAYER                               │
│  Btrfs / ext4 · Samba · NFS · vsftpd · sshd · rsync · nftables  │
│  Docker (rootless-ish, AppArmor, seccomp, user namespaces)      │
│  Linux 6.x (Debian Trixie userspace, OSTree root)               │
└─────────────────────────────────────────────────────────────────┘
```

**Two planes, one rule:** the *data plane* touches the filesystem and network protocols and
never talks to the outside world directly (no HTTP listener). The *control plane* handles
users, HTTP, orchestration, and apps, and reaches the filesystem **only through** the data
plane's gRPC interface. This keeps the attack surface of the privileged code minimal.

## 2. Stack decisions

| Layer | Choice | Why |
|-------|--------|-----|
| Base distro | Debian Trixie (minimal, no GUI) | Predictable, enormous package surface, 5-year support; keeps OEM/community ports cheap |
| Root filesystem | OSTree-managed read-only A/B | Atomic updates, instant rollback, tamper-resistant system state |
| User filesystem | Btrfs (default), ext4 (opt-in) | Snapshots/checksums/compression/quotas at ~1/10th of ZFS RAM cost; see §05 |
| Data-plane language | Rust | Memory safety where filesystem code runs as root; zero-cost abstractions keep RAM low |
| Control-plane language | Go | Fast compile, goroutines for concurrent jobs, single static binaries, large stdlib |
| Frontend | TypeScript + React 19 + Vite | Developer velocity; SPA served by the gateway (no Node at runtime) |
| Apps | Docker + Compose | De-facto standard; images are portable across NAS OSes |
| System database | SQLite per service (WAL mode) | Zero-admin, transactional, fsync-safe; no daemon to babysit |
| IPC | gRPC (unix sockets) + `onyx-bus` (WS pub/sub) | Typed contracts between services; simple event fan-out for UI + apps |
| CLI | Go binary, cobra-style subcommands | Scriptable administration and debugging |
| Auth | Local + TOTP/WebAuthn; OIDC/LDAP/AD optional | Simple by default, interoperable when required |

## 3. Module map (repo layout)

```
onyx/
├── base/                  # OSTree image definition, kernel config, packages
├── services/
│   ├── api/               # onyx-api       — HTTP gateway (Go)
│   ├── core/              # onyx-core      — orchestrator, policy, audit (Go)
│   ├── storaged/          # onyx-storaged  — pools, datasets, scrub (Rust)
│   ├── snapd/             # onyx-snapd     — snapshots/rollback (Rust)
│   ├── backupd/           # onyx-backupd   — backup jobs (Go)
│   ├── shared/            # onyx-shared    — protocol config/supervision (Go)
│   ├── appd/              # onyx-appd      — Docker app lifecycle (Go)
│   ├── netd/              # onyx-netd      — networking, firewall, VPN (Go)
│   ├── agent/             # onyx-agent     — SMART, temps, UPS, health (Rust)
│   ├── updated/           # onyx-updated   — OSTree commits, rollback (Rust)
│   ├── ai/                # onyx-ai        — assistant gateway (Go)
│   ├── bus/               # onyx-bus       — event bus (Go)
│   └── privd/             # onyx-privd     — privilege helper (Rust)
├── proto/                 # gRPC + OpenAPI contracts (source of truth)
├── web/                   # React SPA ("Prism" design system)
├── sdk/                   # onyx-sdk (Go + TS clients, app SDK)
├── installer/             # ISO, SBC images, script installer
└── docs/                  # design docs, RFCs, ops runbooks
```

## 4. Service-to-service communication

- **Synchronous:** gRPC over unix sockets in `/run/onyx/`. Services authenticate each other via
  peer credentials (`SO_PEERCRED`) plus a per-service token read from a root-only file.
- **Asynchronous:** `onyx-bus` — a lightweight publish/subscribe relay (WS + gRPC stream) on a
  unix socket. Events: `fs.change`, `snap.created`, `disk.failure`, `app.stopped`,
  `user.login`, `update.available`. The UI subscribes over WebSocket; apps can subscribe via
  the SDK.
- **Never over TCP loopback by default** — unix sockets only, so a compromised container cannot
  scan the control plane.

## 5. Privilege model

- Every service runs as its own unprivileged system user (`onyx-storage`, `onyx-snap`, …).
- Privileged operations (mount, `btrfs` ioctls, nftables, smartctl) go through
  **`onyx-privd`** — a small Rust helper with a allowlisted, parsed-and-validated command
  grammar, invoked via systemd socket activation. It never shells out; arguments are
  structured, and paths must resolve inside allowed roots.
- The web UI and API never run as root. File *access* is delegated to the protocol daemons
  (Samba, NFS, sshd, vsftpd) which enforce POSIX ACLs and share configs.

## 6. Data flow — a request end to end

1. Browser calls `POST /api/v1/shares` with a JSON body.
2. `onyx-api` authenticates the session, rate-limits, and forwards the validated command to
   `onyx-core` (gRPC).
3. `onyx-core` applies policy (permissions, quota checks, audit log) and calls
   `onyx-shared` to generate the Samba/NFS config.
4. `onyx-shared` writes config to `/etc/onyx/conf.d/`, then asks `onyx-privd` to
   `systemctl reload smbd`; `onyx-privd` validates the config with `testparm` first.
5. `onyx-bus` publishes `share.created`; the UI updates in real time.

## 7. State and configuration

- **Config:** TOML under `/etc/onyx/`, one file per service, written atomically (write →
  fsync → rename) and only via the owning service. No hand-editing at runtime; the UI/API is
  the interface.
- **System DB:** SQLite databases under `/var/lib/onyx/<service>/`. WAL mode, `synchronous=NORMAL`
  for metadata, full fsync for audit/backup records. Backup of the DB set is part of every
  snapshot job.
- **Secrets:** never in config files or the DB in plaintext. Stored in a keyring owned by
  root (`/etc/onyx/keys/`, mode 0700), encrypted with a key derived from the machine identity
  + install passphrase; exposed to services only via `onyx-privd`'s secret API.
- **Runtime state:** `/run/onyx/` (sockets, PID files), `/var/lib/onyx/apps/` (app data).

## 8. Deployment topologies

| Topology | Description |
|----------|-------------|
| **Single box (default)** | Everything on one machine: system disk (or SD) + one or more data disks |
| **Two-disk safe** | Btrfs RAID1 data; system on separate small SSD/SD |
| **Data-disk-only SBC** | RPi 5 / RK3588 board with 1–2 USB/NVMe drives; web UI + protocols on the same box |
| **Headless / CLI-only** | API + CLI; UI optional (`onyx-ui disable`) for kiosk/embedded uses |
| **App-heavy** | Everything plus 10–30 containers; pool sizing guidelines in §05 |

## 9. Performance budgets (enforced in CI)

| Metric | Budget |
|--------|--------|
| Idle RAM, base install (RPi 4, 4 GB) | ≤ 512 MB RSS |
| Idle RAM, +5 typical apps | ≤ 1.5 GB |
| Boot to login screen | ≤ 60 s |
| API p95 latency (local, cached) | ≤ 50 ms |
| UI first contentful paint (cold) | ≤ 1.5 s on RPi 4 |
| Snapshot of 100 GB | ≤ 5 s (Btrfs reflink copy) |
| SMB throughput (GigE, RAID1, 4K mixed) | ≥ 85% of `dd` baseline |

Every release runs a hardware-lab benchmark matrix; regressions block release.

## 10. Failure and resilience model

- **Updates:** A/B OSTree roots; boot into the new root; health check; auto-rollback on failed
  boot or post-boot watchdog (see §10).
- **Storage:** scrub detects bit-rot → alert + (if redundant) self-heal; snapshot schedule
  keeps a rolling recovery window.
- **Control plane:** all services restart-on-failure via systemd with exponential backoff;
  `onyx-core` holds the intent (jobs, shares, apps) in SQLite, so a crashed worker is
  reconciled on restart.
- **Power loss:** journaled fs + WAL; snapshot creation is atomic; `sync` before reboot.
- **No single point of failure in config:** if `/etc/onyx` is lost, the OS rebuilds defaults
  and the data pool re-imports without data loss (metadata lives on the pool).
