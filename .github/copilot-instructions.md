# Copilot Instructions for mato

## Development Workflow

When asked to implement a change, follow this process:

1. **Research** — Use subagents to explore the codebase and understand the problem deeply before making changes. For complex topics, launch multiple parallel research agents.

2. **Plan** — Create a structured plan in `plan.md` (session workspace). List specific tasks with dependencies. Double-check the plan before proceeding.

3. **Implement** — Use subagents for parallel work on independent packages. Each agent should read relevant source files first, make changes, and run `go build ./...` && `go test ./...` before finishing.

4. **Update docs** — If the change affects behavior, update the relevant docs:
   - `README.md` — user-facing quickstart and overview
   - `docs/architecture.md` — system design and code structure
   - `docs/task-format.md` — task file format reference
   - `docs/messaging.md` — messaging protocol
   - `docs/configuration.md` — CLI flags, env vars, Makefile targets

5. **Verify** — Run the full test suite before committing:
   ```bash
   go build ./...
   go vet ./...
   go test -count=1 ./...
   ```

6. **Commit** — Use conventional commit messages. Always include the co-author trailer.

## Code Conventions

- **Go project layout**: `cmd/mato/` for CLI, `internal/` for packages
- **No external dependencies** — stdlib only, no third-party modules
- **Tests alongside source** — each `foo.go` has `foo_test.go` in the same package
- **Integration tests** in `internal/integration/` — use `package integration_test`
- **Prompt tests** execute actual bash commands from `task-instructions.md` in real git repos
- **Race detector** — integration tests should pass with `go test -race`

## Testing Requirements

- All changes must pass `go build ./...` and `go test ./...`
- New features need unit tests in the relevant package
- Cross-package workflows need integration tests in `internal/integration/`
- Changes to `task-instructions.md` need prompt validation tests
- Edge cases and race conditions should be tested explicitly
- Run `make integration-test` for integration tests with race detector

## Subagent Usage

- Use `general-purpose` agents for implementation work
- Use `explore` agents for quick codebase questions
- Launch parallel agents for independent packages (no file conflicts)
- Each agent must read relevant source files before making changes
- Each agent must run build + test before finishing

## Key Architecture Facts

- Tasks are markdown files with optional YAML frontmatter
- Host manages queue: `waiting/` → `backlog/` → `in-progress/` → `ready-to-merge/` → `completed/`
- Agents run in Docker, push task branches only, never the target branch
- Host merge queue squash-merges task branches serially
- `affects:` metadata prevents concurrent conflicting tasks (checked against in-progress/ and ready-to-merge/)
- Conflict-deferred tasks stay in backlog/ but are excluded from `.queue` manifest
- `max_retries` is enforced by both agent prompt and host merge queue
- `<!-- branch: ... -->` in task file tells merge queue which branch to merge
