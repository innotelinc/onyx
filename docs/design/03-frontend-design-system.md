# 03 — Frontend and design system

The ONYX console is built on **Unity**, the shared Innotel theme. Unity is not a
component library and not a build step: it is one palette, one theme switch and a
small semantic vocabulary that every product carries as a byte-identical copy.
ONYX wears the **operations** scheme, with light, dark and follow-the-system
modes.

The console itself is deliberately dependency-free HTML, CSS and one inline
script. It is served by `onyx-web` (nginx); there is no bundler, no framework
runtime and no asset pipeline to keep alive on an appliance. That is a feature:
the surface that recovers a broken pool must not depend on the surface that
failed to build.

## 1. Principles

1. **Calm by default.** Dense while you work, quiet when you are not. No
   decorative motion competing with capacity or health.
2. **One heading per screen.** The masthead names the page; the content starts
   with the lede and the actions.
3. **Status is text and shape as well as colour.** A green dot always has a
   label; an unknown state has its own token and is never painted as success.
4. **The rail is the only navigation, and the only collapse control.** There is
   no second, competing sidebar or activity panel.
5. **Destructive actions look destructive.** Erase, delete, purge and rollback
   are confirmed in words that name what is lost.
6. **Every action is one keystroke from anywhere.** `Ctrl/Cmd+K` opens the same
   palette on every page.

## 2. The theme

The canonical files are `theme/unity-theme.css` and `theme/unity-theme.js` in
the family repository. ONYX ships byte-identical copies at:

- `docker/onyx-web/unity-theme.{css,js}` — served beside the console at
  `/unity-theme.{css,js}` and baked into the `onyx-web` image.
- `web/landing/unity-theme.{css,js}` — served beside the static landing page.

A change to the palette is a change to the canonical file and every copy. The
theme's own test fails when a converted product drifts.

### 2.1 Schemes and modes

| Axis | Values | Stored as |
|------|--------|-----------|
| Scheme | `desk` (violet), `operations` (graphite/blue), `soc` (navy/cyan) | `data-scheme`; ONYX defaults to `operations` |
| Mode | `light`, `dark`, or absent (follow the machine) | `data-mode`, in `localStorage["ontrak.mode"]` |

The pre-paint snippet in `index.html` applies the stored mode before the
stylesheet paints, so a dark screen never flashes white. The toggle cycles
system → light → dark and calls `UnityTheme.cycle()`; Settings exposes the same
choice as a select.

### 2.2 Tokens

No component names a raw colour. The semantic tokens are:

| Group | Tokens | Use |
|-------|--------|-----|
| Canvas | `--canvas`, `--surface`, `--surface-muted`, `--surface-sunken` | page, panels, hover, input wells |
| Lines | `--line`, `--line-strong` | hairlines and field borders |
| Ink | `--ink`, `--ink-soft`, `--ink-faint` | primary, secondary, placeholder |
| Brand | `--brand`, `--brand-soft`, `--brand-ink`, `--brand-ring` | actions, selection, focus |
| Status | `--ok`, `--info`, `--bad`, `--attention`, `--unknown` and their `-soft` pairs | health, refusals, capacity pressure, never-checked |
| Glass | `--glass-tint`, `--glass-tint-strong`, `--glass-line`, `--glass-shadow` | the rail, masthead, cards |
| Shape | `--radius-sm`, `--radius`, `--radius-lg`, `--radius-pill` | fields, panels, dialogs, pills |
| Type | `--font-sans`, `--mono` | UI and numbers/paths |
| Spacing | `--space-1…--space-6` | 4 px grid |

Feature views written before the migration still use the old
`--onyx-*` names inside inline styles; the shell maps each one onto a Unity
token rather than leaving a second private palette behind.

## 3. Shell

```
┌───────────────┬──────────────────────────────────────────────────────┐
│  ONYX         │  Overview            [search or jump… ⌘K]  ●  ◐  ON │
│  Workspace    ├──────────────────────────────────────────────────────┤
│   Overview    │                                                      │
│   Files       │   capacity ring · service health · attention queue   │
│   Search      │   pools · recent access activity · quick actions     │
│  Storage      │                                                      │
│   Storage     │                                                      │
│   Snapshots   │                                                      │
│   Backups     │                                                      │
│  Platform     │                                                      │
│   Apps …      │                                                      │
│  ─────────    │                                                      │
│  ‹ Collapse   │                                                      │
│  ON Operator  │                                                      │
└───────────────┴──────────────────────────────────────────────────────┘
```

- **Rail:** 252 px expanded, 76 px collapsed. One control: the button at the
  bottom of the rail, or `Ctrl/Cmd+B`. The preference persists in
  `localStorage["onyx.rail"]`. On ≤900 px the rail becomes an off-canvas drawer
  with a scrim; the masthead's menu button opens it.
- **Sections:** *Workspace* (Overview, Files, Search), *Storage* (Storage,
  Snapshots, Backups), *Platform* (Apps, Virtual machines, Objects, Shares,
  Users, Audit, Settings). Scrub is a maintenance action on Storage, not a
  destination of its own.
- **Masthead:** the current page title and lede, the command trigger, the live
  service pill, the Unity theme toggle and the account mark.
- **Command palette:** `Ctrl/Cmd+K`. Pages, layout and theme actions, and — once
  there is a query — a pool-wide file search. Choosing the search command fills
  the Search page and runs it.
- **No activity rail.** Activity lives on the Overview next to capacity and
  attention, where it can be read in context. The old right-hand rail was
  placeholder text and has been removed.

## 4. Overview

The landing page answers four questions without opening another page:

1. **How full is the box?** A capacity ring plus used/free/total, read from
   `/storage/overview`.
2. **Is every daemon serving?** The count and a per-service list from
   `/system/status`.
3. **What needs a decision?** Offline pools, storage notices, capacity above
   85 %, and refused requests from `/audit/access`.
4. **What happened recently?** Grants, revocations and refusals, newest first.

Pools are listed with their own used/total and state; quick actions lead to
Files, Snapshots, Shares and Audit.

## 5. Components in use

Buttons (default, primary, disabled), fields and selects, checkboxes, cards,
KPI tiles, capacity ring, status pill, status dots, tags, badges, empty states,
toast, modal command palette, disclosure panels (share access, share history,
SMB password, scrub schedule, remote-account setup), and feature rows.

Every list has an empty state with a next action. Errors from an action appear
as a toast **and** an inline alert; a page that cannot load says what is missing
where the content would have been.

## 6. Accessibility

- A skip link precedes the shell.
- The rail uses links with `aria-current="page"`; the collapse button carries
  `aria-controls` and `aria-expanded`.
- The palette is a modal dialog with `role="dialog"`, `aria-modal`, a labelled
  input and `aria-selected` options; arrow keys, Enter and Escape work.
- Focus-visible rings use `--brand-ring` at 2 px.
- Status is never colour-only: every dot is paired with text.
- `prefers-reduced-motion` collapses transitions and animations.
- The page remains usable at 390 px with no horizontal overflow.

## 7. Technology and delivery

- **Server:** `onyx-web` (nginx) serves `index.html` plus the two Unity files.
  The image is built by `docker/onyx-web/Dockerfile` and published to GHCR.
- **API:** same-origin `/api/v1`; on `app.onyx.innotel.us` the console calls
  `https://api.onyx.innotel.us/api/v1`.
- **State:** the URL hash is the router (`#/files`, `#/storage`, …); preferences
  (rail, theme, pending search, backup source) live in `localStorage` or
  `sessionStorage`.
- **No build step.** A change is a file change; the image copies three files.
- **Testing:** `scripts/web-ui-check.js` runs the inline application script in a
  VM with a DOM stub and asserts the render contracts that have broken before.
  Browser checks exercise the real page at desktop and phone sizes.

## 8. Later

The component catalog in later milestones can grow (virtualised grids, preview
drawer, thumbnails, live job streams), but it does so inside this shell and
vocabulary. A framework migration is not a goal in itself; the console stays
static as long as that is the smaller, more reliable surface.
