# Introduce an explicit run command and structured Copilot settings

`mato` currently behaves like a partial wrapper around the Copilot CLI: the root
command forwards unknown flags, task and review agents share the same model
resolution path, and `default_model` no longer describes the behavior we want.

This proposal replaces that passthrough model with explicit `mato`-owned
configuration for task and review runs.

## Goals

- Remove generic Copilot argument passthrough from the root command.
- Restore standard Cobra parsing by removing `DisableFlagParsing`.
- Introduce `mato run` and make bare `mato` show help instead of implicitly
  starting a run.
- Make task and review model selection explicit and independent.
- Make reasoning effort explicit and independently configurable for task and
  review runs.
- Add first-class CLI flags, env vars, and `.mato.yaml` keys for those settings.
- Simplify CLI ergonomics while touching root flag parsing by promoting `--repo`
  to a persistent root flag shared by subcommands.
- Keep precedence simple: CLI > env > `.mato.yaml` > hardcoded defaults.

## New settings

### CLI flags

These flags apply to `mato run`:

- `--task-model`
- `--review-model`
- `--task-reasoning-effort`
- `--review-reasoning-effort`

### Environment variables

- `MATO_TASK_MODEL`
- `MATO_REVIEW_MODEL`
- `MATO_TASK_REASONING_EFFORT`
- `MATO_REVIEW_REASONING_EFFORT`

### `.mato.yaml`

```yaml
task_model: claude-opus-4.6
review_model: gpt-5.4
task_reasoning_effort: high
review_reasoning_effort: high
```

## Current defaults

- task model: `claude-opus-4.6`
- review model: `gpt-5.4`
- task reasoning effort: `high`
- review reasoning effort: `high`

## Why not generic `*_copilot_args`

This proposal intentionally does not replace CLI passthrough with generic
`task_copilot_args` or `review_copilot_args` settings.

- They mostly recreate passthrough under a different name.
- They weaken validation because `mato` no longer knows which arguments it owns.
- They make docs and help text less clear.
- They make env vars awkward because list-valued arguments need custom parsing.

The goal of this change is to make model and reasoning effort first-class `mato`
settings with explicit semantics rather than preserving a generic Copilot arg
transport layer.

## Removals

- Remove generic Copilot passthrough from the root command.
- Remove `default_model` from `.mato.yaml`.
- Remove `MATO_DEFAULT_MODEL`.
- Remove `copilotArgs` from both the CLI-layer `runConfig` and runner
  `envConfig`.
- Remove `hasModelArg(...)`, `resolveDefaultModel(...)`, and the conditional
  default-injection path that only exists to support passthrough.
- Remove help text and docs that describe forwarding `--model` or using `--` to
  pass raw Copilot args.
- Treat unknown root flags as normal usage errors again.
- Stop treating bare `mato` as implicit run mode.

## Breaking changes

- `default_model` is removed from `.mato.yaml`.
- `MATO_DEFAULT_MODEL` is removed.
- Root-level raw Copilot arg passthrough is removed.
- `mato -- <copilot-args>` is no longer supported as a root run-mode escape
  hatch.
- `mato` with no subcommand now shows help; users must run `mato run` to start
  the orchestrator.
- Unknown root flags now fail with normal Cobra usage errors instead of being
  silently forwarded.

## Why breaking changes are acceptable here

`mato` is still pre-adoption, so this is the best time to clean up the CLI and
configuration surface instead of carrying compatibility layers for behavior we
already want to remove.

- There are no known external users depending on the current passthrough model.
- The current interface reflects an earlier wrapper-oriented design that this
  proposal intentionally replaces.
- Adding compatibility aliases now would preserve confusing terminology such as
  `default_model` and delay the move to an explicit task/review model.
- A clean break keeps implementation, docs, and tests simpler than supporting
  both old and new behavior during an artificial transition period.

## CLI decisions

- Promote `--repo` to a persistent root flag so all repo-aware subcommands share
  one definition and users can write commands like `mato --repo /path status`.
- Introduce `mato run` as the explicit orchestrator command.
- Make bare `mato` show help and available subcommands.
- Keep `mato --version` as a root-level version shortcut, while `mato --help`
  shows root help and `mato run --help` shows run-specific flags.
- Keep `mato version` alongside `mato --version` for consistency with the
  existing version subcommand.
- Keep `--branch` scoped to run and `init`; it is not meaningful for commands
  like `status`, `doctor`, `graph`, or `inspect`.
- Keep `--format` command-local because supported values differ by command.
- Do not add a shared `--model` convenience flag in this change; keeping task
  and review overrides explicit avoids reintroducing ambiguity once their
  defaults intentionally diverge.
- Keep `--task-model`, `--review-model`, `--task-reasoning-effort`, and
  `--review-reasoning-effort` scoped to `mato run`; they do not apply
  to status or maintenance subcommands.

## Validation

- `task_model` and `review_model` must be non-empty after trimming.
- Model names are only validated locally as non-empty strings; actual model
  availability remains enforced by Copilot or the upstream provider.
- `task_reasoning_effort` and `review_reasoning_effort` must match the current
  `copilot --help` choices: `low`, `medium`, `high`, or `xhigh`.
- Reasoning-effort validation intentionally mirrors the current `copilot --help`
  output and may need to be updated if Copilot changes its accepted values.
- CLI flags, env vars, and config file values all use the same normalization
  rules.
- Unknown CLI flags should fail fast instead of falling back to passthrough.
- Validation should happen before Docker launch so bad values fail early.

## Dry-run behavior

- `mato run --dry-run` should accept the new task/review model and reasoning
  flags for consistency with normal run mode.
- Dry-run should resolve and validate those settings using the same precedence
  rules as a real run.
- Dry-run should print the effective resolved task/review model and
  reasoning-effort values so users can debug precedence and configuration.
- Dry-run should not launch containers, invoke Copilot, or attempt to verify
  that a model name is valid server-side.

## Error message expectations

- Unknown root flags should fail with normal Cobra usage errors.
- Unknown run-specific flags should point users to `mato run --help`.
- Invalid reasoning effort values should mention the accepted choices:
  `low`, `medium`, `high`, `xhigh`.
- Removed config keys such as `default_model` should fail as unknown
  `.mato.yaml` fields.

## Non-goals

- Add generic `task_copilot_args` or `review_copilot_args`.
- Preserve arbitrary future Copilot CLI passthrough in this change.
- Expand unrelated command-specific flags into global root flags.
- Change `doctor`, `inspect`, or `status` output to display resolved task/review
  model settings.

## CLI examples

```bash
mato run --task-model claude-opus-4.6 --review-model gpt-5.4
mato run --dry-run --task-model claude-opus-4.6 --review-reasoning-effort high
mato --repo /tmp/repo run --task-reasoning-effort high
mato --repo /tmp/repo status
mato status --repo /tmp/repo
```

## Migration notes

Config and environment migrations should cover all three user-facing surfaces:
CLI flags, host env vars, and `.mato.yaml`.

```bash
# old
mato --model gpt-5.4

# new
mato run --review-model gpt-5.4
```

```bash
# old
mato -- --reasoning-effort high

# new
mato run --task-reasoning-effort high
```

```yaml
# old .mato.yaml
default_model: claude-opus-4.6

# new .mato.yaml
task_model: claude-opus-4.6
review_model: gpt-5.4
```

```bash
# old env
export MATO_DEFAULT_MODEL=claude-opus-4.6

# new env
export MATO_TASK_MODEL=claude-opus-4.6
export MATO_REVIEW_MODEL=gpt-5.4
```

## Acceptance criteria

- Bare `mato` shows help instead of starting the orchestrator.
- `mato run` performs the previous root run behavior.
- `--repo` works as a persistent flag for repo-aware subcommands, including both
  `mato --repo /tmp/repo status` and `mato status --repo /tmp/repo`.
- Unknown root flags fail instead of being forwarded.
- `.mato.yaml` accepts `task_model`, `review_model`,
  `task_reasoning_effort`, and `review_reasoning_effort`.
- `.mato.yaml` no longer accepts `default_model`.
- By default, task runs use `--model claude-opus-4.6 --reasoning-effort high`.
- By default, review runs use `--model gpt-5.4 --reasoning-effort high`.
- CLI, env, and config overrides follow CLI > env > `.mato.yaml` > hardcoded
  default precedence.
- `mato run --dry-run` validates and prints the resolved settings without
  launching Copilot.

## Implementation plan

1. Update CLI parsing in `cmd/mato/main.go`:
   - introduce `mato run` as an explicit subcommand for orchestrator execution
   - make bare `mato` show help instead of starting a run
   - remove `DisableFlagParsing`
   - switch the root command back to normal Cobra flag parsing
   - remove `extractKnownFlags(...)`
   - remove `copilotArgs` from `runConfig`
   - promote `--repo` to a persistent root flag
   - add the four explicit run-only flags on `mato run`
   - resolve those flag values into `runner.RunOptions`
   - keep `--branch` and the new task/review model and reasoning flags scoped to
     `mato run` rather than making them persistent

2. Update config parsing in `internal/config/config.go`:
   - replace `DefaultModel` with `TaskModel`, `ReviewModel`,
     `TaskReasoningEffort`, and `ReviewReasoningEffort`
   - normalize them the same way existing string config values are normalized

3. Update env/config resolution in `cmd/mato/main.go`:
   - read the new env vars
   - resolve CLI/env/config/default precedence for each task/review setting
   - remove references to `MATO_DEFAULT_MODEL`

4. Update runner option plumbing in `internal/runner/runner.go`:
    - expand `RunOptions` to carry task/review model and reasoning effort
    - remove `copilotArgs` from `Run(...)`
    - remove `copilotArgs` from `buildEnvAndRunContext(...)`
    - carry resolved model and reasoning-effort values in `runContext`, since
      they vary per invocation (task vs review) rather than per host environment

5. Update Docker/Copilot command construction in `internal/runner/config.go`:
    - replace the shared passthrough-based model logic
    - keep a single `buildDockerArgs(...)` builder and read resolved model and
      reasoning-effort values from `runContext`
    - always append `--model` and `--reasoning-effort` from resolved settings
    - remove helpers and conditionals that only exist to support passthrough

6. Update task and review launch sites:
    - `internal/runner/task.go` should use task settings
    - `internal/runner/review.go` should use review settings

7. Update tests:
    - remove passthrough-focused tests from `cmd/mato/main_test.go`
    - add CLI/env/config precedence tests for the four new settings
    - add unknown-root-flag failure coverage
    - add coverage that bare `mato` shows help and `mato run` performs the old
      root run behavior
    - add coverage for normal Cobra parsing behavior after removing
      `DisableFlagParsing`
    - update subcommand tests to cover global root-flag usage for persistent
      `--repo`
    - cover both `mato --repo /tmp/repo status` and `mato status --repo /tmp/repo`
    - add dry-run coverage for printing effective resolved task/review settings
    - update `internal/config/config_test.go`
    - update `internal/runner/config_test.go`
    - update `internal/runner/runner_test.go`

8. Update docs:
   - `README.md`
   - `docs/configuration.md`
   - `docs/architecture.md`
   - any implemented proposal docs that still describe passthrough or
     `default_model`

9. Verify with:

```bash
go build ./...
go vet ./...
go test -count=1 ./...
```

## Expected outcome

After this change, `mato` owns its configuration surface instead of acting as a
generic Copilot arg forwarder. Task and review agents can use different models
and reasoning effort defaults with clear precedence and documentation.
