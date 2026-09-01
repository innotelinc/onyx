# Onyx — Online Storage System

**An open-source NAS operating system that feels like a modern app, not an enterprise
appliance.**

Onyx is a lightweight, privacy-first NAS OS for people who want a private file server on
hardware they already own — a Raspberry Pi, a mini-PC, an old laptop. It competes with
TrueNAS, OpenMediaVault, CasaOS, and ZimaOS on the home-to-small-office tier, combining:

- **Storage safety** (Btrfs snapshots, checksums, quotas, scrub) that TrueNAS-class systems
  are known for, at a fraction of the hardware cost,
- **App hosting** (Docker + a curated, sandboxed app store) at CasaOS/ZimaOS levels of ease,
- **A modern interface** (the *Prism* design system) inspired by Linear, GitHub, Arc,
  VisionOS, and Windows 11 — keyboard-first, calm, and dense.

**Key properties**

- ⚡ Boots in under a minute, idles under 512 MB RAM
- 🔒 Safe by default: snapshots on, atomic A/B updates with auto-rollback, no protocols
  exposed until you enable them
- 📁 SMB, NFS, FTP, SFTP, WebDAV, and Rsync from one logical share model
- 🧩 Modular: small, single-purpose services (Go + Rust) behind one typed API
- 📦 App store with signed, sandboxed containers
- 🤖 Optional AI-assisted administration, local-first (or BYO-key remote)
- 🏠 No cloud account required, no telemetry by default, AGPL-3.0 core

## Design documents

The full specification lives in [`docs/design/`](docs/design/README.md) — ten documents
covering product vision, architecture, the Prism design system, backend services, storage,
API, security, user management, the app ecosystem, and installer/update design.

## Status

**Phase: Design + v0.1/v0.2 scaffold.** The documents in `docs/design/` are the canonical
spec. The skeleton is landing: `onyx-core` (Go control plane), `onyx-api` (HTTP gateway),
`onyx-shared` (Go share manager), `onyx-storaged` + `onyx-privd` (Rust data plane), and the
`onyx` CLI, wired together via gRPC over unix sockets with `proto/` as the source of truth.

See the [roadmap](docs/design/01-product-vision.md#7-roadmap) for milestones after Cinder.

## Building and running (dev)

```bash
make bootstrap   # repo-local Go + protoc toolchain into .tools/ (no system installs)
make build       # compiles everything into bin/
scripts/dev.sh start   # unix sockets under .run/onyx, HTTP gateway on 127.0.0.1:8080
bin/onyx status        # CLI → API → core → shared → storaged → privd (all SERVING)
scripts/dev.sh stop
```

## What works today

```bash
bin/onyx pool list        # discover Btrfs pools (via onyx-privd)
bin/onyx pool show photos
bin/onyx device list      # every detected drive (USB, SATA, SSD — incl. hotplug removals)
bin/onyx device show usb-data   # incl. SMART health + temperature
bin/onyx device attach sda3   # mount a drive and expose it as a share
bin/onyx device detach usb-data
bin/onyx events          # device audit trail (attach/detach/health/error)
bin/onyx events --stream # tail live hotplug events as they happen
bin/onyx share create media /mnt/onyx/pool1/@data/media --smb --nfs --readonly
bin/onyx share list
bin/onyx share show media
bin/onyx share delete media
```

**Hotplug drives are handled automatically.** `onyx-storaged` listens on the
kernel's uevent netlink socket (the same feed udev uses — no daemon, no
polling) plus `lsblk` (through `onyx-privd`); the moment a drive appears it is
mounted under `/mnt/onyx/` and right away exposed as an SMB+NFS share —
`onyx share list` shows it live almost instantly, and unplugging detects the
removal, unmounts it, and removes the share. A slow periodic scan (5 s) is
kept as a safety net and is the only trigger where netlink is unavailable.
Removable devices (USB/SD) auto-attach by default; hotplugged internal drives
attach with `onyx device attach` (or run storaged with `--auto-attach=all`).
Call `scripts/hotplug-demo.sh` for a scripted plug/unplug lifecycle demo.

vFAT/exFAT USB sticks are mounted with `uid`/`gid`/`umask` (default 1000:100, umask 002) so the
share's users can actually write to them instead of root-owned files; configure with storaged's
`--mount-uid`, `--mount-gid`, `--fat-umask` (privd allowlists exactly those option tokens).

**Hotplug + health events are an API-visible audit stream.** Every attach, detach and SMART
health result is persisted (`/api/v1/events`, `onyx events`) and tailed live via
Server-Sent Events (`/api/v1/events/stream`, `onyx events --stream`). Drive health comes from
`smartctl -H -A` run inside `onyx-privd` (new allowlisted op): `onyx device show` reports the
health verdict and temperature, with periodic re-checks (`--device-health-interval-ms`).

**Generating shares actually writes real daemon config.** Every share mutation — creating or
`deleting a share, and the hotplug reconciler — runs the full docs/design/02 §6 pipeline:
`onyx-core` renders the complete `smb.conf` (global section + every SMB share) and
`exports` (unique fsids) via `onyx-shared`'s `RenderAll`, `onyx-privd` writes changed files
atomically (default `/etc/onyx/conf.d/`, overridable with privd's `--config-dir`), and
reloads the affected daemons — `testparm` validation first, then `systemctl reload smbd`;
NFS via `exportfs -ra`. It is change-guarded: an unchanged share set rewrites nothing, and a
failed reload always retries. Run the demo with fake `testparm`/`systemctl`/`exportfs`
bins to see the written files and reload log.

## Repo layout

```
base/          OSTree image definition (stub)
services/      onyx-api, onyx-core, onyx-shared (Go) · onyx-storaged, onyx-privd (Rust)
proto/         gRPC contracts (source of truth) + generated stubs
web/           Prism design system + React SPA (planned, v0.2)
sdk/           onyx-sdk Go client + onyx CLI (TS client planned)
installer/     ISO, SBC images, script installer (stub)
scripts/       bootstrap + local dev runner
.tools/        repo-local toolchain (git-ignored)
docs/          design docs, RFCs
```
