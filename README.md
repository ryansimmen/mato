# Multi Agent Task Orchestrator (mato)

Runs autonomous Copilot agents against a filesystem-backed task queue in Docker. Agents claim work, coordinate through `.tasks/`, push task branches, and the host merge queue squash-merges completed work into the target branch.

## Requirements

- Go
- Docker
- GitHub CLI
- Copilot CLI

## Quick Start

```bash
# Install mato binary and the mato-skill Copilot skill (~/.copilot/skills/)
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
```

Useful flags:

- `--repo <path>`: target repository (defaults to the current directory)
- `--branch <name>`: merge target branch (defaults to `mato`)
- `--tasks-dir <path>`: custom task queue location (defaults to `<repo>/.tasks`)

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
- `affects` is used for simple conflict prevention: if two backlog tasks list the exact same path, the lower-priority task is excluded from the `.queue` manifest until the conflict clears.

## Queue Layout

```text
<repo>/.tasks/
├── waiting/         # blocked tasks waiting on dependencies or conflicts
├── backlog/         # ready to run
├── in-progress/     # claimed by an active agent
├── ready-to-merge/  # completed by an agent, waiting for host merge
├── completed/       # merged successfully
├── failed/          # exceeded retry limit
├── messages/
│   ├── events/      # coordination events and status updates
│   └── presence/    # agent presence tracking (reserved for future use)
└── .locks/          # PID locks for agents and merge queue
```

Failed tasks are retried up to 3 times before moving to `failed/`.

## How It Works

1. Add tasks to `waiting/` or `backlog/`.
2. Mato promotes ready tasks into `backlog/`, orders them by priority, and defers exact `affects` conflicts.
3. An agent claims a backlog task, works in an isolated clone, and pushes a `task/<name>` branch.
4. Agents communicate through `.tasks/messages/` so concurrent runs can share intent and completion events.
5. The host merge queue processes `ready-to-merge/` and squash-merges finished task branches into the target branch.
6. Tasks move to `completed/` on success. Missing branches move to `failed/`, merge conflicts requeue to `backlog/` for a fresh attempt, and push failures are retried in `ready-to-merge/`.

If the queue is empty, mato keeps polling until new work appears. The loop exits cleanly on `Ctrl+C`.

## Running Multiple Agents

Start multiple `mato` processes in separate terminals to process tasks in parallel. Task claiming is atomic (`mv`), active agents are tracked in `.locks/`, and orphaned `in-progress/` tasks are recovered automatically on the next loop.

## Status Command

`mato status` prints a terminal-friendly snapshot of the queue:

- counts for `waiting/`, `backlog/`, `in-progress/`, `ready-to-merge/`, `completed/`, and `failed/`
- active agents from `.locks/`
- waiting tasks with dependency progress
- the last 5 coordination messages from `.tasks/messages/events/`

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
