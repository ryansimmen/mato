# AGENTS.md

Guide for AI coding agents working in the `mato` codebase (Multi Agent Task Orchestrator).
Go 1.26, module name `github.com/ryansimmen/mato`, CLI built with `spf13/cobra`.

## Build / Lint / Test Commands

```bash
# Build
go build ./...                          # type-check all packages
make build                              # compile binary to bin/mato
make verify                             # run build, vet, lint, deadcode, and uncached tests

# Lint
go vet ./...                            # built-in static analysis
make lint                               # golangci-lint (bodyclose, copyloopvar, durationcheck, errcheck, errorlint, govet, ineffassign, makezero, misspell, nilerr, nolintlint, staticcheck, unconvert, unused, wastedassign, gofmt, goimports)
make deadcode                           # detect unreachable exported code and unused symbols

# Format
go fmt ./...                            # or: make fmt

# Test — all
make test-fast                         # fast local unit tests, skips integration and race detector
go test -race ./...                     # or: make test
go test -race -count=1 ./...            # or: make test-race
go test -count=1 ./...                  # disable test cache (use before committing)

# Test — single package
go test -race ./internal/queue/...

# Test — single test function
go test -race -run TestSafeRename_MissingSource ./internal/queue/...

# Test — integration only
go test -race -v ./internal/integration/...   # or: make integration-test

# Full verification (run before every commit)
make verify
```

## Project Layout

```
cmd/mato/          CLI entrypoint (cobra root command)
internal/          All library packages:
  atomicwrite/     Atomic file write utilities
  config/          Repository-local .mato.yaml loading
  configresolve/   Repo config resolution + source attribution
  dag/             Dependency graph analysis (Kahn + Tarjan)
  dirs/            Queue directory name constants
  doctor/          Health checks for repo and task queue
  frontmatter/     YAML frontmatter parser
  graph/           Dependency graph visualization
  git/             Git helpers (clone, checkout, commit, push)
  history/         Durable outcome timeline for mato log
  identity/        Agent ID generation (8-char hex)
  inspect/         Single-task troubleshooting command
  integration/     Integration tests (package integration_test)
  lockfile/        PID-based lock files
  merge/           Squash-merge queue
  messaging/       Inter-agent messaging protocol
  pause/           Durable .paused sentinel management
  process/         Process detection via /proc
  queue/           Task queue management + claiming
  queueview/       Read-only queue index and diagnostics
  runner/          Agent lifecycle, Docker, embedded prompts
  runtimedata/     Runtime sidecar state and cleanup
  setup/           Repository bootstrap and init workflow
  status/          mato status command
  taskfile/        Task file helpers (metadata parsing, active-affects collection)
  testutil/        Shared test helpers (SetupRepo, SetupRepoWithTasks)
  timeutil/        Shared time-formatting helpers
  ui/              Shared CLI formatting helpers
docs/              Architecture, configuration, messaging, task-format docs
skills/mato/SKILL.md             Task planning skill (published via `gh skill`)
```

## Code Style

### Imports

Three groups separated by blank lines. Standard library first, then internal
(`github.com/ryansimmen/mato/internal/...`) and third-party — the relative order of the latter two
groups is not strictly enforced but each group is alphabetically sorted.

```go
import (
    "fmt"
    "os"

    "github.com/ryansimmen/mato/internal/queue"

    "github.com/spf13/cobra"
)
```

### Naming

| Element              | Convention    | Example                            |
|----------------------|---------------|------------------------------------|
| Exported functions   | PascalCase    | `ParseTaskFile`, `RecoverOrphanedTasks` |
| Unexported functions | camelCase     | `pollBackoff`, `crossDeviceMove`   |
| Exported types       | PascalCase    | `TaskMeta`, `ClaimedTask`          |
| Unexported types     | camelCase     | `envConfig`, `runContext`          |
| Sentinel errors      | `errXxx` var  | `errSquashMergeConflict`           |
| Regex vars           | camelCase+Re  | `reviewedRe`, `branchUnsafeRe`    |
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

- **Atomic writes**: Use `atomicwrite.WriteFile` or `atomicwrite.WriteFunc` from `internal/atomicwrite/`. Exception: failure record appends use `O_APPEND`.
- **Atomic moves (TOCTOU-safe)**: `queue.AtomicMove(src, dst)` for all file moves. Uses `os.Link` + `os.Remove`; handles `EXDEV` cross-device fallback.
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
   - Do not ask whether to run routine verification or formatting commands;
      run the relevant commands proactively and report the results.
3. **Update docs** — If behavior changed, update this file (`AGENTS.md`) and any affected
   docs: `README.md`, `docs/architecture.md`, `docs/task-format.md`, `docs/messaging.md`,
   `docs/configuration.md`. If task format changed, also update `skills/mato/SKILL.md`.
4. **Verify** — `make verify`
5. **Commit** — Conventional commit messages (`feat:`, `fix:`, `docs:`, etc.).

### Pull Request Safety

- Before editing or describing an existing PR, verify it is **open** with `gh pr view` and confirm its `headRefName` matches the branch you are working on.
- Never update the title/body of a merged or closed PR just because its number or past branch name seems related to the current work.
- If the current branch has no open PR, create a new one instead of reusing an old merged PR.
- When asked to create a PR, default to the current checked-out branch. Do not create a new branch, cherry-pick commits, rebase, or otherwise reshape PR scope unless the user explicitly asks for that or the current branch cannot be used. If the branch contains extra commits, report that and ask whether to proceed as-is or make a clean branch.

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
