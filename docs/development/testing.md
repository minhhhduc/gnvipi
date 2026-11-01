# Testing

Go's standard `testing` package, tests beside the code, no assertion library.
CI runs `go test -race -vet=all ./...` with `SKIP_INTEGRATION_TESTS=1` on ubuntu,
macos and windows.

## Commands

```bash
go test ./...                      # the whole suite
go test -race -vet=all ./...       # what CI runs
go test ./internal/gateway/ir      # one package while iterating
go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out
```

There is no numeric coverage target. A package without tests is a gap, not a
policy violation, but new behaviour ships with the test that fails without it.

## Structure

- Table-driven tests with subtests for model, provider and protocol matrices.
- `t.Helper()` in every helper, `t.Cleanup()` for pools, servers and globals,
  `t.TempDir()` for files and `t.Setenv()` for environment configuration.
- Failure messages print `got`/`want`; assert the error type, the status and the
  body, not just "no error".
- Golden strings for wire formats (YAML, JSON, SSE) — they pin exactly what an
  external client parses.

## Fakes instead of mocks

There is no mocking dependency, and there should not be one:

```go
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
```

- Inject an `http.Client` with a custom `RoundTripper` for transport failures.
- Use `httptest.NewServer` for realistic HTTP and SSE behaviour.
- Use `internal/gateway/gatewaytest` for fake OpenAI Chat, Anthropic and
  Responses upstreams.
- Use function fields and callbacks for browser factories and endpoint selection.
- Make concurrency deterministic with channels, barriers and `sync/atomic`; do
  not sleep except when polling a lifecycle condition that has no signal.

Keep pure normalization, parsing and formatting real — exercise the wire shapes
directly. Never call NVIDIA or Chromium from a unit test; the `cmd/*bench`,
`cmd/probehard`, `cmd/cacheprobe` and `cmd/captchaopt` binaries are manual tools,
not tests.

## Style notes learned in review

- `httptest.NewRequestWithContext(t.Context(), ...)`; never `context.Background()`
  in a test.
- `w.Body.Bytes()` rather than `[]byte(w.Body.String())`.
- Every error checked — the lint config includes blank assignments, so
  `_ = f()` is a finding even in tests.
- A helper that always receives the same argument is a `unparam` finding; use a
  table instead of copy-pasted subtests.