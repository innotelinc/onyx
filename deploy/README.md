# deploy/ — systemd units + runtime layout

This is the native (non-Docker) packaging for the running system. The
containerized platform deployment lives in [`docker-compose.yml`](../docker-compose.yml)
and [`setup.sh`](../setup.sh) instead; the units here serve the bare-metal
appliance install (`scripts/onyx-install`) and the OSTree image (`base/`).

This is the packaging for the running system (docs/design/04#9-packaging): one
systemd unit per service, a tmpfiles.d snippet that recreates the runtime
paths on every boot, the runtime helper scripts (`libexec/`), and — the entry
point — the script installer in [`scripts/onyx-install`](../scripts/onyx-install).

## Layout

```
deploy/
├── libexec/                    # runtime helpers (installed to <prefix>/lib/onyx/)
│   ├── onyx-pool               # data pool auto-mount + fixed subvolume layout
│   ├── onyx-firstboot          # first-boot wizard (hostname, admin user, pool)
│   ├── onyx-bootcheck          # A/B rollback health gate
│   ├── onyx-update             # A/B update: status|check|apply|rollback
│   └── onyx-factory-reset      # system-only reset (--erase-all for the pool)
├── systemd/onyx-{privd,storaged,shared,core,api}.service   # the core daemons
├── systemd/onyx-{snapd,backupd,vmm,appd,ai,objectstore}.service
│                                 # platform daemons (v0.1 skeletons, docs/design/11)
├── systemd/onyx-pool.service         # data pool auto-mount (before the stack)
├── systemd/onyx-firstboot.service    # first-boot wizard (once, marker-guarded)
├── systemd/onyx-bootcheck.service    # rollback health gate (after API)
├── systemd/onyx-updated.service      # A/B update check (appliance only)
├── systemd/onyx-update-check.timer   # daily update check
└── tmpfiles.d/onyx.conf      # /run/onyx, /mnt/onyx, /var/lib/onyx/*,
                              # /etc/onyx/conf.d (recreated on boot)
```

## Unit graph

| Unit | User | Talks to | Notable flags |
|------|------|----------|---------------|
| `onyx-privd.service` | root | (the privilege boundary) | `--config-dir /etc/onyx/conf.d` |
| `onyx-storaged.service` | `onyx-storaged` (group onyx) | privd | `--mount-root /mnt/onyx` |
| `onyx-shared.service` | `onyx-shared` (group onyx) | — | stateless renderer |
| `onyx-core.service` | `onyx-core` (group onyx) | storaged, shared, privd | state at `/var/lib/onyx/core` |
| `onyx-api.service` | `onyx-api` (group onyx) | core | `--listen 127.0.0.1:8080` (loopback only) |
| `onyx-pool.service` | root (oneshot) | — | mounts the labelled pool + ensures subvolumes |
| `onyx-firstboot.service` | root (oneshot) | pool | runs once; prompts on console |
| `onyx-bootcheck.service` | root (oneshot) | api | reboots into previous deployment on failure |
| `onyx-updated.service` | `onyx-core` (oneshot) | — | `onyx-update check`; only runs with a sysroot |

`Requires=`/`After=` chain the startup order pool → privd → storaged+shared →
core → api; every daemon has `Restart=always` so transient failures self-heal.
All non-privd units run with `NoNewPrivileges` + strict-ish sandboxing; the
sockets in `/run/onyx` are group-owned (`/run/onyx` is `2770 root:onyx` and
every unit uses `UMask=0002`), so per-service users can reach each other over
gRPC without ever being root.

## Why the default install binds the API to loopback

docs/design/07: no surface on the wire until the user enables it. A NAS that
silently exposes `smbd`/`nfsd`/an admin API is how home networks get pwned;
the install deliberately requires an explicit change (edit
`deploy/systemd/onyx-api.service`'s `--listen`, or a later per-interface
policy) before anything listens off-loopback. The generated `smb.conf` and
`exports` are written but the daemons themselves aren't started by onyx — the
host's package manager owns `samba`/`nfs-kernel-server`, started only when the
user opts in (`scripts/onyx-install --install-deps`).

## Install / uninstall

```bash
sudo scripts/onyx-install            # build → users → dirs → files → enable+start
sudo scripts/onyx-install --enable-pool       # + data pool auto-mount
sudo scripts/onyx-install --enable-firstboot  # + first-boot wizard on next boot
scripts/onyx-install --destdir /tmp/stage   # stage files only (packaging, tests)
sudo scripts/onyx-install --uninstall       # stop, disable, remove
```