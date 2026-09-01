#!/usr/bin/env python3
"""Provision Nginx Proxy Manager for the ONYX platform (docs/design/11 §3).

Integrated into setup.sh. Idempotent end to end:

  1. log in to the NPM API (POST /api/tokens);
  2. ensure a Let's Encrypt WILDCARD certificate for *.DOMAIN + DOMAIN,
     issued via DNS-01 with the rfc2136 provider (RFC 2136 dynamic update,
     TSIG key) — the same pattern as the other innotelinc platform projects;
  3. ensure an NPM proxy host for every subdomain in NPM_SUBDOMAINS,
     each with the wildcard cert, SSL forced, websockets where enabled;
  4. print the final URL table.

Configuration comes from the environment (see .env.example):

  NPM_BASE_URL     http://127.0.0.1:81            (NPM API)
  NPM_EMAIL        admin account
  NPM_PASSWORD     admin account
  DOMAIN           onyx.innotel.us
  DNS_NAMESERVER   ns.innotel.us:53               (RFC 2136 target)
  TSIG_KEY_NAME    onyx-key.
  TSIG_KEY_SECRET  base64 TSIG secret
  TSIG_KEY_ALGORITHM  hmac-sha256
  NPM_SUBDOMAINS   "app=onyx-web:80 api=onyx-api:8080 ..." (space separated)

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
            "app": "onyx-web:80",
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
        fqdn = f"{sub}.{DOMAIN}"
        host, _, port = target.partition(":")
        desired = {
            "domain_names": [fqdn],
            "forward_scheme": "http",
            "forward_host": host,
            "forward_port": int(port or 80),
            "certificate_id": cert_id,
            "ssl_forced": True,
            "block_exploits": True,
            "caching_enabled": False,
            "allow_websocket_upgrade": sub in ("app", "api", "admin"),
            "access_list_id": "0",
            "advanced_config": "",
            "locations": [],
            "hsts_enabled": False,
            "hsts_subdomains": False,
            "http2_support": True,
            "meta": {"letsencrypt_agree": True, "dns_challenge": True},
        }
        existing = by_domain.get(fqdn)
        if existing:
            api("PUT", f"/api/nginx/proxy-hosts/{existing['id']}", token, desired)
            print(f"  proxy host: updated {fqdn} -> {host}:{port} (id={existing['id']})")
            results.append({"fqdn": fqdn, "target": f"{host}:{port}", "action": "updated"})
        else:
            created = api("POST", "/api/nginx/proxy-hosts", token, desired)
            print(f"  proxy host: created {fqdn} -> {host}:{port} (id={created.get('id')})")
            results.append({"fqdn": fqdn, "target": f"{host}:{port}", "action": "created"})
    return results


def main() -> None:
    print(f"[npm-proxy-hosts] provisioning NPM at {BASE} for *.{DOMAIN}")
    token = login()
    cert_id = ensure_wildcard_cert(token)
    results = ensure_proxy_hosts(token, cert_id)

    print("\nONYX platform endpoints (provisioned):")
    width = max(len(r["fqdn"]) for r in results)
    for r in results:
        print(f"  https://{r['fqdn']:<{width}}  ->  {r['target']}  ({r['action']})")


if __name__ == "__main__":
    main()
