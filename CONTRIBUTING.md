# Contributing To Mato

Thanks for contributing to `mato`.

## Before You Start

- Read the [README](README.md) for the public install and runtime model.
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

`staticcheck` and `deadcode` are managed via `go tool` (declared in `go.mod`) and do not need to be installed separately.

Install the local checkout:

```bash
make install
```

Or, if you only want the local binary without the skill install side effects:

```bash
go install ./cmd/mato
```

`make install` installs the local `mato` binary and also installs the bundled `mato` skill into local CLI skill directories.

## Development Workflow

Fast local checks:

```bash
go build ./...
go test -race ./...
```

Full verification before opening or updating a PR:

```bash
make verify
```

Useful targeted commands:

```bash
go test -race ./internal/queue/...
go test -race -run TestSafeRename_MissingSource ./internal/queue/...
go test -race -v ./internal/integration/...
```

## Expectations

- Add or update tests for behavior changes.
- Update docs when user-visible behavior, setup, configuration, or task format changes.
- Follow existing code structure and naming patterns.
- Prefer small, minimal changes over broad refactors unless the refactor is the point of the work.

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
