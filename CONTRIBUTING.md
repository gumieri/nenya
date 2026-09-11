# Contributing to Nenya

Thanks for your interest in contributing!

## Development

```bash
go build ./...            # build
go test ./... -count=1    # test
go vet ./...              # vet
golangci-lint run         # lint (config in .golangci.yml)
mise run build|test|lint  # or via mise tasks
```

## Ground rules

- **Zero external dependencies.** Nenya uses only the Go standard library.
  New third-party imports require prior discussion in an issue.
- **Conventional Commits** (`feat(scope):`, `fix(scope):`, `docs:`, …).
- **English only** in code, comments, and commit messages.
- Every exported symbol needs a GoDoc comment; every non-trivial function
  needs at least a happy-path test.
- Agent-driven contributions: see [AGENTS.md](AGENTS.md) for the project's
  engineering conventions.

## Pull requests

1. Open or comment on an issue first for anything larger than a typo fix.
2. Keep PRs focused; run the full gate (`go test ./... -count=1` and
   `golangci-lint run`) before pushing.
3. For security vulnerabilities, do **not** open a PR — see
   [docs/SECURITY.md](docs/SECURITY.md).
