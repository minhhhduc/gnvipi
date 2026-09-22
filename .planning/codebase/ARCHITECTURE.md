<!-- refreshed: 2026-09-23 -->
# Architecture

**Analysis Date:** 2026-09-23

## System Overview

```text
┌─────────────────────────────────────────────────────────────┐
│ HTTP gateway / protocol translation                         │
│ `cmd/serve/main.go` + embedded CLIProxyAPI + Gin            │
│ OpenAI Chat, OpenAI Responses, Anthropic Messages, admin    │
└───────────────┬──────────────────────┬──────────────────────┘
                │ NVIDIA model route   │ custom OpenAI routes
                ▼                      ▼
┌──────────────────────────┐  ┌──────────────────────────────┐
│ NVIDIA executor           │  │ CLIProxyAPI compat executor   │
│ `internal/provider/nvidia/`│  │ configured by `cmd/serve`     │
└──────────────┬───────────┘  └──────────────┬───────────────┘
               │                              │
               ▼                              ▼
┌──────────────────────────┐  ┌──────────────────────────────┐
│ `internal/captcha/`       │  │ External custom providers      │
│ Chrome group + token pool │  │ and their configured base URLs│
└──────────────┬───────────┘  └──────────────────────────────┘
               ▼
┌─────────────────────────────────────────────────────────────┐
│ NVIDIA Playground predict API; model metadata from            │
│ `internal/models/registry.go` and embedded JSON               │
└─────────────────────────────────────────────────────────────┘
```

## Component Responsibilities

| Component | Responsibility | File |
|-----------|----------------|------|
| Gateway bootstrap | Parses flags, builds CLIProxyAPI config, installs middleware/routes/hooks, starts listener | `cmd/serve/main.go` |
| Admin surface | Persists model masks/deletions/providers and serves dashboard/model/Chrome controls | `cmd/serve/admin.go` |
| Request adapters | Handles custom-provider passthrough, Responses/Anthropic compatibility, logging/stat capture | `cmd/serve/passthrough.go`, `cmd/serve/responses_anthropic.go` |
| NVIDIA executor | Implements CLIProxyAPI executor contract, validates/translates payloads, acquires captcha, calls upstream, translates responses | `internal/provider/nvidia/executor.go` |
| NVIDIA client | Standalone synchronous/SSE client for direct Playground use | `internal/gnvipi/client.go` |
| Model registry | Maps public model IDs to Playground slugs, namespaces, function IDs, and capabilities | `internal/models/registry.go` |
| Captcha browser layer | Runs/recycles headless Chrome workers and extracts hCaptcha tokens | `internal/captcha/browser.go`, `internal/captcha/group.go` |
| Captcha pool | Buffers expiring tokens, leases them to requests, refills concurrently, reports counters | `internal/captcha/pool.go` |

## Pattern Overview

**Overall:** Adapter/composition architecture around an embedded third-party gateway.

**Key Characteristics:**
- `cmd/serve` composes the transport and routing layer; domain-specific upstream behavior lives in internal packages.
- CLIProxyAPI performs inbound format translation and provider dispatch through its executor interface.
- NVIDIA requests use a registry-driven adapter with shared HTTP transport, captcha leasing, bounded in-flight concurrency, and SSE normalization/coalescing.
- Mutable catalog/provider preferences are persisted locally and rebound into the gateway through config rewrites and model/auth hooks.

## Layers

**Gateway composition:**
- Purpose: Create the service, middleware chain, routes, hooks, and lifecycle.
- Location: `cmd/serve/main.go`
- Contains: Flag parsing, config generation, model catalog handlers, health/admin route registration, startup/shutdown orchestration.
- Depends on: `internal/captcha`, `internal/models`, `internal/provider/nvidia`, CLIProxyAPI SDK, Gin.
- Used by: `go run ./cmd/serve` and the Windows helper scripts.

**Protocol and provider middleware:**
- Purpose: Route custom providers and repair/observe protocol-specific requests.
- Location: `cmd/serve/passthrough.go`, `cmd/serve/responses_anthropic.go`
- Contains: OpenAI/Responses/Anthropic passthrough, tool-result sanitization, request logging and response tees.
- Depends on: Gin context, model preferences, CLIProxyAPI translators.
- Used by: Middleware installed in the `WithServerOptions` builder chain in `cmd/serve/main.go`.

**NVIDIA provider:**
- Purpose: Adapt CLIProxyAPI executor calls to NVIDIA Playground's predict API.
- Location: `internal/provider/nvidia/executor.go`
- Contains: Payload conversion/normalization, model lookup, captcha retry, stream execution, usage/stat recording.
- Depends on: `internal/models`, `internal/captcha`, `internal/gnvipi`, CLIProxyAPI executor/translator APIs.
- Used by: The registered `nvidia` executor in `cmd/serve/main.go`.

**Captcha infrastructure:**
- Purpose: Produce and safely distribute short-lived hCaptcha tokens.
- Location: `internal/captcha/`
- Contains: `Browser`, `BrowserGroup`, `Pool`, URL rotation, harness and champion selection.
- Depends on: chromedp and NVIDIA Playground pages.
- Used by: `internal/provider/nvidia.Executor` and `/admin/chromes` controls.

**Static model metadata:**
- Purpose: Resolve public model IDs into external endpoint invocation details.
- Location: `internal/models/registry.go`, `internal/models/playground_models.default.json`
- Contains: Embedded registry, model capabilities, endpoint/function-ID derivation, lookup errors.
- Depends on: Embedded JSON only.
- Used by: NVIDIA executor, catalog generation, standalone `internal/gnvipi.Client`.

## Data Flow

### Primary NVIDIA Request Path

1. `cmd/serve/main.go:222` builds CLIProxyAPI with middleware and a Gin engine configurator.
2. CLIProxyAPI translates `/v1/chat/completions`, `/v1/responses`, or `/v1/messages` into an executor request; `cmd/serve/main.go:227` orders passthrough and alias middleware.
3. `internal/provider/nvidia/executor.go:160` (`ExecuteStream`) or `:122` (`Execute`) calls `preparePayload`, translating and normalizing to OpenAI-style NVIDIA JSON.
4. `internal/provider/nvidia/executor.go:278` resolves the model using `internal/models.Lookup`.
5. `internal/provider/nvidia/executor.go:309` acquires a captcha token (request header, one-shot flag, or `captcha.Pool`), acquires an in-flight slot, and POSTs to the model's predict endpoint.
6. Streaming responses are coalesced and translated back through CLIProxyAPI at `internal/provider/nvidia/executor.go:186-215`; non-stream responses are translated at `:145`.
7. `internal/provider/nvidia/stats.go` receives usage, latency, TTFB, stream, and error observations; `cmd/serve` request logging fills the custom-provider observation gap.

### Startup and Dynamic Catalog Flow

1. `cmd/serve/main.go:55` parses flags and loads `prefs` from the models file.
2. `buildConfig` creates a temporary CLIProxyAPI config/auth directory; `rewriteConfigYAML` writes admin-configured OpenAI compatibility providers.
3. `nvidia.NewExecutor` is registered with `coreauth.Manager`; `bindNvidiaRuntime` registers the current visible NVIDIA models.
4. `OnAfterStart` ensures local NVIDIA auth, binds models, writes the Claude cache, and starts reconciliation (`cmd/serve/main.go:210-218`).
5. Admin mask/provider changes invoke callbacks in `cmd/serve/admin.go`, rewrite config, rebind models, and refresh Claude model discovery.

### Captcha Flow

1. With `-auto`, `cmd/serve/main.go:345` selects/rotates Playground URLs and creates `captcha.BrowserGroup`.
2. `captcha.NewPool` starts refill workers and an expiry reaper over the browser group's `Extract`/`ExtractN` functions.
3. `Executor.resolveCaptcha` leases a token; `doPredict` commits the lease only after the request is ready and retries recognized captcha failures with a fresh token.
4. `/healthz` exposes pool counters and `/admin/chromes` exposes browser status/control.

**State Management:**
- Process state is held in executor, pool, browser-group, stats, and global model registry singletons.
- Model visibility/deletion/providers persist through `modelPrefs` in `cmd/serve/admin.go`.
- CLIProxyAPI's auth/config/model registries are reconciled after watcher changes by hooks in `cmd/serve/main.go`.

## Key Abstractions

**Provider executor:**
- Purpose: CLIProxyAPI's provider boundary for request execution and streaming.
- Examples: `internal/provider/nvidia/executor.go`, registration in `cmd/serve/main.go:192-202`.
- Pattern: Adapter implementing `PrepareRequest`, `Refresh`, `HttpRequest`, `Execute`, and `ExecuteStream`.

**Model registry:**
- Purpose: Stable public model ID to external invocation metadata.
- Examples: `internal/models/registry.go:42-173`.
- Pattern: Embedded immutable map plus capability lookup; callers use `Lookup` and handle `ErrUnknownModel`.

**Token lease:**
- Purpose: Ensure a pooled captcha token is either committed to an upstream attempt or returned.
- Examples: `internal/captcha/pool.go:42-79`, `internal/provider/nvidia/executor.go:380-391`.
- Pattern: `TakeLease` returns an explicit lease with `Commit`/`Release` semantics.

**Middleware chain:**
- Purpose: Handle custom providers, protocol edge cases, aliases, and request statistics around CLIProxyAPI.
- Examples: `cmd/serve/main.go:227-237`, `cmd/serve/passthrough.go`.
- Pattern: Order-sensitive Gin handlers call `c.Next`/abort and are configured centrally at service construction.

## Entry Points

**Gateway server:**
- Location: `cmd/serve/main.go:55`
- Triggers: `go run ./cmd/serve`, compiled `serve` binary, or `scripts/run.ps1`.
- Responsibilities: Full HTTP gateway, admin console, model catalog, health endpoint, captcha lifecycle.

**Standalone Playground example:**
- Location: `cmd/example/main.go:26`
- Triggers: `go run ./cmd/example`.
- Responsibilities: Demonstrates direct `internal/gnvipi.Client` usage.

**Operational benchmarks/probes:**
- Locations: `cmd/streambench/main.go`, `cmd/hangbench/main.go`, `cmd/probehard/main.go`, `cmd/captchaopt/main.go`, `cmd/cacheprobe/main.go`.
- Triggers: Explicit command execution.
- Responsibilities: Measure streaming, captcha, cache, and endpoint behavior without being part of server startup.

## Architectural Constraints

- **Threading:** Go goroutines handle the HTTP server, stream forwarding, captcha pool workers, and browser workers; shared mutable structures use mutexes/atomics/channels.
- **Global state:** `cmd/serve/admin.go` owns process-wide `prefs`, `deadModels`, and Chrome admin references; `internal/provider/nvidia/stats.go` owns global stats; CLIProxyAPI owns global model registry state.
- **External contract:** NVIDIA requires model-specific `nv-function-id`, `nv-captcha-token`, and predict URLs from `internal/models/registry.go`.
- **Lifecycle ordering:** The listener starts before Chrome/captcha warmup (`cmd/serve/main.go:330-343`) so admin/custom providers remain available when Playground startup is unavailable.
- **Config reload:** Custom provider changes are serialized as generated YAML and picked up by CLIProxyAPI's watcher; keep generated config compatible with `rewriteConfigYAML`.

## Anti-Patterns

### Bypassing the executor boundary

**What happens:** A route calls NVIDIA directly instead of going through `internal/provider/nvidia.Executor`.
**Why it's wrong:** It skips model lookup, captcha leasing/retry, in-flight limits, stream translation, and stats.
**Do this instead:** Route model traffic through CLIProxyAPI registration in `cmd/serve/main.go:192-202` and implement provider behavior in `internal/provider/nvidia/executor.go`.

### Mutating model registries without reconciliation

**What happens:** Catalog changes update only the admin response or only one CLIProxyAPI registry.
**Why it's wrong:** Routing and advertised models diverge, potentially selecting an absent or duplicate provider.
**Do this instead:** Use `prefs.onMaskChange`, `bindNvidiaRuntime`, and the auth/model hooks in `cmd/serve/main.go:674-800`.

## Error Handling

**Strategy:** Convert boundary failures into CLIProxyAPI `coreauth.Error` values with request-scoped, upstream, or availability HTTP statuses; preserve context cancellation.

**Patterns:**
- Validate JSON, tools, model IDs, and stream mode before upstream I/O in `internal/provider/nvidia/executor.go:232-287`.
- Retry only recognizable captcha failures; return 401/503/502 based on token, pool, cancellation, or upstream cause.
- Browser extraction retries in place once, then recycles the Chrome process in `internal/captcha/group.go`.
- Startup degrades gracefully when captcha is unavailable, retaining admin/custom provider service (`cmd/serve/main.go:381-414`).

## Cross-Cutting Concerns

**Logging:** Standard library `log.Printf` in server, executor, and captcha packages; request stats are persisted in an in-memory bounded event log.
**Validation:** JSON normalization and tool validation occur in `internal/provider/nvidia/executor.go`; model lookup is centralized in `internal/models/registry.go`.
**Authentication:** No inbound gateway API keys are enabled; NVIDIA uses request-scoped hCaptcha tokens, while custom providers use configured CLIProxyAPI API-key entries.

---

*Architecture analysis: 2026-09-23*
