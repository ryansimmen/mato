# Explicit Run Command and Copilot Settings — Implementation Plan

Source: [ideas/explicit-run-command-and-copilot-settings.md](ideas/explicit-run-command-and-copilot-settings.md)

## 1. Goal

Replace the implicit passthrough-based CLI model with explicit `mato`-owned
configuration for task and review agent runs. After this change:

- `mato` shows help instead of starting the orchestrator.
- `mato run` starts the orchestrator.
- Task and review agents use independently configurable model and
  reasoning-effort settings.
- Unknown root flags are properly rejected by Cobra.
- `--repo` works as a persistent root flag shared by all repo-aware
  subcommands.

## 2. Scope

### In scope

- New `mato run` subcommand with all orchestrator behavior.
- Four new settings: task-model, review-model, task-reasoning-effort,
  review-reasoning-effort.
- CLI flags, env vars, and `.mato.yaml` keys for each setting.
- Promoting `--repo` to a persistent root flag.
- Removing `DisableFlagParsing`, `extractKnownFlags`, `copilotArgs`,
  `hasModelArg`, `resolveDefaultModel`, `default_model` /
  `MATO_DEFAULT_MODEL`.
- Dry-run output of resolved settings.
- Updating docs and tests.

### Out of scope

- Generic `task_copilot_args` or `review_copilot_args`.
- Changes to `doctor`, `inspect`, or `status` to display resolved model
  settings.
- Adding a shared `--model` convenience flag.

## 3. Design

### 3.1 Root command behavior

The root command gets these changes:

- Remove `DisableFlagParsing: true` (currently at `cmd/mato/main.go:406`).
- Remove the current orchestrator `RunE` (currently at
  `cmd/mato/main.go:407-461`).
- Set `Use` to `"mato"`.
- Update `Long` to a brief description without passthrough references.
- Update `Example` to show `mato run` instead of bare `mato`.
- Add `Args: usageNoArgs` — rejects all positional arguments on the root
  command. This makes `mato -- --model gpt-5.4` and `mato foo` fail with
  clear usage errors, since Cobra passes tokens after `--` as positional
  args.
- Keep `--version` as a root `Flags().Bool` (not persistent — applies only
  to root).
- New `RunE`:
  1. Check `--version` flag. If true, call `printVersion` and return nil.
  2. Otherwise, return `cmd.Help()`.
- Remove the documentation-only flag definitions for `--branch`, `--dry-run`
  on root (these move to `mato run`).

Result: `mato` → help (exit 0), `mato --version` → version (exit 0),
`mato --help` → help (exit 0), `mato run` → orchestrator,
`mato -- --model gpt-5.4` → positional arg error,
`mato --model gpt-5.4` → unknown flag error.

Root help text must not mention passthrough semantics. `mato run --help`
should expose the new run-only flags.

### 3.2 `mato run` subcommand

A new `newRunCmd()` factory creates the `mato run` command:

- `Use: "run"`, `Short: "Start the orchestrator loop"`
- `Args: usageNoArgs`
- Call `configureCommand(cmd)` for consistent `SilenceUsage`/`SilenceErrors`
  and `FlagErrorFunc` behavior, matching the existing subcommand pattern.
- Flags:
  - `--branch` (string, default "")
  - `--dry-run` (bool, default false)
  - `--task-model` (string, default "")
  - `--review-model` (string, default "")
  - `--task-reasoning-effort` (string, default "")
  - `--review-reasoning-effort` (string, default "")
- `RunE`:
  1. Read `--repo` from persistent root flag (captured variable from
     `newRootCmd` scope).
  2. Resolve repo path, validate it, resolve repo root.
  3. Load `.mato.yaml` config.
  4. Resolve branch (CLI `--branch` > env `MATO_BRANCH` > config `branch` >
     default `"mato"`).
  5. Validate branch.
  6. Call `resolveRunOptions(fileCfg, flagTaskModel, flagReviewModel,
     flagTaskReasoningEffort, flagReviewReasoningEffort)` — returns
     fully-resolved `RunOptions`.
  7. If `--dry-run`: call `dryRunFn(repoRoot, branch, opts)` and return.
  8. Otherwise: call `runFn(repoRoot, branch, opts)` and return.

### 3.3 Persistent `--repo` flag

The root command defines:

```go
var repo string
root.PersistentFlags().StringVar(&repo, "repo", "", "Path to the git repository (default: current directory)")
```

All subcommand factory functions capture the `repo` variable. They can
remain top-level functions that accept `*string` as a parameter, or
closures within `newRootCmd`. Either pattern works; the requirement is that
all subcommands read the same `repo` variable.

All per-subcommand `--repo` flag definitions are removed. Cobra handles
persistent flags in both positions, so `mato --repo /path status` and
`mato status --repo /path` both work.

Note: `mato version --repo /path` is accepted as a harmless no-op.

### 3.4 Whitespace-only `--repo` and `--branch` handling

Under Cobra-based parsing, Cobra delivers raw flag values without trimming.
For `--repo`, whitespace-only values reach `resolveRepo`, which returns the
raw string, then `validateRepoPath` fails with "does not exist." For
`--branch`, whitespace-only values reach `validateBranch`, which delegates
to `git check-ref-format --branch` and fails with "invalid branch name."
No special whitespace rejection is added; existing validation surfaces the
same errors.

### 3.5 Config struct changes

`internal/config/config.go`: Replace `DefaultModel *string
yaml:"default_model"` with:

```go
TaskModel             *string `yaml:"task_model"`
ReviewModel           *string `yaml:"review_model"`
TaskReasoningEffort   *string `yaml:"task_reasoning_effort"`
ReviewReasoningEffort *string `yaml:"review_reasoning_effort"`
```

Update `normalize()` to normalize all four new fields and remove the
`DefaultModel` call.

Because `KnownFields(true)` is already enabled
(`internal/config/config.go:46`), any `.mato.yaml` using `default_model`
will fail with a parse error mentioning the field name.

### 3.6 Default resolution: single authoritative layer

**`resolveRunOptions` in `cmd/mato/main.go` is the single authoritative
layer for applying defaults to the four new settings.** No other function
applies defaults for these settings. (The runner still applies its own
defaults for Docker image, agent timeout, and retry cooldown as it does
today.)

The function signature changes to:

```go
func resolveRunOptions(cfg config.Config, flagTaskModel, flagReviewModel, flagTaskReasoningEffort, flagReviewReasoningEffort string) (runner.RunOptions, error)
```

For each of the four settings, resolution follows:

1. CLI flag value (parameter) — `strings.TrimSpace`; if empty after trim,
   fall through.
2. Environment variable — `strings.TrimSpace(os.Getenv(...))` ; if empty
   after trim, fall through.
3. `.mato.yaml` field — already normalized by config loader.
4. Hardcoded default.

**Environment variables:**

| Setting | Env Var |
|---------|---------|
| Task model | `MATO_TASK_MODEL` |
| Review model | `MATO_REVIEW_MODEL` |
| Task reasoning effort | `MATO_TASK_REASONING_EFFORT` |
| Review reasoning effort | `MATO_REVIEW_REASONING_EFFORT` |

Removed: `MATO_DEFAULT_MODEL`

**Hardcoded defaults:**

- task model: `"claude-opus-4.6"` (constant `runner.DefaultTaskModel`)
- review model: `"gpt-5.4"` (constant `runner.DefaultReviewModel`)
- task reasoning effort: `"high"` (constant `runner.DefaultReasoningEffort`)
- review reasoning effort: `"high"` (constant
  `runner.DefaultReasoningEffort`)

**Validation** happens after resolution:

- `validateReasoningEffort(opts.TaskReasoningEffort,
  "task-reasoning-effort")`
- `validateReasoningEffort(opts.ReviewReasoningEffort,
  "review-reasoning-effort")`

By the time `RunOptions` reaches the runner, all four values are fully
resolved, non-empty, and validated.

### 3.7 RunOptions expansion

`internal/runner/runner.go`: Replace `DefaultModel string` in `RunOptions`
with:

```go
TaskModel             string
ReviewModel           string
TaskReasoningEffort   string
ReviewReasoningEffort string
```

Add exported constants:

```go
const DefaultTaskModel = "claude-opus-4.6"
const DefaultReviewModel = "gpt-5.4"
const DefaultReasoningEffort = "high"
```

Remove `defaultCopilotModel`.

### 3.8 Data flow: envConfig and runContext

**envConfig** (immutable per-run):

- Remove `copilotArgs []string`, `defaultModel string`.
- Add `reviewModel string`, `reviewReasoningEffort string` — set from
  `opts.ReviewModel` / `opts.ReviewReasoningEffort` (pre-resolved).

**runContext** (per-invocation):

- Add `model string`, `reasoningEffort string`.

Task invocations: `run.model = opts.TaskModel`,
`run.reasoningEffort = opts.TaskReasoningEffort` (set during
`buildEnvAndRunContext`).

Review invocations: `runReview` overrides
`run.model = env.reviewModel`,
`run.reasoningEffort = env.reviewReasoningEffort` before
`buildDockerArgs`.

### 3.9 buildDockerArgs changes

`internal/runner/config.go`:

- Remove `hasModelArg()`, `resolveDefaultModel()`.
- Replace conditional model/copilotArgs block in `buildDockerArgs()` with:
  ```go
  args = append(args, "--model", run.model, "--reasoning-effort", run.reasoningEffort)
  ```

### 3.10 buildEnvAndRunContext changes

Signature: remove `copilotArgs []string` parameter.

Body:

- Remove `model := resolveDefaultModel(opts.DefaultModel)`,
  `env.copilotArgs`, `env.defaultModel`.
- Set `env.reviewModel = opts.ReviewModel`,
  `env.reviewReasoningEffort = opts.ReviewReasoningEffort`.
- Set `run.model = opts.TaskModel`,
  `run.reasoningEffort = opts.TaskReasoningEffort`.

### 3.11 Run / DryRun signature changes

`Run`: remove `copilotArgs` parameter.

`DryRun`: add `opts RunOptions` parameter; print
`=== Resolved Settings ===` section:

```
=== Resolved Settings ===
  task model:              claude-opus-4.6
  review model:            gpt-5.4
  task reasoning effort:   high
  review reasoning effort: high
```

`DryRun` reads from `opts` directly — no defaults applied.

### 3.12 runReview changes

Before `buildDockerArgs`:

```go
run.model = env.reviewModel
run.reasoningEffort = env.reviewReasoningEffort
```

Go value semantics ensure the caller's `runContext` is unaffected.

### 3.13 Validation

New in `cmd/mato/main.go`:

```go
func validateReasoningEffort(value, flagName string) error
```

Accepted: `"low"`, `"medium"`, `"high"`, `"xhigh"` (case-sensitive).
Error: `fmt.Errorf("invalid %s %q: must be one of low, medium, high,
xhigh", flagName, value)`.

### 3.14 task.go impact

No changes needed. `runOnce` calls
`buildDockerArgs(env, run, extraEnvs, nil)` — `run` already carries task
model/effort.

## 4. Step-by-Step Breakdown

### Step 1: Update `internal/config/config.go`

**Dependencies:** None

- Replace `DefaultModel *string yaml:"default_model"` with four new pointer
  fields.
- Update `normalize()`: add four new `normalizeString` calls, remove
  `DefaultModel` call.

### Step 2: Update `internal/config/config_test.go`

**Dependencies:** Step 1

- Update `TestLoad_AllFields`: new YAML keys, new field assertions.
- Update `TestLoad_PartialFields`: verify new fields nil when absent.
- Update `TestLoad_EmptyStringValues`: verify new fields normalize to nil.
- Add test: `default_model` in YAML triggers unknown-key error.

### Step 3: Update `internal/runner/runner.go`

**Dependencies:** Step 1

- Replace `DefaultModel` in `RunOptions` with four new fields.
- Add exported constants: `DefaultTaskModel`, `DefaultReviewModel`,
  `DefaultReasoningEffort`.
- Remove `defaultCopilotModel`.
- Update `buildEnvAndRunContext`: remove `copilotArgs` param; set review
  fields on env, task fields on run.
- Update `Run`: remove `copilotArgs` param.
- Update `DryRun`: add `opts RunOptions` param, add resolved-settings
  output.

### Step 4: Update `internal/runner/config.go`

**Dependencies:** Step 3

- Remove `copilotArgs`, `defaultModel` from `envConfig`.
- Add `reviewModel`, `reviewReasoningEffort` to `envConfig`.
- Add `model`, `reasoningEffort` to `runContext`.
- Remove `hasModelArg()`, `resolveDefaultModel()`.
- Update `buildDockerArgs()`: unconditional `--model run.model
  --reasoning-effort run.reasoningEffort`.

### Step 5: Update `internal/runner/review.go`

**Dependencies:** Step 4

- Override model/effort in `runReview` before `buildDockerArgs`.

### Step 6: Update `internal/runner/config_test.go`

**Dependencies:** Step 4

- Remove: `TestBuildDockerArgs_CopilotArgsPassthrough`,
  `TestResolveDefaultModel_*` (2 tests), `TestHasModelArg_*` (4 tests).
- Add: model and reasoning-effort from runContext appear in Docker args.
- Add: different model values produce correct args.
- Update: all existing test setups to use new envConfig/runContext fields.

### Step 7: Update `internal/runner/runner_test.go`

**Dependencies:** Steps 3, 5

- Update `buildEnvAndRunContext` tests for new fields and removed
  copilotArgs.
- Update `DryRun` tests: new signature, verify resolved-settings output.
- Update `Run` tests: removed copilotArgs param.

### Step 8: Update `internal/runner/review_test.go`

**Dependencies:** Step 5

- Add: `runReview` uses `env.reviewModel` and `env.reviewReasoningEffort`
  in Docker args.

### Step 9: Update `cmd/mato/main.go`

**Dependencies:** Steps 1-5

- Remove `extractKnownFlags()`, `assignFlag()`.
- Remove `copilotArgs` from `runConfig` (or remove `runConfig` entirely).
- Remove `DisableFlagParsing: true`.
- Update root: `Use`, `Long`, `Example`; add `Args: usageNoArgs`; new
  `RunE` (check `--version`, else help); remove doc-only `--branch` /
  `--dry-run` flags.
- Promote `--repo` to `root.PersistentFlags().StringVar`.
- Create `newRunCmd()` with `configureCommand(cmd)`, flags, and `RunE`
  orchestrator logic.
- Add `root.AddCommand(newRunCmd())`.
- Remove all per-subcommand `--repo` flag definitions; update factories to
  use captured `repo` variable.
- Update `resolveRunOptions`: new signature with flag params, four new
  resolution chains, remove `DefaultModel` / `MATO_DEFAULT_MODEL`.
- Add `validateReasoningEffort`.
- Update `runFn` type: `func(string, string, runner.RunOptions) error`.
- Update `dryRunFn` type: `func(string, string, runner.RunOptions) error`.

### Step 10: Update `cmd/mato/main_test.go`

**Dependencies:** Step 9

- Remove: `TestExtractKnownFlags*` tests, copilot arg forwarding tests.
- Add: bare `mato` shows help (exit 0).
- Add: `mato --version` prints version.
- Add: `mato run` triggers orchestrator.
- Add: four new flags pass correct values to `runFn`.
- Add: `validateReasoningEffort` accepts valid values, rejects invalid.
- Add: table-driven precedence tests (CLI > env > config > default) for
  all four settings.
- Add: whitespace-only env values treated as unset.
- Add: unknown root flags rejected (`mato --model gpt-5.4`).
- Add: root positional args rejected (`mato foo`).
- Add: root `--` passthrough rejected (`mato -- --model gpt-5.4`).
- Add: run `--` passthrough rejected (`mato run -- --model gpt-5.4`).
- Add: persistent `--repo` in multiple positions.
- Add: `mato run --dry-run` calls `dryRunFn` with resolved opts.
- Add: `default_model` in config causes error.
- Update: `TestResolveRunOptions`, existing subcommand tests.

### Step 11: Update integration tests

**Dependencies:** Steps 3, 9

- `internal/integration/config_test.go:33`: Replace
  `"MATO_DEFAULT_MODEL"` with `"MATO_TASK_MODEL"`, `"MATO_REVIEW_MODEL"`,
  `"MATO_TASK_REASONING_EFFORT"`, `"MATO_REVIEW_REASONING_EFFORT"` in
  `filteredHostEnv`.
- `internal/integration/config_test.go:65`: Add `"run"` subcommand to
  invocation.
- `internal/integration/config_test.go:78`: Add `"run"` subcommand to
  invocation.
- `internal/integration/init_test.go:60`: Update `runner.DryRun` call to
  new signature with `RunOptions`.

### Step 12: Update documentation

**Dependencies:** All code steps

**`README.md`:**

- Quick Start: `mato` → `mato run`.
- Useful flags: remove root-level `--model` forwarding, add `mato run`
  flags.
- Docker section: remove `mato --model gpt-5.4`, add
  `mato run --task-model ...`.
- Remove `--` passthrough description.
- Update `.mato.yaml`: remove `default_model`, add new keys.

**`docs/configuration.md`:**

- Root usage line: `mato [--version] [--repo <path>]` — no `--branch`,
  `--dry-run`, or `[copilot-args...]`.
- Add `mato run` usage line with new flags.
- Config File YAML: replace `default_model` with four new keys.
- Precedence table: replace `default_model` row with four new rows.
- CLI Flags table: remove `--` and forwarded `--model`, add `mato run`
  flags; note `--repo` as persistent root flag.
- Remove all passthrough references.

**`docs/architecture.md`:**

- Update `Run()` signature (remove `copilotArgs`).
- Update model resolution description.
- Update `RunOptions` field list.
- Remove `MATO_DEFAULT_MODEL` references.

**`docs/proposals/implemented/add-config-file.md`:**

- Replace `default_model` references with `task_model` / `review_model`
  and reasoning-effort fields throughout.
- Update `Config` struct example to show new fields.
- Update precedence table: replace `default_model` row with four new rows.
- Replace `MATO_DEFAULT_MODEL` with new env var names.
- Update `RunOptions` struct to show new fields.
- Remove `copilotArgs` from `Run` signature and execution flow.
- Remove `hasModelArg()` and forwarded `--model` footnotes.

**`docs/proposals/implemented/mato-init.md`:**

- Line 60: Update or remove the `(no DisableFlagParsing)` note, since
  `DisableFlagParsing` is removed from the root command entirely and is no
  longer a distinguishing characteristic.

**`docs/proposals/implemented/add-cancel-command.md`:**

- No changes needed. The word "forwarded" on line 678 refers to error
  propagation from `queue.ResolveTask`, not Copilot arg passthrough.

### Step 13: Verify

```bash
go build ./... && go vet ./... && go test -count=1 ./...
```

## 5. File Changes

| File | Action | What Changes |
|------|--------|-------------|
| `internal/config/config.go` | Modify | Replace `DefaultModel` with four new fields; update `normalize()` |
| `internal/config/config_test.go` | Modify | Update field tests; add `default_model` rejection test |
| `internal/runner/runner.go` | Modify | Update `RunOptions`, constants, `Run`/`DryRun` signatures, `buildEnvAndRunContext` |
| `internal/runner/config.go` | Modify | Update `envConfig`, `runContext`, `buildDockerArgs`; remove `hasModelArg`, `resolveDefaultModel` |
| `internal/runner/config_test.go` | Modify | Remove passthrough/model-arg tests; add model/reasoning-effort tests |
| `internal/runner/runner_test.go` | Modify | Update `buildEnvAndRunContext`, `DryRun`, `Run` tests |
| `internal/runner/review.go` | Modify | Override `run.model`/`run.reasoningEffort` in `runReview` |
| `internal/runner/review_test.go` | Modify | Add review model/reasoning-effort override tests |
| `internal/runner/task.go` | No change | Already uses `run.model` via `buildDockerArgs` |
| `cmd/mato/main.go` | Modify | Major: add `newRunCmd`, remove passthrough, promote `--repo`, add validation/Args |
| `cmd/mato/main_test.go` | Modify | Major: remove passthrough tests, add run/precedence/validation/positional tests |
| `internal/integration/config_test.go` | Modify | Update env filter, add `run` subcommand |
| `internal/integration/init_test.go` | Modify | Update `DryRun` call signature |
| `README.md` | Modify | Update CLI examples, remove passthrough docs |
| `docs/configuration.md` | Modify | Update root usage, add `mato run`, update config/precedence/flags |
| `docs/architecture.md` | Modify | Update Run signature, model resolution, RunOptions |
| `docs/proposals/implemented/add-config-file.md` | Modify | Replace `default_model`/`MATO_DEFAULT_MODEL`/`copilotArgs`/passthrough |
| `docs/proposals/implemented/mato-init.md` | Modify | Update `DisableFlagParsing` note |

## 6. Error Handling

- **Bare `mato`**: Shows help, exit 0.
- **`mato foo`**: `Args: usageNoArgs` rejects positional args → usage
  error.
- **`mato -- --model gpt-5.4`**: Cobra passes as positional args →
  `usageNoArgs` rejects → usage error.
- **`mato --model gpt-5.4`**: Unknown flag → usage error.
- **`mato run -- --model gpt-5.4`**: `mato run` also has
  `Args: usageNoArgs` → positional arg error.
- **Unknown `mato run` flags**: Cobra rejects → `FlagErrorFunc` wraps with
  `UsageError`.
- **Invalid reasoning effort**: `validateReasoningEffort` returns error
  listing `low`, `medium`, `high`, `xhigh`.
- **`default_model` in `.mato.yaml`**: `KnownFields(true)` rejects with
  parse error.
- **Whitespace-only `--repo`**: Reaches `validateRepoPath`, fails with
  "does not exist".
- **Whitespace-only `--branch`**: Reaches `validateBranch` →
  `git check-ref-format` rejects.
- **Whitespace-only env/CLI model/reasoning values**: Normalized to empty →
  unset → falls through to default.

## 7. Testing Strategy

### Unit tests (`internal/config/config_test.go`)

- All four new fields parse correctly from YAML.
- Whitespace/empty normalization for all four fields.
- `default_model` triggers unknown-key error.
- Partial configs.

### Unit tests (`internal/runner/config_test.go`)

- `buildDockerArgs` includes `--model` and `--reasoning-effort` from
  `runContext`.
- Different model values produce different args.
- All `hasModelArg`/`resolveDefaultModel`/`CopilotArgsPassthrough` tests
  removed.

### Unit tests (`internal/runner/runner_test.go`)

- `buildEnvAndRunContext` populates `run.model`/`run.reasoningEffort` from
  opts.
- `buildEnvAndRunContext` populates
  `env.reviewModel`/`env.reviewReasoningEffort` from opts.
- `buildEnvAndRunContext` no longer takes `copilotArgs`.
- `DryRun` prints resolved settings, accepts `RunOptions`.

### Unit tests (`internal/runner/review_test.go`)

- `runReview` uses `env.reviewModel`/`env.reviewReasoningEffort` in Docker
  args.

### Unit tests (`cmd/mato/main_test.go`)

- Bare `mato` shows help, exit 0.
- `mato --version` prints version.
- `mato run` triggers orchestrator.
- Four new flags pass correct values.
- `validateReasoningEffort` tests (valid and invalid).
- Precedence: CLI > env > config > default (table-driven).
- Whitespace-only env values treated as unset.
- Unknown root flags rejected.
- Root positional args rejected (`mato foo`).
- Root `--` passthrough rejected (`mato -- --model gpt-5.4`).
- Run `--` passthrough rejected (`mato run -- --model gpt-5.4`).
- Persistent `--repo` in multiple positions.
- `mato run --dry-run` calls `dryRunFn` with opts.
- `default_model` in config causes error.

### Integration tests

- `config_test.go`: updated env filter, `run` subcommand.
- `init_test.go`: updated `DryRun` signature.

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Persistent `--repo` conflicts with per-subcommand `--repo` | Remove all per-subcommand definitions first |
| `mato --version` needs `RunE` | Root `RunE` checks `--version` first |
| Breaking: `mato` no longer starts orchestrator | Acceptable per ideas doc; pre-adoption project |
| Signature changes break call sites | All updated simultaneously |
| `mato version --repo /path` harmless no-op | Consistent with Cobra persistent flag semantics |
| Reasoning-effort values may change | Easy to update accepted set |
| `runReview` modifies `runContext` | Go value semantics prevent caller modification |

## 9. Open Questions

None — all design decisions are resolved.
