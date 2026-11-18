# gnvipi — Universal LLM API Gateway & NVIDIA Playground Reverse-Proxy

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev)
![Platforms](https://img.shields.io/badge/Platform-Linux%20%7C%20macOS%20%7C%20Windows-lightgrey)

Self-hosted gateway that gives every AI coding client one endpoint: **Claude
Code**, **OpenAI SDKs**, **Codex**, **OpenCode**, and plain `cURL`. It accepts
three inbound protocols (**OpenAI Chat**, **Anthropic Messages**, **OpenAI
Responses**), translates to any upstream (**OpenAI-compatible**,
**Anthropic-compatible**, **OpenAI Responses**, **NVIDIA Playground**, custom
endpoints), and mints hCaptcha tokens with a headless-Chrome pool so the free
NVIDIA Playground models (DeepSeek-V4, Kimi, GLM, Nemotron, GPT-OSS) work without
a browser.

## Features

- **Multi-protocol in, multi-protocol out** — same-protocol relay and
  cross-protocol translation, with streaming, token counting, and tool calls.
- **NVIDIA Playground without a browser** — Chrome captcha pool, started on
  demand from the Admin Console (`-auto` never launches Chrome at startup).
- **Unified model catalog** — per-provider aliases, wildcards (`openrouter/*`),
  fallback chains, SSRF policy, and the `automode` / `cycle` virtual models.
- **PostgreSQL-backed** — providers, API keys, users/roles, audit trail; provider
  secrets encrypted at rest with `GNVIPI_MASTER_KEY`.
- **Admin Console** at `/admin` — live stats, request history, provider and key
  management, Chrome worker control.
- **Web search MCP** — `search_web` over `/mcp` (streamable HTTP) and plain JSON
  over `/v2/search`.

## Requirements

`Go 1.26+` and `PostgreSQL 17` for a source build; `Docker` + Compose v2 for the
container path. The captcha pool needs a local Chrome/Chromium — the Docker image
ships Chromium, so container users need neither Chrome nor Go on the host.

## Quick Start

### 1. One-command Docker deploy (auto-migrate)

Create `.env` with the one required value (base64, 32 bytes):

```bash
# Linux / macOS / Git Bash
echo "GNVIPI_MASTER_KEY=$(openssl rand -base64 32)" > .env
```

```powershell
# Windows PowerShell
$k = New-Object byte[] 32
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($k)
"GNVIPI_MASTER_KEY=$([Convert]::ToBase64String($k))" | Set-Content .env
```

Then deploy — the helper script builds the image, runs the `migrate` service to
completion, and starts `serve`:

| Platform | Command |
|---|---|
| Linux | `./scripts/linux/deploy.sh` |
| macOS | `./scripts/macos/deploy.sh` |
| Windows | `.\scripts\windows\deploy.ps1` (or `deploy.bat`) |

Options: `--no-build` / `-NoBuild` reuses existing images, `--down` / `-Down`
stops the stack and keeps volumes. Open `http://127.0.0.1:8080/admin` (redirects
to the dashboard at `/`) and sign in as `gnvipi` — on a fresh database the
password is `gnvipi` and the console forces you to change it (set
`GNVIPI_ROOT_PASSWORD` to pick your own). Add providers on the **Models** page,
and use **Chrome** → **Start All** to launch the NVIDIA Playground pool.

Expose it through Cloudflare Tunnel (the `tunnel` Compose profile runs a
`cloudflared` sidecar sharing `serve`'s network namespace):

```bash
echo "CF_TUNNEL=<tunnel-token>" >> .env
echo "COMPOSE_PROFILES=tunnel" >> .env
./scripts/linux/deploy.sh --no-build
```

### 2. Local development (Go + PostgreSQL)

```bash
# Database: dev-only credentials, never reuse outside a sandbox.
docker compose up -d --wait db
export GNVIPI_DATABASE_URL='postgres://gnvipi_dev:gnvipi_dev_only@localhost:5432/gnvipi?sslmode=disable'
export GNVIPI_MASTER_KEY="$(openssl rand -base64 32)"

go build -o ./bin/serve ./cmd/serve            # Windows: -o .\bin\serve.exe
./bin/serve migrate
./bin/serve -addr 127.0.0.1:8080      # Admin Console: http://localhost:8080/admin
```

The OS launchers wrap this and add the Chrome pool, a readiness wait, and an
optional Claude Code session pointed at the gateway:

```bash
./scripts/linux/run.sh            # or ./scripts/macos/run.sh, .\scripts\windows\run.ps1
./scripts/linux/run.sh --claude   # also launches Claude Code (run.ps1 -Claude)
```

Useful flags: `--no-captcha` / `-NoCaptcha` (custom providers only, no Chrome),
`--lan` / `-Lan` (bind `:8080`), `--build`, `--proxy <url>`.

For the DB-backed Claude Code flow, mint a one-time gateway token instead of
using a static gateway key:

```bash
printf '%s\n' '<temporary-password>' | ./bin/serve admin create-superuser --username <name> --with-gateway-token
export GNVIPI_GATEWAY_TOKEN='gw_...'
./scripts/linux/run.sh --claude
```

The password is read from stdin only — at a terminal (or MSYS2/Git Bash mintty)
the command refuses to run so it is never echoed. Client base URLs and full
snippets: [docs/clients.md](docs/clients.md).

### 3. Custom providers only (no Chrome, no database)

```bash
go run ./cmd/serve -addr 127.0.0.1:8080
```

Providers are then read from `-models-file` (default
`internal/models/playground_models.json`; seed an empty one with
`scripts/linux/init.sh`, `scripts/macos/init.sh`, or `scripts\windows\init.ps1`);
with `GNVIPI_DATABASE_URL` set, providers, keys, masks, and the search template
live in PostgreSQL instead.

## Configuration

`serve` reads flags and environment variables; the same names feed
`docker-compose.yml` through `.env`.

| Variable | Purpose |
|---|---|
| `GNVIPI_MASTER_KEY` | **Required in database mode.** Base64 32-byte key that encrypts stored provider secrets. Rotation: `GNVIPI_MASTER_KEY_VERSION`, `GNVIPI_MASTER_KEY_PREVIOUS`. |
| `GNVIPI_DATABASE_URL` | PostgreSQL DSN. Unset → file mode (no accounts, no audit trail). |
| `GNVIPI_MIGRATION_DATABASE_URL` | DDL role used by `serve migrate` / `-auto-migrate`. **Required when `GNVIPI_ENV=production`**; development falls back to `GNVIPI_DATABASE_URL`. |
| `GNVIPI_ENV` | `development` (default) or `production`. `production` requires `GNVIPI_DATABASE_URL` and rejects `-auto-migrate`. |
| `GNVIPI_ROOT_PASSWORD` / `GNVIPI_ROOT_EMAIL` | Seed the built-in `gnvipi` root account on first start. Without the password override the default `gnvipi` is forced to change at first sign-in. |
| `GNVIPI_PUBLIC_URL` / `GNVIPI_DOMAIN` | Public base URL (`https://gateway.example.com`) shown in Admin Console API snippets; bare hosts default to `https`. |
| `GNVIPI_TRUSTED_PROXIES` | Comma-separated IPs/CIDRs trusted for `X-Forwarded-For` / `X-Forwarded-Proto`. Compose defaults to `127.0.0.1/32,::1/128` for the tunnel sidecar. |
| `GNVIPI_SMTP_HOST` / `_PORT` / `_USER` / `_PASSWORD` / `_FROM` | Outbound SMTP delivery for admin OTP notifications. Delivery is disabled when `GNVIPI_SMTP_HOST` is unset. |
| `GNVIPI_ADMIN_TOKEN` | Bootstrap token for the legacy admin surface. Ignored once a database account exists. |
| `GNVIPI_GATEWAY_KEY` | Comma-separated client keys guarding `/v1/*`, `/mcp`, `/v2/search`. Optional; DB tokens are the supported path. |
| `GNVIPI_STATS_FILE` | Stats history (NDJSON, rotated). Defaults to `stats.jsonl` in the OS user cache dir; point it at a volume in containers. |
| `GNVIPI_DB_MAX_CONNS` / `GNVIPI_DB_MIN_CONNS` / `GNVIPI_DB_CONNECT_TIMEOUT` | Connection-pool sizing. |
| `CF_TUNNEL` | Cloudflare Tunnel token consumed by the `tunnel` Compose profile. |
| `COMPOSE_PROFILES` | `tunnel` enables the `cloudflared` sidecar. |
| `GNVIPI_PORT` / `GNVIPI_DB_PORT` / `GNVIPI_DB_USER` / `GNVIPI_DB_PASSWORD` / `GNVIPI_DB_NAME` | Compose-only host ports and dev database credentials. |

Run flags (port, body limits, pooling, `-deny-private-upstreams`, …) and data
locations are in [docs/operations.md](docs/operations.md).

## Endpoints

| Route | Protocol | Function |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | Chat for OpenAI SDKs, OpenCode, HTTP clients |
| `POST /v1/messages` | Anthropic Messages | Drop-in endpoint for Claude Code and Anthropic clients |
| `POST /v1/responses` | OpenAI Responses | Codex and next-gen tools |
| `POST /v1/messages/count_tokens` | Anthropic Token Counting | Upstream or locally estimated counting |
| `GET /v1/models` | Unified catalog | Models across custom providers and NVIDIA Playground |
| `POST` / `GET /mcp` | HTTP Streamable MCP | `search_web` tool |
| `POST /v2/search` | JSON | `{"query"}` in / `{"results"}` out |
| `/` (via `/admin`) | Admin Console | Stats, providers, keys, Chrome workers |
| `GET /healthz` | JSON | Liveness and captcha-pool status |

## Documentation

| Guide | Contents |
|---|---|
| [docs/operations.md](docs/operations.md) | Run flags, stats storage, Admin Console, `cycle` failover, search MCP, data locations, troubleshooting. |
| [docs/postgres-operations.md](docs/postgres-operations.md) | Roles and grants, migrations, backup/PITR, monitoring, retention. |
| [docs/auth.md](docs/auth.md) | Ownership model, root/superuser/admin/user permissions, bootstrap and recovery, secret handling. |
| [docs/providers.md](docs/providers.md) | Provider schema v2, model mappings, wildcards, fallback chains, SSRF policy, migration. |
| [docs/clients.md](docs/clients.md) | Client setup and the cross-protocol compatibility matrix. |
| [docs/architecture/overview.md](docs/architecture/overview.md) | Hub-and-spoke design, normalized IR, execution paths, invariants. |
| [docs/architecture/adapters.md](docs/architecture/adapters.md) | Writing and testing protocol adapters. |
| [docs/development/testing.md](docs/development/testing.md) | Test layout, CI, and conventions. |
| [RELEASE_NOTES.md](RELEASE_NOTES.md) | v2.0 highlights, deprecations, upgrade guide. |

## Security

> [!WARNING]
> `serve` binds `127.0.0.1:8080` (loopback only) by default, but that is the
> only default protection. In **file mode** (no `GNVIPI_DATABASE_URL`) with no
> `-gateway-key` / `GNVIPI_GATEWAY_KEY`, `/v1/*`, `/mcp`, and `/v2/search accept
> **unauthenticated** requests from anything that can reach the port, so a
> wildcard bind (`-addr :8080`, `-p 8080:8080`) or a tunnel exposes them.
> **Database mode** fails closed and requires a database-issued `gw_...` token or
> a configured static key. Placeholders such as `local-gateway` never secure
> anything. `/admin` is account-gated in database mode; in file mode a
> non-loopback bind answers **HTTP 403** until `-admin-token` /
> `GNVIPI_ADMIN_TOKEN` is set. For any non-loopback deployment, set client auth
> and put a firewall or an authenticated reverse proxy in front.

Provider, gateway, and admin credentials are scrubbed from diagnostics and error
responses, and upstream dial targets are checked against cloud-metadata and
link-local addresses both at save time and at dial time. Details:
[docs/clients.md](docs/clients.md), [docs/providers.md](docs/providers.md),
[docs/operations.md](docs/operations.md).

## Contributing & License

Run `go build ./...` and `go vet ./...` before opening a pull request; test and
commit conventions are in [docs/development/conventions.md](docs/development/conventions.md)
and [CONTRIBUTING.md](CONTRIBUTING.md). Released under the [MIT License](./LICENSE).