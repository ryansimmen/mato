# Explicit Run Command and Copilot Settings — Implementation Plan

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
- `DisableFlagParsing` and the manual `extractKnownFlags` parser are removed,
  restoring standard Cobra flag handling.

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
- Updating docs, tests, and Makefile.

### Out of scope

- Generic `task_copilot_args` or `review_copilot_args`.
- Changes to `doctor`, `inspect`, or `status` to display resolved model
  settings.
- Adding a shared `--model` convenience flag.
- Support for future Copilot CLI flags beyond model and reasoning effort.

## 3. Design

### 3.1 Root command behavior

The root command gets these changes:

- Remove `DisableFlagParsing: true` (currently at `cmd/mato/main.go:406`).
- Remove the current orchestrator `RunE` (currently at
  `cmd/mato/main.go:407-461`).
- Set `Use` to `"mato"`.
- Update `Long` to a brief description without passthrough references.
- Update `Example` to show `mato run` instead of bare `mato`.
- Use Cobra's built-in version support: set `root.Version = version` and
  configure `root.SetVersionTemplate(...)`. This gives `mato --version`
  and `mato -v` support (Cobra adds `-v` as a shorthand automatically
  when it is not already taken; see `command.go:1251`). The existing
  `newVersionCmd()` is kept so that `mato version` also continues to work.
- Add `Args: usageNoArgs` — keeps the root command explicit about accepting
  no positional arguments when Cobra resolves execution to root.
- Set `RunE` to a minimal function that shows help:
  ```go
  Args: usageNoArgs,
  RunE: func(cmd *cobra.Command, args []string) error {
      return cmd.Help()
  },
  ```
  `RunE` is required so bare `mato` is runnable and can show help instead
  of short-circuiting through Cobra's non-runnable help path. `Args`
  remains useful for true root-command positional-arg validation, but the
  verified behavior with the Cobra version used by this repo is that
  unresolved tokens such as `mato foo` and post-`--` tokens such as
  `mato -- --model gpt-5.4` still surface as unknown-command errors during
  command resolution rather than as `usageNoArgs` failures.
- Remove the documentation-only flag definitions for `--branch`, `--dry-run`
  on root (these move to `mato run`).

Result: `mato` → help (exit 0), `mato --version` / `mato -v` →
version (exit 0), `mato --help` → help (exit 0),
`mato run` → orchestrator,
`mato foo` → `unknown command "foo" for "mato"`,
`mato -- --model gpt-5.4` → `unknown command "--model" for "mato"`,
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
  6. Call `resolveRunOptions(flags, fileCfg)` — returns fully-resolved
     `RunOptions`.
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

This is a **behavior change** from the current CLI. Today, `extractKnownFlags`
trims whitespace from `--repo` and `--branch` values and rejects
whitespace-only values early with `"flag %s requires a value"` errors
(cmd/mato/main.go:190-193 for `=` form, lines 212-214 for space-separated
form).

Under Cobra-based parsing, Cobra delivers raw flag values without trimming.
For `--repo`, whitespace-only values reach `resolveRepo`, which returns the
raw string, then `validateRepoPath` fails with "does not exist." For
`--branch`, whitespace-only values reach `validateBranch`, which delegates
to `git check-ref-format --branch` and fails with "invalid branch name."

The error messages differ from today's `"flag %s requires a value"`, but
the result is the same: whitespace-only values are rejected before any
work begins. No special whitespace rejection is added; the downstream
validation is sufficient.

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
defaults for Docker image and agent timeout as it does today. Retry
cooldown defaults are applied downstream in `internal/queue/claim.go`.)

A `resolveStringOption` helper keeps the four resolution chains DRY:

```go
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

The `resolveRunOptions` function uses this helper for each setting, then
applies hardcoded defaults for any still-empty values:

```go
func resolveRunOptions(flags runFlags, cfg config.Config) (runner.RunOptions, error)
```

For each of the four settings, resolution follows:

1. CLI flag value — `strings.TrimSpace`; if empty after trim, fall through.
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

These are **new product decisions**. The current codebase has a single
model default (`defaultCopilotModel = "claude-opus-4.6"` in
`internal/runner/runner.go:40`) used for both task and review agents, and
no reasoning-effort concept. The values below are deliberate choices for
the new split-model design:

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

This approach avoids threading `RunOptions` through `pollLoop` and
`pollIterate`. The review-specific values live on the immutable `envConfig`
and are copied to `runContext` only at the review call site.

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

Since `resolveRunOptions` already fully resolved all four values, no
defaults are applied here — values are assigned directly from `opts`.

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
var validReasoningEfforts = map[string]bool{
    "low": true, "medium": true, "high": true, "xhigh": true,
}

func validateReasoningEffort(value, flagName string) error {
    if !validReasoningEfforts[value] {
        return fmt.Errorf("invalid %s %q: must be one of low, medium, high, xhigh", flagName, value)
    }
    return nil
}
```

Model names are validated only as non-empty after trimming. Actual model
availability is enforced by Copilot at runtime. Since defaults are
hardcoded, empty model values only occur if a user explicitly sets an
empty/whitespace override, which `normalizeString` in config already handles
and `strings.TrimSpace` handles for env/CLI values.

### 3.14 task.go impact

No changes needed. `runOnce` calls
`buildDockerArgs(env, run, extraEnvs, nil)` — `run` already carries task
model/effort.

### 3.15 runFn and dryRunFn test hook updates

The package-level test hook variables change signatures:

```go
// Old
var runFn = runner.Run        // func(string, string, []string, RunOptions) error
var dryRunFn = runner.DryRun  // func(string, string) error

// New
var runFn = runner.Run        // func(string, string, RunOptions) error
var dryRunFn = runner.DryRun  // func(string, string, RunOptions) error
```

All test call sites that inject stubs into `runFn` and `dryRunFn` are
updated.

## 4. Step-by-Step Breakdown

Steps are ordered so that each step compiles and passes tests after
completion. Cross-API changes that span caller and callee are grouped in the
same step to avoid intermediate breakage. Dependencies are noted.

**Note:** These steps are planning milestones within a single feature
branch, not independently landable commits. Each step should compile and
pass tests, but the feature is shipped as one cohesive PR.

### Step 1: Update `Config` struct

**Files:** `internal/config/config.go`, `internal/config/config_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Dependencies:** None

1. Remove `DefaultModel *string` from `Config`.
2. Add `TaskModel`, `ReviewModel`, `TaskReasoningEffort`,
   `ReviewReasoningEffort` as `*string` fields with appropriate YAML tags.
3. Update `normalize()` to handle the four new fields.
4. Update `resolveRunOptions` in `cmd/mato/main.go` to stop reading
   `cfg.DefaultModel` and stop resolving `MATO_DEFAULT_MODEL`. The four new
   config fields exist on `Config` after item 2, but `resolveRunOptions`
   does not populate `RunOptions` with them yet — that happens in Step 2
   when `RunOptions` itself gains the new fields.
5. Update `config_test.go`:
   - Remove tests referencing `default_model`.
   - Add tests for parsing the four new keys.
   - Add test that `default_model` in YAML causes a parse error (unknown
     field).
   - Verify whitespace/empty normalization for all four fields.
6. Update `cmd/mato/main_test.go`:
   - Fix any `resolveRunOptions` test that references `DefaultModel` or
     `MATO_DEFAULT_MODEL`.

This step compiles because both the struct change and its callers are
updated together.

### Step 2: Update `RunOptions`, runner internals, and `buildDockerArgs`

**Files:** `internal/runner/runner.go`, `internal/runner/runner_test.go`,
`internal/runner/config.go`, `internal/runner/config_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Dependencies:** Step 1

This step is atomic to avoid a compile gap: removing `DefaultModel` from
`RunOptions` and `defaultCopilotModel` from `config.go` must happen in the
same step as updating `buildEnvAndRunContext` and `buildDockerArgs` (which
read them).

1. Remove `DefaultModel string` from `RunOptions`.
2. Add `TaskModel`, `ReviewModel`, `TaskReasoningEffort`,
   `ReviewReasoningEffort` as `string` fields on `RunOptions`.
3. Replace `defaultCopilotModel` constant with exported
   `DefaultTaskModel`, `DefaultReviewModel`, `DefaultReasoningEffort`.
4. Add `model string` and `reasoningEffort string` fields to `runContext`.
5. Remove `copilotArgs []string` and `defaultModel string` from `envConfig`.
6. Add `reviewModel string` and `reviewReasoningEffort string` to
   `envConfig`.
7. Remove `copilotArgs []string` parameter from `buildEnvAndRunContext`.
8. In `buildEnvAndRunContext`: stop assigning `copilotArgs` and
   `defaultModel` to `envConfig`. Set `env.reviewModel = opts.ReviewModel`,
   `env.reviewReasoningEffort = opts.ReviewReasoningEffort`,
   `run.model = opts.TaskModel`,
   `run.reasoningEffort = opts.TaskReasoningEffort`.
9. Update `buildDockerArgs` tail to always append
   `--model run.model` and `--reasoning-effort run.reasoningEffort` instead
   of the conditional `hasModelArg` / `resolveDefaultModel` / `copilotArgs`
   append.
10. Delete `resolveDefaultModel()` and `hasModelArg()`.
11. Update the `Run()` call to `buildEnvAndRunContext` (remove `copilotArgs`
    argument).
12. Update `cmd/mato/main.go` `resolveRunOptions` to populate the new
    `RunOptions` fields instead of `DefaultModel`.
13. Remove passthrough-focused tests:
    - `TestBuildDockerArgs_CopilotArgsPassthrough` (config_test.go:275)
    - `TestResolveDefaultModel_FallbackToHardcoded` (config_test.go:323)
    - `TestResolveDefaultModel_Configured` (config_test.go:329)
    - `TestHasModelArg_EmptySlice` (config_test.go:335)
    - `TestHasModelArg_ModelWithWhitespace` (config_test.go:344)
    - `TestHasModelArg_ModelEqualsWithValue` (config_test.go:351)
    - `TestHasModelArg_UnrelatedFlags` (config_test.go:357)
14. Remove or rewrite runner tests that depend on `DefaultModel`:
    - `TestResolveDefaultModel` (runner_test.go:182) — tests the deleted
      `resolveDefaultModel` function; remove entirely.
    - `TestBuildEnvAndRunContext_DefaultModelOverride`
      (runner_test.go:4186) — uses `RunOptions{DefaultModel: ...}` and
      asserts `env.defaultModel`; rewrite to use `TaskModel`/`ReviewModel`
      and assert `run.model`/`env.reviewModel`.
15. Add tests verifying `buildDockerArgs` outputs `--model` and
    `--reasoning-effort` from `runContext`.
16. Update all existing test setups to use new `envConfig`/`runContext`
    fields.
17. Update `RunOptions{DefaultModel: ...}` references in `main_test.go`.

This step compiles because `RunOptions`, `envConfig`, `buildDockerArgs`,
`buildEnvAndRunContext`, `resolveDefaultModel`, `hasModelArg`,
`defaultCopilotModel`, and all their callers (`Run`, `resolveRunOptions`)
all update together.

### Step 3: Update `Run()` signature and review plumbing

**Files:** `internal/runner/runner.go`, `internal/runner/runner_test.go`,
`internal/runner/review.go`, `internal/runner/review_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Dependencies:** Step 2

1. Remove `copilotArgs []string` from `Run()` signature.
2. Update `runReview` to override `run.model = env.reviewModel` and
   `run.reasoningEffort = env.reviewReasoningEffort` before
   `buildDockerArgs`.
3. Update `cmd/mato/main.go`: remove `copilotArgs` from `runFn` call.
4. Update `runFn` test hook type.
5. Update all `Run` callsites and stubs in `runner_test.go` and
   `main_test.go`.
6. Add test: `runReview` uses `env.reviewModel` and
   `env.reviewReasoningEffort` in Docker args.

This step compiles because `Run` and its callers update together.

### Step 4: Update dry-run to display resolved settings

**Files:** `internal/runner/runner.go`, `internal/runner/runner_test.go`,
`cmd/mato/main.go`, `cmd/mato/main_test.go`

**Dependencies:** Steps 2, 3

1. Change `DryRun` signature to accept `RunOptions`.
2. Add resolved settings section to dry-run output.
3. Update `dryRunFn` test hook type in `cmd/mato/main.go`.
4. Update `cmd/mato/main.go` to pass `RunOptions` to `dryRunFn`.
5. Update all `dryRunFn` test stubs in `main_test.go`.
6. Update all `DryRun(...)` callsites in `internal/runner/runner_test.go`
   to pass `RunOptions{}` as the new third argument. There are ~17
   callsites across `TestDryRun_*` tests (lines 2585–3325).
7. Add test verifying dry-run output includes the "Resolved Settings"
   section with effective model and reasoning effort values.

### Step 5: Introduce `mato run` and restructure root command

**Files:** `cmd/mato/main.go`, `cmd/mato/main_test.go`

**Dependencies:** Steps 1–4

This is the largest step. Removing `DisableFlagParsing`, `extractKnownFlags`,
and the root `RunE` invalidates many existing tests beyond just
`TestExtractKnownFlags*`. All affected tests must be deleted, rewritten, or
have their invocations updated in this step to keep the suite green.

**Production code changes:**

1. Create `newRunCmd()` returning a `*cobra.Command` with `Use: "run"`.
2. Move the current root `RunE` logic into `newRunCmd()`.
3. Remove `DisableFlagParsing: true` from root.
4. Remove `extractKnownFlags()` and `assignFlag()` helper.
5. Remove `runConfig` struct.
6. Register `--repo` as `root.PersistentFlags().StringVar(...)`.
7. Set `root.Version = version` and configure `root.SetVersionTemplate(...)`
   for `mato --version` output. Remove the manual `--version` flag parsing.
   Keep the existing `newVersionCmd()` so that `mato version` continues to
   work.
8. Register `--branch`, `--dry-run`, `--task-model`, `--review-model`,
   `--task-reasoning-effort`, `--review-reasoning-effort` on the run
   command.
9. Set root `Args: usageNoArgs` and `RunE` to a minimal help-only
   function. `RunE` is needed so bare `mato` is runnable and shows help.
   Keep `Args` for explicit no-arg validation when Cobra selects the root
   command, but do not rely on it to rewrite unknown-command cases: the
   verified behavior in this repo is still `unknown command "foo" for
   "mato"` and `unknown command "--model" for "mato"` after `--`.
10. Update root `Use`, `Short`, `Long`, and `Example` strings (remove
    forwarding examples like `mato --model gpt-5.4`).
11. Add `root.AddCommand(newRunCmd())`.
12. Add `validateReasoningEffort()` function.
13. Implement `resolveStringOption` helper and update `resolveRunOptions` to
    use it for all four settings with full CLI > env > config > default
    precedence.

**Test changes — delete entirely:**

14. Remove `TestExtractKnownFlags` (line 98) — tests the deleted parser.
15. Remove `TestExtractKnownFlags_MissingValue` (line 244) — same.
16. Remove `TestExtractKnownFlags_Version` (line 330) — same.
17. Remove `TestRootCmd_HelpAfterDoubleDashForwarded` (line 385) —
    forwarding no longer exists.
18. Remove `TestRootCmd_UnknownFlagsForwarded` (line 499) — forwarding no
    longer exists.

**Test changes — rewrite assertions:**

19. Update `TestRootCmd_HelpListsCompletionCommand` (line 519) — remove
    the assertion checking for `"mato --model gpt-5.4"`, replace with an
    assertion for the new `run` subcommand in help output.
20. Rewrite `TestRootCmd_InvalidBranchRejected` (line 1541) — stop using
    `extractKnownFlags`; use `mato run --branch=foo..bar` instead.
21. Rewrite `TestRootCmd_NonRepoPathRejected` (line 1572) — stop using
    `extractKnownFlags`; use `mato run --repo=<dir>` instead.

**Test changes — update invocations from bare root to `mato run`:**

All tests that invoke `newRootCmd().SetArgs([]string{"--repo", ...})`
expecting the run path must change to
`SetArgs([]string{"run", "--repo", ...})`:

22. `TestConfigFile_BranchFromConfig` (line 2316)
23. `TestConfigFile_BranchFromEnv` (line 2346)
24. `TestConfigFile_BranchFlagOverridesConfig` (line 2367)
25. `TestConfigFile_BranchEnvOverridesConfig` (line 2389)
26. `TestConfigFile_MissingConfig` (line 2411)
27. `TestConfigFile_InvalidYAML` (line 2435)
28. `TestConfigFile_InvalidAgentTimeout_RunMode` (line 2450)
29. `TestConfigFile_InvalidTimeout_EnvOverride` (line 2465)
30. `TestConfigFile_InvalidCooldown_EnvOverride` (line 2487)
31. `TestConfigFile_RunOptionsFromConfig` (line 2509) — also update
    `opts.DefaultModel` assertion to use `opts.TaskModel` /
    `opts.ReviewModel`.
32. `TestConfigFile_DryRunUsesConfigBranch` (line 2536) — update invocation
    to `run --repo ... --dry-run` and update `dryRunFn` stub signature.
33. `TestConfigFile_WhitespaceEnvBranchRejected` (line 2558) — update to
    `run --repo ...`.

**New tests to add:**

34. Bare `mato` shows help (check stdout for "Available Commands" and
    "run").
35. `mato run` invokes `runFn`.
36. `mato run --task-model X` passes correct model to `runFn`.
37. Unknown root flag returns Cobra error (not silently forwarded).
38. `mato --version` prints version via Cobra's built-in handler.
39. `mato -v` prints version (Cobra auto-adds `-v` shorthand).
40. `mato version` prints version via the existing version subcommand.
41. Root unknown subcommand rejected (`mato foo` → `unknown command "foo"
    for "mato"`).
42. Root `--` passthrough rejected (`mato -- --model gpt-5.4` →
    `unknown command "--model" for "mato"`).
43. Run `--` passthrough rejected (`mato run -- --model gpt-5.4` →
    `unknown command "--model" for "mato run"`).
44. Table-driven precedence tests (CLI > env > config > default) for all
    four settings.
45. Whitespace-only env values treated as unset.
46. `validateReasoningEffort` accepts valid values, rejects invalid.
47. `mato run --dry-run` calls `dryRunFn` with resolved opts.
48. `default_model` in config causes error.
49. Persistent `--repo` in multiple positions.

### Step 6: Remove per-subcommand `--repo` declarations

**Files:** `cmd/mato/main.go`, `cmd/mato/main_test.go`

**Dependencies:** Step 5

1. Remove local `--repo` variable and `cmd.Flags().StringVar(...)` from
   each subcommand: `status`, `doctor`, `graph`, `init`, `inspect`,
   `retry`, `cancel`, `pause`, `resume`.
2. Update each subcommand's `RunE` to read the persistent `repoFlag`
   variable (accessible via closure from `newRootCmd`).
3. Add tests for `mato --repo /path status` and
   `mato status --repo /path`.

### Step 7: Update integration tests

**Files:** `internal/integration/config_test.go`,
`internal/integration/init_test.go`

**Dependencies:** Steps 1–6

1. Update `filteredHostEnv` to exclude `MATO_TASK_MODEL`,
   `MATO_REVIEW_MODEL`, `MATO_TASK_REASONING_EFFORT`,
   `MATO_REVIEW_REASONING_EFFORT` (and remove `MATO_DEFAULT_MODEL`).
2. Update `TestConfigFile_EndToEnd` (line 61) — change CLI invocation from
   bare `mato --repo ...` to `mato run --repo ...`.
3. Update `TestConfigFile_DryRunInvalidBranchFromConfig` (line 74) — same.
4. Update `internal/integration/init_test.go:60`: Update `runner.DryRun`
   call to new signature with `RunOptions`.
5. Add integration test verifying `.mato.yaml` with `default_model` is
   rejected.
6. Add integration test verifying dry-run output includes resolved settings.

### Step 8: Update documentation and Makefile

**Files:** `README.md`, `docs/configuration.md`, `docs/architecture.md`,
`Makefile`

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

**`Makefile`:**

- Update `run` target: replace `COPILOT_ARGS` with
  `go run ... ./cmd/mato run --repo "$(REPO)"`. The new env vars
  (`MATO_TASK_MODEL`, etc.) provide the override surface instead of
  Makefile-level variables.

**Implemented proposals** (`docs/proposals/implemented/add-config-file.md`,
`docs/proposals/implemented/mato-init.md`, etc.) are historical snapshots
describing the design at the time of implementation. They do not need
updating.

### Step 9: Verify

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
| `cmd/mato/main.go` | Modify | Major: add `newRunCmd`, remove passthrough, promote `--repo`, add validation, `resolveStringOption` |
| `cmd/mato/main_test.go` | Modify | Major: remove passthrough tests, add run/precedence/validation/positional tests |
| `internal/integration/config_test.go` | Modify | Update env filter, add `run` subcommand |
| `internal/integration/init_test.go` | Modify | Update `DryRun` call signature |
| `README.md` | Modify | Update CLI examples, remove passthrough docs |
| `docs/configuration.md` | Modify | Update root usage, add `mato run`, update config/precedence/flags |
| `docs/architecture.md` | Modify | Update Run signature, model resolution, RunOptions |
| `Makefile` | Modify | Update `run` target, remove `COPILOT_ARGS` |

## 6. Error Handling

- **Bare `mato`**: no positional args; `RunE` calls `cmd.Help()`, exit 0.
- **`mato foo`**: verified Cobra behavior is `unknown command "foo" for
  "mato"`.
- **`mato -- --model gpt-5.4`**: verified Cobra behavior is
  `unknown command "--model" for "mato"`.
- **`mato --model gpt-5.4`**: Unknown flag → usage error.
- **`mato -v`**: Cobra's auto-added `-v` shorthand → version (exit 0).
- **`mato run -- --model gpt-5.4`**: verified Cobra behavior is
  `unknown command "--model" for "mato run"`.
- **Unknown `mato run` flags**: Cobra rejects → `FlagErrorFunc` wraps with
  `UsageError`.
- **Invalid reasoning effort**: `validateReasoningEffort` returns error
  listing `low`, `medium`, `high`, `xhigh`.
- **`default_model` in `.mato.yaml`**: `KnownFields(true)` rejects with
  parse error.
- **`MATO_DEFAULT_MODEL` set**: Silently ignored (env lookup removed);
  document in migration notes.
- **Whitespace-only `--repo`**: Reaches `validateRepoPath`, fails with
  "does not exist" (behavior change: today's `extractKnownFlags` rejects
  with "flag --repo requires a value").
- **Whitespace-only `--branch`**: Reaches `validateBranch` →
  `git check-ref-format` rejects (behavior change: today's
  `extractKnownFlags` rejects with "flag --branch requires a value").
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

- `TestResolveDefaultModel` (line 182) removed (function deleted).
- `TestBuildEnvAndRunContext_DefaultModelOverride` (line 4186) rewritten to
  verify `TaskModel`/`ReviewModel` propagation.
- `buildEnvAndRunContext` populates `run.model`/`run.reasoningEffort` from
  opts.
- `buildEnvAndRunContext` populates
  `env.reviewModel`/`env.reviewReasoningEffort` from opts.
- `buildEnvAndRunContext` no longer takes `copilotArgs`.
- `DryRun` prints resolved settings, accepts `RunOptions`.
- Default constants have expected values.

### Unit tests (`internal/runner/review_test.go`)

- `runReview` uses `env.reviewModel`/`env.reviewReasoningEffort` in Docker
  args.

### Unit tests (`cmd/mato/main_test.go`)

- Bare `mato` shows help, exit 0.
- `mato --version` prints version.
- `mato -v` prints version (Cobra auto-shorthand).
- `mato version` prints version via version subcommand.
- `mato run` triggers orchestrator.
- Four new flags pass correct values.
- `validateReasoningEffort` tests (valid and invalid).
- `resolveStringOption` precedence logic.
- Precedence: CLI > env > config > default (table-driven).
- Whitespace-only env values treated as unset.
- Unknown root flags rejected.
- Root unknown subcommand rejected (`mato foo` → `unknown command "foo"
  for "mato"`).
- Root `--` passthrough rejected (`mato -- --model gpt-5.4` →
  `unknown command "--model" for "mato"`).
- Run `--` passthrough rejected (`mato run -- --model gpt-5.4` →
  `unknown command "--model" for "mato run"`).
- Persistent `--repo` in multiple positions.
- `mato run --dry-run` calls `dryRunFn` with opts.
- `default_model` in config causes error.

### Integration tests

- `config_test.go`: updated env filter, `run` subcommand.
- `init_test.go`: updated `DryRun` signature.
- Dry-run output includes resolved settings section.
- `.mato.yaml` with `default_model` fails with unknown-field error.

### Prompt validation

No changes to `task-instructions.md` or `review-instructions.md`, so no
prompt validation tests needed.

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Persistent `--repo` conflicts with per-subcommand `--repo` | Remove all per-subcommand definitions in Step 6 |
| Breaking: `mato` no longer starts orchestrator | Acceptable per ideas doc; pre-adoption project |
| Signature changes break call sites | All updated simultaneously within atomic steps |
| `mato version --repo /path` harmless no-op | Consistent with Cobra persistent flag semantics |
| Reasoning-effort values may change | Easy to update accepted set; documented as intentional sync point with Copilot CLI |
| `runReview` modifies `runContext` | Go value semantics prevent caller modification |
| Tests tightly coupled to `extractKnownFlags` | Delete and replace; Cobra-native flag parsing is simpler and needs fewer tests |
| `MATO_DEFAULT_MODEL` silently ignored after removal | Document in migration notes; unlike `default_model` in YAML (which errors), env vars are just not read |
| Step 5 is large and test-heavy | Detailed test-by-test change list with line numbers ensures nothing is missed |

## 9. Open Questions

None — all design decisions are resolved.
