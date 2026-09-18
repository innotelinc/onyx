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

- the host path must be a shared **mount point**. It is a plain directory on a
  fresh install, and a directory that is not a mount point cannot be made
  shared — `mount --make-shared /mnt/onyx` fails with exit 32 ("not mount point
  or bad option") — so it is bound to itself in the same call:
  `mount -o bind,shared /mnt/onyx /mnt/onyx`. That binding is recorded in
  `/etc/fstab` as `/mnt/onyx /mnt/onyx none bind,shared 0 0`, because
  propagation is lost on reboot and the symptom is an empty Files view rather
  than an error. `setup.sh` makes it shared and prints the fstab line;
  `onyx-firstboot` does both, and `scripts/e2e-stack.sh` fails the run when the
  propagation is not `shared`;
- the compose bind mounts use `:rshared`.

Get one half right and the failure is silent and confusing: privd mounts the
pool, the data plane reports it as mounted, and the API's storage root is an
empty directory — which reads as "No storage is mounted" in Files. `setup.sh`
checks and fixes the host half, and `GET /storage/overview` reports the
mismatch (per-entry `visible` flag plus an operator-actionable warning) instead
of pretending there is nothing there.

### 2.2 Which devices can become a pool

Any **whole device** — a whole disk, or a loop device (a file-backed scratch
disk) — and never a partition: a pool owns the device it is created on. There is
no requirement that the device be *removable*: on most machines the only disks
present are internal SATA/NVMe/SCSI/virtio devices, and gating creation on the
hotplug flag would leave an operator with nothing to create a pool on.

What protects the running system is not the hardware flag but what is
**mounted**, which is checked twice:

- `onyx-storaged` refuses a device whose filesystem (or whose partition's, or a
  swap/LVM/RAID holder of it) is mounted anywhere outside the storage root, and
  reports the mountpoint so the operator can decide — except under the forced
  "erase this disk" path, where the holders are released instead (§2.3);
- `onyx-privd` independently refuses to release a mount outside the storage root
  before formatting.

So an OS disk fails with a plain-language reason, and a disk with nothing
mounted on it has nothing to release and formats. Re-creating a pool in place is
the same path with `force`: Onyx's own mount is released, the device is wiped and
re-mounted under the (possibly new) mount name.

The UI labels each offered device — *mounted by Onyx* (will be re-created),
*internal disk*, *loop device* — so the destructive step is a deliberate one, and
never offers a device the host has mounted outside `/mnt/onyx`.

A third check is about the device node rather than the device: `lsblk` reads
`/sys`, so a disk can be listed while `/dev/<kname>` does not exist in the data
plane's mount namespace. That happens in a nested or locked-down container (and
in any deployment that does not pass block devices through), and without a check
the operator picks a disk and the failure arrives *after* the destructive
confirmation, in mkfs's words: `The file /dev/sda does not exist and no size was
specified`. Both layers therefore require the node: `onyx-storaged` refuses the
device before releasing anything and reports it as `node_present = false` in the
device list (so the pool dialog hides it and says why), and `onyx-privd` checks
again before running `wipefs`/`mkfs`/`mount`.

### 2.3 Re-creating a pool on a used disk ("force erase")

Pool creation is destructive, and the disk was almost always used for something
before. `POST /pools` therefore defaults to `force` + `auto_mount`: the operator
picked the disk in the UI, the UI states that its contents are erased, and the
rest is Onyx's job rather than a scavenger hunt through `lsblk` output. A format
that arrives at `mke2fs`/`mkfs.btrfs` still busy fails in the tools' words —
`/dev/sdc is apparently in use by the system; will not make a filesystem here!`
and `ERROR: unable to open /dev/sdc: Resource busy` — which say nothing about
what is holding the disk or where to go next.

`onyx-privd` releases the disk itself, in order, before it wipes anything:

1. **Kernel stacks** — an active swap area on the disk or any of its partitions
   (`swapoff`), a device-mapper mapping stacked on it (`dmsetup remove`), an md
   array (`mdadm --stop`), a loop device (`losetup -d`). This is the step an
   unmount sweep cannot cover: swap and dm/md holders survive every `umount`,
   and they are the usual reason `mke2fs` calls a disk "in use by the system".
2. **Mounts** — every mount of the disk *and of its partitions*, swept twice
   (a nested mount only appears once its outer one is gone), with `umount`
   escalating to `-f` then `-l` for a lingering handle. A mount outside the
   storage root is never touched: it is reported and the operation stops.
3. **Signatures** — `wipefs -a`, then the format itself, retried while the
   kernel finishes releasing the device.

Every release is named in the response (`released dm-0`, `deactivated swap on
/dev/sdc1`), and anything that is still holding the disk afterwards is reported
by name. `onyx-storaged` applies the same policy one layer up: with `force` a
`[SWAP]` partition is released by privd rather than refusing the whole
operation, because a disk that came out of an old NAS with a swap partition is
the normal case, not an error. Any *other* bracketed holder (`[RAID]` and the
like) is still refused with the reason.

### 2.4 Pool root permissions

A freshly formatted ext4/btrfs root is `root:root 0755`. That makes a new pool
**read-only for everything that is supposed to use it**: `onyx-davd` and the
SFTP/FTP/rsync daemons run as ordinary uids, containerised apps write as their
image's user, the object-store tier target writes as `onyx-objectstore`, and
SFTP/FTP share users are deliberately *not* in the `onyx` group. So after a
successful pool mount, `onyx-privd` sets the root of that filesystem to
`--pool-mode` (default `0777`). Per-user and per-share access control belongs to
the protocol layer (SMB/NFS authentication, share ACLs, share ownership), not to
the mode of the volume root.

A deployment that serves a narrower protocol surface can tighten it —
`--pool-mode 2770` with group `onyx`, matching the systemd install's own
`/mnt/onyx` (`2770 root:onyx`, `deploy/tmpfiles.d/onyx.conf`) — at the cost of
SFTP/FTP/WebDAV and app writes, which then have no write access. The failure is
reported, not silent: privd logs the mode it applied and returns a note when it
cannot apply it.

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
| **WebDAV** | Go WebDAV server (`onyx-davd`, `services/davd`) | HTTPS only, integrates with the API auth layer (session or app token); ideal for cloud-sync clients (Nextcloud desktop, RaiDrive). Serves `/webdav/<share>` on loopback, renders no TLS itself and authenticates nobody itself — it requires the gateway's identity header (`X-Onyx-User`), so a request that bypassed the gateway is rejected rather than trusted. NPM provisions the matching location (`WEBDAV_SUBDOMAIN`, default the console host) and maps Authentik's forward-auth username onto that header, which is what makes the connection string the Shares page copies out a URL that works. Before any share enables WebDAV the rendered `davd.conf` does not exist yet, which the systemd unit gates on (`ConditionPathExists`); the daemon itself serves an empty share table and picks the file up when it appears, so a containerized deployment does not restart-loop waiting for a config it cannot condition on |
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
