#!/usr/bin/env bash
# Device trust for the ONYX platform (docs/design/11 §10).
#
# Maintains a minimal internal PKI whose client certificates gate management
# surfaces at the edge (NPM mTLS), plus a helper to issue per-device certs for
# MDM distribution:
#
#   scripts/provision-device-trust.sh                     # ensure the CA exists (setup.sh)
#   scripts/provision-device-trust.sh issue <name> [days] # issue a client cert for a device
#
# Layout under $ONYX_PKI_DIR (default /etc/onyx/pki; sudo may be needed):
#
#   ca.crt / ca.key          ECDSA P-256 self-signed device CA (10 years)
#   serial                   openssl tracking file
#   issued/<name>.crt/.key   per-device client certs (default 365 days)
#
# The CA cert is mounted into the NPM container (docker-compose.yml) and used
# by ssl_verify_client in the proxy hosts' advanced config. Private keys are
# created mode 0600 and never leave the host; distribute <name>.p12 to the
# device via MDM (Apple Configurator/.mobileconfig, Intune, or manual import).
#
# Requires: openssl. Idempotent: re-runs converge (existing CA/certs kept).
set -euo pipefail

PKI_DIR="${ONYX_PKI_DIR:-/etc/onyx/pki}"
DAYS_DEFAULT="${DEVICE_CERT_DAYS:-365}"

usage() {
  sed -n '2,14p' "$0"
  exit "${1:-0}"
}

need_openssl() {
  command -v openssl >/dev/null 2>&1 || { echo "error: openssl is required" >&2; exit 1; }
}

ensure_ca() {
  need_openssl
  mkdir -p "$PKI_DIR/issued"

  if [ -f "$PKI_DIR/ca.crt" ] && [ -f "$PKI_DIR/ca.key" ]; then
    echo ">> [device-trust] CA already present at $PKI_DIR/ca.crt"
    return
  fi

  echo ">> [device-trust] generating device CA (ECDSA P-256, 10 years) in $PKI_DIR ..."
  # -noenc: the CA key lives 0600 on the appliance; a prompt would break setup.sh.
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
    -keyout "$PKI_DIR/ca.key" -out "$PKI_DIR/ca.crt" \
    -days 3650 -nodes -sha256 \
    -subj "/C=US/O=ONYX/OU=Device Trust/CN=ONYX Device CA" >/dev/null 2>&1
  : > "$PKI_DIR/serial"
  echo "   1000" > "$PKI_DIR/serial"
  chmod 700 "$PKI_DIR"
  chmod 600 "$PKI_DIR/ca.key"
  chmod 644 "$PKI_DIR/ca.crt"
  echo ">> [device-trust] CA ready — enroll devices with: $0 issue <name>"
}

next_serial() {
  local cur
  cur="$(tail -1 "$PKI_DIR/serial" 2>/dev/null | tr -d '[:space:]')"
  [ -n "$cur" ] || cur="1000"
  echo $((16#$cur + 1))
}

issue_cert() {
  local name="${1:?usage: $0 issue <name> [days]}"
  local days="${2:-$DAYS_DEFAULT}"
  need_openssl

  if ! [[ "$name" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]]; then
    echo "error: device name must match [a-zA-Z0-9][a-zA-Z0-9._-]*" >&2
    exit 1
  fi

  local crt="$PKI_DIR/issued/$name.crt" key="$PKI_DIR/issued/$name.key"
  local p12="$PKI_DIR/issued/$name.p12" csr="$PKI_DIR/issued/$name.csr"

  if [ -f "$crt" ]; then
    local end
    end="$(openssl x509 -enddate -noout -in "$crt" | cut -d= -f2)"
    echo ">> [device-trust] certificate for '$name' already exists (expires: $end)"
    echo "   delete $crt first to reissue, or use a new device name."
    return
  fi

  echo ">> [device-trust] issuing client cert '$name' (ECDSA P-256, $days days) ..."
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
    -keyout "$key" -out "$csr" -nodes \
    -subj "/C=US/O=ONYX Device Trust/OU=device=$name/CN=$name" >/dev/null 2>&1

  local serial
  serial="$(next_serial)"
  openssl x509 -req -in "$csr" -CA "$PKI_DIR/ca.crt" -CAkey "$PKI_DIR/ca.key" \
    -set_serial "$serial" -days "$days" -sha256 \
    -extfile <(printf "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=clientAuth\n") \
    -out "$crt" >/dev/null 2>&1

  # PKCS#12 bundle for MDM/OS import (passphrase-less; handle per policy).
  openssl pkcs12 -export -in "$crt" -inkey "$key" \
    -certfile "$PKI_DIR/ca.crt" -name "ONYX device: $name" \
    -passout pass: -out "$p12" >/dev/null 2>&1

  echo "$serial" >> "$PKI_DIR/serial"
  chmod 600 "$key"
  rm -f "$csr"

  cat <<EOF

>> [device-trust] issued:
     cert:      $crt
     key:       $key          (mode 0600 — keep on the host)
     pkcs12:    $p12   (install on the device via MDM / OS trust store)

   After installing, verify:  curl --cert $crt --key $key https://app.\${DOMAIN:-onyx.innotel.us}/
   Edge gate is (re)applied by scripts/npm-proxy-hosts.py (DEVICE_TRUST_SUBDOMAINS).
EOF
}

case "${1:-ensure}" in
  ensure) ensure_ca ;;
  issue) shift; issue_cert "$@" ;;
  -h|--help|help) usage 0 ;;
  *) usage 1 ;;
esac
