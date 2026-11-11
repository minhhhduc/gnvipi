# Admin Console

The Admin Console is a headless JSON API plus a standalone vanilla SPA (#170,
#171). The server renders nothing: `/admin/api/*` returns JSON and `/admin`
serves three files from `embed.FS` (`web/admin/index.html`, `app.css`,
`app.js`). No framework, no build step, no external requests — the SPA works on
a machine with no internet access.

## Layout

| Path | Role |
|---|---|
| `web/admin/index.html` | Shell: rail nav, header slot, `<main id="view">`, toast. Loads `/admin/app.css` and `/admin/app.js` as a module. |
| `web/admin/app.css` | The full stylesheet (design system below). |
| `web/admin/app.js` | SPA: API helper, `esc()`, route on `?page=`, one view per console page. |
| `web/admin/embed.go` | `//go:embed index.html app.css app.js` → `adminui.FS`. |
| `cmd/serve/admin_ui.go` | `registerAdminUI` serves the embedded files with `http.ServeFileFS`, one asset per request, nothing held in the heap. |
| `cmd/serve/admin_api.go` | `registerAdminAPIRoutes` registers the JSON endpoints. |
| `cmd/serve/admin_access.go` | `adminAccess` middleware: loopback/authority, token, session cookie, CSRF, `no-store`. |
| `cmd/serve/admin_handlers.go` etc. | The pre-existing handlers the API wraps. |

`registerAdminUI` maps `/admin`, `/admin/`, `/admin/app.css` and
`/admin/app.js` to the files. Unknown paths under `/admin` are 404s.

## SPA routing

The SPA reads `?page=` and renders into `#view`; links are plain anchors, so
reload and back/forward both work.

| `?page=` | View | API calls |
|---|---|---|
| `dashboard` (default) | Stats: KPI tiles, traffic charts, request log | `GET /admin/api/stats` |
| `models` | Providers editor + model catalog, masks, delete/restore | `/admin/api/providers*`, `/admin/api/models*` |
| `config` | Auto model, web search, model cycle | `/admin/api/config/*` |
| `chromes` | Captcha Chrome worker pool | `/admin/api/chromes` |
| `automode` | alias for `config` | |
| anything else | falls back to `models` | |

The old server-rendered pages and the legacy endpoints (`/admin/models`,
`/admin/providers`, `/admin/stats`, `/admin/cycle`, `/admin/automode`,
`/admin/search`, `/admin/chromes`, `/admin/delete`) still exist for
compatibility; the SPA does not use them.

## JSON API

All routes live under `/admin/api` behind `adminAccess`. Request bodies are
capped by `-max-admin-body-bytes`. Errors are `{"error":"..."}` with 4xx/5xx.

### Providers

| Method | Path | Body → Response |
|---|---|---|
| GET | `/admin/api/providers` | → bare array of providers (secrets redacted) |
| POST | `/admin/api/providers` | provider → 201, redacted provider |
| GET | `/admin/api/providers/:id` | → redacted provider, 404 if unknown |
| PUT | `/admin/api/providers/:id` | provider → redacted provider |
| DELETE | `/admin/api/providers/:id` | → `{"ok":true,"deleted":"<id>"}` |
| POST | `/admin/api/providers/test` | unsaved provider → result |
| POST | `/admin/api/providers/:id/test` | → result |

A test result is `{"ok":bool,"status":"...","status_code":N,"latency_ms":N,`
`"message":"...","error":"..."}` (fields depend on outcome). Provider ids are
single path segments; only model ids may contain `/`.

Redaction: `auth.key` is never returned. A `GET`/`PUT` that omits the key
preserves the stored one, so the SPA can round-trip a provider without
re-entering the secret.

### Stats

`GET /admin/api/stats` → `{models, series, series_by_model, events,
started_at, total_frames}`. `?frames=N` instead returns one frame:
`{frame, total_frames, events}`. When no frame is ready the endpoint answers
429 with `Retry-After`; the SPA waits and retries.

### Models

| Method | Path | Body → Response |
|---|---|---|
| GET | `/admin/api/models` | → `{models:[{id,name,publisher,hidden,deleted,custom,base_url?}], off, deleted}` |
| POST | `/admin/api/models` | `{"masks_id":[...]}` → `{off}` (`off` = models now masked) |
| DELETE | `/admin/api/models/*id` | → `{"deleted":"<id>"}` |
| POST | `/admin/api/models/*id` | `retry-failed` → `{ok}`; `<id>/restore` → `{"restored":"<id>"}` |

Model ids are `publisher/model`, so the id is a **catch-all** (`*id`), not a
path parameter — Gin does not decode `%2F`. The SPA encodes each segment and
leaves the slashes literal:

```js
const modelPath = id => String(id).split('/').map(encodeURIComponent).join('/');
```

### Config

| Method | Path | Body → Response |
|---|---|---|
| GET | `/admin/api/config/automode` | → `{model, chat_responses}` |
| POST | `/admin/api/config/automode` | `{"model":...,"chat_responses":bool}` → `{model}` |
| GET | `/admin/api/config/cycle` | → `{name:"cycle", models, available, current}` |
| POST | `/admin/api/config/cycle` | `{"models":[...]}` → `{name, models}` |
| GET | `/admin/api/config/search` | → `{url}` |
| POST | `/admin/api/config/search` | `{"url":...}` → `{url}` |

### Chromes

| Method | Path | Body → Response |
|---|---|---|
| GET | `/admin/api/chromes` | → `{enabled, state, chromes:[{index,pid,killed,busy,warmed,extracts,last_ok}], ready}` |
| POST | `/admin/api/chromes` | `{"index":N,"mode":"start"\|"kill"\|"stop"\|"resume"\|"pause","all":bool}` → status |

An unknown mode is 400, an unknown worker 404. When the lifecycle is
unavailable (headless build), GET reports `enabled:false` and POST is 503. The
SPA polls GET every 5s.

## Design system

The stylesheet defines a small vocabulary; composition is by nesting, not by
per-page classes. It is unchanged from #166.

| Class | Role |
|---|---|
| `.sheet` | A bordered panel. Its direct children are separated by hairlines instead of each being its own card. |
| `.sheet-head` / `.sheet-title` / `.sheet-body` / `.sheet-foot` | Panel header (title, count, actions), padding wrapper, and the action bar pinned to the bottom. A `<details class=sheet>` uses its `<summary class=sheet-head>` as the toggle. |
| `.grp` + `.cnt` | Publisher divider inside the model catalog, with its row count. |
| `.cols` | Auto-fit grid (`minmax(19rem, 1fr)`) for placing whole sheets side by side. |
| `.split` / `.pane` | Named two-pane grid (list pane + work pane), collapses to one column under 1100px. |
| `.sec` | Vertical stack with no border, for content that is not a panel. |
| `.row` / `.static` | One line of a list. `.static` rows are not click-to-toggle. |
| `.chip` | Monospace identifier (`provider id`, `#0`). |
| `.pos` | Leading position number. Used only in the model cycle, where the order is the failover sequence. |
| `button.primary` | The single commit action of a sheet. |
| `.tiles` / `.tile` | KPI row at the top of a panel. |

The console is a light workspace: white sheets (`--panel`) on a cool paper
plane (`--bg`), ink text (`--fg`), one blue for action (`--accent`) and three
state colors (`--on`, `--bad`, `--warn`). Every color, radius and shadow is a
custom property on `:root`. Under 900px the rail collapses to a top strip.

## Behaviour worth knowing

- **Every toggle saves.** A model switch, the auto-model select, the search
  provider and the cycle checkboxes all POST on change; there is no Save button
  for them. The SPA re-fetches only where the response changes the list.
- **The provider editor is collapsed** and opens on Add/edit.
- **Publisher dividers follow the filter**; a group with no visible rows is
  hidden.
- **Chromes poll** every 5s and reflect pool state in the header badge.
- **No legacy fallback.** The SPA talks only to `/admin/api/*`.

## Security invariants

- `adminAccess` guards the JSON API **and** the SPA assets identically: a
  token-protected console returns 401 for `/admin/app.js` too.
- Non-loopback binds require `-admin-token`. Loopback without a token is open,
  as before.
- Mutating requests must be same-origin (`Origin`/`Referer`/`Sec-Fetch-Site`);
  a cross-origin POST is 403. Responses are `no-store`.
- Nothing is loaded from a third-party origin — no CDN, no font service, no
  analytics. `TestAdminPages_*` and the SPA tests pin this.
- `-disable-admin-ui` removes the whole surface (assets, API, legacy routes,
  `adminAccess`); `/v1/*` and `/mcp` keep working. See
  [operations.md](../operations.md).
- `cmd/serve/admin_contract_test.go` is the contract gate: method/path/status
  and response keys for every endpoint, slash-id handling, 404 on unknown API
  paths, token protection of API and assets, and same-origin enforcement.
