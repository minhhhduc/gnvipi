# Client Setup Guide and Cross-Protocol Compatibility Matrix

This document explains how to configure and connect client applications to the gateway, details base URL and authentication conventions, and provides a consolidated feature compatibility matrix across all 9 Inbound × Upstream protocol combinations.

---

## Table of Contents

1. [Base URL Conventions & Endpoints](#base-url-conventions--endpoints)
2. [Authentication & Credential Security](#authentication--credential-security)
3. [Client Configuration Guides](#client-configuration-guides)
   - [Claude Code](#claude-code)
   - [Official OpenAI Python SDK](#official-openai-python-sdk)
   - [Official OpenAI TypeScript / Node.js SDK](#official-openai-typescript--nodejs-sdk)
   - [OpenAI Responses & Codex Clients](#openai-responses--codex-clients)
   - [OpenCode](#opencode)
   - [HTTP & cURL](#http--curl)
4. [Consolidated Feature Compatibility Matrix](#consolidated-feature-compatibility-matrix)
5. [Feature Capabilities & Rejection Policies](#feature-capabilities--rejection-policies)

---

## Base URL Conventions & Endpoints

The gateway listens by default on `http://localhost:8080` (or the address specified by `-addr`). Because different client ecosystems follow distinct URL path conventions, the gateway exposes both `/v1`-prefixed endpoints and root-level routes.

### Base URL: `/v1` vs Gateway Root

| Client SDK / Tool | Recommended Base URL | Why |
|---|---|---|
| **OpenAI SDKs** (Python, TypeScript, Go) | `http://localhost:8080/v1` | Official OpenAI SDKs automatically append `/chat/completions`, `/models`, etc. to `baseURL`. The `/v1` path prefix is expected. |
| **Claude Code & Anthropic SDKs** | `http://localhost:8080` | Anthropic client libraries automatically append `/v1/messages` and `/v1/messages/count_tokens` to `ANTHROPIC_BASE_URL`. |
| **OpenAI Responses / Codex** | `http://localhost:8080/v1` or `http://localhost:8080/backend-api/codex` | Codex CLI and IDE plugins query `/backend-api/codex/responses` or `/v1/responses`. |
| **OpenCode** | `http://localhost:8080/v1` (OpenAI mode)<br>`http://localhost:8080` (Anthropic mode) | OpenCode supports both OpenAI and Anthropic provider backends. |
| **cURL / HTTP Clients** | `http://localhost:8080` | Call exact endpoint paths directly. |

### Supported Client Endpoints

The gateway handles inbound requests across all three primary protocols:

- **OpenAI Chat Completions:** `POST /v1/chat/completions`
- **Anthropic Messages:** `POST /v1/messages`
- **Anthropic Token Counting:** `POST /v1/messages/count_tokens`
- **OpenAI Responses / Codex:** `POST /v1/responses` and `POST /backend-api/codex/responses`
- **Model Discovery:** `GET /v1/models` (returns dual-compatible JSON format honoring OpenAI and Anthropic discovery structures)
- **Health & Ping Checks:** `GET /v1/api/hello`, `HEAD /v1/api/hello`, `GET /api/hello`, `HEAD /api/hello` (provides instant HTTP 200 checks for Claude Code connection validation)

---

## Authentication & Credential Security

When the gateway is launched with `-gateway-key <key>` or the `GNVIPI_GATEWAY_KEY` environment variable, client requests to API endpoints must present a valid gateway token.

### Supported Headers

Clients can authenticate using either:
- `Authorization: Bearer <token>`
- `x-api-key: <token>`

### Precedence and Validation Rules

The gateway authentication middleware (`gatewayAuth`) enforces strict validation invariants:

1. **Authoritative Bearer Token:** When `Authorization` is supplied, it must use the `Bearer <token>` scheme (case-insensitive `Bearer`).
2. **Mutual Consistency:** If both `Authorization` and `x-api-key` headers are present, they **must match the same key**. Any mismatch is rejected immediately with HTTP `401 Unauthorized`.
3. **No Header Smuggling:** Multiple `Authorization` or `x-api-key` headers, malformed schemes (e.g. `Basic`), or extra trailing tokens are rejected.
4. **Credential Isolation (Zero-Leakage):** Upon successful authentication, client credentials (`Authorization` and `x-api-key`) are **stripped from the request context**. The upstream provider receives only the provider-specific credentials configured in the gateway catalog (or none if unauthenticated), never the client's gateway access key.
5. **Constant-Time Verification:** All comparisons use SHA-256 digests in constant time (`subtle.ConstantTimeCompare`) to prevent timing side-channel attacks and hide key rotation orders.

---

## Client Configuration Guides

### Claude Code

Claude Code integrates directly with the gateway. The gateway exposes Anthropic Messages (`/v1/messages`), token counting (`/v1/messages/count_tokens`), connection pings (`/v1/api/hello`), and writes an automatic model discovery cache.

#### 1. Environment Setup

Set the base URL and API key in your shell profile or session:

```bash
export ANTHROPIC_BASE_URL="http://localhost:8080"
export ANTHROPIC_API_KEY="your-gateway-key" # or any non-empty string if unauthenticated
```

#### 2. Automatic Model Discovery Cache

When the gateway starts or updates provider catalog configurations, it automatically writes a model cache file to:
```text
~/.claude/cache/gateway-models.json
```
This enables Claude Code's `/model` switcher to reflect all available models immediately without restarting.

#### 3. Features Tested with Claude Code

- Ephemeral prompt caching headers (`anthropic-beta: prompt-caching-2024-07-31`) and cache control blocks.
- Complex tool definitions and multi-turn agent loops (`tool_use` and `tool_result`).
- System prompt blocks and thinking configurations.
- Fast local token count estimation on `/v1/messages/count_tokens`.

---

### Official OpenAI Python SDK

Install the official OpenAI Python package:

```bash
pip install openai
```

#### Non-Streaming Example

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="your-gateway-key", # or "dummy" if gateway auth is not set
)

response = client.chat.completions.create(
    model="meta/llama-3.3-70b-instruct",
    messages=[
        {"role": "system", "content": "You are a concise programming assistant."},
        {"role": "user", "content": "How do I reverse a string in Go?"},
    ],
)

print(response.choices[0].message.content)
```

#### Streaming (SSE) Example

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="your-gateway-key",
)

stream = client.chat.completions.create(
    model="meta/llama-3.3-70b-instruct",
    messages=[{"role": "user", "content": "Count from 1 to 5."}],
    stream=True,
    stream_options={"include_usage": True},
)

for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
print()
```

#### Structured Output (JSON Schema)

```python
from openai import OpenAI
from pydantic import BaseModel

class Person(BaseModel):
    name: str
    age: int

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="your-gateway-key",
)

completion = client.beta.chat.completions.parse(
    model="meta/llama-3.3-70b-instruct",
    messages=[{"role": "user", "content": "Alice is 30 years old."}],
    response_format=Person,
)

person = completion.choices[0].message.parsed
print(f"Name: {person.name}, Age: {person.age}")
```

---

### Official OpenAI TypeScript / Node.js SDK

Install the package:

```bash
npm install openai
```

#### Chat Completions with Tools

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: process.env.GATEWAY_KEY || "dummy",
});

async function main() {
  const runner = await client.chat.completions.create({
    model: "meta/llama-3.3-70b-instruct",
    messages: [{ role: "user", content: "What is the weather in Tokyo?" }],
    tools: [
      {
        type: "function",
        function: {
          name: "get_weather",
          description: "Get current weather for a city",
          parameters: {
            type: "object",
            properties: {
              city: { type: "string" },
            },
            required: ["city"],
          },
        },
      },
    ],
  });

  console.log(JSON.stringify(runner.choices[0].message, null, 2));
}

main().catch(console.error);
```

---

### OpenAI Responses & Codex Clients

The gateway implements the OpenAI Responses protocol for Codex clients, developer tools, and newer reasoning workflows.

#### Endpoints
- Standard Responses: `POST /v1/responses`
- Codex Compatibility: `POST /backend-api/codex/responses`

#### Example Request Payload

```json
{
  "model": "meta/llama-3.3-70b-instruct",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [{"type": "input_text", "text": "Write a Fibonacci function in Python."}]
    }
  ],
  "reasoning": {
    "effort": "medium"
  }
}
```

---

### OpenCode

OpenCode can be configured to use the gateway as either an OpenAI-compatible or Anthropic-compatible provider.

Add to your OpenCode configuration (`~/.config/opencode/config.json`):

```json
{
  "providers": {
    "gateway-chat": {
      "type": "openai",
      "baseUrl": "http://localhost:8080/v1",
      "apiKey": "your-gateway-key",
      "models": ["meta/llama-3.3-70b-instruct"]
    },
    "gateway-messages": {
      "type": "anthropic",
      "baseUrl": "http://localhost:8080",
      "apiKey": "your-gateway-key",
      "models": ["meta/llama-3.3-70b-instruct"]
    }
  }
}
```

---

### HTTP & cURL

#### 1. Chat Completions (`/v1/chat/completions`)

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer your-gateway-key" \
  -d '{
    "model": "meta/llama-3.3-70b-instruct",
    "messages": [{"role": "user", "content": "Hello!"}],
    "temperature": 0.7
  }'
```

#### 2. Anthropic Messages (`/v1/messages`)

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-gateway-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "meta/llama-3.3-70b-instruct",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

#### 3. Count Tokens (`/v1/messages/count_tokens`)

```bash
curl -X POST http://localhost:8080/v1/messages/count_tokens \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-gateway-key" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "meta/llama-3.3-70b-instruct",
    "messages": [{"role": "user", "content": "Estimate token length for this prompt."}]
  }'
```

#### 4. Model Discovery (`GET /v1/models`)

```bash
curl -X GET http://localhost:8080/v1/models \
  -H "Authorization: Bearer your-gateway-key"
```

---

## Consolidated Feature Compatibility Matrix

The following matrix documents feature support across all **9 Inbound × Upstream** combinations. Every combination has been verified through end-to-end matrix tests (`#56`), tool calling conformance suites (`#57`), client fixtures (`#58`), and capability rejection enforcement (`#34`).

| Inbound Protocol | Upstream Protocol | Streaming (SSE) | Tool Calling (Single) | Parallel Tool Calling | Multimodal Images (URL & Base64) | Reasoning / Thinking Tokens | Structured Output (`json_schema` / `json_object`) | Count Tokens (`/v1/messages/count_tokens`) | Unmodeled Vendor Extensions |
|---|---|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| **OpenAI Chat** | **OpenAI Chat** | Supported | Supported | Supported | Supported | Supported | Supported | N/A (Client protocol lacks endpoint) | Byte-for-byte / Lossless relay |
| **OpenAI Chat** | **Anthropic Messages** | Supported | Supported | Supported | Supported | Supported (Bidirectional effort ↔ budget) | Supported with `tool_emulation` / `json_mode_prompt` (otherwise dropped/rejected) | N/A (Client protocol lacks endpoint) | Preserved via `ir.Request.Extensions` |
| **OpenAI Chat** | **OpenAI Responses** | Supported | Supported | Supported | Supported | Supported (Effort ↔ reasoning item) | Supported (Mapped to `text.format`) | N/A (Client protocol lacks endpoint) | Preserved via `ir.Request.Extensions` |
| **Anthropic Messages** | **OpenAI Chat** | Supported | Supported | Supported | Supported | Supported (Budget tokens ↔ `reasoning_effort`) | Supported via prompt / schema mapping | Supported (Local gateway estimation) | Preserved via `ir.Request.Extensions` |
| **Anthropic Messages** | **Anthropic Messages** | Supported | Supported | Supported | Supported | Supported | Supported with `tool_emulation` / `json_mode_prompt` | Supported (Native upstream relay) | Byte-for-byte / Lossless relay |
| **Anthropic Messages** | **OpenAI Responses** | Supported | Supported | Supported | Supported | Supported (Budget tokens ↔ `reasoning.effort`) | Supported via Responses `text.format` | Supported (Local gateway estimation) | Preserved via `ir.Request.Extensions` |
| **OpenAI Responses** | **OpenAI Chat** | Supported | Supported | Supported | Supported | Supported (`reasoning.effort` ↔ `reasoning_effort`) | Supported (Mapped to `response_format`) | N/A (Client protocol lacks endpoint) | Preserved via `ir.Request.Extensions` |
| **OpenAI Responses** | **Anthropic Messages** | Supported | Supported | Supported | Supported | Supported (`reasoning.effort` ↔ budget tokens) | Supported with `tool_emulation` / `json_mode_prompt` (otherwise dropped/rejected) | N/A (Client protocol lacks endpoint) | Preserved via `ir.Request.Extensions` |
| **OpenAI Responses** | **OpenAI Responses** | Supported | Supported | Supported | Supported | Supported | Supported | N/A (Client protocol lacks endpoint) | Byte-for-byte / Lossless relay |

### Feature Details & Implementation Invariants

1. **Streaming (SSE):**
   - Full event stream conversion across all 9 combinations:
     - Chat `chat.completion.chunk` (`choices[0].delta.content`, `delta.tool_calls`, `delta.reasoning_content`)
     - Anthropic `message_start`, `content_block_start`, `content_block_delta` (`text_delta`, `input_json_delta`, `thinking_delta`), `message_delta`, `message_stop`
     - Responses `response.created`, `response.output_item.added`, `response.output_item.delta`, `response.completed`
   - Terminal events are guaranteed; stream readers are flushed and closed cleanly.

2. **Tool Calling & Parallel Invocations:**
   - Full 2-turn agent execution loops (Tool Call → Tool Result submission → Final Answer) supported in both streaming and non-streaming modes.
   - Cross-protocol normalization of tool definitions (JSON schema), tool call IDs, arguments (with streamed JSON fragmentation coalescing), and tool execution results (including error flags and multimodal output blocks).
   - If an upstream disables `tools` or `parallel_tool_calls`, the gateway enforces capability rejection or parameter dropping as described below.

3. **Multimodal Images:**
   - Automatically converts between OpenAI inline base64 data URLs (`data:image/png;base64,...`), remote HTTP/HTTPS URLs, and Anthropic source blocks (`{"type":"base64","media_type":"...","data":"..."}`).
   - Validated against MIME type allow-lists (`image/jpeg`, `image/png`, `image/gif`, `image/webp`) and a default 20 MB size limit (`DefaultMaxImageBytes`).

4. **Reasoning & Thinking Tokens:**
   - **Anthropic ↔ OpenAI Chat/Responses:**
     - Budget to effort (`BudgetToEffort`):
       - Budget tokens `<= 2048` map to effort `low`.
       - Budget tokens `2049`–`8192` map to effort `medium`.
       - Budget tokens `> 8192` map to effort `high`.
     - Default effort to budget (`DefaultEffortToBudget`):
       - Effort `low` maps to 1024 tokens.
       - Effort `medium` maps to 4096 tokens.
       - Effort `high` maps to 16384 tokens.
   - Streamed reasoning deltas map losslessly to Anthropic `thinking_delta` or OpenAI `reasoning_content` deltas.

5. **Structured Output (`response_format`):**
   - OpenAI Chat and Responses upstreams natively support JSON object mode and strict JSON schema formats.
   - When Anthropic is the upstream, Anthropic's wire protocol lacks a native `response_format` field. The gateway supports structured output for Anthropic upstreams via **Tool Emulation** (`"tool_emulation": true` or `"structured_output": "tool"` in provider extensions) or **JSON Prompt Injection** (`"json_mode_prompt": true`). Without emulation enabled, requesting `response_format` against an Anthropic upstream drops the parameter in lenient mode or rejects it in strict mode.

6. **Count Tokens (`/v1/messages/count_tokens`):**
   - Inbound requests to `/v1/messages/count_tokens` targeting an Anthropic upstream with token count capability are relayed directly to the upstream's native endpoint.
   - Requests targeting OpenAI Chat, OpenAI Responses, or Anthropic upstreams without native count support are processed locally by the gateway's token estimation engine (`internal/gateway/tokencount`), returning an exact HTTP 200 `{"input_tokens": <count>}` estimate.

7. **Vendor Extension Preservation:**
   - **Same-protocol:** Requests are relayed verbatim or byte-for-byte; unmodeled fields are never discarded.
   - **Cross-protocol:** Unmodeled client fields are collected into `ir.Request.Extensions` during decoding and re-injected as top-level fields by outbound serializers, while ensuring reserved or conflicting fields are safely managed.

---

## Feature Capabilities & Rejection Policies

Per Issue `#34`, the gateway enforces provider capability checks to ensure predictable behavior when client requests use features not supported by the resolved upstream provider.

### Policy Modes

The policy mode can be configured globally (defaulting to `lenient`) or per provider via `extensions.feature_policy`:

#### 1. `lenient` Mode (Default)
- Non-semantic unsupported features (such as `top_k`, `reasoning`, `response_format`, or `parallel_tool_calls`) are automatically stripped from the request before forwarding to the upstream.
- The names of all dropped parameters are reported in the response header:
  ```http
  X-Gateway-Dropped-Params: parallel_tool_calls,top_k
  ```
- Semantic requirements that cannot be safely omitted **always fail with HTTP 400 Bad Request** (`gwerr.KindInvalidRequest`), even in lenient mode:
  - `tools` requested when upstream has `tools: false` (`tools are not supported by target upstream`)
  - `n > 1` (multiple choices) (`multiple choices (n > 1) are unsupported`)
  - `previous_response_id` on non-responses upstream (`stateful conversation (previous_response_id) is not supported by target upstream`)

#### 2. `strict` Mode
- Any unsupported feature present in the request immediately halts processing and returns an HTTP 400 Bad Request error detailing the unsupported features:
  ```json
  {
    "error": {
      "message": "unsupported feature for target upstream: reasoning",
      "type": "invalid_request_error"
    }
  }
  ```

### Configuring Capabilities and Policies per Provider

In `models.json` (or via the Admin UI), you can configure capability overrides and policy modes in the provider's `extensions` object:

```json
{
  "id": "my-restricted-provider",
  "name": "Restricted Backend",
  "base_url": "https://api.example.com/v1",
  "protocol": "openai-chat",
  "extensions": {
    "feature_policy": "strict",
    "capabilities": {
      "streaming": true,
      "tools": true,
      "parallel_tool_calls": false,
      "reasoning": false,
      "structured_output": true,
      "images": "base64"
    }
  }
}
```
