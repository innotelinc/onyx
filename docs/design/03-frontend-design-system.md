# 03 — Frontend Design System

The Onyx frontend is built on **Prism**, a design system that synthesizes four references:

- **Linear** — keyboard-first speed, calm hierarchy, command palette as a primary surface.
- **GitHub** — dense, scannable data tables and readable code/activity surfaces.
- **Arc Browser** — persistent sidebar navigation and a polished command bar.
- **VisionOS / Windows 11** — soft glass panels, translucency (Mica/Acrylic-like), rounded
  corners, and restrained, physical-feeling motion.

## 1. Design principles

1. **Calm by default.** Information-dense when you're working, quiet when you're not. No
   bouncing animations, no decorative gradients competing with data.
2. **Dark-first.** The OS is a nightstand appliance; dark is the default theme, light is
   provided and fully supported.
3. **Every action is one click from anywhere.** Command palette, contextual menus, and
   drag-and-drop cover 90% of workflows; the settings tree is for the rest.
4. **Destructive actions look destructive.** Red is reserved for data loss and irreversible
   operations; every destructive control requires a typed confirmation or undo window.
5. **Status at a glance.** Health, sync, and capacity states are encoded in color *and* shape
   (never color alone), with micro-copy explaining what to do.

## 2. Design tokens

All tokens are CSS custom properties emitted from `web/theme/tokens.css` and consumed by
components — no raw values in component code.

### 2.1 Color

**Neutrals** — zinc-family ramp (dark mode defaults):

| Token | Value | Use |
|-------|-------|-----|
| `--onyx-bg` | `#0b0d10` | App background |
| `--onyx-bg-elevated` | `#12151a` | Panels, cards |
| `--onyx-bg-glass` | `rgba(24, 28, 34, 0.72)` | Floating glass surfaces (with `backdrop-filter: blur(24px)`) |
| `--onyx-surface` | `#181c22` | Sidebar, top bar, inputs |
| `--onyx-surface-hover` | `#1f242c` | Row hover |
| `--onyx-border` | `#262c35` | Hairlines (1px) |
| `--onyx-text` | `#e6e9ee` | Primary text |
| `--onyx-text-secondary` | `#b3bcc8` | Secondary text (lifted from `#9aa3b0`; ≥ 10:1 on `--onyx-bg`) |
| `--onyx-text-muted` | `#8f98a6` | Disabled, placeholders (lifted from `#6b7480`; ≥ 6.5:1 on `--onyx-bg`) |

Text tiers are contrast-first: every token used for body copy must clear WCAG AA on its
background, so the secondary/muted neutrals sit one step up the ramp from the original
zinc values. The original `#9aa3b0`/`#6b7480` remain valid for large or decorative
surfaces (file-type chips, watermarks) where AA body-text contrast does not apply.

**Accent — "Onyx Blue"** (`#3b9dff` hue family), one accent only, used sparingly for focus,
active states, and primary actions. Hover `#6cb6ff`, subtle tint `rgba(59,157,255,0.12)`.

**Semantic:**

| Token | Value | Meaning |
|-------|-------|---------|
| `--onyx-success` | `#3ddc84` | Healthy, synced, complete |
| `--onyx-warning` | `#f5b544` | Degraded, attention |
| `--onyx-danger` | `#f25f5f` | Data loss, errors, destructive |
| `--onyx-info` | `#5ac8fa` | Informational, in-progress |

File-type colors follow a fixed 8-color palette (document `#f5b544`, image `#5ac8fa`, video
`#b07cf0`, audio `#3ddc84`, archive `#ff7ab2`, code `#6cb6ff`, disk/image `#f5a97f`,
other `#9aa3b0`) — rendered only as 8px icon chips so the UI never looks rainbow.

### 2.2 Typography

| Role | Family | Notes |
|------|--------|-------|
| UI | Inter (variable) | -0.011em tracking; self-hosted, subset |
| Mono | JetBrains Mono | Terminal, logs, code, hashes |
| Numeric | `font-variant-numeric: tabular-nums` | Tables, stats, sizes |

Scale (rem): 12 / 13 / 14 (base) / 16 / 18 / 22 / 28 / 34. Display faces for dashboard
headlines use the same scale at tighter leading — no decorative display font.

### 2.3 Spacing, radius, elevation

- **Spacing:** 4 px grid (`--space-1…--space-16`). Density control: `comfortable` (default) and
  `compact` toggle per user.
- **Radius:** `--radius-sm: 6px`, `--radius-md: 10px`, `--radius-lg: 16px`; dialogs and drawers
  use 12–16 px; buttons 10 px. VisionOS-soft, Windows-11-rounded.
- **Elevation:** shadows only for floating surfaces (menus, dialogs, command palette): 2-layer
  shadow with 24 px blur + hairline border; glass surfaces use blur instead of shadow.
- **Motion:** 150 ms (micro), 220 ms (standard), 320 ms (large/enter) with `cubic-bezier(.2,.8,.2,1)`;
  drag gestures get spring curves. Reduce motion: all animation collapses to opacity-only when
  `prefers-reduced-motion`.

## 3. Layout system

```
┌───────┬──────────────────────────────────────────────────────┐
│       │  Top bar: breadcrumbs · global search · status · me  │
│ Side  ├──────────────────────────────────────────────────────┤
│ bar   │                                                      │
│ (Arc- │                Content area                          │
│ style)│   lists · grids · drawers · detail panes             │
│       │                                                      │
│ 32px  ├──────────────────────────────────────────────────────┤
│ mini  │  Activity rail (collapsible): tasks, events, health  │
└───────┴──────────────────────────────────────────────────────┘
```

- **Sidebar (arc-style):** 240 px; collapsible to a 64 px icon rail. Sections: *Dashboard,
  Files, Snapshots, Backups, Apps, Shares, Users, Settings*, plus a bottom cluster for
  *Notifications, Activity, Help/AI*. Each item shows live badges (e.g., snapshot count,
  app update count).
- **Command palette (⌘K / Ctrl-K):** the fastest path to everything — search apps, shares,
  files, actions ("create snapshot", "restart Jellyfin", "add user"). Result groups with
  keyboard navigation, fuzzy matching, and recent-items memory.
- **Top bar:** contextual breadcrumbs (Files: `Pool › Photos › 2026`), global search, a live
  status pill (disk health / scrub progress / update available), notification bell, user menu.
- **Activity rail:** right edge, collapsible; streams jobs (scrub %, backup progress,
  app installs) and system events. Live via WebSocket.
- **Responsive:** full layout ≥ 1280 px; tablet collapses the rail; phone opens a mobile
  shell with bottom tabs (Dashboard, Files, Activity, Settings).

## 4. Component catalog (v1)

**Primitives:** Button (primary/secondary/ghost/danger, icon, loading), Input, Select,
Combobox, Switch, Checkbox, Radio, Slider, Tooltip, Badge, StatusBadge, Avatar, Spinner,
Skeleton, EmptyState, Divider.

**Composite:** DataGrid (virtualized, sortable, column-resize, selection model, inline edit),
Tree (file tree), Tabs, Breadcrumb, Drawer, Dialog, Popover, ContextMenu, Toast, CommandPalette,
Pagination, Stepper, Timeline, ActivityFeed, NotificationCenter, Form (schema-driven with inline
validation), Wizard (onboarding), CodeBlock (terminal/log viewer with ANSI color), Chart
(StorageRing, Sparkline, AreaChart, Heatmap), FilePreview (image/video/audio/PDF/text/doc), 
UploadQueue, TransferList, QuotaBar, HealthCard, StatCard.

**Behavioral specs (excerpts):**

- *DataGrid* — sticky header, column pinning, `Shift` multi-select, `Space` preview, `Enter`
  open, inline rename on `F2`, type-ahead search, row actions in a `⋯` menu, bulk action bar
  that appears on selection.
- *Toast* — stacked bottom-right, 4 s default, progress variant for jobs, action buttons
  ("Undo delete" with a 30 s undo window on non-destructive deletions).
- *ContextMenu* — right-click everywhere with data; menu items always show their keyboard
  shortcut.
- *EmptyState* — every list has one, with a single primary action ("Create your first share").

## 5. File explorer spec

The flagship surface. Split-pane layout:

- **Left:** file tree (shared folders + browse of any mounted pool), favorites, recent,
  tags.
- **Center:** file list (DataGrid) or thumbnail grid (toggleable, for images/videos).
- **Right:** preview/context drawer — metadata (size, checksum, created, owner, ACL),
  preview, version history (snapshot timeline), share link, tags, activity.

Interactions: drag-drop upload with progress + pause/resume; drag files between folders;
`⌘/Ctrl+Z` undo for moves/deletes within a session; breadcrumb navigation with drop-downs at
each level; multi-select with `Shift`/`⌘`; search within folder or global (server-side,
with filters: type, size, date, tags, owner); "Open in" menu to copy SMB/WebDAV/rsync paths.

Accessibility: full keyboard model (arrows, `Space` select, `Enter` open, `⌘A`, `⌘F`),
ARIA roles for grid/tree, focus-visible rings at 2 px accent, `prefers-reduced-motion` honored.

## 6. Dashboard

A glanceable, widget-grid home:

- **Storage card:** capacity ring per pool + per-dataset bars; scrub progress; snapshot health.
- **System card:** CPU/RAM/temps/fan with sparklines; UPS state; uptime.
- **Network card:** throughput sparkline, per-protocol active sessions.
- **Activity card:** recent file changes, logins, jobs (from `onyx-bus`).
- **Quick actions:** Create share, Run backup, Take snapshot, Install app, Add user.
- **Health summary banner:** zero-config "All systems healthy" or a prioritized issue list.

## 7. Technology

- **Stack:** React 19 + TypeScript strict, Vite, TanStack Query (server state/caching),
  Zustand (UI state), React Router, `@tanstack/react-virtual` for lists.
- **Styling:** Tailwind CSS v4 with the Prism tokens mapped to theme variables; no component
  library — Prism components are first-party.
- **API:** typed client generated from OpenAPI (`onyx-sdk` TS), WebSocket client for events.
- **i18n:** all strings through `i18next`; en-US default, community locales via Crowdin.
- **Performance:** code-split per route, `content-visibility` on long lists, streaming
  thumbnails, virtualized grids — budgeted in §02.9.
- **Delivery:** built once, served by `onyx-api` from the read-only root (immutable asset
  hashes), so the UI is never writable at runtime.
