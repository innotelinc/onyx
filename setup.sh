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
# A remote edge implies remote device trust unless explicitly local.
if [ "$NPM_MODE" = "cerulean" ] && [ "$DEVICE_TRUST" = "local" ]; then
  echo "warning: DEVICE_TRUST=local with NPM_MODE=cerulean issues certs no local edge enforces — using DEVICE_TRUST=cerulean" >&2
  DEVICE_TRUST="cerulean"
fi
case "$DEVICE_TRUST" in
  off|local|cerulean) ;;
  *) echo "error: DEVICE_TRUST must be off, local or cerulean (got: $DEVICE_TRUST)" >&2; exit 1 ;;
esac

# --- 2. Bring the stack up -----------------------------------------------------
if [ "$NPM_MODE" = "cerulean" ]; then
  # The edge is remote (Cerulean-managed): exclude the local NPM and its
  # published 80/443/81 from this host entirely.
  log "NPM_MODE=cerulean — starting the stack without the local NPM ..."
  if [ "$BUILD" = 1 ]; then
    docker compose up -d --build --scale nginx-proxy-manager=0
  else
    docker compose up -d --scale nginx-proxy-manager=0
  fi
elif [ "$BUILD" = 1 ]; then
  log "building + starting the ONYX platform stack (this first build compiles the daemons)..."
  docker compose up -d --build
else
  log "starting the ONYX platform stack (existing images)"
  docker compose up -d
fi

# --- 3. Wait for ingress + IdP -------------------------------------------------
if [ "$NPM_MODE" = "local" ]; then
  log "waiting for Nginx Proxy Manager API (http://127.0.0.1:81) ..."
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null http://127.0.0.1:81/ && break
    sleep 5
  done
fi

log "waiting for Authentik (http://127.0.0.1:9000) ..."
for _ in $(seq 1 60); do
  curl -sf -o /dev/null http://127.0.0.1:9000/-/health/live/ && break
  sleep 5
done

# --- 4. Authentik provider (SSO) ------------------------------------------------
if [ "$SKIP_AUTH" = 0 ]; then
  log "provisioning Authentik (bootstrap + ONYX OIDC application + passkeys)..."
  bash scripts/provision-authentik.sh || log "authentik provisioning reported a problem — see above"
else
  log "skipping Authentik provisioning (--skip-auth)"
fi

# --- 5. Device trust (docs/design/11 §10) — per DEVICE_TRUST ----------------------
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

# --- 6. Nginx Proxy Manager: wildcard cert + subdomains -------------------------
if [ "$NPM_MODE" = "cerulean" ]; then
  log "edge is Cerulean-managed (NPM_MODE=cerulean) — skipping local NPM provisioning"
else
  log "provisioning NPM: wildcard *.${DOMAIN} via TSIG (RFC 2136) + proxy hosts (mTLS gate)..."
  # shellcheck disable=SC2153
  python3 scripts/npm-proxy-hosts.py
fi

# --- 7. Summary -----------------------------------------------------------------
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

# ── Infisical (SecretOps) — opt-in secret provisioning ──────────────
# Secrets for the Innotel Platform Stack live in Infisical. Enable by
# setting INFISICAL_ADMIN_EMAIL / INFISICAL_ADMIN_PASSWORD and the
# INFISICAL_* keys in .env, then re-run setup (idempotent).
if grep -qE '^INFISICAL_ADMIN_EMAIL=.+' .env 2>/dev/null && \
   grep -qE '^INFISICAL_ADMIN_PASSWORD=.+' .env 2>/dev/null; then
  __root="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd)"
  case "$__root" in
    */scripts) __root="$(dirname "$__root")" ;;
  esac
  if [ -f "$__root/scripts/infisical-setup.sh" ]; then
    echo ">> provisioning secrets into Infisical (SecretOps)..."
    bash "$__root/scripts/infisical-setup.sh" \
      || echo "!! infisical setup failed (see above); .env values remain valid" >&2
  fi
  unset __root
fi
