# 05 — Storage Subsystem

## 1. Filesystem strategy

| FS | Role | Rationale |
|----|------|-----------|
| **Btrfs** | Default data pools | Snapshots (reflink, near-instant), checksums with scrub, transparent zstd compression, per-subvolume quotas, RAID0/1/10 — at a fraction of ZFS's RAM cost (no ARC pressure on small systems) |
| **ext4** | Opt-in simple pools | For users who want maximum simplicity/portability; no snapshots (Onyx then layers file-level backup instead) |
| **ZFS** | Explicit opt-in (advanced) | Available as an install-time option for ZFS die-hards; not the default because RAM/flash-wear profiles are wrong for the target hardware |
| **OSTree (read-only)** | System root | A/B atomic updates (§10) |

**Decision rationale vs. ZFS:** on a Raspberry Pi with 4 GB RAM, ZFS leaves little for apps;
Btrfs delivers the safety features the product promises (snapshots, checksums, compression,
quotas) with ~1/10th the memory overhead. Btrfs RAID1 provides redundancy for the target
2-disk configurations, and the pool format is `btrfs-progs`-standard so data is recoverable
with standard tools even if Onyx disappears.

## 2. Pool and layout model

**Pool** = a Btrfs filesystem over one or more devices. **Dataset** = a named subvolume that
maps to user-visible folders (one per share/backup/app-data). Fixed system layout:

```
/mnt/onyx/<pool>                 # Btrfs top-level
├── @system/                     # OSTree root (read-only, on system disk if present)
├── @config/                     # /etc/onyx (binds to /etc/onyx)
├── @data/                       # user data root
│   ├── <share-subvol>/          # one subvolume per share (r/o snapshots live beside it)
│   └── homes/<user>/            # home subvolume per user
├── @apps/                       # app data (one subvolume per app → clean app snapshots)
├── @backups/                    # backup targets
└── @snapshots/                  # snapshot store (mountpoint for .snapshots trees)
```

Rationale: per-share/per-app subvolumes give **independent snapshot schedules and quotas**,
clean `btrfs send` streams for backup, and bounded rollback scope.

## 3. Snapshots (`onyx-snapd`)

- **Creation:** read-only subvolume snapshots — instant, space-efficient (reflink COW).
- **Schedules:** per-dataset retention policies: e.g. hourly × 24, daily × 7, weekly × 4,
  monthly × 12; policy edits take effect without touching data.
- **UI:** snapshot timeline per folder with browse (mount read-only for point-in-time
  browsing) and one-click rollback (rename current → keep-as-snapshot, promote target).
  Every rollback is preceded by an automatic safety snapshot and logged to audit.
- **Restore granularity:** whole-dataset rollback *or* file-level restore by browsing the
  snapshot mount and copying out.
- **Off-site:** `btrfs send/receive` streams feed `onyx-backupd` for incremental remote
  backups.

## 4. Data integrity

- **Checksums:** Btrfs metadata+data checksums verified by **scrub**; scheduled default
  weekly, tunable to monthly/quarterly on large pools.
- **Self-heal:** on redundant pools, scrub repairs bad copies automatically and reports.
- **SMART (`onyx-agent`):** per-disk attributes + temperature polling; warnings at
  pre-failure thresholds (Reallocated Sectors, Pending Sectors, UDMA CRC); failures promote
  to a dashboard alert and (optionally) email/ntfy notification.
- **FSTRIM:** weekly TRIM on SSDs; wear-leveling friendly defaults for SD cards (noatime,
  `commit=120` on system disks, logs to RAM).

### 2.1 Mount visibility across containers

Pools are mounted under `/mnt/onyx` **on the host**, by onyx-privd (the only
privileged process). Every other container — storaged, api, davd — bind-mounts
that same host directory, so a mount box is only shared if propagation is
*shared* on both sides:

- the host path must be a shared mount (`mount --make-shared /mnt/onyx`, and an
  `/etc/fstab` entry with the `shared` option to survive a reboot);
- the compose bind mounts use `:rshared`.

Get one half right and the failure is silent and confusing: privd mounts the
pool, the data plane reports it as mounted, and the API's storage root is an
empty directory — which reads as "No storage is mounted" in Files. `setup.sh`
checks and fixes the host half, and `GET /storage/overview` reports the
mismatch (per-entry `visible` flag plus an operator-actionable warning) instead
of pretending there is nothing there.

## 5. Quotas and capacity

- **Quotas:** `btrfs qgroup` per user and per share; enforced soft (warn) + hard (block)
  limits, overridable per user. UI shows a QuotaBar in file views and user settings.
- **Reserved space:** 5% emergency headroom per pool; the UI prevents creating shares that
  would push the pool below it.
- **Capacity planning:** the Storage card shows pool health, per-dataset usage, snapshot
  reclaimable space (estimate via qgroup), and projected growth from history.
- **What the card measures:** capacity comes from the *mounted pool* (statfs of the pool's
  mountpoint, falling back to the data plane's own totals when the pool is not reachable),
  never from whatever filesystem the storage root happens to sit on — on a normal install that
  is the host's system disk, and reporting it as "Storage" is how a 220 GB root disk gets
  shown as a 20 TB pool. `GET /storage/overview` is the source; `/files/trash` carries the
  same totals for compatibility.

## 6. Sharing protocols (`onyx-shared`)

All protocols read the same logical share model (name, path, users/groups, read/write,
options) and translate it to per-daemon config. Shares are created once, exposed everywhere.

| Protocol | Daemon | Notes |
|----------|--------|-------|
| **SMB** | Samba | SMB2/3 default (SMB1 disabled), `vfs objects = btrfs` for reflink copy-offload, user-level auth against Onyx users, optional AD/LDAP join; per-share browseable/guest settings |
| **NFS** | Linux NFS | NFSv4 with Kerberos optional; `fsid` per share; squash settings; only exposed on demand (never by default) |
| **FTP** | vsftpd | Explicit FTPS (TLS) required by default; chroot to share root; virtual users mapped to Onyx users |
| **SFTP** | Dedicated `sshd` instance | Scoped config (`Subsystem sftp`, `ForceCommand internal-sftp`, chroot) on its own port (2222, so the host's admin SSH is untouched); keys live in `/etc/onyx/conf.d/sftp/authorized_keys/%u` because a chrooted user's `%h` is inside the share, where the share owner could replace them |
| **WebDAV** | Go WebDAV server (`onyx-davd`, `services/davd`) | HTTPS only, integrates with the API auth layer (session or app token); ideal for cloud-sync clients (Nextcloud desktop, RaiDrive). Serves `/webdav/<share>` on loopback, renders no TLS itself and authenticates nobody itself — it requires the gateway's identity header (`X-Onyx-User`), so a request that bypassed the gateway is rejected rather than trusted |
| **Rsync** | `rsyncd` via systemd socket | Read/write modules per share, restricted to configured users, chroot-style path containment |

**Exposure policy:** every protocol is **off by default**; enabling it is an explicit,
logged act. The Share detail page shows which protocols expose a given share, with copyable
connection strings (`smb://`, `nfs://`, `davs://`, `rsync://`, `sftp://`).

**Lifecycle:** each protocol's daemon is gated on the config file onyx-core renders for it
(`ConditionPathExists=` in systemd). Enabling the first share for a protocol starts that
daemon through onyx-privd (`systemctl reload-or-restart`); disabling the last share rewrites
the config empty and reloads just that daemon. A protocol nobody uses therefore has no
listening process at all.

## 7. Disk management

- **Import/export:** `onyx storaged` scans for Btrfs pools and foreign filesystems; importing
  a foreign disk offers read-only mounting first, with an explicit "adopt" flow that never
  destroys data without typed confirmation.
- **Add disk to pool:** online `btrfs device add` to RAID1 pools (rebalance progress
  reported as a job).
- **Replace disk:** guided wizard — mark failed disk, hot-swap, `device replace` with
  progress + ETA.
- **System vs. data disks:** the OS prefers a small dedicated system disk (SD/SSD); pools on
  data disks stay untouched by updates and can be moved between machines.

## 8. Backup (`onyx-backupd`)

| Target | Mechanism |
|--------|-----------|
| Local disk / USB | rsync or Btrfs send/receive to another pool |
| Remote NAS/server | rsync over SSH (with `--partial` + resume), incremental via hardlinks |
| Object storage (S3-compatible) | rclone-style sync with server-side encryption (age key, client-side) |
| Another Onyx | native `onyx backup peer` protocol (Btrfs send/receive + auth) |
| Cloud drive (v0.5+) | rclone backends (Google Drive, Dropbox, …) |

- **Schedules:** cron-style with UI presets (daily at 02:00, etc.); missed-run catch-up.
- **Encryption:** optional client-side (age) so backups at rest are unreadable without the
  key; keys in the keyring, restorable via a printed recovery phrase.
- **Verification:** `--checksum` on small trees; restore-drill job (monthly, optional) writes
  to a scratch subvolume and reports.
- **Backup of the OS config:** every snapshot job includes `/etc/onyx` + SQLite set, so a
  full disaster restore = reinstall + import pool + restore config.

### 8.1 Cloud and remote targets (setup)

The Shares page registers a cloud account or remote server once and stores it in the single
shared rclone catalog (`/etc/rclone/rclone.conf` — bind-mounted read-write into `onyx-api`
and read-only into `onyx-backupd`), so a target configured there is immediately usable as a
backup destination *and* as a clone target:

| Endpoint | Purpose |
|----------|---------|
| `GET /api/v1/storage/providers` | the closed set of backends the form offers, with each one's option names |
| `GET /api/v1/storage/remotes` | configured targets (name → backend type) |
| `POST /api/v1/storage/remotes` | write one target (`name`, `type`, `params`) |
| `POST /api/v1/storage/remotes/{name}/check` | reachability probe (`rclone lsd`) |
| `DELETE /api/v1/storage/remotes/{name}` | forget a target; data at the provider is untouched |
| `POST /api/v1/storage/clone` | copy a storage folder out to a target (`rclone copy`, additive) |

Only the options a backend declares are accepted, values for password/secret fields are
obscured with `rclone obscure` before they are written, and every invocation uses an
explicit argv — never a shell. OAuth backends (Google Drive, OneDrive, Dropbox, Box,
pCloud) are created without credentials and need one browser approval per target:
`rclone config reconnect <name>:` on the host. CLI equivalents: `onyx storage
providers|remotes|add|rm|check|clone`.

## 9. Performance tuning (defaults, expert-overridable)

- Compression: `zstd:3` on `@data` (best speed/size for mixed media); `zstd:1` on `@apps`.
- Mount options: `noatime,space_cache=v2,ssd` (SSD), `compress-force=zstd` off for
  already-compressed media dirs (per-dataset `nodatacow` where needed).
- Samba: `btrfs` VFS, `aio read size`, socket options defaults; throughput target §02.9.
- The UI never surfaces raw tuning flags by default; a "Expert" toggle in pool settings
  reveals documented options with warnings.
