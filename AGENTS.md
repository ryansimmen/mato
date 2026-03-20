# AGENTS.md

Guide for AI coding agents working in the `mato` codebase (Multi Agent Task Orchestrator).
Go 1.26, module name `mato`, CLI built with `spf13/cobra`.

## Build / Lint / Test Commands

```bash
# Build
go build ./...                          # type-check all packages
make build                              # compile binary to bin/mato

# Lint
go vet ./...                            # built-in static analysis
make lint                               # golangci-lint (errcheck, gosimple, govet, ineffassign, staticcheck, unused, gofmt)

# Format
go fmt ./...                            # or: make fmt

# Test — all
go test -race ./...                     # or: make test
go test -count=1 ./...                  # disable test cache (use before committing)

# Test — single package
go test -race ./internal/queue/...

# Test — single test function
go test -race -run TestSafeRename_MissingSource ./internal/queue/...

# Test — integration only
go test -race -v ./internal/integration/...   # or: make integration-test

# Full verification (run before every commit)
go build ./... && go vet ./... && go test -count=1 ./...
```

## Project Layout

```
cmd/mato/          CLI entrypoint (cobra root command)
internal/          All library packages:
  frontmatter/     YAML frontmatter parser
  git/             Git helpers (clone, checkout, commit, push)
  identity/        Agent ID generation (8-char hex)
  integration/     Integration tests (package integration_test)
  lockfile/        PID-based lock files
  merge/           Squash-merge queue
  messaging/       Inter-agent messaging protocol
  process/         Process detection via /proc
  queue/           Task queue management + claiming
  runner/          Agent lifecycle, Docker, embedded prompts
  status/          mato status command
  testutil/        Shared test helpers (SetupRepo, SetupRepoWithTasks)
docs/              Architecture, configuration, messaging, task-format docs
.github/skills/mato/SKILL.md     Task planning skill
```

## Code Style

### Imports

Three groups separated by blank lines. Standard library first, then internal
(`mato/internal/...`) and third-party — the relative order of the latter two
groups is not strictly enforced but each group is alphabetically sorted.

```go
import (
    "fmt"
    "os"

    "mato/internal/queue"

    "github.com/spf13/cobra"
)
```

### Naming

| Element              | Convention    | Example                            |
|----------------------|---------------|------------------------------------|
| Exported functions   | PascalCase    | `ParseTaskFile`, `RecoverOrphanedTasks` |
| Unexported functions | camelCase     | `pollBackoff`, `safeRename`        |
| Exported types       | PascalCase    | `TaskMeta`, `ClaimedTask`          |
| Unexported types     | camelCase     | `dockerConfig`, `queueEntry`       |
| Sentinel errors      | `errXxx` var  | `errSquashMergeConflict`           |
| Regex vars           | camelCase+Re  | `claimedByRe`, `branchUnsafeRe`   |
| Constants            | camelCase     | `defaultAgentTimeout`, `basePollInterval` |
| Files                | lowercase     | `runner.go`, `frontmatter.go`      |
| Packages             | single word   | `queue`, `merge`, `lockfile`       |
| Task files           | kebab-case.md | `add-dark-mode.md`                 |
| Branches             | `task/<name>` | `task/add-dark-mode`               |
| JSON/YAML tags       | snake_case    | `json:"sent_at"`, `yaml:"depends_on"` |

### Error Handling

- **Wrap with context**: `fmt.Errorf("read task file %s: %w", path, err)` — lowercase verb phrase, always `%w`.
- **Sentinel errors**: Unexported package-level `var errXxx = errors.New(...)`, matched with `errors.Is()`.
- **Non-fatal warnings**: `fmt.Fprintf(os.Stderr, "warning: ...\n")` and continue.
- **Progress output**: `fmt.Printf(...)` to stdout.
- No logging library — plain `fmt` only; third-party loggers acceptable when they add clear value.

### File I/O

- **Atomic writes**: Write to temp file in same directory, then `os.Rename`. See `writeFileAtomically` in `queue.go`. Exception: failure record appends use `O_APPEND`.
- **Atomic moves (TOCTOU-safe)**: `os.Link` + `os.Remove` instead of rename. See `safeRename` in `queue.go`.
- **Permissions**: `0o644` for files, `0o755` for directories.
- **Timestamps**: Always UTC (`time.Now().UTC()`), stored as RFC3339.

### Types and Patterns

- `map[string]struct{}` for set semantics.
- Function variables for test hooks (e.g., `var claimPrependFn = prependClaimedBy`).
- `context.Context` threaded through function chains for cancellation.
- `//go:embed` for markdown instruction files in `runner/`.

### Comments

- Package comments: `// Package xxx ...` on line before `package`.
- Exported symbols: `// FuncName verb-phrase...` (Go convention).
- All `//` style — no `/* */` block comments.

## Testing Conventions

- Standard `testing` package only — no third-party test frameworks.
- Tests live alongside source (`foo_test.go` in same package).
- Integration tests in `internal/integration/` use `package integration_test`.
- **Table-driven tests** with `t.Run`:
  ```go
  tests := []struct {
      name string
      // fields...
  }{
      {"descriptive name", /* ... */},
  }
  for _, tt := range tests {
      t.Run(tt.name, func(t *testing.T) { /* ... */ })
  }
  ```
- Test naming: `TestFunctionName_Scenario` (e.g., `TestSafeRename_MissingSource`).
- Use `t.TempDir()` for temp directories, `t.Helper()` in helpers.
- Shared helpers in `internal/testutil/` (`SetupRepo`, `SetupRepoWithTasks`).
- Race detector always on: `go test -race`.
- No mocks — function variable hooks for test injection.
- New features need unit tests in the relevant package.
- Cross-package workflows need integration tests in `internal/integration/`.
- Changes to `task-instructions.md` need prompt validation tests in `internal/integration/`.
- Edge cases and race conditions should be tested explicitly.

## Development Workflow

1. **Research** — Read relevant source before changing anything.
2. **Implement** — Make changes, run `go build ./...` and `go test ./...`.
3. **Update docs** — If behavior changed, update this file (`AGENTS.md`) and any affected
   docs: `README.md`, `docs/architecture.md`, `docs/task-format.md`, `docs/messaging.md`,
   `docs/configuration.md`. If task format changed, also update `.github/skills/mato/SKILL.md`.
4. **Verify** — `go build ./... && go vet ./... && go test -count=1 ./...`
5. **Commit** — Conventional commit messages (`feat:`, `fix:`, `docs:`, etc.).

## Key Architecture

- Tasks are markdown files with YAML frontmatter, managed in a filesystem queue.
- Agents run in Docker, push task branches; the host squash-merges them serially.
- See `docs/architecture.md` for system design and `docs/task-format.md` for task file format.

## Maintaining This File

Keep AGENTS.md focused on build commands, code style, and testing conventions —
the things an agent needs to write correct code. Detailed architecture, runtime
behavior, and configuration belong in `docs/`. When adding a new convention or
pattern, add it here; when documenting how a subsystem works, put it in the
relevant doc and reference it from the Development Workflow section above.
