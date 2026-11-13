# Admin UI rebuild — design

Date: 2026-11-11
Supersedes: issue #183 (frontend redesign proposal)
Status: awaiting approval — no code written

Issue / branch: issue **#184** (next number; to be created after approval),
branch **`feat/184-admin-ui-rebuild`**.

## Intent

Keep the backend exactly as it is. Rebuild the whole Admin UI/UX from scratch
under `web/admin/`: new layout, new interaction model, new design system,
light and dark themes. The API is the only preserved contract.

The current `app.js` (1,117 lines of hand-built DOM) is the ceiling. A
component model plus a coherent design system is the point; a reskin is not.

## 0. Hard constraints

- **No Go file changes.** `cmd/serve/admin_ui.go`, `web/admin/embed.go`, and
  every `/admin/api/*` handler stay byte-identical.
- **No API contract changes.** No field renames, response-shape changes,
  status-code changes, auth changes (same-origin cookie, `401` →
  `/admin/login`) or CSRF changes.
- **No new routes.** The server routes only `/admin`, `/admin/`,
  `/admin/app.css`, `/admin/app.js`. Client routing stays `?page=…`.
- **Exactly three build outputs** in `web/admin/`: `index.html`, `app.js`,
  `app.css`. No hashed names, no `assets/` directory, no CDN, no web fonts,
  no remote images.
- **No invented metrics.** Every label and value maps to a field the API
  returns. Where a requested visualisation has no backing field, the design
  says so and derives it from existing fields or omits it.
- The exact API fields per view are listed below, before each view's design.

## 1. Genuine redesign, and why the rail stays

The redesign changes information architecture and interaction, not only
colour and components:

- **Stats** becomes a health-first dashboard: a balanced 4×2 KPI grid, a 2×2
  chart board, and the log as a full-width secondary surface. The sparklines
  are gone; the charts are the single source of trends.
- **Models** becomes a master–detail workspace: providers are compact
  selectable rows on the left, the catalog is a filterable table on the right,
  and the provider editor opens as a right-hand drawer. Actions collapse into
  a `⋯` menu.
- **Config** becomes three balanced surfaces with per-surface save state, and
  the cycle editor splits selected (priority order, always fully visible) from
  unselected.
- **Chrome** becomes a status-first page: a large state hero, then the worker
  table, with explicit disabled reasons and confirmations.
- **Global** gains a health pill in the page header, an auto-refresh
  indicator, a persistent error region, and a theme toggle in the rail
  footer. There is no command palette and no collapsible rail.

Why keep the left rail: four stable sections, high information density, and
persistent global status need a persistent frame. A top bar would either
duplicate the status area or crowd it, and the user explicitly dropped the
compact top-nav variant. The rail is retained because it is the best frame
for a console with four destinations plus always-visible health, theme, and
connection state — not because it is the safe choice. It stays fixed-width;
the theme toggle lives in its footer, while the health pill sits in the page
header where it is visible on every page and cannot be cut off by a short
viewport.

## 2. Design system

Tokens (CSS variables, both themes):

| Token | Light | Dark |
|---|---|---|
| `--bg` | `#f8f9fb` | `#0b0f17` |
| `--panel` | `#ffffff` | `#131a24` |
| `--panel-2` | `#f3f5f8` | `#18212e` |
| `--line` | `#e5e7eb` | `#243041` |
| `--line-2` | `#d3d8e0` | `#334155` |
| `--fg` | `#111827` | `#e5edf7` |
| `--dim` | `#6b7280` | `#94a3b8` |
| `--dim2` | `#9aa3b2` | `#64748b` |
| `--accent` | `#4338ca` (indigo) | `#818cf8` |
| `--accent-soft` | `#eef0ff` | `#1e1b4b` |
| `--on` | `#047857` | `#34d399` |
| `--on-soft` | `#e7f6ef` | `#0d2b23` |
| `--bad` | `#b91c1c` | `#f87171` |
| `--bad-soft` | `#fdecec` | `#331516` |
| `--warn` | `#b45309` | `#fbbf24` |
| `--warn-soft` | `#fdf3e3` | `#332610` |
| `--shadow` | `0 1px 2px rgba(16,24,40,.05), 0 1px 3px rgba(16,24,40,.07)` | `0 1px 2px rgba(0,0,0,.3)` |

This palette is deliberately distinct from the current blue
(`#1f5fd0`) and from the previous dark mockup (`#58a6ff`). Type: system sans
for UI, system mono for ids, URLs and numbers (`font-variant-numeric:
tabular-nums`). Spacing scale 4/8/12/16/24/32. Radii 8/10/12. Motion ≤160 ms,
disabled under `prefers-reduced-motion`. Focus rings use `--accent` at 2 px
offset. Contrast target: WCAG AA for text and state chips in both themes.

Theme behaviour: `data-theme="light|dark"` on `<html>`; toggle in the rail
footer; `localStorage["gw-theme"]`; default from `prefers-color-scheme`; a
tiny inline boot script applies the stored value before first paint.

## 3. Views

### 3.1 Stats

**API fields used** — `GET /admin/api/stats`:

- `models[]`: `model`, `requests`, `errors`, `streamed`, `input_tokens`,
  `output_tokens`, `first_ok`, `last_ok`, `avg_ms`, `last_ms`, `ttfb_avg_ms`,
  `ttfb_last_ms`, `ttfb_min_ms`, `ttfb_max_ms`
- `series[]`: `minute`, `requests`, `errors`, `input_tokens`,
  `output_tokens`, `ttfb_avg_ms`
- `series_by_model`: `{ [model]: series[] }`
- `events[]`: `time`, `model`, `input_tokens`, `output_tokens`, `ms`,
  `ttfb_ms`, `streamed`, `error`
- `started_at`, `total_frames`
- `GET /admin/api/stats?frames=N`: `frame`, `total_frames`, `events[]`;
  `429` + `Retry-After` when reads are throttled.

Derived client-side from the fields above: total requests (sum
`models[].requests`), errors % (`errors / requests`), avg TTFB (weighted
`ttfb_avg_ms` by `streamed`, over the **last 200 events**), p95 latency
(percentile of `events[].ms`, over the **last 200 events**), uptime
(`started_at`). The two event-derived KPIs are labelled "last 200 events" in
the UI; they are not lifetime figures. No other metric exists, and none is
invented.

Top models: compute each model's window total from
`series_by_model[model]` (`sum(requests)`), take the top **5** (ties broken by
model id), and draw one line per model with a legend. The selection is fixed
for the render; it does not reshuffle on every poll.

| Current problem | Solution | Related API fields |
|---|---|---|
| 7 KPI cards + lone `uptime` leave a grey hole | 4×2 grid; order: requests, req/min, in tokens, out tokens, errors %, avg TTFB, p95, uptime | `models[].*`, `series[].requests`, `started_at` |
| Sparklines duplicate the charts | Remove sparklines entirely; KPI delta/context text instead | `series[]` already feeds charts |
| Y-axis repeats `1 1 1 0 0` on small values, or clips the line at the top | "Nice" tick step (1/2/5 × 10ⁿ); the axis maximum is rounded **up** to the next tick so it is always ≥ the data maximum; deduplicated labels; lines span the full time axis; tested | `series[]`, `series_by_model` |
| "avg TTFB / min" drops alone to a new row, right half empty | Fixed 2×2 chart board, equal heights: requests, tokens, TTFB, top models | `series[]`, `series_by_model` |
| Zero data draws flat lines; TTFB zero draws a drop to 0 | Per-chart empty state "No requests yet" when the series has no activity; **every** `ttfb_avg_ms = 0` (and null) point is a line break, never a zero; Vitest covers `[0, null, 100]` segments | `series[]`, `models[].streamed` |
| No refresh feedback | Header shows "Updated Ns ago · auto-refresh Ns", paused state when the tab is hidden | `started_at` for context only |
| Log is a separate paginated widget with its own sort | Keep frame paging (`?frames=N`) and column sorting, restyled as a full-width table; honour `Retry-After` on 429 | frame fields above |

Health pill (derived, no new API; **page header only**, same position on all
four pages): from `sum(errors)/sum(requests)` and `sum(requests)`.

| Traffic | Pill |
|---|---|
| fewer than 20 requests | neutral "Low traffic · N requests" (not a health verdict) |
| 0 requests | neutral "No traffic yet" |
| ≥ 20 requests, 0 errors | green "Healthy · 100% success" |
| ≥ 20 requests, error rate < 5% | amber "Degraded · X% errors" |
| ≥ 20 requests, error rate ≥ 5% | red "Failing · X% errors" |

### 3.2 Models

**API fields used**:

- `GET /admin/api/providers` → `Provider[]`: `id`, `name`, `base_url`,
  `proxy_url`, `protocol`, `auth.scheme`, `auth.key`, `auth.key_env`,
  `auth.key_ref`, `auth.header_name`, `headers`, `models[]` (`alias`,
  `upstream_model`, `hidden`, `fallbacks`, `fallback_statuses`,
  `legacy_position`, `allow_shadow`), `timeouts` (`connect`, `tls`,
  `response_header`, `idle`, `total`), `enabled`, `tls`, `forward_headers`,
  `endpoint_path`, `extra_body`, `extensions`, `max_concurrency`,
  `queue_timeout`
- `PUT/DELETE /admin/api/providers/{id}`, `POST /admin/api/providers/{id}/test`
  (and `/providers/test` for the unsaved editor) → test result: `ok`,
  `status` (`success`/`auth_failure`/`connect_failure`/`error`),
  `status_code`, `latency_ms`, `message`, `error`
- Validation errors: `error`, `field`, `errors[]`, `fields{}`,
  `field_errors[]`
- `GET /admin/api/models` → `models[]` (`id`, `name`, `publisher`, `hidden`,
  `custom`, `base_url`), `off`, `deleted[]`
- `POST /admin/api/models` body `{masks_id: []}` → `{off}`
- `DELETE /admin/api/models/{id}` → `{deleted}`;
  `POST /admin/api/models/{id}/restore` → `{restored}`;
  `POST /admin/api/models/retry-failed` → `{ok: true}`

| Current problem | Solution | Related API fields |
|---|---|---|
| URLs truncated (`https://openrouter.ai/...`) | Full URL wraps to two lines with a copy button and a `title` tooltip | `base_url`, `proxy_url` |
| Alias red dot reads as an error | Neutral chips; hidden aliases get a "hidden" label, not red | `models[].hidden` |
| Edit/Test/Delete buttons take three rows of height | Compact row + `⋯` menu with Edit / Test / Delete; Delete opens a confirmation dialog | provider `id`, test result fields |
| Catalog repeats name and id; a separate "Provider" column duplicates the publisher | Two text columns: Model (name primary, publisher as muted secondary line) and ID (mono, muted). No Provider column | `models[].name`, `models[].id`, `models[].publisher` |
| "19 on · 0 off" counter too faint | Prominent status summary in the header plus All / On / Off quick filters | `models[].hidden`, `off` |
| Provider errors and test outcomes unclear | Persistent inline result: badge, latency, `status`, `status_code`, message/error; failures also go to the page error region | test result fields; `error` responses |
| Editor is an inline `<details>` that shifts the page | Right-hand drawer (or modal) with grouped fields and a sticky action bar; unsaved-change guard | create/update payload fields listed above |

Deleted models: a collapsed "Deleted" section from `deleted[]` with Restore
(`POST …/restore`). `POST /admin/api/models` sets the full hidden list, so
toggle changes are debounced and sent as one `masks_id` array.

Provider status display: the dot/badge reflects `enabled` only (green =
enabled, grey = disabled). It never implies a successful connection test.
Until a test runs in this page session, the provider row says "not tested";
after one runs it says "last test in this session: <status>" with latency and
`status_code`. Test results are **not persisted** by the API, so the label and
the value reset on reload — the UI must not show a green "working" dot based
on a test result, and must not show a green dot next to a red failure.

Provider editor layout: right-hand drawer (chosen over a modal so the list
stays visible) with grouped sections — Identity, Authentication, Model
mappings, Headers & routing, Timeouts & limits — a sticky footer with Test
connection / Cancel / Save, inline `field_errors[]` under the offending field,
an unsaved-changes guard on close, and a test-result area in the footer. Provider ID is read-only while editing; "API key env var" is the label for `auth.key_env`; each mapping row expands to `fallbacks`, `fallback_statuses`, `allow_shadow` and `legacy_position`; the Advanced group covers `timeouts` (connect/tls/response_header/idle/total), `tls`, `extra_body`, `extensions`, `max_concurrency`, `queue_timeout` and `key_ref`. Every Provider field listed above is reachable from the editor.

### 3.3 Config

**API fields used**:

- `GET /admin/api/config/automode` → `model` (the response also contains
  `chat_responses`, which is **out of scope** and left untouched);
  `POST` body `{model}` → `{model}`
- `GET /admin/api/config/cycle` → `name`, `models[]`, `available[]`,
  `current`; `POST` body `{name, models[]}` → `{name, models[]}`
- `GET /admin/api/config/search` → `{url}`; `POST` body `{url}` → `{url}`
  (HTTPS URL containing `{query}`; invalid → `400 {error}`)
- `GET /admin/api/models` is reused to list visible model ids.

| Current problem | Solution | Related API fields |
|---|---|---|
| Auto model and Web search cards have mismatched heights | Balanced top row (1fr / 1fr), both cards the same height, ordered by importance: Auto model first | automode `model`; search `url` |
| Cycle has nested scrolling inside page scroll | No inner scroll: "Selected (priority order)" always fully visible, "Unselected" in a separate full list; up/down buttons; add/remove | cycle `models`, `available`, `current` |
| Save state is implicit (`toast` only) | Per-surface state: idle / Saving… / Saved HH:MM:SS / Error, plus retry; sticky save bar for cycle | POST responses and `error` |
| Auto-model selector can go stale | Selector built from visible `models[]` + `cycle` + current `auto.model`, exactly as the backend validates | `models[]`, cycle `models`, automode `model` |

`chat_responses` stays out of scope: the API returns and accepts it, but the
Config page neither displays nor sends it, preserving today's behaviour.

### 3.4 Captcha Chrome

**API fields used**:

- `GET /admin/api/chromes` → `enabled`, `state` (`idle` / `enabled` /
  `stopped` / `starting` / `stopping`), `chromes[]` (`index`, `pid`, `busy`,
  `killed`, `warmed`, `last_ok`, `extracts`), `ready`, `error?`
- `POST /admin/api/chromes` body `{index?, mode, all?}` with `mode` ∈
  `start|resume|kill|stop|pause` → same status shape; error responses:
  `500` (all) or `404` (single) with `error` populated, `400` invalid mode
- When the lifecycle is unavailable: GET returns `{enabled:false,
  state:"idle", chromes:[], ready:0}`, POST returns `503 {error}`.

| Current problem | Solution | Related API fields |
|---|---|---|
| Page nearly empty; the idle state is tiny | State hero: large `state`, then ready tokens, workers running, in use, extracts; idle shows a factual empty state ("No Chrome processes allocated — Start All launches the pool") and no sample/demo data | `state`, `ready`, `chromes[]`, `enabled` |
| Workers are dense single rows | Worker table: index, state badges (running / in use / warm / killed), PID, extracts, last OK, Kill | `chromes[]` fields |
| Kill All disabled with no explanation | Tooltip/help text with the reason derived from `state`/`enabled`; never leave a disabled control unexplained | `state`, `enabled` |
| Kill is destructive without confirmation | Confirm dialog for Kill All and for a single Kill; Start/Kill show optimistic "working…" state | POST mode fields |

The mockups show two genuinely distinct states: `chromes-light/dark` (idle,
from `{enabled:false,state:"idle",chromes:[],ready:0}`) and
`chromes-running-light/dark` (a pool with two workers, all rows sourced from
`chromes[]`). Every number on the page comes from the API; the page never
explains captcha warming with copy the backend does not guarantee.

### 3.5 Global shell

- Header title/actions aligned to the content column; no more title separated
  from the page body.
- Health pill in the page header, same position on every page; derived from
  `stats.models[].requests/errors` and `started_at`. Thresholds and the
  minimum-sample rule are in §3.1. No new API. The rail footer contains only
  the theme toggle, so a short viewport cannot cut the health state off.
- Auto-refresh indicators: 2 s Stats (existing cadence), 5 s Chrome.
- Central error surface: toast for transient feedback plus a persistent,
  dismissible in-page error banner with the API `error` text and a retry
  action. On a stats read failure the page keeps all **8 KPI slots** and
  shows "—" for unavailable values rather than collapsing the grid.
- `401` anywhere → full navigation to `/admin/login`.
- Router: `?page=` with `popstate` support, direct entry to any page,
  unknown page → dashboard with a notice. In-flight fetches are aborted on
  navigation.

## 4. Mockups

Rendered from `docs/superpowers/specs/admin-ui-mockups.html`:

```
?view=stats|models|config|chromes|editor|dialogs|error
&theme=light|dark
&data=normal|tiny|empty        # Stats only
&state=idle|running            # Chrome only
&menu=<provider-id>            # Models: open the ⋯ menu on that row
&deleted=1                     # Models: expand Deleted models
```

Every number shown is either an API field listed above or a client-side
derivation of one; no metric is invented. The mockups are **visual-language
references** (layout, density, palette, component treatment), not a content
spec.

### Stats — normal traffic
![Stats light](mockups/stats-light.png)
![Stats dark](mockups/stats-dark.png)

### Stats — tiny data (0/1/2) and empty
Tiny data proves the Y axis uses unique nice ticks instead of repeated
`0 0 1 1`; the empty set shows the per-chart and whole-page empty states.

![Stats tiny light](mockups/stats-tiny-light.png)
![Stats tiny dark](mockups/stats-tiny-dark.png)
![Stats empty light](mockups/stats-empty-light.png)
![Stats empty dark](mockups/stats-empty-dark.png)

### Models — default, ⋯ menu, deleted models
![Models light](mockups/models-light.png)
![Models dark](mockups/models-dark.png)
![Models menu light](mockups/models-menu-light.png)
![Models menu dark](mockups/models-menu-dark.png)
![Models deleted light](mockups/models-deleted-light.png)

### Provider editor drawer — light / dark
Grouped sections, sticky Test/Save/Cancel bar, an inline `field_errors[]`
example, unsaved-changes badge, session-only test result in the footer.

![Editor light](mockups/editor-light.png)
![Editor dark](mockups/editor-dark.png)

### Config — light / dark
![Config light](mockups/config-light.png)
![Config dark](mockups/config-dark.png)

### Chrome — idle and running (light / dark)
![Chrome idle light](mockups/chromes-light.png)
![Chrome idle dark](mockups/chromes-dark.png)
![Chrome running light](mockups/chromes-running-light.png)
![Chrome running dark](mockups/chromes-running-dark.png)

### Confirmation dialogs and global error state
![Dialogs light](mockups/dialogs-light.png)
![Dialogs dark](mockups/dialogs-dark.png)
![Error state light](mockups/error-light.png)
![Error state dark](mockups/error-dark.png)

Differences vs current: balanced KPI grid and chart board; master–detail
models with drawer editor and `⋯` actions; explicit save states and a
non-scrolling cycle split; a status hero on Chrome with honest idle/running
states; a persistent error banner plus toast; the new indigo palette in both
themes.

## 5. Technical design

### 5.1 Stack

Preact + Vite + TypeScript + Tailwind CSS v4. Why Preact: same component
model as React at ~10 KB instead of ~140 KB. Why not vanilla/esbuild: five
interactive views would re-create today's hand-rolled DOM/state, which is
the problem being removed.

### 5.2 Build pipeline and artifact safety

```
web/admin/src/index.html      # Vite entry template + theme boot script
web/admin/src/**              # TSX sources
web/admin/dist/**             # build output, gitignored
scripts/sync-assets.mjs       # validates dist, then publishes 3 files
```

- Vite `outDir: "dist"`, `emptyOutDir: true`, `base: "/admin/"`, single entry,
  `inlineDynamicImports: true`, `modulePreload: false`,
  `cssCodeSplit: false`.
- `sync-assets.mjs` (run by `npm run build`) refuses to copy unless `dist/`
  contains **exactly** `index.html`, `app.js`, `app.css`; no hashed chunks, no
  `assets/`, no font/image files. It then copies only those three into
  `web/admin/`. It never writes `embed.go`.
- `embed.go` stays byte-identical and its directive must list the three files
  literally:

  ```go
  //go:embed index.html app.css app.js
  var FS embed.FS
  ```

  `node scripts/sync-assets.mjs --check` verifies this Go-free: it reads
  `embed.go` as text, asserts the directive matches exactly that file set
  (no `*`, no directory, no extra file), and exits non-zero otherwise. A
  normalized SHA-256 pin of `embed.go` is kept as a second guard.
- The three built files **are committed** to `web/admin/`, so Go builds,
  Docker, and releases need no Node. `dist/` and `node_modules/` are
  gitignored; nothing else may be written to `web/admin/`.
- Toolchain pinning: `web/admin/.nvmrc` (Node LTS, e.g. `22`) plus
  `"engines": {"node": ">=22"}` in `package.json`. Lockfile committed;
  `npm ci` is the only install command in CI.
- CI gains a `frontend` job in `.github/workflows/ci.yml`:

  ```yaml
  - uses: actions/setup-node@v4
    with: { node-version-file: web/admin/.nvmrc, cache: npm,
            cache-dependency-path: web/admin/package-lock.json }
  - run: npm ci && npm run typecheck && npm run test && npm run build
    working-directory: web/admin
  - run: git diff --exit-code -- web/admin
  ```

  The last step fails if the committed artifacts differ from a fresh build.
- Line endings: `.gitattributes` marks `web/admin/index.html`,
  `web/admin/app.js`, `web/admin/app.css`, `web/admin/embed.go`, and
  `web/admin/go.mod` with `eol=lf`, so Windows checkouts cannot produce a
  spurious artifact diff.
- Existing `release.yml` and the `Dockerfile` are unchanged: they consume the
  committed `web/admin/` files and never run Node.
- CI also enforces no backend drift with `git diff --exit-code -- '*.go'` and
  `git status --porcelain` checks.

### 5.3 Tailwind v4 output

`@tailwindcss/vite`; one CSS entry (`styles.css`) importing
`@import "tailwindcss";`, with no `@import url(...)`, no `@font-face`, no
`url()` to remote or bundled images. `cssCodeSplit: false` produces a single
`app.css`. `sync-assets.mjs` asserts exactly one CSS file, greps the CSS for
remote `@import`/`url(http)`/`@font-face`, and rejects any non-JS/CSS asset.
Fonts are the system stack; icons are inline SVG.

### 5.4 Router

`router.ts` reads `?page=`, validates against the four sections, updates
`document.title`, and listens to `popstate` for Back/Forward. Direct entry to
`?page=config` renders Config. Unknown values fall back to Stats with an
inline notice (no redirect loop). Navigation aborts in-flight requests.
`401` from any API call performs `location.assign("/admin/login")` and stops
rendering.

### 5.5 Data layer

`api.ts`: `api<T>(path, init)` with `credentials: "same-origin"`, JSON
encode/decode, typed response interfaces mirroring the fields above, abort
signals, and an error type carrying `status`, `message`, and the server
`error` field. `modelPath(id)` encodes each `/`-separated segment, keeping
the slashes literal.

### 5.6 Tests

- `npm run typecheck` — `tsc --noEmit`.
- `npm run test` — Vitest:
  - Chart: empty data → empty state; tiny data (1s and 0s) → unique axis
    labels; series with gaps (TTFB zeros) → gaps, not zero lines.
  - `api.ts`: `401` triggers `/admin/login`; malformed JSON surfaces a clear
    error; `429` keeps `Retry-After`.
  - `modelPath`: ids with one and multiple slashes.
  - Router: direct entry, unknown page fallback, popstate.
- `npm run build` + `node scripts/sync-assets.mjs --check` — exactly three
  artifacts; no remote/asset leakage; `embed.go` hash unchanged.
- `go test ./...` must pass unchanged.
- **Backend-unchanged gate:** `git diff --stat` must show **no `.go` files**;
  `git diff --exit-code -- '*.go'` must be clean.

Manual matrix: Stats/Models/Config/Chrome × light/dark; zero-traffic Stats;
provider test success/failure; delete confirmations; cycle reorder; chrome
start/kill with and without the lifecycle available; 401 redirect; offline
(no off-origin requests in DevTools).

## 6. Feature scope

Allowed (no new API): confirmation before destructive actions (provider
delete, model delete, kill, kill all), auto-refresh indicators, persistent
provider/test error display, health pill derived from existing stats, theme
toggle, copy-to-clipboard for URLs/ids.

Explicitly out of scope: command palette / global keyboard shortcuts beyond
the confirm/cancel dialogs, collapsible rail, and `chat_responses`.

Not allowed: anything needing a new endpoint, field, status code, auth
change, or backend edit.

## 7. Backend unchanged

Backend files that must not change (all Go files, specifically):
`cmd/serve/admin_ui.go`, `web/admin/embed.go`, `cmd/serve/admin_api.go`,
`cmd/serve/admin_handlers.go`, `cmd/serve/admin_cycle.go`,
`cmd/serve/chrome_lifecycle.go`, `cmd/serve/admin_access.go`, and every other
`*.go` file.

Endpoints used and their fields:

| Endpoint | Fields |
|---|---|
| `GET /admin/api/stats` | `models[]`(`model`, `requests`, `errors`, `streamed`, `input_tokens`, `output_tokens`, `first_ok`, `last_ok`, `avg_ms`, `last_ms`, `ttfb_avg_ms`, `ttfb_last_ms`, `ttfb_min_ms`, `ttfb_max_ms`), `series[]`(`minute`, `requests`, `errors`, `input_tokens`, `output_tokens`, `ttfb_avg_ms`), `series_by_model`, `events[]`(`time`, `model`, `input_tokens`, `output_tokens`, `ms`, `ttfb_ms`, `streamed`, `error`), `started_at`, `total_frames` |
| `GET /admin/api/stats?frames=N` | `frame`, `total_frames`, `events[]`; `429` + `Retry-After` |
| `GET /admin/api/providers` | `id`, `name`, `base_url`, `proxy_url`, `protocol`, `auth{scheme,key,key_env,key_ref,header_name}`, `headers`, `models[]{alias,upstream_model,hidden,fallbacks,fallback_statuses,legacy_position,allow_shadow}`, `timeouts{connect,tls,response_header,idle,total}`, `enabled`, `tls`, `forward_headers`, `endpoint_path`, `extra_body`, `extensions`, `max_concurrency`, `queue_timeout` |
| `GET/PUT/DELETE /admin/api/providers/{id}` | same Provider fields; delete → `{ok, deleted}` |
| `POST /admin/api/providers` | Provider fields → `201` Provider |
| `POST /admin/api/providers/{id}/test`, `/providers/test` | `ok`, `status`, `status_code`, `latency_ms`, `message`, `error` |
| `GET /admin/api/models` | `models[]{id,name,publisher,hidden,custom,base_url}`, `off`, `deleted[]` |
| `POST /admin/api/models` | `{masks_id: []}` → `{off}` |
| `DELETE /admin/api/models/{id}` | `{deleted}` |
| `POST /admin/api/models/{id}/restore` | `{restored}` |
| `POST /admin/api/models/retry-failed` | `{ok: true}` |
| `GET/POST /admin/api/config/automode` | `model`, `chat_responses` |
| `GET/POST /admin/api/config/cycle` | `name`, `models[]`, `available[]`, `current` (GET); `name`, `models[]` (POST) |
| `GET/POST /admin/api/config/search` | `url` |
| `GET/POST /admin/api/chromes` | `enabled`, `state`, `chromes[]{index,pid,busy,killed,warmed,last_ok,extracts}`, `ready`, `error?` |

Validation error envelope used by provider/model writes: `error`, `field`,
`errors[]`, `fields{}`, `field_errors[]`.

## 8. Decisions and remaining uncertainty

Decided in review; no longer open:

1. **`chat_responses`** — out of scope, neither displayed nor sent.
2. **Health thresholds** — as tabled in §3.1, including the <20-request
   "Low traffic" neutral state.
3. **Provider test results** — session-only, labelled "last test in this
   session"; the provider row's dot reflects `enabled`, never test outcome.
4. **Deleted models** — kept as a collapsed section with Restore.
5. **Palette** — the indigo palette (both themes) is accepted.

Still uncertain:

1. **Issue / branch**: issue **#183** already exists for this redesign;
   implementation uses branch `feat/183-admin-ui-rebuild` rather than
   creating a duplicate issue.
2. **Node version**: `ci.yml`, `release.yml` and the Dockerfile pin only Go, no
   Node, so `web/admin/.nvmrc` uses **Node 22 LTS** (as required by the
   fallback rule).
3. **Deleted-model count badge**: the mockup shows the count; the API's
   `deleted[]` makes it free, but if it can be large the section header may
   need a "show more" step.
