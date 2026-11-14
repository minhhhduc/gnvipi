# gnvipi — Universal LLM API Gateway & NVIDIA Playground Reverse-Proxy

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
![Platforms](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

A unified local gateway for AI coding agents (**Claude Code**, **OpenAI SDKs**,
**Codex**, **OpenCode**, and **cURL**). It bridges 3 inbound client protocols
(**OpenAI Chat**, **Anthropic Messages**, **OpenAI Responses**) to any upstream
provider (**OpenAI-compatible**, **Anthropic-compatible**, **OpenAI Responses**,
**NVIDIA Playground**, and custom endpoints), and adds an automated headless Chrome
captcha pool for the free NVIDIA Playground models (DeepSeek-V4, Kimi, GLM,
Nemotron, GPT-OSS, and more).

Client API-key authentication is optional: set `-gateway-key` or
`GNVIPI_GATEWAY_KEY` to protect `/v1/*`, `/mcp`, and SDK inference aliases. See
[Security](#security) below before exposing the gateway beyond localhost.

---

## Documentation

| Guide | What it covers |
|---|---|
| [Operations](docs/operations.md) | Run flags, stats storage, Admin Console access, `cycle` failover, web search MCP, data locations, troubleshooting, Docker notes. |
| [Provider Configuration](docs/providers.md) | Schema v2 reference, model mappings, wildcard routing, fallback chains, SSRF policy, migration. |
| [Client Setup](docs/clients.md) | Step-by-step setup for Claude Code, OpenAI SDKs, Codex, OpenCode, cURL, and the cross-protocol compatibility matrix. |
| [Architecture Overview](docs/architecture/overview.md) | Hub-and-spoke pattern, normalized IR, execution paths, architectural invariants. |
| [Adapter Developer Guide](docs/architecture/adapters.md) | Implementing, registering, and testing new inbound/outbound protocol adapters. |
| [Roadmap](docs/architecture/universal-llm-gateway-roadmap.md) | Milestone breakdown and issue index. |
| [Release Notes](RELEASE_NOTES.md) | v2.0 feature highlights, deprecations, and upgrade guide. |

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
| `/admin` | Admin Console | Web console for providers, model masks, Chrome workers, and metrics |
| `GET /healthz` | Health Check | Gateway liveness and captcha pool status |

---

## Quick Start

### 1. Custom Providers (Zero-Browser Mode — No Chrome, No Captcha Needed)

For custom upstreams only (OpenAI, Anthropic, OpenRouter, DeepSeek, or local
Ollama / vLLM), the gateway runs as a pure Go proxy without allocating any browser
processes:

```bash
# Any platform: listen locally on port 8080
go run ./cmd/serve -addr 127.0.0.1:8080
```

On Windows:

```powershell
.\scripts\run.ps1 -NoCaptcha
```

Add your providers via the Admin Console at `http://localhost:8080/admin?page=models`
or directly in `-models-file` (default: `internal/models/playground_models.json`).

### 2. NVIDIA Playground (With Headless Chrome Captcha Solvers)

To serve free models from the NVIDIA Playground (e.g.
`deepseek-ai/deepseek-v4-flash-0731`, `moonshotai/kimi-k3`):

1. Start the gateway:
   ```bash
   go run ./cmd/serve -addr 127.0.0.1:8080
   ```
2. Open `http://localhost:8080/admin?page=chromes` and click **Turn On / Start All**.
   *Startup is always idle and never launches Chrome automatically until activated.*
3. On Windows, you can also use:
   ```powershell
   .\scripts\run.ps1          # Starts gateway with Chrome pool ready for activation
   .\scripts\run.ps1 -Claude  # Starts gateway and launches Claude Code pointed at it
   ```

For the DB-backed local flow, start PostgreSQL, migrate, create the first owner,
and give `run.ps1 -Claude` the one-time gateway token:

```powershell
# Local-development credentials only; do not reuse them in production.
docker compose up -d --wait db
$env:GNVIPI_DATABASE_URL='postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable'
$env:GNVIPI_MASTER_KEY='<base64-32-byte-key>'
New-Item -ItemType Directory -Force .\bin | Out-Null
go build -o .\bin\serve.exe ./cmd/serve
.\bin\serve.exe migrate
# The password is read from stdin and never echoed: pipe it in.
'<temporary-password>' | .\bin\serve.exe admin create-owner --username <owner> --with-gateway-token
$env:GNVIPI_GATEWAY_TOKEN='gw_...'
.\scripts\run.ps1 -Claude
```

`serve admin create-owner` and `serve admin reset-password <user>` refuse a
terminal stdin so the password is never echoed; pipe the password in on stdin
instead of typing it interactively.

The bootstrap command prints the gateway token once. `run.ps1` forwards it to
Claude Code as `ANTHROPIC_AUTH_TOKEN` without printing the token.

---

## Client Configuration

DB-backed clients use the generated `gw_...` token. The `local-gateway`
values in the remaining rows are legacy/manual `-gateway-key` placeholders;
`run.ps1 -Claude` rejects that literal.

Point your client at the gateway root or `/v1`, depending on the SDK:

| Client Application / SDK | Base URL | Auth Token / Key |
|---|---|---|
| **Claude Code** | `http://localhost:8080` (no `/v1` suffix) | generated `gw_...` token (DB-backed) or configured `-gateway-key` (legacy) |
| **OpenAI Python SDK** | `http://localhost:8080/v1` | `local-gateway` (or configured `-gateway-key`) |
| **OpenAI Node / TypeScript SDK** | `http://localhost:8080/v1` | `local-gateway` (or configured `-gateway-key`) |
| **Codex / OpenAI Responses** | `http://localhost:8080/v1` | `local-gateway` (or configured `-gateway-key`) |
| **OpenCode** | `http://localhost:8080/v1` (OpenAI) or `http://localhost:8080` (Anthropic) | `local-gateway` (or configured `-gateway-key`) |
| **cURL / HTTP Clients** | `http://localhost:8080` | `Bearer local-gateway` |

### Claude Code

For the DB-backed flow, set the generated token in the environment or in
`.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:8080",
    "ANTHROPIC_AUTH_TOKEN": "gw_...",
    "ANTHROPIC_API_KEY": "",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"
  }
}
```

### OpenAI Python SDK

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

### cURL

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

Node/TypeScript, Codex CLI, OpenCode, and the full compatibility matrix are in
[docs/clients.md](docs/clients.md).

---

## Provider Configuration

Providers are declared with the typed v2 schema, either in the configured models
file (`-models-file`) or from the Admin UI at `/admin?page=models`. From the UI:
give the provider an **ID** and **base URL**, pick the **wire protocol**
(`openai-chat`, `anthropic-messages`, or `openai-responses`), map model aliases
(upstream IDs or wildcards such as `openrouter/*`), add the **API key**, then
**Save & Test Connection**.

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
      "auth": { "scheme": "bearer", "key_env": "OPENROUTER_API_KEY" },
      "models": [{ "alias": "openrouter/*", "upstream_model": "*" }],
      "max_concurrency": 8,
      "queue_timeout": "5s"
    }
  ]
}
```

The complete schema — fallback chains, header policies, timeouts, TLS profiles,
SSRF policy, and legacy migration — is in [docs/providers.md](docs/providers.md).

---

## Security

> [!WARNING]
> The default `-addr` is `127.0.0.1:8080`, so the gateway listens on **loopback
> only**. With no `-gateway-key` configured, `/v1/*` and `/mcp` accept
> **unauthenticated** requests from anything that can reach the port; placeholder
> values such as `local-gateway` do not secure it.
>
> Binding to a wildcard address (`-addr :8080`, `docker run -p 8080:8080`) exposes
> it on **all interfaces**. `/v1/*` and `/mcp` then stay open until you set
> `-gateway-key`, and `/admin` answers **HTTP 403** until you set `-admin-token` or
> `GNVIPI_ADMIN_TOKEN`. For any non-loopback deployment, set both and prefer a
> firewall or an authenticated reverse proxy over direct exposure.

Configured provider, gateway, and admin credentials are scrubbed from diagnostic
dumps, error responses, and new statistics records; upstream dial targets are
checked against cloud-metadata and link-local addresses at configuration save time
and again at dial time. Details: [docs/clients.md](docs/clients.md) for client
authentication, [docs/providers.md](docs/providers.md) for upstream URL policy,
[docs/operations.md](docs/operations.md) for admin sessions and flags.

---

## Admin Console — `/admin`

The built-in console covers the whole runtime: live stats and request history,
model toggles and provider editing, `automode` / `cycle` configuration, and
on-demand Chrome worker pools. Visit `http://localhost:8080/admin` (sign in first
if `-admin-token` is set). Per-tab detail and session rules are in
[docs/operations.md](docs/operations.md).

---

## Docker Deployment

```bash
docker build -t gnvipi:local .
docker run --rm -p 127.0.0.1:8080:8080 --shm-size=2g \
  --mount type=volume,source=gnvipi-data,target=/data \
  -e GNVIPI_STATS_FILE=/data/stats.jsonl \
  gnvipi:local -models-file /data/playground_models.json
```

The image includes Chromium, so Docker users need neither Chrome nor Go on the
host. The named `gnvipi-data` volume survives container removal. Port-publishing
and `-admin-token` rules are in [docs/operations.md](docs/operations.md).

---

## Development & Verification

```bash
go test -race ./...          # unit, integration, and E2E matrix tests
golangci-lint run ./...      # linter
```

Conventions and test layout: [docs/development/conventions.md](docs/development/conventions.md)
and [docs/development/testing.md](docs/development/testing.md).
