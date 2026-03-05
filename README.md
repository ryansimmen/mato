# mato

*Multi Agent Task Orchestrator*

Runs autonomous Copilot agents against a task queue in Docker. Each agent picks a task, works on it, commits to main, and exits.

## Requirements

- Go
- Docker
- GitHub CLI
- Copilot CLI

## Quick Start

```bash
# Build
make build

# Authenticate with Copilot
copilot login

# Configure your target repo
echo "REPO=/path/to/repo" > .env

# Add a task
cat > /path/to/repo/.tasks/backlog/add-retry-logic.md << 'EOF'
# Add retry logic to HTTP client

The fetchData function in pkg/client/http.go does not retry on transient
failures. Wrap calls in a retry loop with exponential backoff (3 attempts,
starting at 500ms). Add tests covering retry on 503 and success on second
attempt.
EOF

# Run the agent
make run
```

Mato will pick up the task, work on it in a Docker container, commit to main, and then poll for the next task.

## How It Works

1. You add task files (markdown) to `<repo>/.tasks/backlog/`
2. Mato loops continuously, polling the backlog every 10 seconds
3. When a task is found, it starts a Docker container with the `copilot` CLI
4. Copilot picks a task, claims it, creates a branch, does the work, merges to main
5. The task file moves to `.tasks/completed/` (or `.tasks/failed/` after too many retries) and the container exits
6. Mato waits 10 seconds then checks for the next task

If the backlog is empty, mato keeps polling until new tasks appear. The loop exits cleanly on `Ctrl+C`.

## Task File Format

```markdown
# Task title

Detailed instructions for the agent.
```

## Task Queue Structure

```
<repo>/.tasks/
├── backlog/       # pending tasks
├── in-progress/   # tasks being worked on
├── completed/     # finished tasks
├── failed/        # tasks that exceeded retry limit
└── .locks/        # PID locks for concurrent agents
```

Failed tasks are retried up to 3 times before being moved to `failed/`.

## Running Multiple Agents

To process tasks in parallel, start multiple mato instances in separate terminals:

```bash
# Terminal 1
make run

# Terminal 2
make run

# Terminal 3
make run
```

Each instance operates independently — it claims a task from the backlog, works on it in its own temporary clone, and merges to main when done. Task claiming is atomic (filesystem `mv`), so no two agents will work on the same task. If an agent crashes, the next instance to start will recover its orphaned task back to the backlog.

## Docker

`make run` is a convenience wrapper around `go run . --repo`. The container:

- Mounts the repo at `/workspace` in an `ubuntu:24.04` container (override with `MATO_DOCKER_IMAGE`).
- Mounts host `copilot` and `gh` CLIs, `~/.copilot` auth, `~/.config/gh`, and SSL certs.
- Runs as your host UID/GID so files are owned by you.
- Passes your git `user.name` and `user.email` for commits.
- Runs `copilot -p <instructions> --autopilot --allow-all --model claude-opus-4.6` inside the container by default.

Pass extra copilot args via `COPILOT_ARGS`, e.g. to change the model:

```bash
make run COPILOT_ARGS="--model gpt-5.3-codex"
```

## Notes

- Task instructions are embedded in the binary (`task-instructions.md`).
- The agent creates a `task/<name>` branch, merges to main, and resolves conflicts if needed.
- Orphaned tasks (from crashed agents) are automatically recovered on the next run.
- Run `make test` to run the test suite.
