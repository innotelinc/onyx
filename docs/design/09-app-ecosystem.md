# 09 — App Ecosystem

## 1. Model

An **app** is a Docker Compose project plus an **Onyx manifest** that describes how the app
integrates with the OS (permissions, volumes, ports, env, health, UI extras). Apps are the
OS's only extension mechanism — there is no "plugin API" separate from the app store; the
SDK exposes the same surfaces apps use.

```
jellyfin/
├── onyx.yaml            # manifest (required)
├── compose.yaml         # standard Docker Compose
├── icon.svg             # 256px
├── README.md            # rendered in the store
└── screenshots/
```

## 2. Manifest (`onyx.yaml`)

```yaml
id: jellyfin
name: Jellyfin
version: 10.10.0
category: media
license: GPL-2.0
homepage: https://jellyfin.org
description: Free software media system.
icon: icon.svg
compose: compose.yaml
resources:
  memory_min: 512Mi
  memory_max: 4Gi
  cpu: "2"
permissions:                 # what the app may ask for
  volumes:
    - /config                # maps to @apps/jellyfin/config
    - /media                 # user grants a share at install
  ports:
    - 8096                   # published to host
  env:                       # secrets stored in keyring, injected as refs
    - JELLYFIN_ADMIN_PIN
  network: bridge            # bridge | host (host = flagged high-risk)
  devices: []                # e.g. /dev/dri for transcoding
capabilities:
  - media
healthcheck: http://localhost:8096/health
backup:
  paths: [/config]
  databases: []
integrations:
  - webui: { path: "/", port: 8096 }
  - share: { name: "Media", mount: "/media" }   # optional auto-share
store:
  sources: ["official", "community", "custom"]
```

## 3. App store

- **Catalog tiers:**
  - **Official** — curated by the Onyx project: reviewed manifests, pinned + signed image
    digests, tested on target hardware, security-responded. (Jellyfin, Immich, Nextcloud,
    Pi-hole, Syncthing, Home Assistant, Gitea, Paperless-ngx, qBittorrent, Vaultwarden, …)
  - **Community** — community-maintained entries; signed by their maintainers; shown with a
    "community" badge and a trust prompt at install.
  - **Custom** — any user-supplied manifest/compose (paste YAML or point at a git repo).
- **Store protocol:** a static, signed JSON index (`apps.json` per store) with per-app
  manifest URLs; indexes can be self-hosted. Updates to an app = new manifest + digests,
  pulled on schedule or on demand.
- **Search & discoverability:** search by name/description, filter by category and
  capabilities, "Works well on Raspberry Pi" filter, install counts (anonymous, local).
- **Reviews are not in v1** (defer; discussion lives in the forum) — but installs show the
  app's upstream health (last release, open CVEs).

## 4. Install UX (the 30-second promise)

1. **Browse** the store, preview screenshots + README, see resource estimate and disk
   footprint ("~1.2 GB + media").
2. **Install** → the wizard shows exactly what it will do, human-readable:
   "Jellyfin will: create its own storage (2 GB), publish port 8096, and request access to
   your *Media* share."
3. One click → `onyx-appd` pulls pinned images (progress), writes compose to
   `/var/lib/onyx/apps/<id>/`, creates the app subvolume, injects secrets, starts, runs the
   healthcheck.
4. **Done screen:** "Open Jellyfin" (opens the web UI via the gateway's app proxy), links
   to settings, and a one-click "Add a backup for this app".

## 5. App lifecycle and supervision

- **Status:** `installed → starting → running | degraded | stopped | updating | error`.
  Health from the manifest healthcheck + container state; degraded apps show in the
  dashboard Health card with one-click logs.
- **Start/stop/restart/update** from the UI or `onyx app` CLI; updates are
  config-preserving (data volume untouched) with automatic pre-update snapshot of the app
  subvolume and one-click rollback to the previous image.
- **Logs:** unified viewer with follow + filter; `--json` for scripting.
- **Uninstall:** typed confirmation; app subvolume is snapshotted first
  (`app-uninstall-<id>-<ts>`), then removed; user can delete the snapshot later.
- **Startup policy:** `enabled | disabled | on-demand` (on-demand = socket/proxy activation
  where supported, for low-RAM devices).

## 6. Sandboxing and permissions (recap from §07.6)

- All apps: user namespaces, seccomp, AppArmor deny-by-default, memory/pids limits,
  bridge networking, read-only rootfs where upstream allows.
- Every permission (volume, port, device, host network, privileged) is **requested at
  install and re-confirmed on update** if the manifest changed; the UI renders
  permission diffs ("This update adds: port 8080").
- Apps cannot see: the host unix sockets, other apps' subvolumes, the API token, or the
  keyring. Apps reach the API only via an injected, scoped app token with their declared
  `integrations`.

## 7. SDK

`onyx-sdk` (Apache-2.0) provides:

- **Go + TypeScript clients** for the public API (same contracts as the UI).
- **App runtime helpers:** health endpoint conventions, graceful-shutdown contract,
  optional `ONYX_*` env injection (app id, data dir, token) — no SDK dependency required to
  run, only to integrate deeply.
- **Manifest tooling:** `onyx app validate` (lint manifest against the schema),
  `onyx app package` (build a store entry + signature), CI templates.
- **Templates:** a `onyx app new` scaffolder with ready-to-run examples (web service,
  worker, media app).

## 8. App-to-app and share integration

- **Share mounts:** apps declare `share:` mounts that the user fulfills by picking a share
  at install (mounted read-only or rw per the manifest); the Files UI shows "Used by Jellyfin"
  on such shares.
- **App proxy:** apps with `integrations.webui` are reachable at `https://<nas>/apps/<id>/`
  through the gateway (single TLS cert, no extra ports) — port publishing becomes optional.
- **Events:** apps can subscribe to `onyx-bus` topics (e.g., a backup hook after
  `snap.created`) via the SDK — the beginning of real app automation.

## 9. Governance and trust

- **Official store policy:** images must be upstream-official or patched only with
  published source; manifests reviewed in the open; CVEs tracked; apps with sustained
  unmaintained status get a "maintenance needed" badge and eventual archival.
- **Community store:** anyone can publish; signing keys are bound to accounts; the UI shows
  publisher identity and a first-install trust prompt.
- **Reproducibility goal:** official store builds reproducible where upstream allows; SBOM
  per app surfaced in the app detail page.
- **Removal policy:** an app can be delisted but installed copies keep working (no kill
  switch); the OS never deletes user data on store changes.

## 10. Roadmap extensions (designed now, built later)

- **App profiles** for low-RAM hardware (e.g., "Jellyfin Lite": transcoding off, lower
  caches) — v1.5.
- **Sidecar apps** (e.g., download helper for a share, auto-backup on import) — v1.5.
- **App marketplace payments/patreon-style support buttons** — v2, only if community asks.
- **Nested containers / VM apps** — explicitly out of scope; Onyx apps are containers only.
