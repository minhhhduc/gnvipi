# Admin Console

The Admin Console is the server-rendered UI at `/admin`. It has no framework, no
build step, and no external requests: every byte of CSS and JS is a Go string
constant served with the page, so the console works on a machine with no
internet access (#166).

## Pages and routing

`adminHTML(page, catalog)` (`cmd/serve/admin_pages.go`) switches on `?page=`:

| `?page=` | Page | Notes |
|---|---|---|
| `dashboard` (default) | Stats: traffic charts, request log | |
| `models` | Upstream providers + model catalog | also the fallback for an unknown value |
| `config` | Auto model, web search, model cycle | |
| `chromes` | Captcha Chrome worker pool | |
| `automode` | alias for `config` | |

`adminHead(active)` (`cmd/serve/admin_shell.go`) emits the doctype, meta, the
whole stylesheet, and the `<header>`: brand, tab bar, and the page's sub-line.
The models filter bar (`modelsBarHTML`) lives inside the header so it stays
pinned to the top while the catalog scrolls. `adminHTML` then appends `<main>`,
the page's data as JS constants (`PAGE`, `STATS_READ_MS`, `FAILED`, `PROVIDERS`,
`PROVIDERS_V2`, `DELETED`), and one `<script>` built by concatenating the
per-page scripts in `cmd/serve/admin_script.go`:

```go
const adminScript = adminBaseScript + adminProviderScript + adminCycleScript + adminStatsScript + adminChromeScript
```

Each script guards on `PAGE` before touching the DOM, so all five ship to every
page and only the matching one runs. Adding a page means adding a `case` to the
switch in `adminHTML`, a label and sub-line in `admin_shell.go`, and a script
whose first line checks `PAGE`.

## Components

The stylesheet defines a small vocabulary. Composition is by nesting, not by
per-page classes.

| Class | Role |
|---|---|
| `.sheet` | A bordered panel. Its direct children are separated by hairlines instead of each being its own card. |
| `.sheet-head` / `.sheet-title` / `.sheet-body` / `.sheet-foot` | Panel header (title, count, actions), padding wrapper, and the action bar pinned to the bottom. A `<details class=sheet>` uses its `<summary class=sheet-head>` as the toggle. |
| `.grp` + `.cnt` | Publisher divider inside the model catalog, with its row count. |
| `.cols` | Auto-fit grid (`minmax(19rem, 1fr)`) for placing whole sheets side by side. |
| `.sec` | Vertical stack with no border, for content that is not a panel. |
| `.row` / `.static` | One line of a list. `.static` rows are not click-to-toggle. |
| `.chip` | Monospace identifier (`provider id`, `#0`). |
| `.pos` | Leading position number. Used only in the model cycle, where the order is the failover sequence. |
| `button.primary` | The single commit action of a sheet. |
| `.tiles` / `.tile` | KPI row at the top of a panel. |

## Design tokens

Every colour, size and space on the page is a custom property on `:root`, so the
whole console retunes from one block (#166).

| Group | Tokens |
|---|---|
| Surface | `--bg` (page plane), `--bg-2`, `--panel` (sheet), `--panel-2` (rows, wells), `--line`, `--line-2` (hairlines and their hover step) |
| Ink | `--fg`, `--dim`, `--dim-2` |
| State | `--on`, `--bad`, `--warn`, `--accent` (signal), `--on-accent` (ink on accent fills) |
| Type | `--fs-2xs` … `--fs-2xl`, `--lh-tight/snug/base`, `--fw-normal/medium/semi/bold`, `--track-tight`, `--track-micro` |
| Space & radius | `--sp-1` … `--sp-5`, `--r-sm`, `--r`, `--r-lg` |

The identity is a warm graphite casing — `#12100d` page, `#1e1a15` sheets,
`#f4ede3` ink — with one cool teal (`#2fc4c0`) doing all the signalling:
the header rail, the active-tab underline, sheet title ticks, and focus rings.
Type is the system stack; mono is reserved for identifiers and figures (`.chip`,
`.pos`, table cells, axis ticks).

Chart chrome is drawn from the page tokens — `.gridline`, `.crosshair` and the
2px surface ring on end dots (`stroke:var(--panel)`) — so the charts read as part
of the console, not a pasted-in library. The eight categorical series hues are a
fixed, colourblind-checked order stored in `MCOLS`
(`cmd/serve/admin_script_stats.go`): assign them by position, never cycle, and
re-run the check before adding a ninth. The chart scripts draw against `--panel`,
so changing the palette means re-checking the charts, not just the CSS.

## Behaviour worth knowing

- **Every toggle saves.** A model switch, the auto-model select, the search
  provider and the cycle checkboxes all POST on change; there is no Save button
  for them. The console reloads only where the server response changes the
  rendered list.
- **The provider editor is collapsed.** `<details id=editor-panel>` opens
  itself from `loadProviderForEdit` and the Add provider button, so the form
  never occupies the page before you ask for it.
- **Publisher dividers follow the filter.** `pruneGroups` in
  `adminBaseScript` hides a `.grp` whose rows are all filtered out.
- **Chromes poll.** The worker sheet re-reads `/admin/chromes` every 5s and
  reflects pool state in the header badge.

## Invariants

These are load-bearing; the redesign did not change them.

- Endpoints: `/admin/models`, `/admin/providers`, `/admin/stats`, `/admin/chromes`,
  `/admin/cycle`, `/admin/automode`, `/admin/search`, `/admin/delete`,
  `/admin/api/providers*`.
- Nothing is loaded from a third-party origin — no CDN, no font service, no
  analytics. `TestAdminPages_*` in `cmd/serve/admin_pages_test.go` pins the
  markup contracts the scripts and handlers rely on, so a redesign that renames
  an id fails the suite rather than the browser.
- CSRF, same-origin checks, and the loopback-only cookie logic are unchanged;
  see `cmd/serve/admin_access.go`.
