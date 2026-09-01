# 01 — Product Vision

> **Onyx (Online Storage System)** is an open-source NAS operating system for people who want a
> private, powerful file server that feels like a modern app — not an enterprise appliance.

## 1. Mission

Self-hosting storage should be as easy as installing a phone app and as reliable as a cloud
service — without renting the cloud. Onyx gives individuals, families, and small teams a
private file server that is **lightweight enough to run on hardware they already own**, safe by
default through snapshots and atomic updates, and pleasant enough to use that configuration is
an exception, not a ritual.

## 2. Who it's for

| Persona | Story | Onyx promise |
|---------|-------|--------------|
| **The homelabber** | Runs a Raspberry Pi 5 or N100 mini-PC with 2–3 drives | A capable NAS that boots in under a minute and idles under 512 MB RAM, leaving headroom for apps |
| **The family keeper** | Wants photo backup, media library, and shared documents off Google/iCloud | One-time setup, then it "just works" — backups, snapshots, and shares run themselves |
| **The small business** | 5–15 staff, needs shared folders, user accounts, off-site backups | Predictable permissions, quota enforcement, audited access, no per-seat licensing |
| **The privacy advocate** | Wants zero telemetry, no cloud account, full data ownership | Local-first everything; an account is optional and never required |
| **The tinkerer** | Wants to run Jellyfin, Immich, Nextcloud, Home Assistant, Pi-hole | One-click app installs with sane defaults, sandboxed, updatable |

Non-goals: Onyx is **not** an enterprise SAN/NAS. No Fibre Channel, no zoned storage, no
multi-node clustering in v1. It competes on the home-to-small-office tier and does that one
tier exceptionally well.

## 3. Positioning vs. competitors

| | **TrueNAS** | **OpenMediaVault** | **CasaOS** | **ZimaOS** | **Onyx** |
|---|---|---|---|---|---|
| Positioning | Enterprise storage appliance | Debian NAS web UI | App-first home server | Polished app-first NAS | **Storage-first, app-friendly, lightweight** |
| Storage power | ZFS (extreme) | mdadm/ext4/Btrfs | Thin (no snapshots) | Basic | **Btrfs with first-class snapshots** |
| Default RAM appetite | 8 GB+ (ZFS) | ~1 GB | ~500 MB | ~1 GB | **< 512 MB idle** |
| Apps | Jails/VMs (complex) | Plugins (mixed quality) | Docker (great UX) | Docker (great UX) | **Docker + curated store, sandboxed** |
| UI polish | Functional, dated | Functional, dated | Modern, simple | Modern, clean | **Linear/GitHub-grade design system** |
| Updates | Reliable (boot envs) | Package-based | Image-based | Image-based | **Atomic A/B with rollback** |
| AI assistance | None | None | None | Partial (local LLM) | **Optional local/remote AI admin** |

**The wedge:** TrueNAS is too heavy and complex for the home tier; CasaOS/ZimaOS are great at
apps but weak on the storage fundamentals (snapshots, quotas, scrub, protocol-depth). Onyx
takes the storage safety of TrueNAS and wraps it in the app experience and design quality of
ZimaOS — at TrueNAS-CORE-in-2015 hardware requirements.

## 4. Design principles

1. **The files are the product.** Storage, snapshots, and sharing are the core; apps extend it.
2. **Low hardware appetite.** Every service must justify its memory. Budget: ≤ 512 MB idle,
   ≤ 1 GB with 3 apps running, boot ≤ 60 s on a Raspberry Pi 4.
3. **Safe by default.** Snapshots on by default, atomic updates, checksum scrubbing, and a
   "restore" path for every destructive action.
4. **Sane defaults, optional depth.** A first-time user never sees a mount option; an expert
   can reach the underlying filesystem and protocols without a hack.
5. **Local-first and private.** No mandatory cloud account, no telemetry by default, all
   metadata stays on-device.
6. **Modular everything.** The core is a set of small services; the app store is the extension
   mechanism for the OS itself, not a bolt-on.
7. **Design matters.** The UI is a product surface, not an admin afterthought. Keyboard-first,
   fast, and calm.

## 5. Success metrics (v1 targets)

| Metric | Target |
|--------|--------|
| Time from boot to first share created | < 15 minutes |
| Idle RAM (RPi 4, base install) | < 512 MB |
| Boot to login (RPi 4, NVMe/SD) | < 60 s |
| Snapshot + rollback | One click, < 5 s for 100 GB data |
| App install (Jellyfin-class) | < 30 s, no terminal |
| Update downtime | < 2 min, automatic rollback on failure |
| Telemetry | Off by default, opt-in only |

## 6. Scope boundaries

**In scope (v1):** single-node NAS; Btrfs pools; SMB/NFS/FTP/SFTP/WebDAV/Rsync; snapshots and
rollback; scheduled backups (local disk, remote rsync/SSH, S3); user/group management with
quotas; Docker app store; atomic updates; REST + WebSocket API; local AI assistant (optional).

**Out of scope (v1, designed-for later):** multi-node clustering, iSCSI targets, VMs,
Kubernetes, tape backup, S3 *serving* (outbound only), AD domain *controller* (join only).

## 7. Roadmap

| Milestone | Theme | Deliverables |
|-----------|-------|--------------|
| **v0.1 "Cinder"** | Skeleton | Bootable image, OSTree base, `onyx-core`, storage service, API, minimal CLI |
| **v0.2 "Flint"** | Files | Web UI shell + design system, file explorer, SMB/NFS shares, users & permissions |
| **v0.3 "Obsidian"** | Safety | Snapshots + rollback UI, scrub scheduling, backup jobs, SMART monitoring, notifications |
| **v0.4 "Jade"** | Apps | App store + sandboxing, Docker integration, SFTP/FTP/WebDAV/Rsync protocol surface |
| **v0.5 "Quartz"** | Intelligence | AI admin assistant, audit reporting, advanced search, mobile app |
| **v1.0** | Release | Security audit, docs, hardening, LTS update channel |

## 8. Open-source governance and licensing

- **Core OS** (base image, system services, UI, API): **AGPL-3.0**. The core must stay free for
  everyone, including appliance vendors.
- **SDKs & app SDK** (`onyx-sdk`): **Apache-2.0** to maximize app-ecosystem adoption.
- **Governance:** a neutral foundation model (e.g., a Linux Foundation-style home) with a
  maintainers council; decision records published in `docs/rfcs/`.
- **Community:** public roadmap, monthly release cadence during beta, and a documented
  contribution ladder from first PR to maintainer.

## 9. Brand and product name

- Product name: **Onyx** (Online Storage System). The black gemstone: hard, dense, quietly
  premium — a good metaphor for a small black box that safely holds what matters.
- Design system: **Prism** (see [03-frontend-design-system.md](03-frontend-design-system.md)).
- CLI: `onyx` (`onyx share list`, `onyx snap create`, …).
- Mobile companion app (v0.5+): **Onyx Pocket** — notifications, quick uploads, remote restore.
