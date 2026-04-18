# Contributing To Mato

Thanks for contributing to `mato`.

## Before You Start

- Read the [README](README.md) for the public install and runtime model.
- File an issue before starting larger changes so scope and approach are aligned.
- Keep changes focused. Small PRs are easier to review and merge.

## Development Setup

Required tools:

- Go 1.26+
- Git
- Docker
- [GitHub CLI (`gh`)](https://cli.github.com/)
- [GitHub Copilot CLI (`copilot`)](https://docs.github.com/en/copilot) for full agent-runtime testing
- `golangci-lint`
- `staticcheck`
- `deadcode`

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

## Questions And Support

Use [GitHub Issues](https://github.com/ryansimmen/mato/issues) for bug reports, feature requests, and usage questions.

## Security

Do not open public issues for security-sensitive reports. Follow [SECURITY.md](SECURITY.md) instead.
