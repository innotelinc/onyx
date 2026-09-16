# 🗄️ ONYX — Platform Stack Role

**Classification: StorageOps**

Storage and infrastructure: file/object storage, backups, snapshots, replication, NAS features, and virtualization storage.

This page declares ONYX's role in the
[**Innotel Platform Stack**](https://github.com/innotelinc/innotel-platform-stack) —
the canonical single-responsibility architecture. The stack is defined in exactly one
place; this page links each product to it and states what this platform owns, consumes,
provides, and explicitly does not own.

> **Convergence:** see the [**build-plane convergence plan**](https://github.com/innotelinc/innotel-platform-stack/blob/main/docs/convergence-onyx-olympus-distro-atlas.md)
> for the shared target across ONYX, Atlas, Distro and Olympus — one web UI, one
> terminal UI, one full-stack app builder, one OmniRoute. ONYX's part: keep
> StorageOps, consume the AI plane, and take the storage side of published builds
> (§6, §8 Phase 4).

## Owns

- File storage
- Object storage
- Backups
- Snapshots
- Replication
- Application storage
- Media storage
- NAS features
- Virtualization storage

## Provides

- Storage to Monarch, Oasis, Signara, and Capstone

## Consumes

- Authentik — identity, SSO
- Cerulean Vault — secrets, credentials
- NPM Edge — public routing, TLS termination at the edge

## Explicitly does NOT own

- Identity (Authentik)
- Billing (Magnate)
- Certificates / trust (Cerulean)

## Service map (ONYX-owned)

| Component | Technology | Job |
| --- | --- | --- |
| `onyx-api` | Go | Control API; reports SecretOps health on `GET /api/v1/system/status` |
| `onyx-objectstore` | Go | S3-compatible object storage (buckets, keys, AWS SigV4 + presigned URLs; HTTP Basic accepted for compatibility) |
| `onyx-appd` | Go | Application hosting / deployment companion |
| `onyx-ai` | Go | AI Storage Advisor + Backup Intelligence; consumes the shared OmniRoute (`OMNIROUTE_BASE_URL` + a gateway key, which may be a `vault://` reference — ONYX holds no provider key) and falls back to local heuristics |

## In the ecosystem

| Flow | Path |
| --- | --- |
| Identity | Cerulean's Authentik (`auth.cerulean.innotel.us`) → OIDC → ONYX sessions |
| Secrets | ONYX `.env` derives from Cerulean Vault (SecretOps) — `vault://` references, never committed |
| Trust | Cerulean issues DNS + per-zone wildcard TLS; NPM Edge fronts `storage.<zone>` and the admin hosts |
| Revenue | Magnate plans/entitlements gate storage tiers (optional) |
| Source of truth | This repository's `docs/stack.md` points back to the Innotel Platform Stack |

## Secrets (Cerulean Vault)

Secrets for this platform live in **Cerulean Vault** — HashiCorp Vault with the KV v2
engine, hosted by Cerulean. Credentials are written there and the stack's `.env` carries
`vault://<mount>/<path>#<key>` references rather than values:

    S3_ACCESS_KEY=vault://cerulean/onyx#S3_ACCESS_KEY

The platform runs Vault as `cerulean-vault` and mints this stack a path-scoped `onyx`
policy token (`VAULT_PRODUCT_TOKENS=onyx` there), which arrives as
`./data/vault/token/onyx.token`. Move the values across with the shared migrator:

```bash
# The migrator runs on the Vault host (or with VAULT_ADDR pointing at it).
export VAULT_ADDR=http://127.0.0.1:8200           # from a container: the stack's own VAULT_ADDR
export VAULT_TOKEN_FILE=./data/vault/token/onyx.token
python3 scripts/vault-migrate.py --dry-run        # what would move; nothing written
python3 scripts/vault-migrate.py --from-env-file .env \
    --keys S3_ACCESS_KEY,S3_SECRET_KEY            # seed from .env
```

There is no second store. Infisical was the predecessor and the `infisical://` scheme,
its compose profile, its setup scripts and the Go package that implemented it are **gone**
from this repo — a value that still carries an `infisical://` reference is a deployment
that has not finished moving, not a fallback this stack supports. `scripts/vault-migrate.py`
can still read an Infisical instance as a *source* (`VAULT_*` unset, `INFISICAL_*` set) for
the move itself.

### Runtime resolution

`vault://` resolution is live: the Go services resolve
references at startup, so switching a value in `.env` needs no code change and no restart
of anything but the service.

- `onyx-objectstore` — `S3_ACCESS_KEY` / `S3_SECRET_KEY` are
  `vault://cerulean/onyx#<KEY>` on a deployed stack. Plain values pass through, which
  is what a local run uses; nothing is written back to the store at boot (secrets get
  there through `scripts/vault-migrate.py`, which the services have no business
  duplicating).
- `onyx-api` — `CERULEAN_API_TOKEN` may be
  `vault://cerulean/onyx/api#CERULEAN_API_TOKEN`; SecretOps health is reported on
  `GET /api/v1/system/status` as `vault: ok | not-configured | error`.

A reference that cannot be resolved is a hard startup failure — an empty S3
credential would open the endpoint rather than close it — and a value that is neither a
`vault://` reference nor a plain value is passed through unchanged rather than guessed at.

Shared client: `services/vault/` (Cerulean Vault, KV v2). `services/vault/vault_test.go`
covers the reference grammar, the KV v2 envelope, the KV v1 refusal, the pass-through
cases and the status reporting.

For a checkout with no platform Vault, `compose.vault.yml` runs a dev-mode one:

```bash
docker compose -f compose.vault.yml --profile vault up -d
export VAULT_ADDR=http://127.0.0.1:8200
export VAULT_TOKEN=<VAULT_DEV_ROOT_TOKEN_ID>
python3 scripts/vault-migrate.py --from-env-file .env \
    --keys S3_ACCESS_KEY,S3_SECRET_KEY,CERULEAN_API_TOKEN
```

## Golden rules

- **Authentik = Identity** · **Cerulean Vault = Secrets** · **Cerulean = Trust** ·
  **ONYX = Storage** · **Magnate = Revenue** · **NPM Edge = Edge** — everything else is a business function.
- No platform duplicates another's responsibility.
- No credit in commits, footers, or headers to anyone but the project owner.

---

*ONYX · StorageOps · [Innotel Platform Stack](https://github.com/innotelinc/innotel-platform-stack)*
