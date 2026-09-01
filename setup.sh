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

# --- 2. Bring the stack up -----------------------------------------------------
if [ "$BUILD" = 1 ]; then
  log "building + starting the ONYX platform stack (this first build compiles the daemons)..."
  docker compose up -d --build
else
  log "starting the ONYX platform stack (existing images)..."
  docker compose up -d
fi

# --- 3. Wait for ingress + IdP -------------------------------------------------
log "waiting for Nginx Proxy Manager API (http://127.0.0.1:81) ..."
for _ in $(seq 1 60); do
  curl -sf -o /dev/null http://127.0.0.1:81/ && break
  sleep 5
done

log "waiting for Authentik (http://127.0.0.1:9000) ..."
for _ in $(seq 1 60); do
  curl -sf -o /dev/null http://127.0.0.1:9000/-/health/live/ && break
  sleep 5
done

# --- 4. Authentik provider (SSO) ------------------------------------------------
if [ "$SKIP_AUTH" = 0 ]; then
  log "provisioning Authentik (bootstrap + ONYX OIDC application)..."
  bash scripts/provision-authentik.sh || log "authentik provisioning reported a problem — see above"
else
  log "skipping Authentik provisioning (--skip-auth)"
fi

# --- 5. Nginx Proxy Manager: wildcard cert + subdomains -------------------------
log "provisioning NPM: wildcard *.${DOMAIN} via TSIG (RFC 2136) + proxy hosts..."
# shellcheck disable=SC2153
python3 scripts/npm-proxy-hosts.py

# --- 6. Summary -----------------------------------------------------------------
cat <<EOF

ONYX platform is up. Public endpoints (once DNS for *.${DOMAIN} points here):

  App       https://app.${DOMAIN}
  API       https://api.${DOMAIN}
  Identity  https://auth.${DOMAIN}     (Authentik — first login creates the admin)
  Storage   https://storage.${DOMAIN}  (S3-compatible, static S3_ACCESS_KEY)
  Backup    https://backup.${DOMAIN}
  Admin     https://admin.${DOMAIN}

NPM admin UI: http://127.0.0.1:81  (login with NPM_EMAIL / NPM_PASSWORD)
Docs: docs/design/11-platform-and-cloud.md
EOF
