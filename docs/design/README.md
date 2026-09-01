# Onyx — Design Documents

**Onyx (Online Storage System)** is an open-source NAS operating system for people who want a
private, powerful file server that feels like a modern app — not an enterprise appliance.

These documents are the living specification for the project. They are written to be read in
order, but each stands alone and is versioned independently so teams can evolve areas at their
own pace.

| # | Document | What it covers |
|---|----------|----------------|
| 01 | [Product Vision](01-product-vision.md) | Why Onyx exists, who it's for, positioning vs. competitors, principles, roadmap |
| 02 | [Technical Architecture](02-technical-architecture.md) | System layers, stack decisions, module map, data flow, performance budgets |
| 03 | [Frontend Design System](03-frontend-design-system.md) | "Prism" design language, tokens, components, file explorer UX, a11y |
| 04 | [Backend Service Architecture](04-backend-service-architecture.md) | Service inventory, IPC, config/state layout, privilege model, observability |
| 05 | [Storage Subsystem](05-storage-subsystem.md) | Btrfs pools, subvolume layout, snapshots, quotas, scrub, sharing protocols |
| 06 | [API Specification](06-api-specification.md) | REST + WebSocket conventions, endpoint catalog, error model, versioning |
| 07 | [Security Model](07-security-model.md) | Threat model, trust boundaries, sandboxing, auth flows, audit, supply chain |
| 08 | [User Management](08-user-management.md) | Users, groups, roles, ACLs, quotas, API keys, OIDC/LDAP/AD integration |
| 09 | [App Ecosystem](09-app-ecosystem.md) | App format, store, install UX, sandboxing, SDK, curated catalog |
| 10 | [Installer and Updates](10-installer-and-updates.md) | Installation targets, first-boot wizard, ostree A/B updates, rollback |

## Status

- **Phase: Design** — all documents are drafts to be validated with prototype builds.
- **Canonical source of truth:** this directory. Code comments must reference document sections
  (`docs/design/05#snapshots`) when they encode a design decision.
- **RFC process:** any change to a numbered section ships as an RFC in `docs/rfcs/` before merge.

## Decisions log (TL;DR)

The single most important decisions, in one place:

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Base OS | Minimal Debian (Trixie) | Predictable tooling, huge package surface, long-term support |
| Root filesystem | OSTree-managed, read-only, A/B | Atomic updates, rollback, tamper resistance |
| Primary filesystem | Btrfs | Snapshots, checksums, compression, quotas at low RAM cost vs. ZFS |
| System language | Rust (data plane), Go (control plane) | Rust for FS-level safety, Go for fast, safe API/orchestration code |
| Frontend | TypeScript + React SPA, "Prism" design system | Fast iteration, rich UX; served from the gateway |
| Apps | Docker containers + Compose, AppArmor/seccomp | Largest ecosystem, familiar to maintainers |
| System DB | SQLite (WAL) per service | Zero-administration, transactional, no separate server |
| Auth | Local accounts + TOTP/WebAuthn, optional OIDC/LDAP/AD | Simple by default, enterprise-compatible on demand |
| License | AGPL-3.0 core, Apache-2.0 SDKs/app SDK | Keeps the core free, encourages app ecosystem |
