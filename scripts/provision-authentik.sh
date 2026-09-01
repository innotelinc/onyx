#!/usr/bin/env bash
# Provision Authentik for the ONYX platform (docs/design/11 §2), called from
# setup.sh. Idempotent:
#   1. wait for the Authentik API (bootstrap token auth),
#   2. create the "ONYX Platform" OAuth2 provider (confidential client) with
#      redirect URIs for app/admin/loopback,
#   3. create the "ONYX Platform" application bound to that provider,
#   4. write AUTHENTIK_CLIENT_ID / AUTHENTIK_CLIENT_SECRET back into .env.
#
# Requires: curl, python3. Env: see .env.example (AUTHENTIK_BOOTSTRAP_TOKEN,
# AUTHENTIK_BOOTSTRAP_EMAIL, DOMAIN, ...).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT/.env"
AUTH_URL="${AUTHENTIK_URL:-http://127.0.0.1:9000}"
DOMAIN="${DOMAIN:-onyx.innotel.us}"

if [ -f "$ENV_FILE" ]; then
  set -a; # shellcheck disable=SC1091
  source "$ENV_FILE"
  set +a
fi

: "${AUTHENTIK_BOOTSTRAP_TOKEN:?AUTHENTIK_BOOTSTRAP_TOKEN is required (see .env)}"

json() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"; }

echo ">> [authentik] waiting for API at ${AUTH_URL} ..."
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null "${AUTH_URL}/api/v3/core/users/me/" \
      -H "Authorization: Bearer ${AUTHENTIK_BOOTSTRAP_TOKEN}"; then
    break
  fi
  sleep 5
  echo "   ... still waiting"
done

AUTH_HEADER="Authorization: Bearer ${AUTHENTIK_BOOTSTRAP_TOKEN}"
if ! curl -sf -o /dev/null "${AUTH_URL}/api/v3/core/users/me/" -H "$AUTH_HEADER"; then
  echo "!! [authentik] API not reachable with the bootstrap token — skipping provider provisioning." >&2
  echo "   Create the ONYX OAuth2 provider manually in the Authentik UI at ${AUTH_URL}." >&2
  exit 0
fi

# Reuse an existing ONYX provider/application when present (idempotent).
EXISTING="$(curl -sf "${AUTH_URL}/api/v3/core/applications/?slug=onyx-platform" -H "$AUTH_HEADER")"
if [ "$(echo "$EXISTING" | json "d['pagination']['count']")" -gt 0 ]; then
  echo ">> [authentik] ONYX Platform application already exists — nothing to do."
  exit 0
fi

FLOW_PK="$(curl -sf "${AUTH_URL}/api/v3/flows/authorization/?slug=default-provider-authorization-implicit-consent" -H "$AUTH_HEADER" | json "d['results'][0]['pk']")"
SIGNING_PK="$(curl -sf "${AUTH_URL}/api/v3/crypto/certificatekeypairs/?name=authentik%20Self-signed%20Certificate" -H "$AUTH_HEADER" | json "d['results'][0]['pk']")"

CLIENT_ID="$(cat /proc/sys/kernel/random/uuid)"
CLIENT_SECRET="$(python3 -c 'import secrets; print(secrets.token_urlsafe(48))')"

echo ">> [authentik] creating OAuth2 provider 'ONYX Platform' ..."
PROVIDER="$(curl -sf -X POST "${AUTH_URL}/api/v3/providers/oauth2/" \
  -H "$AUTH_HEADER" -H "Content-Type: application/json" \
  -d "$(python3 - <<PY
import json
print(json.dumps({
    "name": "ONYX Platform",
    "authorization_flow": "$FLOW_PK",
    "signing_key": "$SIGNING_PK",
    "client_type": "confidential",
    "client_id": "$CLIENT_ID",
    "client_secret": "$CLIENT_SECRET",
    "redirect_uris": [
        "https://app.$DOMAIN/*",
        "https://admin.$DOMAIN/*",
        "http://127.0.0.1:8080/*",
        "http://localhost:8080/*",
    ],
    "allowed_redirect_uris": [
        "https://app.$DOMAIN/*",
        "https://admin.$DOMAIN/*",
        "http://127.0.0.1:8080/*",
        "http://localhost:8080/*",
    ],
}))
PY
)")"
PROVIDER_PK="$(echo "$PROVIDER" | json "d['pk']")"
echo "   provider pk=${PROVIDER_PK}"

echo ">> [authentik] creating application 'ONYX Platform' ..."
curl -sf -X POST "${AUTH_URL}/api/v3/core/applications/" \
  -H "$AUTH_HEADER" -H "Content-Type: application/json" \
  -d "$(python3 - <<PY
import json
print(json.dumps({
    "name": "ONYX Platform",
    "slug": "onyx-platform",
    "provider": $PROVIDER_PK,
    "launch_url": "https://app.$DOMAIN/",
}))
PY
)" >/dev/null
echo "   application slug=onyx-platform"

# Persist the client credentials for downstream services.
if [ -f "$ENV_FILE" ]; then
  sed -i "s|^AUTHENTIK_CLIENT_ID=.*|AUTHENTIK_CLIENT_ID=${CLIENT_ID}|" "$ENV_FILE"
  sed -i "s|^AUTHENTIK_CLIENT_SECRET=.*|AUTHENTIK_CLIENT_SECRET=${CLIENT_SECRET}|" "$ENV_FILE"
  echo ">> [authentik] wrote AUTHENTIK_CLIENT_ID / AUTHENTIK_CLIENT_SECRET to .env"
fi
echo ">> [authentik] done — SSO available at https://auth.${DOMAIN}/"
