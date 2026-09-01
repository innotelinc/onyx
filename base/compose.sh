#!/usr/bin/env bash
# base/compose.sh — compose the Onyx OSTree base image (docs/design/10).
#
# Stages a minimal Debian 13 (trixie) rootfs with debootstrap, drops in the
# onyx services (deploy/systemd units, tmpfiles, and the sysusers seed in
# debian-trixie/files), enables the daemons, and commits everything to an
# OSTree repository that the bootloader serves as an atomic A/B root.
#
# This produces the bootable Cinder image deliverable; it needs a Debian 13
# (or similar) build host with the tools below and root (debootstrap +
# user namespaces or chroot). It is intentionally a thin, readable script —
# the authoritative layout is manifest.json.
#
#   apt install debootstrap ostree python3 btrfs-progs
#   sudo base/compose.sh              # writes base/repo + base/image
#
# Flags:
#   --output-dir DIR   where repo/ and image/ land (default base/)
#   --no-firmware      skip the firmware packages (firmware-linux etc.)
#   --manifest PATH    alternate manifest (default manifest.json next to this)
set -euo pipefail

SELF="$(cd "$(dirname "$0")" && pwd)"
MANIFEST="$SELF/manifest.json"
OUT="$SELF"
FIRMWARE=1
while [ $# -gt 0 ]; do
  case "$1" in
    --output-dir) OUT="$2"; shift 2 ;;
    --no-firmware) FIRMWARE=0; shift ;;
    --manifest) MANIFEST="$2"; shift 2 ;;
    -h|--help) exit 0 ;;
    *) echo "compose.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "compose.sh: missing required tool: $1" >&2
    echo "  install with: apt install debootstrap ostree python3 btrfs-progs" >&2
    exit 1
  }
}
require debootstrap
require ostree
require python3

[ "$(id -u)" = 0 ] || { echo "compose.sh: needs root (debootstrap + ostree)" >&2; exit 1; }

# Read the manifest with python3 (Debian's json module; no jq dependency).
read_json() { # $1 = dotted key
  python3 -c "import json,sys; m=json.load(open('$MANIFEST')); v=m
for k in '$1'.split('.'): v=v[k]
print(v if isinstance(v,str) else '\\n'.join(v))"
}

NAME="$(read_json name)"
REF="$(read_json ref)"
SUITE="$(read_json suite)"
MIRROR="$(read_json mirror)"
ARCH="$(read_json arch)"
VARIANT="$(read_json variant)"

mapfile -t PACKAGES < <(read_json packages)
mapfile -t OPTIONAL < <(read_json optional-packages)
if [ "$FIRMWARE" = 1 ]; then
  mapfile -t FIRMWARE_PKGS < <(read_json firmware-packages)
  PACKAGES+=("${FIRMWARE_PKGS[@]}")
fi

STAGE="$(mktemp -d /tmp/onyx-base.XXXXXX)"
trap 'rm -rf "$STAGE"' EXIT
ROOTFS="$STAGE/rootfs"
mkdir -p "$ROOTFS" "$OUT/repo"

echo "== debootstrap $SUITE ($ARCH, $VARIANT) =="
debootstrap --variant="$VARIANT" --arch="$ARCH" \
  --include="$(IFS=,; echo "${PACKAGES[*]}")" \
  "$SUITE" "$ROOTFS" "$MIRROR"

echo "== optional daemon packages (samba, nfs-kernel-server) =="
if [ "${#OPTIONAL[@]}" -gt 0 ]; then
  # Best-effort: only install what the mirror has for the suite.
  chroot "$ROOTFS" apt-get install -y "${OPTIONAL[@]}" \
    || echo "  (some optional packages unavailable; continuing)"
fi

echo "== installing onyx services into the rootfs =="
install -d -m 0755 "$ROOTFS/usr/lib/systemd/system"
install -d -m 0755 "$ROOTFS/usr/lib/tmpfiles.d"
install -d -m 0755 "$ROOTFS/usr/lib/sysusers.d"
install -d -m 0755 "$ROOTFS/etc/onyx/conf.d"
install -m 0644 "$SELF/../deploy/systemd/onyx-"*.service "$SELF/../deploy/systemd/onyx-"*.timer "$ROOTFS/usr/lib/systemd/system/"
install -m 0644 "$SELF/../deploy/tmpfiles.d/onyx.conf" "$ROOTFS/usr/lib/tmpfiles.d/onyx.conf"
install -m 0644 "$SELF/debian-trixie/files/usr/lib/sysusers.d/onyx.conf" "$ROOTFS/usr/lib/sysusers.d/onyx.conf"

echo "== building the onyx services and installing into the rootfs =="
make -C "$SELF/.." build
install -d -m 0755 "$ROOTFS/usr/local/lib/onyx"
install -m 0755 "$SELF/../bin/onyx" "$ROOTFS/usr/local/bin/onyx"
for b in onyx-privd onyx-storaged onyx-shared onyx-core onyx-api; do
  install -m 0755 "$SELF/../bin/$b" "$ROOTFS/usr/local/lib/onyx/$b"
done

echo "== installing onyx runtime helpers (libexec) =="
for h in "$SELF/../deploy/libexec/onyx-"*; do
  install -m 0755 "$h" "$ROOTFS/usr/local/lib/onyx/$(basename "$h")"
done

# A bootable image needs a kernel the bootloader can hand to the A/B entries;
# fail fast instead of shipping an unbootable commit (assemble-boot needs it).
# Debian installs kernels as /boot/vmlinuz-*; OSTree's deploy finds them only
# under /usr/lib/modules/<ver>/ — promote the pair (like kernel-install does).
KERNEL="$(ls "$ROOTFS"/boot/vmlinuz-* 2>/dev/null | head -n1 || true)"
[ -n "$KERNEL" ] && [ -f "$KERNEL" ] || {
  echo "compose.sh: ERROR — no kernel in the rootfs (/boot/vmlinuz-*)," >&2
  echo "  add linux-image-amd64 (+ initramfs-tools) to manifest.json" >&2
  exit 1
}
KVER="$(basename "$KERNEL" | sed 's/^vmlinuz-//')"
MODDIR="$ROOTFS/usr/lib/modules/$KVER"
echo "== promoting kernel for OSTree: $KVER =="
if [ ! -e "$MODDIR/vmlinuz" ]; then
  install -d -m 0755 "$MODDIR"
  cp -a "$KERNEL" "$MODDIR/vmlinuz"
fi
INITRD="$(ls "$ROOTFS"/boot/initrd.img-* 2>/dev/null | head -n1 || true)"
if [ -n "$INITRD" ] && [ ! -e "$MODDIR/initramfs.img" ]; then
  cp -a "$INITRD" "$MODDIR/initramfs.img"
else
  echo "compose.sh: WARNING — no initramfs in the rootfs; deploy will still work but boot needs one (install initramfs-tools)" >&2
fi

echo "== enabling the onyx units (offline systemctl --root) =="
systemctl --root="$ROOTFS" enable onyx-privd.service onyx-storaged.service \
  onyx-shared.service onyx-core.service onyx-api.service \
  onyx-pool.service onyx-firstboot.service onyx-bootcheck.service \
  onyx-updated.service onyx-update-check.timer

echo "== first-boot seeding (users + runtime dirs) =="
install -m 0755 "$SELF/debian-trixie/postinst.sh" "$ROOTFS/usr/local/sbin/onyx-postinst.sh"
# chroot needs the kernel pseudo-filesystems (systemd-sysusers/tmpfiles read
# /proc; apt postinst hooks touch /dev). Mount them, run, and always unmount.
mount --bind /proc "$ROOTFS/proc" 2>/dev/null || true
mount --bind /sys "$ROOTFS/sys" 2>/dev/null || true
mount --bind /dev "$ROOTFS/dev" 2>/dev/null || true
mount --bind /run "$ROOTFS/run" 2>/dev/null || true
chroot "$ROOTFS" /usr/local/sbin/onyx-postinst.sh || {
  umount "$ROOTFS/run" "$ROOTFS/dev" "$ROOTFS/sys" "$ROOTFS/proc" 2>/dev/null || true
  exit 1
}
umount "$ROOTFS/run" "$ROOTFS/dev" "$ROOTFS/sys" "$ROOTFS/proc" 2>/dev/null || true

echo "== ostree-ifying /etc (shipped config moves to /usr/etc) =="
# OSTree trees must not carry a mutable /etc in the commit: shipped config
# lives in /usr/etc and is merged into the deployment's /etc at deploy time
# (the empty /etc dir in the tree marks the merge point). This is what the
# assembler's "Preparing /etc" error means without it. The sysusers/tmpfiles
# seed from postinst already wrote into the chroot /etc — moving it now ships
# those identities as the image defaults.
mkdir -p "$ROOTFS/usr/etc"
if [ -d "$ROOTFS/etc" ]; then
  find "$ROOTFS/etc" -mindepth 1 -maxdepth 1 -exec mv -t "$ROOTFS/usr/etc" {} + 2>/dev/null || true
fi
rm -rf "$ROOTFS/etc"
mkdir -p "$ROOTFS/etc"

# The image ships its own os-release: Debian's /etc/os-release is a *relative*
# symlink (../usr/lib/os-release) that dangles once the tree is ostree-ified
# (from /usr/etc, ".." resolves to /usr, not /). It must also be a real file,
# not a link, because the bootloader reads it for the BLS entry title — so drop
# whatever was moved and write a fresh one.
rm -f "$ROOTFS/usr/etc/os-release"
printf 'ID=onyx\nNAME="Onyx"\nVERSION_ID=0.1\nPRETTY_NAME="Onyx 0.1 (Cinder)"\nHOME_URL="https://github.com/innotelinc/onyx"\n' >"$ROOTFS/usr/etc/os-release"

echo "== cleaning runtime pseudo-filesystems before commit =="
# debootstrap's second stage leaves device nodes in /dev (and may create /proc,
# /sys, /run). OSTree cannot commit those — and they must not ship anyway:
# devtmpfs/udev, procfs, sysfs and tmpfs recreate them at boot. An empty dir is
# enough for the commit (systemd mounts over it).
for d in dev proc sys run tmp; do
  find "$ROOTFS/$d" -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true
done

# OSTree images expect an empty /dev and /tmp at commit time.
mkdir -p "$ROOTFS/dev" "$ROOTFS/tmp"

# Re-flush the boot dir (assemble-boot deploys from this commit; the kernel
# promote step earlier wrote /usr/lib/modules/<ver>/vmlinuz, which is inside
# the tree and unaffected).
echo "== committing to the OSTree repo =="
ostree init --repo="$OUT/repo" --mode=bare-user-only 2>/dev/null || true
ostree commit --repo="$OUT/repo" --branch="$REF" \
  --subject="onyx base image ($NAME, $SUITE $ARCH)" \
  --skip-if-unchanged "$ROOTFS"

echo "== assembling the bootable A/B sysroot ($OUT/image/sysroot) =="
bash "$SELF/image/assemble-boot.sh" --repo "$OUT/repo" --ref "$REF" \
  --output "$OUT/image/sysroot" --clean

echo
echo "composed: $OUT/repo (ref $REF) + bootable sysroot at $OUT/image/sysroot"
echo
echo "next steps (docs/design/10):"
echo "  1. flash the sysroot + a systemd-boot ESP to the target disk"
echo "     (scripts/onyx-install --disk DEV after adding --disk mode, or rsync"
echo "     the sysroot to the system partition) — first boot runs onyx-firstboot."
echo "  2. updates & rollback: onyx-update check/apply/rollback + boot counting"
echo "     via onyx-bootcheck (docs/design/10#5)."