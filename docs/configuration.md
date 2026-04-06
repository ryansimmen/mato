# Mato Configuration Reference
This document covers every supported configuration surface in `mato`: CLI flags,
subcommands, environment variables, Docker runtime behavior, prerequisites, and the
Makefile targets used to build and run it.

## Prerequisites
Install these tools on the host that runs `mato`:
- Go 1.26+ (the module currently declares `go 1.26.0`)
- Docker
- [Git](https://git-scm.com/downloads)
- [GitHub CLI (`gh`)](https://cli.github.com/)
- [GitHub Copilot CLI (`copilot`)](https://docs.github.com/en/copilot)
- [`gopls`](https://pkg.go.dev/golang.org/x/tools/gopls) on the host `PATH` if you
  want Go LSP features inside Docker agent containers
`mato` locates `copilot`, `git`, `git-upload-pack`, `git-receive-pack`, and `gh`
on the host and bind-mounts those executables into agent containers. When
`gopls` is available, `mato` bind-mounts that binary too; when it is missing,
task and review containers still launch but Go LSP features are unavailable and
`mato` prints a warning up front.

## CLI Usage
```text
mato [--version] [--repo <path>]
mato config [--repo <path>] [--format text|json]
mato run [--repo <path>] [--branch <name>] [--dry-run | --once | --until-idle] [--task-model <model>] [--review-model <model>] [--task-reasoning-effort <level>] [--review-reasoning-effort <level>]
mato init [--repo <path>] [--branch <name>] [--format text|json]
mato status [--repo <path>] [--watch] [--interval <duration>] [--format text|json] [--verbose]
mato log [--repo <path>] [--limit <n>] [--format text|json]
mato doctor [--repo <path>] [--fix] [--format text|json] [--only <check>]
mato graph [--repo <path>] [--format text|dot|json] [--all]
mato inspect [--repo <path>] [--format text|json] <task-ref>
mato retry [--repo <path>] <task-ref> [task-ref...]
mato cancel [--repo <path>] <task-ref> [task-ref...]
mato pause [--repo <path>]
mato resume [--repo <path>]
mato version
```
Valid `--only` check names: `git`, `tools`, `config`, `docker`, `queue`, `tasks`, `locks`, `hygiene`, `deps`.
The task queue location is fixed at `<repo>/.mato`.
`mato init` performs lightweight repository bootstrap without Docker. It resolves the repository root, checks out or creates the target branch, creates the `.mato/` queue, lock, and messaging directories, ensures git identity exists locally, updates `.gitignore` with `/.mato/`, and guarantees the target branch has at least one commit.
Run mode creates the queue structure if needed, starts the Docker-based Copilot loop,
and merges completed work into the target branch. If the target branch does not exist
yet, `mato` creates it.
Dry-run mode (`--dry-run`) validates the task queue setup without launching Docker
containers. It parses all task files, reports ready dependencies that would be promoted
from `waiting/` to `backlog/`, diagnoses misplaced dependency-blocked backlog tasks,
detects `affects` conflicts, and prints a summary of the effective runnable backlog and
queue state. Useful for verifying setup in CI or before a real run. No files are
modified.
Bounded run modes keep the normal startup and cleanup behavior but change the exit
contract. `--once` runs one full host poll iteration and exits even if more work remains.
`--until-idle` keeps polling until there is no immediately claimable backlog work, no
pending review work, and no tasks in `ready-to-merge/`. `--dry-run`, `--once`, and
`--until-idle` are mutually exclusive.
Status mode prints queue counts, active agents, waiting-task dependency summaries, and
recent messages. `mato status` rejects both extra positional arguments and
unrecognized flags such as `--branch`.
## Config File
`mato` optionally loads a config file from the repository root (next to `.git/`).
Both `.mato.yaml` and `.mato.yml` are accepted. If both files exist, `mato`
returns an error asking you to remove one to avoid ambiguity.
All fields are optional:

```yaml
branch: main
docker_image: ubuntu:24.04
task_model: claude-opus-4.6
review_model: gpt-5.4
review_session_resume_enabled: true
task_reasoning_effort: high
review_reasoning_effort: high
agent_timeout: 45m
retry_cooldown: 5m
```

- Config is repo-local only; there is no global config file.
- Unknown YAML keys cause a parse error. This catches typos such as `dockr_image`
  instead of silently ignoring them.
- Empty and whitespace-only string values are treated as unset.

## Which Surface To Use
Use the narrowest surface that matches the scope of the setting:

| Need | Use | Why |
| --- | --- | --- |
| Change behavior for one command invocation | CLI flags | Best for one-off overrides and scripts that should be explicit at the call site. |
| Set personal defaults on one machine without committing them | host configuration environment variables | Best for shell profiles, `direnv`, CI, and local wrappers. |
| Set shared defaults for everyone who works in the repo | `.mato.yaml` | Best for committed, repo-local defaults such as branch or Docker image. |
| Control scheduling behavior for one task | task frontmatter | Best for task-specific metadata such as priority, dependencies, touched files, or retry budget. |
| Inspect runtime state inside a container or custom script | injected container runtime variables | These are outputs of `mato` at runtime, not normal user configuration inputs. |

`mato` intentionally does not mirror every setting across every surface. For example,
`--repo` is CLI-only because repo selection happens before `.mato.yaml` can be
discovered, and task frontmatter stays separate from repo config because it is
task-specific scheduling metadata.

## Precedence
Settings resolve in this order: CLI flag > environment variable > `.mato.yaml` > hardcoded default.

`mato config` reports the standing repository defaults after env/config/default resolution and intentionally does not try to model one-shot flags from other subcommands such as `mato run --task-model`.

For user-facing CLI defaults, treat command help as canonical: `mato --help`,
`mato run --help`, and each subcommand's `--help` output reflect the built-in
fallbacks documented here.

Only settings that exist on more than one surface participate in this precedence
chain. Task frontmatter is separate from CLI/env/config resolution and controls
per-task scheduling behavior. It can still override runtime defaults that `mato`
passes into containers as reference values; for example, task `max_retries`
frontmatter is authoritative over the injected `MATO_MAX_RETRIES` default.

| Setting | CLI Flag | Env Var | Config File | Default |
| --- | --- | --- | --- | --- |
| repo | `--repo` | — | — | current directory |
| branch | `mato run --branch`, `mato init --branch` | `MATO_BRANCH` | `branch` | `mato` |
| docker image | — | `MATO_DOCKER_IMAGE` | `docker_image` | `ubuntu:24.04` |
| task model | `mato run --task-model` | `MATO_TASK_MODEL` | `task_model` | `claude-opus-4.6` |
| review model | `mato run --review-model` | `MATO_REVIEW_MODEL` | `review_model` | `gpt-5.4` |
| review session resume | — | `MATO_REVIEW_SESSION_RESUME_ENABLED` | `review_session_resume_enabled` | `true` |
| task reasoning effort | `mato run --task-reasoning-effort` | `MATO_TASK_REASONING_EFFORT` | `task_reasoning_effort` | `high` |
| review reasoning effort | `mato run --review-reasoning-effort` | `MATO_REVIEW_REASONING_EFFORT` | `review_reasoning_effort` | `high` |
| agent timeout | — | `MATO_AGENT_TIMEOUT` | `agent_timeout` | `30m` |
| retry cooldown | — | `MATO_RETRY_COOLDOWN` | `retry_cooldown` | `2m` |

### `mato config`
`mato config` shows the effective repository-level configuration and the source of each resolved setting.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |
| `--format` | `text` | Output format: `text` or `json`. |

The command is read-only and does not require an existing `.mato/` directory. It works in any git repository, reports which config file was loaded (`.mato.yaml`, `.mato.yml`, or none), and annotates env-sourced values with the exact variable name in text output, for example `env: MATO_DOCKER_IMAGE`.

## CLI Flags
Long flags support both `--flag value` and `--flag=value` forms.

| Flag | Applies to | Default | Description |
| --- | --- | --- | --- |
| `--repo <path>` | root persistent flag and all repo-aware subcommands | current directory | Target Git repository. `mato` resolves it to the repository top level with `git rev-parse --show-toplevel`. |
| `--branch <name>` | `mato run`, `mato init` | `mato` | Target branch used for merge processing and repository bootstrap. |
| `--dry-run` | `mato run` | `false` | Validate queue setup without launching Docker containers. Parses task files, reports ready dependency promotions, diagnoses dependency-blocked backlog tasks, detects `affects` conflicts, and prints a summary of the effective runnable backlog and queue state. Exits after one pass. |
| `--once` | `mato run` | `false` | Run exactly one host poll iteration, then exit. This can claim a task, process one existing review from the iteration snapshot, and merge ready tasks, but it does not keep polling to drain follow-on review or merge work. |
| `--until-idle` | `mato run` | `false` | Keep polling until no immediately claimable backlog tasks remain, no review candidates remain, and no tasks remain in `ready-to-merge/`, then exit. A paused but otherwise empty queue is considered idle. |
| `--task-model <model>` | `mato run` | `claude-opus-4.6` | Copilot model used for task agents. |
| `--review-model <model>` | `mato run` | `gpt-5.4` | Copilot model used for review agents. |
| `--task-reasoning-effort <level>` | `mato run` | `high` | Reasoning effort for task agents. Valid values: `low`, `medium`, `high`, `xhigh`. |
| `--review-reasoning-effort <level>` | `mato run` | `high` | Reasoning effort for review agents. Valid values: `low`, `medium`, `high`, `xhigh`. |
| `--help`, `-h` | all commands | none | Show help and exit. |

## Subcommands
### `mato status`
`mato status` reads the queue directory and reports:
- a compact default text dashboard with queue headline counts, active agents,
  attention summaries, and the next runnable tasks
- warnings in the compact `Attention` area when queue infrastructure reads fail
- a fuller diagnostic text view behind `--verbose`

Use `--format json` to get machine-readable output. The `runnable_backlog`
array in the JSON output lists tasks in the same priority order as the text
view. The JSON `counts` object includes both `waiting` (number of files in
`waiting/`) and `blocked` (semantic count of dependency-blocked tasks including
misplaced backlog tasks); the text output only shows `blocked`. The `waiting`
array in JSON lists dependency-blocked tasks; each entry's `dependencies` field
is an array of `{id, status}` objects (empty when there are no dependencies).

Use `--verbose` to show the expanded text dashboard, including detailed queue
sections such as dependency-blocked tasks, conflict-deferred tasks, recent
messages, and recent completions.

Supported flags: `--repo`, `--watch`, `--interval`, `--format`, `--verbose`,
and `--help`/`-h`.

### `mato init`
`mato init` bootstraps a repository for mato use in one explicit step. It is intended for first-time setup, CI preparation, or dry-run validation flows where users want `.mato/` and the target branch created without running the full orchestrator.

When the target branch does not already exist locally, `mato init` checks the live `origin` branch before creating it. If the remote branch exists, `mato init` creates the local branch from `origin/<branch>`. If the remote is reachable and the branch does not exist there, `mato init` creates the branch from the current `HEAD` and ignores any stale cached `origin/<branch>` ref. If `origin` is unavailable, `mato init` may still fall back to a cached `origin/<branch>` ref and reports that choice in its output.

Use `--format json` for automation and CI bootstrap flows. The JSON form emits the same initialization result as a structured object, including fields such as `dirs_created`, `gitignore_updated`, `branch_name`, `branch_source`, and `tasks_dir`, so scripts do not need to parse the human-readable text output.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. The command resolves it to the repository top level. |
| `--branch <name>` | `mato` | Target branch to create or check out. |
| `--format` | `text` | Output format: `text` or `json`. |

`mato init` always creates the queue at `<repo>/.mato` and ensures `/.mato/` is present in `.gitignore`. When the repository has no commits and `/.mato/` is already ignored, mato still creates the bootstrap commit through normal `git commit` semantics so commit hooks, signing, and other commit-time policy checks still run without staging unrelated files.

### `mato log`
`mato log` shows recent durable task outcomes so operators can answer "what happened recently?" without manually checking multiple queue directories. It reads host-written completion details from `.mato/messages/completions/` plus durable task markers for failures and review rejections. When a task has no durable `<!-- review-rejection: ... -->` marker, the command falls back to the preserved `.mato/messages/verdict-<task>.json` rejection so retryable review feedback still appears in history.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |
| `--limit <n>` | `20` | Maximum number of events to show. `0` means unlimited. |
| `--format` | `text` | Output format: `text` or `json`. |

### `mato graph`
`mato graph` visualizes the task dependency topology. It reuses `PollIndex` and
`DiagnoseDependencies` to show dependency edges, blocked tasks, cycles, and
hidden (off-graph) dependencies. The command is read-only and makes no
filesystem changes.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |
| `--format` | `text` | Output format: `text`, `dot`, or `json`. |
| `--all` | `false` | Include completed and failed tasks. |

Example usage:
```bash
# Text output (default)
mato graph

# Graphviz DOT pipeline
mato graph --format dot | dot -Tpng > deps.png

# Machine-readable JSON
mato graph --format json

# Include completed and failed tasks
mato graph --all
```

### `mato doctor`
`mato doctor` runs structured health checks on the repository and task queue.
Full runs include an explicit `config` check that validates effective repo
defaults using the same env/config/default precedence as `mato config` and
`mato run`. Docker-only runs stay intentionally narrow: `mato doctor --only
docker` resolves only the Docker image and ignores unrelated invalid run
settings. Queue-only runs such as `mato doctor --only queue,tasks,deps` skip
unrelated config validation entirely.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |
| `--fix` | `false` | Auto-repair safe issues: stale locks (agent PIDs, review locks, merge locks), orphaned in-progress tasks, missing queue dirs, Docker image pulls, stale event messages, and leftover atomic-write temp files. |
| `--format` | `text` | Output format: `text` or `json`. |
| `--only <check>` | all checks | Run only specified checks (repeatable). Valid names: `git`, `tools`, `config`, `docker`, `queue`, `tasks`, `locks`, `hygiene`, `deps`. |

Examples:

```bash
# Validate effective repo defaults only
mato doctor --only config

# Validate config, then Docker reachability and image availability
mato doctor --only config,docker

# Validate queue health without unrelated config coupling
mato doctor --only queue,tasks,deps
```

Recommended queue-only preflight command:

```bash
mato doctor --only queue,tasks,deps
```

This focuses on queue layout, task parsing, and dependency integrity without
running Docker checks.

### `mato retry`
`mato retry` requeues one or more failed tasks back to `backlog/`. It reads the
task file from `failed/`, strips task-failure markers (`<!-- failure: -->`,
`<!-- review-failure: -->`, `<!-- cancelled: -->`, `<!-- cycle-failure: -->`, `<!-- terminal-failure: -->`),
and writes the cleaned content to `backlog/`. Review feedback markers
(`<!-- review-rejection: -->`) are preserved so the next attempt can still see
prior reviewer guidance. The original file in `failed/` is only removed after a
successful write, ensuring no data loss on collision or write error. If the retried
task still has unmet `depends_on`, the next reconcile pass moves it back to
`waiting/`. Task refs can be a filename, filename stem, or explicit task `id`
for tasks already in `failed/`.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |

Example usage:
```bash
# Retry a single task
mato retry fix-login-bug

# Retry multiple tasks at once
mato retry fix-login-bug add-dark-mode
```

### `mato inspect`
`mato inspect` explains the current state of one task using the same queue
snapshot and scheduling logic as the host.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |
| `--format` | `text` | Output format: `text` or `json`. |

### `mato cancel`
`mato cancel` withdraws one or more queued tasks by moving them to `failed/`
and appending a `<!-- cancelled: operator at ... -->` marker. It resolves task
refs queue-wide by filename, filename stem, or explicit task `id`, warns when
the cancelled task is still being worked or merged, and reports blocked
dependents that will remain stuck until the task is retried.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |

Example usage:
```bash
# Cancel a single task
mato cancel fix-login-bug

# Cancel multiple tasks at once
mato cancel fix-login-bug add-dark-mode
```

### `mato pause`
`mato pause` writes `<repo>/.mato/.paused` with the current UTC timestamp. The
running orchestrator treats the repo as paused for safety: it skips new task
claims and review launches, continues refreshing `.queue`, and keeps draining
`ready-to-merge/`.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |

### `mato resume`
`mato resume` removes `<repo>/.mato/.paused` and allows the orchestrator to
resume normal claim and review polling.

| Flag | Default | Description |
| --- | --- | --- |
| `--repo <path>` | current directory | Path to the git repository. |

### `mato version`
`mato version` prints the build version in a script-friendly format.

Builds prefer the nearest reachable tag matching `v*`. Non-release tags are
ignored for version stamping. When no matching release tag is reachable, the
build falls back to the commit hash.

Example usage:
```bash
mato version
mato --version
```

### `mato completion`
`mato completion <shell>` prints the shell completion script for one of Cobra's
supported shells.

Example usage:
```bash
mato completion bash > ~/.local/share/bash-completion/completions/mato
mato completion zsh > ~/.zfunc/_mato
```

## Host Configuration Environment Variables
These are the only environment variables intended to be set by users on the host.
They override `.mato.yaml` when both are present. If you want a shared repo
default, prefer `.mato.yaml`; if you want a personal default, prefer these env
vars.

| Variable | Default | Description |
| --- | --- | --- |
| `MATO_BRANCH` | `mato` | Default target branch for `mato run` and `mato init` when `--branch` is not passed. Overrides `.mato.yaml` `branch`. Empty is treated as unset; whitespace-only values are rejected. |
| `MATO_DOCKER_IMAGE` | `ubuntu:24.04` | Docker image used for agent containers. Overrides `.mato.yaml` `docker_image`. |
| `MATO_TASK_MODEL` | `claude-opus-4.6` | Default Copilot model used for task agents. Overrides `.mato.yaml` `task_model`. |
| `MATO_REVIEW_MODEL` | `gpt-5.4` | Default Copilot model used for review agents. Overrides `.mato.yaml` `review_model`. |
| `MATO_REVIEW_SESSION_RESUME_ENABLED` | `true` | Enables durable Copilot session resume for review agents. Accepts `true`/`false` style boolean values. Overrides `.mato.yaml` `review_session_resume_enabled`. There is intentionally no CLI flag for this setting. |
| `MATO_TASK_REASONING_EFFORT` | `high` | Reasoning effort for task agents. Overrides `.mato.yaml` `task_reasoning_effort`. Valid values: `low`, `medium`, `high`, `xhigh`. |
| `MATO_REVIEW_REASONING_EFFORT` | `high` | Reasoning effort for review agents. Overrides `.mato.yaml` `review_reasoning_effort`. Valid values: `low`, `medium`, `high`, `xhigh`. |
| `MATO_AGENT_TIMEOUT` | `30m` | Maximum wall-clock time for a single agent run. Accepts Go duration strings (e.g. `45m`, `1h`). Must be positive. Overrides `.mato.yaml` `agent_timeout`. |
| `MATO_RETRY_COOLDOWN` | `2m` | Minimum time to wait after a task failure before the task can be claimed again. Prevents rapid retry churn when agents crash immediately after launch. Accepts Go duration strings (e.g. `2m`, `5m`, `30s`). Must be positive; invalid values cause an error. Overrides `.mato.yaml` `retry_cooldown`. |

## Injected Container Runtime Variables
These variables are set by `mato` inside agent or review containers at runtime.
They are documented for debugging and custom tooling; users normally do not set
them manually. Think of them as runtime outputs of `mato`, not configuration
inputs.

| Variable | Default | Description |
| --- | --- | --- |
| `MATO_AGENT_ID` | generated per run | Agent identity injected by `mato` so the running agent can identify itself. |
| `MATO_MAX_RETRIES` | `3` | Passed to container for reference; the host enforces the retry budget in `queue.SelectAndClaimTask(...)` and `shouldFailTaskAfterNextFailure(...)` (in `internal/merge/taskops.go`). Per-task overrides via `max_retries` frontmatter take precedence. |
| `MATO_MESSAGING_ENABLED` | `1` | Injected by `mato` for agent-side tooling. The embedded prompt already uses hardcoded `.mato` paths, so this is mainly useful to custom scripts or wrappers. |
| `MATO_MESSAGES_DIR` | `/workspace/.mato/messages` | Injected path to the shared messages directory for custom tooling. The embedded prompt separately hardcodes the same `/workspace/.mato/messages` path. |
| `MATO_TASK_FILE` | none | Claimed task filename (e.g. `my-task.md`). Set per-run by the host after claiming a task. |
| `MATO_TASK_BRANCH` | none | Derived task branch name (e.g. `task/my-task`). Set per-run by the host after claiming a task. |
| `MATO_TASK_TITLE` | none | Extracted from the first non-empty, non-HTML-comment body line in the task file (heading markers stripped if present; leading full-line `<!-- ... -->` comments are skipped), falling back to filename stem. Set per-run by the host after claiming a task. |
| `MATO_TASK_PATH` | none | Absolute path to the task file in `in-progress/` (e.g. `/workspace/.mato/in-progress/my-task.md`). Set per-run by the host after claiming a task. |
| `MATO_DEPENDENCY_CONTEXT` | none | Path to a JSON file containing completion details for resolved `depends_on` tasks (e.g. `/workspace/.mato/messages/dependency-context-my-task.md.json`). Each element contains `task_id`, `task_file`, `branch`, `commit_sha`, `files_changed`, `title`, and `merged_at`. Set per-run by the host only when the claimed task has `depends_on` entries with available completion data in `.mato/messages/completions/`. Written to a file instead of passed inline to avoid ARG_MAX / Docker env var size limits. |
| `MATO_FILE_CLAIMS` | none | Path to the file-claims JSON index inside the container (e.g. `/workspace/.mato/messages/file-claims.json`). The host writes this index before agent launch via `messaging.BuildAndWriteFileClaims(...)`. It maps active `affects:` entries to `{task, status}` objects; keys ending with `/` are directory-prefix claims that apply to all files underneath, and keys containing glob metacharacters (`*`, `?`, `[`, `{`) are glob-pattern claims that apply to any matching file. |
| `MATO_PREVIOUS_FAILURES` | none | Injected when the task file contains previous `<!-- failure: ... -->` records. Contains newline-separated failure lines extracted by `extractFailureLines(...)`. Agents can read this during `VERIFY_CLAIM` to understand why earlier attempts failed and avoid repeating the same mistakes. |
| `MATO_REVIEW_MODE` | none | Set to `1` inside review agent containers. Indicates the container is running a review agent, not a task agent. Not user-configurable. |
| `MATO_REVIEW_FEEDBACK` | none | Injected when prior review feedback is available — either from durable `<!-- review-rejection: ... -->` records in the task file or from a preserved verdict fallback file. Contains newline-separated review rejection records from prior review attempts. The implementing agent can read this during `VERIFY_CLAIM` to address the reviewer's feedback. |
| `MATO_REVIEW_VERDICT_PATH` | none | Path to the JSON verdict file where the review agent writes its verdict (e.g. `/workspace/.mato/messages/verdict-my-task.md.json`). Set per-run by the host when launching a review agent. The verdict structure is `{"verdict":"approve\|reject\|error","reason":"..."}`. Not set for task agents. |

`MATO_DEPENDENCY_CONTEXT` is conditionally injected only when the claimed task has
`depends_on` entries whose completion details are available. It contains a file
path (not inline JSON) to avoid shell ARG_MAX limits with many dependencies.
`MATO_FILE_CLAIMS` is always injected when a task is claimed.
`MATO_PREVIOUS_FAILURES` is conditionally injected only when the task file contains
failure records from prior attempts.
`MATO_REVIEW_MODE` is injected only inside review agent containers.
`MATO_REVIEW_VERDICT_PATH` is injected only inside review agent containers.
`MATO_REVIEW_FEEDBACK` is conditionally injected when prior review feedback is
available. The host checks two sources in priority order: durable
`<!-- review-rejection: ... -->` records in the task file, and a preserved verdict
fallback file (`.mato/messages/verdict-<filename>.json`). The verdict fallback
survives `mato retry` and retry-exhausted transitions, so review feedback remains
accessible across retryable cycles. Only truly terminal transitions (cancel,
merge/completion) clear the verdict fallback.

## Docker Configuration
Each agent run uses `docker run --rm --init` with either `-it` (when stdin is a terminal) or
`-i` (when stdin is not a terminal, e.g. CI, cron, systemd). The `--init` flag ensures an
init process reaps zombie child processes inside the container. The working directory is
`/workspace` and user mapping `--user <host-uid>:<host-gid>` preserves host file
ownership.

### Bind mounts
| Host path | Container path | Notes |
| --- | --- | --- |
| temporary clone of the repo | `/workspace` | The agent works in an isolated clone so multiple agents can run concurrently. |
| `<repo>/.mato` | `/workspace/.mato` | Shares the task queue and messaging state with the host. |
| resolved repo root | same absolute host path | Keeps the clone's `origin` local-path remote reachable for fetch/push. |
| host `copilot` binary | `/usr/local/bin/copilot` (ro) | Runs Copilot CLI inside the container. |
| host `git` binary | `/usr/local/bin/git` (ro) | Provides Git inside the container. |
| host `git-upload-pack` | `/usr/local/bin/git-upload-pack` (ro) | Needed when Git fetches from the local-path remote. |
| host `git-receive-pack` | `/usr/local/bin/git-receive-pack` (ro) | Needed when Git pushes to the local-path remote. |
| host `gh` binary | `/usr/local/bin/gh` (ro) | Makes GitHub CLI available in the container. |
| host `gopls` binary | `/usr/local/bin/gopls` (ro, optional) | Enables Go LSP requests inside the container when `gopls` exists on the host `PATH`. |
| host `GOROOT` | `/usr/local/go` (ro) | Provides the Go toolchain in the container. |
| host `~/.copilot` | `$HOME/.copilot` | For Copilot authentication and package data. |
| host `~/.cache/copilot` | `$HOME/.cache/copilot` | Copilot cache data. |
| host `~/go/pkg/mod` | `$HOME/go/pkg/mod` | Shares Go module cache. |
| host `~/.cache/go-build` | `$HOME/.cache/go-build` | Shares Go build cache. |
| host `~/.config/gh` | `$HOME/.config/gh` (ro) | Mounted only if it exists, to forward `gh` authentication/config. |
| host git-templates dir | same absolute host path (ro) | Mounted only if Git's `init.templateDir` is configured, to preserve Git hooks and templates. |
| host `/etc/ssl/certs` | `/etc/ssl/certs` (ro) | Mounted only if present, to preserve CA trust. |

### Container environment and runtime behavior
- `HOME` inside the container is set to the host home directory path.
- `GOROOT=/usr/local/go` and `PATH` are set so mounted Go and CLI binaries are usable.
- `GOPATH`, `GOMODCACHE`, and `GOCACHE` point at the mounted host cache paths.
- `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=safe.directory`, and `GIT_CONFIG_VALUE_0=*` allow Git to trust mounted worktrees even if ownership looks unusual.
- If Git user name/email are configured on the host repository or globally, `mato` forwards them as `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, and `GIT_COMMITTER_EMAIL`.
- The container command is `copilot [--resume=<session-id>] -p <embedded prompt> --autopilot --allow-all --model <resolved-model> --reasoning-effort <resolved-effort>`. `mato` only appends `--resume=<session-id>` when durable session metadata exists for the current task or review phase.
- If `gopls` is not found on the host `PATH`, `mato` warns before launching the container so missing Go LSP support is explicit instead of surfacing later as a generic Copilot LSP failure.
When choosing a custom Docker image via `MATO_DOCKER_IMAGE` or `.mato.yaml`, use an image compatible with the mounted
host binaries and standard Linux filesystem layout expected above.

## Makefile Targets
The Makefile loads `.env` if present, exports its variables, and defaults to the
`help` target.
| Target | Description |
| --- | --- |
| `build` | Build `bin/mato` with `go build -ldflags "$(GO_LDFLAGS)" -o bin/mato ./cmd/mato`. By default `GO_LDFLAGS` stamps `main.version` from `git describe --tags --match 'v*' --always --dirty`, which prefers release-style `v*` tags, falls back to the commit hash when no matching tag is reachable, and falls back to `dev` when git metadata is unavailable. |
| `install` | Install `mato` into `GOBIN`/`GOPATH/bin` with `go install -ldflags "$(GO_LDFLAGS)" ./cmd/mato`, then run `scripts/install-skill.sh` to install the `mato` skill to `~/.copilot/skills/mato/` and, when `opencode` is on `PATH`, `~/.config/opencode/skills/mato/`. The skill is a task planner that breaks down work into actionable task files. |
| `clean` | Remove the `bin/` directory. |
| `fmt` | Run `go fmt ./...`. |
| `integration-test` | Run `go test -race -v ./internal/integration/...`. |
| `run` | Run `go run -ldflags "$(GO_LDFLAGS)" ./cmd/mato run --repo "$(REPO)"`. `REPO` is required; set it in `.env` or on the command line. |
| `test` | Run `go test -race ./...`. |
| `vet` | Run `go vet ./...`. |
| `lint` | Run `golangci-lint run ./...`. |
| `help` | Print the target list and descriptions. |
Additional behavior:
- `all` runs `fmt`, `vet`, `build`, and `test`.
- `VERSION` can be overridden on the make command line; otherwise it comes from `git describe --tags --match 'v*' --always --dirty`, which ignores non-release tags, falls back to the commit hash when no matching release tag is reachable, and falls back to `dev` when git metadata is unavailable.
- `REPO` is required for `make run` and may be supplied from `.env`.
