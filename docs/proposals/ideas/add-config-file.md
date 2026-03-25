---
id: add-config-file
priority: 30
affects:
  - internal/config/config.go
  - internal/config/config_test.go
  - internal/runner/runner.go
  - internal/runner/config.go
  - internal/queue/claim.go
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - docs/configuration.md
  - README.md
  - AGENTS.md
tags: [feature, configuration, ux]
estimated_complexity: large
---
# Mato Config File — Implementation Plan

## 1. Goal

Add support for an optional YAML configuration file (`.mato.yaml`) that provides
persistent, per-repository settings for mato. This complements the existing CLI
flags and environment variables, giving users a committed (or gitignored) way to
configure default behavior without repetitive command-line arguments or shell
profile modifications.

## 2. Scope

### In Scope

- New `internal/config/` package for config file parsing.
- Config file format definition (YAML, using existing `gopkg.in/yaml.v3`).
- Config file loaded from the repo root (next to `.git/`).
- Precedence chain: CLI flag > environment variable > config file > hardcoded
  default.
- Integration with the root command and all subcommands.
- Integration with runner settings (docker image, default model, agent timeout,
  retry cooldown).
- `RunOptions` struct in runner for resolved configuration values.
- Test hooks (`runFn`, `dryRunFn`) for command-level testing.
- Unit tests, command-level tests, and integration tests.
- Documentation updates: `docs/configuration.md`, `README.md`, `AGENTS.md`.

### Out of Scope

- Global/user-level config file (`~/.config/mato/config.yaml`). Follow-up.
- Config file scaffolding command (e.g., `mato config init`). Follow-up.
- Doctor config validation check category. Follow-up.
- Viper or other config library.
- Task frontmatter format changes.
- Implementing the proposed-but-unimplemented `MATO_BRANCH` env var.
- Fixing runner's hardcoded `/.tasks/` gitignore pattern to use actual tasks
  directory. Pre-existing limitation; `mato init` already handles custom tasks
  dirs correctly (`internal/setup/setup.go:200-212`).

## 3. Design

### Config File Format

**File name**: `.mato.yaml` (no `.yml` alias)
**Location**: repository root (same directory as `.git/`)

```yaml
# .mato.yaml — mato configuration file
# All fields are optional. CLI flags and environment variables take precedence.

# Target branch for merge processing (default: "mato")
branch: main

# Path to the tasks directory (default: <repo>/.tasks)
# Relative paths are resolved against the repo root.
tasks_dir: .tasks

# Docker image for agent containers (default: ubuntu:24.04)
docker_image: ubuntu:24.04

# Default Copilot model (default: claude-opus-4.6)
default_model: claude-sonnet-4

# Maximum wall-clock time per agent run (default: 30m)
# Accepts Go duration strings. Must be positive.
agent_timeout: 45m

# Minimum wait after task failure before retry (default: 2m)
# Accepts Go duration strings. Must be positive.
retry_cooldown: 5m
```

All fields are optional. Unknown YAML keys are silently ignored
(`gopkg.in/yaml.v3` default `Unmarshal` behavior). This is an intentional
forward-compatibility choice: a newer mato version can add fields without
breaking users who run an older version. The tradeoff is that key-name typos
(e.g., `agent_timout`) are not caught. This is documented.

### Config Struct

```go
package config

// Config represents the settings from a .mato.yaml file.
// All fields are pointers to distinguish "not set" from "zero value".
type Config struct {
    Branch        *string `yaml:"branch"`
    TasksDir      *string `yaml:"tasks_dir"`
    DockerImage   *string `yaml:"docker_image"`
    DefaultModel  *string `yaml:"default_model"`
    AgentTimeout  *string `yaml:"agent_timeout"`
    RetryCooldown *string `yaml:"retry_cooldown"`
}
```

No accessor methods. Callers check `cfg.Branch != nil` and dereference
`*cfg.Branch` directly. Empty/whitespace-only string values are normalized to
nil during loading so callers only need to check `!= nil`.

### Config Package API

```go
const configFileName = ".mato.yaml"

// Load reads .mato.yaml from dir. Returns zero Config if file not found.
func Load(dir string) (Config, error)

// LoadFile parses a specific file path.
func LoadFile(path string) (Config, error)
```

The config package is pure parse + normalize. No validation functions. All
validation happens at point of use in the consuming layer (`cmd/mato/main.go`).

### Resolution and Validation Boundary

**Key principle**: All precedence resolution happens in `cmd/mato/main.go`.
Internal packages (`runner`, `queue`) receive fully resolved concrete values
and do not perform their own env-var lookups for settings that are also in the
config file.

This is a deliberate change from the current architecture where `runner` and
`queue` read env vars directly. Moving resolution to the command layer ensures
a single source of truth for precedence and enables correct validation:
config-file values are only validated when they are the effective value (not
overridden by an env var).

**Resolution flow for runner settings** (in `cmd/mato/main.go`):

```go
// resolveRunOptions resolves runner settings from the full precedence chain:
// env var > config file > default. Config values are only parsed/validated
// when they would be the effective value (env var is not set).
func resolveRunOptions(cfg config.Config) (runner.RunOptions, error) {
    var opts runner.RunOptions

    // docker_image: env > config > default
    if v := os.Getenv("MATO_DOCKER_IMAGE"); v != "" {
        opts.DockerImage = v
    } else if cfg.DockerImage != nil {
        opts.DockerImage = *cfg.DockerImage
    }
    // empty string → runner uses "ubuntu:24.04" default

    // default_model: env > config > default
    if v := os.Getenv("MATO_DEFAULT_MODEL"); v != "" {
        opts.DefaultModel = v
    } else if cfg.DefaultModel != nil {
        opts.DefaultModel = *cfg.DefaultModel
    }

    // agent_timeout: env > config > default
    if v := os.Getenv("MATO_AGENT_TIMEOUT"); v != "" {
        d, err := time.ParseDuration(v)
        if err != nil { return opts, fmt.Errorf("parse MATO_AGENT_TIMEOUT %q: %w", v, err) }
        if d <= 0 { return opts, fmt.Errorf("MATO_AGENT_TIMEOUT must be positive, got %v", d) }
        opts.AgentTimeout = d
    } else if cfg.AgentTimeout != nil {
        d, err := time.ParseDuration(*cfg.AgentTimeout)
        if err != nil { return opts, fmt.Errorf("invalid agent_timeout %q in .mato.yaml: %w", *cfg.AgentTimeout, err) }
        if d <= 0 { return opts, fmt.Errorf("agent_timeout in .mato.yaml must be positive, got %v", d) }
        opts.AgentTimeout = d
    }
    // zero → runner uses defaultAgentTimeout (30m)

    // retry_cooldown: env > config > default
    if v := os.Getenv("MATO_RETRY_COOLDOWN"); v != "" {
        if d, err := time.ParseDuration(v); err == nil && d > 0 {
            opts.RetryCooldown = d
        }
        // invalid env values silently ignored (matches current behavior)
    } else if cfg.RetryCooldown != nil {
        d, err := time.ParseDuration(*cfg.RetryCooldown)
        if err != nil { return opts, fmt.Errorf("invalid retry_cooldown %q in .mato.yaml: %w", *cfg.RetryCooldown, err) }
        if d <= 0 { return opts, fmt.Errorf("retry_cooldown in .mato.yaml must be positive, got %v", d) }
        opts.RetryCooldown = d
    }
    // zero → runner uses defaultRetryCooldown (2m)

    return opts, nil
}
```

This design ensures:
- Env var always wins (checked first).
- Config value is only parsed/validated when it would be effective.
- Invalid config with a valid env override does **not** block execution.
- `retry_cooldown` env var remains lenient (invalid silently ignored, matching
  current `internal/queue/claim.go:406-412`); config file is strict (parse
  error is fatal, since config is a deliberate document).

### RunOptions

```go
// RunOptions holds fully resolved configuration values for a mato run.
// Zero values mean "use hardcoded default."
type RunOptions struct {
    DockerImage   string
    DefaultModel  string
    AgentTimeout  time.Duration
    RetryCooldown time.Duration
}
```

`runner.Run()` signature becomes:
```go
func Run(repoRoot, branch, tasksDirOverride string, copilotArgs []string, opts RunOptions) error
```

Inside `Run()`, the option values are used directly. The env-var lookups for
`MATO_DOCKER_IMAGE`, `MATO_DEFAULT_MODEL`, `MATO_AGENT_TIMEOUT`, and
`MATO_RETRY_COOLDOWN` are **removed from the runner** since resolution now
happens in the command layer. When `RunOptions` fields are zero, runner uses
its existing hardcoded defaults.

`DryRun()` signature is unchanged (does not use docker image, model, timeout,
or retry cooldown).

### How RunOptions Reach Docker

- **docker_image**: Stored in `envConfig.image`. If `opts.DockerImage != ""`,
  use it; else `"ubuntu:24.04"`.
- **default_model**: Stored in new `envConfig.resolvedModel`. If
  `opts.DefaultModel != ""`, use it; else `"claude-opus-4.6"`.
  `buildDockerArgs` uses `env.resolvedModel`. Standalone `defaultModel()`
  removed.
- **agent_timeout**: If `opts.AgentTimeout > 0`, use it; else
  `defaultAgentTimeout` (30m). `parseAgentTimeout()` removed (resolution moved
  to command layer).
- **retry_cooldown**: Passed to `queue.SelectAndClaimTask()` as a `cooldown`
  parameter. If zero, queue uses `defaultRetryCooldown` (2m).
  `retryCooldown()` simplified to just use the passed value or default.

### Test Hooks

```go
// cmd/mato/main.go

// runFn is the function used to start the orchestrator loop. Defaults to
// runner.Run, replaceable in tests to observe resolved values.
var runFn = runner.Run

// dryRunFn is the function used for dry-run validation. Defaults to
// runner.DryRun, replaceable in tests.
var dryRunFn = runner.DryRun
```

Matches existing `doctorRunFn` pattern (`cmd/mato/main.go:388-390`).

### Shared Helpers (cmd/mato/main.go)

```go
// resolveConfigBranch: flag > config > "mato".
func resolveConfigBranch(cfg config.Config, flagValue string) string

// resolveConfigTasksDir: flag > config > "".
// Config-file relative paths resolved against repoRoot.
// CLI flag values returned UNCHANGED — each command applies its own
// relative-path semantics afterward (init: repo-root-relative per
// cmd/mato/main.go:273-278; other commands: CWD-relative or downstream).
func resolveConfigTasksDir(repoRoot string, cfg config.Config, flagValue string) string
```

### Per-Command Config Integration

**Root command run mode**:
```
1. resolveRepo → validateRepoPath → resolveRepoRoot → repoRoot
2. config.Load(repoRoot) → fileCfg (fatal on read/parse error)
3. opts, err = resolveRunOptions(fileCfg) (fatal on invalid effective durations)
4. branch = resolveConfigBranch(fileCfg, cfg.branch)
5. validateBranch(branch)
6. tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, cfg.tasksDir)
7. runFn(repoRoot, branch, tasksDir, copilotArgs, opts)
```

**Root command dry-run**:
```
1-2. Same as run (Load only, no resolveRunOptions — dry-run doesn't use them)
3. branch = resolveConfigBranch(fileCfg, cfg.branch)
4. validateBranch(branch)
5. tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, cfg.tasksDir)
6. dryRunFn(repoRoot, branch, tasksDir)
```

`runner.Run()` no longer re-resolves repo root internally (the redundant
`git rev-parse --show-toplevel` at `internal/runner/runner.go:264` is removed
since the caller provides the resolved root).

**Init**:
```
1. resolveRepo + validateRepoPath + resolveRepoRoot (existing, line 264)
2. config.Load(repoRoot) (fatal on error)
3. branch = resolveConfigBranch(fileCfg, initBranch)
4. validateBranch(branch)
5. tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, initTasksDir)
6. Existing init-specific relative path resolution (unchanged):
   if tasksDir == "" { tasksDir = filepath.Join(repoRoot, ".tasks") }
   else if !filepath.IsAbs(tasksDir) { tasksDir = filepath.Join(repoRoot, tasksDir) }
7. setup.InitRepo(repoRoot, branch, filepath.Clean(tasksDir))
```

Init's relative-path handling (step 6) is preserved from current code
(`cmd/mato/main.go:273-278`). The shared helper returns flag values unchanged.

**Status** — config resolved before format/watch branching:
```
1. Format/watch validation (existing)
2. resolveRepo → resolveRepoRoot (new — needed for config discovery)
3. config.Load(repoRoot) (fatal on error)
4. tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, statusTasksDir)
5. All three paths use same resolved tasksDir:
   - JSON: status.ShowJSON(os.Stdout, repoRoot, tasksDir)
   - Watch: status.Watch(ctx, repoRoot, tasksDir, interval)
   - Default: status.Show(repoRoot, tasksDir)
```

No branch resolution. No runner settings resolution. Config resolution happens
before the format/watch branch so all paths receive the same tasksDir.

Internal functions (`status.ShowTo`, `status.ShowJSON`) re-resolve repo root
via `git rev-parse`; this duplication is intentional and idempotent.

**Graph**: Same pattern as status. No branch. No runner settings.
```
1. Format validation
2. resolveRepo → resolveRepoRoot (new)
3. config.Load(repoRoot) (fatal)
4. tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, graphTasksDir)
5. graph.Show(repoRoot, tasksDir, format, showAll)
```

**Doctor** — best-effort config, conditional on `doctorTasksDir == ""`:
```
If --tasks-dir not set:
  Try resolveRepoRoot(repoInput) → on error: skip config
  Try config.Load(repoRoot) → on error: warn stderr, skip config
  If loaded: tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, "")
If --tasks-dir set: skip config entirely
doctorRunFn(ctx, repoInput, tasksDir, opts)
```

Doctor's internal repo resolution (`internal/doctor/checks.go`) continues
to produce structured findings. The command-level config load is purely for
`tasks_dir` defaulting.

**Retry**:
```
If --tasks-dir set: use directly, skip config
Else:
  resolveRepo + resolveRepoRoot (existing, lines 498-508)
  config.Load(repoRoot) (fatal on error)
  tasksDir = resolveConfigTasksDir(repoRoot, fileCfg, "")
  If empty: filepath.Join(repoRoot, ".tasks")
```

### Validation Summary

| What | When | How |
|------|------|-----|
| YAML syntax | `config.Load()` | `yaml.Unmarshal` error |
| Branch name | After `resolveConfigBranch()` | Existing `validateBranch()` |
| Duration strings | `resolveRunOptions()` (run only) | `time.ParseDuration`; only when effective (env not set) |
| Tasks dir path | Downstream use time | Existing `validateTasksDir()` etc. |
| Docker image | Never | Arbitrary string |
| Default model | Never | Arbitrary string |

Validation happens at point-of-use, after precedence resolution. Config-file
values that are overridden by higher-precedence sources (env vars, CLI flags)
are never parsed or validated. Commands that don't use a field don't validate
it.

### Validation Scope Per Command

| Command | Loads config | Resolves from config | Validates |
|---------|-------------|---------------------|-----------|
| `mato` (run) | Fatal | branch, tasks_dir, docker_image, default_model, agent_timeout, retry_cooldown | durations (effective only), branch |
| `mato --dry-run` | Fatal | branch, tasks_dir | branch |
| `mato init` | Fatal | branch, tasks_dir | branch |
| `mato status` | Fatal | tasks_dir only | — |
| `mato graph` | Fatal | tasks_dir only | — |
| `mato retry` | Fatal (if loaded) | tasks_dir only | — |
| `mato doctor` | Best-effort | tasks_dir only | — |

### Precedence Summary

| Setting | CLI Flag | Env Var | Config File | Default |
|---------|----------|---------|-------------|---------|
| repo | `--repo` | — | — | CWD |
| branch | `--branch` | — | `branch` | `"mato"` |
| tasks_dir | `--tasks-dir` | — | `tasks_dir` | `<repo>/.tasks` |
| dry_run | `--dry-run` | — | — | `false` |
| docker_image | — | `MATO_DOCKER_IMAGE` | `docker_image` | `ubuntu:24.04` |
| default_model | `--model`* | `MATO_DEFAULT_MODEL` | `default_model` | `claude-opus-4.6` |
| agent_timeout | — | `MATO_AGENT_TIMEOUT` | `agent_timeout` | `30m` |
| retry_cooldown | — | `MATO_RETRY_COOLDOWN` | `retry_cooldown` | `2m` |

*`--model` is a Copilot CLI arg forwarded through, not a mato flag. Detection
uses existing `hasModelArg()` function (`internal/runner/config.go:226-237`).

### Known Limitation

`runner.Run()` hardcodes `/.tasks/` in its `.gitignore` update
(`internal/runner/runner.go:309`), regardless of the actual tasks directory.
This is a pre-existing limitation. `mato init` computes the correct ignore
pattern from the actual tasks dir path. Aligning `runner.Run()` with init's
behavior is a natural follow-up.

## 4. Step-by-Step Breakdown

### Step 1: Create `internal/config/` package

**File**: `internal/config/config.go` (new)
**Dependency**: None (imports: `os`, `fmt`, `path/filepath`, `strings`,
`gopkg.in/yaml.v3`).

Contains:
- `configFileName` constant: `.mato.yaml`
- `Config` struct with exported pointer fields and yaml tags
- `Load(dir string) (Config, error)` — loads from dir
- `LoadFile(path string) (Config, error)` — parses specific path
- `normalize(cfg *Config)` — unexported; sets empty/whitespace `*string` to nil

### Step 2: Unit tests for config package

**File**: `internal/config/config_test.go` (new)
**Dependency**: Step 1.

### Step 3: Update runner to accept RunOptions

**Files**: `internal/runner/runner.go`, `internal/runner/config.go`
**Dependency**: None.

Changes:
- Add `RunOptions` struct to `runner.go`
- Update `Run()` signature: add `opts RunOptions`
- Update `buildEnvAndRunContext()`: use `opts.DockerImage` when non-empty
  instead of reading `MATO_DOCKER_IMAGE`; use `opts.DefaultModel` when
  non-empty; store in `envConfig.resolvedModel` (new field)
- Update `buildDockerArgs`: use `env.resolvedModel` instead of calling
  `defaultModel()`
- Remove `defaultModel()` function
- Remove `parseAgentTimeout()` (resolution moved to command layer); use
  `opts.AgentTimeout` or `defaultAgentTimeout` directly
- Remove redundant `git rev-parse --show-toplevel` from top of `Run()`

### Step 4: Thread retry cooldown through runner to queue

**Files**: `internal/runner/runner.go`, `internal/queue/claim.go`
**Dependency**: Step 3.

Changes:
- `queue.SelectAndClaimTask()`: add `cooldown time.Duration` parameter
- Simplify `retryCooldown()`: use passed cooldown if > 0, else
  `defaultRetryCooldown`
- Remove env-var lookup from `retryCooldown()` (moved to command layer)
- Runner poll loop passes `opts.RetryCooldown`

### Step 5: Integrate config into root command

**File**: `cmd/mato/main.go`
**Dependency**: Steps 1, 3, 4.

Changes:
- Add `runFn` and `dryRunFn` test hooks
- Import `mato/internal/config`
- Add `resolveConfigBranch()`, `resolveConfigTasksDir()`,
  `resolveRunOptions()` helpers
- Root command `RunE`: resolve repo root → load config → resolve all values
  → call `runFn`/`dryRunFn`

### Step 6: Integrate config into subcommands

**File**: `cmd/mato/main.go`
**Dependency**: Steps 1, 5.

Changes per subcommand:
- **init**: Load config, resolve branch and tasks_dir, preserve existing
  relative-path handling
- **status**: Resolve repo root (new), load config, resolve tasks_dir before
  format/watch branching
- **graph**: Resolve repo root (new), load config, resolve tasks_dir
- **doctor**: Best-effort config load when `doctorTasksDir == ""`, skip when
  flag set
- **retry**: Load config after resolving repo root (when tasks_dir not set)

### Step 7: Update runner tests

**Files**: `internal/runner/runner_test.go`, `internal/runner/config_test.go`
**Dependency**: Steps 3-4.

### Step 8: Update queue tests

**File**: `internal/queue/claim_test.go`
**Dependency**: Step 4.

Update existing `SelectAndClaimTask` test call sites to pass zero cooldown
(preserving existing behavior). Add test for non-zero cooldown.

### Step 9: Command-level tests

**File**: `cmd/mato/main_test.go`
**Dependency**: Steps 5-6.

Tests use `runFn`/`dryRunFn` hooks to intercept and verify resolved values
without Docker.

### Step 10: Integration tests

**File**: `internal/integration/config_test.go` (new, `package integration_test`)
**Dependency**: Steps 5-6.

### Step 11: Documentation

**Files**: `docs/configuration.md` (update existing CLI/env tables for
precedence, add config file section), `README.md` (mention `.mato.yaml`),
`AGENTS.md` (add `config/` to project layout).
**Dependency**: Steps 1-10.

## 5. File Changes

| File | Action | What Changes |
|------|--------|--------------|
| `internal/config/config.go` | Create | Config struct, Load, LoadFile, normalize |
| `internal/config/config_test.go` | Create | 11 unit tests |
| `internal/runner/runner.go` | Modify | RunOptions struct, Run() signature, remove redundant repo root, remove env-var lookups for config-backed settings |
| `internal/runner/config.go` | Modify | resolvedModel in envConfig, buildEnvAndRunContext uses opts, buildDockerArgs uses env.resolvedModel, remove defaultModel(), remove parseAgentTimeout() |
| `internal/runner/runner_test.go` | Modify | Update Run() call sites, add RunOptions tests |
| `internal/runner/config_test.go` | Modify | Update for removed functions, add opts tests |
| `internal/queue/claim.go` | Modify | SelectAndClaimTask cooldown param, simplify retryCooldown() |
| `internal/queue/claim_test.go` | Modify | Update call sites, add cooldown test |
| `cmd/mato/main.go` | Modify | runFn/dryRunFn hooks, config loading, resolution helpers, all subcommands |
| `cmd/mato/main_test.go` | Modify | 20 config precedence tests |
| `internal/integration/config_test.go` | Create | 2 end-to-end tests |
| `docs/configuration.md` | Modify | Update existing tables, add config file section |
| `README.md` | Modify | Mention .mato.yaml |
| `AGENTS.md` | Modify | Add config/ to project layout |

## 6. Error Handling

- **Config file not found**: Zero Config returned, no error. Normal case.
- **Config file unreadable**: `fmt.Errorf("read config file %s: %w", path, err)`
  — fatal for all commands except doctor (best-effort skip).
- **Config file invalid YAML**: `fmt.Errorf("parse config file %s: %w", path, err)`
  — fatal for all commands except doctor (best-effort skip with warning).
- **Invalid `agent_timeout` in config (effective)**: Fatal via
  `resolveRunOptions()`. Only triggered in run mode when env var not set.
- **Invalid `retry_cooldown` in config (effective)**: Fatal via
  `resolveRunOptions()`. Only triggered in run mode when env var not set.
- **Invalid `branch` in config**: Fatal via existing `validateBranch()`. Only
  triggered in commands that use branch (run, dry-run, init).
- **Invalid config value overridden by env var**: No error. The overridden
  value is never parsed.
- **Doctor errors**: All config errors downgraded to stderr warnings.
- **Empty/whitespace values**: Normalized to nil during loading.
- All errors: lowercase verb phrase, `%w` wrapping per codebase convention.

## 7. Testing Strategy

### `internal/config/config_test.go` (11 cases)

| Test | Verifies |
|------|----------|
| `TestLoad_AllFields` | All fields parsed from YAML |
| `TestLoad_PartialFields` | Unset fields are nil |
| `TestLoad_EmptyFile` | Zero Config, no error |
| `TestLoad_FileNotFound` | Zero Config, no error |
| `TestLoad_InvalidYAML` | Returns error |
| `TestLoad_EmptyStringValues` | Normalized to nil |
| `TestLoad_WhitespaceValues` | Normalized to nil |
| `TestLoad_UnknownKeys` | No error |
| `TestLoad_RelativeTasksDir` | Preserved as-is in struct |
| `TestLoadFile_ValidPath` | Parses successfully |
| `TestLoadFile_InvalidPath` | Returns error |

### `internal/runner/` tests (7 new/updated)

| Test | Verifies |
|------|----------|
| `TestRunOptions_DockerImageOverride` | opts.DockerImage → envConfig.image |
| `TestRunOptions_DefaultModelOverride` | opts.DefaultModel → envConfig.resolvedModel |
| `TestRunOptions_AgentTimeoutOverride` | opts.AgentTimeout → runContext.timeout |
| `TestRunOptions_RetryCooldownOverride` | opts.RetryCooldown passed to queue |
| `TestRunOptions_ZeroValues` | Existing default behavior preserved |
| `TestBuildDockerArgs_UsesResolvedModel` | Docker args include correct model |
| `TestBuildEnvAndRunContext_OptsApplied` | opts values stored in envConfig |

### `internal/queue/claim_test.go` (1 new + updates)

| Test | Verifies |
|------|----------|
| Existing tests | Pass zero cooldown (unchanged behavior) |
| `TestSelectAndClaimTask_CustomCooldown` | Non-zero cooldown used |

### `cmd/mato/main_test.go` (20 cases)

| Test | Verifies |
|------|----------|
| `TestConfigFile_BranchFromConfig` | Config branch used when flag absent |
| `TestConfigFile_BranchFlagOverridesConfig` | CLI flag wins |
| `TestConfigFile_TasksDirFromConfig` | Config tasks_dir used |
| `TestConfigFile_TasksDirFlagOverridesConfig` | CLI flag wins |
| `TestConfigFile_RelativeTasksDir` | Config relative resolved against root |
| `TestConfigFile_MissingConfig` | Falls through to defaults |
| `TestConfigFile_InvalidYAML` | Error returned |
| `TestConfigFile_InvalidAgentTimeout_RunMode` | Fatal when effective |
| `TestConfigFile_InvalidAgentTimeout_StatusOK` | Status ignores (doesn't use) |
| `TestConfigFile_InvalidBranch_StatusOK` | Status ignores (doesn't resolve) |
| `TestConfigFile_InvalidTimeout_EnvOverride` | Succeeds: env overrides bad config |
| `TestConfigFile_InvalidCooldown_EnvOverride` | Succeeds: env overrides bad config |
| `TestConfigFile_RunOptionsFromConfig` | Values reach RunOptions via runFn |
| `TestConfigFile_InitUsesConfig` | Init branch + tasks_dir from config |
| `TestConfigFile_InitRelativeTasksDir` | Init preserves repo-root-relative |
| `TestConfigFile_StatusUsesConfig` | Status text uses config tasks_dir |
| `TestConfigFile_StatusJSONUsesConfig` | Status JSON uses config tasks_dir |
| `TestConfigFile_GraphUsesConfig` | Graph uses config tasks_dir |
| `TestConfigFile_RetryUsesConfig` | Retry uses config tasks_dir |
| `TestConfigFile_DoctorBestEffort` | Doctor proceeds on invalid config |

### `internal/integration/config_test.go` (2 cases)

| Test | Verifies |
|------|----------|
| `TestConfigFile_EndToEnd` | Full config load + resolution in real git repo |
| `TestConfigFile_DryRunWithConfig` | Dry-run uses branch and tasks_dir from config |

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `runner.Run()` signature change | All call sites (production + tests) updated atomically |
| `queue.SelectAndClaimTask()` parameter change | All call sites updated |
| Env-var lookups moved from runner/queue to command layer | Behavioral equivalent; tested |
| Root command now resolves repo root | Functionally identical to prior delegation |
| Status/graph add command-level repo root resolution | Idempotent with internal resolution |
| Import cycle `config` → internal | `config` imports only stdlib + yaml.v3 |
| Doctor config error | Best-effort: extracts tasks_dir only |
| Unknown YAML keys | Forward compatibility; documented |
| Runner gitignore hardcoded `/.tasks/` | Pre-existing; deferred follow-up |
| `defaultModel()` and `parseAgentTimeout()` removed | Resolution moved to command layer |
| Init relative `--tasks-dir` path | Shared helper returns flag unchanged; init applies own logic |

## 9. Open Questions

1. **Should `repo` be configurable via config file?** No: the repo path
   determines where to find the config file, creating a circular dependency.

2. **Should `dry_run` be configurable via config file?** No: per-invocation
   flag, not a persistent setting.

3. **Should `runner.Run()` skip internal repo root resolution?** Yes: the
   root command now provides the resolved root, so the `git rev-parse
   --show-toplevel` call at the top of `Run()` is removed.

## 10. Evolution Notes

This plan went through 11 review iterations with GPT-5.4. Key design decisions
that evolved:

1. **Config discovery**: Started as "walk up from CWD." Simplified to
   repo-root-only lookup (Round 2).

2. **Resolution boundary**: Initially threaded raw `Config` struct through
   internal packages. Revised to resolve all values at the command layer and
   pass concrete values (Round 2).

3. **Validation scope**: Initially validated all fields for all commands.
   Narrowed through several rounds to validate only fields each command
   actually consumes (Rounds 6-10).

4. **Precedence enforcement**: Initially validated config values before
   checking env overrides, which blocked execution even when env vars provided
   valid effective values. Fixed to only validate the winning value after
   precedence resolution (Round 11).

5. **Doctor tolerance**: Evolved from fatal config errors to best-effort
   loading conditional on `--tasks-dir` flag (Rounds 4-5).

6. **Status command coverage**: All three status paths (text, JSON, watch)
   receive the same config-resolved tasksDir by resolving before the
   format/watch branch (Round 4).

7. **Init relative paths**: Shared helper preserves init's existing
   repo-root-relative CLI flag behavior by returning flag values unchanged
   (Round 10).
