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

## 5. Quotas and capacity

- **Quotas:** `btrfs qgroup` per user and per share; enforced soft (warn) + hard (block)
  limits, overridable per user. UI shows a QuotaBar in file views and user settings.
- **Reserved space:** 5% emergency headroom per pool; the UI prevents creating shares that
  would push the pool below it.
- **Capacity planning:** the Storage card shows pool health, per-dataset usage, snapshot
  reclaimable space (estimate via qgroup), and projected growth from history.

## 6. Sharing protocols (`onyx-shared`)

All protocols read the same logical share model (name, path, users/groups, read/write,
options) and translate it to per-daemon config. Shares are created once, exposed everywhere.

| Protocol | Daemon | Notes |
|----------|--------|-------|
| **SMB** | Samba | SMB2/3 default (SMB1 disabled), `vfs objects = btrfs` for reflink copy-offload, user-level auth against Onyx users, optional AD/LDAP join; per-share browseable/guest settings |
| **NFS** | Linux NFS | NFSv4 with Kerberos optional; `fsid` per share; squash settings; only exposed on demand (never by default) |
| **FTP** | vsftpd | Explicit FTPS (TLS) required by default; chroot to share root; virtual users mapped to Onyx users |
| **SFTP** | Dedicated `sshd` instance | Scoped config (`Subsystem sftp`, `ForceCommand internal-sftp`, chroot), on a separate port or default 22 with main SSH locked down; key + password auth |
| **WebDAV** | Go WebDAV server (`onyx-davd`) | HTTPS only, integrates with the API auth layer (session or app token); ideal for cloud-sync clients (Nextcloud desktop, RaiDrive) |
| **Rsync** | `rsyncd` via systemd socket | Read/write modules per share, restricted to configured users, chroot-style path containment |

**Exposure policy:** every protocol is **off by default**; enabling it is an explicit,
logged act. The Share detail page shows which protocols expose a given share, with copyable
connection strings (`smb://`, `nfs://`, `davs://`, `rsync://`, `sftp://`).

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

## 9. Performance tuning (defaults, expert-overridable)

- Compression: `zstd:3` on `@data` (best speed/size for mixed media); `zstd:1` on `@apps`.
- Mount options: `noatime,space_cache=v2,ssd` (SSD), `compress-force=zstd` off for
  already-compressed media dirs (per-dataset `nodatacow` where needed).
- Samba: `btrfs` VFS, `aio read size`, socket options defaults; throughput target §02.9.
- The UI never surfaces raw tuning flags by default; a "Expert" toggle in pool settings
  reveals documented options with warnings.
