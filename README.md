# Multi Agent Task Orchestrator (mato)

> _Run multiple autonomous Copilot agents in parallel, each isolated in Docker and AI-reviewed before merge._

[![CI](https://github.com/ryansimmen/mato/actions/workflows/ci.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/ci.yml)
[![CodeQL](https://github.com/ryansimmen/mato/actions/workflows/codeql.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://img.shields.io/ossf-scorecard/github.com/ryansimmen/mato?label=OpenSSF%20Scorecard)](https://securityscorecards.dev/viewer/?uri=github.com/ryansimmen/mato)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
[![Go Reference](https://pkg.go.dev/badge/github.com/ryansimmen/mato.svg)](https://pkg.go.dev/github.com/ryansimmen/mato)
![License](https://img.shields.io/badge/License-MIT-green)
![Status](https://img.shields.io/badge/Status-alpha-orange)

`mato` turns markdown task files into a filesystem-backed work queue for autonomous coding agents. Workers claim tasks, push branches, and serialize reviewed changes back into your target branch.

## How It Works

1. The bundled `mato` skill turns a request into markdown task files under `.mato/backlog` or `.mato/waiting`.
2. `mato run` claims runnable tasks and launches isolated Copilot agents in Docker.
3. Each agent works on its own task branch and commits locally.
4. The host pushes completed task branches, runs an AI review pass, and requeues rejected work with feedback.
5. Approved tasks are squash-merged serially into the target branch and recorded in the completion log.

See [Architecture](docs/architecture.md) for more details.

## When To Use This

`mato` is useful when work can be split into multiple clear tasks and run through the same review-and-merge gate:

- large cleanup or refactor efforts with independent files or packages
- bug sweeps where each finding can become a focused task
- dependency-ordered implementation plans that should advance one task at a time
- parallel documentation, test, and maintenance work across one repository

## Requirements

Runtime requirements for operators:

- Linux
- Docker
- [GitHub CLI](https://github.com/cli/cli#installation) (`gh` v2.90.0 or later)
- [GitHub Copilot CLI](https://docs.github.com/en/copilot/how-tos/set-up/installing-github-copilot-in-the-cli)

Tooling for building from source or contributing is documented in [CONTRIBUTING.md](CONTRIBUTING.md#development-setup).

## Install

`mato` ships signed `linux/amd64` and `linux/arm64` binaries with each release:

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | bash
```

Install the bundled task-planning skill with the [GitHub CLI](https://cli.github.com/) (`gh` v2.90.0 or later):

```bash
gh skill install ryansimmen/mato mato --scope user
```

The CLI runs the queue; the skill creates the task files that populate it.

See [Install](docs/install.md) for alternative CLI installation and verification methods.

## Quick Start

```bash
# cd into the target repository
cd /path/to/repo

# Bootstrap the repository for mato
mato init

# Generate task files for the queue
copilot --interactive "Review this codebase for logical errors and create mato tasks of your findings"

# Start one worker
mato run
```

The `mato` skill writes task files into `.mato/backlog` or `.mato/waiting`. These task files live under `.mato`; the scheduler reads their frontmatter for dependency, priority, and conflict metadata, then passes the markdown body to the agent as instructions.

A minimal task file looks like:

```md
---
id: add-http-timeout
priority: 20
affects:
  - src/client/http.go
  - src/client/http_test.go
---
# Add a timeout to outbound HTTP requests

Ensure outbound HTTP requests use a reasonable timeout so callers do not hang indefinitely when a service is slow or unavailable.

Add or update tests that cover the timeout behavior.
```

For the full task-file specification, see [Task Format](docs/task-format.md). If setup or queue validation fails, run `mato doctor` for the full health check.

Run more workers or inspect the queue from other terminals:

```bash
# Start another worker
mato run

# Inspect the queue health
mato status

# List active queue tasks in a flat view
mato list

# Visualize the dependency graph
mato graph

# View completions
mato log
```

The `mato status` view looks like:

```text
Queue: 5 backlog | 3 runnable | 1 running | 1 review | 0 merge | 0 failed
Pause: not paused   Merge queue: idle

Agents (2)
  agent-abc12345  validate-config.md  task/validate-config  WORK  2 min
  agent-def67890  add-http-timeout.md  task/add-http-timeout  VERIFY_REVIEW  45 sec

Attention
  1 blocked by dependencies

Next Up
  1. log-failed-jobs.md — Log failed background jobs with context
  2. add-retry-backoff.md — Retry transient API failures with backoff
  3. improve-error-messages.md — Include actionable hints in error messages
```

## Skill Installation Notes

`gh skill` writes to the appropriate per-host directory (e.g. `~/.copilot/skills/mato/` for GitHub Copilot, `~/.claude/skills/mato/` for Claude Code). Use `--agent claude-code|cursor|codex|gemini|antigravity` to target a non-Copilot host. Run `gh skill update mato` to pick up changes after a new release.

OpenCode is not yet a `gh skill`-supported host; install there with `gh skill install ryansimmen/mato mato --dir ~/.config/opencode/skills` as a workaround.

See [Configuration](docs/configuration.md) for all flags, environment variables, and `.mato.yaml` options.

## Safety Model

- Agents run in short-lived Docker containers against isolated temporary clones.
- Work lands on task branches first; agents do not merge directly into the target branch.
- Every completed task branch passes through an AI review before merge.
- The host serializes squash merges through a merge lock to avoid concurrent target-branch writes.
- Queue state is ordinary filesystem data under `.mato/`, so operators can inspect, pause, cancel, retry, and diagnose work with CLI commands.
- Task `affects:` metadata lets the scheduler defer overlapping work while other agents, reviews, or merges are active.

## What Mato Does Not Do

- `mato` does not replace human review or repository policy; it adds an AI review gate before merging back into the target branch on your machine.
- `mato` does not run on macOS or Windows hosts.
- `mato` does not require a service, daemon, or database; queue state stays in the repository-local `.mato/` directory.
- `mato` does not keep agents in a shared planning session after task creation; coordination happens through task files, Git branches, and queue metadata.

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
| `mato pause` | Pause new claims and review launches. |
| `mato resume` | Resume normal polling after a pause. |

## Documentation

- [Architecture](docs/architecture.md) - host loop, task lifecycle, review flow, merge queue
- [Install](docs/install.md) - binary install, bundled skill install, manual download verification, build from source
- [Configuration](docs/configuration.md) - CLI flags, environment variables, `.mato.yaml`, Docker setup
- [Task Format](docs/task-format.md) - frontmatter fields, runtime markers, placement rules, examples
- [Messaging](docs/messaging.md) - inter-agent coordination protocol
- [Contributing](CONTRIBUTING.md) - development setup, expectations, and PR guidance
- [Changelog](CHANGELOG.md) - notable changes per release
- [Code Of Conduct](CODE_OF_CONDUCT.md) - community participation guidelines
- [Support](SUPPORT.md) - where to ask questions and file issues
