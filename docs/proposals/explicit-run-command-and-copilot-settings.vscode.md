# VsCode Explicit Run Command and Copilot Settings — Implementation Plan

## 1. Goal

Replace `mato`'s generic Copilot argument passthrough with explicit, first-class
settings for task and review model selection and reasoning effort. Introduce
`mato run` as the explicit orchestrator command, make bare `mato` show help, and
promote `--repo` to a persistent root flag shared by all subcommands.

This cleans up the CLI surface, removes `DisableFlagParsing` and the manual
`extractKnownFlags` parser, restores standard Cobra flag handling, and makes
task/review configuration independently configurable across CLI flags, env vars,
and `.mato.yaml`.

## 2. Scope

### In Scope

- Introduce `mato run` subcommand that performs the current root run behavior.
- Make bare `mato` (no subcommand) show help instead of implicit run.
- Remove `DisableFlagParsing` from the root command.
- Remove `extractKnownFlags()` and restore standard Cobra flag parsing.
- Promote `--repo` to a persistent root flag shared by all subcommands.
- Add `--task-model`, `--review-model`, `--task-reasoning-effort`,
  `--review-reasoning-effort` as flags on `mato run`.
- Add `MATO_TASK_MODEL`, `MATO_REVIEW_MODEL`, `MATO_TASK_REASONING_EFFORT`,
  `MATO_REVIEW_REASONING_EFFORT` env vars.
- Add `task_model`, `review_model`, `task_reasoning_effort`,
  `review_reasoning_effort` to `.mato.yaml`.
- Remove `default_model` from `Config`, `DefaultModel` from `RunOptions`,
  `MATO_DEFAULT_MODEL` env var, `copilotArgs` from `runConfig`/`envConfig`,
  `hasModelArg()`, `resolveDefaultModel()`, and `extractKnownFlags()`.
- Move model and reasoning effort into `runContext` (per-invocation) so task
  and review calls can use different values.
- Update `buildDockerArgs` to always append `--model` and `--reasoning-effort`
  from `runContext`.
- Update `DryRun` to accept, validate, and print resolved task/review settings.
- Update tests across `cmd/mato/main_test.go`, `internal/runner/config_test.go`,
  `internal/runner/runner_test.go`, and `internal/config/config_test.go`.
- Update documentation: `README.md`, `docs/configuration.md`,
  `docs/architecture.md`.
- Update `internal/integration/config_test.go` helper to exclude new env vars.

### Out of Scope

- Generic `task_copilot_args` / `review_copilot_args` settings.
- Changes to `doctor`, `inspect`, or `status` output.
- Shared `--model` convenience flag.
- Support for future Copilot CLI flags beyond model and reasoning effort.

## 3. Design

### 3.1 New `mato run` Subcommand

The current root `RunE` handler becomes the body of a new `newRunCmd()` function
that returns a `*cobra.Command` with `Use: "run"`. The root command uses Cobra's
built-in version support via `root.Version = version` and
`root.SetVersionTemplate(...)`. This means `mato --version` and `mato version`
both work without a custom `RunE` on the root. Bare `mato` (no subcommand, no
`--version`) shows help by Cobra's default behavior when no `RunE` is set.

The root command drops `DisableFlagParsing: true` and removes all flag
definitions that were previously documentation-only (`--repo`, `--branch`,
`--dry-run`, `--version`). The `--repo` flag moves to
`root.PersistentFlags().StringVar(...)` so all subcommands inherit it. The
existing `newVersionCmd()` subcommand is kept for `mato version` compatibility.

The run command owns these flags:
- `--branch` (string)
- `--dry-run` (bool)
- `--task-model` (string)
- `--review-model` (string)
- `--task-reasoning-effort` (string)
- `--review-reasoning-effort` (string)

### 3.2 Persistent `--repo` Flag

Currently, every subcommand (`status`, `doctor`, `graph`, `init`, `inspect`,
`retry`, `cancel`, `pause`, `resume`) declares its own local `--repo` string
variable and `cmd.Flags().StringVar(...)` call. After this change:

- The root command declares `var repoFlag string` and registers it as
  `root.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to the git
  repository (default: current directory)")`.
- All subcommands remove their local `--repo` declarations and instead read
  `repoFlag` from the closure (or from a package-level var if needed).
- `resolveRepo(repoFlag)` continues to work as before — it returns the flag
  value if non-empty, or `os.Getwd()` otherwise.

This means both `mato --repo /path status` and `mato status --repo /path` work
identically, since Cobra resolves persistent flags from any position.

### 3.3 Config File Changes

`internal/config/config.go`:

```go
type Config struct {
    Branch               *string `yaml:"branch"`
    DockerImage          *string `yaml:"docker_image"`
    TaskModel            *string `yaml:"task_model"`
    ReviewModel          *string `yaml:"review_model"`
    TaskReasoningEffort  *string `yaml:"task_reasoning_effort"`
    ReviewReasoningEffort *string `yaml:"review_reasoning_effort"`
    AgentTimeout         *string `yaml:"agent_timeout"`
    RetryCooldown        *string `yaml:"retry_cooldown"`
}
```

The `DefaultModel *string` field and its `yaml:"default_model"` tag are removed.
Because `dec.KnownFields(true)` is already set, any `.mato.yaml` that still
contains `default_model` will fail with a clear parse error — no migration code
is needed.

The `normalize()` function gains entries for the four new fields.

### 3.4 RunOptions Changes

`internal/runner/runner.go`:

```go
type RunOptions struct {
    DockerImage          string
    TaskModel            string
    ReviewModel          string
    TaskReasoningEffort  string
    ReviewReasoningEffort string
    AgentTimeout         time.Duration
    RetryCooldown        time.Duration
}
```

`DefaultModel` is removed and replaced by `TaskModel` and `ReviewModel`.
`TaskReasoningEffort` and `ReviewReasoningEffort` are new string fields.

### 3.5 Removing `copilotArgs` from `envConfig`

The `copilotArgs []string` field is removed from `envConfig`. The
`defaultModel string` field is also removed from `envConfig` since model
selection is now per-invocation, not per-environment.

### 3.6 Moving Model/Reasoning to `runContext`

`runContext` gains two new fields:

```go
type runContext struct {
    cloneDir        string
    prompt          string
    agentID         string
    timeout         time.Duration
    model           string
    reasoningEffort string
}
```

These are set differently for task runs vs. review runs:

- **Task path** (`runOnce`): `run.model = opts.TaskModel`,
  `run.reasoningEffort = opts.TaskReasoningEffort` (with defaults applied).
- **Review path** (`runReview`): `run.model = opts.ReviewModel`,
  `run.reasoningEffort = opts.ReviewReasoningEffort` (with defaults applied).

The resolved model and reasoning effort are assigned before calling
`buildDockerArgs` in each path.

### 3.7 `buildDockerArgs` Changes

`internal/runner/config.go`:

The function signature stays the same: `buildDockerArgs(env envConfig, run
runContext, extraEnvs []string, extraVolumes []string) []string`. However, the
tail of the function changes from:

```go
// Old
args = append(args, "copilot", "-p", run.prompt, "--autopilot", "--allow-all")
if !hasModelArg(env.copilotArgs) {
    args = append(args, "--model", resolveDefaultModel(env.defaultModel))
}
args = append(args, env.copilotArgs...)
```

To:

```go
// New
args = append(args, "copilot", "-p", run.prompt, "--autopilot", "--allow-all",
    "--model", run.model,
    "--reasoning-effort", run.reasoningEffort,
)
```

The `resolveDefaultModel()`, `hasModelArg()` functions are deleted.
The `defaultCopilotModel` constant is replaced by two constants:

```go
const defaultTaskModel    = "claude-opus-4.6"
const defaultReviewModel  = "gpt-5.4"
const defaultReasoningEffort = "high"
```

### 3.8 `buildEnvAndRunContext` Changes

The `copilotArgs []string` parameter is removed. The `defaultModel` field is no
longer set on `envConfig`. Model and reasoning effort defaults are applied when
constructing the initial `runContext`:

```go
func buildEnvAndRunContext(branch string, tools hostTools, agentID, gitName, gitEmail string, repoRoot, tasksDir string, opts RunOptions) (envConfig, runContext) {
    // ... existing image/timeout resolution ...

    taskModel := opts.TaskModel
    if taskModel == "" {
        taskModel = defaultTaskModel
    }
    taskReasoning := opts.TaskReasoningEffort
    if taskReasoning == "" {
        taskReasoning = defaultReasoningEffort
    }

    // runContext starts with task defaults; review overrides model/reasoning
    // before calling buildDockerArgs in the review path.
    run := runContext{
        prompt:          prompt,
        agentID:         agentID,
        timeout:         timeout,
        model:           taskModel,
        reasoningEffort: taskReasoning,
    }
    return env, run
}
```

The review model and reasoning effort are applied in the review call path (see
§3.9).

### 3.9 Task and Review Launch Sites

**`internal/runner/task.go` — `runOnce`**: No model/reasoning changes needed
because `buildEnvAndRunContext` already sets task defaults on `runContext`. The
`runContext` passed to `runOnce` carries the correct task model and reasoning
effort.

**`internal/runner/review.go` — `runReview`**: Before calling `buildDockerArgs`,
the function overrides model and reasoning effort on the `runContext`:

```go
func runReview(ctx context.Context, env envConfig, run runContext, task *queue.ClaimedTask, branch string, opts RunOptions) error {
    run.prompt = strings.ReplaceAll(reviewInstructions, ...)
    // Override model/reasoning for review
    run.model = opts.ReviewModel
    if run.model == "" {
        run.model = defaultReviewModel
    }
    run.reasoningEffort = opts.ReviewReasoningEffort
    if run.reasoningEffort == "" {
        run.reasoningEffort = defaultReasoningEffort
    }
    // ... rest of review logic ...
}
```

This requires threading `opts RunOptions` (or at least the review-specific
fields) through the `pollLoop` → `pollIterate` → review call chain. The cleanest
approach: `pollLoop` already receives `cooldown time.Duration` from `opts`;
expand it to receive the full `RunOptions` (or a `reviewSettings` subset). Since
`RunOptions` is a simple value struct, passing it is lightweight.

### 3.10 `Run()` Signature Change

The exported `Run` function changes from:

```go
func Run(repoRoot, branch string, copilotArgs []string, opts RunOptions) error
```

To:

```go
func Run(repoRoot, branch string, opts RunOptions) error
```

The `copilotArgs` parameter is removed. Callers in `cmd/mato/main.go` are
updated accordingly.

### 3.11 `pollLoop` and `pollIterate` Plumbing

`pollLoop` currently receives `cooldown time.Duration`. This changes to receive
the full `RunOptions` so it can pass review settings to `pollIterate` and
ultimately to `runReview`. A lightweight alternative would be a subset struct,
but `RunOptions` is small and already exists.

```go
func pollLoop(ctx context.Context, env envConfig, run runContext, repoRoot, tasksDir, branch, agentID string, opts RunOptions) error
```

`pollIterate` similarly receives `RunOptions` and passes review settings when
invoking the review path.

### 3.12 Precedence Resolution

`resolveRunOptions` in `cmd/mato/main.go` changes to accept CLI flag values and
resolve them against env vars and config:

```go
type runFlags struct {
    taskModel            string
    reviewModel          string
    taskReasoningEffort  string
    reviewReasoningEffort string
    branch               string
    dryRun               bool
}

func resolveRunOptions(flags runFlags, cfg config.Config) (runner.RunOptions, error) {
    var opts runner.RunOptions

    // Docker image (no CLI flag for this)
    // ... existing docker_image resolution ...

    // Task model: CLI > env > config > default
    opts.TaskModel = resolveStringOption(flags.taskModel, "MATO_TASK_MODEL", cfg.TaskModel)

    // Review model: CLI > env > config > default
    opts.ReviewModel = resolveStringOption(flags.reviewModel, "MATO_REVIEW_MODEL", cfg.ReviewModel)

    // Task reasoning effort: CLI > env > config > default
    raw := resolveStringOption(flags.taskReasoningEffort, "MATO_TASK_REASONING_EFFORT", cfg.TaskReasoningEffort)
    if raw != "" {
        if err := validateReasoningEffort(raw); err != nil {
            return opts, err
        }
        opts.TaskReasoningEffort = raw
    }

    // Review reasoning effort: CLI > env > config > default
    raw = resolveStringOption(flags.reviewReasoningEffort, "MATO_REVIEW_REASONING_EFFORT", cfg.ReviewReasoningEffort)
    if raw != "" {
        if err := validateReasoningEffort(raw); err != nil {
            return opts, err
        }
        opts.ReviewReasoningEffort = raw
    }

    // ... existing timeout/cooldown resolution ...
    return opts, nil
}

func resolveStringOption(flagVal, envKey string, configVal *string) string {
    if v := strings.TrimSpace(flagVal); v != "" {
        return v
    }
    if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
        return v
    }
    if configVal != nil {
        return *configVal // already normalized by config.normalize()
    }
    return ""
}
```

### 3.13 Reasoning Effort Validation

A new `validateReasoningEffort` function in `cmd/mato/main.go`:

```go
var validReasoningEfforts = map[string]bool{
    "low": true, "medium": true, "high": true, "xhigh": true,
}

func validateReasoningEffort(value string) error {
    if !validReasoningEfforts[value] {
        return fmt.Errorf("invalid reasoning effort %q: must be one of low, medium, high, xhigh", value)
    }
    return nil
}
```

Validation runs after precedence resolution so that invalid values from any
source (CLI, env, config) are caught before Docker launch.

### 3.14 Model Validation

Model names are validated only as non-empty after trimming. Actual model
availability is enforced by Copilot at runtime. Since defaults are hardcoded,
empty model values only occur if a user explicitly sets an empty/whitespace
override, which `normalizeString` in config already handles. No additional
validation needed beyond what `normalizeString` provides for config values and
`strings.TrimSpace` provides for env/CLI values.

### 3.15 Dry-Run Changes

`runner.DryRun` currently takes `(repoRoot, branch string)`. It changes to also
accept the fully resolved `RunOptions` for settings display. This reuses the
existing struct rather than introducing a dedicated dry-run settings type:

```go
func DryRun(repoRoot, branch string, opts RunOptions) error
```

After the existing dry-run sections, a new section prints resolved settings
(applying defaults for empty fields so the output always shows effective values):

```
=== Resolved Settings ===
  task_model:              claude-opus-4.6
  review_model:            gpt-5.4
  task_reasoning_effort:   high
  review_reasoning_effort: high
```

The `mato run` handler passes the same `RunOptions` to both `DryRun` and `Run`.

### 3.16 `runFn` and `dryRunFn` Test Hook Updates

The package-level test hook variables change signatures:

```go
// Old
var runFn = runner.Run        // func(string, string, []string, RunOptions) error
var dryRunFn = runner.DryRun  // func(string, string) error

// New
var runFn = runner.Run        // func(string, string, RunOptions) error
var dryRunFn = runner.DryRun  // func(string, string, RunOptions) error
```

All test call sites that inject stubs into `runFn` and `dryRunFn` are updated.

## 4. Step-by-Step Breakdown

Steps are ordered so that each step compiles and passes tests after completion.
Cross-API changes that span caller and callee are grouped in the same step to
avoid intermediate breakage. Dependencies are noted in parentheses.

**Note:** These steps are planning milestones within a single feature branch,
not independently landable commits. Each step should compile and pass tests, but
the feature is shipped as one cohesive PR. Steps 1 and 2 may temporarily leave
the runtime with different settings than the final state (e.g., Step 1 removes
`default_model` before the replacement settings are wired end-to-end), which is
fine because the branch is not merged until all steps are complete.

### Step 1: Update `Config` struct

**Files:** `internal/config/config.go`, `internal/config/config_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

1. Remove `DefaultModel *string` from `Config`.
2. Add `TaskModel`, `ReviewModel`, `TaskReasoningEffort`,
   `ReviewReasoningEffort` as `*string` fields with appropriate YAML tags.
3. Update `normalize()` to handle the four new fields.
4. Update `resolveRunOptions` in `cmd/mato/main.go` to stop reading
   `cfg.DefaultModel` and stop resolving `MATO_DEFAULT_MODEL`. The four new
   config fields exist on `Config` after item 2, but `resolveRunOptions` does
   not populate `RunOptions` with them yet — that happens in Step 2 when
   `RunOptions` itself gains the new fields.
5. Update `config_test.go`:
   - Remove tests referencing `default_model`.
   - Add tests for parsing the four new keys.
   - Add test that `default_model` in YAML causes a parse error (unknown field).
6. Update `cmd/mato/main_test.go`:
   - Fix any `resolveRunOptions` test that references `DefaultModel` or
     `MATO_DEFAULT_MODEL`.

This step compiles because both the struct change and its callers are updated
together.

### Step 2: Update `RunOptions`, runner internals, and `buildDockerArgs`

**Files:** `internal/runner/runner.go`, `internal/runner/runner_test.go`,
`internal/runner/config.go`, `internal/runner/config_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Depends on:** Step 1

This step is atomic to avoid a compile gap: removing `DefaultModel` from
`RunOptions` and `defaultCopilotModel` from `config.go` must happen in the same
step as updating `buildEnvAndRunContext` and `buildDockerArgs` (which read them).

1. Remove `DefaultModel string` from `RunOptions`.
2. Add `TaskModel`, `ReviewModel`, `TaskReasoningEffort`,
   `ReviewReasoningEffort` as `string` fields on `RunOptions`.
3. Replace `defaultCopilotModel` constant with `defaultTaskModel`,
   `defaultReviewModel`, `defaultReasoningEffort`.
4. Add `model string` and `reasoningEffort string` fields to `runContext`.
5. Remove `copilotArgs []string` and `defaultModel string` from `envConfig`.
6. Remove `copilotArgs []string` parameter from `buildEnvAndRunContext`.
7. In `buildEnvAndRunContext`: stop assigning `copilotArgs` and `defaultModel`
   to `envConfig`. Resolve task model and reasoning effort defaults and assign
   to `runContext.model` and `runContext.reasoningEffort`.
8. Update `buildDockerArgs` tail to always append `--model run.model` and
   `--reasoning-effort run.reasoningEffort` instead of the conditional
   `hasModelArg` / `resolveDefaultModel` / `copilotArgs` append.
9. Delete `resolveDefaultModel()` and `hasModelArg()`.
10. Update the `Run()` call to `buildEnvAndRunContext` (remove `copilotArgs`
    argument).
11. Update `cmd/mato/main.go` `resolveRunOptions` to populate the new
    `RunOptions` fields instead of `DefaultModel`.
12. Update all test references to `RunOptions{DefaultModel: ...}`,
    `buildEnvAndRunContext`, and `buildDockerArgs` across `runner_test.go`,
    `config_test.go`, and `main_test.go`.
13. Remove passthrough-focused tests (`TestBuildDockerArgs_CopilotArgsPassthrough`,
    `TestResolveDefaultModel`, `TestHasModelArg*`, `TestBuildDockerArgs_ModelPriority`
    passthrough cases).
14. Add tests verifying `buildDockerArgs` outputs `--model` and
    `--reasoning-effort` from `runContext`.

This step compiles because `RunOptions`, `envConfig`, `buildDockerArgs`,
`buildEnvAndRunContext`, `resolveDefaultModel`, `hasModelArg`,
`defaultCopilotModel`, and all their callers (`Run`, `resolveRunOptions`) all
update together in one atomic step.

### Step 3: Update `Run()` signature, poll loop, and review plumbing

**Files:** `internal/runner/runner.go`, `internal/runner/runner_test.go`,
`internal/runner/review.go`, `internal/runner/review_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Depends on:** Step 2

1. Remove `copilotArgs []string` from `Run()` signature.
2. Update `Run()` to pass `opts` (full `RunOptions`) to `pollLoop` instead of
   `opts.RetryCooldown`.
3. Update `pollLoop` signature: replace `cooldown time.Duration` with
   `opts RunOptions`. Extract `cooldown` from `opts.RetryCooldown` internally.
4. Update `pollIterate` signature: add `opts RunOptions` parameter.
5. Update `pollReview` (defined in `runner.go`) and its function variable
   `pollReviewFn` to accept `opts RunOptions`. Forward `opts` to `runReview`.
6. Update `runReview` (defined in `review.go`) to accept `opts RunOptions`.
   Before calling `buildDockerArgs`, override `run.model` and
   `run.reasoningEffort` with review-specific values from `opts`
   (falling back to `defaultReviewModel` / `defaultReasoningEffort` for empty).
7. Update `cmd/mato/main.go`: remove `copilotArgs` from `runFn` call.
8. Update `runFn` test hook type.
9. Update **all** `Run`, `pollLoop`, `pollIterate`, and `pollReviewFn`
   callsites and stubs in `internal/runner/runner_test.go` to match the new
   signatures. This includes every direct `pollIterate(...)` call (there are
   ~7 across the `TestPollIterate_*` tests), every `pollReviewFn` stub
   assignment, and the `pollLoop` and `Run` calls in
   `TestPollLoop_ExitsOnCancelledContext` and any test that calls `Run`
   directly.
10. Update **all** `runFn` stub signatures in `cmd/mato/main_test.go` (every
    `TestConfigFile_*` test and any other test that assigns to `runFn`).
11. Update `runReview` call sites and test stubs in
    `internal/runner/review_test.go`.

This step compiles because `Run`, `pollLoop`, `pollIterate`, `pollReview`
(in `runner.go`), `runReview` (in `review.go`), and the caller
(`cmd/mato/main.go`) all update together in one atomic step.

### Step 4: Update dry-run to display resolved settings

**Files:** `internal/runner/runner.go`, `internal/runner/runner_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Depends on:** Steps 2, 3

1. Change `DryRun` signature to accept `RunOptions`.
2. Add resolved settings section to dry-run output (apply defaults for display).
3. Update `dryRunFn` test hook type in `cmd/mato/main.go`.
4. Update `cmd/mato/main.go` to pass `RunOptions` to `dryRunFn`.
5. Update all `dryRunFn` test stubs in `main_test.go`.
6. Update **all** `DryRun(...)` callsites in `internal/runner/runner_test.go`
   to pass `RunOptions{}` as the new third argument. There are ~17 callsites
   across `TestDryRun_BasicValidation`, `TestDryRun_DetectsParseErrors`,
   `TestDryRun_DetectsOverlaps`, `TestDryRun_MissingDirectories`,
   `TestDryRun_ReportsPromotableDependencies`, `TestDryRun_NoDockerLaunched`,
   and the remaining `TestDryRun_*` tests.
7. Add test verifying dry-run output includes the "Resolved Settings" section
   with effective model and reasoning effort values.

### Step 5: Introduce `mato run` and restructure root command

**Files:** `cmd/mato/main.go`, `cmd/mato/main_test.go`

**Depends on:** Steps 1–4

This is the largest step. Removing `DisableFlagParsing`, `extractKnownFlags`,
and the root `RunE` invalidates many existing tests beyond just
`TestExtractKnownFlags*`. All affected tests must be deleted, rewritten, or have
their invocations updated in this step to keep the suite green.

**Production code changes:**

1. Create `newRunCmd()` returning a `*cobra.Command` with `Use: "run"`.
2. Move the current root `RunE` logic into `newRunCmd()`.
3. Remove `DisableFlagParsing: true` from root.
4. Remove `extractKnownFlags()` and `assignFlag()` helper.
5. Remove `runConfig` struct.
6. Register `--repo` as `root.PersistentFlags().StringVar(...)`.
7. Set `root.Version = version` and configure `root.SetVersionTemplate(...)` for
   `mato --version` output. Remove the manual `--version` flag parsing.
8. Register `--branch`, `--dry-run`, `--task-model`, `--review-model`,
   `--task-reasoning-effort`, `--review-reasoning-effort` on the run command.
9. Remove root `RunE` entirely so bare `mato` shows help.
10. Update root `Use`, `Short`, `Long`, and `Example` strings (remove forwarding
    examples like `mato --model gpt-5.4`).
11. Add `root.AddCommand(newRunCmd())`.
12. Wire CLI flag values from `newRunCmd()` into `resolveRunOptions`. Step 5
    owns the initial `resolveRunOptions` call site wiring; Step 7 adds the
    `resolveStringOption` helper and full precedence logic.
13. Add `validateReasoningEffort()` function.

**Test changes — delete entirely:**

14. Remove `TestExtractKnownFlags` (line 98) — tests the deleted parser.
15. Remove `TestExtractKnownFlags_MissingValue` (line 244) — same.
16. Remove `TestExtractKnownFlags_Version` (line 330) — same.
17. Remove `TestRootCmd_HelpAfterDoubleDashForwarded` (line 385) — forwarding
    no longer exists.
18. Remove `TestRootCmd_UnknownFlagsForwarded` (line 499) — forwarding no
    longer exists.

**Test changes — rewrite assertions:**

19. Update `TestRootCmd_HelpListsCompletionCommand` (line 519) — remove the
    assertion checking for `"mato --model gpt-5.4"`, replace with an assertion
    for the new `run` subcommand in help output.
20. Rewrite `TestRootCmd_InvalidBranchRejected` (line 1541) — stop using
    `extractKnownFlags`; use `mato run --branch=foo..bar` instead.
21. Rewrite `TestRootCmd_NonRepoPathRejected` (line 1572) — stop using
    `extractKnownFlags`; use `mato run --repo=<dir>` instead.

**Test changes — update invocations from bare root to `mato run`:**

All tests that invoke `newRootCmd().SetArgs([]string{"--repo", ...})` expecting
the run path must change to `SetArgs([]string{"run", "--repo", ...})`:

22. `TestConfigFile_BranchFromConfig` (line 2316)
23. `TestConfigFile_BranchFromEnv` (line 2346)
24. `TestConfigFile_BranchFlagOverridesConfig` (line 2367)
25. `TestConfigFile_BranchEnvOverridesConfig` (line 2389)
26. `TestConfigFile_MissingConfig` (line 2411)
27. `TestConfigFile_InvalidYAML` (line 2435)
28. `TestConfigFile_InvalidAgentTimeout_RunMode` (line 2451)
29. `TestConfigFile_InvalidTimeout_EnvOverride` (line 2465)
30. `TestConfigFile_InvalidCooldown_EnvOverride` (line 2488)
31. `TestConfigFile_RunOptionsFromConfig` (line 2512) — also update
    `opts.DefaultModel` assertion to use `opts.TaskModel` / `opts.ReviewModel`.
32. `TestConfigFile_DryRunUsesConfigBranch` (line 2539) — update invocation
    to `run --repo ... --dry-run` and update `dryRunFn` stub signature.
33. `TestConfigFile_WhitespaceEnvBranchRejected` (line 2558) — update to
    `run --repo ...`.

**New tests to add:**

34. Bare `mato` shows help (check stdout for "Available Commands" and "run").
35. `mato run` invokes `runFn`.
36. `mato run --task-model X` passes correct model to `runFn`.
37. Unknown root flag returns Cobra error (not silently forwarded).
38. `mato --version` prints version via Cobra's built-in handler.

### Step 6: Remove per-subcommand `--repo` declarations

**Files:** `cmd/mato/main.go`, `cmd/mato/main_test.go`

**Depends on:** Step 5

1. Remove local `--repo` variable and `cmd.Flags().StringVar(...)` from each
   subcommand: `status`, `doctor`, `graph`, `init`, `inspect`, `retry`,
   `cancel`, `pause`, `resume`.
2. Update each subcommand's `RunE` to read the persistent `repoFlag` variable
   (accessible via closure from `newRootCmd`).
3. Add tests for `mato --repo /path status` and `mato status --repo /path`.

### Step 7: Implement `resolveStringOption` and full precedence logic

**Files:** `cmd/mato/main.go`, `cmd/mato/main_test.go`

**Depends on:** Step 5

Step 5 wires CLI flag values into `resolveRunOptions` and updates the call site.
This step adds the `resolveStringOption` helper and implements the full
CLI > env > config > default precedence chain for the four new settings. The
separation keeps Step 5 focused on the structural CLI refactor while this step
adds the layered resolution logic and its tests.

1. Implement `resolveStringOption(flag, envKey, configVal)` helper with
   `strings.TrimSpace` on all surfaces.
2. Update `resolveRunOptions` to use `resolveStringOption` for `task_model`,
   `review_model`, `task_reasoning_effort`, `review_reasoning_effort`.
3. Add comprehensive precedence tests for all four settings (CLI beats env,
   env beats config, config beats default, empty/whitespace treated as unset).
4. Add reasoning effort validation tests (valid: `low|medium|high|xhigh`,
   invalid: everything else).

### Step 8: Update integration tests and helpers

**Files:** `internal/integration/config_test.go`

**Depends on:** Steps 5–7

1. Update `filteredHostEnv` (line ~14) to exclude
   `MATO_TASK_MODEL`, `MATO_REVIEW_MODEL`, `MATO_TASK_REASONING_EFFORT`,
   `MATO_REVIEW_REASONING_EFFORT` (and remove `MATO_DEFAULT_MODEL`).
2. Update `TestConfigFile_EndToEnd` (line 61) — change CLI invocation from
   bare `mato --repo ... --dry-run` to `mato run --repo ... --dry-run`.
3. Update `TestConfigFile_DryRunInvalidBranchFromConfig` (line 74) — same
   invocation update.
4. Add integration test verifying `.mato.yaml` with `default_model` is rejected.
5. Add integration test verifying dry-run output includes resolved settings.

### Step 9: Update documentation

**Files:** `README.md`, `docs/configuration.md`, `docs/architecture.md`,
`Makefile`

**Depends on:** Steps 5–8

1. Update CLI usage block in `docs/configuration.md` and `README.md` to show
   `mato run` with new flags.
2. Update precedence table to replace `default_model` row with `task_model`,
   `review_model`, `task_reasoning_effort`, `review_reasoning_effort` rows.
3. Update `.mato.yaml` example to use new keys.
4. Update all `mato --model gpt-5.4` examples to `mato run --review-model
   gpt-5.4` (or equivalent).
5. Remove passthrough documentation (`--` separator, unknown flag forwarding).
6. Add migration section noting removal of `default_model`,
   `MATO_DEFAULT_MODEL`, and arg passthrough.
7. Update `docs/architecture.md`:
   - Change `runner.Run(repoRoot, branch, copilotArgs, opts)` signature
     references to `runner.Run(repoRoot, branch, opts)`.
   - Remove `copilotArgs` from the Docker command construction description.
   - Replace `MATO_DEFAULT_MODEL` references with the new settings.
8. Update `Makefile` `run` target: replace `COPILOT_ARGS` with
   `go run ... ./cmd/mato run --repo "$(REPO)"`. The new env vars
   (`MATO_TASK_MODEL`, etc.) provide the override surface instead of
   Makefile-level variables.
9. Note in implemented proposals that reference `default_model` or passthrough
   (e.g., `docs/proposals/implemented/add-config-file.md`) are historical
   snapshots and do not need updating, since they describe the design at the
   time of implementation.

## 5. File Changes

| File | Action | Summary |
|------|--------|---------|
| `internal/config/config.go` | Modify | Replace `DefaultModel` with 4 new fields, update `normalize()` |
| `internal/config/config_test.go` | Modify | Replace `default_model` tests with new field tests |
| `internal/runner/runner.go` | Modify | Update `RunOptions`, `runContext`, `Run()`, `buildEnvAndRunContext`, `DryRun`, `pollLoop`, `pollIterate` |
| `internal/runner/runner_test.go` | Modify | Update tests for changed signatures and remove passthrough tests |
| `internal/runner/config.go` | Modify | Remove `copilotArgs`/`defaultModel` from `envConfig`, update `buildDockerArgs`, delete helpers |
| `internal/runner/config_test.go` | Modify | Replace passthrough/model tests with new model/reasoning tests |
| `internal/runner/review.go` | Modify | Accept `RunOptions` and apply review-specific model/reasoning settings |
| `internal/runner/review_test.go` | Modify | Update review tests for new parameter |
| `cmd/mato/main.go` | Modify | Restructure root command, add `newRunCmd`, persistent `--repo`, new resolution logic |
| `cmd/mato/main_test.go` | Modify | Replace extractKnownFlags tests, add run command tests, update stubs |
| `internal/integration/config_test.go` | Modify | Update `filteredHostEnv` for new env vars, update CLI invocations |
| `README.md` | Modify | Update CLI examples, config examples, remove passthrough docs |
| `docs/configuration.md` | Modify | Update CLI usage, precedence table, config file format, env vars |
| `docs/architecture.md` | Modify | Update `Run()` signature, Docker command construction, remove passthrough refs |
| `Makefile` | Modify | Update `run` target to remove `COPILOT_ARGS` |

## 6. Error Handling

| Scenario | Behavior |
|----------|----------|
| Unknown root flag (e.g., `mato --foo`) | Standard Cobra error: `Error: unknown flag: --foo` + root usage |
| Unknown run flag (e.g., `mato run --foo`) | Standard Cobra error + run usage (via shared `SetFlagErrorFunc` in `configureCommand`) |
| Invalid reasoning effort | `invalid reasoning effort "bad": must be one of low, medium, high, xhigh` |
| `default_model` in `.mato.yaml` | YAML parse error: unknown field (from `dec.KnownFields(true)`) |
| `MATO_DEFAULT_MODEL` set | Silently ignored (env lookup removed); document in migration notes |
| Bare `mato` with no subcommand | Shows help and subcommand list |
| `mato run` with no repo | Falls back to `os.Getwd()` (existing behavior) |
| Empty/whitespace model from env/config | Treated as unset, hardcoded default applies |

## 7. Testing Strategy

### Unit Tests

**`internal/config/config_test.go`:**
- Parse YAML with `task_model`, `review_model`, `task_reasoning_effort`,
  `review_reasoning_effort`.
- Verify `default_model` causes unknown-field error.
- Verify normalization (whitespace trimming, nil for empty values).

**`internal/runner/config_test.go`:**
- `buildDockerArgs` always appends `--model` and `--reasoning-effort` from
  `runContext`.
- No `copilotArgs` passthrough tests (removed).
- Verify model and reasoning effort appear in correct position in Docker args.

**`internal/runner/runner_test.go`:**
- `buildEnvAndRunContext` sets task defaults on `runContext`.
- `RunOptions` field mapping is correct.
- Default constants have expected values.
- `DryRun` prints resolved settings section.

**`cmd/mato/main_test.go`:**
- `resolveRunOptions` precedence: CLI > env > config > default for all four
  settings.
- `validateReasoningEffort` accepts `low|medium|high|xhigh`, rejects others.
- `resolveStringOption` precedence logic.
- Bare `mato` returns help (check stdout for "Available Commands").
- `mato run` invokes `runFn`.
- `mato run --task-model X` passes correct model to `runFn`.
- `mato run --dry-run` invokes `dryRunFn` with settings.
- Unknown root flag returns Cobra error.
- `mato --repo /path status` and `mato status --repo /path` both work.
- Version flag (`mato --version`) still works.

### Integration Tests

**`internal/integration/config_test.go`:**
- Update `filteredHostEnv` to exclude `MATO_TASK_MODEL`, `MATO_REVIEW_MODEL`,
  `MATO_TASK_REASONING_EFFORT`, `MATO_REVIEW_REASONING_EFFORT` and remove
  `MATO_DEFAULT_MODEL`.
- Update CLI invocations from bare `mato --repo ... --dry-run` to
  `mato run --repo ... --dry-run` (note: `--repo` is now persistent and works
  before or after `run`).
- Verify `mato run --dry-run` output includes resolved settings section.
- Verify `.mato.yaml` with `default_model` fails with unknown-field error.

**Other integration test files:**
- Update any test that invokes `mato` with the old root run mode to use
  `mato run` instead.

### Prompt Validation

No changes to `task-instructions.md` or `review-instructions.md`, so no prompt
validation tests needed.

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| `pollLoop`/`pollIterate` signature changes are large | Passing `RunOptions` as a single value struct minimizes diff; all fields are read-only throughout the loop |
| Tests tightly coupled to `extractKnownFlags` | Delete and replace; the new Cobra-native flag parsing is simpler and needs fewer tests |
| `MATO_DEFAULT_MODEL` silently ignored | Document in migration notes; unlike `default_model` in YAML (which errors), env vars are just not read |
| Review model/reasoning threading through poll loop | Clean value-struct plumbing through 2-3 function signatures; no concurrency concerns since poll loop is single-threaded |
| Copilot CLI changes `--reasoning-effort` accepted values | Validation mirrors current `copilot --help`; documented as intentional sync point |

## 9. Open Questions

None. All design decisions are resolved in this plan.
