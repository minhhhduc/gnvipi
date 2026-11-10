# gnvipi — Universal LLM API Gateway & NVIDIA Playground Reverse-Proxy

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
![Platforms](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

A unified local gateway for AI coding agents (**Claude Code**, **OpenAI SDKs**, **Codex**, **OpenCode**, and **cURL**) bridging 3 inbound client protocols (**OpenAI Chat**, **Anthropic Messages**, **OpenAI Responses**) to any upstream provider (**OpenAI-compatible**, **Anthropic-compatible**, **OpenAI Responses**, **NVIDIA Playground**, and custom endpoints).

The gateway combines a high-performance **Adapter-Hub** translation engine and zero-copy passthrough relay with an automated headless Chrome captcha pool for NVIDIA Playground models (DeepSeek-V4, Kimi, GLM, Nemotron, GPT-OSS, and more), protected by dial-time SSRF defenses, inbound authentication, and a built-in Admin Console v2.

**Client API-key authentication is optional**: set `-gateway-key` or `GNVIPI_GATEWAY_KEY` to protect `/v1/*`, `/mcp`, and SDK inference aliases. Without a configured key, legacy no-auth behavior remains. Admin access is open only on loopback listeners; non-loopback listeners require `-admin-token` or `GNVIPI_ADMIN_TOKEN`. The default `-addr :8080`, Windows helper, and Docker Compose expose all interfaces. Use a loopback bind (`-addr 127.0.0.1:8080`) or restrict access with a firewall or authenticated reverse proxy. Placeholder client credentials do not secure the gateway.

Configured provider/gateway/admin credentials and common authentication forms are redacted from diagnostics, error responses, and new statistics records. Rotated credentials remain protected for in-flight requests. Successful model output is unchanged; diagnostic bodies/SSE frames are buffered with a 1 MiB limit before disclosure.

---

## Documentation Links

- **[Release Notes](RELEASE_NOTES.md)** — Universal LLM Gateway v2.0 feature highlights, deprecations, and upgrade guide.
- **[Provider Configuration Guide](docs/providers.md)** — Schema v2 reference, model mappings, wildcard routing, fallback chains, and migration (#61).
- **[Client Setup & Compatibility Matrix Guide](docs/clients.md)** — Step-by-step setup for Claude Code, OpenAI SDKs, Codex, OpenCode, and the 9-combination compatibility matrix (#62).
- **[Architecture Overview](docs/architecture/overview.md)** — Hub-and-Spoke pattern, normalized IR, component layout, and architectural invariants.
- **[Adapter Developer Guide](docs/architecture/adapters.md)** — How to implement, register, and test new inbound/outbound protocol adapters (#63).
- **[Master Implementation Roadmap](docs/architecture/universal-llm-gateway-roadmap.md)** — Complete 10-phase milestone breakdown and issue index (#65).

---

## Endpoints

| Route | Format / Functionality | Description |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | Standard chat endpoint for OpenAI SDKs, OpenCode, and HTTP clients |
| `POST /v1/messages` | Anthropic Messages | Drop-in Anthropic endpoint for Claude Code and Anthropic clients |
| `POST /v1/responses` | OpenAI Responses | OpenAI Responses protocol endpoint for Codex and next-gen tools |
| `POST /v1/messages/count_tokens` | Anthropic Token Counting | Accurate token counting across custom providers and local estimation |
| `GET /v1/models` | Unified Model Catalog | Dynamic catalog listing across all custom providers and NVIDIA Playground |
| `POST /mcp` | HTTP Streamable MCP | Model Context Protocol search tool (`search_web`) |
| `/admin` | Admin Console v2 | Web console for providers, model masks, Chrome workers, and metrics |
| `GET /healthz` | Health Check | Gateway liveness and captcha pool status |

---

## Quick Start

### 1. Custom Providers (Zero-Browser Mode — No Chrome, No Captcha Needed)

If you only use custom upstreams (such as OpenAI, Anthropic, OpenRouter, DeepSeek, or local Ollama / vLLM engines), the gateway runs as a pure lightweight Go proxy without allocating any browser processes:

```bash
# Any platform: listen locally on port 8080
go run ./cmd/serve -addr 127.0.0.1:8080
```

On Windows:
```powershell
.\scripts\run.ps1 -NoCaptcha
```

Add your providers via the Admin Console at `http://localhost:8080/admin` or directly in `-models-file` (default: `internal/models/playground_models.json`).

### 2. NVIDIA Playground (With Headless Chrome Captcha Solvers)

To serve free models from the NVIDIA Playground (e.g. `deepseek-ai/deepseek-v4-flash-0731`, `moonshotai/kimi-k3`):

1. Start the gateway:
   ```bash
   go run ./cmd/serve -addr 127.0.0.1:8080
   ```
2. Open `http://localhost:8080/admin?page=chromes` and click **Turn On / Start All**.  
   *Note: Startup is always idle and never launches Chrome automatically until activated.*
3. On Windows, you can also use:
   ```powershell
   .\scripts\run.ps1                    # Starts gateway with Chrome pool ready for activation
   .\scripts\run.ps1 -Claude            # Starts gateway and launches Claude Code pointed at it
   ```

---

## Client Configuration

The gateway exposes a unified interface. Point your client to the appropriate base URL:

| Client Application / SDK | Base URL | Auth Token / Key |
|---|---|---|
| **Claude Code** | `http://localhost:8080` (no `/v1` suffix) | `local-gateway` (or configured `-gateway-key`) |
| **OpenAI Python SDK** | `http://localhost:8080/v1` | `local-gateway` (or configured `-gateway-key`) |
| **OpenAI Node / TypeScript SDK** | `http://localhost:8080/v1` | `local-gateway` (or configured `-gateway-key`) |
| **Codex / OpenAI Responses** | `http://localhost:8080/v1` | `local-gateway` (or configured `-gateway-key`) |
| **OpenCode** | `http://localhost:8080/v1` (OpenAI) or `http://localhost:8080` (Anthropic) | `local-gateway` (or configured `-gateway-key`) |
| **cURL / HTTP Clients** | `http://localhost:8080` | `Bearer local-gateway` |

### Configuration Examples

#### Claude Code
Set the environment variables or place this in `.claude/settings.json`:
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

#### Official OpenAI Python SDK
```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="local-gateway",  # or your GNVIPI_GATEWAY_KEY
)

response = client.chat.completions.create(
    model="openrouter/anthropic/claude-3.7-sonnet",
    messages=[{"role": "user", "content": "Explain quantum computing in one sentence."}],
)
print(response.choices[0].message.content)
```

#### OpenAI Node.js / TypeScript SDK
```typescript
import OpenAI from "openai";

const openai = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "local-gateway",
});

const stream = await openai.chat.completions.create({
  model: "z-ai/glm-5.2",
  messages: [{ role: "user", content: "Write a poem about Go." }],
  stream: true,
});

for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content || "");
}
```

#### Codex CLI
Configure Codex to target the gateway Responses endpoint:
```bash
export OPENAI_BASE_URL="http://localhost:8080/v1"
export OPENAI_API_KEY="local-gateway"
```

#### cURL
```bash
curl -sS -N http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer local-gateway" \
  -d '{
    "model": "z-ai/glm-5.2",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

---

## Provider Configuration (Schema v2)

Providers are declared using the typed v2 schema in the configured models file (`-models-file`, default `internal/models/playground_models.json`), or saved from the Admin UI at `/admin?page=models`.

### Adding Providers via Admin UI
1. Navigate to `http://localhost:8080/admin?page=models`.
2. Under **Custom Providers**, provide:
   - **Provider Name / ID:** Identifier for namespacing (e.g. `openrouter`, `local-ollama`).
   - **Base URL:** Upstream endpoint root (e.g. `https://openrouter.ai/api/v1` or `http://127.0.0.1:11434/v1`).
   - **Wire Protocol:** Choose `openai-chat`, `anthropic-messages`, or `openai-responses`.
   - **Models & Wildcards:** Map aliases to physical upstream model IDs, or use wildcard patterns (e.g. `openrouter/*`).
   - **API Key:** Upstream credentials (stored securely and segregated from public catalogs).
3. Click **Save & Test Connection**.

### Adding Providers via Configuration File
You can define providers directly in your configured `-models-file` (default `internal/models/playground_models.json`):

```json
{
  "schema_version": 2,
  "providers_v2": [
    {
      "id": "openrouter",
      "name": "OpenRouter",
      "base_url": "https://openrouter.ai/api/v1",
      "protocol": "openai-chat",
      "enabled": true,
      "auth": {
        "scheme": "bearer",
        "key_env": "OPENROUTER_API_KEY"
      },
      "models": [
        {
          "alias": "openrouter/*",
          "upstream_model": "*"
        }
      ],
      "max_concurrency": 8,
      "queue_timeout": "5s"
    },
    {
      "id": "ollama",
      "name": "Local Ollama",
      "base_url": "http://127.0.0.1:11434/v1",
      "protocol": "openai-chat",
      "enabled": true,
      "auth": {
        "scheme": "none"
      },
      "models": [
        {
          "alias": "ollama/llama3",
          "upstream_model": "llama3:latest"
        }
      ]
    }
  ]
}
```

See [docs/providers.md](docs/providers.md) for the complete schema specification, fallback chains, and header policies.

---

## Security & Network Binding

> [!WARNING]
> **Network Exposure & Inbound Authentication:**  
> The default `-addr` is `:8080`, exposing the server on **all network interfaces**. By default, `-gateway-key` is not configured, meaning `/v1/*` and `/mcp` inference routes are **unauthenticated** unless `-gateway-key` or `GNVIPI_GATEWAY_KEY` is set. Placeholder credentials (such as `local-gateway`) do not secure the gateway when authentication is disabled.  
> 
> **Recommended setup:** Always use a loopback bind (`-addr 127.0.0.1:8080`) for local workloads, or protect the port with a firewall or authenticated reverse proxy and configure `-gateway-key`. The `/admin` console is restricted to loopback listeners by default; non-loopback listeners require `-admin-token` or `GNVIPI_ADMIN_TOKEN`.

### Inbound Authentication
When `-gateway-key` or `GNVIPI_GATEWAY_KEY` is set, client requests must present a valid key via `Authorization: Bearer <key>` or `x-api-key: <key>`. Comma-separated keys permit zero-downtime key rotation. Client credentials are removed before provider calls; provider credentials remain separate. `/healthz` remains public. An explicit `-gateway-key` overrides the environment, including an empty value to disable it.

### Credential Redaction
Configured provider, gateway, and admin credentials, along with standard authentication tokens, are scrubbed from diagnostic dumps, error responses, and new statistics records (`internal/gateway/redact`). Rotated credentials remain protected for in-flight requests. Diagnostic bodies and SSE frames are buffered with a 1 MiB limit before disclosure. Credential storage and existing historical files are not migrated by this filtering layer.

### Dial-Time SSRF Prevention
By default, the gateway strictly blocks all cloud metadata services (`169.254.169.254`) and IPv6 link-local addresses at both configuration save time and connection dial time. Custom upstream URLs require HTTP(S), a host, and no embedded user credentials. Loopback and RFC 1918 private LAN services are permitted by default; pass `-deny-private-upstreams` in hardened multi-tenant environments to restrict all private LAN targets. Passing `-allow-unsafe-upstreams` permits link-local and cloud-metadata addresses (**unsafe**).

---

## Operational Flags

| Flag | Environment Variable | Default | Purpose |
|---|---|---|---|
| `-addr` | — | `:8080` | Listen host and port. Exposes all network interfaces by default; use `127.0.0.1:8080` for local-only binding. |
| `-gateway-key` | `GNVIPI_GATEWAY_KEY` | *(disabled)* | Optional comma-separated API keys protecting `/v1/*`, `/mcp`, and inference routes. |
| `-admin-token` | `GNVIPI_ADMIN_TOKEN` | *(disabled)* | Authentication token required for non-loopback `/admin` access. |
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

Body limits apply before routing, including SDK inference aliases. Oversized requests return protocol-shaped HTTP 413. Client limits bound both encoded bytes and decoded `zstd` bodies; every layer of a supported encoding stack is bounded. Both limits must be positive.

Stats limits and intervals must be positive, with segment bytes no greater than the total cap. Read intervals cannot exceed the browser timer limit (`2147483647ms`, ~24.8 days). Repeated frame polls are cached; rapid switches to a different uncached frame return HTTP 429 with `Retry-After`, which the UI honors. Rotation and retention work even without an open dashboard.

---

## Admin Console — `/admin`

When an admin token is configured, visit `/admin` to sign in. API clients may send `Authorization: Bearer <token>` or `X-Admin-Token`. Browser sessions use HttpOnly, SameSite-strict cookies and expire after 12 hours or a server restart. Plain-HTTP cookie login is allowed only on loopback; remote browser login requires a real TLS connection, not an untrusted forwarded scheme header. All mutations require same-origin browser provenance. Explicit `-admin-token` values override the environment, including empty values; non-loopback access then stays closed.

The built-in web console provides full operational observability:
- **`?page=dashboard`:** Real-time request and token throughput KPIs, TTFT, p95 latency over the last 200 requests, error rates, and 200-request rolling history frames surviving restarts. Aggregate counters and charts reset on server restart. Logs default to `gnvipi/stats.jsonl` under the OS user cache directory (override with `GNVIPI_STATS_FILE`).
- **`?page=models`:** Toggle models on/off (persisted), delete/restore catalog entries, and configure v2 custom providers with live validation.
- **`?page=config`:** Configure virtual `automode` (Claude auto-model alias targeting an enabled real model or `cycle`), `cycle` (ordered round-robin with automatic failover), and web search MCP settings.
- **`?page=chromes`:** Manage headless Chrome worker pools on demand (Turn On / Start All, Kill All). Startup is always idle and never allocates Chrome or token pools until activated.

---

## Model Cycling (`cycle`)

The **config** tab (`/admin?page=config`) always exposes the virtual model `cycle`. Check the models to include and use the arrow controls to define their order. Changes save automatically to `-models-file`. `cycle` remains visible even when empty, returning HTTP 503 until at least one model is selected.

When client requests target `cycle`:
- The gateway routes to the first configured model in the sequence.
- If that model returns **429, 404, 408, 410, or a 5xx status code**, the gateway automatically retries on the next enabled model in the sequence and keeps that model active.
- A successful stream is forwarded immediately; once streaming bytes have been committed to the client, downstream errors cannot be retried safely.
- The active cycle position resets to the first model when the gateway restarts or when the cycle configuration changes.

---

## Web Search (MCP)

The gateway includes a Streamable HTTP Model Context Protocol (`POST /mcp`) endpoint supporting web search:
1. In `/admin?page=config`, select **DuckDuckGo** or **Bing**.
2. Add the tool to Claude Code globally:
   ```bash
   claude mcp add --scope user --transport http gateway-search http://localhost:8080/mcp
   ```

---

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

Overrides are file paths; the default stats log lives beside the captcha state file. A missing models file uses the embedded catalog, but its parent directory must exist and be writable for admin changes to save. Use separate models files for simultaneously running instances.

> [!IMPORTANT]
> **One stats file per process:** Use a distinct `GNVIPI_STATS_FILE` per simultaneous gateway process. Each process owns its active stats file and numeric retained segments.

Stats logs store request metadata, not prompt bodies. Active and numeric segment files are bounded to 16 MiB by default, rotate at 4 MiB or 24h, and expire after 7 days. Inference runs on the selected upstream provider or NVIDIA Playground, and request content is forwarded there—not processed locally inside the gateway.

---

## Troubleshooting

`/healthz` checks server liveness, not captcha readiness. Admin, MCP, and custom-provider routes remain available while Chrome starts or when captcha is disabled. For NVIDIA generation, enable Chrome captcha from Admin (`/admin?page=chromes`), then check `pool.ready` in `/healthz` and worker state on `/admin?page=chromes`.

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

---

## Docker Deployment

A lightweight Docker setup is provided:

```bash
docker build -t gnvipi:local .
docker run --rm -p 127.0.0.1:8080:8080 --shm-size=2g \
  --mount type=volume,source=gnvipi-data,target=/data \
  -e GNVIPI_STATS_FILE=/data/stats.jsonl \
  gnvipi:local -models-file /data/playground_models.json
```

The named `gnvipi-data` volume remains when the container is removed. The image includes Chromium; Docker users do not need Chrome or Go on the host.

---

## Development & Verification

Run the full race-detector test suite and linter:

```bash
# Run all unit, integration, and E2E matrix tests
go test -race ./...

# Run linter
golangci-lint run ./...
```
