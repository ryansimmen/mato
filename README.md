# Multi Agent Task Orchestrator (mato)

Runs autonomous Copilot agents against a filesystem-backed task queue in Docker. Agents claim work, coordinate through `.tasks/`, commit on task branches, and the host pushes task branches and squash-merges completed work into the target branch. Every task branch is automatically reviewed by an AI review agent before merging. The review agent checks for bugs, logic errors, regressions, and convention violations. See [Architecture](docs/architecture.md) for details.

## Requirements

- Go
- Docker
- GitHub CLI
- Copilot CLI

## Quick Start

```bash
# Install mato binary and the mato Copilot skill (~/.copilot/skills/)
make install

# Authenticate with Copilot
copilot login

# cd into the target repo
cd /path/to/repo

# Create a ready task in backlog/
mkdir -p .tasks/backlog
cat > .tasks/backlog/add-retry-logic.md << 'EOF'
---
id: add-retry-logic
priority: 10
affects: [pkg/client/http.go]
tags: [backend]
---
# Add retry logic to HTTP client

Wrap fetchData in a retry loop with exponential backoff and add tests.
EOF

# Run the orchestrator (stays running and keeps polling for work)
mato

# In a separate terminal, inspect queue health
mato status

# Check system prerequisites and queue integrity
mato doctor
```

Useful flags:

- `--repo <path>`: target repository (defaults to the current directory); empty and whitespace-only values are rejected
- `--branch <name>`: merge target branch (defaults to `mato`); empty and whitespace-only values are rejected
- `--tasks-dir <path>`: custom task queue location (defaults to `<repo>/.tasks`); empty and whitespace-only values are rejected
- `--dry-run[=<bool>]`: validate queue setup without launching Docker containers (defaults to `false`; bare `--dry-run` is equivalent to `--dry-run=true`)

Arguments after a `--` separator are always forwarded to the Copilot CLI without
interpretation — even `--help` and `-h` (e.g., `mato -- --help` forwards
`--help` to Copilot instead of showing mato's own usage).

## Task Files

Task files are markdown with optional YAML frontmatter. Lower `priority` values run first; if omitted, the default priority is `50`.

```yaml
---
id: add-retry-logic
priority: 10
depends_on: [setup-http-client]
affects: [pkg/client/http.go]
tags: [backend]
---
```

After the frontmatter, write normal markdown instructions for the agent.

### Dependencies and priority

- Put blocked tasks in `waiting/`.
- `depends_on` entries refer to task IDs.
- On each loop, completed dependencies promote a task from `waiting/` to `backlog/`.
- Lower numbers mean higher priority.
- `affects` is used for simple conflict prevention: if two backlog tasks have overlapping entries, the lower-priority task is excluded from the `.queue` manifest until the conflict clears. Entries are compared using three matching modes — exact strings are compared literally, entries ending with `/` are treated as directory prefixes that match any path underneath them (e.g. `pkg/client/` conflicts with `pkg/client/http.go`), and entries containing glob metacharacters (`*`, `?`, `[`, `{`) are matched as glob patterns using `doublestar` syntax (e.g. `internal/runner/*.go` conflicts with `internal/runner/task.go`). Conflict-deferred tasks remain in `backlog/` (they are not moved to `waiting/`).

## Queue Layout

```text
<repo>/.tasks/
├── waiting/         # blocked tasks waiting on dependencies
├── backlog/         # ready to run
├── in-progress/     # claimed by an active agent
├── ready-for-review/# completed by agent, waiting for AI review
├── ready-to-merge/  # approved by review agent, waiting for host merge
├── completed/       # merged successfully
├── failed/          # exceeded retry limit
├── messages/
│   ├── events/      # coordination events and status updates
│   ├── completions/ # host-written completion details for merged tasks
│   └── presence/    # host-managed agent presence tracking
└── .locks/          # PID locks for agents and merge queue
```

Tasks that accumulate `max_retries` failure records (default 3) are moved to `failed/`.

## How It Works

1. Add tasks to `waiting/` or `backlog/`.
2. Mato promotes ready tasks into `backlog/`, orders them by priority, and defers overlapping `affects` conflicts (exact paths, directory prefixes, and glob patterns).
3. An agent claims a backlog task, works in an isolated clone on a host-created `task/<name>` branch, and commits. The host pushes the branch after the agent exits.
4. Agents communicate through `.tasks/messages/` so concurrent runs can share intent and completion events.
5. A review agent automatically evaluates each completed task branch. Approved tasks advance to `ready-to-merge/`; rejected tasks return to `backlog/` with feedback for the next attempt.
6. The host merge queue processes `ready-to-merge/` and squash-merges finished task branches into the target branch.
7. Tasks move to `completed/` on success. Missing branches move to `failed/`, merge conflicts requeue to `backlog/` for a fresh attempt, and push failures are retried in `ready-to-merge/`.

If the queue is empty, mato keeps polling until new work appears. The loop exits cleanly on `Ctrl+C`.

## Running Multiple Agents

Start multiple `mato` processes in separate terminals to process tasks in parallel. Task claiming is atomic (`mv`), active agents are tracked in `.locks/`, and orphaned `in-progress/` tasks are recovered automatically on the next loop.

## Status Command

`mato status` prints a terminal-friendly snapshot of the queue:

- counts for `waiting/`, `backlog/`, `in-progress/`, `ready-for-review/`, `ready-to-merge/`, `completed/`, and `failed/`
- runnable backlog in execution order (priority-sorted, conflict-deferred tasks excluded)
- active agents from `.locks/`
- waiting tasks with dependency progress
- conflict-deferred tasks with blocking details
- the last 5 coordination messages from `.tasks/messages/events/`

The runnable backlog shows what the host will claim next, in the same priority
order used by `.tasks/.queue`. Use `--json` to get the same ordered list as
`runnable_backlog` in the JSON output.

Use `--watch` (`-w`) to continuously refresh the display. The `--interval` flag
sets the refresh period (default `2s`). The interval must be a positive duration;
zero or negative values are rejected with an error.

## Graph Command

`mato graph` visualizes the task dependency topology:

```bash
# Text output (default)
mato graph

# Graphviz DOT pipeline
mato graph --format dot | dot -Tpng > graph.png

# Machine-readable JSON
mato graph --format json

# Include completed and failed tasks
mato graph --all
```

The graph reuses `PollIndex` and `DiagnoseDependencies` to show dependency
edges, blocked tasks, cycles, and hidden (off-graph) dependencies. Output is
read-only and makes no filesystem changes.

## Doctor Command

`mato doctor` runs a structured health check across the repository and task queue:

- git repository detection and configuration
- host tool availability (git, docker, gh, copilot)
- Docker daemon connectivity
- queue directory layout (missing or unexpected directories)
- task file parsing (frontmatter errors, invalid globs)
- lock and orphan detection (stale PID locks, orphaned in-progress tasks)
- dependency integrity (cycles, unknown IDs, ambiguous prefixes, duplicates)

Use `--fix` to auto-repair safe, idempotent issues such as missing directories,
stale locks, and orphaned tasks. Use `--format json` for machine-readable output.
The `--only` flag accepts a comma-separated list of check categories to run
(`git`, `tools`, `docker`, `queue`, `tasks`, `locks`, `deps`);
non-selected checks appear as skipped. Exit code 0 means healthy, 1 means
warnings only, and 2 means errors were found.

## Retry Command

`mato retry` requeues failed tasks back to backlog for another attempt:

```bash
# Retry a single task
mato retry fix-login-bug

# Retry multiple tasks
mato retry fix-login-bug add-dark-mode
```

The command strips all failure markers (`<!-- failure: -->`, `<!-- review-failure: -->`,
`<!-- cycle-failure: -->`, `<!-- review-rejection: -->`, `<!-- terminal-failure: -->`)
from the task file and writes the cleaned content to `backlog/`. If the task already
exists in `backlog/`, the command prints an error and leaves the `failed/` copy
unchanged (no data loss).

## Docker

`mato` launches an `ubuntu:24.04` container by default (override with `MATO_DOCKER_IMAGE`). The container mounts a temporary clone at `/workspace` plus the original repo path for local `git fetch`/`git push`, mounts host `copilot`, `git`, `gh`, and credentials/config, runs as your UID/GID, and forwards extra Copilot CLI args such as:

```bash
mato --model gpt-5.3-codex
```

## Notes

- Task instructions are embedded in the binary (`task-instructions.md`).
- The default merge target branch is `mato`.
- Run `go test ./...` (or `make test`) to run the test suite.

## Documentation

- [Architecture](docs/architecture.md) — system design, host loop, agent lifecycle, merge queue
- [Task Format](docs/task-format.md) — frontmatter fields, defaults, examples
- [Messaging](docs/messaging.md) — inter-agent coordination protocol
- [Configuration](docs/configuration.md) — CLI flags, environment variables, Docker setup
