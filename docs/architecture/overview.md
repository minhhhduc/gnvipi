# Architecture overview

The gateway is a Go service with two halves that share one process:

- **CLIProxyAPI** (embedded, `github.com/router-for-me/CLIProxyAPI/v7`) serves the
  OpenAI- and Anthropic-compatible API surface and owns provider authentication,
  routing and translation for the providers it knows.
- **gnvipi** adds the NVIDIA captcha-backed provider, the local model manager and
  the admin pages, and forwards custom OpenAI-compatible endpoints.

The universal gateway (`internal/gateway/*`) is the replacement core being built
on top of this; see [universal-llm-gateway-roadmap.md](universal-llm-gateway-roadmap.md)
for its milestones and dependency graph. Do not design new work around the
legacy middleware chain without reading that roadmap first.

## Where code lives

| Path | Purpose |
|---|---|
| `cmd/serve` | The gateway binary: startup, wiring, middleware chain, admin routes. |
| `internal/gnvipi` | Client for the upstream API the gateway proxies. |
| `internal/provider/nvidia` | NVIDIA provider: request normalization, execution, stats. |
| `internal/captcha` | Chromium/CDP pool that mints CAPTCHA tokens. |
| `internal/models` | Embedded catalog of playground models. |
| `internal/gateway/ir` | Protocol-neutral request/response/stream representation. |
| `internal/gateway/sse` | Spec-compliant SSE reader and writer. |
| `internal/gateway/gwerr` | Normalized errors and their per-protocol encodings. |
| `internal/gateway/adapter` | Inbound/outbound adapter interfaces and registry. |
| `internal/gateway/provider` | Typed provider configuration schema v2. |
| `internal/gateway/gatewaytest` | Fake upstreams for adapter and relay tests. |

## Rules that keep it maintainable

- **One owner per concern.** Parsing, normalization, routing and transport stay in
  separate helpers; a handler orchestrates, it does not implement them.
- **The IR is the only cross-protocol contract.** Adapters convert to and from
  `internal/gateway/ir`; they never import each other.
- **Register explicitly.** Adapters are registered from one wiring point in
  `cmd/serve`, never from `init()`, so packages can be added in parallel.
- **No credential in a log, an error body or a catalog file.** See the security
  issues (#36, #37, #51) for the current state of that work.
- **Don't mutate a global registry outside its reconciler.** `modelPrefs` changes
  go through its locked setters and notify callbacks; see `cmd/serve/admin.go`.