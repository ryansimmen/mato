# mato

*Multi Agent Task Orchestrator*

Runs autonomous Copilot agents against a task queue in Docker. Each agent picks a task, works on it, commits to main, and exits.

## Requirements

- Go
- Docker
- GitHub CLI
- Copilot CLI


## How It Works

1. You add task files (markdown) to `<repo>/.tasks/backlog/`
2. Mato starts a Docker container with the `copilot` CLI
3. Copilot picks a task, claims it, creates a branch, does the work, merges to main
4. The task file moves to `.tasks/completed/` (or `.tasks/failed/` after too many retries) and the container exits

Multiple mato instances can run in parallel against the same repo. Each agent registers itself with a PID lock, claims tasks atomically, and merges independently.

## Quick Start

```bash
# Add a task
cat > /path/to/repo/.tasks/backlog/my-task.md << 'EOF'
# Add a health check endpoint

Add a /healthz endpoint that returns 200 OK.
EOF

# Run the agent
make run REPO=/path/to/repo
```

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

## Docker

`make run` is a convenience wrapper around `go run . --repo`. The container:

- Mounts the repo at `/workspace` in an `ubuntu:24.04` container (override with `MATO_DOCKER_IMAGE`).
- Mounts host `copilot` and `gh` CLIs, `~/.copilot` auth, `~/.config/gh`, and SSL certs.
- Runs as your host UID/GID so files are owned by you.
- Passes your git `user.name` and `user.email` for commits.
- Runs `copilot -p <instructions> --autopilot --allow-all --model claude-opus-4.6` inside the container by default.

Pass extra copilot args after `--`, e.g.:

```bash
bin/mato --repo /path/to/repo -- --model claude-opus-4.6
```

You can also create a `.env` file to avoid passing `REPO` every time:

```bash
echo "REPO=/path/to/repo" > .env
make run
```

## Build

```bash
make build    # builds bin/mato
make test     # runs tests
```

## Notes

- Task instructions are embedded in the binary (`task-instructions.md`).
- Authenticate first with `copilot login`.
- The agent creates a `task/<name>` branch, merges to main, and resolves conflicts if needed.
- Orphaned tasks (from crashed agents) are automatically recovered on the next run.
