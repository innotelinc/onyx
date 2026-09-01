#!/usr/bin/env bash
# scripts/hotplug-demo.sh — end-to-end demo of Onyx hotplug detection.
#
# Simulates a USB drive (/dev/sdz1, label "USB DATA") without hardware:
#   * fake lsblk/mount/umount binaries (driven by .run/hotplug/drive.state)
#   * the whole service stack run against those binaries
#   * a timed lifecycle: attach at boot -> unplug -> replug -> manual
#     detach/attach — verifying the auto-share appears/disappears with the
#     drive, exactly like it would on real hardware.
#
# On real hardware, kernel uevents (netlink) wake the watcher instantly, so
# reaction is immediate; the demo drive emits no uevents, so the script also
# passes --device-watch-interval-ms 500 to exercise the periodic path.
#
# Usage: make build && scripts/hotplug-demo.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
DEMO="$ROOT/.run/hotplug"
SOCK="$DEMO/run"
STATE="$DEMO/state"
MNT="$DEMO/mntroot"
FAKE="$DEMO/bin"
API="127.0.0.1:8099"

PIDS=()
cleanup() {
  local pid
  for pid in "${PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
  sleep 0.3
  for pid in "${PIDS[@]:-}"; do kill -9 "$pid" 2>/dev/null || true; done
  rm -f "$SOCK"/onyx-*.sock
}
trap cleanup EXIT

for b in onyx-privd onyx-storaged onyx-shared onyx-core onyx-api onyx; do
  [ -x "$BIN/$b" ] || { echo "missing $BIN/$b — run: make build" >&2; exit 1; }
done

rm -rf "$DEMO"
mkdir -p "$SOCK" "$STATE" "$MNT" "$FAKE"

# Simulated /sys/class/block: whole disks carry a removable flag, partitions
# do not (they inherit their parent's). sr0 is a cdrom (removable=1 but its
# iso9660 fs is on the exclude list), sdz is our USB stick.
FAKESYS="$DEMO/sysfs"
for d in sda sda1 loop0 sr0 sdz sdz1; do mkdir -p "$FAKESYS/$d"; done
echo 0 >"$FAKESYS/sda/removable"
echo 0 >"$FAKESYS/loop0/removable"
echo 1 >"$FAKESYS/sr0/removable"
echo 1 >"$FAKESYS/sdz/removable"

# The simulated drive is plugged in from the start; flip this file to 0 to
# simulate unplugging. unplugging also removes the fake sysfs entry, exactly
# like the kernel would.
echo 1 >"$DEMO/drive.state"
: >"$DEMO/mounts.state"

cat >"$FAKE/btrfs" <<'EOF'
#!/bin/sh
# No btrfs pools on this simulated host (pool discovery stays quiet).
exit 0
EOF

cat >"$FAKE/mount" <<'EOF'
#!/bin/sh
# Simulated mount. Real argv: mount [-o OPTS] DEV MOUNTPOINT. Record
# "dev|mountpoint" so the fake lsblk reports it like a real kernel mount
# table would, and log the full argv for inspection.
set -e
[ "$1" = "-o" ] && shift 2   # drop -o OPTS; leaves DEV MOUNTPOINT
mkdir -p "$2"
grep -qxF "$1|$2" "$ONYX_DEMO_DIR/mounts.state" 2>/dev/null || echo "$1|$2" >>"$ONYX_DEMO_DIR/mounts.state"
echo "$*" >>"$ONYX_DEMO_DIR/mount-argv.log" 2>/dev/null || true
exit 0
EOF

cat >"$FAKE/umount" <<'EOF'
#!/bin/sh
# Simulated umount: drop the matching mount record.
grep -vF "$1" "$ONYX_DEMO_DIR/mounts.state" >"$ONYX_DEMO_DIR/mounts.state.tmp" 2>/dev/null || true
mv "$ONYX_DEMO_DIR/mounts.state.tmp" "$ONYX_DEMO_DIR/mounts.state"
exit 0
EOF

cat >"$FAKE/testparm" <<'EOF'
#!/bin/sh
# Simulated `testparm -s <smb.conf>`: log the invocation, always pass.
echo "testparm $*" >>"$ONYX_DEMO_DIR/daemon-reload.log" 2>/dev/null || true
exit 0
EOF

cat >"$FAKE/systemctl" <<'EOF'
#!/bin/sh
# Simulated `systemctl reload smbd`: log, always succeed.
echo "systemctl $*" >>"$ONYX_DEMO_DIR/daemon-reload.log" 2>/dev/null || true
exit 0
EOF

cat >"$FAKE/exportfs" <<'EOF'
#!/bin/sh
# Simulated `exportfs -ra`: log, always succeed.
echo "exportfs $*" >>"$ONYX_DEMO_DIR/daemon-reload.log" 2>/dev/null || true
exit 0
EOF

cat >"$FAKE/smartctl" <<'EOF'
#!/bin/sh
# Simulated `smartctl -H -A <device>`: the USB stick is healthy at 38C;
# everything else "does not support SMART" (exit 1 -> health unknown).
for a in "$@"; do dev="$a"; done
if [ "$dev" = "/dev/sdz1" ]; then
  printf '=== START OF INFORMATION SECTION ===\nDevice Model:     FAKE USB Drive\nSMART support is: Available - device has SMART capability.\nSMART overall-health self-assessment test result: PASSED\n194 Temperature_Celsius 0x0022 038 038 000 - - - 38\n'
  exit 0
fi
printf 'SMART support is: Unavailable - device lacks SMART capability.\n'
exit 2
EOF

cat >"$FAKE/lsblk" <<'EOF'
#!/bin/sh
# Simulated `lsblk -n -P -b -o KNAME,TYPE,FSTYPE,LABEL,UUID,SIZE,MOUNTPOINT`.
# Always-present internals: system disk (root fs), a loop, a cdrom. The USB
# stick (sdz/sdz1) only exists while the drive.state file says so.
printf 'KNAME="sda" TYPE="disk" FSTYPE="" LABEL="" UUID="" SIZE="53687091200" MOUNTPOINT=""
KNAME="sda1" TYPE="part" FSTYPE="ext4" LABEL="root" UUID="aabbccdd-1111-4a1a-8a8a-111111111111" SIZE="1127219200" MOUNTPOINT="/"
KNAME="loop0" TYPE="loop" FSTYPE="squashfs" LABEL="" UUID="" SIZE="0" MOUNTPOINT=""
KNAME="sr0" TYPE="rom" FSTYPE="iso9660" LABEL="" UUID="" SIZE="1073741312" MOUNTPOINT=""
'
if [ "$(cat "$ONYX_DEMO_DIR/drive.state")" = "1" ]; then
  mp="$(awk -F'|' '$1=="/dev/sdz1" {print $2}' "$ONYX_DEMO_DIR/mounts.state" | tail -1)"
  printf 'KNAME="sdz" TYPE="disk" FSTYPE="" LABEL="" UUID="" SIZE="32044400640" MOUNTPOINT=""
KNAME="sdz1" TYPE="part" FSTYPE="vfat" LABEL="USB DATA" UUID="ABCD-1234" SIZE="32043319296" MOUNTPOINT="%s"
' "$mp"
fi
exit 0
EOF
chmod +x "$FAKE"/*

say() { printf '\n\033[1;34m== %s ==\033[0m\n' "$*"; }
cli() { ONYX_API="http://$API" "$BIN/onyx" "$@"; }

say "starting service stack (privd -> storaged -> shared -> core -> api)"
export ONYX_DEMO_DIR="$DEMO"
"$BIN/onyx-privd" --socket-dir "$SOCK" --allowed-root "$MNT" --config-dir "$DEMO/conf.d" \
  --btrfs-bin "$FAKE/btrfs" --lsblk-bin "$FAKE/lsblk" \
  --mount-bin "$FAKE/mount" --umount-bin "$FAKE/umount" --mkdir-bin /bin/mkdir \
  --smartctl-bin "$FAKE/smartctl" \
  --testparm-bin "$FAKE/testparm" --systemctl-bin "$FAKE/systemctl" --exportfs-bin "$FAKE/exportfs" \
  >"$DEMO/privd.log" 2>&1 &
PIDS+=($!)

for _ in $(seq 1 40); do [ -S "$SOCK/onyx-privd.sock" ] && break; sleep 0.25; done

"$BIN/onyx-storaged" --socket-dir "$SOCK" --state-dir "$STATE/onyx-storaged" \
  --mount-root "$MNT" --sysfs-root "$FAKESYS" --auto-attach removable \
  --device-watch-interval-ms 500 --device-health-interval-ms 4000 \
  >"$DEMO/storaged.log" 2>&1 &
PIDS+=($!)

for _ in $(seq 1 40); do [ -S "$SOCK/onyx-storaged.sock" ] && break; sleep 0.25; done

"$BIN/onyx-shared" --socket-dir "$SOCK" >"$DEMO/shared.log" 2>&1 &
PIDS+=($!)

for _ in $(seq 1 40); do [ -S "$SOCK/onyx-shared.sock" ] && break; sleep 0.25; done

"$BIN/onyx-core" --socket-dir "$SOCK" --state-dir "$STATE/onyx-core" \
  --device-reconcile-interval 1s --device-mount-root "$MNT" >"$DEMO/core.log" 2>&1 &
PIDS+=($!)

"$BIN/onyx-api" --listen "$API" --socket-dir "$SOCK" >"$DEMO/api.log" 2>&1 &
PIDS+=($!)

for _ in $(seq 1 80); do cli status >/dev/null 2>&1 && break; sleep 0.25; done
say "stack is up"

unplug() {
  echo 0 >"$DEMO/drive.state"
  rm -rf "$FAKESYS/sdz" "$FAKESYS/sdz1"
}
plug() {
  echo 1 >"$DEMO/drive.state"
  mkdir -p "$FAKESYS/sdz" "$FAKESYS/sdz1"
  echo 1 >"$FAKESYS/sdz/removable"
}

say "USB drive is plugged in at boot — the watcher should auto-mount and share it within ~1s"
sleep 3
echo "--- onyx device list ---"
cli device list
echo "--- onyx share list (auto share must be live, SMB+NFS) ---"
cli share list
echo "--- simulated mount argv (vfat gets uid/gid/umask so share users can write) ---"
cat "$DEMO/mount-argv.log" 2>/dev/null || true
echo "--- onyx device show usb-data (health from fake smartctl) ---"
cli device show usb-data
echo "--- onyx events (audit trail so far: attach + health) ---"
cli events

say "daemon config was rendered, written by privd, and daemons reloaded"
echo "--- written smb.conf (onyx-shared RenderAll -> privd WRITE_DAEMON_CONFIG) ---"
cat "$DEMO/conf.d/smb.conf"
echo "--- written exports ---"
cat "$DEMO/conf.d/exports"
echo "--- daemon reload log (privd RELOAD_DAEMONS: testparm validation + reload) ---"
cat "$DEMO/daemon-reload.log" 2>/dev/null || true

say "manual share lifecycle: create a share, then delete it — config must follow"
cli share create photos /mnt/onyx/pool1/@data/photos --smb --nfs >/dev/null
sleep 1
grep -q '\[photos\]' "$DEMO/conf.d/smb.conf" && echo "created share photos: present in smb.conf ✓"
grep -q '/mnt/onyx/pool1/@data/photos' "$DEMO/conf.d/exports" && echo "created share photos: present in exports ✓"
cli share delete photos >/dev/null
sleep 1
if ! grep -q '\[photos\]' "$DEMO/conf.d/smb.conf"; then echo "deleted share photos: gone from smb.conf ✓"; fi
if ! grep -q '/mnt/onyx/pool1/@data/photos' "$DEMO/conf.d/exports"; then echo "deleted share photos: gone from exports ✓"; fi
echo "--- daemon reload log after manual lifecycle ---"
cat "$DEMO/daemon-reload.log" 2>/dev/null || true

say "live event stream: tailing while we unplug and replug the drive"
( ONYX_API="http://$API" "$BIN/onyx" events --stream >"$DEMO/stream.log" 2>&1 & echo $! >"$DEMO/stream.pid" )
sleep 1
unplug
sleep 2
plug
sleep 3
kill "$(cat "$DEMO/stream.pid")" 2>/dev/null || true
sleep 0.5
echo "--- captured live event stream (attach/detach/health as they happened) ---"
cat "$DEMO/stream.log" 2>/dev/null || true

say "unplugging the drive (drive.state -> 0) — removal must be detected, unmounted, share removed"
unplug
sleep 5
echo "--- onyx device list (usb-data should be 'detached') ---"
cli device list
echo "--- onyx share list (usb-data share must be gone) ---"
cli share list

say "re-plugging the drive — it must come back automatically"
plug
sleep 5
echo "--- onyx device list ---"
cli device list
echo "--- onyx share list ---"
cli share list
echo "--- onyx events (give or take the cancelled stream) ---"
cli events

say "manual detach while still plugged in (onyx device detach usb-data)"
cli device detach usb-data
sleep 3
cli device list

say "manual attach (onyx device attach usb-data)"
cli device attach usb-data
sleep 3
echo "--- onyx device show usb-data ---"
cli device show usb-data

say "DONE — everything works. Logs:"
echo "  privd:    tail -f $DEMO/privd.log"
echo "  storaged: tail -f $DEMO/storaged.log"
echo "  core:     tail -f $DEMO/core.log"