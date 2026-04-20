# Multi Agent Task Orchestrator (mato)

> _Run a swarm of autonomous Copilot agents against one repository — each working in its own Docker sandbox, every change reviewed by AI before it merges._

[![CI](https://github.com/ryansimmen/mato/actions/workflows/ci.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/ci.yml)
[![CodeQL](https://github.com/ryansimmen/mato/actions/workflows/codeql.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://github.com/ryansimmen/mato/actions/workflows/scorecard.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/scorecard.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/license-MIT-green)
![Status](https://img.shields.io/badge/status-alpha-orange)

`mato` orchestrates autonomous coding agents against a filesystem-backed task queue in Docker. Agents claim work, push task branches, and completed work is reviewed before it is merged back into the target branch.

See [Architecture](docs/architecture.md) for the detailed runtime design.

> **Status:** alpha. APIs, task-file format, and CLI flags may change between commits. Pin to a commit SHA if you depend on it today.
>
> **Run only on machines and repositories you trust** — `mato` is an operator tool, not a sandbox. See [Security](#security) for details.

## Install

### CLI Only

Install the CLI from source:

```bash
go install github.com/ryansimmen/mato/cmd/mato@latest
```

### Bundled `mato` Skill

The `mato` task-planner skill is published from this repo and installed via the [GitHub CLI](https://cli.github.com/) (`gh` v2.90.0 or later):

```bash
gh skill install ryansimmen/mato mato --scope user
```

`gh skill` writes to the appropriate per-host directory (e.g. `~/.copilot/skills/mato/` for GitHub Copilot, `~/.claude/skills/mato/` for Claude Code). Use `--agent claude-code|cursor|codex|gemini|antigravity` to target a non-Copilot host. Run `gh skill update mato` to pick up changes after a new release.

OpenCode is not yet a `gh skill`-supported host; install there with `gh skill install ryansimmen/mato mato --dir ~/.config/opencode/skills` as a workaround.

## Requirements

Runtime requirements for operators:

- Linux
- Docker
- Go 1.26+
- [GitHub CLI (`gh`)](https://github.com/cli/cli#installation)
- [GitHub Copilot CLI (`copilot`)](https://docs.github.com/en/copilot/how-tos/set-up/installing-github-copilot-in-the-cli)

Additional contributor tools:

- [`golangci-lint`](https://golangci-lint.run/welcome/install/) v2.11.4+
- optional `gopls`

`staticcheck` and `deadcode` are managed via `go tool` and do not need to be installed separately.

## Quick Start

```bash
# Install the CLI
go install github.com/ryansimmen/mato/cmd/mato@latest

# cd into the target repository
cd /path/to/repo

# Bootstrap the repository for mato
mato init
```

If you also installed the bundled `mato` skill with `gh skill install`, you can use Copilot to generate task files for the queue. For example:

```bash
copilot --interactive "Review this codebase for logical errors and create mato tasks of your findings"
```

The `mato` skill researches the codebase and writes task files into `.mato/backlog` or `.mato/waiting`.

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

## Security

`mato` is an operator tool, not a sandbox. It launches autonomous agents in Docker, bind-mounts host tooling and configuration into containers, and may forward GitHub authentication into those containers so the agent runtime can function. Only run it on repositories, branches, and machines you trust.

Report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## Documentation

- [Architecture](docs/architecture.md) - host loop, task lifecycle, review flow, merge queue
- [Configuration](docs/configuration.md) - CLI flags, environment variables, `.mato.yaml`, Docker setup
- [Task Format](docs/task-format.md) - frontmatter fields, runtime markers, placement rules, examples
- [Messaging](docs/messaging.md) - inter-agent coordination protocol
- [Contributing](CONTRIBUTING.md) - development setup, expectations, and PR guidance
- [Changelog](CHANGELOG.md) - notable changes per release
- [Code Of Conduct](CODE_OF_CONDUCT.md) - community participation guidelines
- [Support](SUPPORT.md) - where to ask questions and file issues
