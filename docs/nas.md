# ONYX NAS development branch

`dev/nas` is the NAS development line. It includes the cloud account and removable
drive fixes from the previous work, with storage, file sharing and backups as the
default workflow. The previous branch remains unchanged.

## Default NAS services

The native `--profile nas` install includes privd, storaged, shared, core, API,
snapd, backupd and davd, with the CLI and web console. Samba, NFS, SFTP, FTP and
rsync are host protocol daemons installed by `--install-deps`; share configuration
is rendered through the existing control plane. WebDAV starts when configured.
Apps, VMs, AI and object storage are omitted from this service set. Use
`--profile platform` to install the extended stack. Existing platform services
are not automatically removed or stopped when changing profiles.

## Linux host installation

Use a Debian-family host with systemd and real device access. Build the project,
then run `sudo bash scripts/onyx-install --profile nas --install-deps`. This installs
dependencies, state directories, binaries, service units and console assets.
The admin console is served by the API on loopback port 8080; it is not a
publicly authenticated admin server. Use an SSH tunnel or authenticated HTTPS
ingress for remote administration. Keep the admin/API port off the public Internet.
Sharing protocols are configured separately for trusted LAN clients.

Use `--enable-pool` for the existing labeled Btrfs pool boot mount, and
`--enable-firstboot` only when the console first-boot wizard is wanted. Installation
does not format a disk. For package inspection without host changes:

```bash
bash scripts/onyx-install --profile nas --destdir /tmp/onyx-nas-package
python3 scripts/nas-install-check.py
```

The API owns `/var/lib/onyx/api`; file operations and backup writes have access
to `/mnt/onyx`. The native rclone catalog is owned by onyx-api with group onyx,
mode 0640, so backupd can read credentials without exposing them to other users.
Docker uses the shared container UID and its existing private catalog helper.

## Storage and backups

1. In Storage, attach an existing disk, USB stick or USB HDD. Attach preserves
   its filesystem; creating a new pool is a separate formatting operation.
2. Browse mounted data in Files. Create folders and import files there.
3. Create a network share and configure user permissions and Samba passwords
   where required. Use the connection addresses shown for Windows, macOS or Linux.
4. Configure local or cloud backup destinations and create backup jobs. Google
   Cloud Storage takes a service-account JSON key; Google Drive and OneDrive use
   authorization. iCloud requires Apple verification and its documented backend
   limitations. Cloud remotes are backup/clone targets, not locally mounted disks.

USB mounting needs filesystem support on the host. NTFS/exFAT tools are installed;
the kernel still supplies exFAT support. Btrfs snapshots require Btrfs storage.
Cloud access requires actual credentials and provider permissions.
For iCloud, rclone must be at least 1.69. The installer reuses `.tools/bin/rclone`
when the development environment has prepared it; otherwise check the distro
version and install a current, verified rclone release before connecting iCloud.

## Development and validation

`ONYX_AUTO_ATTACH=none scripts/dev.sh start` runs the native development services
without automatically mounting physical disks. The API serves the NAS console
and API on one origin. Repo-local dependencies live in `.tools`; see the root
README for bootstrap and build commands.

Run `make check`, `python3 scripts/nas-install-check.py`, and the console smoke
check `node scripts/web-ui-check.js`. Package checks exercise both profiles and
pool/firstboot flags. UI checks cover NAS navigation, optional platform navigation,
cloud credentials and health accounting. Real USB and network-protocol tests
require a suitable Linux host; cloud transfer tests require the account owner.

Docker's default service set also excludes the optional platform daemons.
Use `ONYX_MODE=platform ./setup.sh` or explicit `--profile platform` to enable
them. The web container proxies `/api/` to the API so the console works at a LAN
hostname/address, preserving HTTPS scheme information from a trusted ingress.
The Docker stack does not package the host Samba/NFS daemons; the native install
is the recommended NAS deployment.
