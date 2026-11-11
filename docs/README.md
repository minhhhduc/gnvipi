# Documentation

Start here. Everything under `docs/` is hand-written and reviewed; there is no
generated analysis tree beside it.

| Document | What it covers |
|---|---|
| [operations.md](operations.md) | Running the gateway: flags, stats storage, Admin Console access, virtual models, web search MCP, data locations, troubleshooting, Docker notes. |
| [providers.md](providers.md) | Universal LLM provider configuration guide, v2 schema reference, and migration instructions (#61). |
| [clients.md](clients.md) | Client setup guide (Claude Code, OpenAI SDKs, Codex, OpenCode, cURL) and cross-protocol compatibility matrix (#62). |
| [architecture/overview.md](architecture/overview.md) | Universal LLM Gateway v2 architecture overview: Hub-and-Spoke pattern, normalized IR, component layout, and design principles (#64). |
| [architecture/adapters.md](architecture/adapters.md) | Universal gateway adapter developer guide: IR, interfaces, streaming invariants, and testing (#63). |
| [architecture/admin-console.md](architecture/admin-console.md) | Admin Console architecture: the headless JSON API, the embedded `web/admin` SPA, its design vocabulary, and the contract/security invariants (#170–#173). |
| [architecture/universal-llm-gateway-roadmap.md](architecture/universal-llm-gateway-roadmap.md) | The universal gateway master roadmap: 10 completed milestones, dependency graph, and execution waves (#65). |
| [../RELEASE_NOTES.md](../RELEASE_NOTES.md) | Universal LLM Gateway v2.0 release notes, core feature highlights, deprecations, and upgrade instructions (#64). |
| [development/conventions.md](development/conventions.md) | Build, lint, and commit conventions this repository enforces. |
| [development/testing.md](development/testing.md) | How to run the tests, how CI runs them, and how to write new ones. |

## Adding a document

- Put it under `docs/architecture/` if it describes the system, `docs/development/`
  if it tells a contributor how to work on it.
- Add a row to the table above in the same pull request.
- Keep it short and keep it true: a page that repeats what `go test`, CI or the
  linter already enforce is worse than no page.
