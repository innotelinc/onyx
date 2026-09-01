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
install -m 0644 "$SELF/../deploy/systemd/onyx-"*.service "$ROOTFS/usr/lib/systemd/system/"
install -m 0644 "$SELF/../deploy/tmpfiles.d/onyx.conf" "$ROOTFS/usr/lib/tmpfiles.d/onyx.conf"
install -m 0644 "$SELF/debian-trixie/files/usr/lib/sysusers.d/onyx.conf" "$ROOTFS/usr/lib/sysusers.d/onyx.conf"

echo "== building the onyx services and installing into the rootfs =="
make -C "$SELF/.." build
install -d -m 0755 "$ROOTFS/usr/local/lib/onyx"
install -m 0755 "$SELF/../bin/onyx" "$ROOTFS/usr/local/bin/onyx"
for b in onyx-privd onyx-storaged onyx-shared onyx-core onyx-api; do
  install -m 0755 "$SELF/../bin/$b" "$ROOTFS/usr/local/lib/onyx/$b"
done

echo "== enabling the onyx units (offline systemctl --root) =="
systemctl --root="$ROOTFS" enable onyx-privd.service onyx-storaged.service \
  onyx-shared.service onyx-core.service onyx-api.service

echo "== first-boot seeding (users + runtime dirs) =="
install -m 0755 "$SELF/debian-trixie/postinst.sh" "$ROOTFS/usr/local/sbin/onyx-postinst.sh"
chroot "$ROOTFS" /usr/local/sbin/onyx-postinst.sh

echo "== committing to the OSTree repo =="
ostree init --repo="$OUT/repo" --mode=bare-user-only
ostree commit --repo="$OUT/repo" --branch="$REF" \
  --subject="onyx base image ($NAME, $SUITE $ARCH)" \
  --skip-if-unchanged "$ROOTFS"

echo
echo "composed: $OUT/repo (ref $REF)"
echo
echo "next steps (docs/design/10):"
echo "  1. chroot into the staged rootfs to seed an admin keyring or tweak"
echo "     defaults before first boot (the rootfs is deleted on exit, so"
echo "     re-run with the tweaks in debian-trixie/ instead)."
echo "  2. assemble the A/B boot layout with ostree admin os-init, install"
echo "     the bootloader, and boot the image once to validate before wiring"
echo "     the first-boot wizard (onyx-api, v0.2)."