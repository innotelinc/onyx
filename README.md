<div align="center">

# 🗄️ ONYX — Online Storage System

**The NAS and infrastructure platform that replaces TrueNAS and ZimaOS — storage, virtualization, object storage, backup, and app hosting in one self-hosted stack.**

ONYX is a lightweight, privacy-first NAS *and* infrastructure platform for hardware you
already own — a mini-PC, a server, an old laptop. Enterprise-grade storage safety
(Btrfs snapshots, checksums, scrubs), CasaOS/ZimaOS-level app and VM hosting, and a calm,
keyboard-first interface (the *Prism* design system) — in a stack that boots in under a
minute and idles under 512 MB RAM.

[![CI](https://github.com/innotelinc/onyx/actions/workflows/ci.yml/badge.svg)](https://github.com/innotelinc/onyx/actions/workflows/ci.yml)
[![Release](https://github.com/innotelinc/onyx/actions/workflows/release.yml/badge.svg)](https://github.com/innotelinc/onyx/actions/workflows/release.yml)
[![Pages](https://github.com/innotelinc/onyx/actions/workflows/pages.yml/badge.svg)](https://github.com/innotelinc/onyx/actions/workflows/pages.yml)
[![Latest release](https://img.shields.io/github/v/release/innotelinc/onyx?color=18181b)](https://innotelinc.github.io/onyx/releases)

</div>

> **About ONYX** — a next-generation storage and infrastructure platform that replaces
> TrueNAS and ZimaOS: enterprise-grade storage, virtualization, object storage, backup
> management, cloud synchronization, and application hosting in one self-hosted stack —
> SSO everywhere via Authentik, safe by default with snapshots and A/B rollback, and no
> cloud account required. **Landing page:** [innotelinc.github.io/onyx](https://innotelinc.github.io/onyx)

---

## Services

Primary domain: `onyx.innotel.us`

| Service | URL | Backed by |
|---------|-----|-----------|
| App (web UI) | `https://app.onyx.innotel.us` | `onyx-web` (SPA, v0.2) |
| API gateway | `https://api.onyx.innotel.us` | `onyx-api` |
| Identity / SSO | `https://auth.onyx.innotel.us` | Authentik |
| Storage (S3-compatible) | `https://storage.onyx.innotel.us` | `onyx-objectstore` |
| Backup | `https://backup.onyx.innotel.us` | `onyx-backupd` |
| Admin | `https://admin.onyx.innotel.us` | `onyx-api` (admin surface) |

All traffic terminates TLS at **Nginx Proxy Manager**, which is provisioned
automatically by `setup.sh` (via `scripts/npm-proxy-hosts.py`) with a
Let's Encrypt **wildcard certificate** for `*.onyx.innotel.us`, issued through
a **TSIG (RFC 2136) DNS-01 challenge** against your DNS server — the same
pattern used across the innotelinc platform projects.

## Platform stack

- **Identity:** [Authentik](https://goauthentik.io) — OIDC/SSO for every
  service (`auth.onyx.innotel.us`); `scripts/provision-authentik.sh` creates
  the ONYX application + OAuth2 provider automatically.
- **Reverse proxy:** Nginx Proxy Manager, provisioned via its API —
  wildcard cert, proxy hosts for all six subdomains, WebSocket support.
- **Runtime:** every service is containerized (Docker); the appliance image
  (`base/`) still ships the native systemd deployment for bare metal.
- **Storage core:** Btrfs pools, snapshots, backups, hotplug device handling,
  share generation (SMB/NFS/FTP/SFTP/WebDAV/Rsync from one logical model).
- **Platform services:** virtualization (`onyx-vmm`), container management
  (`onyx-appd`), object storage + hybrid cloud (`onyx-objectstore`), AI Storage
  Advisor + Backup Intelligence (`onyx-ai`).
- **CI/CD:** GitHub Actions builds, checks, and publishes container images to
  GHCR and attaches release artifacts to every tagged build.
- **Device trust (optional):** passkeys via Authentik plus an mTLS client-cert
  gate on `app`/`admin` — `DEVICE_TRUST=off|local|cerulean` chooses between
  off, an ONYX-run device CA (standalone), or the **Cerulean** control plane
  (certificate issuance from its dashboard/MDM, remote NPM edge) —
  docs/design/11 §10.
- **Infisical secrets (runtime):** `S3_*` and `CERULEAN_API_TOKEN` accept
  `infisical://<name>` references resolved at startup (shared Go client in
  `services/infisical/`); `onyx-objectstore` mirrors plain S3 credentials into
  Infisical on boot, and `GET /api/v1/status` reports SecretOps health.
- **Landing page:** [`web/landing/`](web/landing/) — static, Prism-styled
  project page published to <https://innotelinc.github.io/onyx/> by
  [`.github/workflows/pages.yml`](.github/workflows/pages.yml).
- **Design-doc site:** the eleven documents in [`docs/design/`](docs/design/)
  render as a MkDocs Material site at
  <https://innotelinc.github.io/onyx/docs/> (same Pages deploy;
  [`mkdocs.yml`](mkdocs.yml) is presentation-only — the docs directory stays
  the source of truth).

## Key properties

- ⚡ Boots in under a minute, idles under 512 MB RAM
- 🔒 Safe by default: snapshots on, atomic A/B updates with auto-rollback, no
  protocols exposed until you enable them
- 📁 SMB, NFS, FTP, SFTP, WebDAV, and Rsync from one logical share model
- 🧩 Modular: small, single-purpose services (Go + Rust) behind one typed API
- 🏠 SSO everywhere via Authentik (OIDC), no cloud account required
- 🤖 AI Storage Advisor + Backup Intelligence, local-first (or BYO-key remote)
- 🗄️ S3-compatible object storage + hybrid cloud sync (`onyx-objectstore`)
- 🔄 Automated releases: GHCR images + tarball artifacts on every tag

## Design documents

The full specification lives in [`docs/design/`](docs/design/README.md) —
eleven documents covering product vision, architecture, the Prism design
system, backend services, storage, API, security, user management, the app
ecosystem, installer/update design, and the new **platform & cloud layer**
(`11-platform-and-cloud.md`: Authentik, NPM provisioning, wildcard TSIG certs,
subdomain routing, Dockerized deployment, virtualization, container
management, AI advisor, object storage + hybrid cloud).

## Status

**Phase: v0.1 "Cinder" — core skeleton + platform layer.** The appliance
skeleton is complete and installable: `onyx-core` (Go control plane),
`onyx-api` (HTTP gateway), `onyx-shared` (Go share manager), `onyx-storaged` +
`onyx-privd` (Rust data plane), and the `onyx` CLI, wired together via gRPC
over unix sockets (`proto/` as the source of truth), shipped as systemd
services by [`scripts/onyx-install`](scripts/onyx-install). The bootable OSTree
base image (`base/`) composes a Debian Trixie rootfs with the onyx stack, a
kernel, and a bootloader with A/B rollback.

On top of that, the **platform layer** is in place:

- `docker/` + `docker-compose.yml` — every daemon containerized. Authentik and NPM
  are **external by default** (shared platform services); set `AUTHENTIK_MODE=local`
  or `NPM_MODE=local` in `.env` to start bundled replacements as Compose profiles.
- [`setup.sh`](setup.sh) — one-command deploy: env generation, `docker compose
  up`, Authentik bootstrap + ONYX OIDC provider, then NPM provisioning
  (wildcard `*.onyx.innotel.us` cert via TSIG, six proxy hosts).
- [`scripts/npm-proxy-hosts.py`](scripts/npm-proxy-hosts.py) — NPM API client:
  login, wildcard certificate request (Let's Encrypt DNS-01 over RFC 2136),
  idempotent proxy-host create/update for every subdomain.
- `.github/workflows/` — CI (bootstrap → vet → test → build) and release
  (tagged builds publish GHCR images and attach tarball + checksum artifacts).
- New platform daemons (`onyx-snapd`, `onyx-backupd`, `onyx-vmm`, `onyx-appd`,
  `onyx-ai`, `onyx-objectstore`) as compilable gRPC service skeletons with
  proto contracts — see [`services/README.md`](services/README.md).

Next milestone: [v0.2 "Flint"](docs/design/01-product-vision.md#7-roadmap) —
the web UI (Prism), file explorer, SMB/NFS shares UX, users & permissions, and
the interactive web first-boot wizard on top of this base.

## Quick start (Docker platform)

```bash
cp .env.example .env        # edit: DOMAIN, NPM creds, TSIG key, Authentik secrets
./setup.sh                  # compose up → Authentik bootstrap → NPM provision
```

`setup.sh` is idempotent: safe to re-run; it prints the final URL table
(`app`/`api`/`auth`/`storage`/`backup`/`admin` on `onyx.innotel.us`).

### Authentik and NPM modes

ONYX uses shared platform services by default. Authentik and NPM are **not started**
by the base Compose file — point at the external instances via `AUTHENTIK_URL` and
`NPM_BASE_URL`. To run local replacements instead:

```bash
AUTHENTIK_MODE=local NPM_MODE=local ./setup.sh
# or edit .env: AUTHENTIK_MODE=local, NPM_MODE=local
```

Local mode starts Authentik (server + worker + postgres + redis) and NPM (with
MariaDB + backup-ui) as Compose profiles. Do not enable the local profile when
another Innotel stack already owns these services.

## Building and running (dev, native)

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
kept as a safety net. Removable devices (USB/SD) auto-attach by default;
hotplugged internal drives attach with `onyx device attach`. Call
`scripts/hotplug-demo.sh` for a scripted plug/unplug lifecycle demo.

**Generating shares actually writes real daemon config.** Every share mutation
runs the full pipeline: `onyx-core` renders the complete `smb.conf` (global +
every SMB share) and `exports` (unique fsids) via `onyx-shared`'s `RenderAll`,
`onyx-privd` writes changed files atomically (default `/etc/onyx/conf.d/`),
and reloads the affected daemons — `testparm` validation first, then
`systemctl reload smbd`; NFS via `exportfs -ra`. It is change-guarded: an
unchanged share set rewrites nothing, and a failed reload always retries.

## Installing on a real host (systemd)

The same skeleton installs as proper systemd services — per-service
unprivileged users, sockets in `/run/onyx`, state in `/var/lib/onyx/*`,
generated daemon config in `/etc/onyx/conf.d` (see [`deploy/`](deploy/README.md)
for the unit graph and runtime layout):

```bash
sudo scripts/onyx-install            # build → users → dirs → units → enable+start
scripts/onyx-install --destdir /tmp/stage   # stage files only (packaging/tests)
sudo scripts/onyx-install --uninstall       # stop, disable, remove
```

Only `onyx-privd` runs as root — it is the single privilege boundary; every
other daemon is sandboxed (`NoNewPrivileges`, strict filesystem policy). The
API gateway binds `127.0.0.1` by default, so **nothing is exposed to the
network until you deliberately change it**. Optional serving daemons (samba,
nfs-kernel-server, smartmontools, btrfs-progs) are the host's packages, not
onyx's: `sudo scripts/onyx-install --install-deps` installs them.

## License

ONYX is dual-licensed per the design (docs/design/01 §8): the core OS
(root `LICENSE`) is **AGPL-3.0**; the SDK under `sdk/` (`sdk/LICENSE`) is
**Apache-2.0**. No third-party author attributions are asserted; the project
is owned by its operators.

## Repo layout

```
base/          OSTree image definition (compose scaffold: manifest + compose.sh + seed)
deploy/        systemd units + tmpfiles for the running system
docker/        per-service Dockerfiles for the containerized platform
services/      onyx-api, onyx-core, onyx-shared, onyx-snapd, onyx-backupd, onyx-vmm,
               onyx-appd, onyx-ai, onyx-objectstore (Go) · onyx-storaged, onyx-privd (Rust)
proto/         gRPC contracts (source of truth) + generated stubs
web/           landing page (GitHub Pages) · Prism design system + React SPA (planned, v0.2)
sdk/           onyx-sdk Go client + onyx CLI, Apache-2.0 (TS client planned)
installer/     ISO + SBC image design; the script installer lives in scripts/
scripts/       bootstrap, local dev runner, onyx-install, npm-proxy-hosts.py,
               provision-authentik.sh
.github/       CI + release workflows (GHCR images, release artifacts)
.tools/        repo-local toolchain (git-ignored)
docs/          design docs (+ MkDocs Material site, published to Pages) · RFCs
```

## 🏛️ Platform stack

ONYX is the ecosystem's **StorageOps** platform — file/object storage, backups, snapshots, and NAS features in the
[**Innotel Platform Stack**](https://github.com/innotelinc/innotel-platform-stack) — the
canonical single-responsibility architecture where Authentik owns identity, Infisical owns
secrets, Cerulean owns trust, ONYX owns storage, Magnate owns revenue, NPM Edge owns the edge, and every other
platform is a business function that consumes them. See
[docs/stack.md](docs/stack.md) for this platform's owns/consumes boundaries and its
Infisical secret setup.
