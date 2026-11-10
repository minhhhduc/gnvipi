# gnvipi — Universal LLM API Gateway & NVIDIA Playground Reverse-Proxy

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
![Platforms](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

A unified local gateway for AI coding agents (**Claude Code**, **OpenAI SDKs**, **OpenCode**, **Codex**, **Aider**) bridging 3 inbound client protocols (**OpenAI Chat**, **Anthropic Messages**, **OpenAI Responses**) to any upstream provider (**OpenAI-compatible**, **Anthropic-compatible**, **OpenAI Responses**, **NVIDIA Playground**, and custom endpoints).

The gateway combines a high-performance **Adapter-Hub** translation engine and zero-copy passthrough relay with an automated headless Chrome captcha pool for NVIDIA Playground models (DeepSeek-V4, Kimi, GLM, Nemotron, GPT-OSS, and more), protected by dial-time SSRF defenses, inbound authentication, and a built-in Admin Console v2.

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
| `POST /v1/chat/completions` | OpenAI Chat Completions | Standard chat endpoint for OpenAI SDKs, OpenCode, and Aider |
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

Add your providers via the Admin Console at `http://localhost:8080/admin` or directly in `data/providers.json`.

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

Providers are declared using the typed v2 schema in `data/providers.json` (or saved from the Admin UI at `/admin?page=models` into `-models-file`).

### Adding Providers via Admin UI
1. Navigate to `http://localhost:8080/admin?page=models`.
2. Under **Custom Providers**, provide:
   - **Provider Name / ID:** Identifier for namespacing (e.g. `openrouter`, `local-ollama`).
   - **Base URL:** Upstream endpoint root (e.g. `https://openrouter.ai/api/v1` or `http://127.0.0.1:11434/v1`).
   - **Wire Protocol:** Choose `openai_chat`, `anthropic_messages`, or `openai_responses`.
   - **Models & Wildcards:** Map aliases to physical upstream model IDs, or use wildcard patterns (e.g. `openrouter/*`).
   - **API Key:** Upstream credentials (stored securely and segregated from public catalogs).
3. Click **Save & Test Connection**.

### Adding Providers via Configuration File
You can define providers directly in `data/providers.json` or your configured models file:

```json
{
  "schema_version": 2,
  "providers": [
    {
      "id": "openrouter",
      "base_url": "https://openrouter.ai/api/v1",
      "protocol": "openai_chat",
      "auth": {
        "type": "bearer",
        "key_env": "OPENROUTER_API_KEY"
      },
      "models": [
        {
          "alias": "openrouter/*",
          "upstream_model": "*",
          "strip_prefix": true
        }
      ],
      "concurrency": {
        "max_concurrent": 8,
        "max_wait_ms": 5000
      }
    },
    {
      "id": "ollama",
      "base_url": "http://127.0.0.1:11434/v1",
      "protocol": "openai_chat",
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

## Security & Operational Flags

The gateway is secure by default:

| Flag | Environment Variable | Default | Purpose |
|---|---|---|---|
| `-addr` | — | `:8080` | Listen host and port. Use `127.0.0.1:8080` for local-only binding. |
| `-gateway-key` | `GNVIPI_GATEWAY_KEY` | *(disabled)* | Optional comma-separated API keys protecting `/v1/*` and `/mcp`. |
| `-admin-token` | `GNVIPI_ADMIN_TOKEN` | *(disabled)* | Authentication token required for non-loopback `/admin` access. |
| `-gateway-v2` | — | `true` | Gateway core dispatcher routing all custom providers (default on; false is deprecated). |
| `-max-body-bytes` | — | `33554432` (32 MiB) | Maximum request body bytes for `/v1/*` routes (enforces `http.MaxBytesReader`). |
| `-max-admin-body-bytes`| — | `1048576` (1 MiB) | Maximum request body bytes for administrative endpoints. |
| `-deny-private-upstreams`| — | `false` | Deny loopback and RFC 1918 private LAN addresses for custom upstreams (SSRF protection). |
| `-allow-unsafe-upstreams`| — | `false` | Permit link-local and cloud-metadata addresses (`169.254.169.254`) — **unsafe**. |
| `-models-file` | — | `internal/models/playground_models.json` | Path to saved provider configuration and model preferences. |

### Inbound Authentication
When `-gateway-key` or `GNVIPI_GATEWAY_KEY` is set, client requests must present a valid key via `Authorization: Bearer <key>` or `x-api-key: <key>`. Comma-separated keys permit zero-downtime key rotation.

### Dial-Time SSRF Prevention
By default, the gateway strictly blocks all cloud metadata services (`169.254.169.254`) and IPv6 link-local addresses. Pass `-deny-private-upstreams` in hardened multi-tenant environments to restrict all private LAN targets.

---

## Admin Console — `/admin`

The built-in web console provides full operational observability:
- **`?page=dashboard`:** Real-time request and token throughput KPIs, TTFT, p95 latency, error rates, and 200-request rolling history survive restarts.
- **`?page=models`:** Toggle models on/off, delete/restore catalog entries, and configure v2 custom providers with live validation.
- **`?page=config`:** Configure virtual `automode` (Claude auto-model alias), `cycle` (ordered round-robin with automatic failover), and web search MCP settings.
- **`?page=chromes`:** Manage headless Chrome worker pools on demand (Turn On / Start All, Kill All).

---

## Web Search (MCP)

The gateway includes a Streamable HTTP Model Context Protocol (`POST /mcp`) endpoint supporting web search:
1. In `/admin?page=config`, select **DuckDuckGo** or **Bing**.
2. Add the tool to Claude Code globally:
   ```bash
   claude mcp add --scope user --transport http gateway-search http://localhost:8080/mcp
   ```

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

---

## Development & Verification

Run the full race-detector test suite and linter:

```bash
# Run all unit, integration, and E2E matrix tests
go test -race ./...

# Run linter
golangci-lint run ./...
```
