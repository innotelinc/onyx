# 07 — Security Model

## 1. Threat model

Onyx is a home-to-small-office appliance on the LAN, optionally exposed to the internet
(via VPN or forwarded ports). Primary adversaries:

| Adversary | Goal | Primary exposure |
|-----------|------|------------------|
| Internet scanner/bot | RCE, cryptolock data | Exposed web UI, SMB, protocols |
| Malicious app container | Escape, read host/pool/other apps | Docker sandbox |
| Rogue LAN client | Access shares without permission | SMB/NFS/FTP/WebDAV |
| Malicious file | Trigger parser/handler bugs (SFTP, thumbnails, archives) | File pipelines |
| Stolen device/disk | Read data at rest | Disk theft, lost laptop |
| Malicious update | Backdoor via update channel | Update pipeline |
| Insider (limited user) | Privilege escalation, unauthorized access | Role model, API keys |
| Malicious AI prompt | Trick assistant into destructive actions | `onyx-ai` tool use |

Out of scope (documented): defense against physical attackers with console access to an
unencrypted install (mitigated by optional disk encryption), and nation-state adversaries.

## 2. Trust boundaries

```
Internet ──► TLS ──► onyx-api ──► onyx-core ──► services ──► onyx-privd ──► kernel/fs
   ▲              │(unprivileged)   │(unprivileged)     │             │ (root, allowlisted)
   │              └── authN/authZ + rate limit ──────────┘
LAN clients ──► protocol daemons (Samba/NFS/sshd/vsftpd/rsyncd) ──► POSIX ACLs ──► pool
Apps ──► Docker (namespaces, seccomp, AppArmor) ──► bind mounts (scoped) ──► @apps/<app>
```

Rules:
- The **network boundary** terminates at `onyx-api` and the protocol daemons. Nothing else
  listens on TCP.
- The **privilege boundary** is `onyx-privd`: the only process that elevates, with an
  allowlist (see §04.7).
- The **app boundary** is the container sandbox; apps never see host unix sockets, the API
  token, or other apps' data.
- The **data boundary** is POSIX ACLs + quota on the pool; every service runs as its own uid.

## 3. Secure defaults (non-negotiable)

- All management traffic HTTPS-only; HTTP redirects and is disabled after first setup.
- **No protocol exposed by default** (SMB, NFS, FTP, SFTP, WebDAV, Rsync all off until
  explicitly enabled).
- Web UI not reachable from WAN by default; guides recommend VPN (built-in WireGuard) for
  remote access instead of port-forwarding.
- Admin account enforces a strong password policy at setup; 2FA offered at first login and
  recommended for any user with admin rights.
- Telemetry off; no cloud account required; outbound network calls only for user-initiated
  actions (updates checked on schedule, opt-in).
- Passwords hashed with argon2id (memory-hard); API keys stored hashed (SHA-256 with pepper);
  session tokens random 256-bit, hashed at rest.

## 4. Authentication & sessions

- **Local accounts:** username + argon2id password. Login rate-limited and logged;
  `failed_login` events stream to the bus and can trigger notifications.
- **Second factor:** TOTP (RFC 6238) and WebAuthn/passkeys (platform authenticators). 2FA is
  per-user enforced-or-optional (admin policy).
- **Sessions:** opaque random tokens, stored server-side with device metadata; idle timeout
  (default 2 h) + absolute lifetime (default 30 days); revocable individually and globally
  ("sign out all devices"). Rotation on privilege change.
- **API keys / app tokens:** scoped (`shares.read`, `backups.write`, …), expiring, revocable,
  never reused; used by CLI, scripts, mobile, and apps. The WebDAV endpoint accepts app
  tokens for cloud-sync clients.
- **Directory integration (optional):** OIDC (Google/Entra/Keycloak/Authentik) and LDAP/AD
  join for user/group sourcing; local accounts remain for break-glass admin.
- **Break-glass:** local admin + TOTP always works even when directory is down; documented
  recovery path (console reset with pool preservation).

## 5. Authorization model

- **RBAC:** roles `admin`, `operator` (manage shares/apps/backups, not users/security),
  `user` (own home + granted shares), `viewer` (read-only). Custom roles = permission sets
  from the API catalog.
- **Share access:** per-share grants `read`/`read-write` to users and groups; enforced at
  every layer (API, SMB ACLs, NFS, WebDAV, SFTP chroot, rsync modules).
- **Filesystem reality check:** the API layer validates against the same policy DB the
  protocol daemons use — no layer trusts another.
- **Privileged operations** (pool changes, rollback, user admin, firewall) require admin
  role and are audit-logged; destructive ones additionally require a typed confirmation or a
  short-lived confirmation token.

## 6. Application sandboxing

- **Docker** with user namespaces (`userns-remap`), `--pids-limit`, memory limits from the
  app manifest, read-only rootfs where supported.
- **AppArmor** profile per app (deny-by-default host access), **seccomp** default profile.
- **No host mounts** except explicit `volume:` entries the user approved at install;
  `@apps/<app>` subvolume bind-mounted at the app path, so each app's data is isolated and
  snapshotable.
- App network: bridge network by default; port publishing requires an explicit, visible
  step in the UI ("Publish port 8096 for Jellyfin?"); host-network apps are flagged as
  high-risk and warn.
- **No Docker socket in the web process:** `onyx-appd` talks to Docker via its own
  authenticated, proxied API; the UI never holds a Docker socket.
- Image supply chain: pinned digests + signature verification (cosign) for the curated
  store; update notifications when an image digest changes.

## 7. Secrets management

- Keyring at `/etc/onyx/keys/` (root-only, 0700), encrypted with a key derived from machine
  identity + an install-time passphrase (printed once, recoverable via recovery phrase).
- Services fetch secrets from `onyx-privd`'s secret API over the authenticated unix socket;
  secrets never appear in config files, env dumps, or logs (redaction filter in the log
  pipeline).
- App env secrets (DB passwords, API tokens) are injected at container start from the
  keyring, stored in the app manifest only as references.
- Backup encryption keys: client-side age keys; recovery phrase printed at creation.

## 8. Network security

- **Firewall (nftables, `onyx-netd`):** default-deny inbound except established/related;
  only enabled services open ports, each with a UI "Expose" toggle and a confirm dialog.
- **SMB1 disabled**; SMB signing required (`server signing = mandatory`); NFS restricted to
  configured subnets; FTP requires TLS; SFTP chrooted.
- **WireGuard VPN** as the recommended remote-access path (key pairs per device, UI-managed).
- **DDoS/brute force:** per-IP fail2ban-style ban via `onyx-netd` (configurable), SYN
  cookies on.
- **mDNS/UPnP:** mDNS (advertising) on by default for discovery, SSDP/UPnP off; port-forward
  hints never auto-configure routers.

## 9. Audit logging

- Append-only, checksum-chained audit log (`/var/lib/onyx/audit/`): every auth event,
  permission change, share/app/backup change, snapshot rollback, secret access, AI tool call,
  and privileged op — with actor, IP, request_id, outcome.
- Admins query via `/api/v1/audit`; export JSON/CSV/Syslog. The log is protected from
  tampering by non-root services and its chain is verifiable (`onyx audit verify`).
- Notifications: configurable alerting (email, ntfy, webhook) for security-relevant events
  (failed logins, new admin, rollback, disk failure, cert expiry).

## 10. Supply chain & updates

- All OS images signed (sigstore/cosign + OpenPGP cross-signed); the update client verifies
  signatures, digests, and the update channel's certificate before applying.
- Every release ships an SBOM (SPDX) and CVE-scan results; the UI surfaces
  "N packages with known CVEs" with severity.
- Third-party binaries pinned to hashes; reproducible-build CI for core services (goal:
  full reproducibility for Rust/Go services by v1).
- **Compromise containment:** because the root is read-only and A/B, a tampered system is
  detectable (`onyx status verify` compares root hashes) and recoverable (boot previous root).

## 11. Application security (in-house code)

- Rust services: `unsafe` blocks are counted and reviewed; `cargo deny` in CI.
- Go services: `go vet`, govulncheck, fuzzing on parsers (config, archive, thumbnail paths).
- Web: Content-Security-Policy strict (no inline scripts), HSTS, X-Content-Type-Options,
  CSRF tokens for cookie-auth, `sandbox` iframes for previews, no `target=_blank` without
  `rel=noopener`.
- File pipelines (thumbnails, archive preview, AV if enabled) run in isolated worker
  processes with no pool write access.
- External security review before v1.0; public disclosure policy + `security@` contact.

## 12. AI assistant security

- `onyx-ai` runs as an unprivileged service with **no direct FS/network privileges**; it can
  only call the public API as a constrained principal (read-only default, mutating tools
  require per-call user confirmation rendered in the UI — not just chat text).
- Prompt-injection hardening: system prompt pins scope; tool results are treated as data,
  never instructions; sensitive actions (delete, rollback, user changes, firewall) are
  blocked for the assistant entirely unless explicitly enabled by an admin.
- Every AI action audit-logged (`ai.action`) with exact payloads; a "show me what it did"
  panel lists the calls behind each answer.
- Local LLM mode keeps conversation data on-device; remote mode is opt-in, BYO-key, and
  warns that prompts may leave the device.
