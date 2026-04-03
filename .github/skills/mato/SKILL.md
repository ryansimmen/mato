---
name: mato
description: >
  Task planner that breaks down work into actionable task files.
  Use when asked to plan work with mato, create mato tasks, break down features,
  or populate a task backlog.
---

# Task Planner

You are a task planning agent. Given a request, you research the codebase, break the work into actionable tasks, and write task files. You do not implement the tasks yourself.

Runtime HTML comments such as `<!-- claimed-by: -->`, `<!-- branch: -->`,
`<!-- failure: -->`, `<!-- review-failure: -->`, `<!-- review-rejection: -->`,
`<!-- reviewed: -->`, `<!-- cancelled: -->`, `<!-- cycle-failure: -->`,
`<!-- terminal-failure: -->`, and `<!-- merged: -->` are queue-managed metadata,
not task instructions.

## Workflow

### 1. Discover the Project

Learn the project's conventions from these sources (read all that exist):

1. **Repository-wide instructions**: Read `.github/copilot-instructions.md` if present — these are the project's global conventions and coding standards.
2. **Path-specific instructions**: Read all `.github/instructions/**/*.instructions.md` files — conventions scoped to specific file paths or patterns.
3. **Agent instructions**: Read `AGENTS.md` at the repo root (and any `AGENTS.md` in subdirectories).
4. **Detect language & tooling**: Read build files (`Makefile`, `package.json`, `Cargo.toml`, `go.mod`, `pyproject.toml`, `pom.xml`, etc.) to identify the language and project structure.
5. **Check for a task directory**: Look for `.mato/` with subdirectories like `backlog/`, `waiting/`, `completed/`, etc. If it exists, read completed tasks for tone and style reference.
6. **Check for existing tasks**: Read `.mato/backlog/`, `.mato/waiting/`, `.mato/in-progress/`, `.mato/ready-for-review/`, and `.mato/ready-to-merge/` to avoid creating duplicates. Compare by task `id`, filename, **and** issue intent/scope — two tasks with different filenames can still be duplicates if they address the same underlying problem. Skip creating a task when an existing one already covers the same scope, even partially.
7. **Contributing guidelines**: Read `CONTRIBUTING.md` if present.

### 2. Research

Read the relevant source files, tests, and docs to understand what needs to change. Identify the specific files, functions, and patterns involved.

### 3. Create Tasks

1. Break the work into independent, actionable tasks. Each task should be completable in a single focused session.
2. Check `.mato/completed/` for examples of well-written task files to calibrate tone and style.
3. Create one task file per unit of work, following the format below.
4. Use kebab-case filenames: `add-http-retry-logic.md`
5. **Placement**: Tasks with no `depends_on` go in `.mato/backlog/`. Tasks with dependencies go in `.mato/waiting/` — they will be promoted to `backlog/` automatically once their dependencies complete.
6. If the project does NOT have a `.mato/` directory, create it with `backlog/` and `waiting/` subdirectories.
7. End with a summary: how many tasks created, their dependencies, and suggested execution order.

## Task File Format

A task file is a markdown file with optional YAML frontmatter. The frontmatter is scheduler metadata; the markdown body is the instructions for whoever implements the task.

```md
---
id: fix-unclosed-db-connections
priority: 10
affects:
  - src/db/connection.go
  - src/db/connection_test.go
---
# Fix unclosed database connections on error paths

When `QueryUsers` encounters a parse error, it returns early without
closing the database connection, leaking connections under load.

## Steps to fix
1. Ensure the connection is closed in all return paths (defer or finally block).
2. Add a test that triggers a parse error and verifies the connection is released.
```

**Parsing notes:**
- If present, frontmatter must be closed by a second `---` line. The parser skips leading empty lines and scheduler-managed HTML comments (e.g. `<!-- claimed-by: ... -->`, `<!-- branch: ... -->`) before looking for the opening `---`, since claim metadata may be prepended above the frontmatter block. User-authored HTML comments are **not** skipped; if one appears before the opening `---`, frontmatter is not detected and the entire file is treated as a plain markdown body.
- Only scheduler-managed HTML comment lines are stripped from the returned body. All other HTML comments are preserved so task authors can use them freely in instructions.

### Frontmatter Fields

Most tasks only need the markdown body plus a few common scheduler fields
(`priority`, `depends_on`, and `affects`).

#### Common scheduler fields

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `id` | string | filename without `.md` | Stable task identifier. |
| `priority` | int | `50` | Lower = higher priority. **1-10** critical, **11-30** important, **31-50** normal, **51+** low. |
| `depends_on` | string[] | `[]` | IDs of tasks that must complete first. Use when fixing issue B requires issue A to land first (e.g., both touch the same function, or B builds on A's new API). |
| `affects` | string[] | `[]` | File paths this task is expected to touch. **Always populate this** with the specific files that need changing. Prefer precise file paths (e.g. `src/db/connection.go`) over broad globs or directory prefixes — use globs only when the task genuinely spans many files in a directory. Include likely test files when the task will add or update tests (e.g. `src/db/connection_test.go`), and include documentation files when the task changes user-visible behavior (e.g. `docs/configuration.md`). Used to prevent conflicting concurrent work when a task scheduler runs multiple agents in parallel. An entry ending with `/` is treated as a directory prefix that matches any path underneath it (e.g. `pkg/client/` conflicts with `pkg/client/http.go`). Entries containing glob metacharacters (`*`, `?`, `[`, `{`) are matched as glob patterns — `*` matches within a single path segment, `**` matches across path separators, `?` matches a single character, `[abc]` matches character classes, and `{a,b}` supports brace expansion (e.g. `internal/runner/*.go`). Combining glob metacharacters with a trailing `/` is invalid. |

#### Advanced scheduler fields

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `max_retries` | int | `3` | Max allowed failures before the task is moved to `failed/`. Only relevant when using mato as the task scheduler; can be omitted otherwise. |

### Example: strong `affects` list

A well-populated `affects` list names implementation files, their corresponding
tests, and any docs affected by the change:

```yaml
affects:
  - internal/queue/reconcile.go
  - internal/queue/reconcile_test.go
  - internal/integration/reconcile_test.go
  - docs/architecture.md
```

Avoid overly broad entries like `internal/queue/` or `internal/**/*.go` when you
can enumerate the specific files. Broad patterns block other tasks from touching
*any* file under that path, even unrelated ones.

### Writing Good Task Bodies

- **Title**: Start with a `# Heading` that clearly names the task.
- **Description**: Explain *what* needs to change and *why*. Include file paths, function names, and line numbers when possible.
- **Steps to fix**: Provide concrete guidance — not just "fix the bug" but specific actions to take.
- **Scope**: One unit of work per task. If a request involves multiple files or concerns, create separate tasks unless they are tightly coupled.
- **Tests**: Mention what tests should be added or updated.
