# Multi Agent Task Orchestrator (mato)

> _Run a swarm of autonomous Copilot agents against one repository — each working in its own Docker sandbox, every change reviewed by AI before it merges._

[![CI](https://github.com/ryansimmen/mato/actions/workflows/ci.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/ci.yml)
[![CodeQL](https://github.com/ryansimmen/mato/actions/workflows/codeql.yml/badge.svg)](https://github.com/ryansimmen/mato/actions/workflows/codeql.yml)
[![OpenSSF Scorecard](https://img.shields.io/ossf-scorecard/github.com/ryansimmen/mato?label=OpenSSF%20Scorecard)](https://securityscorecards.dev/viewer/?uri=github.com/ryansimmen/mato)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
[![Go Reference](https://pkg.go.dev/badge/github.com/ryansimmen/mato.svg)](https://pkg.go.dev/github.com/ryansimmen/mato)
![License](https://img.shields.io/badge/License-MIT-green)
![Status](https://img.shields.io/badge/Status-alpha-orange)

`mato` orchestrates autonomous coding agents against a filesystem-backed task queue in Docker. Agents claim work, push task branches, and completed work is reviewed before it is merged back into the target branch.

See [Architecture](docs/architecture.md) for the detailed runtime design.

> **Status:** alpha. APIs, task-file format, and CLI flags may change between commits. Pin to a commit SHA if you depend on it today.
>
> **Run only on machines and repositories you trust** — `mato` is an operator tool, not a sandbox. See [Security](#security) for details.

## Install

### Linux binary (recommended)

`mato` ships signed `linux/amd64` and `linux/arm64` binaries with each release. The install script downloads the archive, verifies its `sha256` checksum, and (when [`cosign`](https://docs.sigstore.dev/cosign/installation/) is on `PATH`) verifies the cosign signature before installing the binary.

Inspect-then-run (recommended):

```bash
curl -fsSLO https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh
less install.sh   # review the script
bash install.sh
```

One-liner:

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | bash
```

System-wide install (`/usr/local/bin`):

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | sudo bash
```

The script honors two environment variables:

- `VERSION` — release tag (e.g. `v0.1.4`). Defaults to the latest release.
- `PREFIX` — install prefix; the binary is placed in `$PREFIX/bin/mato`. Defaults to `/usr/local` for root, `$HOME/.local` for non-root.

```bash
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh \
  | VERSION=v0.1.4 PREFIX=$HOME/custom bash
```

macOS and Windows are not currently published as binaries — see [Build from source](#build-from-source).

### Verify the download

Each release publishes a `*.intoto.jsonl` SLSA build provenance bundle, per-archive cosign `.sigstore.json` bundles, a signed `checksums.txt`, and per-archive [SPDX 2.3](https://spdx.dev/) SBOMs. The install script performs verification automatically when run, but you can also verify a manually-downloaded archive.

**With `gh` (recommended):**

```bash
gh release download v0.1.4 -R ryansimmen/mato -p 'mato_0.1.4_linux_amd64.tar.gz'
gh attestation verify -R ryansimmen/mato mato_0.1.4_linux_amd64.tar.gz
```

A successful verification exits 0; in non-interactive shells the command is silent on success. Use `--format json` for full attestation details.

**Without `gh`** (using `sha256sum` and [`cosign`](https://docs.sigstore.dev/cosign/installation/)):

```bash
VERSION=v0.1.4
ASSETS="mato_${VERSION#v}_linux_amd64.tar.gz checksums.txt checksums.txt.sigstore.json mato_${VERSION#v}_linux_amd64.tar.gz.sigstore.json"
for f in $ASSETS; do
  curl -fsSLO "https://github.com/ryansimmen/mato/releases/download/${VERSION}/${f}"
done

sha256sum --ignore-missing -c checksums.txt

CERT_ID="https://github.com/ryansimmen/mato/.github/workflows/release.yml@refs/tags/${VERSION}"
ISSUER="https://token.actions.githubusercontent.com"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "$CERT_ID" \
  --certificate-oidc-issuer "$ISSUER" \
  checksums.txt

cosign verify-blob \
  --bundle "mato_${VERSION#v}_linux_amd64.tar.gz.sigstore.json" \
  --certificate-identity "$CERT_ID" \
  --certificate-oidc-issuer "$ISSUER" \
  "mato_${VERSION#v}_linux_amd64.tar.gz"
```

SBOM (`*.sbom.json`) and SLSA provenance (`*.intoto.jsonl`) bundles are also attached to each release.

### Build from source

If you have [Go](https://go.dev/doc/install) 1.26+:

```bash
go install github.com/ryansimmen/mato/cmd/mato@latest
```

Note: binaries built via `go install` do not embed a version string (`mato --version` reports `dev`).

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
- [GitHub CLI (`gh`)](https://github.com/cli/cli#installation)
- [GitHub Copilot CLI (`copilot`)](https://docs.github.com/en/copilot/how-tos/set-up/installing-github-copilot-in-the-cli)

Additional contributor tools:

- [Go](https://go.dev/doc/install) 1.26+ (only required when building from source)
- [`golangci-lint`](https://golangci-lint.run/welcome/install/) v2.11.4+
- optional `gopls`

`staticcheck` and `deadcode` are managed via `go tool` and do not need to be installed separately.

## Quick Start

```bash
# Install the CLI
curl -fsSL https://raw.githubusercontent.com/ryansimmen/mato/main/scripts/install.sh | bash

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
