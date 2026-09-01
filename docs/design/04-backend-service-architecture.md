# 04 — Backend Service Architecture

## 1. Service inventory

All services are small, single-purpose daemons. Each exposes a gRPC service (contracts in
`proto/`), owns one SQLite database, and runs as its own unprivileged system user.

| Service | Lang | Responsibility | Privileged ops |
|---------|------|----------------|----------------|
| `onyx-api` | Go | HTTP/2 gateway: REST + WebSocket, sessions, rate limiting, static UI | none |
| `onyx-core` | Go | Orchestrator: policy, job scheduler, audit log, service registry, reconciliation | none |
| `onyx-storaged` | Rust | Pools, datasets, quotas, scrub, disk import/export, filesystem stats | via `onyx-privd` |
| `onyx-snapd` | Rust | Snapshot create/list/delete/rollback, retention policies, browse/restore mounts | via `onyx-privd` |
| `onyx-backupd` | Go | Backup jobs (local, rsync/SSH, S3), schedules, restore, encryption (age/rclone-style) | none |
| `onyx-shared` | Go | Share manager: SMB/NFS/FTP/SFTP/WebDAV/Rsync config generation + reload | via `onyx-privd` |
| `onyx-appd` | Go | Docker Compose lifecycle, image pull, app store index, health checks, logs | Docker socket (rootless) |
| `onyx-netd` | Go | NIC config, DHCP/static, firewall (nftables), port forwards, WireGuard VPN | via `onyx-privd` |
| `onyx-agent` | Rust | SMART, temps, fans, UPS (NUT), disk health, power actions | via `onyx-privd` |
| `onyx-updated` | Rust | OSTree commit fetch/apply, A/B boot mgmt, rollback, update channels | via `onyx-privd` |
| `onyx-ai` | Go | AI assistant gateway: tool-calling over the same APIs the UI uses, local or remote LLM | none |
| `onyx-bus` | Go | Pub/sub relay for events (WS + gRPC stream over unix socket) | none |
| `onyx-privd` | Rust | Privilege helper: allowlisted structured commands executed with elevated rights | **root** (smallest possible surface) |

## 2. Why Go + Rust

- **Go** for the control plane: fast development, goroutines map naturally to concurrent jobs
  (backups, app pulls), single static binaries, excellent HTTP/gRPC support. Memory is
  acceptable because control-plane services are small and event-driven.
- **Rust** for the data plane: filesystem metadata parsing, ioctl marshaling, and protocol
  code are exactly where memory unsafety has historically caused NAS CVEs. Rust gives safety
  with no GC and no runtime, keeping idle RSS tiny (`onyx-storaged` targets < 30 MB).

## 3. Contracts and codegen

- `proto/*.proto` (gRPC) and `proto/openapi.yaml` (HTTP) are the **single source of truth**.
- All service-to-service calls use generated clients. A CI check fails if implementation
  strays from the contract.
- `onyx-sdk` exposes the same contracts as typed Go and TypeScript clients, so apps and
  scripts talk to Onyx exactly like the UI does.

## 4. Config and state layout

```
/etc/onyx/                 # generated config, root-owned, 0750
├── onyx-api.toml          # one file per service
├── onyx-core.toml
├── conf.d/                # generated daemon configs (smb.conf, exports, vsftpd, webdav)
└── keys/                  # encrypted keyring (0700, root)
/var/lib/onyx/             # service state
├── <service>/*.sqlite     # WAL-mode SQLite per service
├── apps/                  # app data dirs (bind mounts into containers)
└── snapshots/             # snapshot store mountpoint (managed by snapd)
/run/onyx/                 # unix sockets, pidfiles (tmpfs)
```

Rules: config is written only by the owning service; the UI/API/CLI never write files
directly. Every write is `write → fsync → rename` so a crash never leaves half-written
config. Config changes are versioned so updates can migrate them.

## 5. Inter-service communication

- **Request/response:** gRPC on unix sockets. Peer verification via `SO_PEERCRED` (uid must
  match the expected service user) plus a per-service bearer token in a root-only file.
- **Events:** `onyx-bus` pub/sub. Topics: `fs.change`, `snap.created`, `snap.deleted`,
  `backup.progress`, `backup.finished`, `disk.smart`, `disk.failure`, `app.installed`,
  `app.stopped`, `user.login`, `user.failed_login`, `update.available`, `net.link`. Events are
  at-least-once; consumers are idempotent. UI receives them over a single WebSocket.
- **No TCP loopback** for inter-service traffic by default — a compromised container or app
  process cannot port-scan the control plane.

## 6. Jobs and reconciliation

- Long-running work (backups, scrub, app installs, snapshot retention) is modeled as
  **jobs** with a persisted state machine in `onyx-core`'s SQLite (`job_id, type, state,
  progress, params, result`).
- Workers pull from a queue; on restart, interrupted jobs resume or fail cleanly. The UI
  renders jobs from the Activity rail (§03.3), and all job events stream over the bus.
- `onyx-core` periodically reconciles: desired state (shares, apps, schedules) in its DB vs.
  actual state reported by services; drift is corrected automatically and logged.

## 7. Privilege helper (`onyx-privd`)

The single root process. Design rules:

- **No shell, ever.** It parses structured commands (`PrivReq { op, args }`) against an
  allowlist: `mount`, `btrfs` (subset of subcommands), `nft`, `smartctl`, `systemctl` (subset),
  `setfacl`, keyring ops.
- Each op has a validator: e.g. mount points must resolve under `/mnt/onyx/` and datasets must
  match `^[a-z0-9_.-]+$`; paths are canonicalized and must stay inside allowed roots.
- Runs under systemd socket activation; every request is logged to the audit trail with
  caller identity.
- If `onyx-privd` is compromised, the blast radius is contained to the allowlist — it cannot
  execute arbitrary commands.

**Implemented (v0.1):** `services/privd` with `btrfs filesystem show --raw` and
`btrfs filesystem usage -b` allowed (contract in `proto/onyx/v1/privd.proto`); execution is
via explicit `argv` with a hard timeout, never a shell, and mount paths are canonicalized
and required to stay under `/mnt/onyx`. Peer-identity audit (SO_PEERCRED) and the remaining
ops (`nft`, `smartctl`, `systemctl`, …) arrive with the milestones that need them.

## 8. Observability

- **Logs:** structured JSON to `journald`, tagged `service=…, request_id=…`. The UI exposes a
  unified log viewer (CodeBlock with ANSI + filter).
- **Metrics:** Prometheus text format on a unix socket (`onyx-agent` scrapes); dashboard
  sparklines use the same source.
- **Audit:** `onyx-core` records security-relevant events (logins, permission changes,
  share changes, rollbacks, key access) to an append-only, checksum-chained audit log
  (`/var/lib/onyx/audit/`), readable by admins and exportable for SIEM ingestion.
- **Health:** every service exposes a gRPC `Health` RPC; `onyx-agent` aggregates into the
  status pill and the Health card.

## 9. Lifecycle and packaging

- Each service is a systemd unit: `onyx-<name>.service`, `Restart=on-failure`,
  `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`,
  `PrivateTmp=yes`, `MemoryDenyWriteExecute=yes` (Go services: `SystemCallFilter` tightened
  per service).
- Binaries ship in the read-only OSTree root at `/usr/lib/onyx/`; config/state live in
  `/etc`/`/var` (writable). Updates replace binaries atomically without touching data.
- Versioning: semantic, with a `onyx version` CLI command and a `GET /api/v1/system/version`.

## 10. CLI

`onyx` mirrors the API: `onyx share|snap|backup|app|user|pool|net|update|status|logs|audit`.
Designed for scripting: `--json` everywhere, non-zero exit codes with structured errors.
The CLI authenticates via a machine token (`onyx login --token`), never storing passwords.
