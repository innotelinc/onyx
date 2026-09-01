# installer/ — ISO, SBC images, script installer

Installation targets and flows are specced in
[docs/design/10-installer-and-updates.md](../docs/design/10-installer-and-updates.md):

1. **Script installer** (`scripts/onyx-install`) — for existing Linux hosts:
   build → users → dirs → units → enable+start, with optional `--enable-pool`
   (data pool auto-mount) and `--enable-firstboot` (first-boot wizard).
2. **SBC images** — prebuilt SD/eMMC images (RPi 5, RK3588 boards); the image
   pipeline in [`../base/`](../base/) (compose → OSTree → A/B sysroot) is the
   foundation these will flash.
3. **ISO** — for x86 mini-PCs; the same OSTree sysroot can be staged onto a
   target disk with `ostree admin` (the sequence `base/image/assemble-boot.sh`
   performs and validates).

**Status at v0.1: script installer shipped; image pipeline complete.** The
script installer turns an existing Debian-family host into an ONYX box: it
builds, creates the unprivileged service users + runtime dirs, installs the
binaries, systemd units, tmpfiles **and runtime helpers** (`deploy/libexec/`),
and enables/starts the stack. The OSTree base (`../base/`) now composes a
bootable A/B image: `base/compose.sh` produces the repo, `assemble-boot.sh`
produces the two-slot sysroot with BLS entries, and the image carries the pool
auto-mount, first-boot wizard, boot-health rollback gate and update check
units. The remaining gap is the guided/ISO front end (partitioning +
bootloader install onto a chosen disk), which builds on the exact `ostree admin`
sequence the assembler already validates.

Design decisions that matter for the image builders (from doc 10):

- System on a small separate disk/SD; data pool on the big disk(s), Btrfs.
- A/B system partitions with an atomic `ostree` root and automatic rollback on
  failed boot health check (implemented: `onyx-bootcheck` + boot counting).
- First-boot wizard runs once from `onyx-firstboot.service` (implemented:
  hostname, admin user/password, pool layout); the interactive web wizard
  (2FA, storage tour) lands with onyx-api user management (v0.2).
- Factory reset wipes system partitions only; the data pool is untouched unless
  the user explicitly chooses otherwise (`onyx-factory-reset --erase-all`).

## Planned layout

```
installer/
├── scripts/onyx-install      # script installer (target: existing Linux)
├── sbc/                      # image build scripts (debootstrap + ostree)
├── iso/                      # ISO build scripts
└── common/                   # partition layout, bootloader config, first-boot seed
```