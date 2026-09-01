# installer/ — ISO, SBC images, script installer

Installation targets and flows are specced in
[docs/design/10-installer-and-updates.md](../docs/design/10-installer-and-updates.md):

1. **Script installer** (`scripts/onyx-install`) — for existing Linux hosts: wipe
   a target disk, create the Btrfs data pool + system partitions, deploy the
   OSTree commit, install the bootloader.
2. **SBC images** — prebuilt SD/eMMC images (RPi 5, RK3588 boards).
3. **ISO** — for x86 mini-PCs.

**Status at v0.1: script installer shipped, image builders still stubs.** The
script installer ([`scripts/onyx-install`](../scripts/onyx-install)) turns an
existing Debian-family host into an Onyx box: it builds, creates the
unprivileged service users + runtime dirs, installs the binaries, systemd
units and tmpfiles, and enables/starts the stack. What does not exist yet is
the OSTree base (`../base/`) — the target-disk wiping, A/B image layout and
bootloader install — because that base image doesn't exist yet.
Design decisions that will matter when the image builders are built (from doc 10):

- System on a small separate disk/SD; data pool on the big disk(s), Btrfs.
- A/B system partitions with an atomic `ostree` root and automatic rollback on
  failed boot health check.
- First-boot wizard runs from `onyx-api` (set admin user, optional passphrase
  for the keyring, network, update channel).
- Factory reset wipes system partitions only; the data pool is untouched unless
  the user explicitly chooses otherwise.

## Planned layout

```
installer/
├── scripts/onyx-install      # script installer (target: existing Linux)
├── sbc/                      # image build scripts (debootstrap + ostree)
├── iso/                      # ISO build scripts
└── common/                   # partition layout, bootloader config, first-boot seed
```