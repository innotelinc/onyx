# 11 — Platform and Cloud

**ONYX — Online Storage System — Platform.** This document specifies the
platform layer that sits on top of the core appliance (docs/design/01–10):
how the system presents itself as a managed, cloud-connected infrastructure
platform — SSO identity, edge reverse proxy with wildcard TLS, Dockerized
deployment, virtualization, container management, object storage with hybrid
cloud, AI-assisted administration, and the automated release pipeline.

It is the spec behind `setup.sh`, `docker-compose.yml`,
`scripts/npm-proxy-hosts.py`, `scripts/provision-authentik.sh`,
`.github/workflows/`, and the platform daemons (`onyx-snapd`, `onyx-backupd`,
`onyx-vmm`, `onyx-appd`, `onyx-ai`, `onyx-objectstore`).

## 1. Platform identity

- **Product name:** ONYX — Online Storage System — Platform
- **Repository slug:** `onyx-oss-platform`
- **Primary domain:** `onyx.innotel.us`

### 1.1 Service subdomains

Every public surface is a subdomain of the primary domain, routed by Nginx
Proxy Manager (NPM), all behind one wildcard TLS certificate:

| Subdomain | Service | Container | Notes |
|-----------|---------|-----------|-------|
| `app.onyx.innotel.us` | Web UI (Prism SPA, v0.2) | `onyx-web` | static frontend; talks to `api` + `auth` |
| `api.onyx.innotel.us` | HTTP gateway | `onyx-api` | `/api/v1/*`, `/healthz`; also serves `admin` routes |
| `auth.onyx.innotel.us` | Identity / SSO | `authentik-server` | OIDC provider for every other surface |
| `storage.onyx.innotel.us` | S3-compatible object storage | `onyx-objectstore` | `GET/PUT/DELETE` buckets+objects; hybrid-cloud tiering |
| `backup.onyx.innotel.us` | Backup service | `onyx-backupd` | JSON API: jobs, schedules, restore, intelligence |
| `admin.onyx.innotel.us` | Admin console | `onyx-api` (admin routes) | separate hostname, same gateway; access via Authentik |

The mapping is data, not code: `setup.sh` writes it into the environment
(`NPM_SUBDOMAINS`) and `scripts/npm-proxy-hosts.py` turns it into NPM proxy
hosts.

## 2. Identity: Authentik

Authentik is the platform IdP (docs/design/08 moves to it as the primary
provider; local accounts remain as a fallback for offline operation).

- Deployed as containers: `postgres` (state), `redis` (cache), `authentik-server`,
  `authentik-worker` (see §5).
- Bootstrap: `AUTHENTIK_BOOTSTRAP_TOKEN` + `AUTHENTIK_BOOTSTRAP_EMAIL` seed the
  first superuser; `scripts/provision-authentik.sh` waits for readiness, then
  creates the **ONYX Platform** OAuth2 provider and application:
  - authorization/redirect: `https://app.onyx.innotel.us/*`,
    `https://admin.onyx.innotel.us/*`, loopback for local dev;
  - client id/secret are written back into `.env` (`AUTHENTIK_CLIENT_ID`/
    `AUTHENTIK_CLIENT_SECRET`) for downstream services.
- All six subdomains trust `auth.onyx.innotel.us`; NPM protects the
  `admin`/`app` hosts with Authentik's forward-auth where enabled.
- **Passkeys:** the authentication flow carries a WebAuthn stage in
  authentication mode, so enrolled devices confirm sign-in with a platform
  authenticator (docs/design/11 §10).

## 3. Edge: Nginx Proxy Manager

NPM is the only ingress. Everything else binds loopback inside the compose
network (`127.0.0.1` on the host for the native install, per docs/design/07).

### 3.1 API provisioning (`scripts/npm-proxy-hosts.py`)

`setup.sh` integrates this script (the requirement "npm-proxy-hosts.py into
setup.sh"): it is the single source of truth for the NPM state.

1. **Login** — `POST /api/tokens` with `NPM_EMAIL`/`NPM_PASSWORD` (env).
2. **Wildcard certificate** — `POST /api/nginx/certificates` requesting a
   Let's Encrypt certificate for `["*.onyx.innotel.us", "onyx.innotel.us"]`
   with `meta.dns_challenge = true`, provider **`rfc2136`** (RFC 2136 dynamic
   DNS update), carrying the TSIG key:
   - `DNS_NAMESERVER` (e.g. `ns.innotel.us:53`)
   - `TSIG_KEY_NAME`, `TSIG_KEY_SECRET`, `TSIG_KEY_ALGORITHM` (e.g. `hmac-sha256`)
   - Idempotent: if a cert for the wildcard already exists (matched by domain),
     it is reused/renewed rather than duplicated.
3. **Proxy hosts** — for each `NPM_SUBDOMAINS` entry, `GET /api/nginx/proxy-hosts`
   → if absent, `POST /api/nginx/proxy-hosts` with the forward target from the
   compose network (container DNS name + port); if present, update it
   (`PUT`). WebSocket-enabled for `app`/`api`/`admin`.
4. **Report** — prints the final URL table.

Auth to the NPM API is HTTP Basic; all requests over `NPM_BASE_URL`
(default `http://127.0.0.1:81`).

### 3.2 Wildcard certificates

- Issuer: Let's Encrypt, via NPM.
- Challenge: **DNS-01** using the `rfc2136` provider — certbot performs
  `nsupdate` against the authoritative server using the TSIG key, adding a
  `_acme-challenge` TXT record, then removing it after validation. This is the
  same TSIG pattern used by the other innotelinc platform projects.
- Because the challenge is DNS-based, no inbound port 80/443 is needed to
  issue the certificate — only to serve traffic.

## 4. Dockerized deployment

Every daemon has a `Dockerfile` under `docker/` and ships in
`docker-compose.yml`. Two runtimes coexist:

- **Native appliance** (bare metal / OSTree image): systemd units in
  `deploy/systemd/`, installed by `scripts/onyx-install` — unchanged core.
- **Containerized platform** (this document): `docker compose up -d` from
  `setup.sh`, all services on an internal network, ingress via NPM only.

The Go daemons use a shared multi-stage pattern (builder `golang:1.27` →
scratch/alpine runtime, static binaries); the Rust daemons build with
`rust:1-slim` and copy the static binary out. Images are published to GHCR:

```
ghcr.io/innotelinc/onyx-oss-platform/onyx-core
ghcr.io/innotelinc/onyx-oss-platform/onyx-api
ghcr.io/innotelinc/onyx-oss-platform/onyx-shared
ghcr.io/innotelinc/onyx-oss-platform/onyx-storaged
ghcr.io/innotelinc/onyx-oss-platform/onyx-privd
ghcr.io/innotelinc/onyx-oss-platform/onyx-snapd
ghcr.io/innotelinc/onyx-oss-platform/onyx-backupd
ghcr.io/innotelinc/onyx-oss-platform/onyx-vmm
ghcr.io/innotelinc/onyx-oss-platform/onyx-appd
ghcr.io/innotelinc/onyx-oss-platform/onyx-ai
ghcr.io/innotelinc/onyx-oss-platform/onyx-objectstore
```

Within the compose network, gRPC over unix sockets is replaced by gRPC over
the shared network (same contracts in `proto/`); state volumes map to
`/var/lib/onyx/<service>` and the data pool mounts into the storaged/privd
containers.

## 5. Stack inventory (compose)

| Container | Image | Purpose |
|-----------|-------|---------|
| `onyx-privd` | ghcr.io/innotelinc/onyx-oss-platform/onyx-privd | root privilege helper (host-mapped, `--privileged`-free: only needed capabilities) |
| `onyx-storaged` | ghcr.io/.../onyx-storaged | Btrfs discovery, hotplug, SMART |
| `onyx-shared` | ghcr.io/.../onyx-shared | share config rendering |
| `onyx-core` | ghcr.io/.../onyx-core | control plane orchestrator |
| `onyx-api` | ghcr.io/.../onyx-api | HTTP gateway (exposed only to NPM) |
| `onyx-snapd` | ghcr.io/.../onyx-snapd | snapshots |
| `onyx-backupd` | ghcr.io/.../onyx-backupd | backups + intelligence |
| `onyx-vmm` | ghcr.io/.../onyx-vmm | virtualization |
| `onyx-appd` | ghcr.io/.../onyx-appd | container/app management |
| `onyx-ai` | ghcr.io/.../onyx-ai | AI Storage Advisor |
| `onyx-objectstore` | ghcr.io/.../onyx-objectstore | S3-compatible storage |
| `onyx-web` | ghcr.io/.../onyx-web | static SPA (v0.2) |
| `postgres` | postgres:16 | Authentik state |
| `redis` | redis:7 | Authentik cache/queue |
| `authentik-server` | ghcr.io/goauthentik/server | SSO |
| `authentik-worker` | ghcr.io/goauthentik/server | background jobs |
| `nginx-proxy-manager` | jc21/nginx-proxy-manager | ingress (ports 80/443/81) |

## 6. Platform daemons

All six are gRPC services generated from `proto/onyx/v1/`, following the
service conventions of docs/design/04. v0.1 ships compilable skeletons
(contract + server + wiring); roadmap milestones implement the data plane.

### 6.1 `onyx-snapd` — snapshots

Btrfs snapshot lifecycle on top of the fixed subvolume layout
(`@data/@apps/@backups/@snapshots`, docs/design/05):

- `CreateSnapshot` (subvolume, name, optional readonly), `ListSnapshots`,
  `DeleteSnapshot`, `RollbackSnapshot` (snapshot → subvolume, requires
  unmounted target).
- Scheduling hooks for `onyx-backupd` (snapshot-before-backup).

### 6.2 `onyx-backupd` — backups + Backup Intelligence

- Jobs: source (share/snapshot/pool) → target (local, NFS, remote ONYX,
  S3-compatible cloud via `onyx-objectstore`); schedules (cron); retention
  policies; `RestoreBackup`.
- **Backup Intelligence:** `GetBackupReport` aggregates job history,
  success/failure trends, data-change velocity per source, and
  recovery-time/recovery-point estimates. Heuristics run locally;
  `onyx-ai` can enrich the same report with natural-language findings.

### 6.3 `onyx-vmm` — virtualization

- VM inventory (`ListVMs`), lifecycle (`CreateVM`, `StartVM`, `StopVM`,
  `DeleteVM`), resources (vCPU/RAM/disk), and media attachment.
- v0.4 milestone: libvirt/QEMU backend; disk images on the pool's
  `@apps`/`@data` subvolumes.

### 6.4 `onyx-appd` — container management

- App catalog (`ListApps`, `InstallApp`, `UninstallApp`), container lifecycle
  (`StartContainer`, `StopContainer`, `RestartContainer`), and compose
  manifests from the signed app store (docs/design/09).

### 6.5 `onyx-ai` — AI Storage Advisor + Backup Intelligence

- `AnalyzeStorage`: pool/device telemetry (from `onyx-storaged`) →
  recommendations (free-space runway, snapshot cadence, scrub health,
  expansion guidance). Deterministic heuristics are computed in-process;
  a provider hook (`AI_PROVIDER`, `AI_API_KEY`, `AI_MODEL` — local or BYO-key
  remote) turns the same findings into natural-language advice.
- `AnalyzeBackups`: consumes `onyx-backupd`'s report (6.2) and produces
  findings + priorities.
- Privacy: no telemetry leaves the box unless an explicit BYO-key provider is
  configured (docs/design/07).

### 6.6 `onyx-objectstore` — object storage + hybrid cloud

- S3-compatible API surface (`storage.onyx.innotel.us`): `ListBuckets`,
  `PutObject`, `GetObject`, `DeleteObject`, `DeleteBucket` — served over HTTPS
  by NPM; static credentials from env (`S3_ACCESS_KEY`/`S3_SECRET_KEY`).
- Bucket storage on the pool (Btrfs subvolume per bucket → snapshots/scrub
  come free).
- **Hybrid cloud:** per-bucket lifecycle policies — `LOCAL`, `CLOUD` (primary
  in an external S3 provider), or `TIERED` (local hot tier, cloud cold tier,
  with sync/eviction rules). The cloud side is pluggable (AWS S3, Backblaze
  B2, other S3-compatible endpoints) via `HYBRID_CLOUD_ENDPOINT` +
  credentials.

## 7. Release pipeline

`.github/workflows/` implements CI/CD:

- **`ci.yml`** — on push/PR: `make bootstrap` (cached), `make check`
  (vet + tests), `make build`. Validation only; nothing is published.
- **`release.yml`** — one publish pipeline for both event kinds:
  - `main` push → build + publish `:latest` images;
  - tag `v*` push → build + publish `:latest` + `:<tag>` images, then create
    the GitHub Release with tarball artifacts;
  - `workflow_dispatch` → manual re-publish (latest).

  The workflow is concurrency-guarded (group keyed by commit SHA,
  cancel-in-progress), so a tag pushed on a commit that just triggered the
  `main` `latest` build cancels the duplicate instead of racing it on the
  first-run compile and the GHCR push. Buildx uses the `gha` cache, so
  subsequent builds reuse compiled layers instead of compiling cold.

  The release job (artifacts) depends on the image job, so the GitHub Release
  only appears after every image is published.

## 8. Operational flow (setup.sh)

```
./setup.sh
  1. validate: docker + compose present, env loaded
  2. cp .env.example .env if missing (then require editing secrets)
  3. docker compose up -d --build     (onyx stack + Authentik + NPM)
  4. wait for NPM API + Authentik readiness
  5. scripts/provision-authentik.sh    (bootstrap superuser, ONYX OIDC app, passkey stage)
  6. device trust — per DEVICE_TRUST:
       off      → nothing to do (passkeys still on)
       local    → scripts/provision-device-trust.sh (ensure the CA)
       cerulean → POST the mTLS gate request to the Cerulean API
  7. NPM provisioning — per DEVICE_TRUST/NPM_MODE:
       local edge  → scripts/npm-proxy-hosts.py (TSIG wildcard + hosts + mTLS gate)
       cerulean edge → skipped; the edge is Cerulean-managed, cert gates are
                       requested in step 6
  8. print URL table (app/api/auth/storage/backup/admin)
```

Idempotent end to end: re-runs converge (cert reuse, proxy-host update,
provider re-check).

## 9. Security posture

- Single ingress (NPM) on 80/443; everything else loopback-only inside the
  compose network (extends docs/design/07 to the containerized deployment).
- Wildcard cert keys live in NPM's data volume; TSIG secret only in `.env`
  (never committed — `.env` is git-ignored, `.env.example` has placeholders).
- `onyx-privd` remains the only privilege boundary; containers run as the
  same unprivileged per-service users where possible.
- Authentik is the SSO choke point: forward-auth on `app`/`admin`, OIDC for
  the API, service-to-service traffic stays on the internal network.
- **Device trust is opt-in** (`DEVICE_TRUST=off` by default): when enabled,
  passkeys confirm identity in Authentik while mTLS client certificates gate
  the edge — `local` CA + local NPM for standalone deploys, or the Cerulean
  control plane (dashboard issuance, remote NPM) for managed fleets
  (docs/design/11 §10).

## 10. Device trust (passkeys + mTLS client certificates)

Management surfaces (`app`, `admin`) can be gated by two factors that bind a
login to a *device*, not just a password. Device trust is **optional** and
selected by one switch, `DEVICE_TRUST=off|local|cerulean` (default `off`):

1. **Passkeys (WebAuthn)** — Authentik is the IdP, so platform
   authenticators (Touch ID, Windows Hello, Android/iOS, hardware keys) are
   enabled at the source: the default `default-authentication-identification`
   flow gets a **WebAuthn stage in authentication mode**, so every login is
   confirmed by the device's passkey once one is registered. Users register
   passkeys from Authentik's settings; `provision-authentik.sh` flips the
   flow stage on automatically.
2. **TLS client certificates (mTLS)** — the edge requires a client
   certificate from a trusted device CA before the request even reaches
   Authentik's proxy. A device without a trusted certificate cannot start an
   OIDC flow, which removes credential-stuffing and stolen-password classes
   of attack for the admin surface entirely. Who runs the CA and where the
   gate is applied depends on `DEVICE_TRUST`:

| Mode | Device CA | Certificate issuance | mTLS enforcement | For |
|------|-----------|----------------------|------------------|-----|
| `off` (default) | — | — | — | devices stay optional |
| `local` | ONYX host (`/etc/onyx/pki`, `provision-device-trust.sh`) | CLI helper → `.p12` for MDM import | local NPM (`ssl_verify_client`) | standalone deploys |
| `cerulean` | **Cerulean** (remote MDM/control plane) | **Cerulean dashboard** (approve, issue, revoke) | requested from Cerulean via its API | fleets behind Cerulean |

The result: on an enrolled device, sign-in is one passkey tap. On any other
device, the TLS handshake fails before a password is ever asked for.

### 10.1 `local` mode — device CA (`scripts/provision-device-trust.sh`)

A minimal internal PKI maintained under `/etc/onyx/pki` (host bind mount,
`ONYX_PKI_DIR` to override; private keys are mode 0600, never leave the
host):

```
ca.crt / ca.key          ECDSA P-256 self-signed device CA (10 years)
serial                   openssl tracking file
issued/<name>.crt/.key   per-device client certs (default 365 days)
```

- `scripts/provision-device-trust.sh` (no args) creates the CA if missing —
  idempotent, called from `setup.sh` only in `local` mode.
- `scripts/provision-device-trust.sh issue laptop [days]` issues (or renews)
  a client certificate with CN `<name>` plus the
  `ONYX Device Trust` / `device=<name>` O/OU identifiers, for distribution
  via your MDM (Apple Configurator / `.mobileconfig`, Intune, Fleet, or a
  manual import into the OS/browser trust store).

### 10.2 `local` mode — edge enforcement (`npm-proxy-hosts.py`)

For every subdomain listed in `DEVICE_TRUST_SUBDOMAINS` (default
`app admin`), the proxy host's **advanced config** is extended with the Nginx
mTLS directives — merged idempotently with any existing advanced config:

```nginx
ssl_client_certificate /etc/letsencrypt/onyx-device-ca/ca.crt;
ssl_verify_client on;
ssl_verify_depth 2;
```

The CA is mounted into the NPM container from the host PKI dir
(`docker-compose.yml`); `ssl_verify_client on` makes NPM return **400 with
`No required SSL certificate was sent`** for devices without an enrolled
certificate — before any auth flow starts. Nginx passes the verified
subject to upstreams in `X-SSL-Client-Subject-DN`, which Authentik forward
auth can use for audit trail enrichment.

### 10.3 `cerulean` mode — remote control plane

When the platform is managed by **Cerulean** (the innotelinc fleet
control plane; also owns the remote NPM and the open-source MDM underneath),
ONYX owns *policy*, Cerulean owns *enforcement*:

- **Issuance lives in the Cerulean dashboard:** an operator approves a
  device, Cerulean's MDM pushes the client certificate, and the device CA
  pool is managed entirely remotely — ONYX never stores CA keys in this
  mode (`provision-device-trust.sh` is not run).
- **ONYX requests the gate** by calling Cerulean's device-trust API
  (`CERULEAN_API_URL` + `CERULEAN_API_TOKEN` in `.env`):

  ```
  POST {CERULEAN_API_URL}/api/v1/fleet/{FLEET_ID}/device-trust
  Authorization: Bearer ${CERULEAN_API_TOKEN}
  {
    "domain": "onyx.innotel.us",
    "subdomains": ["app", "admin"],      # = DEVICE_TRUST_SUBDOMAINS
    "enforcement": "mtls",
    "client_ca": "cerulean-managed"
  }
  ```

  Cerulean applies the equivalent `ssl_verify_client` configuration on its
  remote NPM (or drops the mTLS requirement on revoke) and reports the
  enrolled device list back to the dashboard.
- **`setup.sh` skips local NPM provisioning** — with a remote edge there is
  no local NPM to configure (§8).
- **Complement, not replacement:** passkeys remain Authentik-side in every
  mode; Cerulean enforces the edge gate, Authentik enforces the identity
  gate, and a device needs both factors to reach `app`/`admin`.

The Cerulean contract above is an interface, not a client: this repo ships
the request schema and the `setup.sh` hook, while the server side (and the
open-source MDM it drives — e.g. Fleet, MicroMDM, or Headwind) belongs to
Cerulean.

### 10.4 Trade-offs and operations

- **Strictness:** `ssl_verify_client on` was chosen over `optional` so the
  gate is real; enrollment is a deliberate act (`local`: issue cert →
  install on device; `cerulean`: approve in dashboard → MDM pushes). 
- **Revocation:** `local` — delete the issued cert or rotate the CA and
  re-provision; `cerulean` — revoke from the dashboard, the edge drops the
  device on the next sync.
- **Failure mode:** `local` CA loss is recoverable by regenerating and
  re-enrolling the (small) fleet; `cerulean` mode has no local key material
  to lose.
- **S3/backup surfaces are exempt:** machine clients (S3 keys, backup
  agents) authenticate with credentials, not device certificates; only
  interactive surfaces are device-gated.
