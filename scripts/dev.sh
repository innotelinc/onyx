#!/usr/bin/env bash
# Local dev runner: starts onyx-storaged, onyx-core and onyx-api on unix
# sockets under .run/onyx (no root, no /run, no systemd needed).
# Usage: scripts/dev.sh [start|stop|status|logs|restart]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bin"
RUN="$ROOT/.run"
# Socket paths must be absolute: gRPC's unix:// target parser treats a leading
# path segment as an authority, so relative socket dirs break dialing.
SOCK_DIR="$(cd "${ONYX_SOCKET_DIR:-$RUN/onyx}" 2>/dev/null && pwd || realpath -m "${ONYX_SOCKET_DIR:-$RUN/onyx}")"
STATE_DIR="$(realpath -m "${ONYX_STATE_DIR:-$RUN/state}")"
API_LISTEN="${ONYX_API_LISTEN:-127.0.0.1:8080}"

SERVICES=(onyx-privd onyx-storaged onyx-shared onyx-snapd onyx-backupd onyx-core onyx-api)

mkdir -p "$SOCK_DIR" "$STATE_DIR"

live_pids() {
  for s in "${SERVICES[@]}"; do
    local f="$RUN/$s.pid"
    if [ -f "$f" ]; then
      local pid; pid="$(cat "$f")"
      if kill -0 "$pid" 2>/dev/null; then echo "$pid"; fi
    fi
  done
}

start() {
  [ -x "$BIN/onyx-storaged" ] || { echo "binaries missing — run: make build" >&2; exit 1; }

  # onyx-privd (Rust privilege helper, the one root process).
  # ONYX_PRIVD_BTRFS_BIN overrides the btrfs binary for sandbox testing.
  # Written daemon config (smb.conf/exports) lands under .run/, never /etc.
  local btrfs_arg=()
  [ -n "${ONYX_PRIVD_BTRFS_BIN:-}" ] && btrfs_arg=(--btrfs-bin "$ONYX_PRIVD_BTRFS_BIN")
  "$BIN/onyx-privd" --socket-dir "$SOCK_DIR" --config-dir "$RUN/conf.d" "${btrfs_arg[@]}" \
    >"$RUN/onyx-privd.log" 2>&1 &
  echo $! >"$RUN/onyx-privd.pid"

  # wait for the privd socket before starting storaged (it scans via privd)
  for _ in $(seq 1 40); do
    [ -S "$SOCK_DIR/onyx-privd.sock" ] && break
    sleep 0.25
  done

  # onyx-storaged (Rust data plane)
  "$BIN/onyx-storaged" --socket-dir "$SOCK_DIR" --state-dir "$STATE_DIR/onyx-storaged" \
    >"$RUN/onyx-storaged.log" 2>&1 &
  echo $! >"$RUN/onyx-storaged.pid"

  # wait for the storaged socket before starting core (core forwards to it)
  for _ in $(seq 1 40); do
    [ -S "$SOCK_DIR/onyx-storaged.sock" ] && break
    sleep 0.25
  done

  # onyx-shared (Go share manager)
  "$BIN/onyx-shared" --socket-dir "$SOCK_DIR" >"$RUN/onyx-shared.log" 2>&1 &
  echo $! >"$RUN/onyx-shared.pid"

  # wait for the shared socket before starting core (core calls it on create)
  for _ in $(seq 1 40); do
    [ -S "$SOCK_DIR/onyx-shared.sock" ] && break
    sleep 0.25
  done

  # onyx-snapd (snapshot service)
  "$BIN/onyx-snapd" --socket-dir "$SOCK_DIR" --state-dir "$STATE_DIR/onyx-snapd" \
    >"$RUN/onyx-snapd.log" 2>&1 &
  echo $! >"$RUN/onyx-snapd.pid"

  # onyx-backupd (backup service)
  "$BIN/onyx-backupd" --socket-dir "$SOCK_DIR" --state-dir "$STATE_DIR/onyx-backupd" \
    >"$RUN/onyx-backupd.log" 2>&1 &
  echo $! >"$RUN/onyx-backupd.pid"

  # wait for the Obsidian safety services before starting the API
  for sock in onyx-snapd.sock onyx-backupd.sock; do
    for _ in $(seq 1 40); do
      [ -S "$SOCK_DIR/$sock" ] && break
      sleep 0.25
    done
    [ -S "$SOCK_DIR/$sock" ] || { echo "failed to start ${sock%.sock}" >&2; stop; exit 1; }
  done

  # onyx-core (Go control plane)
  "$BIN/onyx-core" --socket-dir "$SOCK_DIR" --state-dir "$STATE_DIR/onyx-core" \
    >"$RUN/onyx-core.log" 2>&1 &
  echo $! >"$RUN/onyx-core.pid"

  # wait for core before starting the HTTP gateway
  for _ in $(seq 1 40); do
    [ -S "$SOCK_DIR/onyx-core.sock" ] && break
    sleep 0.25
  done
  [ -S "$SOCK_DIR/onyx-core.sock" ] || { echo "failed to start onyx-core" >&2; stop; exit 1; }

  # onyx-api (Go HTTP gateway)
  "$BIN/onyx-api" --listen "$API_LISTEN" --socket-dir "$SOCK_DIR" \
    >"$RUN/onyx-api.log" 2>&1 &
  echo $! >"$RUN/onyx-api.pid"

  for _ in $(seq 1 40); do
    kill -0 "$(cat "$RUN/onyx-api.pid")" 2>/dev/null && curl -fsS "http://$API_LISTEN/healthz" >/dev/null 2>&1 && break
    sleep 0.25
  done
  kill -0 "$(cat "$RUN/onyx-api.pid")" 2>/dev/null || { echo "failed to start onyx-api" >&2; stop; exit 1; }

  echo "started: sockets in $SOCK_DIR, API at http://$API_LISTEN"
  echo "  logs:  tail -f $RUN/onyx-*.log"
  echo "  stop:  scripts/dev.sh stop"
}

stop() {
  local pids; pids="$(live_pids)"
  if [ -n "$pids" ]; then
    kill $pids 2>/dev/null || true
    sleep 0.5
    # SIGKILL any stragglers
    local remaining; remaining="$(live_pids)"
    [ -n "$remaining" ] && kill -9 $remaining 2>/dev/null || true
  fi
  rm -f "$RUN/onyx-"*.pid "$SOCK_DIR"/onyx-*.sock
  echo "stopped"
}

status() {
  local pids; pids="$(live_pids)"
  if [ -n "$pids" ]; then
    echo "running pids: $pids"
    socks=""; for s in "$SOCK_DIR"/onyx-*.sock; do [ -S "$s" ] && socks="$socks $(basename "$s")"; done
    echo "sockets:$socks"
  else
    echo "not running"
  fi
}

restart() { stop; start; }

cmd="${1:-start}"
case "$cmd" in
  start) start ;;
  stop) stop ;;
  restart) restart ;;
  status) status ;;
  logs) tail -f "$RUN"/onyx-*.log ;;
  *) echo "usage: $0 [start|stop|restart|status|logs]" >&2; exit 2 ;;
esac