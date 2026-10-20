# Changelog

All notable changes to ONYX are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and ONYX uses
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] — 2026-10-20 — "Jade"

The platform milestone, and the console that makes it usable: the app store,
virtual machines, object storage, the full share-protocol surface, and a fresh
console built on the shared **Unity** theme.

### Added

- **New overview.** Capacity ring with used/free/total, service health, pool
  state, recent access activity, a needs-attention queue (offline pools, storage
  notices, refusals, capacity above 85%) and quick actions.
- **Collapsible left rail.** 252 px expanded ↔ 76 px icon rail, one control
  (the rail button or Ctrl/Cmd+B), persisted per browser. On a phone the same
  rail is an off-canvas drawer.
- **Command palette** (Ctrl/Cmd+K) covering pages, actions and a pool-wide file
  search; choosing a file search fills the Search page and runs it.
- **Scrub maintenance on the Storage page.** Every pool is listed with its
  cadence, next run and a `Run now` action; schedules are edited inline instead
  of through two browser prompts.
- **Snapshot form.** Pick the pool from the mounted pools and name the
  subvolume in the page, rather than typing a pool name into a prompt.
- **Share connection matrix** for SMB (Windows and macOS/Linux), NFS, FTP,
  SFTP, WebDAV and rsync, with per-protocol copy buttons.
- **Unity theme assets** served beside the console and the landing page, with
  light, dark and follow-the-system modes.
- **Audit CSV export** and refused-request summaries, including who was refused
  and which share is refusing the most.
- **Recovery health** from backup jobs, and per-pool capacity and state.

### Changed

- The console is now dependency-free HTML/CSS/JS on Unity's `operations` scheme;
  the old Prism palette and the separate right-hand activity rail are gone.
- The navigation is grouped by Workspace, Storage and Platform; Search is a
  first-class destination, while Scrub is a Storage action.
- App and container state, VMs, buckets with tiering, and shares now share one
  status vocabulary and one row layout.
- Theme choice moved from `data-theme` on the old Prism tokens to Unity's
  `data-mode` / `data-scheme`, stored under the family's `ontrak.*` keys.
- The version reported by `/api/v1/system/status` and `bin/onyx version` is now
  `0.4.0` / `Jade`.

### Fixed

- Share connection strings are built from the protocol list without shadowing
  the array, so the Shares page can always render its connections.
- A missing `/audit/access` page no longer leaves the overview's activity panel
  in a loading state.
- Storage notices, pool state and the Files capacity card all read the same
  endpoints, so the overview cannot contradict them.
- The browser test harness now instruments the application script rather than
  the pre-paint theme snippet, and its DOM stub understands attributes and
  class lists.

### Removed

- The hard-coded activity rail ("System status endpoint connected", …).
- Hard-coded zero badges on Snapshots and Shares.
- The stale "Safety plane / Obsidian" dashboard card.
- The separate top-level Scrub destination (folded into Storage).
- The old `data-theme`/`onyx.theme` preference and Prism token block.

## [0.3.0] — 2026-09 — "Obsidian"

Safety surfaces: persistent snapshot catalogs, rollback, backup jobs and
recovery reports, scrub scheduling, and SMART-backed device health.

## [0.2.0] — 2026-08 — "Flint"

Files: the first console shell, file explorer, SMB/NFS shares, users and
permissions, and the versioned API.

## [0.1.0] — 2026-07 — "Cinder"

Skeleton: bootable OSTree base, `onyx-core`, storage daemons, API, CLI and the
platform layer.

[0.4.0]: https://github.com/innotelinc/onyx/releases/tag/v0.4.0
