# External Integrations

**Analysis Date:** 2026-09-23

## APIs & External Services

**NVIDIA Playground / NVCF:**
- NVIDIA Playground model pages and predict API provide anonymous, hCaptcha-gated LLM inference and SSE streams (`internal/gnvipi/client.go`, `internal/models/registry.go`).
  - SDK/Client: Native Go `net/http` client in `internal/gnvipi`; gateway executor in `internal/provider/nvidia`.
  - Auth: Per-request `nv-captcha-token` plus model-specific `nv-function-id`; no NVIDIA account/API key is configured.
  - Endpoints: `https://buildapi.ngc.nvidia.com/v2/predict/models/{namespace}/{slug}` and `https://build.nvidia.com/` (`internal/gnvipi/client.go`, `internal/captcha/url_provider.go`).

**hCaptcha:**
- hCaptcha widgets embedded in NVIDIA Playground pages are executed and polled through browser JavaScript (`internal/captcha/extract.go`).
  - SDK/Client: chromedp/CDP with a pool of long-lived Chromium processes (`internal/captcha/browser.go`, `internal/captcha/group.go`, `internal/captcha/pool.go`).
  - Auth: Sitekey/widget session and one-shot tokens; token is forwarded to NVIDIA, not persisted as application state.

**Custom OpenAI-compatible and Anthropic providers:**
- Admin-managed providers are forwarded by CLIProxyAPI or explicit passthrough middleware (`cmd/serve/admin.go`, `cmd/serve/passthrough.go`).
  - SDK/Client: CLIProxyAPI OpenAI compatibility executor and Go `net/http` passthrough clients.
  - Auth: Provider API keys stored in the local model preferences file and sent as provider credentials (`cmd/serve/main.go`, `cmd/serve/passthrough.go`).
  - Modes include normal OpenAI-compatible, `messages_only` for `/v1/messages`, and `responses_only` for `/v1/responses`.

**CLIProxyAPI gateway:**
- Embedded library exposes `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, model routing, and format translation (`cmd/serve/main.go`).
  - SDK/Client: `github.com/router-for-me/CLIProxyAPI/v7/sdk/*`.
  - Auth: Temporary local auth directory contains a synthetic NVIDIA provider record; inbound API key verification is disabled (`buildConfig` in `cmd/serve/main.go`).

## Data Storage

**Databases:**
- None detected. Runtime state is file-backed JSON/NDJSON rather than SQL or a hosted database.

**File Storage:**
- Local filesystem only: model masks/deletions/custom providers at the `-models-file` path (`cmd/serve/admin.go`); embedded default catalog at `internal/models/playground_models.default.json`.
- CAPTCHA champion state defaults under the OS user cache (`~/.cache/gnvipi`, `%LocalAppData%/gnvipi`, or macOS cache) and can be overridden with `GNVIPI_CAPTCHA_STATE` (`internal/captcha/champion.go`).
- Request statistics append as JSON Lines beside champion state or at `GNVIPI_STATS_FILE` (`internal/provider/nvidia/stats.go`).
- CLIProxyAPI auth/config are generated in a temporary directory and removed on shutdown (`buildConfig`/`defer os.RemoveAll` in `cmd/serve/main.go`).

**Caching:**
- In-memory model registry, captcha token pool, request statistics window, and CLIProxyAPI registries; no Redis or remote cache.
- Best-effort Claude Code model discovery cache is written to `~/.claude/cache/gateway-models.json` (`writeClaudeModelCache` in `cmd/serve/main.go`).

## Authentication & Identity

**Auth Provider:**
- Custom anonymous/local gateway authentication. No inbound API keys are registered; callers may supply `nv-captcha-token` per request, while `-captcha` supplies one token or `-auto` obtains tokens (`cmd/serve/main.go`, `internal/provider/nvidia/executor.go`).
- Outbound custom-provider credentials are API keys configured through `/admin` and serialized into generated CLIProxyAPI config (`cmd/serve/admin.go`, `cmd/serve/main.go`).

## Monitoring & Observability

**Error Tracking:**
- None detected; errors are returned through HTTP and logged.

**Logs:**
- Standard Go `log` output records startup, proxy/browser/pool state, upstream failures, and request metrics. `/healthz` reports pool counters (`cmd/serve/main.go`).
- Per-request metrics are held in `GlobalStats` and persisted as append-only NDJSON (`internal/provider/nvidia/stats.go`).

## CI/CD & Deployment

**Hosting:**
- Self-hosted binary or Docker/Compose. The image is Debian Bookworm slim with Chromium and a `wget` healthcheck (`Dockerfile`, `docker-compose.yml`).

**CI Pipeline:**
- GitHub workflow files are present under `.github/`; no external deployment service or release integration is detected in the application code.

## Environment Configuration

**Required env vars:**
- None for basic operation; flags and client-supplied captcha token are sufficient.
- Optional: `CHROME_PROXY`, `CHROME_PROXY_REMOTE_DNS`, `CHROME_PATH`, `CHROMEDP_NO_SANDBOX`, `CHROMEDP_ALLOW_IMAGES`, `GNVIPI_CAPTCHA_STATE`, `GNVIPI_STATS_FILE`.

**Secrets location:**
- Custom provider API keys are entered through the admin surface and persisted in the configured model JSON; generated provider config is temporary and mode `0600` (`cmd/serve/main.go`). Do not inspect or commit secret-bearing local files.

## Webhooks & Callbacks

**Incoming:**
- No webhooks. HTTP clients call gateway routes such as `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/models`, and `/healthz` (`README.md`, `cmd/serve/main.go`).

**Outgoing:**
- Outbound HTTPS POST requests go to NVIDIA predict endpoints and configured custom provider endpoints; Chrome navigates to NVIDIA Playground pages and hCaptcha resources. SSE responses are streamed back to clients (`internal/gnvipi/client.go`, `internal/provider/nvidia/executor.go`, `cmd/serve/passthrough.go`).

---

*Integration audit: 2026-09-23*
