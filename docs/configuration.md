# Mato Configuration Reference
This document covers every supported configuration surface in `mato`: CLI flags,
subcommands, environment variables, Docker runtime behavior, prerequisites, and the
Makefile targets used to build and run it.

## Prerequisites
Install these tools on the host that runs `mato`:
- Go 1.26+ (the module currently declares `go 1.26.0`)
- Docker
- Git
- GitHub CLI (`gh`)
- GitHub Copilot CLI (`copilot`)
`mato` locates `copilot`, `git`, `git-upload-pack`, `git-receive-pack`, and `gh` on
the host and bind-mounts those executables into agent containers.

## CLI Usage
```text
mato [--repo <path>] [--branch <name>] [--tasks-dir <path>] [copilot-args...]
mato status [--repo <path>] [--tasks-dir <path>]
```
Run mode creates the queue structure if needed, starts the Docker-based Copilot loop,
and merges completed work into the target branch. If the target branch does not exist
yet, `mato` creates it.
Status mode prints queue counts, active agents, waiting-task dependency summaries, and
recent messages. `mato status` rejects extra positional arguments, but it does
silently accept `--branch` even though status ignores that value.
Use `--` to stop `mato` flag parsing and forward the remaining arguments verbatim to
Copilot CLI. In run mode, unrecognized arguments are also passed through to Copilot.

## CLI Flags
Long flags support both `--flag value` and `--flag=value` forms.
| Flag | Applies to | Default | Description |
| --- | --- | --- | --- |
| `--repo <path>` | run, status | current directory | Target Git repository. `mato` resolves it to the repository top level with `git rev-parse --show-toplevel`. |
| `--branch <name>` | run; accepted-but-ignored by status | `mato` | Target branch used for merge processing. `mato status` parses this flag via shared argument handling but does not use it. |
| `--tasks-dir <path>` | run, status | `<repo>/.tasks` | Task queue directory. If omitted, `mato` uses `.tasks` under the resolved repository root. |
| `--help`, `-h` | run, status | none | Show help and exit. |
| `--` | run | none | Forward all following arguments directly to Copilot CLI without further `mato` parsing. |

## Subcommands
### `mato status`
`mato status` reads the queue directory and reports:
- counts for `waiting`, `backlog`, `in-progress`, `ready-to-merge`, `completed`, and `failed`
- active agents discovered from `.tasks/.locks/*.pid`
- waiting tasks plus dependency-status summaries
- the five most recent messages from `.tasks/messages`
Supported flags: `--repo`, `--tasks-dir`, and `--help`/`-h`. `--branch` is also accepted by the shared parser, but `mato status` ignores it.

## Environment Variables
| Variable | Scope | Default | Description |
| --- | --- | --- | --- |
| `MATO_DOCKER_IMAGE` | host | `ubuntu:24.04` | Docker image used for agent containers. Set this before starting `mato` to use a custom image. |
| `MATO_AGENT_ID` | container | generated per run | Agent identity injected by `mato` so the running agent can identify itself. |
| `MATO_MAX_RETRIES` | container | `3` | Passed to container for reference; the host enforces the retry budget in `queue.SelectAndClaimTask(...)` and `merge.shouldFailTask(...)`. Per-task overrides via `max_retries` frontmatter take precedence. |
| `MATO_MESSAGING_ENABLED` | container | `1` | Injected by `mato` for agent-side tooling. The embedded prompt already uses hardcoded `.tasks` paths, so this is mainly useful to custom scripts or wrappers. |
| `MATO_MESSAGES_DIR` | container | `/workspace/.tasks/messages` | Injected path to the shared messages directory for custom tooling. The embedded prompt separately hardcodes the same `/workspace/.tasks/messages` path. |
| `MATO_TASK_FILE` | container | none | Claimed task filename (e.g. `my-task.md`). Set per-run by the host after claiming a task. |
| `MATO_TASK_BRANCH` | container | none | Derived task branch name (e.g. `task/my-task`). Set per-run by the host after claiming a task. |
| `MATO_TASK_TITLE` | container | none | Extracted task title from the `# ` heading in the task file. Set per-run by the host after claiming a task. |
| `MATO_TASK_PATH` | container | none | Absolute path to the task file in `in-progress/` (e.g. `/workspace/.tasks/in-progress/my-task.md`). Set per-run by the host after claiming a task. |
Only `MATO_DOCKER_IMAGE` is intended as a host-side configuration input. The other
variables are injected by `mato` inside each container and are normally not set manually.

## Docker Configuration
Each agent run uses `docker run --rm -it` with working directory `/workspace` and user
mapping `--user <host-uid>:<host-gid>`, preserving host file ownership.

### Bind mounts
| Host path | Container path | Notes |
| --- | --- | --- |
| temporary clone of the repo | `/workspace` | The agent works in an isolated clone so multiple agents can run concurrently. |
| configured tasks dir | `/workspace/.tasks` | Shares the task queue and messaging state with the host. |
| resolved repo root | same absolute host path | Keeps the clone's `origin` local-path remote reachable for fetch/push. |
| host `copilot` binary | `/usr/local/bin/copilot` (ro) | Runs Copilot CLI inside the container. |
| host `git` binary | `/usr/local/bin/git` (ro) | Provides Git inside the container. |
| host `git-upload-pack` | `/usr/local/bin/git-upload-pack` (ro) | Needed when Git fetches from the local-path remote. |
| host `git-receive-pack` | `/usr/local/bin/git-receive-pack` (ro) | Needed when Git pushes to the local-path remote. |
| host `gh` binary | `/usr/local/bin/gh` (ro) | Makes GitHub CLI available in the container. |
| host `GOROOT` | `/usr/local/go` (ro) | Provides the Go toolchain in the container. |
| host `~/.copilot` | `$HOME/.copilot` | For Copilot authentication and package data. |
| host `~/go/pkg/mod` | `$HOME/go/pkg/mod` | Shares Go module cache. |
| host `~/.cache/go-build` | `$HOME/.cache/go-build` | Shares Go build cache. |
| host `~/.config/gh` | `$HOME/.config/gh` (ro) | Mounted only if it exists, to forward `gh` authentication/config. |
| host `/etc/ssl/certs` | `/etc/ssl/certs` (ro) | Mounted only if present, to preserve CA trust. |

### Container environment and runtime behavior
- `HOME` inside the container is set to the host home directory path.
- `GOROOT=/usr/local/go` and `PATH` are set so mounted Go and CLI binaries are usable.
- `GOPATH`, `GOMODCACHE`, and `GOCACHE` point at the mounted host cache paths.
- `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=safe.directory`, and `GIT_CONFIG_VALUE_0=*` allow Git to trust mounted worktrees even if ownership looks unusual.
- If Git user name/email are configured on the host repository or globally, `mato` forwards them as `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, and `GIT_COMMITTER_EMAIL`.
- The container command is `copilot -p <embedded prompt> --autopilot --allow-all`.
- If no model is present in forwarded Copilot arguments, `mato` adds `--model claude-opus-4.6` automatically.
When choosing a custom `MATO_DOCKER_IMAGE`, use an image compatible with the mounted
host binaries and standard Linux filesystem layout expected above.

## Makefile Targets
The Makefile loads `.env` if present, exports its variables, and defaults to the
`help` target.
| Target | Description |
| --- | --- |
| `build` | Build `bin/mato` with `go build -o bin/mato ./cmd/mato`. |
| `install` | Install `mato` into `GOBIN`/`GOPATH/bin` with `go install ./cmd/mato`. |
| `clean` | Remove the `bin/` directory. |
| `fmt` | Run `go fmt ./...`. |
| `integration-test` | Run `go test -race -v ./internal/integration/...`. |
| `run` | Run `go run ./cmd/mato --repo "$(REPO)" $(COPILOT_ARGS)`. `REPO` is required; set it in `.env` or on the command line. |
| `test` | Run `go test ./...`. |
| `help` | Print the target list and descriptions. |
Additional behavior:
- `all` runs `fmt`, `build`, and `test`.
- `REPO` is required for `make run` and may be supplied from `.env`.
- `COPILOT_ARGS` is passed through to `mato`, which then forwards those arguments to Copilot CLI.
