# Documentation

Start here. Everything under `docs/` is hand-written and reviewed; there is no
generated analysis tree beside it.

| Document | What it covers |
|---|---|
| [architecture/universal-llm-gateway-roadmap.md](architecture/universal-llm-gateway-roadmap.md) | The universal gateway backlog: milestones, dependency graph and execution waves (#65). |
| [architecture/overview.md](architecture/overview.md) | How the current gateway is put together and where new code belongs. |
| [development/conventions.md](development/conventions.md) | Build, lint and commit conventions this repository enforces. |
| [development/testing.md](development/testing.md) | How to run the tests, how CI runs them, and how to write new ones. |

## Adding a document

- Put it under `docs/architecture/` if it describes the system, `docs/development/`
  if it tells a contributor how to work on it.
- Add a row to the table above in the same pull request.
- Keep it short and keep it true: a page that repeats what `go test`, CI or the
  linter already enforce is worse than no page.