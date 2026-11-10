# Architecture Overview

This document provides a comprehensive overview of the **Universal LLM API Gateway (v2.0)** architecture, explaining how incoming client requests across diverse LLM protocols are routed, normalized, relayed, or translated to upstream providers.

---

## 1. System Topology & Positioning

The gateway serves as a unified local reverse proxy and protocol translator for developer tools and AI coding agents (such as **Claude Code**, **OpenAI Python/Node SDKs**, **Codex**, **OpenCode**, and **Aider**). It simultaneously exposes the three major LLM client protocols and routes requests either to upstream custom API providers or to the automated headless-browser NVIDIA Playground pool:

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Client Applications                             │
│   (Claude Code, OpenAI Python/Node SDK, Codex, OpenCode, Aider, cURL)  │
└────────────────────────────────────┬───────────────────────────────────┘
                                     │ Inbound HTTP (JSON or SSE)
                                     ▼
┌────────────────────────────────────────────────────────────────────────┐
│                          Gateway Inbound Layer                         │
│  ┌───────────────────────┬──────────────────────┬────────────────────┐ │
│  │   Anthropic Messages  │  OpenAI Chat Compl.  │  OpenAI Responses  │ │
│  │   POST /v1/messages   │ POST /chat/complet.  │ POST /v1/responses │ │
│  │   POST /v1/.../tokens │                      │                    │ │
│  └───────────┬───────────┴──────────┬───────────┴──────────┬─────────┘ │
└──────────────┼──────────────────────┼──────────────────────┼───────────┘
               │                      │                      │
               ▼                      ▼                      ▼
┌────────────────────────────────────────────────────────────────────────┐
│               Gateway Core: Dispatcher & Security Guard                │
│  - Inbound Authentication (`-gateway-key` / `GNVIPI_GATEWAY_KEY`)      │
│  - Request Body Bounds (`http.MaxBytesReader` via `-max-body-bytes`)  │
│  - Request Context, Correlation IDs (`X-Request-Id`), Usage Recorder   │
│  - Model Resolver & Prefix Routing (Exact Alias, Wildcard, Fallback)   │
└──────────────────┬─────────────────┬─────────────────┬─────────────────┘
                   │                 │                 │
     Same-Protocol │   Cross-Protocol│   NVIDIA Models │
     Passthrough   │   Translation   │   (Playground)  │
                   ▼                 ▼                 ▼
┌────────────────────┐ ┌───────────────────────────┐ ┌───────────────────┐
│ Same-Proto Relay   │ │ Hub: Inbound Adapter      │ │ CLIProxyAPI &     │
│ (Zero-copy byte    │ │       Decode -> IR        │ │ NVIDIA Executor   │
│  streaming relay)  │ │ Outbound Adapter Registry │ │ (hCaptcha Pool    │
│                    │ │       IR -> Upstream Wire │ │  & Predict API)   │
└─────────┬──────────┘ └─────────────┬─────────────┘ └─────────┬─────────┘
          │                          │                         │
          └──────────────────────────┼─────────────────────────┘
                                     │
                                     ▼
┌────────────────────────────────────────────────────────────────────────┐
│                  Shared Upstream Client & Reliability                  │
│  - Bounded Connect / TLS / Header Timeouts & Idle Stream Watchdogs     │
│  - SSRF Prevention (`netx`: Cloud Metadata 169.254.169.254 / Private)  │
│  - Pre-Stream Retries (429, 502, 503, 504 with Backoff & Jitter)       │
│  - Per-Provider Concurrency Limiting Semaphores & Queue Deadlines      │
│  - Central Secret Redaction Filter (Scrubbing Keys from Errors & Logs) │
└────────────────────────────────────┬───────────────────────────────────┘
                                     │
                                     ▼
┌────────────────────────────────────────────────────────────────────────┐
│                         Upstream Providers                             │
│   (Anthropic API, OpenAI, OpenRouter, DeepSeek, Local Ollama/vLLM)     │
└────────────────────────────────────────────────────────────────────────┘
```

---

## 2. The Adapter-Hub Architecture & The Normalized IR

In a system supporting $N$ inbound client protocols and $M$ outbound upstream protocols, a naive pairwise translator requires $N \times M$ specialized bridges. Adding a new protocol requires updating every existing protocol translator.

The Universal LLM Gateway eliminates this combinatorial explosion by implementing the **Adapter-Hub pattern** centered on a normalized, protocol-neutral **Internal Representation (IR)** defined in `internal/gateway/ir`:

```
                           ┌────────────────────────┐
Inbound Protocol           │   Intermediate Repr.   │        Outbound Protocol
                           │      (ir.Request)      │
[Anthropic Inbound]   ───▶ │                        │ ───▶ [Anthropic Outbound]
[OpenAI Chat Inbound] ───▶ │      ir.Response       │ ───▶ [OpenAI Chat Outbound]
[Responses Inbound]   ───▶ │     ir.StreamEvent     │ ───▶ [Custom / Responses Out]
                           └────────────────────────┘
```

### Decoupled Translation Steps

1. **Inbound Decoding:** The inbound adapter decodes the wire HTTP request body into a canonical `*ir.Request`. It validates client-side parameters, parses messages, extracts multimodal images, normalizes tool definitions into `ir.ToolDefinition`, and parses reasoning parameters (e.g. `thinking.budget_tokens` or `reasoning_effort`).
2. **Model & Provider Resolution:** The dispatcher consults the model resolver (`internal/gateway/routing`) to match the requested model alias to an upstream provider and physical model identifier.
3. **Outbound Compilation:** The outbound adapter translates the `*ir.Request` into the specific JSON schema or payload required by the upstream target (e.g., converting Anthropic tools into OpenAI `tool_calls` schema or OpenAI Responses tools).
4. **Upstream Execution:** The request is dispatched via the shared HTTP transport with concurrency limits, retry policies, and SSRF guards.
5. **Response & Stream Decoding:**
   - **Non-streaming:** The outbound adapter parses the upstream JSON into canonical `*ir.Response`. The inbound adapter encodes `*ir.Response` back into the client-expected wire format.
   - **Streaming:** The outbound adapter reads the upstream SSE stream, parsing raw frames into an iterator sequence of canonical `ir.StreamEvent` items (`StreamEventContentDelta`, `StreamEventReasoningDelta`, `StreamEventToolCallDelta`, etc.). The inbound adapter serializes these events into the client's expected SSE event format (e.g., Anthropic `content_block_delta` or OpenAI `chat.completion.chunk`).

---

## 3. Execution Paths

The gateway evaluates each incoming request and directs it down one of three optimized execution paths:

### A. Same-Protocol Passthrough Relay (Zero-Copy)

When a client's inbound protocol matches the upstream provider's native protocol (for example, an OpenAI Chat Completions client calling an OpenAI-compatible upstream endpoint):
- The gateway **bypasses intermediate IR encoding and decoding entirely**.
- The request body is relayed verbatim, injecting upstream credentials and stripping hop-by-hop headers via `internal/gateway/policy`.
- Upstream streaming responses are relayed byte-for-byte directly to the client with sub-millisecond overhead.
- This ensures absolute maximum throughput, minimal memory allocation, and zero loss of provider-specific extensions.

### B. Cross-Protocol Translation (Hub-and-Spoke)

When the inbound client protocol differs from the upstream provider protocol (for example, Claude Code querying an OpenAI-compatible DeepSeek model, or an OpenAI client calling Anthropic Claude 3.7 Sonnet):
- The request traverses the full Inbound Adapter $\to$ Canonical IR $\to$ Outbound Adapter pipeline.
- Capabilities and unsupported parameters are checked cleanly against protocol feature flags.
- Tool calling schemas, reasoning tokens, and structured outputs are mapped transparently.

### C. NVIDIA Playground Reverse Proxy

Requests targeting NVIDIA Playground models (e.g., `deepseek-ai/deepseek-v4-flash-0731`, `moonshotai/kimi-k3`):
- Fall through the gateway v2 dispatcher to the embedded CLIProxyAPI / NVIDIA executor pipeline.
- Lease single-use hCaptcha tokens minted on demand by the background Chromium worker pool (`internal/captcha`).
- Benefit from SSE delta coalescing (`internal/provider/nvidia/coalesce.go`) to optimize token streaming for local GUI clients.

---

## 4. Component Directory Layout

The following table lists every key package across the repository and its specific responsibility:

| Package | Purpose & Responsibility |
|---|---|
| **`cmd/serve`** | Gateway binary entry point: CLI flag parsing, signal handling, dependency wiring, middleware chain assembly, and Admin UI v2 server. |
| **`internal/gnvipi`** | Client library for the upstream NVIDIA Playground predict API. |
| **`internal/provider/nvidia`** | Captcha-backed NVIDIA provider executor, SSE coalescing, and persistent usage stats. |
| **`internal/captcha`** | Headless Chrome / CDP worker pool for minting single-use hCaptcha tokens (idle on startup, started via Admin UI). |
| **`internal/models`** | Embedded catalog of playground models, capabilities, and fallback configurations. |
| **`internal/gateway/ir`** | Protocol-neutral Internal Representation (`Request`, `Response`, `StreamEvent`, `ContentBlock`, `StopReason`). Zero framework or external HTTP dependencies. |
| **`internal/gateway/sse`** | Spec-compliant Server-Sent Events parser (`Reader`) and serializer (`Writer`) with automatic chunk flushing, ping keepalives, and mid-stream error formatting. |
| **`internal/gateway/gwerr`** | Normalized gateway error model (`Kind`, `Status`, `Message`), safe secret scrubbing, and protocol-specific wire error formatters (Anthropic, OpenAI Chat, OpenAI Responses). |
| **`internal/gateway/adapter`** | Inbound, outbound, and streaming adapter interfaces, capability bitmasks, and central adapter registry. |
| **`internal/gateway/core`** | Generic non-streaming and streaming translation engine coordinating decode-execute-encode workflows. |
| **`internal/gateway/dispatch`** | Central HTTP request dispatcher (`Dispatcher`), request-scoped context initialization, route matching, model resolution, and error handling. |
| **`internal/gateway/inbound/anthropic`** | Inbound adapter for Anthropic Messages (`POST /v1/messages`), JSON request decoder, non-streaming response encoder, and SSE event stream encoder. |
| **`internal/gateway/inbound/openaichat`** | Inbound adapter for OpenAI Chat Completions (`POST /v1/chat/completions`), JSON request decoder, response encoder, and SSE chunk stream encoder. |
| **`internal/gateway/inbound/openairesponses`** | Inbound adapter for OpenAI Responses (`POST /v1/responses`), JSON request decoder, response encoder, and SSE event stream encoder. |
| **`internal/gateway/tokencount`** | Token estimation engine for Anthropic token counting (`POST /v1/messages/count_tokens`) across custom providers. |
| **`internal/gateway/outbound/anthropic`** | Outbound adapter translating IR to Anthropic Messages wire requests, parsing upstream JSON responses, and parsing upstream SSE streams. |
| **`internal/gateway/outbound/openaichat`** | Outbound adapter translating IR to OpenAI Chat wire requests, parsing upstream JSON responses, and parsing upstream SSE chunks. |
| **`internal/gateway/outbound/openairesponses`** | Outbound adapter translating IR to OpenAI Responses wire requests, parsing upstream JSON responses, and parsing upstream SSE events. |
| **`internal/gateway/relay`** | Zero-copy, same-protocol passthrough streaming relay bypassing adapter translation when inbound and upstream protocols match. |
| **`internal/gateway/routing`** | Registry-backed model resolver supporting exact model aliases, wildcard prefix routing (`provider/*`), model cycling (`cycle`), and `automode`. |
| **`internal/gateway/provider`** | Typed provider configuration schema v2, atomic JSON persistence, validation, and legacy catalog migration. |
| **`internal/gateway/policy`** | Upstream header forwarding policies, hop-by-hop header stripping, and provider auth injection. |
| **`internal/gateway/upstream`** | Shared HTTP client factory with granular connect, TLS handshake, header timeouts, proxy support, and TLS profiles. |
| **`internal/gateway/netx`** | Dial-time network security policies, SSRF guards, and private IP / cloud metadata IP range enforcement. |
| **`internal/gateway/redact`** | Central high-performance secret scrubbing filter preventing API keys and credentials from leaking into logs, errors, or stats. |
| **`internal/gateway/reliability`** | Pre-stream retry policies with exponential backoff and jitter, and per-provider concurrency limiting semaphores with queue wait deadlines. |

---

## 5. Architectural Principles

### 1. IR Decoupling
Adapters convert strictly to and from the normalized `internal/gateway/ir` types. **Inbound and outbound adapters never import or depend on each other.** Adding a new protocol requires only one inbound decoder and one outbound compiler; all existing protocols immediately gain bidirectional interoperability with the new protocol.

### 2. Explicit Registration Without Magic
All adapters are registered explicitly during server startup in `internal/gateway/dispatch/register.go`. Go package `init()` side-effects are strictly forbidden, ensuring deterministic boot order, painless unit testing, and easy tree shaking.

### 3. Same-Protocol Streaming Relay
Whenever an incoming request matches the upstream wire protocol, the gateway bypasses IR conversion and uses a direct streaming relay (`internal/gateway/relay`). This provides bare-metal reverse-proxy performance with sub-millisecond TTFT (time-to-first-token).

### 4. Security by Default
- **Inbound Authentication:** Client API keys are validated early via `-gateway-key` / `GNVIPI_GATEWAY_KEY`.
- **Admin Hardening:** Admin routes are restricted to loopback addresses by default and require `-admin-token` / `GNVIPI_ADMIN_TOKEN` when exposed remotely. CSRF and Origin checks protect all mutation endpoints.
- **SSRF Defense:** Provider URLs and dial targets are validated against cloud metadata IPs (`169.254.169.254`) and link-local addresses at both configuration save time and connection dial time.
- **Request Body Limits:** `http.MaxBytesReader` enforces hard memory ceilings (`-max-body-bytes`, default 32 MiB) before route dispatch.
- **Secret Redaction:** Configured API keys and authorization headers are scrubbed centrally from diagnostic dumps, error responses, and stats logs.

### 5. Reliability & Resilience
- **Pre-Stream Retries:** Upstream connection drops or transient status codes (429, 502, 503, 504) trigger configurable pre-stream retries with exponential backoff and full jitter before any bytes are committed to the client.
- **Concurrency Semaphores:** Per-provider concurrency semaphores limit active in-flight requests and queue pending requests up to configurable deadlines, preventing upstream rate-limit thrashing.
- **Idle Stream Watchdogs:** Upstream streaming connections are wrapped with idle-read watchdogs to terminate hung streams cleanly without leaking goroutines.

---

## 6. Related Documentation

- [Provider Configuration Guide (v2)](../providers.md): Schema reference, wire protocols, and examples.
- [Client Setup Guide & Compatibility Matrix](../clients.md): Client configuration examples and cross-protocol feature matrix.
- [Adapter Developer Guide](adapters.md): Guide to implementing new inbound and outbound protocol adapters.
- [Master Implementation Roadmap](universal-llm-gateway-roadmap.md): Milestone dependency graph and complete issue index.
- [Release Notes](../../RELEASE_NOTES.md): Universal LLM Gateway v2.0 release notes and migration instructions.
