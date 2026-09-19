#!/usr/bin/env bash
# ONYX — Online Storage System — Platform: one-command deploy (docs/design/11 §8).
#
#   ./setup.sh            # compose up (build) → Authentik → NPM provisioning
#   ./setup.sh --no-build # use existing images instead of rebuilding
#   ./setup.sh --skip-auth# skip Authentik provider provisioning
#   ./setup.sh --help
#
# Idempotent: safe to re-run; converges certs, proxy hosts and providers.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

BUILD=1
SKIP_AUTH=0
for arg in "$@"; do
  case "$arg" in
    --no-build) BUILD=0 ;;
    --skip-auth) SKIP_AUTH=1 ;;
    --help|-h)
      sed -n '2,9p' "$0"
      exit 0
      ;;
    *) echo "unknown option: $arg (see ./setup.sh --help)" >&2; exit 1 ;;
  esac
done

log() { printf '\033[1;34m>>\033[0m %s\n' "$*"; }

# Enable the version-controlled commit-guard hooks (.githooks) if this is a
# git checkout (blocks attribution to anyone but Darnel Hunter).
if [ -d "$ROOT/.githooks" ] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git config core.hooksPath "$ROOT/.githooks"
  log "commit guard hook enabled (core.hooksPath -> .githooks)"
fi

# --- 0. Environment -----------------------------------------------------------
if [ ! -f .env ]; then
  cp .env.example .env
  cat <<'EOF'

Created .env from .env.example.
  * Edit .env and fill in the secrets (NPM_EMAIL/NPM_PASSWORD, TSIG_KEY_SECRET,
    AUTHENTIK_* and S3_*), then re-run ./setup.sh.
EOF
  exit 1
fi
set -a; # shellcheck disable=SC1091
source .env
set +a

# --- 1. Prerequisites ----------------------------------------------------------
for tool in docker python3 curl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: $tool is required but not installed" >&2
    exit 1
  fi
done
docker compose version >/dev/null 2>&1 || { echo "error: docker compose plugin is required" >&2; exit 1; }

DOMAIN="${DOMAIN:-onyx.innotel.us}"

# Device trust + edge mode (docs/design/11 §10). Device trust is optional:
#   off (default) | local (ONYX-run CA + local NPM gate) | cerulean (remote)
DEVICE_TRUST="${DEVICE_TRUST:-off}"
NPM_MODE="${NPM_MODE:-local}"
# Identity mode: remote (default, shared Authentik) or local (bundled replacement).
# AUTHENTIK_MODE=local implies AUTHENTIK_URL=http://127.0.0.1:9000 and starts
# the authentik compose profile; remote uses the external AUTHENTIK_URL directly.
AUTHENTIK_MODE="${AUTHENTIK_MODE:-remote}"
# A remote edge implies remote device trust unless explicitly local.
if [ "$NPM_MODE" = "cerulean" ] && [ "$DEVICE_TRUST" = "local" ]; then
  echo "warning: DEVICE_TRUST=local with NPM_MODE=cerulean issues certs no local edge enforces — using DEVICE_TRUST=cerulean" >&2
  DEVICE_TRUST="cerulean"
fi
case "$DEVICE_TRUST" in
  off|local|cerulean) ;;
  *) echo "error: DEVICE_TRUST must be off, local or cerulean (got: $DEVICE_TRUST)" >&2; exit 1 ;;
esac

# --- 2. Remote/cloud storage config (docs/design/05#6) -------------------------
# onyx-api writes rclone remotes here (the Shares page's cloud & remote storage
# setup) and onyx-backupd reads the same catalog for remote backup targets. The
# containers run as the unprivileged "onyx" user (uid 100, gid 101 in alpine),
# so the bind-mounted directory has to be writable by it — otherwise the API can
# read the catalog but every setup attempt fails with a permission error.
RCLONE_DIR="${RCLONE_CONFIG_DIR:-./data/rclone}"
mkdir -p "$RCLONE_DIR"
if [ "$(id -u)" = "0" ]; then
  chown -R 100:101 "$RCLONE_DIR" 2>/dev/null || true
else
  chmod 0777 "$RCLONE_DIR" 2>/dev/null || true
fi
[ -f "$RCLONE_DIR/rclone.conf" ] || : > "$RCLONE_DIR/rclone.conf"
chmod 0666 "$RCLONE_DIR/rclone.conf" 2>/dev/null || true

# --- 2b. Storage root + mount propagation (docs/design/05#2) ------------------
# Pools live under /mnt/onyx on the *host*: onyx-privd mounts them there inside
# its container, and every other onyx container bind-mounts the same directory.
# That only works when the host path is a shared mount. On a private mount the
# mount stops at privd's namespace — the data plane reports the pool as mounted
# while onyx-api sees an empty directory, which the Files page shows as "No
# storage is mounted". Both halves have to be right: this makes the host path
# shared, and docker-compose.yml binds it :rshared.
STORAGE_ROOT="${ONYX_STORAGE_ROOT:-/mnt/onyx}"
propagation_of() {
  # The optional fields of this mount's /proc/self/mountinfo entry carry the
  # propagation, up to the "-" separator. Field 5 is the mount point.
  awk -v target="$1" '
    $5 == target {
      for (i = 7; i <= NF; i++) {
        if ($i == "-") break
        if ($i ~ /^shared:/) { print "shared"; exit }
      }
      print "private"; exit
    }' /proc/self/mountinfo 2>/dev/null
}
if [ "$(id -u)" = "0" ]; then
  mkdir -p "$STORAGE_ROOT"
  # The containers run as their own `onyx` user (uid 100) with no counterpart on
  # the host, so a root:root 0755 storage root is writable by nobody in the
  # stack: uploads, mkdir, apps and a bucket's tier target all fail with
  # "permission denied". The systemd install expresses the same intent as
  # `2770 root:onyx` in deploy/tmpfiles.d/onyx.conf — a group the host actually
  # has. Compose has no such group, so the root is opened up instead; on a
  # dedicated appliance host that is the same trust level as the pool roots
  # onyx-privd now creates (--pool-mode, default 0777).
  chmod 0777 "$STORAGE_ROOT" 2>/dev/null || true
  if [ "$(propagation_of "$STORAGE_ROOT")" = "shared" ]; then
    log "storage root ${STORAGE_ROOT} is shared — pool mounts propagate to every container"
  # Two attempts, in order, and the second is the one that works on a plain
  # directory: `mount --make-shared` acts on a *mount*, so on a directory that is
  # not a mount point it fails ("not mount point or bad option", exit 32) — and
  # /mnt/onyx is a plain directory on every host that has no fstab entry for it.
  # Binding it to itself first is what gives the kernel a mount to share.
  elif mount --make-shared "$STORAGE_ROOT" 2>/dev/null \
    || mount -o bind,shared "$STORAGE_ROOT" "$STORAGE_ROOT" 2>/dev/null; then
    if [ "$(propagation_of "$STORAGE_ROOT")" = "shared" ]; then
      log "made ${STORAGE_ROOT} a shared mount so pool mounts reach the other containers"
      log "note: keep it across reboots with an /etc/fstab entry:"
      log "      ${STORAGE_ROOT} ${STORAGE_ROOT} none bind,shared 0 0"
    else
      echo "warning: mount reported success but ${STORAGE_ROOT} is not shared." >&2
      echo "         A pool created now stays invisible to the API and WebDAV containers." >&2
    fi
  else
    echo "warning: could not make ${STORAGE_ROOT} a shared mount." >&2
    echo "         A pool created now stays invisible to the API and WebDAV containers." >&2
    echo "         Fix it with: sudo mount -o bind,shared ${STORAGE_ROOT} ${STORAGE_ROOT}" >&2
  fi
else
  echo "warning: not running as root — skipped the ${STORAGE_ROOT} shared-mount check." >&2
  echo "         If pools are created but Files stays empty: sudo mount --make-shared ${STORAGE_ROOT}" >&2
fi

# --- 3. Bring the stack up -----------------------------------------------------
# Compose profiles for local replacements of shared platform services.
PROFILES=""
[ "$AUTHENTIK_MODE" = "local" ] && PROFILES="$PROFILES authentik"
[ "$NPM_MODE" = "local" ] && PROFILES="$PROFILES npm"

if [ -n "$PROFILES" ]; then
  log "starting profiles:$PROFILES"
fi

if [ "$NPM_MODE" = "cerulean" ]; then
  # The edge is remote (Cerulean-managed): exclude the local NPM and its
  # published 80/443/81 from this host entirely.
  log "NPM_MODE=cerulean — starting the stack without the local NPM ..."
  if [ "$BUILD" = 1 ]; then
    docker compose --profile${PROFILES:+ $PROFILES} up -d --build
  else
    docker compose --profile${PROFILES:+ $PROFILES} up -d
  fi
elif [ "$BUILD" = 1 ]; then
  log "building + starting the ONYX platform stack (this first build compiles the daemons)..."
  docker compose --profile${PROFILES:+ $PROFILES} up -d --build
else
  log "starting the ONYX platform stack (existing images)"
  docker compose --profile${PROFILES:+ $PROFILES} up -d
fi

# --- 4. Wait for ingress + IdP -------------------------------------------------
if [ "$NPM_MODE" = "local" ]; then
  log "waiting for Nginx Proxy Manager API (http://127.0.0.1:81) ..."
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null http://127.0.0.1:81/ && break
    sleep 5
  done
fi

# Use the configured Authentik endpoint (local or external).
AUTHENTIK_URL="${AUTHENTIK_URL:-http://127.0.0.1:9000}"
log "waiting for Authentik (${AUTHENTIK_URL}) ..."
for _ in $(seq 1 60); do
  curl -sf -o /dev/null "${AUTHENTIK_URL}/-/health/live/" && break
  sleep 5
done

# --- 5. Authentik provider (SSO) ------------------------------------------------
if [ "$SKIP_AUTH" = 0 ]; then
  log "provisioning Authentik (bootstrap + ONYX OIDC application + passkeys)..."
  bash scripts/provision-authentik.sh || log "authentik provisioning reported a problem — see above"
else
  log "skipping Authentik provisioning (--skip-auth)"
fi

# --- 6. Device trust (docs/design/11 §10) — per DEVICE_TRUST ----------------------
# Passkeys are provisioned with Authentik in step 4 regardless of mode.
DT_SUBDOMAINS="${DEVICE_TRUST_SUBDOMAINS:-app admin}"
case "$DEVICE_TRUST" in
  off)
    log "device trust: off (passkeys remain available via Authentik)"
    ;;
  local)
    PKI_DIR="${ONYX_PKI_DIR:-/etc/onyx/pki}"
    if mkdir -p "$PKI_DIR" 2>/dev/null && [ -w "$PKI_DIR" ]; then
      log "device trust: local — ensuring device CA in $PKI_DIR ..."
      bash scripts/provision-device-trust.sh || log "device-trust CA setup reported a problem — see above"
    else
      log "cannot write $PKI_DIR — run 'sudo scripts/provision-device-trust.sh' once, then re-run"
    fi
    ;;
  cerulean)
    log "device trust: cerulean — requesting mTLS gate from the Cerulean API ..."
    : "${CERULEAN_API_URL:?set CERULEAN_API_URL in .env (docs/design/11 §10.3)}"
    : "${CERULEAN_API_TOKEN:?set CERULEAN_API_TOKEN in .env}"
    : "${FLEET_ID:?set FLEET_ID in .env}"
    [ -n "$DT_SUBDOMAINS" ] || DT_SUBDOMAINS="app admin"
    dt_json="["; first=1
    for s in $DT_SUBDOMAINS; do
      [ "$first" = 1 ] || dt_json+="," 
      dt_json+="\"$s\""
      first=0
    done
    dt_json+=']'
    code="$(curl -s -o /tmp/cerulean-dt-response -w '%{http_code}' \
      -X POST "${CERULEAN_API_URL%/}/api/v1/fleet/${FLEET_ID}/device-trust" \
      -H "Authorization: Bearer ${CERULEAN_API_TOKEN}" -H "Content-Type: application/json" \
      -d "{\"domain\":\"${DOMAIN}\",\"subdomains\":${dt_json},\"enforcement\":\"mtls\",\"client_ca\":\"cerulean-managed\"}")"
    if [ "$code" = 000 ]; then
      log "cerulean API unreachable at ${CERULEAN_API_URL} — fix CERULEAN_API_URL or set DEVICE_TRUST=off"
    elif [ "$code" = 200 ] || [ "$code" = 201 ] || [ "$code" = 202 ]; then
      log "cerulean accepted the device-trust gate for: ${DT_SUBDOMAINS} — issue device certs from the Cerulean dashboard"
    else
      log "cerulean device-trust request returned HTTP ${code} — check the Cerulean dashboard/API"
    fi
    rm -f /tmp/cerulean-dt-response
    ;;
esac

# --- 7. Nginx Proxy Manager: wildcard cert + subdomains -------------------------
if [ "$NPM_MODE" = "cerulean" ]; then
  log "edge is Cerulean-managed (NPM_MODE=cerulean) — skipping local NPM provisioning"
else
  log "provisioning NPM: wildcard *.${DOMAIN} via TSIG (RFC 2136) + proxy hosts (mTLS gate)..."
  # shellcheck disable=SC2153
  python3 scripts/npm-proxy-hosts.py
fi

# --- 8. Summary -----------------------------------------------------------------
cat <<EOF

ONYX platform is up. Public endpoints (once DNS for *.${DOMAIN} points here):

  App       https://app.${DOMAIN}
  API       https://api.${DOMAIN}
  Identity  https://auth.${DOMAIN}     (Authentik — first login creates the admin)
  Storage   https://storage.${DOMAIN}  (S3-compatible, static S3_ACCESS_KEY)
  Backup    https://backup.${DOMAIN}
  Admin     https://admin.${DOMAIN}

Device trust: ${DEVICE_TRUST} (docs/design/11 §10).
EOF
if [ "$DEVICE_TRUST" = "local" ]; then
  cat <<'EOF'
  Enroll a device:  scripts/provision-device-trust.sh issue <name>
  Install the .p12 via your MDM, then sign in with a passkey.
EOF
elif [ "$DEVICE_TRUST" = "cerulean" ]; then
  cat <<'EOF'
  Certificates are issued from the Cerulean dashboard (MDM-managed);
  the mTLS gate on app/admin was requested from the Cerulean API.
EOF
fi
cat <<EOF
EOF
if [ "$NPM_MODE" = "local" ]; then
  echo "NPM admin UI: http://127.0.0.1:81  (login with NPM_EMAIL / NPM_PASSWORD)"
else
  echo "Edge: Cerulean-managed (NPM_MODE=cerulean) — no local NPM; routes + certs live in Cerulean."
fi
echo "Docs: docs/design/11-platform-and-cloud.md · https://innotelinc.github.io/onyx/docs/"

# ── SecretOps — Cerulean Vault ──────────────────────────────────────
# The platform's only secret store. Nothing to provision here: Vault runs
# centrally as `cerulean-vault`, this stack reads it with its own path-scoped
# token (./data/vault/token/onyx.token, minted there), and `vault://` references
# in .env resolve at service startup. Move values in with the shared migrator:
#   python3 scripts/vault-migrate.py --from-env-file .env --keys S3_ACCESS_KEY,S3_SECRET_KEY
if grep -qE '^S3_ACCESS_KEY=vault://' .env 2>/dev/null; then
  if [ -s ./data/vault/token/onyx.token ]; then
    echo ">> SecretOps: Cerulean Vault (token present, S3 credentials resolve at startup)"
  else
    echo "!! S3_ACCESS_KEY is a vault:// reference but ./data/vault/token/onyx.token is" >&2
    echo "   missing — copy it from Cerulean ('data/vault/token/onyx.token' there)." >&2
  fi
fi
