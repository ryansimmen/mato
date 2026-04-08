# `mato` Config File - Implementation Plan

> **Status: Implemented** — This proposal has been fully implemented.
> **Read-only:** This file is a historical snapshot. Update the source code and living docs instead.
> The text below describes the original design; see the source code for
> the current implementation.

## 1. Goal

Add support for an optional YAML configuration file (`.mato.yaml`) that provides
persistent, per-repository settings for mato. This complements the existing CLI
flags and environment variables, giving users a committed (or gitignored) way to
configure default behavior without repetitive command-line arguments or shell
profile modifications.

This proposal assumes the current hard-break queue model:

- the queue path is fixed at `<repo>/.mato`
- the container-visible queue path is fixed at `/workspace/.mato`
- queue location is not configurable via CLI, env var, or config file

## 2. Scope

### In Scope

- New `internal/config/` package for config file parsing.
- Config file format definition (YAML, using existing `gopkg.in/yaml.v3`).
- Config file loaded from the repo root (next to `.git/`).
- Precedence chain: CLI flag > environment variable > config file > hardcoded
  default.
- Integration with the root command and relevant subcommands.
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
- Making the queue location configurable.

## 3. Design

### Config File Format

**File name**: `.mato.yaml` (`.mato.yml` also accepted; added later)
**Location**: repository root (same directory as `.git/`)

```yaml
# .mato.yaml - mato configuration file
# All fields are optional. CLI flags and environment variables take precedence.

# Target branch for merge processing (default: "mato")
branch: main

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
(for example `agent_timout`) are not caught. This should be documented.

### Config Struct

```go
package config

// Config represents the settings from a .mato.yaml file.
// All fields are pointers to distinguish "not set" from "zero value".
type Config struct {
    Branch        *string `yaml:"branch"`
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
    // empty string -> runner uses "ubuntu:24.04" default

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
    // zero -> runner uses defaultAgentTimeout (30m)

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
    // zero -> runner uses defaultRetryCooldown (2m)

    return opts, nil
}
```

This design ensures:
- Env var always wins (checked first).
- Config value is only parsed/validated when it would be effective.
- Invalid config with a valid env override does **not** block execution.
- `retry_cooldown` env var remains lenient (invalid silently ignored, matching
  current behavior); config file is strict (parse error is fatal, since config
  is a deliberate document).

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
func Run(repoRoot, branch string, copilotArgs []string, opts RunOptions) error
```

Inside `Run()`, the option values are used directly. The env-var lookups for
`MATO_DOCKER_IMAGE`, `MATO_DEFAULT_MODEL`, `MATO_AGENT_TIMEOUT`, and
`MATO_RETRY_COOLDOWN` are removed from the runner since resolution now happens
in the command layer. When `RunOptions` fields are zero, runner uses its
existing hardcoded defaults.

`DryRun()` and `InitRepo()` signatures remain focused on `repoRoot` and
`branch`; both continue to derive the queue path as `<repo>/.mato` from the
resolved repo root.

### How RunOptions Reach Docker

- **docker_image**: Stored in `envConfig.image`. If `opts.DockerImage != ""`,
  use it; else `"ubuntu:24.04"`.
- **default_model**: If `opts.DefaultModel != ""`, use it; else
  `"claude-opus-4.6"`.
- **agent_timeout**: If `opts.AgentTimeout > 0`, use it; else
  `defaultAgentTimeout` (30m).
- **retry_cooldown**: Passed to `queue.SelectAndClaimTask()` as a `cooldown`
  parameter. If zero, queue uses `defaultRetryCooldown` (2m).

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

Matches the existing `doctorRunFn` pattern.

### Shared Helpers (cmd/mato/main.go)

```go
// resolveConfigBranch: flag > config > "mato".
func resolveConfigBranch(cfg config.Config, flagValue string) string

// resolveRunOptions: env > config > default for runner settings.
func resolveRunOptions(cfg config.Config) (runner.RunOptions, error)
```

### Per-Command Config Integration

**Root command run mode**:
```
1. resolveRepo -> validateRepoPath -> resolveRepoRoot -> repoRoot
2. config.Load(repoRoot) -> fileCfg (fatal on read/parse error)
3. opts, err = resolveRunOptions(fileCfg) (fatal on invalid effective durations)
4. branch = resolveConfigBranch(fileCfg, cfg.branch)
5. validateBranch(branch)
6. runFn(repoRoot, branch, copilotArgs, opts)
```

**Root command dry-run**:
```
1-2. Same as run (Load only; no resolveRunOptions - dry-run doesn't use them)
3. branch = resolveConfigBranch(fileCfg, cfg.branch)
4. validateBranch(branch)
5. dryRunFn(repoRoot, branch)
```

**Init**:
```
1. resolveRepo + validateRepoPath + resolveRepoRoot
2. config.Load(repoRoot) (fatal on error)
3. branch = resolveConfigBranch(fileCfg, initBranch)
4. validateBranch(branch)
5. setup.InitRepo(repoRoot, branch)
```

**Status / Graph / Retry / Doctor**:

These commands do not need config-file integration in the first version because
they do not consume any config-backed settings. They already derive the queue as
`<repo>/.mato` from the resolved repo root, and that path is intentionally not
configurable.

### Validation Summary

| What | When | How |
|------|------|-----|
| YAML syntax | `config.Load()` | `yaml.Unmarshal` error |
| Branch name | After `resolveConfigBranch()` | Existing `validateBranch()` |
| Duration strings | `resolveRunOptions()` (run only) | `time.ParseDuration`; only when effective (env not set) |
| Docker image | Never | Arbitrary string |
| Default model | Never | Arbitrary string |

Validation happens at point-of-use, after precedence resolution. Config-file
values that are overridden by higher-precedence sources (env vars, CLI flags)
are never parsed or validated. Commands that do not use a field do not validate
it.

### Validation Scope Per Command

| Command | Loads config | Resolves from config | Validates |
|---------|-------------|---------------------|-----------|
| `mato` (run) | Fatal | branch, docker_image, default_model, agent_timeout, retry_cooldown | durations (effective only), branch |
| `mato --dry-run` | Fatal | branch | branch |
| `mato init` | Fatal | branch | branch |
| `mato status` | No | — | — |
| `mato graph` | No | — | — |
| `mato retry` | No | — | — |
| `mato doctor` | No | — | — |

### Precedence Summary

| Setting | CLI Flag | Env Var | Config File | Default |
|---------|----------|---------|-------------|---------|
| repo | `--repo` | — | — | CWD |
| branch | `--branch` | — | `branch` | `"mato"` |
| dry_run | `--dry-run` | — | — | `false` |
| docker_image | — | `MATO_DOCKER_IMAGE` | `docker_image` | `ubuntu:24.04` |
| default_model | `--model`* | `MATO_DEFAULT_MODEL` | `default_model` | `claude-opus-4.6` |
| agent_timeout | — | `MATO_AGENT_TIMEOUT` | `agent_timeout` | `30m` |
| retry_cooldown | — | `MATO_RETRY_COOLDOWN` | `retry_cooldown` | `2m` |

*`--model` is a Copilot CLI arg forwarded through, not a mato flag. Detection
uses existing `hasModelArg()` logic.

## 4. Step-by-Step Breakdown

### Step 1: Create `internal/config/` package

**File**: `internal/config/config.go` (new)
**Dependency**: None (imports: `os`, `fmt`, `path/filepath`, `strings`,
`gopkg.in/yaml.v3`).

Contains:
- `configFileName` constant: `.mato.yaml`
- `Config` struct with exported pointer fields and yaml tags
- `Load(dir string) (Config, error)` - loads from dir
- `LoadFile(path string) (Config, error)` - parses specific path
- `normalize(cfg *Config)` - unexported; sets empty/whitespace `*string` to nil

### Step 2: Unit tests for config package

**File**: `internal/config/config_test.go` (new)
**Dependency**: Step 1.

### Step 3: Update runner to accept RunOptions

**Files**: `internal/runner/runner.go`, `internal/runner/config.go`
**Dependency**: None.

Changes:
- Add `RunOptions` struct to `runner.go`
- Update `Run()` signature: add `opts RunOptions`
- Update runtime config wiring to use `opts.DockerImage`, `opts.DefaultModel`,
  `opts.AgentTimeout`, and `opts.RetryCooldown` instead of reading config-backed
  env vars directly in the runner

### Step 4: Thread retry cooldown through runner to queue

**Files**: `internal/runner/runner.go`, `internal/queue/claim.go`
**Dependency**: Step 3.

Changes:
- `queue.SelectAndClaimTask()`: add `cooldown time.Duration` parameter
- Simplify retry cooldown resolution to use the passed value or the default

### Step 5: Integrate config into root command

**File**: `cmd/mato/main.go`
**Dependency**: Steps 1, 3, 4.

Changes:
- Add `runFn` and `dryRunFn` test hooks
- Import `mato/internal/config`
- Add `resolveConfigBranch()` and `resolveRunOptions()` helpers
- Root command `RunE`: resolve repo root -> load config -> resolve all values
  -> call `runFn` / `dryRunFn`

### Step 6: Integrate config into init

**File**: `cmd/mato/main.go`
**Dependency**: Steps 1, 5.

Changes:
- `init`: load config, resolve branch, call `setup.InitRepo(repoRoot, branch)`
- Leave other subcommands unchanged in v1 because they do not consume any
  config-backed settings

### Step 7: Update runner tests

**Files**: `internal/runner/runner_test.go`, `internal/runner/config_test.go`
**Dependency**: Steps 3-4.

### Step 8: Update queue tests

**File**: `internal/queue/claim_test.go`
**Dependency**: Step 4.

Update existing `SelectAndClaimTask` test call sites to pass zero cooldown
(preserving existing behavior). Add a test for non-zero cooldown.

### Step 9: Command-level tests

**File**: `cmd/mato/main_test.go`
**Dependency**: Steps 5-6.

Tests use `runFn` / `dryRunFn` hooks to intercept and verify resolved values
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
| `internal/config/config_test.go` | Create | Unit tests |
| `internal/runner/runner.go` | Modify | `RunOptions` struct, `Run()` signature, remove config-backed env lookups |
| `internal/runner/config.go` | Modify | Runtime wiring uses resolved options |
| `internal/queue/claim.go` | Modify | `SelectAndClaimTask` cooldown param |
| `internal/queue/claim_test.go` | Modify | Update call sites, add cooldown test |
| `cmd/mato/main.go` | Modify | `runFn` / `dryRunFn` hooks, config loading, resolution helpers |
| `cmd/mato/main_test.go` | Modify | Config precedence tests |
| `internal/integration/config_test.go` | Create | End-to-end config tests |
| `docs/configuration.md` | Modify | Add config file section |
| `README.md` | Modify | Mention `.mato.yaml` |
| `AGENTS.md` | Modify | Add `config/` to project layout |

## 6. Error Handling

- **Config file not found**: Zero Config returned, no error. Normal case.
- **Config file unreadable**: `fmt.Errorf("read config file %s: %w", path, err)`
  - fatal for commands that load config.
- **Config file invalid YAML**: `fmt.Errorf("parse config file %s: %w", path, err)`
  - fatal for commands that load config.
- **Invalid `agent_timeout` in config (effective)**: Fatal via
  `resolveRunOptions()`. Only triggered in run mode when env var is not set.
- **Invalid `retry_cooldown` in config (effective)**: Fatal via
  `resolveRunOptions()`. Only triggered in run mode when env var is not set.
- **Invalid `branch` in config**: Fatal via existing `validateBranch()`. Only
  triggered in commands that use branch (run, dry-run, init).
- **Invalid config value overridden by env var**: No error. The overridden
  value is never parsed.
- **Empty/whitespace values**: Normalized to nil during loading.
- All errors: lowercase verb phrase, `%w` wrapping per codebase convention.

## 7. Testing Strategy

### `internal/config/config_test.go`

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
| `TestLoadFile_ValidPath` | Parses successfully |
| `TestLoadFile_InvalidPath` | Returns error |

### `internal/runner/` tests

| Test | Verifies |
|------|----------|
| `TestRunOptions_DockerImageOverride` | `opts.DockerImage` reaches runtime config |
| `TestRunOptions_DefaultModelOverride` | `opts.DefaultModel` reaches docker args |
| `TestRunOptions_AgentTimeoutOverride` | `opts.AgentTimeout` reaches run context |
| `TestRunOptions_RetryCooldownOverride` | `opts.RetryCooldown` passed to queue |
| `TestRunOptions_ZeroValues` | Existing default behavior preserved |

### `internal/queue/claim_test.go`

| Test | Verifies |
|------|----------|
| Existing tests | Pass zero cooldown (unchanged behavior) |
| `TestSelectAndClaimTask_CustomCooldown` | Non-zero cooldown used |

### `cmd/mato/main_test.go`

| Test | Verifies |
|------|----------|
| `TestConfigFile_BranchFromConfig` | Config branch used when flag absent |
| `TestConfigFile_BranchFlagOverridesConfig` | CLI flag wins |
| `TestConfigFile_MissingConfig` | Falls through to defaults |
| `TestConfigFile_InvalidYAML` | Error returned |
| `TestConfigFile_InvalidAgentTimeout_RunMode` | Fatal when effective |
| `TestConfigFile_InvalidTimeout_EnvOverride` | Succeeds: env overrides bad config |
| `TestConfigFile_InvalidCooldown_EnvOverride` | Succeeds: env overrides bad config |
| `TestConfigFile_RunOptionsFromConfig` | Values reach `RunOptions` via `runFn` |
| `TestConfigFile_InitUsesConfig` | Init branch from config |

### `internal/integration/config_test.go`

| Test | Verifies |
|------|----------|
| `TestConfigFile_EndToEnd` | Full config load + resolution in real git repo |
| `TestConfigFile_DryRunWithConfig` | Dry-run uses branch from config |

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `runner.Run()` signature change | All call sites (production + tests) updated atomically |
| `queue.SelectAndClaimTask()` parameter change | All call sites updated |
| Env-var lookups moved from runner/queue to command layer | Behavior preserved and tested |
| Root command now resolves repo root earlier | Functionally equivalent to prior delegation |
| Invalid config blocks execution too aggressively | Validate only effective values after precedence resolution |
| Unknown YAML keys hide typos | Forward compatibility; documented |

## 9. Open Questions

1. Should `repo` be configurable via config file? No: the repo path determines
   where to find the config file, creating a circular dependency.

2. Should `dry_run` be configurable via config file? No: per-invocation flag,
   not a persistent setting.

3. Should future config support be extended to review-specific defaults, or
   should the first version stay limited to the existing runner settings?

## 10. Evolution Notes

This plan went through multiple review iterations. Key design decisions that
settled over time:

1. **Config discovery**: Simplified to repo-root-only lookup.

2. **Resolution boundary**: Values are resolved at the command layer and passed
   into internal packages as concrete values.

3. **Validation scope**: Only fields a command actually consumes should be
   validated.

4. **Queue model alignment**: The config proposal now assumes the hard-break
   `.mato` queue model rather than trying to make queue location configurable.
