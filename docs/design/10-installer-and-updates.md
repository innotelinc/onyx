# 10 — Installer and Updates

## 1. Installation targets

| Target | Media | Experience |
|--------|-------|------------|
| x86_64 (UEFI) — mini-PC, old laptop, NUC | ISO (hybrid) | Guided installer, 5–10 min |
| ARM64 SBC — Raspberry Pi 4/5, RK3588 boards | Prebuilt image (`.img.xz`) | Flash & boot, first-boot wizard on screen |
| ARM64 SBC — generic | Same image; per-board DTB bundles | As above |
| Existing Debian/Ubuntu server | Script installer (`curl … | bash`-style, but audited & pinned) | Converts the box in place, keeps data disks |
| Virtual machines | ISO (virtio + EFI) | Standard guided flow |
| Nested/dev | Containerized dev image (QEMU/lima) | `onyx dev up` for contributors |

## 2. Partitioning

**Two-disk layout (recommended, automatic):**

```
System disk (SD/SSD, ≥ 16 GB)
├── p1  EFI System Partition (512 MiB, FAT32)
├── p2  A/B system roots: A (6 GB, ext4/ro), B (6 GB, ext4/ro)   ← OSTree
├── p3  config+state (rest of disk, Btrfs or ext4)               ← /etc/onyx, /var/lib/onyx
Data disk(s)
└── p1  Btrfs pool (@data, @apps, @backups, @snapshots)
```

- **Single-disk fallback:** one disk with a smaller system partition carved off; the
  remainder is the pool. Wizard warns this mixes system and data on one disk.
- **Custom (expert):** manual partitioning with a warning screen; installer validates that
  the data pool is never touched during install/update.
- **Disk encryption:** optional LUKS2 on system + data at install (checkbox); recovery
  key printed and (optionally) backed to a file; see §07.1 for the physical-theft threat
  note.

## 3. Guided installer flow (ISO)

1. **Language / keyboard** (defaults en-US).
2. **Disk selection** — "Automatic (recommended)" shows a plain-language summary:
   "System will use *Samsung 64 GB SD*. Your data disks (sdb, sdc) will be untouched."
3. **Pool setup** — single disk / two disks (RAID1) / custom; live capacity estimate.
4. **Network** — DHCP default, static optional; hostname default `onyx`.
5. **Admin account** — username, strong password meter, 2FA prompt after first boot.
6. **Summary** — everything in one scrollable, human-readable list; "Install".
7. Progress with phase labels (partition → deploy root → configure → finalize); ETA.
8. **Reboot → first-boot wizard** (see §4).

Script installer follows the same steps non-interactively via flags
(`onyx-install --disk /dev/sda --pool-raid1 sdb sdc --admin admin`), with `--yes`.

## 4. First-boot wizard (the 15-minute promise)

1. Welcome + theme (dark/light) and language.
2. **2FA setup** for the admin account (WebAuthn or TOTP).
3. **Storage tour** — shows the pool, explains snapshots with a live demo (create snapshot →
   delete a test file → roll back). This one demo is the "aha" moment of the product.
4. **Create first share** ("Family photos" preset with sensible defaults).
5. **Enable protocols** — "Which do you want to turn on?" with plain-language descriptions
   (SMB: "Windows/Mac/Android", NFS: "Linux/other NAS", …). All optional, all off by default.
6. **App store** — "Install something?" carousel of popular apps.
7. **Remote access** — options: (a) stay LAN-only, (b) set up WireGuard VPN (guided),
   (c) advanced/external. Never defaults to port-forwarding.
8. **Notifications** — email/ntfy/webhook, optional.
9. **Telemetry** — opt-in, one clear screen, "No, thanks" is the emphasized default.
10. Done → dashboard.

## 5. Update model (OSTree A/B)

```
┌────────────┐     ┌────────────┐
│ Root A     │     │ Root B     │     ← identical structure: read-only OS + immutable UI
│ (running)  │     │ (staged)   │
└────────────┘     └────────────┘
      ▲                  ▲
      └── deploy ────────┘        onyx-updated applies new commit to inactive slot
```

- **Channels:** `stable` (default), `beta`, `nightly` (dev only). Each channel's repo is
  signed (sigstore + OpenPGP cross-signed); `onyx-updated` verifies signatures and the
  commit's parent chain before staging.
- **Check & download:** scheduled (default daily) + manual; downloads apply only to the
  inactive slot — zero impact on the running system.
- **Apply:** reboot into new root (`ostree admin pin` semantics: keep previous as fallback).
- **Health gate:** after boot, `onyx-agent` runs a boot-health check (services up, pool
  importable, UI responds, watchdog timer). On failure → automatic reboot into the previous
  root, event logged, user notified. Manual rollback also available: `onyx update rollback`
  or a dashboard button.
- **Config migration:** each release ships a migration script run on first boot of the new
  root; migrations are transactional (backup of `/etc/onyx` + DBs taken first); failure
  rolls back to the old root with the old config intact.
- **User-data guarantee:** data lives on the pool, never in the root — updates can never
  touch user data by construction.

## 6. Update UX

- Status pill shows "Update available: Onyx 0.4.2" with release notes (rendered markdown,
  linked CVEs fixed, upgrade warnings).
- "Update now" → progress (download %, staging, reboot countdown with 30 s cancel window,
  configurable maintenance window). Background auto-update is opt-in.
- **Downtime target:** < 2 min from reboot to healthy dashboard.
- **App updates are separate** from OS updates (apps update independently via `onyx-appd`);
  an OS update never silently changes app images.

## 7. Rollback and recovery

- **Rollback paths (all in the UI/CLI, all audited):**
  | What broke | Recovery |
  |------------|----------|
  | OS update | Boot previous A/B root (auto or manual) |
  | Config | Restore `/etc/onyx` + DBs from last snapshot (config snapshot taken before every update) |
  | App update | Roll back image to previous digest (pre-update app snapshot) |
  | Data change | Snapshot timeline browse/restore (§05.3) |
  | Whole system | Reinstall → import pool → restore config snapshot (documented runbook) |
- **Recovery console:** a minimal TUI (keyboard-only, no web) on the console for when the
  UI can't boot: import pool, rollback root, reset admin password, export logs to USB.
- **Factory reset:** wipes system partitions only; data pool untouched unless the user
  explicitly chooses "erase everything" (typed twice).

## 8. Maintenance operations

- **Export/import:** "Download config backup" (encrypted, with recovery phrase) from
  Settings; importing restores users, shares, schedules, app manifests, and keyring.
- **Migrations between devices:** import config + physically move data disks; pool import
  (`onyx pool import`) detects the layout and re-registers shares.
- **Logs for support:** "Download diagnostics" bundles config (redacted), logs, audit, and
  hardware info into an encrypted archive for issue triage.

## 9. Release engineering

- **Cadence:** nightly builds (CI), beta every 2 weeks, stable monthly during beta phase;
  LTS at v1.0 (3 years, security-only).
- **Pipeline:** reproducible builds (goal: bit-for-bit for Go/Rust services) → SBOM + CVE
  scan → hardware-lab test matrix (RPi4/RPi5/N100/AMD) → signed commits → staged rollout
  (10% → 50% → 100% over 2 weeks, auto-pause on regression signals from opt-in telemetry).
- **Verification:** `onyx status verify` (root hash check), update signature verification
  (documented in §07.10).

## 10. Documentation & support story

- **In-product:** contextual help in every wizard step, tooltips on every setting, a
  built-in "How do I…" search backed by the same docs site.
- **Docs site:** getting-started, per-protocol guides (SMB on Windows/macOS/Linux, WebDAV
  with RaiDrive, rsync scripts), backup guides, hardware compatibility list.
- **Community:** forum + Discord; "app requests" and "hardware reports" are structured
  issue templates.
