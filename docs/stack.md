# 🗄️ ONYX — Platform Stack Role

**Classification: StorageOps**

Storage and infrastructure: file/object storage, backups, snapshots, replication, NAS features, and virtualization storage.

This page declares ONYX's role in the
[**Innotel Platform Stack**](https://github.com/innotelinc/innotel-platform-stack) —
the canonical single-responsibility architecture. The stack is defined in exactly one
place; this page links each product to it and states what this platform owns, consumes,
provides, and explicitly does not own.

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
- Infisical — secrets, credentials
- NPM Edge — public routing, TLS termination at the edge

## Explicitly does NOT own

- Identity (Authentik)
- Billing (Magnate)
- Certificates / trust (Cerulean)


## Secrets (Infisical)

Secrets for this platform live in **Infisical** (SecretOps): credentials are imported
into an Infisical workspace and the stack's `.env` is derived from it. Enable it with:

```bash
# generate the required keys and add them to .env
openssl rand -base64 32   # INFISICAL_ENCRYPTION_KEY
openssl rand -hex 16      # INFISICAL_AUTH_SECRET
openssl rand -hex 16      # INFISICAL_DB_PASSWORD

# start the profile and provision the workspace + import .env secrets
docker compose -f docker-compose.yml -f compose.infisical.yml --profile infisical up -d
bash scripts/infisical-setup.sh
```

See [compose.infisical.yml](../compose.infisical.yml) and
[scripts/infisical-setup.py](../scripts/infisical-setup.py) for details.

### Runtime resolution (`infisical://`)

With `INFISICAL_ADDR` / `INFISICAL_TOKEN` / `INFISICAL_WORKSPACE_ID` in `.env` (written
back by `scripts/infisical-setup.py`), the Go services resolve secrets at runtime:

- `onyx-objectstore` — `S3_ACCESS_KEY` / `S3_SECRET_KEY` may be `infisical://<name>`
  references; plain values are **mirrored into Infisical on boot** (best-effort), so
  after one boot you can switch `.env` to references.
- `onyx-api` — `CERULEAN_API_TOKEN` may be an `infisical://<name>` reference; Infisical
  health is reported on `GET /api/v1/status` (`infisical: ok | not-configured | error`).

Shared client: `services/infisical/` (same contract as Cerulean's resolver).

## Golden rules

- **Authentik = Identity** · **Infisical = Secrets** · **Cerulean = Trust** ·
  **ONYX = Storage** · **Magnate = Revenue** · **NPM Edge = Edge** — everything else is a business function.
- No platform duplicates another's responsibility.
- No credit in commits, footers, or headers to anyone but the project owner.

---

*ONYX · StorageOps · [Innotel Platform Stack](https://github.com/innotelinc/innotel-platform-stack)*
