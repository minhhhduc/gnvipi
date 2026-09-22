# Testing Patterns

**Analysis Date:** 2026-09-23

## Test Framework

**Runner:**
- Go's standard `testing` package; tests are distributed beside implementation across `internal/*` and `cmd/*`.
- No separate test configuration file or external assertion framework is present.

**Assertion Library:**
- Standard `testing.T` methods (`Fatal`, `Fatalf`, `Error`, `Errorf`, `Helper`, `Cleanup`, `TempDir`, `Setenv`). Assertions use explicit `got`/`want` comparisons.

**Run Commands:**
```bash
go test ./...                 # Run the complete suite
go test -race -vet=all ./... # CI test command across packages
go test ./internal/captcha    # Focus a package while iterating
golangci-lint run --timeout=5m # CI lint/format checks
```

CI also runs `govulncheck` and builds `./cmd/serve`; see `.github/workflows/ci.yml`.

## Test File Organization

**Location:**
- Tests are co-located with implementation in the same package directory, such as `internal/captcha/pool_test.go` beside `pool.go` and `cmd/serve/serve_test.go` beside server code.

**Naming:**
- Files use the implementation concern plus `_test.go` (`normalize_test.go`, `executor_test.go`, `responses_anthropic_test.go`).
- Tests use `Test...`; no benchmark or fuzz suite is detected.

**Structure:**
```
internal/<package>/*_test.go
cmd/<command>/*_test.go
```

## Test Structure

**Suite Organization:**
```go
func TestNormalizeRequestBodyMapsReasoningEffortByModel(t *testing.T) {
    cases := []struct { name, model, in, key string; want any }{ /* cases */ }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            out, err := NormalizeRequestBody(/* input */)
            if err != nil { t.Fatal(err) }
            if got := /* field */; got != tc.want { t.Fatalf("got %#v want %#v", got, tc.want) }
        })
    }
}
```

**Patterns:**
- Prefer table-driven tests and subtests for model/provider/format matrices (`internal/provider/nvidia/normalize_test.go`, `internal/provider/nvidia/executor_test.go`).
- Use `t.Helper()` in reusable setup/assertion helpers and `t.Cleanup()` for pools, servers, registry entries, global clients, and temporary files.
- Use `t.TempDir()` for filesystem tests and `t.Setenv()` for isolated environment configuration (`cmd/serve/serve_test.go`, `internal/captcha/config_test.go`).
- Assert error type/status/message as well as success payloads; name expected values in failure output with `got`/`want`.

## Mocking

**Framework:** Standard-library seams only; there is no mocking dependency.

**Patterns:**
```go
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
```

- Inject an `http.Client` with a custom `RoundTripper` for transport failures and request assertions (`cmd/serve/passthrough_test.go`, `internal/provider/nvidia/executor_test.go`).
- Use `httptest.NewServer` for realistic HTTP/SSE responses, headers, body capture, and retry behavior (`internal/provider/nvidia/executor_test.go`).
- Use function fields/config callbacks for browser factories, URL providers, and upstream endpoint selection (`internal/captcha/group_test.go`).
- Use channels, atomics, and controlled releases to make concurrency/cancellation deterministic; do not sleep unless polling a lifecycle condition.

**What to Mock:**
- External HTTP, browser factories, captcha extraction, endpoint selection, and global registries when testing routing or lifecycle logic.

**What NOT to Mock:**
- Keep pure normalization/parsing/formatting logic real (`internal/provider/nvidia/*_test.go`); exercise JSON and SSE wire shapes directly.
- Avoid live NVIDIA/Chrome calls in unit tests. Operational commands under `cmd/cacheprobe`, `cmd/captchaopt`, `cmd/hangbench`, and `cmd/streambench` are manual/live experiments, not automated tests.

## Fixtures and Factories

**Test Data:**
```go
const fixtureStreamDelta = `{"choices":[{"delta":{"content":"done"}}]}`
```

- Use inline JSON/SSE fixtures for protocol cases (`internal/gnvipi/client_test.go`, `internal/provider/nvidia/coalesce_test.go`).
- Use small constructors such as `newStaticPool`, `newExecutorLeasePool`, and `newTrackedBrowser` to create controlled state; register cleanup inside the helper.

**Location:**
- Fixtures are local constants or helper constructors in the owning `_test.go` file. No shared fixture directory is detected.

## Coverage

**Requirements:** No numeric coverage target is enforced. CI requires all packages to pass `go test -race -vet=all ./...`.

**View Coverage:**
```bash
go test ./... -cover
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
```

## Test Types

**Unit Tests:**
- Pure functions, JSON normalization, model registry invariants, stats, token accounting, and error classification are tested directly.

**Integration Tests:**
- In-process HTTP tests use `httptest` servers and Gin routers to verify middleware, request translation, SSE, retries, and response forwarding. CI sets `SKIP_INTEGRATION_TESTS=1`, but no automated test currently checks that variable.

**E2E Tests:**
- Not used in the automated suite. Chrome/NVIDIA live behavior is exercised by manual benchmark/probe commands and scripts.

## Common Patterns

**Async Testing:**
```go
done := make(chan error, 1)
go func() { done <- operation(ctx) }()
select {
case err := <-done:
    if err != nil { t.Fatal(err) }
case <-time.After(2 * time.Second):
    t.Fatal("operation hung")
}
```

- Prefer context cancellation and channel barriers; use `sync/atomic` for cross-goroutine counters. Concurrency-heavy tests in `internal/captcha/pool_test.go`, `internal/captcha/group_test.go`, and `internal/provider/nvidia/executor_test.go` are designed to run under `-race`.

**Error Testing:**
```go
var authErr *coreauth.Error
if !errors.As(err, &authErr) { t.Fatalf("error type %T", err) }
if authErr.HTTPStatus != http.StatusServiceUnavailable { t.Fatal("...") }
```

- Check `errors.Is`/`errors.As` for wrapped errors, then verify stable status/type/message details. For HTTP failures, assert both response code and parsed body/error classification.

---

*Testing analysis: 2026-09-23*
