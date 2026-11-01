# Conventions

What this repository actually enforces, in one page. If a rule is not enforced
by `gofmt`, `go vet`, `golangci-lint` or CI, it is a preference — say so when you
apply it in review.

## Build and verify

Run before every commit; CI runs the same commands:

```bash
gofmt -l ./...                                   # must print nothing
go build ./...
go vet ./...
SKIP_INTEGRATION_TESTS=1 go test -race -vet=all ./...
golangci-lint run --timeout=5m
```

CI (`.github/workflows/ci.yml`) runs on ubuntu, macos and windows; a change that
passes locally on Windows only is still not green until all three pass.

## Naming

- Files: lowercase, describing the concern; `_test.go` beside the code it tests.
- Packages: short, lowercase, one word (`ir`, `sse`, `gwerr`, `captcha`, `nvidia`).
- Exported identifiers: PascalCase, initialisms capitalized (`HTTPClient`, `SSE`).
  Unexported helpers: lowerCamelCase (`buildRequest`, `parseError`).
- Booleans read as predicates: `streamed`, `MessagesOnly`, `Enabled`.
- Tests: `Test<Subject><Behavior>`, with the scenario encoded when it matters
  (`TestFromLegacyKeepsDifferentEndpointsApart`).

## Code

- `gofmt` + `goimports`; standard library first, then a blank line, then project
  and third-party imports. Aliases only for genuine collisions (`coreauth`).
- `.golangci.yml` enables errcheck (blank assignments included), govet,
  ineffassign, staticcheck, unused, bodyclose, gocritic, nilerr, noctx,
  unconvert, unparam, misspell, usestdlibvars, errorlint and copyloopvar.
  Every returned error is checked or deliberately handled — `_ =` is a lint
  finding, not a workaround.
- Comments explain *why*: upstream protocol quirks, concurrency invariants,
  compatibility decisions, next to the invariant they protect. Do not narrate
  what the next line does.

## Errors

- Return errors from the layer that knows the problem; add context with `%w`.
- Callers classify with `errors.Is` / `errors.As`, never string matching.
- Validate at the trust boundary (HTTP input, config file, admin payload) and
  name the offending field in the message.
- Gateway adapters return `*gwerr.Error` values; the dispatcher renders them per
  protocol. Adapters do not write error bodies themselves.

## Logging

- `log` from the standard library, for actionable lifecycle and recovery events.
- Never log tokens, API keys or full request bodies. Redact before formatting.
- Rate-limit messages that can repeat per request or per retry.

## Commits and pull requests

- Conventional commits: `type(scope): summary` (`feat(gateway):`, `fix(serve):`,
  `test(serve):`, `docs:`, `chore:`).
- One issue per PR, `Closes #<n>` in the body, branched from `main` as
  `feat/<n>-slug`, `fix/<n>-slug` or `docs/<n>-slug`.
- Never force-push, never push to `main` directly, never commit secrets,
  binaries, logs or `runs/` artifacts.
- Stage files explicitly; review `git diff --cached` before committing.