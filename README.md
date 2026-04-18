# Multi Agent Task Orchestrator (mato)

[![CI](https://github.com/ryansimmen/mato/actions/workflows/ci.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-green)

Runs an autonomous Copilot agent swarm against a filesystem-backed task queue in Docker. Agents claim work and every completed branch is automatically reviewed by an AI review agent. See
[Architecture](docs/architecture.md) for details.

## Requirements

- Go
- Git
- Docker
- GitHub CLI
- Copilot CLI

## Quick Start

```bash
# Install the mato binary and skill
make install

# cd into the target repository
cd /path/to/repo

# Bootstrap the repository for mato
mato init
```

After initializing the repo, use Copilot with the `mato` skill to generate task files for the queue. For example:

```bash
copilot --interactive "Review this codebase for logical errors and create mato tasks of your findings"
```

The `mato` skill will research the codebase and write task files into `.mato/backlog` or `.mato/waiting`.

These task files live under `.mato`. The scheduler reads their frontmatter for dependency, priority, and conflict metadata, then passes the markdown body to the agent as instructions.

For the full task-file specification, see [Task Format](docs/task-format.md).

```bash
# Start one worker
mato run

# In another terminal, start another worker
mato run

# In a third terminal, inspect the queue health
mato status

# List active queue tasks in a flat view
mato list

# Visualize the dependency graph
mato graph

# View completions
mato log
```

See [Configuration](docs/configuration.md) for all flags, environment variables, and `.mato.yaml` options.

## Queue Layout

```text
<repo>/.mato/
├── waiting/           # dependency-blocked tasks
├── backlog/           # runnable tasks and affects-deferred tasks
├── in-progress/       # claimed by an active agent
├── ready-for-review/  # completed by agent, waiting for AI review
├── ready-to-merge/    # approved by review agent, waiting for host merge
├── completed/         # merged successfully
├── failed/            # exceeded retry limit or cancelled by operator
├── messages/
│   ├── events/        # coordination events and status updates
│   ├── completions/   # host-written completion details for merged tasks
│   └── presence/      # host-managed agent presence tracking
├── .locks/            # PID locks for agents and merge queue
└── .paused            # durable pause sentinel
```

## Commands

| Command | Description |
|---------|-------------|
| `mato` | Show help for the CLI. |
| `mato init` | Bootstrap `.mato`, messaging directories, and the target branch. |
| `mato run` | Start the host loop that claims, reviews, and merges tasks. |
| `mato status` | Show queue counts, active agents, and the next runnable tasks. |
| `mato list` | List queue tasks as a flat table or JSON array, with state filtering. |
| `mato graph` | Visualize task dependencies and blocked work. |
| `mato doctor` | Validate prerequisites, queue health, task parsing, and dependency integrity. |
| `mato inspect` | Explain why a task is blocked, deferred, runnable, or finished. |
| `mato log` | Show recent durable task outcomes. |
| `mato config` | Show effective repository defaults and where each value came from. |
| `mato cancel` | Move tasks to `failed` with a cancellation marker. |
| `mato retry` | Requeue one or more failed tasks. |
| `mato pause` | Pause new claims and review launches. Supports `--format text|json` for script-friendly output. |
| `mato resume` | Resume normal polling after a pause. Supports `--format text|json` for script-friendly output. |


## Documentation

- [Architecture](docs/architecture.md) - host loop, task lifecycle, review flow, merge queue
- [Configuration](docs/configuration.md) - CLI flags, environment variables, `.mato.yaml`, Docker setup
- [Task Format](docs/task-format.md) - frontmatter fields, runtime markers, placement rules, examples
- [Messaging](docs/messaging.md) - inter-agent coordination protocol
