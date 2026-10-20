#!/usr/bin/env bash
# docker-rebuild.sh — build every compose image with the version it actually is.
#
# The stack spent a release reporting `0.4.0-dev` / `commit: unknown` because
# VERSION and COMMIT are build arguments with development defaults, and nothing
# on the host ever passed them:
#
#   args: { VERSION: "${VERSION:-0.4.0-dev}", COMMIT: "${COMMIT:-unknown}" }
#
# A default that is honest in development becomes a lie in a deployment: the
# status page claimed a number no commit had. This script reads both values from
# the checkout being built, so the image can only describe the tree it came
# from. `git describe` names the nearest tag and how far past it the build is
# (v0.4.0-3-g173140f); `git rev-parse --short` names the exact commit.
#
# A dirty tree is stamped `-dirty` on the commit: the build then says it does
# not match a commit, which is the truth. Override either value for a release
# build with VERSION=v0.4.1 COMMIT=... ; the override wins.
#
# Builds run one service at a time by default. Each Go image compiles the whole
# module, and on a 2-core host parallel legs only contend for the same cores —
# serial is not slower here, it just makes the log readable. PARALLEL=N for a
# bigger box.
#
# Usage:
#   scripts/docker-rebuild.sh                 # every service in compose order
#   scripts/docker-rebuild.sh onyx-api        # just these
#   VERSION=v0.4.1 scripts/docker-rebuild.sh  # release stamp
#
# Env:
#   PARALLEL      services built at once (default 1)
#   LOG           build log path (default: stdout only)
#   COMPOSE_FILE  compose file (default: docker-compose.yml in the repo root)

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

COMPOSE_FILE="${COMPOSE_FILE:-docker-compose.yml}"
PARALLEL="${PARALLEL:-1}"

if ! git rev-parse --git-dir >/dev/null 2>&1; then
  echo "docker-rebuild: not a git checkout — cannot stamp the build" >&2
  exit 1
fi

# The values the images will carry. Explicit environment wins over the checkout.
if [ -z "${VERSION:-}" ]; then
  VERSION="$(git describe --tags --always)"
fi
if [ -z "${COMMIT:-}" ]; then
  COMMIT="$(git rev-parse --short HEAD)"
  # Only tracked-file changes can reach an image: COPY takes tracked sources and
  # an untracked build helper does not ship. So one modified tracked file makes
  # the stamp wrong, while a stray untracked file does not.
  if ! git diff --quiet HEAD -- . 2>/dev/null; then
    COMMIT="${COMMIT}-dirty"
  fi
fi
export VERSION COMMIT

log() { printf '%s\n' "$*"; }

log "=== onyx rebuild"
log "    tree    $(git rev-parse --short HEAD) ($(git symbolic-ref --short -q HEAD || echo detached))"
log "    VERSION $VERSION"
log "    COMMIT  $COMMIT"

services=("$@")
if [ "${#services[@]}" -eq 0 ]; then
  mapfile -t services < <(docker compose -f "$COMPOSE_FILE" config --services)
fi
if [ "${#services[@]}" -eq 0 ]; then
  echo "docker-rebuild: compose listed no services — is $COMPOSE_FILE right?" >&2
  exit 1
fi

build_one() {
  local svc="$1" start end
  start="$(date -u +%H:%M:%S)"
  if docker compose -f "$COMPOSE_FILE" build "$svc"; then
    end="$(date -u +%H:%M:%S)"
    log "=== $start→$end built $svc"
  else
    log "=== FAILED $svc" >&2
    return 1
  fi
}
export -f build_one log
export COMPOSE_FILE

failed=()
if [ "$PARALLEL" -le 1 ]; then
  for svc in "${services[@]}"; do
    build_one "$svc" || failed+=("$svc")
  done
else
  # A parallel leg is a subshell, so failures come back through a file rather
  # than through the array.
  failed_file="$(mktemp)"
  trap 'rm -f "$failed_file"' EXIT
  running=0
  for svc in "${services[@]}"; do
    ( build_one "$svc" || echo "$svc" >> "$failed_file" ) &
    running=$((running + 1))
    if [ "$running" -ge "$PARALLEL" ]; then wait; running=0; fi
  done
  wait
  mapfile -t failed < "$failed_file"
fi

if [ "${#failed[@]}" -gt 0 ]; then
  log "=== ${#failed[@]} service(s) FAILED: ${failed[*]}"
  log "    images for those services are still the previous build; do not 'up -d' them"
  exit 1
fi

log "=== ALL BUILT — VERSION=$VERSION COMMIT=$COMMIT"
log "    next: docker compose up -d  (recreate so containers pick up the new images)"
