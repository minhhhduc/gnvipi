# Universal LLM API Gateway v2.0 Release Notes

We are thrilled to announce the official release of **Universal LLM API Gateway v2.0**.

This release marks the evolution of `gnvipi` from a specialized NVIDIA Playground reverse-proxy into a comprehensive, high-performance **Universal LLM API Gateway**. The gateway acts as a local adapter hub that allows AI coding agents (Claude Code, OpenAI SDKs, Codex, OpenCode, cURL) to communicate seamlessly with any upstream provider (OpenAI-compatible, Anthropic-compatible, OpenAI Responses, NVIDIA Playground, or custom endpoints) regardless of protocol differences.

---

## Highlights & Key Capabilities

- **Universal Translation Matrix:** Full bidirectional translation across all 3 primary LLM wire protocols: **OpenAI Chat Completions**, **Anthropic Messages**, and **OpenAI Responses** (9 protocol combinations in both non-streaming JSON and Server-Sent Events streaming).
- **Zero-Copy Passthrough Relay:** When client and upstream wire protocols match, requests and SSE streams bypass translation entirely for bare-metal proxy performance and sub-millisecond TTFT.
- **Full Tool Calling Parity:** Seamless conversion between Anthropic `tool_use`/`tool_result`, OpenAI `tool_calls`/`tool`, and OpenAI Responses `function_call` across both single-turn and streaming interactions.
- **Reasoning Effort & Thinking Budget Translation:** Transparent bidirectional mapping between Anthropic `thinking.budget_tokens` and OpenAI `reasoning_effort` (`low`, `medium`, `high`).
- **Multimodal Image Support:** Automatic translation between OpenAI inline Data URLs (`data:image/...;base64,...`) and Anthropic base64 image content blocks.
- **Structured Outputs Parity:** Unified translation of `response_format` JSON mode and JSON schema definitions.
- **Provider Store v2 & Resilience:** Hierarchical provider schema, prefix-stripping wildcard routing (`provider/*`), multi-hop fallback chains, and virtual model routing (`automode`, `cycle`).
- **Hardened Security Architecture:** Inbound API key authentication, loopback-only Admin UI with token gating and CSRF defense, dial-time SSRF guards, hard memory body limits (`MaxBytesReader`), and central secret redaction.
- **Admin UI v2:** Brand-new administrative dashboard for configuring providers, running connection tests, masking models, inspecting per-request analytics, and controlling the headless Chrome captcha worker pool.

---

## 1. Universal Protocol Translation & Streaming

### The 9-Combination Cross-Protocol Matrix

The v2 gateway core operates around a normalized **Internal Representation (IR)** (`internal/gateway/ir`). Inbound adapters decode client requests into the IR, and outbound adapters encode the IR into the upstream wire protocol. Responses and SSE streams are transformed symmetrically:

| Inbound Client Protocol | Upstream: OpenAI Chat | Upstream: Anthropic Messages | Upstream: OpenAI Responses |
|---|---|---|---|
| **OpenAI Chat Completions** (`POST /v1/chat/completions`) | Passthrough Relay (Zero-Copy) | Cross-Protocol Translation | Cross-Protocol Translation |
| **Anthropic Messages** (`POST /v1/messages`) | Cross-Protocol Translation | Passthrough Relay (Zero-Copy) | Cross-Protocol Translation |
| **OpenAI Responses** (`POST /v1/responses`) | Cross-Protocol Translation | Cross-Protocol Translation | Passthrough Relay (Zero-Copy) |

In addition, Anthropic token counting (`POST /v1/messages/count_tokens`) is natively supported across all upstream providers via exact adapter mapping or calibrated local heuristics (`internal/gateway/tokencount`).

### Zero-Copy Same-Protocol Relay

When the client and upstream provider share the same wire protocol, the gateway bypasses IR parsing and serialization entirely. The request body is relayed directly, upstream headers are sanitized according to RFC rules, and the upstream response SSE stream is piped to the client with sub-millisecond latency.

### Full Streaming Interoperability

The streaming engine (`internal/gateway/sse` & `internal/gateway/core`) normalizes streaming events into canonical chunks:
- Content deltas (`text_delta`, `content_block_delta`)
- Reasoning/thinking deltas (`thinking_delta`, `reasoning_content`)
- Tool call invocations and argument deltas (`tool_call_delta`, `input_json_delta`)
- Mid-stream upstream error recovery and graceful disconnection handling.

---

## 2. Advanced Feature Parity

### Tool Calling & Function Execution
- Translates Anthropic tool declarations (`input_schema`) to OpenAI function definitions (`parameters`) and vice versa.
- Preserves unique tool call IDs across protocol boundaries.
- Reconstructs fragmented streaming tool arguments (`partial_json`) into complete JSON payloads for non-streaming consumers.
- Handles multi-turn tool result chains accurately across Anthropic `role: user` tool result blocks and OpenAI `role: tool` messages.

### Reasoning Effort & Thinking Budgets
- Translates Anthropic Claude 3.7 / thinking mode configurations (`thinking: {"type": "enabled", "budget_tokens": N}`) into OpenAI reasoning effort levels (`low`, `medium`, `high`) using token budget heuristics.
- Maps OpenAI reasoning effort settings into Anthropic thinking budgets.
- Emits dedicated thinking blocks and handles models that output reasoning in `reasoning_content` deltas.

### Multimodal Vision
- Ingests image inputs formatted as data URIs (`data:image/png;base64,...`) or Anthropic image blocks (`source: {"type": "base64", "media_type": "...", "data": "..."}`).
- Normalizes image formats, validates byte ceilings, and synthesizes protocol-compliant image payloads for upstream models.

### JSON Mode & Structured Outputs
- Automatically maps `response_format: {"type": "json_object"}` or `json_schema` across OpenAI and Anthropic upstream endpoints.

---

## 3. Resilient Routing & V2 Provider Management

### Provider Schema v2
Providers are organized into physical upstream targets under the configured models file (`-models-file`, default: `internal/models/playground_models.json`):
- **Multiple Models per Provider:** Physical endpoints are decoupled from model identifiers.
- **Wildcard Prefix Routing:** Wildcard mappings such as `openrouter/*` forward any model request under that prefix directly to OpenRouter, automatically stripping or preserving the prefix as configured.
- **Failover & Fallback Chains:** Providers can declare fallback provider IDs. If the primary upstream returns a transient error (e.g., 429, 502, 503, 504) before streaming commits, the gateway automatically retries on the fallback provider.
- **Virtual Models:** Virtual models `automode` (Claude auto-model alias) and `cycle` (ordered round-robin with error-driven failover) are routed through the v2 resolver across all client protocols.

### Admin Console v2
The Admin UI at `/admin` has been rewritten:
- **Provider Editor:** Add, update, test, and remove v2 providers with live connection validation.
- **Model Masking:** Enable or disable specific models dynamically; changes are reflected immediately in `GET /v1/models`.
- **Live Diagnostics & Analytics:** Per-request latency, token throughput, TTFT (time-to-first-token), and error tracking with historical charts.
- **Chrome Worker Management:** Explicit start/stop controls for headless Chrome captcha solvers used by NVIDIA Playground models.

---

## 4. Security & Hardening

- **Inbound Client Authentication:** Protect gateway endpoints (`/v1/*`, `/mcp`) using `-gateway-key` or `GNVIPI_GATEWAY_KEY`. Supports multiple comma-separated keys for zero-downtime rotation.
- **Admin Access Hardening:** `/admin` routes default to loopback access (`127.0.0.1` / `::1`). Remote exposure requires `-admin-token` or `GNVIPI_ADMIN_TOKEN`. All mutations enforce SameSite cookies and CSRF Origin checks.
- **Dial-Time SSRF Prevention:** Network transport (`internal/gateway/netx`) blocks cloud metadata IP addresses (`169.254.169.254`), IPv6 equivalents, and link-local ranges at both configuration save time and dial time. The `-deny-private-upstreams` flag enables strict blocking of all RFC 1918 private IP addresses.
- **Request Body Ceilings:** Gateway boundaries enforce memory limits via `http.MaxBytesReader` (`-max-body-bytes`, default 32 MiB; `-max-admin-body-bytes`, default 1 MiB).
- **Central Secret Redaction:** Upstream API keys, gateway tokens, and authorization headers are scrubbed centrally from diagnostic dumps, error responses, and persistent stats logs.

---

## 5. Verification & Test Coverage

The v2 gateway release has been validated by an exhaustive automated test suite:
- **E2E 18-Combination Matrix:** Validates all 9 Inbound $\times$ Outbound protocol pairs in both non-streaming JSON and streaming SSE modes against live fake upstreams (`internal/gateway/gatewaytest`).
- **Tool Conformance Suite:** Validates end-to-end tool calling across all protocol pairs, verifying tool definition mapping, argument streaming, and multi-turn execution.
- **Client Fixtures:** Integration test suites verifying compatibility with official client SDKs:
  - Claude Code CLI
  - OpenAI Python SDK (`openai>=1.0.0`)
  - OpenAI TypeScript/Node.js SDK
  - OpenAI Responses / Codex CLI
  - OpenCode
  - cURL / HTTP clients
- **Security & Limits Suite:** Verifies SSRF dial blocking, body size limit enforcement (HTTP 413), authentication failure responses (HTTP 401), and secret redaction.

---

## 6. Deprecations & Breaking Changes

1. **Gateway v2 Enabled by Default:**  
   The gateway core dispatcher (`-gateway-v2`) is now enabled by default (`-gateway-v2=true`). All custom provider traffic is routed through the v2 dispatcher. The legacy passthrough middleware is deprecated; setting `-gateway-v2=false` provides a temporary escape hatch.
2. **Provider Configuration Schema Migration:**  
   The flat provider representation from v1 has been superseded by Schema v2. When loading legacy configuration files, the gateway automatically migrates existing providers to v2 and persists literal API keys securely.
3. **Admin UI Access Policy:**  
   Administrative mutations now require a valid admin token when the server is bound to non-loopback interfaces. Unauthenticated access from remote IPs will receive HTTP 403 Forbidden.

---

## 7. Upgrade & Migration Guide

### 1. Starting the Gateway
No flags are required to enable v2. Simply run:

```bash
# Standard local start (custom providers work immediately without Chrome)
go run ./cmd/serve -addr 127.0.0.1:8080
```

On Windows:
```powershell
.\scripts\run.ps1 -NoCaptcha
```

### 2. Automatic Config Migration
If you have an existing `internal/models/playground_models.json` file:
- On startup, the gateway detects legacy custom endpoints and converts them to v2 providers in memory.
- The next configuration save from the Admin UI persists the new v2 schema.
- API keys are stripped from public catalog files and managed via secure storage.

### 3. Client Configuration
- **Claude Code:** Point `ANTHROPIC_BASE_URL` to `http://localhost:8080`.
- **OpenAI SDK:** Point `base_url` to `http://localhost:8080/v1`.
- **OpenAI Responses / Codex:** Point to `http://localhost:8080/v1`.

For detailed client configuration examples, see [Client Setup Guide](docs/clients.md).  
For advanced provider schemas and wildcard rules, see [Provider Configuration Guide](docs/providers.md).
