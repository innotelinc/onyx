# 08 — User Management

## 1. User model

```
User {
  id, username, display_name, email,
  password_hash (argon2id),   // absent for directory-sourced users
  auth: { totp_enabled, webauthn_credentials[] },
  role: admin | operator | user | viewer,
  groups: [Group],
  home_subvolume, quota { soft, hard, used },
  status: active | locked | pending,
  metadata: { created_at, last_login_at, must_change_password, ssh_keys[] }
}
Group { id, name, members[], default_share_permissions? }
```

- **Built-ins:** `admin` (first account, created at install — never removable, renameable),
  `backup` (service account used by backup jobs), `guest` (disabled by default; drives
  guest-access shares).
- **Users map 1:1 to POSIX users** on the host, with home subvolumes under
  `@data/homes/<user>`. SMB, SFTP, FTP, and rsync all authenticate against this same
  account store, so a password change propagates everywhere at once.

## 2. Roles and permissions

| Role | Shares (own) | Shares (granted) | Apps | Backups | Users & security | System settings | Pool/disk |
|------|--------------|------------------|------|---------|------------------|-----------------|-----------|
| **admin** | rw | rw | manage | manage | manage | manage | manage |
| **operator** | rw | rw | manage | manage | view only | view only | — |
| **user** | rw | per grant | use | own jobs only | — | — | — |
| **viewer** | ro | ro | use (read-only) | — | — | — | — |

- **Custom roles:** admin-defined permission sets drawn from the API catalog (e.g.,
  "media-manager": `shares.read, apps.manage(jellyfin, plex)`).
- **Permission checks are centralized** in `onyx-core` policy evaluation; the UI renders
  based on the same checks, and the protocol daemons enforce equivalent grants via config.
- Effective access = **role ∪ group grants ∪ per-share grants**, with deny-overrides for
  explicit blocks (e.g., a group is granted a share, one member is blocked).

## 3. Share and quota management

- Share grants are per user/group with `read` / `read-write`; visible in both directions
  (user view: "You can access: Photos (rw), Family (ro)").
- **Quotas:** per-user soft/hard on the home subvolume; per-share quotas at the share
  subvolume. Soft → warning banner + notification; hard → writes blocked with a clear UI
  message. Admins get a quota overview table with usage bars and one-click "grant more".
- Quota inheritance: a group can carry a default quota applied to new members.

## 4. Authentication methods per user

| Method | Local users | Directory users |
|--------|-------------|-----------------|
| Password (argon2id) | ✅ (2FA optional/required by policy) | via LDAP/AD bind |
| TOTP | ✅ | ✅ (local 2FA) |
| WebAuthn / passkey | ✅ | ✅ |
| SSH key | ✅ (SFTP/SSH) | via directory |
| App token | ✅ scoped | ✅ scoped |
| OIDC SSO | optional identity link | ✅ primary |

- **2FA policy:** admin can require 2FA for all admins, all users, or leave optional.
  First-login wizard nudges 2FA for admin accounts ("takes 30 seconds").
- **Password policy:** minimum 12 chars, optional complexity; password change forces token
  revocation for that user (except long-lived app tokens, which get an expiry notice).
- **Lockout:** 5 failed attempts → 15 min lock, exponential backoff; admins unlock; failed
  logins feed the security notification stream.

## 5. Lifecycle and onboarding

1. **Install:** create the first admin (username, strong password, 2FA prompt).
2. **Invite:** admin adds a user → invite link/email with a one-time token (expires 72 h)
   → user sets own password + 2FA. No shared passwords ever.
3. **Daily use:** user logs in, sees only their home + granted shares + permitted apps.
4. **Suspension:** lock user (session revoked, SMB/SFTP denied, apps stopped for that
   identity); data retained.
5. **Deletion:** typed confirmation; data moved to a `deleted-<user>-<ts>` snapshot first
   (retention per policy, default 30 days), then removable — never silent.

## 6. Directory integration (optional)

- **OIDC:** add provider (discovery URL + client credentials) → "Sign in with <provider>"
   appears on the login screen; group claims map to Onyx groups; a local admin break-glass
   account is always kept.
- **LDAP/AD:** join the domain; users/groups sourced from the directory (read-only
  reference), Onyx roles assigned via group mapping rules (e.g., `CN=NAS-Admins` → admin).
  Passwords validated by the directory; Onyx never stores them.
- **Scope guard:** directory-sourced users get the `user` role by default until an admin
  maps them; misconfigurations never grant admin implicitly.

## 7. Session and device management

- "Devices" page lists active sessions (browser, CLI, mobile, WebDAV clients) with last-seen,
  IP, and user-agent; per-device revoke and "revoke all".
- Concurrent session limit per user (default 10) with oldest-eviction; admin override.
- Password/2FA changes revoke sessions except current device, with a confirm prompt.

## 8. Delegation (v1.5+, designed now)

- **Share links:** expiring, password-optional links to files/folders for people without
  accounts (download-only or upload-only drop boxes) — implemented as scoped app tokens
  bound to a single path; the Links page lists, expires, and revokes them.
- **Operator delegation:** an admin can grant a user `operator` on a subset of shares
  ("manage family share, nothing else") via custom roles.

## 9. Audit and privacy for users

- Users can view the audit entries that concern them (own logins, own file actions) in a
  "My activity" view — transparency without exposing other users' data.
- Admins see the full audit log; user-level exports available for compliance (small
  business) — see [07-security-model.md](07-security-model.md#9).
