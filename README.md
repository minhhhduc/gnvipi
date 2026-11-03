# gnvipi — NVIDIA Playground Gateway

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
![Platforms](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

A self-hosted multi-format LLM gateway that serves **NVIDIA Playground models
(deepseek-v4, kimi, glm, nemotron, gpt-oss, …)** plus any number of
**custom OpenAI-compatible / Anthropic endpoints** behind one local server, with
automatic hCaptcha solving (headless Chrome pool) and a built-in admin console
for model management and per-request statistics.

Embedded [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) does the
inbound format translation; this repo provides the NVIDIA provider executor,
the captcha automation, and the admin surface. **Client API-key authentication
is optional**: set `-gateway-key` or `GNVIPI_GATEWAY_KEY` to protect `/v1/*`,
`/mcp`, and SDK inference aliases. Without a configured key, legacy no-auth
behavior remains. Admin access is open only on loopback listeners; non-loopback
listeners require `-admin-token` or `GNVIPI_ADMIN_TOKEN`. The default `-addr :8080`,
Windows helper, and Docker Compose expose all interfaces. Use a loopback bind
or restrict access with a firewall or authenticated reverse proxy. Placeholder
Claude credentials do not secure the gateway.

Configured provider/gateway/admin credentials and common authentication forms
are redacted from diagnostics, error responses and new statistics records.
Rotated credentials remain protected for in-flight requests. Successful model
output is unchanged; diagnostic bodies/SSE frames are buffered with a 1 MiB
limit before disclosure. Credential storage and existing historical files are
not migrated by this filtering layer.

## Endpoints

| Route | Format |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages (drop-in for Claude Code) |
| `POST /v1/messages/count_tokens` | Anthropic token count (local estimate for NVIDIA models) |
| `GET /v1/models` | catalog (respecting admin on/off mask), plus `automode` and `cycle` |
| `POST /mcp` | Streamable HTTP MCP (`search_web`) |
| `/admin` | admin console (see below) |
| `GET /healthz` | health + captcha pool stats |

## Quick start

Requires **Go 1.26+** and **Chrome/Chromium** for `-auto`; set `CHROME_PATH` to
the browser executable if it is not detected. Claude Code is optional, except
when using `-Claude`. Run commands from the repository root.

```powershell
.\scripts\init.ps1                   # optional: create local settings (skips if present)

# Windows: initializes settings and builds if needed; starts with -auto
.\scripts\run.ps1                   # :8080, 3 chromes, pool of 6
.\scripts\run.ps1 -Port 8081        # test instance
.\scripts\run.ps1 -Claude           # start gateway + open Claude Code pointed at it
.\scripts\run.ps1 -Harness -Batch 6 # low-RAM captcha harness
.\scripts\run.ps1 -Proxy socks5://127.0.0.1:1080
.\scripts\run.ps1 -NoCaptcha        # custom providers / MCP only; no Chrome
```

```bash
# Any platform; local-only listener (embedded catalog needs no initialization)
go run ./cmd/serve -auto -addr 127.0.0.1:8080
go run ./cmd/serve -auto -addr 127.0.0.1:8080 -pool-workers=2 -coalesce-ms=0 -max-inflight=8
go run ./cmd/serve -addr 127.0.0.1:8080 -captcha "P1_..."  # one-shot manual token, no browser
go run ./cmd/serve -addr 127.0.0.1:8080                  # custom providers / MCP only
```

## Client usage

Use the base URL appropriate to your client; update the port if you changed it:

| Client | Base URL |
|---|---|
| OpenAI-compatible SDK | `http://localhost:8080/v1` |
| Claude Code / Anthropic SDK | `http://localhost:8080` (no `/v1` suffix) |
| MCP client | `http://localhost:8080/mcp` |

Clients that require an API key can use `local-gateway` as a placeholder; it
is not checked. Select an enabled model ID from `GET /v1/models`, including
playground IDs (`deepseek-ai/deepseek-v4-flash-0731`, `moonshotai/kimi-k3`, …)
and custom-provider aliases.

After starting the server, check health/catalog and send a streaming request
(example for Bash; replace the model with an enabled ID from your catalog):

```bash
curl -fsS http://localhost:8080/healthz
curl -fsS http://localhost:8080/v1/models
curl -sS -N http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"z-ai/glm-5.2","messages":[{"role":"user","content":"Say hello."}],"max_tokens":128,"stream":true}'
```

Set `"stream":false` for a single JSON response. NVIDIA generation needs
`-auto`, a one-shot `-captcha` token, or a fresh `nv-captcha-token` request
header. No NVIDIA API key is required; custom-provider requests do not use
captcha. NVIDIA `/v1/messages/count_tokens` is a local approximation, not an
exact model-tokenizer or billing count.

With gateway authentication enabled, set your client's `OPENAI_API_KEY` or
`ANTHROPIC_API_KEY` to a gateway key. Bearer `Authorization` and `x-api-key` are
accepted; if both are sent, they must contain the same valid key. Comma-separated
keys support rotation. Client credentials are removed before provider calls;
provider credentials stay separate. `/healthz` remains public. An explicit
`-gateway-key` overrides the environment, including an empty value to disable it.

## Admin console — `/admin`

When an admin token is configured, visit `/admin` to sign in. API clients may
send `Authorization: Bearer <token>` or `X-Admin-Token`. Browser sessions use
HttpOnly, SameSite-strict cookies and expire after 12 hours or a server restart.
Plain-HTTP cookie login is allowed only on loopback; remote browser login needs
a real TLS connection, not an untrusted forwarded scheme header. All mutations
require same-origin browser provenance. Explicit `-admin-token` values override
the environment, including empty values; non-loopback access then stays closed.

One page per tab (`?page=`), each doing exactly one thing:

- **`?page=dashboard`** — LLM request/token statistics **since server start**
  for NVIDIA and custom-provider traffic. KPI tiles (requests, req/min,
  tokens in/out, error %, TTFB, p95 latency over the last 200 requests, uptime),
  per-minute and per-model request charts, and sortable **200-request history
  frames**. Request history survives restarts; aggregate counters and charts
  do not. Logs default to `gnvipi/stats.jsonl` under the OS user cache directory
  (override with `GNVIPI_STATS_FILE`).
  History is size/time rotated and retained within a configurable disk cap;
  frame reads use a sparse in-memory index rather than rescanning all history.
- **`?page=models`** — toggle every model on/off (persisted), delete/restore
  models from the catalog, add/remove custom endpoints (including
  `messages_only` and `responses_only` upstream modes; select only one).
  Delete/restore does not change an independent on/off mask. Historical files
  that already stored a deleted model in `masks_id` keep that explicit mask;
  if it was only deletion-generated, re-enable the model once after restoring.
- **`?page=config`** — select the target for virtual `automode` (Anthropic
  Messages requests only), configure `cycle`, and choose web-search settings.
  `automode` may target an enabled real model or `cycle`; the alias rewrite
  changes only the top-level `model` field before normal provider translation.
  It does not replace Claude's safety prompt, shorten transcripts, or remap
  built-in `claude-` model names. Stats use the resolved real model.
  `?page=automode` remains a legacy alias for this page.
- **`?page=chromes`** — stop/start individual captcha Chrome processes or all
  workers, and inspect busy/warm state and extract counts (`-auto` required).

## Custom providers

Open `/admin?page=models` and enter a provider name, its base URL (normally
ending in `/v1`), upstream model ID, and API key if required. Add one entry per
model. For example, provider `remote` and model `chat-model` advertise the alias
`remote/chat-model`; use the exact ID returned by `/v1/models`.

Choose the API format the upstream actually accepts:

| Mode | Upstream API | Recommended gateway route |
|---|---|---|
| Neither checkbox | OpenAI Chat Completions | `/v1/chat/completions` |
| `messages_only` | Anthropic Messages | `/v1/messages` |
| `responses_only` | OpenAI Responses | `/v1/responses` (native) or `/v1/messages` (adapted) |

Do not select both modes. Use a provider base URL rather than the full
`/v1/messages` endpoint for `messages_only` providers. Reuse a provider name
only for models sharing a base URL and credentials; otherwise choose distinct
names. Tool calling, reasoning, and multimodal support still depend on the
upstream model.

## How it works

```
client ── /v1/* ──▶ gateway
                    ├── CLIProxyAPI translation ──▶ NVIDIA executor ──▶ NVIDIA predict API
                    │                                    ▲
                    │                              captcha token pool
                    └── passthrough / translation ──▶ custom upstream

NVIDIA executor + custom-provider handlers ──▶ GlobalStats (usage + latency)
```

- **Captcha pool**: `-auto` prewarms single-use hCaptcha tokens with headless
  Chrome (`internal/captcha`); expired tokens are discarded (`-pool-ttl`).
  The pool can batch-mint via a local harness page
  (`-captcha-harness -pool-batch=N`) to cut RAM and wall time.
- **SSE coalescing**: consecutive content deltas merge within `-coalesce-ms`
  (default 16) to reduce event-loop churn; first token always flushes
  immediately so TTFT is untouched.
- **Claude Code**: mask changes refresh `/v1/models` and write a best-effort
  Claude model-discovery cache; `.\scripts\run.ps1 -Claude` sets the base URL
  and launches Claude Code.
- **Request identity**: safe inbound `X-Request-Id` values are echoed and forwarded
  to custom upstreams; otherwise a random ID is generated. Upstream response IDs
  cannot replace the canonical client ID. The normalized core exposes an opt-in,
  once-only IR usage recorder with first-byte timing and JSON access logs; legacy
  traffic keeps its existing recorder and is not counted twice.

## Notable flags

```
-auto                     prewarm captcha pool (headless Chrome)
-pool-size/-pool-workers  pool depth / concurrent Chrome processes
-pool-ttl D               discard expired pooled tokens (default 90s)
-warm-timeout D           background pool warmup budget (default 3m)
-captcha-harness          mint on a blank local page (~50MB/chrome, batchable)
-pool-batch N             tokens per browser borrow (harness only)
-coalesce-ms N            SSE delta merge window (0=off)
-max-inflight N           max concurrent NVIDIA streams (0=unlimited)
-captcha-wait D           per-request wait for a pooled token
-inflight-wait D          wait for an NVIDIA stream slot; timeout -> 503
-models-file P            catalog + masks + provider/auto/cycle/search settings
-captcha-select-budget D  benchmark playground URLs, persist champion
-addr HOST:PORT           listen address (default :8080 = all interfaces)
-chrome-proxy URL         proxy for Chrome + NVIDIA API (or CHROME_PROXY)
-max-body-bytes N         client API body limit (default 32 MiB)
-max-admin-body-bytes N   admin body limit (default 1 MiB)
-gateway-key KEYS         comma-separated client keys (or GNVIPI_GATEWAY_KEY)
-admin-token TOKEN        admin access token (or GNVIPI_ADMIN_TOKEN)
-stats-max-bytes N        total retained history bytes (default 16 MiB)
-stats-segment-bytes N    bytes per log segment (default 4 MiB)
-stats-rotate-interval D  active segment age (default 24h)
-stats-retention D        history retention (default 7d)
-stats-read-interval D    dashboard/history cadence (default 1s)
```

With `-auto`, direct Go runs default to a pool of 3 tokens and 1 Chrome worker;
`run.ps1` uses 6 tokens and 3 workers. Run `go run ./cmd/serve -h` for all flags
and defaults.

Body limits apply before routing, including SDK inference aliases. Oversized
requests return protocol-shaped HTTP 413. Client limits bound both encoded bytes
and decoded `zstd` bodies; every layer of a supported encoding stack is bounded.
Both limits must be positive.

Stats limits and intervals must be positive, with segment bytes no greater than
the total cap. Read intervals cannot exceed the browser timer limit
(`2147483647ms`, about 24.8 days). Repeated frame polls are cached; rapid switches
to a different uncached frame return HTTP 429 with `Retry-After`, which the UI
honors. Rotation and retention work even without an open dashboard.

## Data and configuration

| Data | Default location | Override |
|---|---|---|
| Catalog, admin settings, provider API keys | `internal/models/playground_models.json` | `-models-file` |
| Request history | User cache: `gnvipi/stats.jsonl` | `GNVIPI_STATS_FILE` |
| Captcha champion state | User cache: `gnvipi/playground-state.json` | `GNVIPI_CAPTCHA_STATE` |

User cache means `~/.cache` on Linux (or `$XDG_CACHE_HOME`),
`~/Library/Caches` on macOS, and `%LOCALAPPDATA%` on Windows. Overrides are
file paths; the default stats log lives beside the captcha state file.
A missing models file uses the embedded catalog, but its parent directory
must exist and be writable for admin changes to save. Use separate models
files for simultaneously running instances.
Use a distinct `GNVIPI_STATS_FILE` per simultaneous gateway process: each process
owns its active stats file and numeric retained segments.

Provider API keys are saved in **plaintext**. Only the default models-file
path is Git-ignored here; protect custom paths and backups. Stats logs store
request metadata, not prompt bodies. Active and numeric segment files are bounded
to 16 MiB by default, rotate at 4 MiB or 24h, and expire after seven days.
Inference runs on the selected NVIDIA or
custom upstream, and request content is forwarded there—not processed by a
local model inside this gateway.

## Development

```bash
go build ./... && go test ./... -skip '^TestLiveSearchSmoke$'
```

The optional live search smoke test requires a gateway listening on port 8081
with search configured. It makes real backend requests:

```bash
go test ./cmd/serve -run '^TestLiveSearchSmoke$' -count=1 -v
```

## Claude Code setup

Claude Code keeps its local configuration under `.claude/`, which is ignored
by Git in this repository. To point Claude Code at this local Anthropic-
compatible gateway, use this `settings.json` content:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8080",
    "ANTHROPIC_AUTH_TOKEN": "local-gateway",
    "ANTHROPIC_API_KEY": "",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"
  }
}
```

Save that block as `.claude/settings.json` for this project. The
`.\scripts\run.ps1 -Claude` helper sets the base URL/auth token and clears
`ANTHROPIC_API_KEY` for the launched session; it does **not** set the model
discovery flag.

Use `.claude/settings.local.json` for machine-specific overrides and keep
secrets out of shared settings. Both project settings files are Git-ignored
in this repository. `ANTHROPIC_BASE_URL` must be the gateway root URL, not
`/v1/messages` or a URL ending in `/v1`; adjust it when changing the port.

## Web search (MCP)

In `/admin?page=config`, choose **Bing** or **DuckDuckGo** to enable gateway
search. Choices save automatically to `-models-file` (default:
`internal/models/playground_models.json`) and are restored on restart.
Search is disabled until a valid search URL is configured.

General-web searches use Bing and DuckDuckGo. Academic, legal, and identifier
queries can also use arXiv, Crossref, OpenAlex, and Semantic Scholar. Results
are normalized, deduplicated, and ranked, with titles, URLs, and snippets when
available.

**Current limitation:** the Config UI also accepts Google, Google Scholar,
and custom HTTPS URL templates, but the active MCP pipeline does not fetch
those URLs. Only Bing and DuckDuckGo selections currently change the web
search engine.

To make `search_web` available in every Claude Code project on this machine,
register the gateway once:

```bash
claude mcp add --scope user --transport http gateway-search http://localhost:8080/mcp
```

The gateway must be running when Claude Code searches. This is separate from
Claude Code's built-in `WebSearch`; upstreams may block or rate-limit automated
requests, and the tool reports backend errors when no usable results remain.
To prefer gateway search in every Claude Code project, merge these entries
into `~/.claude/settings.json` (preserve existing settings and permission
entries), then restart Claude Code:

```json
{
  "permissions": {
    "allow": ["mcp__gateway-search__search_web"],
    "deny": ["WebSearch"]
  }
}
```

Add this instruction to `~/.claude/CLAUDE.md`:

> Use `mcp__gateway-search__search_web` for web search; it follows the gateway
> Config page. If unavailable, say that the gateway must be running. Do not
> substitute built-in `WebSearch` or treat its zero-search response as a
> gateway search result.

## Model cycling

The **config** tab always exposes the virtual model `cycle`. Check the
models to use and use the arrow buttons to set their order. Changes save
automatically to `-models-file`. `cycle` remains listed with no models selected,
but requests to it return 503 until one is chosen.
Send requests with `cycle`; the gateway keeps it mapped to
one model until that model returns 429, 404, 408, 410, or a 5xx response,
then retries on the next enabled model and keeps that model active. A successful
stream is forwarded immediately, so an error after streaming has begun
cannot be retried safely. The active position resets to the first model
when the gateway restarts or the cycle configuration changes.

## Troubleshooting

`/healthz` checks server liveness, not captcha readiness. Admin, MCP, and
custom-provider routes are available while Chrome starts or is unavailable.
For NVIDIA generation with `-auto`, check `pool.ready` in `/healthz` and
worker state on `/admin?page=chromes`.

| Symptom | What to check |
|---|---|
| Port already in use | Choose another `-Port` / `-addr` and update client URLs. |
| `captcha pool empty` / 503 | Wait for warmup; check Chrome, `CHROME_PATH`, and proxy connectivity. The default `-captcha-wait` is 30s. |
| Invalid or expired captcha / 401 | Supply a fresh manual token, or let `-auto` mint replacements; tokens cannot be reused. |
| `max in-flight upstream streams reached` / 503 | Reduce concurrency or adjust `-max-inflight` and `-inflight-wait` to match available capacity. |
| `cycle` has no available models / 503 | Select at least one enabled model on the Config page. |
| `automode target is hidden` / 404 | Re-enable the target or choose a different enabled model. |
| Admin changes fail to save | Check the `-models-file` parent directory and permissions; container defaults need a writable mount. |

## Project layout

Key packages: `cmd/serve` (gateway + admin), `internal/gnvipi` (playground API
client), `internal/provider/nvidia`
(executor, SSE coalescing/normalizing, `stats.go`), `internal/captcha`
(browser group, pool, harness, champion selection), `internal/models`
(playground catalog). Local build output goes to `bin/`.

## Docker

`Dockerfile` packages the gateway and Chromium; `docker-compose.yml` provides
a basic deployment. Its port mapping publishes `8080` on all interfaces by
default; bind it to `127.0.0.1:8080:8080` for local-only access.

For saved admin settings, mount a writable persistent directory and pass
`-models-file /data/playground_models.json`. The runtime image has no
`internal/models` directory, so its default settings path cannot be saved
without a mount or directory setup.

Example for Bash on Linux/macOS/WSL, with local-only access and persistent
settings/request history:

```bash
docker build -t gnvipi:local .
docker run --rm -p 127.0.0.1:8080:8080 --shm-size=2g \
  --mount type=volume,source=gnvipi-data,target=/data \
  -e GNVIPI_STATS_FILE=/data/stats.jsonl \
  gnvipi:local -auto -models-file /data/playground_models.json
```

The named `gnvipi-data` volume remains when the container is removed. The
image includes Chromium; Docker users do not need Chrome or Go on the host.
