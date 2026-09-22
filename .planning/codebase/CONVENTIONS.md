# Coding Conventions

**Analysis Date:** 2026-09-23

## Naming Patterns

**Files:**
- Go files use lowercase names describing the concern, with `_test.go` for tests; examples include `internal/provider/nvidia/normalize.go`, `internal/captcha/pool_backoff_test.go`, and `cmd/serve/passthrough.go`.
- Package names are short lowercase names (`captcha`, `nvidia`, `gnvipi`, `models`); commands under `cmd/*` use `package main`.

**Functions:**
- Exported functions and types use PascalCase (`NewPool`, `NormalizeRequestBody`, `ChatResponse`); unexported helpers use lowerCamelCase (`safeExtract`, `parseError`, `rewriteJSONModel`).
- Test names follow `Test<Subject><Behavior>` and often encode the scenario (`TestPoolTokenLease_CommitAllowsRefill`, `TestBuildRequestUnknownModel`).

**Variables:**
- Local variables and parameters use concise lowerCamelCase (`ctx`, `req`, `raw`, `got`, `want`); initialisms remain capitalized in exported names (`HTTPClient`, `URLProvider`, `SSE`).
- Boolean names read as predicates (`changed`, `streamed`, `MessagesOnly`, `ResponsesOnly`).

**Types:**
- Structs model request/response and domain concepts (`ChatRequest`, `StreamChunk`, `BrowserConfig`, `PoolConfig`); option functions use `type Option func(*Client)`.
- JSON fields use explicit `json` tags; nullable upstream values use pointers where absence matters (`Usage`, `PromptTokensDetails`, `*bool`).

## Code Style

**Formatting:**
- Use `gofmt` and `goimports`; both are enforced by `.golangci.yml`.
- Keep comments adjacent to exported declarations and document behavior, defaults, upstream quirks, and lifecycle constraints. Representative examples are `internal/gnvipi/client.go` and `internal/captcha/pool.go`.

**Linting:**
- `.golangci.yml` enables `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`, `bodyclose`, `gocritic`, `nilerr`, `noctx`, `unconvert`, `unparam`, `misspell`, `usestdlibvars`, `errorlint`, and `copyloopvar`.
- `gocritic` diagnostic/performance/style tags, US spelling, checked blank assignments, and a five-minute lint timeout are project defaults. Preserve checked errors, close response bodies, and pass contexts through I/O.

## Import Organization

**Order:**
1. Standard library imports (`context`, `encoding/json`, `net/http`, `testing`).
2. A blank line, then project and third-party imports grouped by package ownership.
3. Blank-line-separated side-effect imports are used when registering external translators, as in `internal/provider/nvidia/executor_test.go`.

**Path Aliases:**
- No custom aliases are configured. Use `github.com/minhhhduc/gnvipi/...`; aliases are reserved for deliberate name collisions (`coreauth`, `clipexec`, `sdktranslator`).

## Error Handling

**Patterns:**
- Return errors from lower layers and add operation context with `%w`, for example `internal/gnvipi/client.go` wraps request, transport, read, and decode failures as `gnvipi: ...`.
- Use `errors.Is`/`errors.As` where callers need classification; `internal/provider/nvidia/executor_test.go` verifies `*coreauth.Error` and wrapped transport behavior.
- Validate at boundaries and return descriptive errors naming the bad model, endpoint, or state. HTTP handlers translate failures into status responses rather than panicking.
- Explicitly check or intentionally discard every error to satisfy `errcheck`; blank assignments are still lint-visible.

## Logging

**Framework:** `log` from the standard library for operational background work; handler/package code otherwise returns errors or records request statistics.

**Patterns:**
- Log only actionable lifecycle/recovery events, with rate limiting for recurring background failures (`internal/captcha/pool.go`).
- Do not log tokens or credentials. Tests use captured responses and counters rather than asserting log text.

## Comments

**When to Comment:**
- Explain upstream protocol quirks, concurrency invariants, compatibility decisions, and workarounds next to the invariant they protect (`internal/captcha/pool.go`, `cmd/serve/passthrough.go`).
- Use Given/When/Then comments in long behavioral tests where they clarify event sequencing (`internal/captcha/group_test.go`, `internal/provider/nvidia/executor_test.go`).

**JSDoc/TSDoc:**
- Not applicable. Go doc comments are used for exported APIs and package-level usage, especially `internal/gnvipi/client.go` and command entrypoints under `cmd/`.

## Function Design

**Size:** Keep public methods focused on one request/lifecycle operation; split parsing, normalization, routing, and transport into helpers (`buildRequest`, `parseError`, `NormalizeRequestBody`). Larger handlers are acceptable when they own one complete middleware/page concern.

**Parameters:** Pass `context.Context` first for cancellable work. Use config/options structs for multiple settings (`captcha.PoolConfig`, `nvidia.Options`) and callbacks/interfaces for transport seams.

**Return Values:** Return `(value, error)` for fallible operations; return pointers for optional response/domain objects. Streaming APIs return errors and deliver data through callbacks/channels (`StreamChat`, executor streams). Preserve zero values for absent JSON fields.

## Module Design

**Exports:** Export only public client/domain APIs and command-facing constructors; keep protocol parsing, synchronization, and routing helpers unexported within their package.

**Barrel Files:** Not used. Add implementation to the owning package directory and import that package directly.

---

*Convention analysis: 2026-09-23*
