# Provider Configuration Guide (v2)

This document is the authoritative reference for configuring upstream providers in `gnvipi`. It covers the v2 provider schema, wire protocols, authentication, model mapping, wildcard routing, fallback chains, timeouts, concurrency controls, SSRF security policies, and the migration path from legacy configurations.

---

## Table of Contents

1. [Overview & Schema Architecture](#overview--schema-architecture)
2. [Provider V2 Schema Reference](#provider-v2-schema-reference)
   - [Core Identity & Base URL](#core-identity--base-url)
   - [Wire Protocols](#wire-protocols)
   - [Authentication & Secrets Management](#authentication--secrets-management)
   - [Headers & Header Policies](#headers--header-policies)
   - [Model Mappings & Wildcard Routing](#model-mappings--wildcard-routing)
   - [Fallback Chains](#fallback-chains)
   - [Timeouts](#timeouts)
   - [Concurrency Limits](#concurrency-limits)
   - [TLS Profiles](#tls-profiles)
   - [SSRF & Upstream Network Security](#ssrf--upstream-network-security)
   - [Extensions & Extra Body](#extensions--extra-body)
3. [Practical Configuration Examples](#practical-configuration-examples)
   - [1. OpenRouter (Prefix-Stripping Wildcard & Fallbacks)](#1-openrouter-prefix-stripping-wildcard--fallbacks)
   - [2. Anthropic Direct](#2-anthropic-direct)
   - [3. OpenAI Direct (Concurrency Limits & Timeouts)](#3-openai-direct-concurrency-limits--timeouts)
   - [4. Local Ollama (Unauthenticated Loopback)](#4-local-ollama-unauthenticated-loopback)
   - [5. Local vLLM Engine](#5-local-vllm-engine)
   - [6. Anthropic-Compatible Gateway Proxy](#6-anthropic-compatible-gateway-proxy)
   - [7. Complete `models.json` Catalog Document](#7-complete-modelsjson-catalog-document)
4. [Migration Guide: Legacy to V2](#migration-guide-legacy-to-v2)
   - [Legacy Format Overview](#legacy-format-overview)
   - [Field & Protocol Mappings](#field--protocol-mappings)
   - [Endpoint Grouping & Model Consolidation](#endpoint-grouping--model-consolidation)
   - [Credentials Separation (`secrets.json`)](#credentials-separation-secretsjson)
   - [Side-by-Side Migration Example](#side-by-side-migration-example)

---

## Overview & Schema Architecture

The universal LLM gateway decouples inbound client protocol interfaces from outbound upstream provider protocols. In v2:

- **Providers** represent physical or logical upstream endpoints (such as OpenAI, Anthropic, OpenRouter, or an on-premises Ollama/vLLM instance).
- **Models** are grouped within their owning provider rather than duplicated across flat endpoint rows.
- **Secrets** are segregated from public catalog definitions, stored either in dedicated restricted files (`secrets.json` with `0600` permissions) or referenced from environment variables.
- **Routing** maps client request model aliases to upstream model identifiers using exact matches or wildcard prefixes with automatic prefix-stripping.
- **Resilience** features include multi-hop fallback chains, granular timeouts, per-provider concurrency limits with queue wait times, and SSRF dial protection.

Configuration documents are stored in `models.json` under schema version 2:

```json
{
  "schema_version": 2,
  "providers_v2": []
}
```

---

## Provider V2 Schema Reference

### Core Identity & Base URL

| Field | Type | Required | Description |
|---|---|---|---|
| `id` | `string` | **Yes** | Unique identifier for the provider (e.g. `"openrouter"`, `"anthropic-direct"`). Must not be empty. |
| `name` | `string` | **Yes** | Human-readable display label (e.g. `"OpenRouter"`, `"Anthropic"`). |
| `base_url` | `string` | **Yes** | Upstream base URL (e.g. `"https://api.openai.com/v1"`). |
| `proxy_url` | `string` | No | Optional HTTP/HTTPS/SOCKS5 proxy dedicated to this provider (e.g. `"socks5://proxy.internal:1080"`). Host-only; query and path are forbidden. |
| `enabled` | `bool` | **Yes** | When `false`, this provider and all its models are excluded from runtime routing. |

#### Base URL Validation Rules

1. **Scheme**: Must use `http` or `https`. All other schemes (such as `ftp`, `file`) are rejected.
2. **Host**: Must include a non-empty hostname or IP address.
3. **No Userinfo**: Credentials in URLs (e.g. `http://user:pass@host`) are strictly rejected.
4. **No Query or Fragment**: Query strings (`?foo=bar`) and hash fragments (`#target`) are rejected.
5. **Port**: Optional; if specified, must be between 1 and 65535.
6. **Path Prefix**: Path prefixes (e.g. `/v1` or `/api/v1`) are retained when constructing upstream endpoints.

---

### Wire Protocols

The `protocol` field determines the wire serialization adapter used for outbound requests.

| Protocol Value | Name | Default Upstream Endpoint |
|---|---|---|
| `openai-chat` | OpenAI Chat Completions API | `POST {base_url}/chat/completions` |
| `anthropic-messages` | Anthropic Messages API | `POST {base_url}/messages` |
| `openai-responses` | OpenAI Responses API | `POST {base_url}/responses` |
| `custom:<name>` | Custom outbound adapter | Defined by registered adapter |

- The gateway automatically translates between inbound client protocols and outbound provider protocols. For instance, a client calling `POST /v1/chat/completions` can seamlessly route to a provider speaking `anthropic-messages`.
- Custom adapters (`custom:<name>`) must match `[a-z0-9][a-z0-9._-]*`.

---

### Authentication & Secrets Management

The `auth` object configures how credentials are sent upstream.

```json
{
  "auth": {
    "scheme": "bearer",
    "key_env": "OPENAI_API_KEY"
  }
}
```

| Field | Type | Description |
|---|---|---|
| `scheme` | `string` | Credential delivery scheme: `"bearer"`, `"x-api-key"`, or `"none"`. |
| `key` | `string` | Literal API key string. In production, literal keys are saved to `secrets.json` and scrubbed from `models.json`. |
| `key_env` | `string` | Name of environment variable containing the secret (e.g. `"ANTHROPIC_API_KEY"`). Runtime reads `os.Getenv(key_env)`. (Alias: `api_key_env`). |
| `key_ref` | `string` | Key name stored in `secrets.json`. Automatically assigned to the provider ID if omitted. |
| `header_name` | `string` | Optional header name override to send credentials to a non-standard header while preserving value formatting. |

#### Scheme Rules

- `none`: Key fields (`key`, `key_env`, `key_ref`) must be empty. Useful for local models (Ollama, local vLLM).
- `bearer`: Emits `Authorization: Bearer <key>`.
- `x-api-key`: Emits `x-api-key: <key>`.
- **Exclusivity**: Set either `key` or `key_env`, not both.

#### Secrets Storage (`secrets.json`)

To prevent exposing API tokens in version control or backups:
- Secrets are persisted to `secrets.json` alongside `models.json`.
- POSIX permissions are strictly enforced to `0600` (read/write by owner only).
- The catalog document `models.json` retains `key_ref: "<id>"` with `key: ""`.

---

### Headers & Header Policies

#### Custom Static Headers (`headers`)

A map of headers always appended to upstream requests:

```json
{
  "headers": {
    "User-Agent": "curl/8.4.0",
    "anthropic-version": "2023-06-01",
    "HTTP-Referer": "https://example.com"
  }
}
```

- Header names are automatically canonicalized into MIME header case (e.g. `X-Custom-Header`).
- **Forbidden Upstream Headers**: Hop-by-hop headers (`Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade`), transport headers (`Host`, `Content-Length`, `Proxy-Connection`), and authentication headers (`Authorization`, `X-Api-Key`, `Cookie`, `Set-Cookie`). Credentials must be configured through `auth`.

#### Forwarded Client Headers (`forward_headers`)

An allow-list of client request headers forwarded through to the upstream:

```json
{
  "forward_headers": ["x-request-id", "cf-aig-metadata"]
}
```

- Client credentials (`Authorization`, `X-Api-Key`, `Cookie`) and transport headers cannot be forwarded.

---

### Model Mappings & Wildcard Routing

Each provider defines served models in the `models` array:

```json
{
  "models": [
    {
      "alias": "claude-3-5-sonnet",
      "upstream_model": "claude-3-5-sonnet-20241022",
      "hidden": false,
      "fallbacks": ["claude-3-opus"],
      "fallback_statuses": [429, 500, 502, 503, 504]
    }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `alias` | `string` | The identifier client requests specify in `"model"`. Supports wildcard trailing `/*`. |
| `upstream_model` | `string` | The model name sent to the upstream provider. |
| `hidden` | `bool` | When `true`, hides the model from catalog discovery `/v1/models` without disabling direct routing. |
| `fallbacks` | `[]string` | Candidate model aliases tried in order when an upstream call fails. |
| `fallback_statuses`| `[]int` | Upstream HTTP status codes that trigger fallback failover. |
| `allow_shadow` | `bool` | Allows mapping aliases that would otherwise shadow protected system models. |

#### Wildcard Matching & Prefix-Stripping

Wildcard mappings allow routing an entire model namespace with a single definition:

1. **Prefix-Stripping Wildcard**:
   - `alias`: `"openrouter/*"`
   - `upstream_model`: `"*"` (or `""`)
   - A request for `openrouter/anthropic/claude-3.5-sonnet` extracts the suffix after `openrouter/` and sends `anthropic/claude-3.5-sonnet` upstream.
2. **Mapped-Prefix Wildcard**:
   - `alias`: `"local/*"`
   - `upstream_model`: `"vllm/*"`
   - A request for `local/llama-3` sends `vllm/llama-3` upstream.
3. **Fixed Upstream Target**:
   - `alias`: `"sandbox/*"`
   - `upstream_model`: `"mock-eval-v1"`
   - Any request starting with `sandbox/` is routed to `mock-eval-v1`.

#### Resolution Precedence

When resolving an incoming model name:
1. **Exact Alias Match**: `models[i].alias == client_model`
2. **Longest Prefix Wildcard Match**: Wildcards are sorted by descending prefix length; the longest matching prefix wins.
3. **Bare Upstream Name Match**: Direct upstream model identifier if bare routing is enabled.
4. **Fallthrough**: Default system routing.

---

### Fallback Chains

Fallback chains guarantee high availability when upstream providers experience outages, capacity limits, or rate limits:

- **Ordered Failover**: When a model invocation fails with a qualifying status, the dispatcher resolves `fallbacks[0]`, then `fallbacks[1]`, etc.
- **Status Filter (`fallback_statuses`)**: If specified, failover triggers only on matching HTTP status codes (e.g. `[429, 500, 502, 503]`). If omitted, all upstream errors qualify.
- **TTFB Boundary (Stream Safety)**:
  - For non-streaming requests, fallbacks are attempted after any complete upstream failure.
  - For streaming requests, fallbacks can only trigger **before** the first response header / token chunk is emitted to the client (Time To First Byte). Once the client stream begins, the error is transmitted directly to preserve protocol framing.
- **Multi-Hop & Cycle Protection**: Transitive fallbacks are flattened with visited-node tracking to prevent infinite loops.

---

### Timeouts

Granular timeouts protect system resources against unresponsive upstreams:

```json
{
  "timeouts": {
    "connect": "5s",
    "tls": "5s",
    "response_header": "30s",
    "idle": "60s",
    "total": "120s"
  }
}
```

| Field | JSON / Duration | Description |
|---|---|---|
| `connect` | `string` or `int64` | TCP connect timeout (e.g. `"5s"` or `5000000000` ns). |
| `tls` | `string` or `int64` | TLS handshake timeout (e.g. `"5s"`). |
| `response_header` | `string` or `int64` | Maximum duration waiting for HTTP response headers from upstream. |
| `idle` | `string` or `int64` | Maximum silence between stream chunks. Catches stalled SSE connections. |
| `total` | `string` or `int64` | Maximum total duration for non-streaming requests. |

Duration values accept Go duration strings (e.g. `"5s"`, `"1m"`, `"300ms"`) or integer nanoseconds.

---

### Concurrency Limits

Prevent upstream rate limit penalties and resource exhaustion using provider-level concurrency bounds:

```json
{
  "max_concurrency": 25,
  "queue_timeout": "5s"
}
```

| Field | Type | Description |
|---|---|---|
| `max_concurrency` | `int` | Maximum simultaneous in-flight requests to this provider. `<= 0` indicates unbounded concurrency. |
| `queue_timeout` | `string` or `int64` | Maximum time a request waits in the queue for an available concurrency token. Values `<= 0` immediately reject with `HTTP 503 Overloaded`. |

---

### TLS Profiles

The `tls` setting specifies client TLS fingerprint impersonation:

| Profile | Description |
|---|---|
| `default` | Standard Go `crypto/tls` client. Recommended for direct public APIs (OpenAI, Anthropic, Ollama). |
| `chrome` | Impersonates Google Chrome TLS Client Hello fingerprint using uTLS. Essential when communicating through Cloudflare or anti-bot reverse proxies that block default Go HTTP clients. |

---

### SSRF & Upstream Network Security

The gateway enforces stringent Server-Side Request Forgery (SSRF) defenses:

#### Target Restrictions

- **Cloud Metadata Protection**: Targets pointing to AWS/GCP/Azure link-local metadata services (`169.254.169.254`, `[fe80::]/10`, `[fd00:ec2::254]`, `metadata.google.internal`) are blocked by default.
- **DNS Rebinding Prevention**: Upstream hostnames are resolved and checked before establishing network connections. The resolved IP is pinned for the connection dialer, and a post-resolution socket control hook verifies the destination.
- **Cross-Origin Redirect Blocking**: HTTP redirects are validated; redirects switching origins or attempting to access blocked IP spaces are aborted.

#### Policy Modes & CLI Flags

The gateway can be started with specific URL policy flags:

| Flag | Policy Setting | Effect |
|---|---|---|
| *(Default)* | Default URL Policy | Allows local loopback and RFC 1918 private LAN addresses (for Ollama, vLLM, local dev). Blocks cloud metadata and link-local targets. |
| `--deny-private-upstreams` | `DenyPrivate: true` | Hardened production mode. Strictly denies all private IP ranges (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), loopback (`127.0.0.1`, `localhost`, `::1`), unspecified, and multicast. |
| `--allow-unsafe-upstreams` | `AllowUnsafe: true` | Disables metadata blocking. (Only recommended for specialized testing). |

> [!NOTE]
> Trusted proxies (`proxy_url`) running on localhost or private networks (e.g. corporate forward proxies) are permitted under `--deny-private-upstreams` because the final destination target is verified and pinned separately.

---

### Extensions & Extra Body

#### Endpoint Path Overrides (`endpoint_path`)

Allows remapping the standard protocol path suffix:

```json
{
  "endpoint_path": {
    "openai-chat": "/api/v2/chat"
  }
}
```

Path must start with a single `/`, and cannot contain query parameters, fragment identifiers, or `..` path traversals.

#### Extra Body Injection (`extra_body`)

Injects static top-level JSON fields into every outbound request body:

```json
{
  "extra_body": {
    "user": "gateway-system",
    "metadata": {
      "cluster": "prod-us-east"
    }
  }
}
```

> [!WARNING]
> Overriding `model` or `stream` via `extra_body` is strictly forbidden and rejected at configuration validation time.

---

## Practical Configuration Examples

All examples below use placeholder credentials (never commit real secrets).

### 1. OpenRouter (Prefix-Stripping Wildcard & Fallbacks)

Configures OpenRouter as an OpenAI-compatible provider. Strips the `openrouter/` prefix from incoming model names and sets up automatic fallback.

```json
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
  "headers": {
    "HTTP-Referer": "https://github.com/minhhhduc/gnvipi",
    "X-Title": "Universal Gateway"
  },
  "models": [
    {
      "alias": "openrouter/*",
      "upstream_model": "*",
      "fallbacks": ["openrouter/anthropic/claude-3-haiku"],
      "fallback_statuses": [429, 500, 502, 503, 504]
    },
    {
      "alias": "openrouter/anthropic/claude-3-haiku",
      "upstream_model": "anthropic/claude-3-haiku"
    }
  ],
  "timeouts": {
    "connect": "5s",
    "idle": "30s",
    "total": "120s"
  }
}
```

---

### 2. Anthropic Direct

Direct Anthropic Messages integration with Chrome TLS fingerprint impersonation to guarantee reliable handshakes.

```json
{
  "id": "anthropic-direct",
  "name": "Anthropic Direct",
  "base_url": "https://api.anthropic.com",
  "protocol": "anthropic-messages",
  "enabled": true,
  "tls": "chrome",
  "auth": {
    "scheme": "x-api-key",
    "key_env": "ANTHROPIC_API_KEY"
  },
  "headers": {
    "User-Agent": "curl/8.4.0",
    "anthropic-version": "2023-06-01"
  },
  "models": [
    {
      "alias": "claude-3-5-sonnet",
      "upstream_model": "claude-3-5-sonnet-20241022",
      "fallbacks": ["claude-3-haiku"],
      "fallback_statuses": [429, 529, 500, 503]
    },
    {
      "alias": "claude-3-haiku",
      "upstream_model": "claude-3-haiku-20240307"
    }
  ],
  "timeouts": {
    "connect": "5s",
    "response_header": "20s",
    "idle": "60s",
    "total": "180s"
  }
}
```

YAML equivalent:

```yaml
id: anthropic-direct
name: Anthropic Direct
base_url: https://api.anthropic.com
protocol: anthropic-messages
enabled: true
tls: chrome
auth:
  scheme: x-api-key
  key_env: ANTHROPIC_API_KEY
headers:
  User-Agent: curl/8.4.0
  anthropic-version: "2023-06-01"
models:
  - alias: claude-3-5-sonnet
    upstream_model: claude-3-5-sonnet-20241022
    fallbacks:
      - claude-3-haiku
    fallback_statuses:
      - 429
      - 529
      - 500
      - 503
  - alias: claude-3-haiku
    upstream_model: claude-3-haiku-20240307
timeouts:
  connect: 5s
  response_header: 20s
  idle: 60s
  total: 180s
```

---

### 3. OpenAI Direct (Concurrency Limits & Timeouts)

OpenAI provider with strict in-flight concurrency caps and queue buffering.

```json
{
  "id": "openai-direct",
  "name": "OpenAI Direct",
  "base_url": "https://api.openai.com/v1",
  "protocol": "openai-chat",
  "enabled": true,
  "auth": {
    "scheme": "bearer",
    "key_env": "OPENAI_API_KEY"
  },
  "models": [
    {
      "alias": "gpt-4o",
      "upstream_model": "gpt-4o",
      "fallbacks": ["gpt-4o-mini"],
      "fallback_statuses": [429, 500, 503]
    },
    {
      "alias": "gpt-4o-mini",
      "upstream_model": "gpt-4o-mini"
    }
  ],
  "max_concurrency": 50,
  "queue_timeout": "5s",
  "timeouts": {
    "connect": "3s",
    "idle": "45s",
    "total": "90s"
  }
}
```

---

### 4. Local Ollama (Unauthenticated Loopback)

Connects to a local Ollama server running on `localhost:11434`. Permitted under default SSRF policy.

```json
{
  "id": "local-ollama",
  "name": "Local Ollama",
  "base_url": "http://127.0.0.1:11434/v1",
  "protocol": "openai-chat",
  "enabled": true,
  "auth": {
    "scheme": "none"
  },
  "models": [
    {
      "alias": "ollama/llama3.2",
      "upstream_model": "llama3.2:latest"
    },
    {
      "alias": "ollama/qwen2.5-coder",
      "upstream_model": "qwen2.5-coder:7b"
    }
  ],
  "timeouts": {
    "connect": "2s",
    "idle": "60s",
    "total": "300s"
  }
}
```

---

### 5. Local vLLM Engine

Connects to a high-throughput vLLM cluster endpoint with high concurrency limits.

```json
{
  "id": "local-vllm",
  "name": "Local vLLM Server",
  "base_url": "http://127.0.0.1:8000/v1",
  "protocol": "openai-chat",
  "enabled": true,
  "auth": {
    "scheme": "bearer",
    "key": "sk-local-vllm-dev-token"
  },
  "models": [
    {
      "alias": "vllm/qwen2.5-72b",
      "upstream_model": "Qwen/Qwen2.5-72B-Instruct-AWQ"
    }
  ],
  "max_concurrency": 128,
  "queue_timeout": "10s",
  "timeouts": {
    "connect": "2s",
    "idle": "60s",
    "total": "240s"
  }
}
```

---

### 6. Anthropic-Compatible Gateway Proxy

Connects to an intermediate Anthropic proxy (e.g. Cloudflare AI Gateway) forwarding custom tracing headers and using an alternate credential header.

```json
{
  "id": "cf-anthropic-gateway",
  "name": "Cloudflare Anthropic Gateway",
  "base_url": "https://gateway.ai.cloudflare.com/v1/my-account/my-gateway/anthropic",
  "protocol": "anthropic-messages",
  "enabled": true,
  "auth": {
    "scheme": "x-api-key",
    "key_env": "ANTHROPIC_API_KEY",
    "header_name": "x-api-key"
  },
  "forward_headers": [
    "cf-aig-metadata",
    "cf-aig-cache-key",
    "x-request-id"
  ],
  "headers": {
    "anthropic-version": "2023-06-01"
  },
  "models": [
    {
      "alias": "proxy/claude-3-5-sonnet",
      "upstream_model": "claude-3-5-sonnet-20241022"
    }
  ],
  "timeouts": {
    "connect": "5s",
    "idle": "45s",
    "total": "120s"
  }
}
```

---

### 7. Complete `models.json` Catalog Document

An example of a full `models.json` file containing multiple v2 providers:

```json
{
  "schema_version": 2,
  "providers_v2": [
    {
      "id": "openai-main",
      "name": "OpenAI Main",
      "base_url": "https://api.openai.com/v1",
      "protocol": "openai-chat",
      "enabled": true,
      "auth": {
        "scheme": "bearer",
        "key_env": "OPENAI_API_KEY"
      },
      "models": [
        {
          "alias": "gpt-4o",
          "upstream_model": "gpt-4o"
        }
      ]
    },
    {
      "id": "ollama-dev",
      "name": "Local Ollama",
      "base_url": "http://127.0.0.1:11434/v1",
      "protocol": "openai-chat",
      "enabled": true,
      "auth": {
        "scheme": "none"
      },
      "models": [
        {
          "alias": "local/deepseek",
          "upstream_model": "deepseek-r1:latest"
        }
      ]
    }
  ]
}
```

---

## Migration Guide: Legacy to V2

### Legacy Format Overview

In gateway versions prior to v2, providers were stored as a flat array in `models.json` under `"providers"` with no schema version or `schema_version: 1`:

- Each entry represented a **single model**. Multiple models hosted on the same endpoint had duplicate `name`, `base_url`, and `api_key` entries.
- Wire protocol was determined by two boolean flags: `messages_only` and `responses_only`.
- API keys were stored directly inline in `models.json`.

---

### Field & Protocol Mappings

| Legacy Field | V2 Mapping | Notes |
|---|---|---|
| `name` | `name`, `id` | V2 generates an `id` from `name` (with numerical suffixes `-2`, `-3` for duplicates). |
| `base_url` | `base_url` | Base URL rules apply (http/https, no userinfo/query). |
| `api_key` / `api_key_env` | `auth.key` / `auth.key_env` | Mapped to `Auth` struct; literal keys migrate to `secrets.json`. |
| `model` | `models[i].upstream_model` | Upstream model identifier. |
| `alias` | `models[i].alias` | Client-facing alias (defaults to `name/model` if alias was empty). |
| `messages_only: true` | `protocol: "anthropic-messages"` | Also injects default Anthropic headers (`x-api-key`, Chrome TLS, curl User-Agent). |
| `responses_only: true` | `protocol: "openai-responses"` | OpenAI responses wire protocol. |
| *(Neither flag set)* | `protocol: "openai-chat"` | Default wire protocol. |
| *(Both flags set)* | **INVALID** | Rejected by validator: `messages_only and responses_only are mutually exclusive`. |

---

### Endpoint Grouping & Model Consolidation

When the gateway loads a legacy `models.json`:
1. **Grouping**: Legacy entries sharing identical `(name, base_url, api_key, protocol)` coordinates are consolidated into a single `Provider` instance.
2. **Models Array**: Each legacy entry becomes one item in the provider's `models` slice.
3. **Precedence**: `legacy_position` preserves the exact original flat routing priority.
4. **Collision Resolution**: If two entries share a name but differ in URL or protocol, the secondary provider is assigned a collision-free ID (e.g. `anthropic-2`).

---

### Credentials Separation (`secrets.json`)

On the first save in v2 mode:
1. Literal API keys are written to `secrets.json` with permissions `0600`.
2. The key in `models.json` is replaced by `key_ref: "<provider_id>"` with an empty `key`.
3. If an environment variable was used (`key_env` or legacy `api_key_env`), it is preserved directly in `models.json` under `auth.key_env`.

---

### Side-by-Side Migration Example

#### Before: Legacy `models.json`

```json
{
  "schema_version": 1,
  "providers": [
    {
      "name": "anthropic",
      "base_url": "https://api.anthropic.com",
      "api_key_env": "ANTHROPIC_API_KEY",
      "model": "claude-3-5-sonnet-20241022",
      "alias": "claude-3.5-sonnet",
      "messages_only": true
    },
    {
      "name": "anthropic",
      "base_url": "https://api.anthropic.com",
      "api_key_env": "ANTHROPIC_API_KEY",
      "model": "claude-3-haiku-20240307",
      "alias": "claude-3-haiku",
      "messages_only": true
    }
  ]
}
```

#### After: V2 `models.json`

```json
{
  "schema_version": 2,
  "providers_v2": [
    {
      "id": "anthropic",
      "name": "anthropic",
      "base_url": "https://api.anthropic.com",
      "protocol": "anthropic-messages",
      "enabled": true,
      "tls": "chrome",
      "auth": {
        "scheme": "x-api-key",
        "key_env": "ANTHROPIC_API_KEY"
      },
      "headers": {
        "User-Agent": "curl/8.4.0"
      },
      "models": [
        {
          "alias": "claude-3.5-sonnet",
          "upstream_model": "claude-3-5-sonnet-20241022"
        },
        {
          "alias": "claude-3-haiku",
          "upstream_model": "claude-3-haiku-20240307"
        }
      ]
    }
  ]
}
```
