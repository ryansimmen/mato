# Multi Agent Task Orchestrator (mato)

Runs autonomous Copilot agents against a filesystem-backed task queue in Docker. Agents claim work, coordinate through `.mato/`, commit on task branches, and the host pushes task branches and squash-merges completed work into the target branch. Every task branch is automatically reviewed by an AI review agent before merging. The review agent checks for bugs, logic errors, regressions, and convention violations. See [Architecture](docs/architecture.md) for details.

## Requirements

- Go
- Docker
- GitHub CLI
- Copilot CLI

## Quick Start

```bash
# Install mato binary and the mato skill (Copilot, plus OpenCode when installed)
make install

# Authenticate with Copilot
copilot login

# cd into the target repo
cd /path/to/repo

# Initialize the repo for mato
mato init

# Create a ready task in backlog/
cat > .mato/backlog/add-retry-logic.md << 'EOF'
---
id: add-retry-logic
priority: 10
affects: [pkg/client/http.go]
---
# Add retry logic to HTTP client

Wrap fetchData in a retry loop with exponential backoff and add tests.
EOF

# Run the orchestrator (stays running and keeps polling for work)
mato run

# In a separate terminal, inspect queue health
mato status

# Check system prerequisites and queue integrity
mato doctor
```

Useful flags:

- `--repo <path>`: target repository (defaults to the current directory)
- `mato run --branch <name>`: merge target branch (defaults to `mato`)
- `mato run --dry-run`: validate queue setup without launching Docker containers
- `mato run --task-model`, `--review-model`: override task and review agent models
- `mato run --task-reasoning-effort`, `--review-reasoning-effort`: override task and review reasoning effort

You can also set `MATO_BRANCH` for a host-side branch default that overrides `.mato.yaml` but is still overridden by `--branch`.

Use `mato init` to bootstrap `.mato/`, messaging directories, `.gitignore`, and the target branch without requiring Docker or Copilot. The command is idempotent, so rerunning it is safe. When the branch is missing locally, `mato init` tells you whether it reused a local branch, created from live `origin/<branch>`, created from current `HEAD` because the remote branch was missing, or fell back to a cached remote-tracking ref because `origin` was unavailable.

You can also add an optional `.mato.yaml` at the repository root to persist defaults such as `branch`, `docker_image`, `task_model`, `review_model`, `task_reasoning_effort`, `review_reasoning_effort`, `agent_timeout`, and `retry_cooldown`. CLI flags still win over config, and host env vars still win over both.

## Task Files

Task files are markdown with optional YAML frontmatter. Lower `priority` values run first; if omitted, the default priority is `50`.

```yaml
---
id: add-retry-logic
priority: 10
depends_on: [setup-http-client]
affects: [pkg/client/http.go]
---
```

After the frontmatter, write normal markdown instructions for the agent.

### Dependencies and priority

- Put blocked tasks in `waiting/`.
- `depends_on` entries refer to task IDs.
- `depends_on` is authoritative regardless of directory placement. Tasks with unsatisfied dependencies are dependency-blocked even if they were manually or automatically placed in `backlog/`.
- On each loop, completed dependencies promote a task from `waiting/` to `backlog/`. If a dependency-blocked task is found in `backlog/`, mato moves it back to `waiting/` before writing `.queue` or claiming work.
- Lower numbers mean higher priority.
- `affects` is used for simple conflict prevention: if two backlog tasks have overlapping entries, the lower-priority task is excluded from the `.queue` manifest until the conflict clears. Entries are compared using three matching modes — exact strings are compared literally, entries ending with `/` are treated as directory prefixes that match any path underneath them (e.g. `pkg/client/` conflicts with `pkg/client/http.go`), and entries containing glob metacharacters (`*`, `?`, `[`, `{`) are matched as glob patterns using `doublestar` syntax (e.g. `internal/runner/*.go` conflicts with `internal/runner/task.go`). Conflict-deferred tasks remain in `backlog/` (they are not moved to `waiting/`). Invalid glob syntax (e.g. combining metacharacters with a trailing `/`) is a fatal task error: the queue quarantines the task into `failed/`, and `mato doctor` reports it at error severity.

## Queue Layout

```text
<repo>/.mato/
├── .paused          # optional durable operator pause sentinel
├── waiting/         # dependency-blocked tasks
├── backlog/         # runnable tasks and affects-deferred tasks
├── in-progress/     # claimed by an active agent
├── ready-for-review/# completed by agent, waiting for AI review
├── ready-to-merge/  # approved by review agent, waiting for host merge
├── completed/       # merged successfully
├── failed/          # exceeded retry limit or cancelled by operator
├── messages/
│   ├── events/      # coordination events and status updates
│   ├── completions/ # host-written completion details for merged tasks
│   └── presence/    # host-managed agent presence tracking
└── .locks/          # PID locks for agents and merge queue
```

Tasks that accumulate `max_retries` failure records (default 3) are moved to `failed/`.
Operators can also move queued tasks to `failed/` deliberately with `mato cancel`.

## How It Works

1. Add tasks to `waiting/` or `backlog/`.
2. Mato promotes ready tasks into `backlog/`, moves misplaced dependency-blocked backlog tasks back to `waiting/`, orders runnable backlog tasks by priority, and defers overlapping `affects` conflicts (exact paths, directory prefixes, and glob patterns).
3. An agent claims a backlog task, works in an isolated clone on a host-created `task/<name>` branch, and commits. The host pushes the branch after the agent exits.
4. Agents communicate through `.mato/messages/` so concurrent runs can share intent and completion events.
5. A review agent automatically evaluates each completed task branch. Approved tasks advance to `ready-to-merge/`; rejected tasks return to `backlog/` with feedback for the next attempt.
6. The host merge queue processes `ready-to-merge/` and squash-merges finished task branches into the target branch.
7. Tasks move to `completed/` on success. Missing branches move to `failed/`, merge conflicts requeue to `backlog/` for a fresh attempt, and push failures are retried in `ready-to-merge/`.

If the queue is empty, mato keeps polling until new work appears. The loop exits cleanly on `Ctrl+C`.
If `.mato/.paused` exists, the loop skips new claims and review launches, keeps
refreshing `.queue`, and continues draining `ready-to-merge/` until the repo is
resumed.

## Running Multiple Agents

Start multiple `mato` processes in separate terminals to process tasks in parallel. Task claiming is atomic (`mv`), active agents are tracked in `.locks/`, and orphaned `in-progress/` tasks are recovered automatically on the next loop.

## Status Command

`mato status` prints a terminal-friendly snapshot of the queue:

- counts for `backlog/`, dependency-blocked work, `in-progress/`, `ready-for-review/`, `ready-to-merge/`, `completed/`, and `failed/`
- runnable backlog in execution order (priority-sorted, dependency-blocked and conflict-deferred tasks excluded)
- active agents from `.locks/`
- waiting tasks with dependency progress
- conflict-deferred tasks with blocking details
- the last 5 coordination messages from `.mato/messages/events/`

The runnable backlog shows what the host will claim next, in the same priority
order used by `.mato/.queue`. Use `--format json` to get the same ordered list
as `runnable_backlog` in the JSON output.

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

## Inspect Command

`mato inspect` explains the current state of one task using the same queue
snapshot and scheduling logic as the host:

```bash
# Human-readable explanation
mato inspect add-retry-logic

# Machine-readable JSON
mato inspect add-retry-logic --format json
```

It resolves a task by filename, filename stem, or explicit frontmatter `id`,
then reports the current queue state, the actionable status (`blocked`,
`deferred`, `runnable`, `running`, `ready_for_review`, `ready_to_merge`,
`completed`, `failed`, or `invalid`), and the most relevant next step. The
command is read-only: it never moves tasks or writes markers.

## Doctor Command

`mato doctor` runs a structured health check across the repository and task queue:

- git repository detection and configuration
- host tool availability (git, docker, gh, copilot)
- Docker daemon connectivity
- queue directory layout and read errors
- task file parsing (frontmatter errors, invalid globs)
- lock and orphan detection (stale PID locks, orphaned in-progress tasks)
- dependency integrity (cycles, unknown IDs, ambiguous prefixes, duplicates)

Use `--fix` to auto-repair safe, idempotent issues such as missing directories,
stale locks, and orphaned tasks. Use `--format json` for machine-readable output.
The `--only` flag accepts a comma-separated list of check categories to run
(`git`, `tools`, `docker`, `queue`, `tasks`, `locks`, `hygiene`, `deps`);
non-selected checks appear as skipped. Exit code 0 means healthy, 1 means
warnings only, and 2 means errors were found.

For queue-focused preflight validation, prefer:

```bash
mato doctor --only queue,tasks,deps
```

That mode stays read-only and skips unrelated Docker checks and Docker-image
config loading.

## Retry Command

`mato retry` requeues failed tasks back to backlog for another attempt:

```bash
# Retry a single task
mato retry fix-login-bug

# Retry multiple tasks
mato retry fix-login-bug add-dark-mode
```

The command strips task-failure markers (`<!-- failure: -->`,
`<!-- review-failure: -->`, `<!-- cancelled: -->`, `<!-- cycle-failure: -->`, `<!-- terminal-failure: -->`)
from the task file and writes the cleaned content to `backlog/`. Review
feedback markers (`<!-- review-rejection: -->`) are preserved so the next
attempt still receives prior reviewer guidance. If the task already exists in
`backlog/`, the command prints an error and leaves the `failed/` copy unchanged
(no data loss).

## Pause and Resume Commands

`mato pause` writes a durable `.mato/.paused` sentinel so the host stops
claiming new tasks and stops launching review agents:

```bash
mato pause
```

`mato resume` removes the sentinel and restores normal polling behavior:

```bash
mato resume
```

While paused, already-running task agents are allowed to finish and the merge
queue continues draining `ready-to-merge/`.

## Cancel Command

`mato cancel` withdraws queued tasks by moving them to `failed/` and appending a
`<!-- cancelled: operator at ... -->` marker:

```bash
# Cancel a single task
mato cancel fix-login-bug

# Cancel multiple tasks
mato cancel fix-login-bug add-dark-mode
```

If cancelled tasks are later retried with `mato retry`, the cancelled markers are
stripped and the tasks return to `backlog/` like any other failed task.

## Version Command

`mato version` prints the build version in a script-friendly format:

- release builds prefer the nearest reachable tag matching `v*`
- non-release tags such as `before-refactor` are ignored for version stamping
- if no matching release tag is reachable, the build falls back to the commit hash

```bash
mato version
```

You can also use the root-level convenience flag:

```bash
mato --version
```

## Docker

`mato` launches an `ubuntu:24.04` container by default. Override it with `MATO_DOCKER_IMAGE` or set `docker_image` in `.mato.yaml`. The container mounts a temporary clone at `/workspace` plus the original repo path for local `git fetch`/`git push`, mounts host `copilot`, `git`, `gh`, and credentials/config, runs as your UID/GID, and passes explicit model settings such as:

```bash
mato run --task-model claude-opus-4.6 --review-model gpt-5.4
```

## Shell Completion

`mato` exposes Cobra's built-in shell completion command:

```bash
# Bash
mato completion bash > ~/.local/share/bash-completion/completions/mato

# Zsh
mato completion zsh > ~/.zfunc/_mato
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
