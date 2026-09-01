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

**Phase: Design + v0.1 "Cinder" scaffold.** The documents in `docs/design/` are the canonical
spec. The first code — the v0.1 skeleton — is landing: `onyx-core` (Go control plane),
`onyx-api` (HTTP gateway), `onyx-storaged` (Rust data plane), and the `onyx` CLI, wired
together via gRPC over unix sockets with `proto/` as the source of truth.

See the [roadmap](docs/design/01-product-vision.md#7-roadmap) for milestones after Cinder.

## Building and running (dev)

```bash
make bootstrap   # repo-local Go + protoc toolchain into .tools/ (no system installs)
make build       # compiles everything into bin/
scripts/dev.sh start   # sockets under .run/onyx, HTTP gateway on 127.0.0.1:8080
bin/onyx status        # CLI → API → core → storaged (SERVING)
scripts/dev.sh stop
```

## Repo layout

```
base/          OSTree image definition (stub)
services/      onyx-api, onyx-core (Go) · onyx-storaged (Rust) · more on the roadmap
proto/         gRPC contracts (source of truth) + generated stubs
web/           Prism design system + React SPA (planned, v0.2)
sdk/           onyx-sdk Go client + onyx CLI (TS client planned)
installer/     ISO, SBC images, script installer (stub)
scripts/       bootstrap + local dev runner
.tools/        repo-local toolchain (git-ignored)
docs/          design docs, RFCs
```
