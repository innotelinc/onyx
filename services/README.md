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
| `onyx-core` | Go | [`core/`](core/) | gRPC `Health` + `Core` + `CoreShares` (SystemStatus, pool/device forwarding, share CRUD in SQLite) + hotplug reconciler that auto-creates/removes shares for mounted drives; **config applier** — renders the full share set via onyx-shared and writes/reloads the daemon config through onyx-privd (change-guarded); SQLite state dir init |
| `onyx-api`  | Go | [`api/`](api/) | HTTP gateway: `/api/v1/system/*`, `/api/v1/pools`, `/api/v1/shares`, `/api/v1/devices*`, `/healthz`; error envelope per docs/design/06 |
| `onyx-shared` | Go | [`shared/`](shared/) | Share manager: renders per-protocol daemon config (smb.conf fragments, NFS exports) from the logical share model; `RenderAll` produces the complete smb.conf + exports files (deterministic, unique fsids) |
| `onyx-storaged` | Rust | [`storaged/`](storaged/) | gRPC `Health` + `Storaged`; Btrfs pool discovery via `onyx-privd`, cached in a SQLite registry (TTL refresh); **hotplug watcher** — kernel uevent (netlink) driven, scans `lsblk`, auto-mounts removable drives under `/mnt/onyx/` via privd, unmounts on detach; slow periodic scan as fallback; SMART health sweep + persistent audit trail (ListEvents/WatchDevices) |
| `onyx-privd` | Rust | [`privd/`](privd/) | Root privilege helper: allowlisted ops with per-op validation, no-shell exec, timeout — `btrfs` (`show --raw`, `usage -b`), block device ops (`lsblk` scan, `mount`/`umount` with allowlisted uid/gid/umask options), `smartctl -H -A` health probe, **atomic daemon-config write** (`WRITE_DAEMON_CONFIG`: smb.conf/exports under `--config-dir`) + **validated reloads** (`RELOAD_DAEMONS`: `testparm` → `systemctl reload smbd`, `exportfs -ra`) |

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

## Running as a system

Each service has a systemd unit in [`deploy/systemd/`](../deploy/systemd/) plus
a tmpfiles snippet for the runtime paths; the units chain privd →
storaged/shared → core → api and are installed by
[`scripts/onyx-install`](../scripts/onyx-install) (or `make install`).

## Local development

```bash
make build        # everything into bin/
scripts/dev.sh start   # unix sockets under .run/onyx, HTTP gateway on 127.0.0.1:8080
bin/onyx status   # CLI talks to the gateway
```