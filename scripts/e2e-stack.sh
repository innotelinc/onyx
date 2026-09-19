#!/usr/bin/env bash
# End-to-end check of a *running* Onyx stack (docs/design/11): the three v0.4
# flows that span every layer, driven through the gateway exactly as the Web
# console drives them.
#
#   1. pool      — create a pool on a real block device (needs POOL_DEVICE)
#   2. app       — install an app from the catalog and require its containers up
#   3. tiered    — create a TIERED bucket, upload over the S3 API, verify the
#                  cloud copy, evict the cold local copies and refetch one
#
# The point is the *span*: API → core → storaged/privd → kernel, or appd → the
# Docker engine, or objectstore → rclone → the tier target. A unit test cannot
# see a permission, a mount namespace or a missing helper binary; this can, and
# it is how the pool-root permission and storage-root bugs were found.
#
# Usage:
#   scripts/e2e-stack.sh                       # app + tiered (pool skipped)
#   POOL_DEVICE=/dev/sdc scripts/e2e-stack.sh  # all three, on a scratch disk
#   POOL_IMAGE=/tmp/onyx-pool.img scripts/e2e-stack.sh
#                                              # all three, on a loop device
#                                              # the script creates and removes
#   APP_ID=jellyfin scripts/e2e-stack.sh
#   PROJECT=onyx-e2e scripts/e2e-stack.sh      # compose project to look in
#
# POOL_DEVICE is destroyed. Point it at a disk you do not need. POOL_IMAGE is a
# file this script creates, formats and deletes — the safe way to run the
# destructive flow on a machine with no spare disk (CI, a laptop): the loop
# device it hands to storaged is a real whole disk to the kernel, so wipefs,
# mkfs, mount and the pool-root mode all take the production path.
#
# Requirements: the stack is up (`docker compose up -d`), bash, python3.
set -euo pipefail

API="${ONYX_API:-http://127.0.0.1:2081}/api/v1"
PROJECT="${PROJECT:-onyx-e2e}"
APP_ID="${APP_ID:-nextcloud}"
POOL_DEVICE="${POOL_DEVICE:-}"
# POOL_IMAGE turns the pool flow into a self-contained one: the script makes the
# image, attaches it to a free loop device and detaches it again on the way out.
POOL_IMAGE="${POOL_IMAGE:-}"
POOL_IMAGE_SIZE="${POOL_IMAGE_SIZE:-2G}"
# Privilege prefix for losetup: empty as root, `sudo` on a CI runner (the loop
# control device is root-only, and the harness must not assume it is root).
SUDO="${SUDO:-}"
# Provisioning the loop device is the only way to run the pool flow on a host
# with no spare disk, so where that is the intent (CI) a failure to provision is
# a failure rather than a skip.
REQUIRE_POOL="${E2E_REQUIRE_POOL:-0}"
LOOP_DEVICE=""
LOOP_ERROR=""
POOL_NAME="${POOL_NAME:-e2e-pool}"
BUCKET="${BUCKET:-e2e-tiered}"
TIER_TARGET="${TIER_TARGET:-/mnt/onyx/e2e-tier-cold}"
KEEP="${KEEP:-0}"

pass=0
fail=0
skip=0

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '  \033[32mPASS\033[0m %s\n' "$*"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; fail=$((fail + 1)); }
meh() { printf '  \033[33mSKIP\033[0m %s\n' "$*"; skip=$((skip + 1)); }

command -v python3 >/dev/null 2>&1 || {
  echo "python3 is required to read JSON responses" >&2
  exit 2
}

api() { # api METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -m 300 -X "$method" "$API$path" -H 'Content-Type: application/json' -d "$body"
  else
    curl -sS -m 300 -X "$method" "$API$path"
  fi
}

json_get() { # json_get <python expr over `d`> [extra argv...]
  python3 -c "import json,sys;d=json.load(sys.stdin);print(eval(sys.argv[1]))" "$@" 2>/dev/null || true
}

# The container that owns a given compose service, so rclone runs where the
# generated config and the pool are visible.
container_of() {
  docker ps --filter "label=com.docker.compose.project=$PROJECT" \
    --filter "label=com.docker.compose.service=$1" --format '{{.Names}}' | head -1
}

# rclone against the stack's own object store over the S3 API, exactly as an
# application would reach it.
OX_RCLONE_ENV=()
load_s3_env() {
  local c; c="$(container_of onyx-objectstore)"
  [ -n "$c" ] || return 1
  local access secret
  access="$(docker inspect "$c" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^S3_ACCESS_KEY=//p')"
  secret="$(docker inspect "$c" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^S3_SECRET_KEY=//p')"
  [ -n "$access" ] || return 1
  OX_RCLONE_ENV=(
    -e RCLONE_CONFIG_OX_TYPE=s3 -e RCLONE_CONFIG_OX_PROVIDER=Other
    -e "RCLONE_CONFIG_OX_ACCESS_KEY_ID=$access"
    -e "RCLONE_CONFIG_OX_SECRET_ACCESS_KEY=$secret"
    -e RCLONE_CONFIG_OX_ENDPOINT=http://onyx-objectstore:9000
    -e RCLONE_CONFIG_OX_FORCE_PATH_STYLE=true
  )
}

ox() { # ox <rclone args...>
  local c; c="$(container_of onyx-backupd)"
  docker exec "${OX_RCLONE_ENV[@]}" "$c" rclone "$@"
}

# Write the upload's source *inside* the rclone container: its /tmp is not the
# host's, and a file created on the host is simply not there.
ox_write() { # ox_write <container path> <content>
  local c; c="$(container_of onyx-backupd)"
  docker exec -i "$c" sh -c "cat > '$1'" <<<"$2" || return 1
}

# loop_for_image <image> <size> — create the image and attach a free loop
# device, setting LOOP_DEVICE.
#
# The destructive flow needs a block device the data plane can format, and most
# hosts running this script have no disposable disk to give it. A loop device
# backed by a file is a whole disk with no partition table as far as the kernel,
# lsblk and mkfs are concerned, so the flow is the production one — and the
# storage it destroys is a file this script owns. On failure it explains itself
# in LOOP_ERROR and takes the image with it.
loop_for_image() {
  local image="$1" size="$2" out
  LOOP_DEVICE=""
  LOOP_ERROR=""
  command -v losetup >/dev/null 2>&1 || { LOOP_ERROR="losetup is not installed"; return 1; }
  command -v truncate >/dev/null 2>&1 || { LOOP_ERROR="truncate is not installed"; return 1; }
  rm -f "$image"
  if ! truncate -s "$size" "$image" 2>/dev/null; then
    LOOP_ERROR="cannot create $image"
    return 1
  fi
  if ! out="$($SUDO losetup -f --show "$image" 2>&1)"; then
    # losetup explains itself over several lines; the report is one line.
    LOOP_ERROR="losetup could not attach $image: $(printf '%s' "${out:-failed}" | tr '\n' ' ' | tr -s ' ' | sed 's/ $//')"
    rm -f "$image"
    return 1
  fi
  if [ ! -b "$out" ]; then
    # Something answered but there is no device node here — a container without
    # device passthrough, not a full host.
    LOOP_ERROR="losetup returned ${out:-nothing}, which is not a block device here: this environment cannot provide one"
    rm -f "$image"
    return 1
  fi
  LOOP_DEVICE="$out"
}

cleanup() {
  if [ "$KEEP" = 1 ]; then
    echo
    echo "KEEP=1 — leaving ${POOL_NAME}, ${BUCKET} and ${APP_ID} in place"
    if [ -n "$LOOP_DEVICE" ]; then
      echo "         $LOOP_DEVICE still backs $POOL_IMAGE (detach it with: $SUDO losetup -d $LOOP_DEVICE)"
    fi
    return
  fi
  say cleanup
  api DELETE "/buckets/$BUCKET?force=true" >/dev/null 2>&1 || true
  api DELETE "/apps/$APP_ID?purge_data=true&force=true" >/dev/null 2>&1 || true
  if [ -n "$POOL_DEVICE" ]; then
    # The pool goes first: its mount has to be gone before the device backing it
    # is detached, or the loop device stays busy and the image is never freed.
    api DELETE "/pools/$POOL_NAME" >/dev/null 2>&1 || true
  fi
  if [ -n "$LOOP_DEVICE" ]; then
    $SUDO losetup -d "$LOOP_DEVICE" >/dev/null 2>&1 || true
    rm -f "$POOL_IMAGE"
    echo "  detached $LOOP_DEVICE and removed $POOL_IMAGE"
  fi
  echo "  removed the bucket, the app and (if created) the pool"
}
trap cleanup EXIT

say "stack reachable"
if status="$(api GET /system/version)"; then
  echo "  $(printf '%s' "$status" | head -c 160)"
else
  bad "no answer from $API — is the stack up?"
  exit 1
fi

# --- 1. pool ------------------------------------------------------------------
say "1. create a pool"
if [ -z "$POOL_DEVICE" ] && [ -n "$POOL_IMAGE" ]; then
  if loop_for_image "$POOL_IMAGE" "$POOL_IMAGE_SIZE"; then
    POOL_DEVICE="$LOOP_DEVICE"
    echo "  $POOL_DEVICE backs $POOL_IMAGE ($POOL_IMAGE_SIZE, created by this script)"
  elif [ "$REQUIRE_POOL" = 1 ]; then
    bad "no loop device for $POOL_IMAGE: $LOOP_ERROR"
  else
    meh "no loop device for $POOL_IMAGE ($LOOP_ERROR) — the pool flow is skipped"
  fi
fi
if [ -z "$POOL_DEVICE" ]; then
  # Reported only when the caller asked for no pool at all; a POOL_IMAGE that
  # could not be provisioned has already said why above.
  [ -n "$POOL_IMAGE" ] || meh "no pool device: set POOL_DEVICE=/dev/sdc (a scratch whole disk) or POOL_IMAGE=</path/img>"
else
  # The device has to exist where the data plane runs, not just on the host: a
  # container that cannot see /dev/<disk> cannot format it, and the failure the
  # operator would get from mkfs says nothing about that.
  if [ ! -b "$POOL_DEVICE" ]; then
    bad "$POOL_DEVICE is not a block device here — the data plane cannot format it"
    echo "       (on a nested/incus-like host, device nodes have to be passed through)"
  else
    created="$(api POST /pools "{\"name\":\"$POOL_NAME\",\"device\":\"$POOL_DEVICE\",\"fs_type\":\"ext4\"}")"
    if printf '%s' "$created" | grep -q '"error"'; then
      bad "pool creation failed: $created"
    else
      ok "pool $POOL_NAME created on $POOL_DEVICE ($(json_get 'd["fsType"]' <<<"$created"))"
      # The root of a new ext4/btrfs filesystem is root:root 0755, which would
      # make every unprivileged writer fail later with a bare "permission
      # denied" — so the mode is asserted here, where the cause is still clear.
      mode="$(stat -c '%a' "/mnt/onyx/$POOL_NAME" 2>/dev/null || echo '?')"
      if [ "$mode" = "777" ]; then
        ok "pool root /mnt/onyx/$POOL_NAME is writable by the unprivileged services (mode $mode)"
      else
        bad "pool root mode is $mode, expected 777 (--pool-mode)"
      fi
      if docker exec "$(container_of onyx-api)" touch "/mnt/onyx/$POOL_NAME/.e2e-write" 2>/dev/null; then
        ok "onyx-api can write into the pool"
        docker exec "$(container_of onyx-api)" rm -f "/mnt/onyx/$POOL_NAME/.e2e-write" 2>/dev/null || true
      else
        bad "onyx-api cannot write into the pool"
      fi
    fi
  fi
fi

# --- 2. app -------------------------------------------------------------------
say "2. install an app ($APP_ID)"
installed="$(api POST /apps "{\"app_id\":\"$APP_ID\"}")"
if printf '%s' "$installed" | grep -q 'already installed'; then
  # Install has to be exercised from scratch, so the previous installation is
  # removed first (the flow the console's Install button drives on a reinstall).
  api DELETE "/apps/$APP_ID?purge_data=true&force=true" >/dev/null 2>&1 || true
  installed="$(api POST /apps "{\"app_id\":\"$APP_ID\"}")"
fi
if printf '%s' "$installed" | grep -q '"error"'; then
  bad "install failed: $installed"
else
  ok "$APP_ID accepted by appd"
fi

# An install is only real once its containers are up: appd starting a compose
# project can succeed while every container crash-loops (a sandbox posture the
# image refuses, a missing dependency, a port already taken).
# The wait covers a first install on a cold host, image pulls included: an
# `appd` install that pulls its images from a registry takes minutes, and a
# timeout that is too short reads as a crash-looping app.
crashed=0
for _ in $(seq 1 60); do
  containers="$(api GET /containers)"
  running="$(json_get 'len([c for c in d["containers"] if c["appId"]==sys.argv[2] and c["status"]=="running"])' "$APP_ID" <<<"$containers")"
  crashed="$(json_get 'len([c for c in d["containers"] if c["appId"]==sys.argv[2] and c["status"] in ("exited","restarting")])' "$APP_ID" <<<"$containers")"
  [ "${running:-0}" -gt 0 ] && [ "${crashed:-0}" = 0 ] && break
  sleep 3
done
if [ "${running:-0}" -gt 0 ] && [ "${crashed:-0}" = 0 ]; then
  ok "$APP_ID is running in ${running} container(s), none crash-looping"
else
  bad "$APP_ID: ${running:-0} running, ${crashed:-0} exited/restarting — see: docker logs onyx-$APP_ID"
fi

# --- 3. tiered bucket ---------------------------------------------------------
say "3. tiered bucket: sync, verify, evict, refetch"
if ! load_s3_env; then
  bad "could not read S3_ACCESS_KEY from the objectstore container"
else
  # A tier target is a directory on the pool: the object store writes there as
  # its own unprivileged user, so the storage root has to be writable by it.
  root_mode="$(stat -c '%a' /mnt/onyx 2>/dev/null || echo '?')"
  case "$root_mode" in
    *7|*6|*3|*2) : ;;
    *) bad "/mnt/onyx mode is $root_mode — the object store cannot write a tier target (setup.sh sets 0777)" ;;
  esac
  # Propagation is the other half of the same requirement, and the harder half to
  # see: with /mnt/onyx private, storaged mounts a pool inside its own namespace
  # and everything looks healthy from the inside while the API and WebDAV
  # containers see an empty directory (docs/design/05#2.4). Check it here — a
  # mount namespace is exactly what this run exists to catch.
  # `shared` anywhere in the flag list, not only at the end: a container's bind
  # of a shared host path reports `shared,slave` (the slave flag is how it
  # follows the host), which is exactly the working case. Matching the suffix
  # made the check fail on a correct deployment.
  root_prop="$(findmnt -no PROPAGATION /mnt/onyx 2>/dev/null || echo 'unknown')"
  if printf '%s' "$root_prop" | tr ',' '\n' | grep -qx shared; then
    ok "/mnt/onyx is a $root_prop mount — pool mounts reach every container"
  else
    bad "/mnt/onyx propagation is '${root_prop}' — pools would stay invisible to the API/WebDAV containers (fix: mount -o bind,shared /mnt/onyx /mnt/onyx)"
  fi

  api DELETE "/buckets/$BUCKET?force=true" >/dev/null 2>&1 || true
  bucket="$(api POST /buckets "{\"name\":\"$BUCKET\",\"tier\":\"tiered\",\"cloud_target\":\"$TIER_TARGET\",\"evict_after_days\":1}")"
  if printf '%s' "$bucket" | grep -q '"error"'; then
    bad "bucket creation failed: $bucket"
  else
    ok "tiered bucket $BUCKET → $TIER_TARGET (evict after 1 day)"
    for n in 1 2 3; do
      ox_write "/tmp/onyx-e2e-o$n.txt" "object $n content" || bad "could not stage o$n in the container"
      # copyto, not copy: the object key is the *destination* path, so the
      # object is `o<n>.txt` regardless of the staging file's name.
      if out="$(ox copyto "/tmp/onyx-e2e-o$n.txt" "ox:$BUCKET/o$n.txt" 2>&1)"; then
        :
      else
        bad "upload o$n failed: $(printf '%s' "$out" | tail -1)"
      fi
    done
    uploaded="$(ox lsf "ox:$BUCKET/" 2>/dev/null | wc -l)"
    [ "${uploaded:-0}" -ge 3 ] && ok "3 objects uploaded over the S3 API" \
      || bad "expected 3 objects over the S3 API, saw ${uploaded:-0}"

    # Age the local copies so they are evictable: eviction releases copies older
    # than the bucket's evict_after_days, and a just-written object is not old.
    os_container="$(container_of onyx-objectstore)"
    docker exec "$os_container" sh -c \
      "find /var/lib/onyx/objectstore/objects/$BUCKET -type f -exec touch -t 202001010000.00 {} +" \
      >/dev/null 2>&1 || true

    synced="$(api POST "/buckets/$BUCKET/sync?evict=1")"
    evicted="$(json_get 'd.get("evicted",0)' <<<"$synced")"
    # A count of zero is omitted from the response, so an absent key is zero.
    left="$(json_get 'd["bucket"].get("localObjects",0)' <<<"$synced")"
    if printf '%s' "$synced" | grep -q '"error"'; then
      bad "sync failed: $synced"
    elif [ "${evicted:-0}" -ge 3 ] && [ "${left:-x}" = "0" ]; then
      ok "cloud copy verified and $evicted cold local copies evicted (0 left)"
    else
      bad "expected 3 evictions and 0 local objects, got evicted=$evicted left=$left: $synced"
    fi

    # Eviction is only safe if a read still works: the object must come back
    # from the cloud and be cached locally again.
    got=""; read_err=""
    read_err="$(ox cat "ox:$BUCKET/o2.txt" 2>&1 >/tmp/onyx-e2e-read)" || true
    got="$(cat /tmp/onyx-e2e-read 2>/dev/null)"; rm -f /tmp/onyx-e2e-read
    if [ "$got" = "object 2 content" ]; then
      ok "read of an evicted object refetched the right bytes from the cloud"
    else
      bad "read of an evicted object returned '$(printf '%s' "$got" | head -c 80)' ($(printf '%s' "$read_err" | tail -1))"
    fi
    if docker exec "$os_container" test -f "/var/lib/onyx/objectstore/objects/$BUCKET/o2.txt" 2>/dev/null; then
      ok "the refetched object is cached locally again"
    else
      bad "the refetched object was not cached"
    fi
    # The download lands in a temporary file and is renamed into place; a leak
    # here would leave an object-shaped file in the bucket that the next sync
    # uploads to the cloud as data.
    leftovers="$(docker exec "$os_container" sh -c "find /var/lib/onyx/objectstore/objects/$BUCKET -name 'refetch-*' 2>/dev/null | wc -l" 2>/dev/null)"
    [ "${leftovers:-x}" = "0" ] && ok "the refetch left no temporary download behind" \
      || bad "the refetch left ${leftovers:-?} temporary download(s) in the bucket"
  fi
fi

# --- verdict ------------------------------------------------------------------
say "result"
printf '  %d passed, %d failed, %d skipped\n' "$pass" "$fail" "$skip"
[ "$fail" = 0 ] || exit 1
