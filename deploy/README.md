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
│                                 # platform daemons (docs/design/11)
├── systemd/onyx-davd.service         # WebDAV shares (v0.4 protocol surface)
├── systemd/onyx-sftp.service         # dedicated sshd for SFTP shares
├── systemd/vsftpd.service.d/onyx.conf    # drop-in: FTP from /etc/onyx/conf.d
├── systemd/rsyncd.service.d/onyx.conf    # drop-in: rsync from /etc/onyx/conf.d
├── systemd/onyx-pool.service         # data pool auto-mount (before the stack)
├── systemd/onyx-firstboot.service    # first-boot wizard (once, marker-guarded)
├── systemd/onyx-bootcheck.service    # rollback health gate (after API)
├── systemd/onyx-updated.service      # A/B update check (appliance only)
├── systemd/onyx-update-check.timer   # daily update check
└── tmpfiles.d/onyx.conf      # /run/onyx, /mnt/onyx, /var/lib/onyx/*,
                              # /etc/onyx/conf.d, /etc/onyx/conf.d/sftp
                              # (recreated on boot)
```

## Unit graph

| Unit | User | Talks to | Notable flags |
|------|------|----------|---------------|
| `onyx-privd.service` | root | (the privilege boundary) | `--config-dir /etc/onyx/conf.d` |
| `onyx-storaged.service` | `onyx-storaged` (group onyx) | privd | `--mount-root /mnt/onyx` |
| `onyx-shared.service` | `onyx-shared` (group onyx) | — | stateless renderer |
| `onyx-core.service` | `onyx-core` (group onyx) | storaged, shared, privd | state at `/var/lib/onyx/core` |
| `onyx-api.service` | `onyx-api` (group onyx) | core | `--listen 127.0.0.1:8080` (loopback only) |
| `onyx-vmm.service` | `onyx-vmm` (groups onyx, libvirt, kvm) | libvirt (`virsh`, `qemu-img`) | `--disk-root /mnt/onyx/main-pool/@apps/vms` |
| `onyx-appd.service` | `onyx-appd` (group onyx) | the Docker engine | installed apps run as `onyx-app-<app>` compose projects |
| `onyx-objectstore.service` | `onyx-objectstore` (group onyx) | rclone remotes | `--rclone-config /etc/rclone/rclone.conf` (CLOUD/TIERED buckets) |
| `onyx-davd.service` | `onyx-davd` (group onyx) | — | reads `/etc/onyx/conf.d/davd.conf`; requires the gateway's identity header |
| `onyx-pool.service` | root (oneshot) | — | mounts the labelled pool + ensures subvolumes |
| `onyx-firstboot.service` | root (oneshot) | pool | runs once; prompts on console |
| `onyx-bootcheck.service` | root (oneshot) | api | reboots into previous deployment on failure |
| `onyx-updated.service` | `onyx-core` (oneshot) | — | `onyx-update check`; only runs with a sysroot |
| `onyx-davd.service` | `onyx-davd` (group onyx) | HTTP on loopback, gRPC Health | `--config /etc/onyx/conf.d/davd.conf`; SIGHUP reloads shares |
| `onyx-sftp.service` | root (sshd drops privileges) | — | dedicated sshd `-f /etc/onyx/conf.d/sshd_config`, Port 2222 |
| `vsftpd.service.d/onyx.conf` | distro unit | — | repointed at `/etc/onyx/conf.d/vsftpd.conf` |
| `rsyncd.service.d/onyx.conf` | distro unit | — | repointed at `/etc/onyx/conf.d/rsyncd.conf` |

### Protocol daemons start with their first share

`onyx-davd`, `onyx-sftp`, `vsftpd` and `rsyncd` all serve a config file that
onyx-core renders, so each one is gated on that file existing
(`ConditionPathExists=`): the unit is enabled, but inactive until a share
enables its protocol. `onyx-privd` brings it up at that moment with
`systemctl reload-or-restart`, which starts a stopped daemon and reloads a
running one — the operator never has to enable a protocol's daemon by hand.
The distribution's `vsftpd`/`rsync` units are repointed by the drop-ins above,
and their own conditions are reset first so `/etc/vsftpd.conf` /
`/etc/rsyncd.conf` are no longer consulted.

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
user opts in (`scripts/onyx-install --install-deps`). The v0.4 protocol
daemons follow the same rule: they exist only while a share enables them, and
`onyx-davd` additionally refuses any request that did not arrive through the
gateway (it authenticates nobody itself, it trusts the identity header the
gateway sets).

## Install / uninstall

```bash
sudo scripts/onyx-install            # build → users → dirs → files → enable+start
sudo scripts/onyx-install --enable-pool       # + data pool auto-mount
sudo scripts/onyx-install --enable-firstboot  # + first-boot wizard on next boot
scripts/onyx-install --destdir /tmp/stage   # stage files only (packaging, tests)
sudo scripts/onyx-install --uninstall       # stop, disable, remove
```