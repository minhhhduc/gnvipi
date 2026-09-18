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
the captcha automation, and the admin surface. Inbound gateway API keys are
**not** enabled — keep it on localhost.

## Endpoints

| Route | Format |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages (drop-in for Claude Code) |
| `GET /v1/models` | catalog (respecting admin on/off mask) |
| `/admin` | admin console (see below) |
| `GET /healthz` | health + captcha pool stats |

## Quick start

```powershell
.\scripts\init.ps1             # one-time: create local model catalog (skips if present)

# Windows (recommended): builds if needed, checks the port, starts with -auto
.\scripts\run.ps1                      # :8080, 3 chromes, pool of 6
.\scripts\run.ps1 -Port 8081          # test instance
.\scripts\run.ps1 -Claude             # start gateway + open Claude Code pointed at it
.\scripts\run.ps1 -Harness -Batch 6   # low-RAM captcha harness
.\scripts\run.ps1 -Proxy socks5://127.0.0.1:1080
```

```bash
# Any platform
go run ./cmd/serve -auto -addr :8080
go run ./cmd/serve -auto -pool-workers=2 -coalesce-ms=0 -max-inflight=8
go run ./cmd/serve -captcha "P1_..."   # one-shot manual token, no browser
```

Point any client at `http://localhost:8080`. Model ids come from the playground
catalog (`deepseek-ai/deepseek-v4-flash-0731`, `moonshotai/kimi-k3`, …) plus
`<provider>/<model>` aliases you add in the admin page.

## Admin console — `/admin`

One page per tab (`?page=`), each doing exactly one thing:

- **`?page=dashboard`** — token/request statistics **since server start**, for
  every API route: playground models (executor) *and* custom providers
  (passthrough middleware). KPI tiles (requests, req/min, tokens in/out,
  error %, TTFB, p95 latency, uptime), per-minute charts with crosshair
  tooltips, per-model table (only models with traffic), and a scrollable
  **log of the last 200 individual requests** (time · model · provider ·
  stream · tokens · latency · TTFB · ok/error), sortable by column.
- **`?page=models`** — toggle every model on/off (persisted), delete/restore
  models from the catalog, add/remove custom OpenAI-compatible endpoints
  (including `messages_only` mode for WAF-blocked upstreams).
- **`?page=chromes`** — live captcha Chrome workers: pause/resume each process,
  see busy/warm state and extract counts.

## How it works

```
client ──/v1/*──▶ CLIProxyAPI ──▶ nvidia executor ──▶ playground predict API
                    │                    │                    ▲
                    │                    └── scrapeUsage ──────┤ SSE usage
                    ▼                                         │
              passthrough (custom providers) ──────────────────┤
              captcha pool (headless Chrome + hCaptcha) ───────┘ nv-captcha-token
              GlobalStats.Observe ◀── every call on every path
```

- **Captcha pool**: `-auto` prewarms hCaptcha tokens with headless Chrome
  (`internal/captcha`); requests take a token from the pool, stale tokens are
  recycled (`-pool-ttl`), the pool can batch-mint via a local harness page
  (`-captcha-harness -pool-batch=N`) to cut RAM and wall time.
- **SSE coalescing**: consecutive content deltas merge within `-coalesce-ms`
  (default 16) to reduce event-loop churn; first token always flushes
  immediately so TTFT is untouched.
- **Claude Code**: `/admin` mask changes rewrite the gateway model cache so
  `/model` reflects the catalog without re-login; `-claude` flag or
  `.\scripts\run.ps1 -Claude` wires `ANTHROPIC_BASE_URL` for you.

## Notable flags

```
-auto                     prewarm captcha pool (headless Chrome)
-pool-size/-pool-workers  pool depth / concurrent Chrome processes
-captcha-harness          mint on a blank local page (~50MB/chrome, batchable)
-pool-batch N             tokens per browser borrow (harness only)
-coalesce-ms N            SSE delta merge window (0=off)
-max-inflight N -inflight-wait  upstream stream limiter -> 503 when saturated
-captcha-wait D           per-request wait for a pooled token
-inflight-wait D          wait for a free stream slot
-models-file P            catalog + mask + providers persistence
-captcha-select-budget D  benchmark playground URLs, persist champion
-addr :8080  -chrome-proxy URL
```

## Development

```bash
go build ./... && go test ./...
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
    "ANTHROPIC_API_KEY": "local-gateway",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"
  }
}
```

Save that block as `.claude/settings.json` if you want Claude Code to use it.
If you run `.\scripts\run.ps1 -Claude`, the script sets these variables
automatically for the launched Claude Code session.

- `.claude/settings.json` — shared Claude Code environment for this repo.
- Keep secrets and machine-specific settings out of this file.
- `.claude/settings.local.json` — optional local-only settings; do not commit
  personal or sensitive values.
- `README.md` — project context, setup instructions, and development guidance
  for both developers and Claude Code.

For a local Claude Code session, start the gateway and configure the endpoint
with the existing helper:

```powershell
.\scripts\run.ps1 -Claude
```

This starts the gateway and opens Claude Code with `ANTHROPIC_BASE_URL` pointed
at the local Anthropic-compatible `/v1/messages` endpoint. If you start the
gateway yourself, use `http://localhost:8080` as the base URL.

Key packages: `cmd/serve` (gateway + admin), `internal/gnvipi` (playground API
client), `internal/provider/nvidia`
(executor, SSE coalescing/normalizing, `stats.go`), `internal/captcha`
(browser group, pool, harness, champion selection), `internal/models`
(playground catalog). Local build output goes to `bin/`.

Docker deployment for the captcha browser is in `Dockerfile` /
`docker-compose.yml`.
