# Codebase Concerns

**Analysis Date:** 2026-09-23

## Tech Debt

**Large gateway entrypoint and admin module:**
- Issue: `cmd/serve/main.go` (864 lines) and `cmd/serve/admin.go` (1363 lines) combine process lifecycle, dependency wiring, HTTP route registration, persistence, HTML/CSS/JavaScript rendering, and model management.
- Files: `cmd/serve/main.go`, `cmd/serve/admin.go`
- Impact: Changes to routing, startup/shutdown, or admin persistence have a wide regression surface and are difficult to review or test in isolation.
- Fix approach: Split lifecycle/configuration, route handlers, persistence, and embedded admin assets into focused packages while preserving the existing `modelPrefs` synchronization contract.

**Duplicated custom-provider forwarding paths:**
- Issue: OpenAI chat, OpenAI Responses, and Anthropic Messages forwarding each reconstruct requests, authorization, response copying, and statistics handling separately.
- Files: `cmd/serve/passthrough.go:67`, `cmd/serve/passthrough.go:121`, `cmd/serve/passthrough.go:303`
- Impact: Header filtering, timeout, error, and streaming fixes can apply to one provider mode but not another.
- Fix approach: Introduce one bounded, policy-controlled relay helper for target construction, request headers, response streaming, and stats recording; keep format-specific model rewriting at the edges.

## Known Bugs

**Full stats-file scan on frame reads:**
- Symptoms: Every `/admin/stats?frames=N` request reads and splits the entire append-only statistics file before selecting a 200-event frame.
- Files: `internal/provider/nvidia/stats.go:315`, `internal/provider/nvidia/stats.go:330`, `cmd/serve/admin.go:451`
- Trigger: Run the gateway long enough to accumulate a large `stats.jsonl`, then open or poll the dashboard and switch frames.
- Workaround: Remove or rotate the stats file manually and avoid historical frame views.

## Security Considerations

**Unauthenticated administrative control surface:**
- Risk: The server intentionally disables inbound API-key verification and registers `/admin`, `/admin/models`, `/admin/providers`, `/admin/delete`, `/admin/retry-failed`, and `/admin/chromes` without an authentication or CSRF check. Any network peer able to reach a non-loopback bind address can change routing, add endpoints, or kill/start Chrome workers.
- Files: `cmd/serve/main.go:21`, `cmd/serve/main.go:126`, `cmd/serve/main.go:231`, `cmd/serve/admin.go:390`
- Current mitigation: README instructs operators to keep the service on localhost; the default `:8080` bind does not enforce that boundary.
- Recommendations: Bind to loopback by default, add an explicit opt-in for LAN exposure, and protect admin mutations with an authentication token plus CSRF/origin checks.

**Plaintext custom-provider secrets:**
- Risk: `/admin/providers` accepts API keys, `modelPrefs.save` serializes them into the models file, and the admin page embeds the complete provider list as JavaScript (`PROVIDERS`). A readable file or browser/admin response exposes upstream credentials.
- Files: `cmd/serve/admin.go:76`, `cmd/serve/admin.go:221`, `cmd/serve/admin.go:487`, `cmd/serve/admin.go:665`
- Current mitigation: The generated models file is locally ignored by Git, but filesystem permissions and the unauthenticated admin route remain the operator's responsibility.
- Recommendations: Store secrets outside the catalog, use restrictive file permissions, redact keys in HTML/API responses, and pass credentials to the relay through a protected secret store.

**Server-side request forgery through custom endpoints:**
- Risk: Admin input validates only that `BaseURL` is non-empty. The relay then makes server-side requests to that URL and attaches the saved bearer or x-api-key credential, allowing an administrator-compromised client to target internal services or metadata endpoints.
- Files: `cmd/serve/admin.go:487`, `cmd/serve/passthrough.go:112`, `cmd/serve/passthrough.go:199`, `cmd/serve/passthrough.go:347`
- Current mitigation: None detected beyond access to the unauthenticated admin route.
- Recommendations: Validate URL scheme/host, block loopback/link-local/private destinations unless explicitly allowed, resolve and re-check DNS addresses, and support an allowlist for approved upstream hosts.

**Chromium runs without sandboxing when configured for containers:**
- Risk: `CHROMEDP_NO_SANDBOX=1` adds Chrome's `--no-sandbox` flag. A browser automation compromise then has weaker process isolation.
- Files: `internal/captcha/browser.go:105`, `Dockerfile:24`
- Current mitigation: The Docker image runs a dedicated service container and the flag is opt-in through the environment in code, but the deployment contract should make the tradeoff explicit.
- Recommendations: Prefer a non-root container with Chrome sandbox support; use no-sandbox only in a separately documented, restricted deployment profile.

## Performance Bottlenecks

**Unbounded statistics log and repeated whole-file parsing:**
- Problem: Every successful or failed request appends to `stats.jsonl`; no retention, rotation, or maximum size is enforced, while frame counts and frame reads load the whole file.
- Files: `internal/provider/nvidia/stats.go:293`, `internal/provider/nvidia/stats.go:315`, `internal/provider/nvidia/stats.go:330`
- Cause: The append-only file is treated as the historical index and `os.ReadFile` is used for every query.
- Improvement path: Add size/time rotation and retention, maintain a bounded in-memory index or seekable offsets, and rate-limit dashboard polling/history reads.

**Unbounded request bodies in passthrough and admin routes:**
- Problem: Middleware calls `io.ReadAll(c.Request.Body)` without a size limit before parsing or forwarding.
- Files: `cmd/serve/passthrough.go:71`, `cmd/serve/passthrough.go:154`, `cmd/serve/passthrough.go:248`, `cmd/serve/admin.go:410`, `cmd/serve/admin.go:482`
- Cause: The gateway relies on upstream/framework behavior rather than applying a local maximum body size.
- Improvement path: Wrap bodies with `http.MaxBytesReader` at the gateway boundary, reject oversized JSON with 413, and configure separate limits for chat payloads and admin configuration.

**Custom upstream clients have no request timeout:**
- Problem: `messagesOnlyClient` and `compatClient` have no client-level timeout; non-stream requests can hold resources indefinitely when an upstream stalls.
- Files: `cmd/serve/passthrough.go:47`, `cmd/serve/passthrough.go:55`, `cmd/serve/passthrough.go:112`
- Cause: Request context is passed through, but clients and handlers do not impose a deadline for callers that omit one.
- Improvement path: Set transport/header/read deadlines appropriate to streaming versus non-streaming routes and enforce a maximum upstream lifetime.

## Fragile Areas

**Captcha browser lifecycle and external-site dependency:**
- Files: `internal/captcha/browser.go:90`, `internal/captcha/browser.go:143`, `internal/captcha/group.go:190`, `cmd/serve/main.go:344`
- Why fragile: Startup depends on Chromium, CDP, a third-party page, hCaptcha widget behavior, proxy configuration, and a 90-second navigation path; a site or browser change can leave the pool empty while the gateway remains reachable.
- Safe modification: Keep browser allocation, warmup, extraction, and pool replacement behind the existing interfaces; test with fake extractors and exercise cancellation/kill/restart paths before changing timeouts.
- Test coverage: Unit tests cover pool/group behavior, but real Chromium/proxy/site failures are environment-dependent and not covered by ordinary `go test ./...`.

**Global mutable routing and statistics state:**
- Files: `cmd/serve/admin.go:28`, `cmd/serve/admin.go:44`, `internal/provider/nvidia/stats.go:104`, `cmd/serve/main.go:175`
- Why fragile: Process-wide `prefs`, `deadModels`, `GlobalStats`, and callback fields are updated by HTTP handlers, watcher callbacks, reconcilers, and background workers.
- Safe modification: Preserve lock ordering (`changeMu` before preference mutation), avoid callbacks while holding preference locks, and add race-enabled tests around provider/model updates and reconciliation.
- Test coverage: Existing tests cover selected handlers and packages, but concurrent admin updates plus watcher/reconciler activity are not comprehensively exercised.

**Generated configuration and runtime file writes:**
- Files: `cmd/serve/admin.go:221`, `cmd/serve/main.go:436`, `cmd/serve/main.go:583`
- Why fragile: Catalog/provider changes rewrite files and Claude's cache as side effects of HTTP requests; a process crash or filesystem error can leave external consumers stale even when in-memory routing changed.
- Safe modification: Keep atomic temp-file/rename semantics, check directory permissions, and make cache/config updates retryable and observable without failing the primary request.
- Test coverage: Persistence error paths and cross-process file changes are lightly covered.

## Scaling Limits

**Captcha pool and Chrome memory footprint:**
- Current capacity: Defaults create three Chrome workers and a pool of three ready tokens in `cmd/serve/main.go:56` and `cmd/serve/main.go:57`.
- Limit: Each worker owns a full headless Chrome process; increasing `-pool-workers` scales memory and external-site load linearly, and a full/expired token pool can still block requests for `-captcha-wait`.
- Scaling path: Add explicit memory/worker admission controls, expose queue depth and extraction latency, and prefer harness mode only where its security and site-compatibility tradeoffs are accepted.

**Single global stats mutex:**
- Current capacity: Every request records through `GlobalStats.Observe` and `AppendLog` under shared state.
- Limit: High request rates serialize counters, event retention, and synchronous append writes; dashboard snapshots also use the same mutex.
- Scaling path: Decouple request recording with a bounded channel/batch writer and keep snapshots lock-light.

## Dependencies at Risk

**Third-party browser/site behavior:**
- Risk: Captcha extraction depends on `chromedp`, CDP protocol details, Chromium flags, and NVIDIA/hCaptcha page structure rather than a stable API.
- Impact: Browser or upstream page changes can break token minting and make all automatic requests return 503/401.
- Migration plan: Keep `ExtractFunc`/`ExtractNFunc` seams, add a health signal and fallback/manual-token mode, and pin/test supported Chromium and CDP combinations in deployment.

## Missing Critical Features

**Operational authentication and request policy:**
- Problem: There is no built-in inbound auth, rate limiting, per-client quota, or admin audit trail even though the service forwards credentials and controls external providers.
- Blocks: Safe LAN/shared-host deployment and reliable attribution of destructive admin changes.

**Stats retention/rotation policy:**
- Problem: The append-only runtime log has no configured maximum size or retention period.
- Blocks: Predictable disk usage and low-cost long-running deployment.

## Test Coverage Gaps

**End-to-end security and resource limits:**
- What's not tested: Unauthenticated route exposure, CSRF, secret redaction, SSRF/private-address rejection, oversized request rejection, and stalled-upstream timeout behavior.
- Files: `cmd/serve/admin.go`, `cmd/serve/passthrough.go`, `cmd/serve/main.go`
- Risk: Security and denial-of-service regressions can pass unit tests unnoticed.
- Priority: High

**Full repository test reproducibility:**
- What's not tested: A clean `go test ./...` run in the current workspace could not complete because the Go build cache returned `Access is denied`; package tests that did run passed, but the repository lacks a documented cache/workspace workaround.
- Files: `go.mod`, `CONTRIBUTING.md`
- Risk: CI/local environment differences can hide build failures or discourage verification.
- Priority: Medium

---

*Concerns audit: 2026-09-23*
