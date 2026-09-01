# base/ — OSTree image definition

The root filesystem of the Onyx appliance: a minimal Debian Trixie userspace
(`base/debian-trixie/`), composed into an OSTree repository, updated atomically
as A/B snapshots, and served by the bootloader (docs/design/10-installer-and-updates.md).

This directory is a **stub at v0.1**: the design is in
[docs/design/10](../docs/design/10-installer-and-updates.md) and the repo
layout is planned below. The v0.1 "Cinder" milestone builds the OSTree base as
the bootable image deliverable; until then, the services in `services/` run
directly on a host (see `scripts/dev.sh`).

## Planned contents

```
base/
├── debian-trixie/       # seeded rootfs: packages, users, /etc/onyx defaults
│   ├── files/           # static files applied into the rootfs
│   └── postinst.sh      # first-boot seeding (systemd units, keys, dirs)
├── manifest.json        # OSTree compose config (packages, ref, kernel)
├── image/               # bootloader + initramfs assembly (A/B layout)
└── repo/                # ostree repo output (generated, never committed)
```

## References

- [docs/design/02 §1, §2](/docs/design/02-technical-architecture.md) — OSTree read-only A/B root
- [docs/design/10](/docs/design/10-installer-and-updates.md) — installer, updates, rollback
- [docs/design/04 §9](/docs/design/04-backend-service-architecture.md) — systemd units, packaging of `onyx-*` services