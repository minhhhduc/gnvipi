# Codebase Structure

**Analysis Date:** 2026-09-23

## Directory Layout

```text
gnvipi/
├── cmd/
│   ├── serve/          # Main gateway, admin UI, passthrough adapters, tests
│   ├── example/        # Direct client usage example
│   ├── streambench/    # Streaming benchmark
│   ├── hangbench/      # Hang/latency experiment
│   ├── probehard/      # Endpoint probing utility
│   ├── captchaopt/     # Captcha URL/strategy benchmark
│   └── cacheprobe/     # Cache behavior probe
├── internal/
│   ├── provider/nvidia/ # CLIProxyAPI executor, normalization, SSE, stats
│   ├── captcha/        # Chrome automation, browser group, token pool
│   ├── gnvipi/         # Standalone NVIDIA Playground HTTP/SSE client
│   └── models/         # Embedded Playground model registry and JSON catalog
├── prompts/             # Manual test prompts
├── scripts/             # Build/run/scrape/benchmark helpers
├── runs/                # Experiment notes and raw outputs
├── bin/                 # Local build output
├── Dockerfile           # Container image for deployment
├── docker-compose.yml   # Container orchestration
├── go.mod / go.sum      # Go module and dependency lock data
└── README.md            # Usage, routes, flags, and architecture overview
```

## Directory Purposes

**`cmd/serve/`:**
- Purpose: Production gateway composition and HTTP-facing behavior.
- Contains: `main.go`, admin persistence/UI, passthrough middleware, Anthropic/Responses adapters, tests.
- Key files: `cmd/serve/main.go`, `cmd/serve/admin.go`, `cmd/serve/passthrough.go`, `cmd/serve/responses_anthropic.go`.

**`internal/provider/nvidia/`:**
- Purpose: NVIDIA-specific provider implementation behind CLIProxyAPI.
- Contains: `executor.go`, request normalization/compatibility, reasoning profiles, SSE coalescing, token counting, stats, tests.
- Key files: `internal/provider/nvidia/executor.go`, `internal/provider/nvidia/normalize.go`, `internal/provider/nvidia/coalesce.go`, `internal/provider/nvidia/stats.go`.

**`internal/captcha/`:**
- Purpose: Headless Chrome and hCaptcha token production.
- Contains: `browser.go`, `group.go`, `pool.go`, `batch.go`, harness/sitekey extraction, URL/champion selection, tests.
- Key files: `internal/captcha/group.go`, `internal/captcha/pool.go`, `internal/captcha/browser.go`.

**`internal/models/`:**
- Purpose: Catalog and invocation metadata for supported Playground models.
- Contains: `registry.go`, embedded `playground_models.default.json`, registry tests.
- Key files: `internal/models/registry.go`, `internal/models/playground_models.default.json`.

**`internal/gnvipi/`:**
- Purpose: Reusable direct client independent of the gateway server.
- Contains: request/response types, sync chat, SSE stream client, error parsing, tests.
- Key files: `internal/gnvipi/client.go`.

**`cmd/*` utilities:**
- Purpose: Keep experiments and operational probes separate from the production server.
- Contains: one `main.go` per executable, with utility-local tests where needed.
- Key files: `cmd/example/main.go`, `cmd/streambench/main.go`, `cmd/captchaopt/main.go`.

## Key File Locations

**Entry Points:**
- `cmd/serve/main.go`: production HTTP gateway and lifecycle.
- `cmd/example/main.go`: direct client example.
- `cmd/streambench/main.go`, `cmd/hangbench/main.go`, `cmd/probehard/main.go`, `cmd/captchaopt/main.go`, `cmd/cacheprobe/main.go`: explicit diagnostic binaries.

**Configuration:**
- `go.mod`: Go 1.26 module and direct dependencies.
- `cmd/serve/main.go:55-72`: runtime flags and defaults.
- `cmd/serve/main.go:603-643`: generated CLIProxyAPI configuration/auth setup.
- `cmd/serve/admin.go:106-242`: persisted model/provider preferences.
- `Dockerfile`, `docker-compose.yml`: container deployment.

**Core Logic:**
- `internal/provider/nvidia/executor.go`: provider request/response path.
- `internal/captcha/pool.go`: token buffering and leases.
- `internal/captcha/group.go`: browser-worker scheduling/recovery.
- `internal/models/registry.go`: model resolution.
- `cmd/serve/passthrough.go`: custom provider routing/stat capture.

**Testing:**
- Tests are co-located beside implementations: `internal/provider/nvidia/*_test.go`, `internal/captcha/*_test.go`, `internal/models/registry_test.go`, `internal/gnvipi/client_test.go`, and `cmd/serve/*_test.go`.
- Run the repository suite with `go test ./...`.

## Naming Conventions

**Files:**
- Go implementation files use lowercase snake-free names such as `executor.go`, `registry.go`, `passthrough.go`; tests use the same base plus `_test.go`.
- Executable directories use short lowercase names under `cmd/`; implementation is normally `main.go`.

**Directories:**
- Go package paths are lowercase and domain-oriented: `internal/provider/nvidia`, `internal/captcha`, `internal/models`.
- Keep production server code under `cmd/serve`; keep reusable implementation under `internal/`.

## Where to Add New Code

**New Feature:**
- Gateway wiring/routes/middleware: add or extend `cmd/serve/main.go`, with protocol-specific middleware in `cmd/serve/passthrough.go` or a focused sibling file.
- NVIDIA behavior: add to `internal/provider/nvidia/`; preserve the executor boundary and add a co-located test.
- Captcha behavior: add to `internal/captcha/`; expose narrow interfaces (`ExtractFunc`, `ExtractNFunc`) when integrating with the pool.
- Tests: place beside the target package using `_test.go`.

**New Component/Module:**
- Reusable gateway/provider logic belongs under `internal/<domain>/`.
- A standalone executable belongs under `cmd/<name>/main.go`; do not import `cmd/serve` internals into utilities.

**Utilities:**
- Shared model resolution belongs in `internal/models/registry.go`.
- Shared direct Playground HTTP behavior belongs in `internal/gnvipi/client.go`.
- Small server-only helpers belong in the relevant `cmd/serve` file rather than creating a package for one caller.

## Special Directories

**`internal/models/playground_models.default.json`:**
- Purpose: Embedded default model catalog consumed at compile time by `registry.go`.
- Generated: Yes, by `scripts/scrape_playground_models.py`.
- Committed: Yes.

**`runs/`:**
- Purpose: Benchmark/experiment notes and raw captures.
- Generated: Yes, by manual experiments and scripts.
- Committed: Current experiment artifacts are present.

**`bin/`:**
- Purpose: Local build output.
- Generated: Yes.
- Committed: Build output directory is present; follow `.gitignore` before adding artifacts.

**`.planning/codebase/`:**
- Purpose: GSD architecture/quality/technology mapping documents.
- Generated: Yes, by mapping workflows.
- Committed: Treat generated mapping docs as planning artifacts; do not place runtime code here.

---

*Structure analysis: 2026-09-23*
