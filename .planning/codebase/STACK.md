# Technology Stack

**Analysis Date:** 2026-09-23

## Languages

**Primary:**
- Go 1.26 - Gateway, NVIDIA client, captcha automation, model registry, admin API, and CLI tools (`cmd/`, `internal/`).

**Secondary:**
- Python 3 - Model catalog scraping utility (`scripts/scrape_playground_models.py`).
- PowerShell and Bash - Local build/run and benchmark helpers (`scripts/*.ps1`, `scripts/*.sh`).
- YAML/JSON - Docker Compose, linter configuration, and model catalog/state files (`docker-compose.yml`, `.golangci.yml`, `internal/models/playground_models.default.json`).

## Runtime

**Environment:**
- Go 1.26 toolchain (`go.mod`).
- Native binaries run on Linux, macOS, and Windows; production container uses Debian Bookworm (`Dockerfile`).

**Package Manager:**
- Go modules (`go.mod`, `go.sum`).
- Lockfile: `go.sum` present.

## Frameworks

**Core:**
- Gin v1.10.1 - HTTP engine and admin/health middleware (`cmd/serve/main.go`).
- CLIProxyAPI/v7 v7.2.97 - Gateway routing, OpenAI/Anthropic/Responses translation, auth registry, and OpenAI-compatible provider handling (`cmd/serve/main.go`).
- chromedp v0.16.0 and cdproto - Headless Chromium/CDP control for hCaptcha extraction (`internal/captcha/`).

**Testing:**
- Go standard `testing` package - Unit and integration-style HTTP tests throughout `internal/` and `cmd/`.

**Build/Dev:**
- `go build`, `go run`, and `go test` - Build and verification commands documented in `README.md`.
- Docker multi-stage build - Static `serve` binary with `CGO_ENABLED=0`, stripped via `-trimpath -ldflags` (`Dockerfile`).
- golangci-lint with gofmt/goimports and selected static/performance linters (`.golangci.yml`).

## Key Dependencies

**Critical:**
- `github.com/router-for-me/CLIProxyAPI/v7` v7.2.97 - Core gateway and format adapters used by `cmd/serve` and `internal/provider/nvidia`.
- `github.com/gin-gonic/gin` v1.10.1 - HTTP routing/middleware and JSON responses.
- `github.com/chromedp/chromedp` v0.16.0 - Long-lived Chrome workers for CAPTCHA automation.
- `github.com/chromedp/cdproto` (2026-07-14 revision) - Chrome DevTools protocol types used with chromedp.

**Infrastructure:**
- Go `net/http`, `net/url`, `encoding/json`, `os`, and `path/filepath` - Upstream calls, proxying, JSON persistence, and local state.
- CLIProxyAPI transitive translator/auth/config packages - Provider registration, request translation, and temporary auth/config handling.

## Configuration

**Environment:**
- CLI flags are the primary runtime configuration (`cmd/serve/main.go`), including listen address, model file, captcha mode, pool sizing, concurrency, and proxy.
- `CHROME_PROXY` optionally supplies the upstream/Chrome proxy; `CHROME_PROXY_REMOTE_DNS=1` enables SOCKS DNS routing (`internal/captcha/config.go`).
- `CHROME_PATH`, `CHROMEDP_NO_SANDBOX`, and `CHROMEDP_ALLOW_IMAGES` tune Chromium discovery/sandboxing/image loading (`internal/captcha/browser.go`).
- `GNVIPI_CAPTCHA_STATE` overrides champion state location; `GNVIPI_STATS_FILE` overrides append-only request statistics location (`internal/captcha/champion.go`, `internal/provider/nvidia/stats.go`).
- `.env`-style files are not required by the Go runtime; `.env*` existence, if present, must remain local and secret values must not be committed.

**Build:**
- `go.mod`/`go.sum` define modules; `.golangci.yml` defines lint/format rules.
- `Dockerfile` builds only `./cmd/serve`; `docker-compose.yml` publishes port 8080 and passes optional `CHROME_PROXY`/`VERSION`.
- `internal/models/playground_models.default.json` is embedded at compile time; `internal/models/playground_models.json` is a local runtime catalog written by the init script.

## Platform Requirements

**Development:**
- Go 1.26; Chromium/Chrome is required for `-auto` captcha mode (or use `-captcha`/client-provided `nv-captcha-token`).
- Network access to NVIDIA Playground/build API and hCaptcha; optional SOCKS/HTTP proxy supported.

**Production:**
- A host or container with Chromium, CA certificates, and sufficient shared memory for concurrent browser workers. The supplied image installs Chromium, fonts, `wget`, and exposes `:8080` (`Dockerfile`).
- Keep the gateway on a trusted/local network: inbound gateway API keys are intentionally disabled (`cmd/serve/main.go`, `README.md`).

---

*Stack analysis: 2026-09-23*
