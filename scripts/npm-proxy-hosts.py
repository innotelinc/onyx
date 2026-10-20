#!/usr/bin/env python3
"""Provision Nginx Proxy Manager for the ONYX platform (docs/design/11 §3).

Integrated into setup.sh. Idempotent end to end:

  1. log in to the NPM API (POST /api/tokens);
  2. ensure a Let's Encrypt WILDCARD certificate for *.DOMAIN + DOMAIN,
     issued via DNS-01 with the rfc2136 provider (RFC 2136 dynamic update,
     TSIG key) — the same pattern as the other innotelinc platform projects;
  3. ensure an NPM proxy host for every entry in NPM_SUBDOMAINS — `@` is the
     apex, where the console is served — each with the wildcard cert, SSL
     forced, websockets where enabled;
  4. keep the retired `app.<domain>` console host pointing at the apex with a
     301 when nothing else claims that name;
  5. print the final URL table.

Configuration comes from the environment (see .env.example):

  NPM_BASE_URL     http://127.0.0.1:81            (NPM API)
  NPM_EMAIL        admin account
  NPM_PASSWORD     admin account
  DOMAIN           onyx.innotel.us
  DNS_NAMESERVER   ns.innotel.us:53               (RFC 2136 target)
  TSIG_KEY_NAME    onyx-key.
  TSIG_KEY_SECRET  base64 TSIG secret
  TSIG_KEY_ALGORITHM  hmac-sha256
  NPM_SUBDOMAINS   "@=onyx-web:80 api=onyx-api:8080 ..." (space separated;
                   `@` is the apex, where the console is served)

No third-party packages: stdlib urllib only. Exits non-zero on any API error.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("NPM_BASE_URL", "http://127.0.0.1:81").rstrip("/")
EMAIL = os.environ.get("NPM_EMAIL", "")
PASSWORD = os.environ.get("NPM_PASSWORD", "")
DOMAIN = os.environ.get("DOMAIN", "onyx.innotel.us").strip().lstrip(".")
NAMESERVER = os.environ.get("DNS_NAMESERVER", "")
TSIG_NAME = os.environ.get("TSIG_KEY_NAME", "")
TSIG_SECRET = os.environ.get("TSIG_KEY_SECRET", "")
TSIG_ALGO = os.environ.get("TSIG_KEY_ALGORITHM", "hmac-sha256")

# Device trust (docs/design/11 §10): hosts gated by TLS client certs.
# Space-separated subdomain names (`@` is the apex); empty disables the gate.
DEVICE_TRUST_SUBDOMAINS = [
    s.strip()
    for s in os.environ.get("DEVICE_TRUST_SUBDOMAINS", "admin").replace(",", " ").split()
    if s.strip()
]
# The device CA is generated on the host by scripts/provision-device-trust.sh
# and bind-mounted into the NPM container at /etc/letsencrypt/onyx-device-ca
# (docker-compose.yml), so that path is hardcoded in the generated block.


def with_device_trust(existing: str, subdomain: str) -> str:
    """Merge the mTLS directives into a proxy host's advanced config.

    Idempotent: a previously managed block is replaced in place; anything the
    operator added is preserved. When subdomain is not gated, a stale managed
    block is stripped so re-runs converge in both directions.
    """
    begin, end = "# managed by npm-proxy-hosts.py (device trust", "# end device trust"
    text = existing or ""
    # strip any previous managed block first
    if begin in text:
        pre, rest = text.split(begin, 1)
        _, post = rest.split(end, 1)
        text = (pre + post).lstrip("\n")
    if subdomain not in DEVICE_TRUST_SUBDOMAINS:
        return text
    block = (
        f"{begin}, docs/design/11 §10)\n"
        "ssl_client_certificate /etc/letsencrypt/onyx-device-ca/ca.crt;\n"
        "ssl_verify_client on;\n"
        "ssl_verify_depth 2;\n"
        f"{end}\n"
    )
    return (block + text).strip() + "\n" if text.strip() else block


# WebDAV ingress (docs/design/05 §5). onyx-davd serves /webdav/<share>, but it
# authenticates nobody itself: it requires the identity header its gateway
# asserts and refuses a request that has none. So the gateway has to assert it,
# and the username comes from the Authentik forward-auth header — which is what
# makes the WebDAV connection string the Shares page copies out a URL that works
# instead of one that answers 401.
WEBDAV_SUBDOMAIN = os.environ.get("WEBDAV_SUBDOMAIN", "@").strip() or "@"
WEBDAV_TARGET = os.environ.get("WEBDAV_TARGET", "onyx-davd:8081").strip()
WEBDAV_IDENTITY_HEADER = "X-Onyx-User"
WEBDAV_IDENTITY_SOURCE = "$http_x_authentik_username"


def webdav_location() -> dict:
    """The /webdav location added to the WebDAV subdomain's proxy host."""
    host, _, port = WEBDAV_TARGET.partition(":")
    return {
        "path": "/webdav",
        "forward_scheme": "http",
        "forward_host": host,
        "forward_port": int(port or 8081),
        # The path is preserved: davd routes on /webdav/<share>, so a rewrite
        # would send every request to a share named nothing.
        "forward_path": "/webdav",
        "advanced_config": (
            "# managed by npm-proxy-hosts.py (webdav identity, docs/design/05 §5)\n"
            f"proxy_set_header {WEBDAV_IDENTITY_HEADER} {WEBDAV_IDENTITY_SOURCE};\n"
        ),
    }


def hostname_for(sub: str) -> str:
    """The hostname a NPM_SUBDOMAINS entry names. `@` (or nothing) is the apex."""
    sub = sub.strip()
    return DOMAIN if sub in ("", "@") else f"{sub}.{DOMAIN}"


def die(msg: str) -> None:
    print(f"[npm-proxy-hosts] error: {msg}", file=sys.stderr)
    sys.exit(1)


def api(method: str, path: str, token: str, body: dict | None = None) -> dict:
    req = urllib.request.Request(BASE + path, method=method)
    req.add_header("Authorization", f"Bearer {token}")
    req.add_header("Content-Type", "application/json")
    data = json.dumps(body).encode() if body is not None else None
    try:
        with urllib.request.urlopen(req, data=data, timeout=30) as resp:
            raw = resp.read()
            if not raw:
                return {}
            return json.loads(raw)
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")[:300]
        die(f"{method} {path} -> HTTP {e.code}: {detail}")
    except Exception as e:  # noqa: BLE001 - surface any transport failure
        die(f"{method} {path} -> {e}")


def login() -> str:
    if not EMAIL or not PASSWORD:
        die("NPM_EMAIL / NPM_PASSWORD are required (set them in .env)")
    body = api("POST", "/api/tokens", "", {"identity": EMAIL, "secret": PASSWORD})
    token = body.get("token")
    if not token:
        die("login failed: no token returned (wrong NPM credentials?)")
    return token


def ensure_wildcard_cert(token: str) -> int:
    wildcard = f"*.{DOMAIN}"
    certs = api("GET", "/api/nginx/certificates", token)
    if not isinstance(certs, list):
        die("GET /api/nginx/certificates returned an unexpected shape")
    for c in certs:
        names = c.get("domain_names") or []
        if wildcard in names:
            print(f"  certificate: reuse existing id={c.get('id')} for {wildcard}")
            return int(c["id"])

    if not NAMESERVER or not TSIG_NAME or not TSIG_SECRET:
        die(
            "wildcard certificate requested but DNS_NAMESERVER / TSIG_KEY_NAME / "
            "TSIG_KEY_SECRET are not set (RFC 2136 DNS-01 challenge)"
        )

    creds = "\n".join(
        [
            "# ONYX wildcard cert — RFC 2136 (TSIG) DNS challenge",
            f"dns_rfc2136_server = {NAMESERVER}",
            f"dns_rfc2136_name = {TSIG_NAME}",
            f"dns_rfc2136_secret = {TSIG_SECRET}",
            f"dns_rfc2136_algorithm = {TSIG_ALGO}",
        ]
    )
    payload = {
        "provider": "letsencrypt",
        "domain_names": [wildcard, DOMAIN],
        "meta": {
            "letsencrypt_agree": True,
            "dns_challenge": True,
            "dns_provider": "rfc2136",
            "dns_provider_credentials": creds,
            "propagation_seconds": 30,
        },
    }
    cert = api("POST", "/api/nginx/certificates", token, payload)
    cid = cert.get("id")
    if not cid:
        die("certificate request accepted but no id returned")
    print(f"  certificate: requested id={cid} for {wildcard} (issuance runs in background)")
    return int(cid)


def ensure_proxy_hosts(token: str, cert_id: int) -> list[dict]:
    subdomains: dict[str, str] = {}
    for pair in os.environ.get("NPM_SUBDOMAINS", "").split():
        if "=" not in pair:
            continue
        sub, target = pair.split("=", 1)
        subdomains[sub.strip()] = target.strip()
    if not subdomains:
        subdomains = {
            "@": "onyx-web:80",
            "api": "onyx-api:8080",
            "auth": "authentic-server:9000",
            "storage": "onyx-objectstore:9000",
            "backup": "onyx-backupd:8084",
            "admin": "onyx-api:8080",
        }
        print("  subdomains: NPM_SUBDOMAINS unset, using platform defaults")

    hosts = api("GET", "/api/nginx/proxy-hosts", token)
    if not isinstance(hosts, list):
        die("GET /api/nginx/proxy-hosts returned an unexpected shape")
    by_domain: dict[str, dict] = {}
    for h in hosts:
        for name in h.get("domain_names") or []:
            by_domain[name] = h

    results = []
    for sub, target in sorted(subdomains.items()):
        fqdn = hostname_for(sub)
        host, _, port = target.partition(":")
        existing = by_domain.get(fqdn)
        gated = "mTLS" if sub in DEVICE_TRUST_SUBDOMAINS else "open"
        desired = {
            "domain_names": [fqdn],
            "forward_scheme": "http",
            "forward_host": host,
            "forward_port": int(port or 80),
            "certificate_id": cert_id,
            "ssl_forced": True,
            "block_exploits": True,
            "caching_enabled": False,
            "allow_websocket_upgrade": sub in ("@", "app", "api", "admin"),
            "access_list_id": "0",
            "advanced_config": with_device_trust(existing.get("advanced_config") if existing else "", sub),
            # The WebDAV surface lives under /webdav on one subdomain (default
            # the console host, so the URL the Shares page advertises resolves).
            "locations": [webdav_location()] if sub == WEBDAV_SUBDOMAIN else [],
            "hsts_enabled": False,
            "hsts_subdomains": False,
            "http2_support": True,
            "meta": {"letsencrypt_agree": True, "dns_challenge": True},
        }
        if existing:
            api("PUT", f"/api/nginx/proxy-hosts/{existing['id']}", token, desired)
            print(f"  proxy host: updated {fqdn} -> {host}:{port} (id={existing['id']}, {gated})")
            results.append({"fqdn": fqdn, "target": f"{host}:{port}", "action": "updated", "gate": gated})
        else:
            created = api("POST", "/api/nginx/proxy-hosts", token, desired)
            print(f"  proxy host: created {fqdn} -> {host}:{port} (id={created.get('id')}, {gated})")
            results.append({"fqdn": fqdn, "target": f"{host}:{port}", "action": "created", "gate": gated})
    return results


# The console used to live at app.<domain>. Moving it to the apex would break
# every bookmark and every URL already handed out, so the old name is kept as a
# permanent 301 instead of being deleted. NPM refuses a redirect on a name that
# a proxy host still uses, so an operator who has re-pointed `app` at something
# else keeps their host and is told about it.
RETIRED_CONSOLE = "app"


def console_redirect_payload(cert_id: int) -> dict:
    return {
        "domain_names": [hostname_for(RETIRED_CONSOLE)],
        "forward_scheme": "https",
        "forward_domain_name": DOMAIN,
        "forward_http_code": 301,
        "preserve_path": True,
        "certificate_id": cert_id,
        "ssl_forced": True,
        "http2_support": True,
        "block_exploits": True,
        # NPM's schema requires the column even when the redirect needs no
        # advanced directives; omitting it is a 500 from the insert.
        "advanced_config": "",
        # The host only redirects; it never asks for a certificate of its own.
        "meta": {"letsencrypt_agree": False, "dns_challenge": False},
    }


def ensure_console_redirect(token: str, cert_id: int) -> str:
    fqdn = hostname_for(RETIRED_CONSOLE)
    proxies = api("GET", "/api/nginx/proxy-hosts", token)
    if isinstance(proxies, list) and any(fqdn in (h.get("domain_names") or []) for h in proxies):
        print(f"  redirect: {fqdn} is still a proxy host, left alone")
        return "left alone"

    payload = console_redirect_payload(cert_id)
    redirects = api("GET", "/api/nginx/redirection-hosts", token)
    if isinstance(redirects, list):
        for r in redirects:
            if fqdn in (r.get("domain_names") or []):
                api("PUT", f"/api/nginx/redirection-hosts/{r['id']}", token, payload)
                print(f"  redirect: {fqdn} -> https://{DOMAIN} (301, updated)")
                return "updated"
    api("POST", "/api/nginx/redirection-hosts", token, payload)
    print(f"  redirect: {fqdn} -> https://{DOMAIN} (301, created)")
    return "created"


def main() -> None:
    print(f"[npm-proxy-hosts] provisioning NPM at {BASE} for *.{DOMAIN}")
    token = login()
    cert_id = ensure_wildcard_cert(token)
    results = ensure_proxy_hosts(token, cert_id)
    redirect = ensure_console_redirect(token, cert_id)

    print("\nONYX platform endpoints (provisioned):")
    width = max(len(r["fqdn"]) for r in results)
    for r in results:
        print(f"  https://{r['fqdn']:<{width}}  ->  {r['target']}  ({r['action']}, {r['gate']})")
    print(
        "\nWebDAV: https://{fqdn}/webdav/<share> -> {target} (identity header "
        "{header}, from Authentik forward-auth)".format(
            fqdn=hostname_for(WEBDAV_SUBDOMAIN), target=WEBDAV_TARGET, header=WEBDAV_IDENTITY_HEADER
        )
    )
    print(f"Old console host: https://{hostname_for(RETIRED_CONSOLE)} -> https://{DOMAIN} ({redirect})")
    if DEVICE_TRUST_SUBDOMAINS:
        print(
            "\nDevice trust: {subs} require a client certificate from the ONYX device CA.\n"
            "Issue device certs with: scripts/provision-device-trust.sh issue <name>".format(
                subs=", ".join(f"https://{hostname_for(s)}" for s in DEVICE_TRUST_SUBDOMAINS)
            )
        )


if __name__ == "__main__":
    main()
