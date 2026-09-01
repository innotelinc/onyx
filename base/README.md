# base/ — OSTree image definition

The root filesystem of the Onyx appliance: a minimal Debian Trixie userspace
(`base/debian-trixie/`), composed into an OSTree repository, updated atomically
as A/B snapshots, and served by the bootloader (docs/design/10-installer-and-updates.md).

**Status at v0.1: bootable image pipeline is complete and validated.** The
compose script builds the full image and `base/image/assemble-boot.sh` turns
the commit into a bootable A/B sysroot:

```bash
apt install debootstrap ostree python3 btrfs-progs
sudo base/compose.sh          # or: make image
```

The script fails fast with a clear message when a tool is missing; see
`manifest.json` for the exact package set (base + kernel/bootloader + optional
samba/nfs + firmware, `--no-firmware` to skip). It runs the full pipeline:

1. **debootstrap** a minimal Trixie rootfs from the mirror in `manifest.json`,
2. **drop in onyx**: the five daemons + CLI (`bin/`, from `make build`), the
   systemd units + tmpfiles (`deploy/`), the runtime helpers
   (`deploy/libexec/`), and the per-service users (`debian-trixie/files/`),
3. **enable** the onyx units offline via `systemctl --root`,
4. **first-boot seed** users + runtime dirs (`debian-trixie/postinst.sh`),
5. **promote the kernel** into `/usr/lib/modules/<ver>/vmlinuz` where ostree
   deploy expects it (Debian keeps kernels in `/boot`; OSTree does not see them
   there),
6. **commit** the rootfs to `base/repo` (bare-user-only), and
7. **assemble** the bootable A/B sysroot at `base/image/sysroot`
   (`base/image/assemble-boot.sh`): repo init → os-init → pull → deploy twice,
   yielding the two deployment slots (A = boot default, B = rollback) with
   systemd-boot BLS entries and loader.conf.

## Contents

```
base/
├── manifest.json             # compose config: suite, mirror, packages, ref
├── compose.sh                # debootstrap → drop in onyx → kernel promote
│                             #   → ostree commit → A/B boot assembly
├── image/
│   └── assemble-boot.sh      # repo → bootable A/B sysroot (os-init, deploy×2,
│                             #   BLS entries, loader.conf; validated here)
├── debian-trixie/
│   ├── files/
│   │   └── usr/lib/sysusers.d/onyx.conf   # per-service users (same identities
│   │                                     # onyx-install creates on a host)
│   └── postinst.sh           # first-boot seed: users + runtime dirs
├── image/                    # bootloader + initramfs assembly (generated)
└── repo/                     # ostree repo output (generated, never committed)
```

The `deploy/systemd/*.service` units and `deploy/tmpfiles.d/onyx.conf` are
shared verbatim between the host install (`scripts/onyx-install`) and this
image — one runtime layout, two delivery paths. The image additionally carries
`deploy/systemd/onyx-pool.service` (data pool auto-mount),
`onyx-firstboot.service` (first-boot wizard), `onyx-bootcheck.service`
(rollback health gate), and `onyx-updated.service` + timer (A/B update check) —
all wired into the image's enabled-unit set.

## What the validated image gives you (docs/design/10)

- **Bootloader + A/B switching:** `assemble-boot.sh` produces the two OSTree
  deployments and systemd-boot BLS entries; `onyx-bootcheck` + boot counting
  make rollback automatic (failed health check → reboot into previous root).
- **Data pool auto-mount:** `onyx-pool.service` discovers the labelled Btrfs
  pool, mounts it at `/mnt/onyx/<pool>`, and keeps the fixed @data/@apps/
  @backups/@snapshots subvolume layout present.
- **First-boot wizard:** `onyx-firstboot.service` runs once (marker-guarded):
  hostname, admin user + password (kernel cmdline seeds `onyx.*=` or
  interactive), pool layout. The interactive web wizard (2FA, storage tour)
  ships with onyx-api user management in v0.2.
- **Update pipeline:** `onyx-update status|check|apply|rollback` + the daily
  `onyx-update-check.timer`; factory reset via `onyx-factory-reset`
  (system-only by default, `--erase-all` to wipe the pool).

## References

- [docs/design/02 §1, §2](/docs/design/02-technical-architecture.md) — OSTree read-only A/B root
- [docs/design/10](/docs/design/10-installer-and-updates.md) — installer, updates, rollback
- [docs/design/04 §9](/docs/design/04-backend-service-architecture.md) — systemd units, packaging of `onyx-*` services