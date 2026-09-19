# 06 — API Specification

## 1. Conventions

- **Base path:** `/api/v1` (versioned; breaking changes bump the major).
- **Transport:** HTTPS only; HTTP/2. JSON bodies (`application/json`), UTF-8.
- **Auth:** `Authorization: Bearer <token>` (session, API key, or app token). Sessions also
  supported via `X-Onyx-CSRF` header + cookie for browser flows. WebSocket upgrades carry the
  token in the first message (`{"type":"auth","token":…}`) — never in the URL.
- **Idempotency:** mutating requests may send `Idempotency-Key`; the gateway returns the
  original response on replay (30 min window, keyed per user).
- **Pagination:** cursor-based. `?limit=50&cursor=<opaque>` → `{"items":[…],"next_cursor":…}`.
  Page-size caps: 500 for lists, 2000 for file listings (with server-side filtering).
- **Filtering/sorting:** `?filter[type]=image&sort=-modified_at` (RFC-compliant filter syntax;
  allowlisted per resource).
- **Field selection:** `?fields=id,name,size` to trim payloads for the CLI/mobile.
- **Search:** `?q=` with a global index where supported (files, apps, users, shares).

## 2. Error model

```json
{
  "error": {
    "code": "share_not_found",
    "message": "Share 'photos' does not exist.",
    "details": { "share": "photos" },
    "request_id": "req_9f2c…",
    "retryable": false
  }
}
```

- Codes are stable snake_case strings (machine-consumable), messages are human-readable and
  localized by the client.
- HTTP mapping: 400 `invalid_argument`, 401 `unauthenticated`, 403 `permission_denied`,
  404 `not_found`, 409 `conflict` (e.g., name exists, pool busy), 412 `precondition_failed`
  (e.g., `If-Match` ETag mismatch), 422 `validation_failed` (field-level errors in
  `details.fields`), 429 `rate_limited` (with `Retry-After`), 500 `internal`, 503
  `service_unavailable` (retryable).
- Validation errors include per-field messages: `details.fields: [{name, reason}]`.

## 3. Rate limiting

Per-user, per-IP buckets at the gateway: 300 req/min interactive, 30 req/min for bulk
endpoints, WebSocket messages at 60/s. `RateLimit-*` headers on every response. App tokens
get their own higher allowance; misbehaving clients get 429 + backoff guidance.

## 4. WebSocket events (`/api/v1/ws`)

One authenticated socket streams `onyx-bus` events with a subscription message
(`{"type":"subscribe","topics":["snap.*","backup.*"]}`). Event envelope:

```json
{ "topic": "backup.progress", "ts": "2026-08-31T10:00:00Z", "data": { "job_id": "j_1", "pct": 42 } }
```

Delivery is at-least-once with sequence numbers; clients reconcile state by re-fetching on
reconnect.

## 5. Endpoint catalog (v1)

### System
| Method | Path | Notes |
|--------|------|-------|
| GET | `/system/status` | Health, versions, uptime, pool summary |
| GET | `/system/version` | OS + service versions, update channel |
| GET | `/system/logs?service=&since=` | Unified log stream (NDJSON) |
| POST | `/system/reboot` / `/system/shutdown` | Require admin + confirmation token |

### Pools & disks
| Method | Path |
|--------|------|
| GET/POST | `/pools` (list/create) |
| GET/PATCH/DELETE | `/pools/{id}` |
| POST | `/pools/{id}/devices` (add disk) |
| POST | `/pools/{id}/devices/{dev}/replace` |
| GET | `/devices` (list; the read performs a throttled live rescan), `/devices/{name}` |
| POST | `/devices/{name}/attach`, `/devices/{name}/detach` |
| GET | `/disks` (SMART + layout), `/disks/{id}` |
| POST | `/disks/{id}/smart` (run now) |
| GET | `/storage/overview` | Capacity headline for Files: one entry per pool/mount with `mounted` + `visible` flags, the primary capacity, and warnings when the data plane has mounted something this API process cannot reach (mount propagation is not shared) |

### Platform (v0.4 Jade)
| Method | Path |
|--------|------|
| GET | `/apps?status=` (installed), `/app-store` (catalog) |
| POST | `/apps` (install: `{app_id, version}`) |
| DELETE | `/apps/{id}?purge_data=` |
| GET | `/containers?app_id=` |
| POST | `/containers/{id}/start|stop|restart` |
| GET/POST | `/vms` |
| POST | `/vms/{id}/start|stop` (stop takes `{graceful}`) |
| DELETE | `/vms/{id}?delete_disk=` |
| GET/POST | `/buckets` (tier: local \| cloud \| tiered; cloud tiers take `{cloud_target}` — a configured rclone remote — and TIERED takes `{evict_after_days}`) |
| POST | `/buckets/{name}/sync?evict=` (mirror to the cloud target; `evict=1` releases local copies the cloud has been verified to hold) |
| DELETE | `/buckets/{name}?force=` (also purges the cloud target) |
| GET | `/ai/advisor?pool=`, `/ai/backup-advice` |

### Datasets & shares
| Method | Path |
|--------|------|
| GET/POST | `/datasets` (create = subvolume) |
| GET/PATCH/DELETE | `/datasets/{id}` (quota, options) |
| GET/POST | `/shares` |
| GET/PATCH/DELETE | `/shares/{id}` |
| POST | `/shares/{id}/protocols/{proto}` (enable/configure SMB/NFS/…) |
| GET | `/shares/{id}/connections` (connection strings) |
| GET/PUT | `/shares/{name}/access` (who can reach one share: the grants in core, plus which grantees Samba will accept; PUT takes the share's whole map and writes only the diff) |

### Files
| Method | Path | Notes |
|--------|------|-------|
| GET | `/files?path=&limit=&cursor=&q=` | Read-only directory listing rooted at the configured storage root; metadata includes size, mode, and modified time |
| GET | `/files/meta?path=` | Single entry |
| PUT | `/files/content?path=` | Planned resumable upload (see §6, tus resumable) |
| GET | `/files/content?path=` | Download (range requests) |
| POST | `/files/mkdir`, `/files/rename` | Root-confined mutations; rename refuses replacement |
| POST | `/files/delete` | Soft-delete into `.trash` under the storage root; no permanent delete |
| GET | `/files/search?q=` | Global + per-dir search |

### Snapshots
| Method | Path |
|--------|------|
| GET/POST | `/snapshots` (list/create for dataset) |
| POST | `/snapshots/{id}/rollback` | Requires an unmounted source; returns the completed restore point |
| GET | `/pools/{name}/scrub-schedule` | Current scrub schedule and last/next run |
| PUT | `/pools/{name}/scrub-schedule` | Set weekly/monthly scrub cadence |
| POST | `/pools/{name}/scrub` | Queue a manual scrub |
| GET | `/snapshots/{id}/browse?path=` |
| POST | `/snapshots/{id}/restore` (file-level, target path) |
| PUT | `/datasets/{id}/snapshot-policy` |

### Backups
| Method | Path |
|--------|------|
| GET/POST | `/backup-jobs` |
| GET/PATCH/DELETE | `/backup-jobs/{id}` |
| POST | `/backup-jobs/{id}/run`, `/backup-jobs/{id}/cancel` |
| GET | `/backup-jobs/{id}/history` |
| GET | `/backup-report` | Recovery health, RPO/RTO estimates, and trend |

### Apps
| Method | Path |
|--------|------|
| GET | `/apps` (installed) |
| GET | `/app-store` (catalog), `/app-store/{id}` |
| POST | `/apps` (install: `{store_id, config}`) |
| GET/PATCH/DELETE | `/apps/{id}` (config, uninstall) |
| POST | `/apps/{id}/start|stop|restart|update` |
| GET | `/apps/{id}/logs?lines=` |
| GET/POST | `/apps/{id}/ports` (port mapping wizard) |

### Users & access
| Method | Path |
|--------|------|
| GET/POST | `/users` |
| GET/PATCH/DELETE | `/users/{id}` (password, quota, groups, 2FA reset) |
| GET/POST | `/groups`, `/roles` |
| GET/PUT | `/users/{id}/permissions` (per-share grants; recorded in core, enforced by the share backends) |
| POST/DELETE | `/users/{id}/smb-password` (create/re-password or remove the user's Samba account; the password rides the request body and is never echoed) |
| GET | `/samba/accounts` (the names Samba's passdb contains, with `available:false` when the deployment has no Samba) |
| GET | `/audit/access?share=&limit=` (the grant changes and the refusals, newest first; `denied` events are written by onyx-davd) |
| GET/POST/DELETE | `/api-keys` (scoped, expiring) |
| GET/POST | `/auth/login`, `/auth/logout`, `/auth/refresh` |
| GET/POST/DELETE | `/auth/webauthn`, `/auth/totp` |
| GET | `/auth/session` |
| GET/POST/DELETE | `/oidc-providers` (and LDAP/AD via `/directory/`) |

### Network & system settings
| Method | Path |
|--------|------|
| GET/PUT | `/network/interfaces` |
| GET/PUT | `/network/firewall` (nftables ruleset UI) |
| GET/POST/DELETE | `/network/forwards` |
| GET/PUT | `/network/vpn` (WireGuard) |
| GET/PUT | `/settings` (notifications, telemetry opt-in, language, theme) |
| GET/POST | `/audit?filter=` (admin) |
| POST | `/ai/chat` (SSE stream), `/ai/actions` (dry-run tool calls) |

## 6. File upload/download

- **Uploads:** resumable via the **tus** protocol (`/api/v1/files/tus`), with server-side
  chunk assembly and pause/resume — required for large media over flaky home Wi-Fi.
- **Downloads:** standard GET with `Range`; the gateway streams from the pool with zero-copy
  `sendfile` and enforces per-protocol bandwidth caps set on the share.
- **Integrity:** every upload records the file checksum; the UI verifies on completion and
  reports mismatches. Thumbnails are generated server-side (`onyx-agent` sidecar) at
  configurable sizes.

## 7. Versioning and stability

- `v1` is additive-only while stable; new fields are optional, never breaking.
- Runtime release metadata is exposed by `GET /api/v1/system/version`: `version`,
  `api_version`, `codename`, and `commit`. The release version is injected at build
  time so all daemons report the same artifact identity.
- Deprecations: marked `Deprecated: true` in OpenAPI, minimum 2 minor releases before removal.
- The OpenAPI document is published at `/api/v1/openapi.json` and drives the typed SDKs and
  the Prism form generator (schemas → forms), so UI forms and API validation never drift.

## 8. AI-assisted admin surface

`POST /ai/chat` streams assistant turns (SSE). The assistant is **tool-constrained**: it may
only invoke the same endpoints above, with two extra guardrails:

1. Read-only tools by default; mutating tools require per-call user confirmation in the UI.
2. Every tool call is recorded in the audit log as `ai.action` with the exact request and
   response.

Local mode: a bundled small LLM via llama.cpp (CPU-friendly 1–4B quantized, optional download)
with RAG over the user's own docs/FAQ. Remote mode: BYO-key for stronger models (OpenAI-
compatible endpoint). See [07-security-model.md](07-security-model.md#1-threat-model) for the threat model.
