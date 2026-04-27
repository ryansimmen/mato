# Contributing To Mato

Thanks for contributing to `mato`.

## Before You Start

- Read the [README](README.md) for the public install and runtime model.
- Read [GOVERNANCE.md](GOVERNANCE.md) for the decision model, roles, review expectations, and continuity policy.
- File an issue before starting larger changes so scope and approach are aligned.
- For substantial features or design changes, open an issue or draft a short proposal in [`docs/proposals/`](docs/proposals/) before implementing.
- Keep changes focused. Small PRs are easier to review and merge.

## Development Setup

Required tools:

- Go 1.26+
- Git
- Docker
- [GitHub CLI (`gh`)](https://github.com/cli/cli#installation)
- [GitHub Copilot CLI (`copilot`)](https://docs.github.com/en/copilot/how-tos/set-up/install-copilot-cli) for full agent-runtime testing
- [`golangci-lint`](https://golangci-lint.run/welcome/install/) v2.11.4 or newer
- optional: [`gopls`](https://pkg.go.dev/golang.org/x/tools/gopls) for editor integration

`staticcheck` and `deadcode` are managed via `go tool` (declared in `go.mod`) and do not need to be installed separately.

Install the local checkout:

```bash
make install
```

`make install` builds and installs the `mato` binary into `GOBIN`. The bundled `mato` skill (`skills/mato/SKILL.md`) is published separately and installed via [`gh skill`](https://cli.github.com/manual/gh_skill) (requires `gh` v2.90.0 or later):

```bash
# Install from the local checkout (handy while iterating on the skill)
gh skill install . mato --from-local --scope user

# Or install the published version from GitHub
gh skill install ryansimmen/mato mato --scope user
```

After editing `skills/mato/SKILL.md`, validate against the [agentskills.io spec](https://agentskills.io/specification) before opening a PR:

```bash
gh skill publish --dry-run
```

## Development Workflow

Fast local checks:

```bash
go build ./...
make test-fast
```

`make test-fast` skips integration tests and the race detector for a quick local loop. Use the full verification command before opening or updating a PR.

Full verification before opening or updating a PR:

```bash
make verify
```

Useful targeted commands:

```bash
go test -race ./...
go test -race ./internal/queue/...
go test -race -run TestSafeRename_MissingSource ./internal/queue/...
go test -race -v ./internal/integration/...
```

Coverage measurement and regression-test evidence are documented in [Testing](docs/testing.md).

## Code Style

- Format Go code with `gofmt`; `make verify` runs `golangci-lint`, which includes `gofmt` and `goimports` checks.
- Follow idiomatic Go naming and structure, including the conventions in [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments).
- Keep imports grouped and sorted by Go tooling conventions.
- Prefer small, minimal changes over broad refactors unless the refactor is the point of the work.

## Expectations

- Add or update tests for behavior changes.
- Update docs when user-visible behavior, setup, configuration, or task format changes.
- Follow existing code structure and naming patterns.

## Pull Requests

- Explain the problem and the intended behavior change.
- Include validation notes with the commands you ran.
- Call out follow-up work separately instead of hiding it in the PR.
- Keep unrelated changes out of the same PR.

## Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/) prefixes:

- `feat:` new user-facing functionality
- `fix:` bug fix
- `docs:` documentation only
- `refactor:` internal change with no behavior change
- `test:` test-only change
- `chore:` tooling, build, or housekeeping

## Questions And Support

Use [GitHub Issues](https://github.com/ryansimmen/mato/issues) for bug reports, feature requests, and usage questions.

## Security

Do not open public issues for security-sensitive reports. Follow [SECURITY.md](SECURITY.md) instead.
