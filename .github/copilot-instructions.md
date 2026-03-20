# Copilot Instructions for mato

## Development Workflow

When asked to implement a change, follow this process:

1. **Research** — Explore the codebase and understand the problem before making changes.

2. **Implement** — Read relevant source files first, make changes, and run `go build ./...` && `go test ./...` before finishing.

3. **Update docs** — If the change affects behavior, update the relevant docs:
   - `README.md` — user-facing quickstart and overview
   - `docs/architecture.md` — system design and code structure
   - `docs/task-format.md` — task file format reference (also update `.github/skills/mato/SKILL.md` if changing the task format — it's distributed standalone via `scripts/install-skill.sh`)
   - `docs/messaging.md` — messaging protocol
   - `docs/configuration.md` — CLI flags, env vars, Makefile targets

4. **Verify** — Run the full test suite before committing:
   ```bash
   go build ./...
   go vet ./...
   go test -count=1 ./...
   ```

5. **Commit** — Use conventional commit messages.

## Code Conventions

- **Go project layout**: `cmd/mato/` for CLI, `internal/` for packages
- **Tests alongside source** — each `foo.go` has `foo_test.go` in the same package
- **Integration tests** in `internal/integration/` — use `package integration_test`
- **Prompt tests** execute actual bash commands from `task-instructions.md` in real git repos
- **Race detector** — integration tests should pass with `go test -race`
- **Minimal dependencies** — only external dep is `gopkg.in/yaml.v3`; everything else uses Go standard library. Do not add third-party packages without strong justification.
- **Naming conventions** — task files: `kebab-case.md`, branches: `task/<sanitized-name>`, agent IDs: 8-char hex, queue manifest: `.queue`

## Error Handling & Logging

- **Error wrapping**: `fmt.Errorf("context: %w", err)` — always add context, always use `%w` for wrappable errors
- **Sentinel errors**: Defined as unexported package-level `var` (e.g., `errTaskBranchNotPushed`), matched via `errors.Is`
- **Non-fatal warnings**: `fmt.Fprintf(os.Stderr, "warning: ...\n")` and continue — never fatal for recoverable issues
- **Progress output**: `fmt.Printf(...)` to stdout for user-facing info
- **No logging library** — plain `fmt` only; no structured logging

## File I/O & Timestamps

- **Atomic writes**: New file writes should use "write to temp file in same dir, then `os.Rename`" to prevent partial writes. Known exceptions: failure record appends use `O_APPEND` (append-only, not full rewrites), and `recoverStuckTask` uses `os.Link` + `os.Remove` to avoid TOCTOU races.
- **Timestamps**: Always UTC — call `.UTC()` on `time.Now()`. Store as RFC3339 in task metadata HTML comments.
- **HTML comment metadata**: Runtime state tracked via `<!-- claimed-by: ... -->`, `<!-- failure: ... -->`, `<!-- branch: ... -->`, `<!-- merged: ... -->`, `<!-- reviewed: ... -->`, `<!-- review-rejection: ... -->` — these are the core state mechanism for task lifecycle

## Testing Requirements

- All changes must pass `go build ./...` and `go test ./...`
- New features need unit tests in the relevant package
- Cross-package workflows need integration tests in `internal/integration/`
- Changes to `task-instructions.md` need prompt validation tests
- Edge cases and race conditions should be tested explicitly
- Run `make integration-test` for integration tests with race detector

## Key Architecture Facts

- Tasks are markdown files with optional YAML frontmatter
- Host manages queue: `waiting/` → `backlog/` → `in-progress/` → `ready-for-review/` → `ready-to-merge/` → `completed/` (or `failed/` when retry budget is exhausted or task branch is missing). Review rejection sends tasks `ready-for-review/` → `backlog/` for retry.
- Agents run in Docker, push task branches only, never the target branch
- Review agents evaluate task branches before merge — see `internal/runner/review-instructions.md`. Rejections do not count against `max_retries`; feedback is injected via `MATO_REVIEW_FEEDBACK` on retry.
- Host merge queue squash-merges task branches serially
- `affects:` metadata prevents concurrent conflicting tasks (checked against in-progress/, ready-for-review/, and ready-to-merge/)
- Conflict-deferred tasks stay in backlog/ but are excluded from `.queue` manifest
- `max_retries` is enforced by the host: `queue.SelectAndClaimTask` checks before launching an agent, and host merge queue checks after merge failures
- Task selection, claiming, retry-budget checks, and intent messages are handled by Go (host-side) before the agent container launches
- The agent prompt receives pre-resolved task info via `MATO_TASK_FILE`, `MATO_TASK_BRANCH`, `MATO_TASK_TITLE`, `MATO_TASK_PATH` env vars
- `<!-- branch: ... -->` in task file tells merge queue which branch to merge
- **Lock files**: `.locks/` directory holds agent PIDs (`.locks/<agentID>.pid`), merge lock (`.locks/merge.lock`), and review locks (`review-<filename>.lock`). Identity format is `PID:starttime` for stale lock detection via `/proc/<pid>/stat`.
- **Messaging**: `.tasks/messages/` has `events/`, `presence/`, and `completions/` subdirs. Message types: `intent`, `progress`, `conflict-warning`, `completion`. The host writes file claims (`file-claims.json`) from active tasks' `affects` metadata.
- **Docker defaults**: Image `ubuntu:24.04` (override via `MATO_DOCKER_IMAGE`), model `claude-opus-4.6` (override via `MATO_DEFAULT_MODEL` env var or by passing `--model` in copilot args). Key injected env vars: `MATO_AGENT_ID`, `MATO_TASK_FILE`, `MATO_TASK_BRANCH`, `MATO_TASK_TITLE`, `MATO_TASK_PATH`, `MATO_MESSAGING_ENABLED`, `MATO_PREVIOUS_FAILURES`, `MATO_REVIEW_FEEDBACK`, `MATO_FILE_CLAIMS`, `MATO_DEPENDENCY_CONTEXT`.
- **`mato status`**: Shows queue overview, active agents, in-progress/deferred/blocked tasks, recent completions and messages. See `internal/status/status.go`.
- **Skills**: `.github/skills/mato/SKILL.md` is a standalone task planning skill installed via `scripts/install-skill.sh` to `~/.copilot/skills/`.
