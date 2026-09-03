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

## Golden rules

- **Authentik = Identity** · **Infisical = Secrets** · **Cerulean = Trust** ·
  **ONYX = Storage** · **Magnate = Revenue** — everything else is a business function.
- No platform duplicates another's responsibility.
- No credit in commits, footers, or headers to anyone but the project owner.

---

*ONYX · StorageOps · [Innotel Platform Stack](https://github.com/innotelinc/innotel-platform-stack)*
