# Operations

Running the gateway: flags, storage, the Admin Console, virtual models, and what
to check when a request fails. Configuration of upstreams lives in
[providers.md](providers.md); client-side setup lives in [clients.md](clients.md).

## Run flags

| Flag | Environment Variable | Default | Purpose |
|---|---|---|---|
| `-addr` | — | `127.0.0.1:8080` | Listen host and port. Loopback by default; use `:8080` to expose all network interfaces. |
| `-gateway-key` | `GNVIPI_GATEWAY_KEY` | *(disabled)* | Optional comma-separated API keys protecting `/v1/*`, `/mcp`, and inference routes. |
| `-admin-token` | `GNVIPI_ADMIN_TOKEN` | *(disabled)* | Admin authentication token. Required for `/admin` on non-loopback binds; the console stays open on loopback without it. |
| `-disable-admin-ui` | — | `false` | Remove the admin UI and admin API entirely (headless proxy mode); `/v1/*` and `/mcp` are unaffected. |
| `-gateway-v2` | — | `true` | Gateway core dispatcher routing all custom providers (default on; false is deprecated). |
| `-models-file` | — | `internal/models/playground_models.json` | Path to saved provider configuration, masks, and model preferences. |
| `-max-body-bytes` | — | `33554432` (32 MiB) | Maximum request body bytes for `/v1/*` routes (enforces `http.MaxBytesReader`). |
| `-max-admin-body-bytes` | — | `1048576` (1 MiB) | Maximum request body bytes for administrative endpoints. |
| `-deny-private-upstreams` | — | `false` | Deny loopback and RFC 1918 private LAN addresses for custom upstreams (SSRF protection). |
| `-allow-unsafe-upstreams` | — | `false` | Permit link-local and cloud-metadata addresses (`169.254.169.254`) — **unsafe**. |
| `-pool-size` / `-pool-workers` | — | `3` / `1` (`6` / `3` in `run.ps1`) | Token pool depth / headless Chrome processes allocated by Start All. |
| `-pool-ttl` | — | `90s` | Time-to-live before discarding expired pooled captcha tokens. |
| `-pool-batch` | — | `1` | Tokens minted per browser borrow (`-captcha-harness` only). |
| `-warm-timeout` | — | `3m` | Background pool warmup budget after Start All. |
| `-captcha` | — | — | One-shot manual captcha token (runs without browser). |
| `-captcha-harness` | — | `false` | Mint captcha tokens on a blank local page (~50MB RAM per Chrome, batchable). |
| `-captcha-wait` | — | `30s` | Per-request wait timeout for a pooled captcha token. |
| `-captcha-select-budget` | — | `15s` | Benchmark playground URLs on Start All and persist champion endpoint. |
| `-chrome-proxy` | `CHROME_PROXY` | — | Proxy URL for Chrome and NVIDIA Playground API. |
| `-coalesce-ms` | — | `16` | SSE delta merge window in milliseconds (0 = disabled). First token flushes immediately. |
| `-max-inflight` | — | `0` (unlimited) | Maximum concurrent NVIDIA upstream streams (0 = unlimited). |
| `-inflight-wait` | — | `30s` | Maximum wait time for an NVIDIA stream slot before returning HTTP 503. |
| `-stats-max-bytes` | — | `16777216` (16 MiB) | Total retained statistics history size in bytes. |
| `-stats-segment-bytes` | — | `4194304` (4 MiB) | Maximum bytes per log segment before rotating. |
| `-stats-rotate-interval` | — | `24h` | Maximum active segment age before rotating. |
| `-stats-retention` | — | `7d` | Retention duration for historical request segments. |
| `-stats-read-interval` | — | `1s` | Dashboard / history polling cadence. |

Body limits apply before routing, including SDK inference aliases. Oversized
requests return protocol-shaped HTTP 413. Client limits bound both encoded bytes
and decoded `zstd` bodies; every layer of a supported encoding stack is bounded.
Both limits must be positive.

Stats limits and intervals must be positive, with segment bytes no greater than
the total cap. Read intervals cannot exceed the browser timer limit
(`2147483647ms`, ~24.8 days). Repeated frame polls are cached; rapid switches to a
different uncached frame return HTTP 429 with `Retry-After`, which the UI honors.
Rotation and retention work even without an open dashboard.

## Admin Console — `/admin`

When an admin token is configured, visit `/admin` to sign in. API clients may
send `Authorization: Bearer <token>` or `X-Admin-Token`. Browser sessions use
HttpOnly, SameSite-strict cookies and expire after 12 hours or a server restart.
Plain-HTTP cookie login is allowed only on loopback; remote browser login requires
a real TLS connection, not an untrusted forwarded scheme header. All mutations
require same-origin browser provenance. Explicit `-admin-token` values override
the environment, including empty values; non-loopback access then stays closed.

- **`?page=dashboard`:** Real-time request and token throughput KPIs, TTFT, p95 latency over the last 200 requests, error rates, and 200-request rolling history frames surviving restarts. Aggregate counters and charts reset on server restart. Logs default to `gnvipi/stats.jsonl` under the OS user cache directory (override with `GNVIPI_STATS_FILE`).
- **`?page=models`:** Toggle models on/off (persisted), delete/restore catalog entries, and configure v2 custom providers with live validation.
- **`?page=config`:** Configure virtual `automode` (Claude auto-model alias targeting an enabled real model or `cycle`), `cycle` (ordered round-robin with automatic failover), and web search MCP settings.
- **`?page=chromes`:** Manage headless Chrome worker pools on demand (Turn On / Start All, Kill All). Startup is always idle and never allocates Chrome or token pools until activated.

## Virtual models

`automode` is a Claude-side alias: requests to it are forwarded, unchanged, to the
model selected on the config page.

The **config** tab (`/admin?page=config`) always exposes the virtual model
`cycle`. Check the models to include and use the arrow controls to define their
order. Changes save automatically to `-models-file`. `cycle` remains visible even
when empty, returning HTTP 503 until at least one model is selected.

When client requests target `cycle`:

- The gateway routes to the first configured model in the sequence.
- If that model returns **429, 404, 408, 410, or a 5xx status code**, the gateway automatically retries on the next enabled model in the sequence and keeps that model active.
- A successful stream is forwarded immediately; once streaming bytes have been committed to the client, downstream errors cannot be retried safely.
- The active cycle position resets to the first model when the gateway restarts or when the cycle configuration changes.

## Web Search (MCP)

The gateway includes a Streamable HTTP Model Context Protocol (`POST /mcp`)
endpoint supporting web search:

1. In `/admin?page=config`, select **DuckDuckGo** or **Bing**.
2. Add the tool to Claude Code globally:
   ```bash
   claude mcp add --scope user --transport http gateway-search http://localhost:8080/mcp
   ```

## Data and Configuration

| Data | Default Location | Override |
|---|---|---|
| Catalog, admin settings, provider API keys | `internal/models/playground_models.json` | `-models-file` |
| Request history | User cache: `gnvipi/stats.jsonl` | `GNVIPI_STATS_FILE` |
| Captcha champion state | User cache: `gnvipi/playground-state.json` | `GNVIPI_CAPTCHA_STATE` |

User cache resolves to:

- Linux: `~/.cache` (or `$XDG_CACHE_HOME`)
- macOS: `~/Library/Caches`
- Windows: `%LOCALAPPDATA%`

Overrides are file paths; the default stats log lives beside the captcha state
file. A missing models file uses the embedded catalog, but its parent directory
must exist and be writable for admin changes to save. Use separate models files
for simultaneously running instances.

> [!IMPORTANT]
> **One stats file per process:** Use a distinct `GNVIPI_STATS_FILE` per simultaneous gateway process. Each process owns its active stats file and numeric retained segments.

Stats logs store request metadata, not prompt bodies. Active and numeric segment
files are bounded to 16 MiB by default, rotate at 4 MiB or 24h, and expire after 7
days. Inference runs on the selected upstream provider or NVIDIA Playground, and
request content is forwarded there—not processed locally inside the gateway.

## Docker

The container always listens on `:8080` internally, because a loopback bind would
be unreachable from outside it. Exposure is controlled by port publishing instead:
`-p 127.0.0.1:8080:8080` keeps the gateway local to the host,
`docker-compose.yml` ships the same loopback form, and `-p 8080:8080` publishes on
every host interface. That last form is a non-loopback listener from the gateway's
point of view, so pass `-admin-token` (`-e GNVIPI_ADMIN_TOKEN=...`) to keep
`/admin` reachable.

## Troubleshooting

`/healthz` checks server liveness, not captcha readiness. Admin, MCP, and
custom-provider routes remain available while Chrome starts or when captcha is
disabled. For NVIDIA generation, enable Chrome captcha from Admin
(`/admin?page=chromes`), then check `pool.ready` in `/healthz` and worker state on
`/admin?page=chromes`.

### Configuration snapshot

With `GNVIPI_DATABASE_URL` set, the gateway loads providers, keys and aliases once
at startup and serves every request from that in-memory snapshot; a database that
cannot produce a valid first snapshot aborts startup instead of serving an empty
configuration. Later writes are picked up through `LISTEN gnvipi_config` plus a
30-second version poll, so a missed notification is repaired within one interval.

When the database becomes unreachable the process keeps serving the last snapshot
that fully validated and reports it:

| Field | Meaning |
|---|---|
| `db_ok` | The database answered during this probe. |
| `degraded` | Serving from the last known good snapshot: database down or the last reload failed. |
| `status` | `ok` or `degraded`. `ok` in the body is `false` when degraded, but the HTTP status stays 200 because requests are still served. |
| `snapshot_version` | Version of the snapshot currently in service. |
| `config_version` | Last `config_state.version` observed, ahead of the snapshot while a reload is pending or failing. |
| `snapshot_age` | Seconds since that snapshot was published. |
| `detail` | Error behind a degraded state, when known. |

Without `GNVIPI_DATABASE_URL` these fields are absent and `/healthz` keeps its
file-mode shape.

| Symptom | What to check |
|---|---|
| Port already in use | Choose another `-Port` / `-addr` and update client URLs. |
| `captcha token required` / 503 | Click **Turn On / Start All** in Admin, or provide a manual token. Chrome is always off on server startup. |
| `captcha pool empty` / 503 | Wait for warmup after Start All; check Chrome, `CHROME_PATH`, and proxy connectivity. The default `-captcha-wait` is 30s. |
| Invalid or expired captcha / 401 | Supply a fresh manual token, or let the enabled pool mint replacements; tokens cannot be reused. |
| `max in-flight upstream streams reached` / 503 | Reduce concurrency or adjust `-max-inflight` and `-inflight-wait` to match available capacity. |
| `cycle` has no available models / 503 | Select at least one enabled model on the Config page (`/admin?page=config`). |
| `automode target is hidden` / 404 | Re-enable the target model or choose a different enabled model. |
| Admin changes fail to save | Check the `-models-file` parent directory and write permissions; container deployments require a writable mount. |
