# Onyx services

Small, single-purpose daemons (docs/design/04#1-service-inventory). Each service:

- runs as its own unprivileged system user,
- owns one SQLite database (WAL) under `/var/lib/onyx/<service>/`,
- exposes a gRPC service generated from [`proto/`](../proto/) (the source of truth),
- listens only on a unix socket in `/run/onyx/` — no TCP for inter-service traffic,
- ships as a systemd unit `onyx-<name>.service` (packaging lands with the installer).

## Implemented (v0.1 skeleton)

| Service | Lang | Location | What it does today |
|---------|------|----------|--------------------|
| `onyx-core` | Go | [`core/`](core/) | gRPC `Health` + `Core` + `CoreShares` (SystemStatus, pool forwarding, share CRUD in SQLite); SQLite state dir init |
| `onyx-api`  | Go | [`api/`](api/) | HTTP gateway: `/api/v1/system/*`, `/api/v1/pools`, `/api/v1/shares`, `/healthz`; error envelope per docs/design/06 |
| `onyx-shared` | Go | [`shared/`](shared/) | Share manager: renders per-protocol daemon config (smb.conf fragments, NFS exports) from the logical share model |
| `onyx-storaged` | Rust | [`storaged/`](storaged/) | gRPC `Health` + `Storaged`; real Btrfs pool discovery via `onyx-privd`, cached in a SQLite registry (TTL refresh) |
| `onyx-privd` | Rust | [`privd/`](privd/) | Root privilege helper: allowlisted `btrfs` ops (`show --raw`, `usage -b`) with per-op validation, no-shell exec, timeout |

## Planned (roadmap)

| Service | Lang | Lands in |
|---------|------|----------|
| `onyx-snapd` | Rust | v0.3 |
| `onyx-backupd` | Go | v0.3 |
| `onyx-shared` | Go | v0.2 |
| `onyx-appd` | Go | v0.4 |
| `onyx-netd` | Go | v0.4 |
| `onyx-agent` | Rust | v0.3 |
| `onyx-updated` | Rust | v0.2 |
| `onyx-ai` | Go | v0.5 |
| `onyx-bus` | Go | v0.2 |
| `onyx-privd` | Rust | v0.2 (first privileged op) |

## Local development

```bash
make build        # everything into bin/
scripts/dev.sh start   # unix sockets under .run/onyx, HTTP gateway on 127.0.0.1:8080
bin/onyx status   # CLI talks to the gateway
```