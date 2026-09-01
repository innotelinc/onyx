#!/usr/bin/env bash
# base/image/assemble-boot.sh — assemble a bootable A/B OSTree sysroot from a
# composed rootfs commit (docs/design/10#2, #5).
#
# Input: an OSTree repo produced by base/compose.sh (the commit contains the
# full Debian rootfs + onyx services). Output: base/image/sysroot — the exact
# layout `ostree admin` manages on the appliance disk:
#
#   sysroot/
#   ├── boot/                       # ESP mountpoint -> loader + kernels
#   │   ├── loader.1/
#   │   │   ├── loader.conf         # systemd-boot config (default entry, timeout)
#   │   │   └── entries/            # one BLS .conf per deployment slot
#   │   └── ostree/                 # kernels + initramfs per deployment
#   └── ostree/
#       ├── repo                    # the system OSTree repo (bare)
#       └── deploy/onyx/deploy/     # the two A/B deployment slots (.0, .1)
#
# Two `ostree admin deploy` calls create the A/B pair: deploy #1 is slot B,
# deploy #2 (same ref) is slot A and becomes the boot default. systemd-boot
# lists both; the health gate (onyx-bootcheck + boot counting) falls back to B
# when A fails to boot — the atomic-update, auto-rollback story of the product.
#
# This script runs at *image compose time* to produce and validate the layout;
# the same `ostree admin` sequence runs later on the target disk at install
# time (scripts/onyx-install --disk, docs/design/10#3).
#
# Flags:
#   --repo DIR     composed OSTree repo (default base/repo)
#   --output DIR   target dir for the assembled sysroot (default base/image/sysroot)
#   --os NAME      ostree osname (default onyx)
#   --ref REF      branch to deploy (default onyx/debian-trixie)
#   --clean        wipe the output dir first
set -euo pipefail

SELF="$(cd "$(dirname "$0")/.." && pwd)"
REPO="$SELF/repo"
OUT="$SELF/image/sysroot"
OS="onyx"
REF="onyx/debian-trixie"
CLEAN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --output) OUT="$2"; shift 2 ;;
    --os) OS="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    --clean) CLEAN=1; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//' | grep -v '^!' ; exit 0 ;;
    *) echo "assemble-boot.sh: unknown argument: $1" >&2; exit 2 ;;
  esac
done

require() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "assemble-boot.sh: missing required tool: $1" >&2; exit 1
  }
}
require ostree
[ -d "$REPO" ] || { echo "assemble-boot.sh: no repo at $REPO — run base/compose.sh first" >&2; exit 1; }
[ "$(id -u)" = 0 ] || { echo "assemble-boot.sh: needs root (bare repo + os-init)" >&2; exit 1; }

[ "$CLEAN" = 1 ] && rm -rf "$OUT"
mkdir -p "$OUT/boot" "$OUT/ostree/deploy"

SYSREPO="$OUT/ostree/repo"
echo "== initializing sysroot repo (bare) =="
ostree --repo="$SYSREPO" init --mode=bare

echo "== os-init: creating stateroot '$OS' =="
ostree admin --sysroot="$OUT" os-init "$OS"

echo "== pulling '$REF' from $REPO =="
ostree --repo="$SYSREPO" pull-local --remote=origin "$REPO" "$REF"

echo "== deploy (slot B, previous) =="
ostree admin --sysroot="$OUT" deploy --os="$OS" "$REF"

echo "== deploy (slot A, boot default) =="
ostree admin --sysroot="$OUT" deploy --os="$OS" "$REF"

echo "== bootloader: systemd-boot loader.conf =="
# loader.1 is the current bootversion dir OSTree manages; write loader.conf so
# systemd-boot picks the newest deployment and shows the rollback entry.
LOADER_DIR="$(dirname "$(find "$OUT/boot" -maxdepth 2 -name entries -type d | head -n1)")"
if [ -n "$LOADER_DIR" ]; then
  cat >"$LOADER_DIR/loader.conf" <<EOF
default  auto
timeout  5
console-mode max
editor   0
EOF
  echo "wrote $LOADER_DIR/loader.conf"
else
  echo "WARNING: no BLS entries dir found — did deploy produce /boot? (kernel in the commit?)" >&2
fi

echo "== verification: deployment slots + boot entries =="
ostree admin --sysroot="$OUT" status
echo
entries="$(find "$OUT/boot" -name '*.conf' -path '*/entries/*' | sort)"
if [ -n "$entries" ]; then
  for e in $entries; do echo "boot entry: $e"; done
else
  echo "ERROR: no boot loader entries produced" >&2
  exit 1
fi

echo
echo "assembled A/B sysroot at $OUT"
echo "  slots: $(ostree admin --sysroot="$OUT" status | grep -c '^  onyx') deployment(s) — A (default) + B (rollback)"
echo
echo "next: install this sysroot + a systemd-boot ESP onto the target disk"
echo "  (scripts/onyx-install --disk, or rsync the sysroot to the system partition)"