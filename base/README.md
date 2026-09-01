# base/ — OSTree image definition

The root filesystem of the Onyx appliance: a minimal Debian Trixie userspace
(`base/debian-trixie/`), composed into an OSTree repository, updated atomically
as A/B snapshots, and served by the bootloader (docs/design/10-installer-and-updates.md).

**Status at v0.1: compose scaffold, not yet a validated boot image.** The
services run directly on a host via `scripts/dev.sh` or install for real as
systemd units via `scripts/onyx-install`. What exists here is the *build*
itself — [`compose.sh`](compose.sh) stages the rootfs, drops in the onyx
daemons, and commits an OSTree ref — ready to run on a Debian 13 build host:

```bash
apt install debootstrap ostree python3 btrfs-progs
sudo base/compose.sh          # or: make image
```

The script fails fast with a clear message when a tool is missing; see
`manifest.json` for the exact package set (base + optional samba/nfs +
firmware, `--no-firmware` to skip).

## Contents

```
base/
├── manifest.json             # compose config: suite, mirror, packages, ref
├── compose.sh                # debootstrap → drop in onyx → systemctl enable
│                             #   → ostree commit (the Cinder image deliverable)
├── debian-trixie/
│   ├── files/
│   │   └── usr/lib/sysusers.d/onyx.conf   # per-service users (same identities
│   │                                     # onyx-install creates on a host)
│   └── postinst.sh           # (planned) first-boot seed; admin wizard ships
│                             # with onyx-api in v0.2 per docs/design/10#2
├── image/                    # bootloader + initramfs assembly (generated)
└── repo/                     # ostree repo output (generated, never committed)
```

The `deploy/systemd/*.service` units and `deploy/tmpfiles.d/onyx.conf` are
shared verbatim between the host install (`scripts/onyx-install`) and this
image — one runtime layout, two delivery paths.

## What a validated image still needs

These are the known gaps before `ostree admin os-init` + boot can be called
done (they track docs/design/10 and the installer README):

- **Bootloader + A/B switching** (`base/image/`): systemd-boot/grub layout
  with the two OSTree deployments and rollback-on-bad-boot.
- **Data pool auto-mount**: a systemd-gpt-auto or fstab/`btrfs device scan`
  unit that brings up the data pool at `/mnt/onyx` on boot.
- **First-boot wizard**: sets the admin user + keyring from `onyx-api`
  (v0.2, with user management).
- **Factory reset / update pipeline** (docs/design/10#3): A/B commit swap and
  the `onyx-updated` service.

## References

- [docs/design/02 §1, §2](/docs/design/02-technical-architecture.md) — OSTree read-only A/B root
- [docs/design/10](/docs/design/10-installer-and-updates.md) — installer, updates, rollback
- [docs/design/04 §9](/docs/design/04-backend-service-architecture.md) — systemd units, packaging of `onyx-*` services