# `mato config` - Implementation Plan

> **Status: Implemented** — This proposal has been fully implemented.
> The text below describes the implemented design; see the source code for the
> current behavior.

## 1. Goal

Add a read-only `mato config` command that shows the effective repository-level
configuration mato will use and where each setting came from.

Today mato supports configuration through:

- repo-local `.mato.yaml` or `.mato.yml`
- host environment variables
- built-in defaults
- command-specific flags on commands like `run` and `init`

But there is no operator-facing command that answers questions such as:

- which branch will `mato run` use by default here?
- which config file was loaded?
- is an env var overriding the committed config file?
- why are these model, timeout, retry, and Docker defaults in effect?

This proposal adds that visibility while also cleaning up the current config
resolution architecture so one shared internal package owns precedence logic and
source attribution.

## 2. Scope

### In scope

- New top-level `mato config` command.
- Read-only effective-value reporting.
- New shared internal config-resolution package extracted out of `cmd/mato/`.
- Explicit source attribution for resolved settings.
- Accurate default reporting for settings whose defaults are currently applied
  outside `cmd/mato/`.
- Text and JSON output.
- Preservation of current config discovery behavior:
  - load from repo root
  - accept `.mato.yaml` and `.mato.yml`
  - error when both exist
- A safe rollout split across two PRs.
- Unit tests, command tests, and integration coverage.
- Documentation updates.

### Out of scope

- Editing `.mato.yaml` in place.
- Global config files.
- Interactive config editing prompts.
- `mato config init`, `set`, `unset`, or `edit` subcommands.
- Simulating every possible one-shot invocation flag combination in one command.
- Broader `doctor` refactors beyond Docker-image/config resolution.

## 3. Design

### CLI shape

Phase 1 should use a flat command:

```text
mato config [--repo <path>] [--format text|json]
```

Behavior:

- `--repo` follows the existing repo-aware command pattern.
- `--format` supports `text` and `json`.
- The command reports the effective repository defaults mato would use in the
  absence of one-shot subcommand flags.

Do not add a `show` subcommand yet. The current mato CLI is flat (`status`,
`doctor`, `log`, `graph`, `inspect`, `version`), and adding a `show` layer now
would create complexity without current value. If future config-mutating or
config-initialization commands are needed, subcommands can be added later.

### No `.mato/` directory requirement

`mato config` should work in any git repository, including repositories that
have not been initialized with `mato init` and therefore do not yet have a
`.mato/` directory.

This command inspects repository configuration, not queue state. It should not
call `requireTasksDir(...)` and should not warn merely because `.mato/` is
absent.

### Architectural prerequisite: extract config resolution from `cmd/mato`

The current resolution helpers live in `cmd/mato/resolve.go` under package
`main`:

- `resolveEnvBranch()`
- `resolveConfigBranch()`
- `resolveStringOption()`
- `resolveBoolOption()`
- `resolveDurationOption()`
- `resolveRunOptions()`

That prevents a new internal package from reusing them directly.

This proposal therefore starts by extracting config resolution into a reusable
internal package. The new `mato config` command should be built on top of that
same shared package so precedence and validation logic live in one place.

### New package: `internal/configresolve/`

Create a new package that owns repo-local config resolution and provenance.

Use the name `configresolve` as-is. The package has a narrow, concrete purpose:
resolve effective config values plus their sources.

Responsibilities:

- load repo config using the same discovery rules as today
- resolve effective values using the current precedence chain
- expose the source of each resolved value
- expose the specific environment variable name when the source is `env`
- centralize env-var metadata so the resolver, renderer, and tests do not each
  need their own copies of env var names
- know the actual built-in defaults, including defaults that are currently
  applied in `internal/runner/` or `internal/queue/`
- provide helpers reusable by `run`, `init`, `doctor`, and `mato config`

The package should keep env-var names in one place, ideally as constants or a
small metadata table, for the settings it resolves:

- `MATO_BRANCH`
- `MATO_DOCKER_IMAGE`
- `MATO_TASK_MODEL`
- `MATO_REVIEW_MODEL`
- `MATO_REVIEW_SESSION_RESUME_ENABLED`
- `MATO_TASK_REASONING_EFFORT`
- `MATO_REVIEW_REASONING_EFFORT`
- `MATO_AGENT_TIMEOUT`
- `MATO_RETRY_COOLDOWN`

That keeps source attribution, precedence logic, and test expectations aligned.

Suggested source enum:

```go
type Source string

const (
    SourceFlag    Source = "flag"
    SourceEnv     Source = "env"
    SourceConfig  Source = "config"
    SourceDefault Source = "default"
)
```

Use one generic resolved wrapper rather than separate `ResolvedString`,
`ResolvedBool`, and `ResolvedDuration` types:

```go
type Resolved[T any] struct {
    Value   T      `json:"value"`
    Source  Source `json:"source"`
    EnvVar  string `json:"env_var,omitempty"`
}
```

`EnvVar` is set only when `SourceEnv` is used. This lets text output explain
exactly which variable is responsible, for example `MATO_DOCKER_IMAGE`.

### Config loading API

The current `internal/config.Load(dir)` API returns only `Config`, which is not
enough for `mato config` because the command must report:

- whether a config file exists
- which path was loaded
- the current ambiguity error when both `.mato.yaml` and `.mato.yml` exist

Instead of adding a parallel `LoadWithMeta(...)` helper, change `Load(...)`
itself to return a richer result:

```go
type LoadResult struct {
    Config Config
    Path   string
    Exists bool
}

func Load(dir string) (LoadResult, error)
```

This is a small internal migration with only a few current call sites, and it
avoids maintaining two overlapping loading APIs.

### Shared resolved views

The new resolver should expose one model for standing repository defaults:

```go
type RepoDefaults struct {
    RepoRoot     string           `json:"repo_root"`
    ConfigPath   string           `json:"config_path,omitempty"`
    ConfigExists bool             `json:"config_exists"`

    Branch                     Resolved[string] `json:"branch"`
    DockerImage                Resolved[string] `json:"docker_image"`
    TaskModel                  Resolved[string] `json:"task_model"`
    ReviewModel                Resolved[string] `json:"review_model"`
    ReviewSessionResumeEnabled Resolved[bool]   `json:"review_session_resume_enabled"`
    TaskReasoningEffort        Resolved[string] `json:"task_reasoning_effort"`
    ReviewReasoningEffort      Resolved[string] `json:"review_reasoning_effort"`
    AgentTimeout               Resolved[string] `json:"agent_timeout"`
    RetryCooldown              Resolved[string] `json:"retry_cooldown"`
}
```

Use this top-level JSON shape directly rather than wrapping fields in a nested
`settings` object. The command has a fixed, narrow field set, so a flat JSON
structure is simpler for humans and easier to consume in shell tooling.

### Accurate built-in defaults

This command must report actual effective defaults, not placeholder zero values.

Today some defaults are not applied in `cmd/mato/resolve.go`:

- `agent_timeout` falls back inside `internal/runner/runner.go`
- `retry_cooldown` falls back inside `internal/queue/claim.go`

The shared resolver must know these defaults directly and report them as the
effective values when unset. For example:

- `agent_timeout: 30m0s (default)`
- `retry_cooldown: 2m0s (default)`

Returning `0s` for these fields would be wrong and would drift from actual run
behavior.

The shared resolver should also own the branch default directly. When no flag,
env var, or config value is present, the effective branch should resolve to
`mato` from within `configresolve` itself rather than implicitly depending on
legacy command-layer helpers.

### Resolver API boundaries

Recommended package surface:

```go
// Resolve standing repo defaults with provenance.
func ResolveRepoDefaults(repoRoot string) (*RepoDefaults, error)

// Resolve branch for commands that accept a flag override.
func ResolveBranch(load config.LoadResult, flagValue string) (Resolved[string], error)

// Resolve run configuration for commands that accept flag overrides.
func ResolveRunConfig(flags RunFlags, load config.LoadResult) (RunConfig, error)
```

`ResolveRepoDefaults` should compose from the lower-level resolvers rather than
re-implementing precedence, parsing, or defaulting logic. In practice that
means:

- load repo config once via `config.Load(...)`
- call `ResolveBranch(load, "")`
- call `ResolveRunConfig(RunFlags{}, load)`
- convert the resolved run values into the `RepoDefaults` view model

This is especially important for duration-backed settings such as
`agent_timeout` and `retry_cooldown`. `RunConfig` should remain the single owner
of duration parsing, validation, and defaulting. `RepoDefaults` should only
format those already-resolved duration values as strings for stable JSON/text
output.

Use a provenance-aware `RunConfig` as the single source of truth for run-related
resolution rather than returning both `runner.RunOptions` and a parallel set of
provenance fields.

Suggested shape:

```go
type RunConfig struct {
    DockerImage                Resolved[string]
    TaskModel                  Resolved[string]
    ReviewModel                Resolved[string]
    ReviewSessionResumeEnabled Resolved[bool]
    TaskReasoningEffort        Resolved[string]
    ReviewReasoningEffort      Resolved[string]
    AgentTimeout               Resolved[time.Duration]
    RetryCooldown              Resolved[time.Duration]
}

func (c RunConfig) RunOptions() runner.RunOptions
```

This avoids duplicating the same data in two representations and ensures
provenance cannot silently drift from the concrete values `runner` consumes.

`RunFlags` should reflect the flags that actually exist on `mato run` today.
Do not introduce synthetic flag precedence for settings that do not currently
have run-command flags, such as `docker_image`,
`review_session_resume_enabled`, `agent_timeout`, or `retry_cooldown`.

The command layer should keep command-specific validation such as mutually
exclusive flags, but config-source precedence and parsing should move out of
`cmd/mato/resolve.go`.

### `mato config` output contract

Text output should optimize for fast debugging.

Use a fixed explicit ordered field list rather than ad hoc hand-written print
statements spread through the renderer. Recommended text order:

1. `branch`
2. `docker_image`
3. `task_model`
4. `review_model`
5. `review_session_resume_enabled`
6. `task_reasoning_effort`
7. `review_reasoning_effort`
8. `agent_timeout`
9. `retry_cooldown`

When the source is `env`, text output should show the specific variable name.

Example:

```text
Repo: /work/repo
Config file: /work/repo/.mato.yml

branch: main                         (config)
docker_image: ghcr.io/acme/mato:dev  (env: MATO_DOCKER_IMAGE)
task_model: claude-opus-4.6          (default)
review_model: gpt-5.4                (config)
review_session_resume_enabled: true  (default)
task_reasoning_effort: high          (config)
review_reasoning_effort: high        (config)
agent_timeout: 30m0s                 (default)
retry_cooldown: 2m0s                 (default)
```

If no config file exists:

```text
Repo: /work/repo
Config file: none
```

JSON output should expose the same facts in structured form and preserve source
attribution. Durations in `RepoDefaults` should be encoded as strings so the
output stays stable and human-readable.

### Existing command migration

The final architecture should migrate current command-layer resolution to the
new shared package in a focused way:

- `mato run`
  - use shared branch resolution
  - use shared run-config resolution
- `mato init`
  - use shared branch resolution
- `mato doctor`
  - use the shared resolver only for Docker-image and config-loading behavior
  - do not broaden this into a larger doctor refactor
  - preserve current fallback behavior: if repo-root resolution fails, Docker
    image resolution still falls back to env/default; if repo root is known,
    config load/parse errors remain fatal; an env override must not hide a
    malformed committed config file

Other helpers in `cmd/mato/resolve.go` that are unrelated to config resolution,
such as repo path and git validation, can stay where they are.

### Error handling

- Invalid `--format` remains a usage error handled in the command layer.
- Repo path validation remains unchanged.
- Config parse errors remain fatal.
- `.mato.yaml` / `.mato.yml` ambiguity remains fatal.
- Invalid env var bool/duration values should continue returning clear errors,
  matching current behavior.
- `mato config` remains read-only.

## 4. Rollout Plan

Implement this in two PRs rather than one large change.

### PR 1: Shared resolution extraction and migration

Goal: move config resolution into a reusable package and migrate existing
consumers before adding the new command.

#### Step 1: Enrich `internal/config.Load(...)`

**Files**: `internal/config/config.go`, `internal/config/config_test.go`

Change `Load(...)` to return `LoadResult`. Preserve current discovery and parse
behavior. Tests should cover `.mato.yaml`, `.mato.yml`, neither, and both.

#### Step 2: Create `internal/configresolve/`

**Files**: `internal/configresolve/configresolve.go`,
`internal/configresolve/configresolve_test.go`

Add:

- source enum
- generic `Resolved[T]`
- standing repo-default resolution
- branch resolution with provenance
- run-config resolution with provenance
- centralized env-var metadata/constants for resolved settings
- explicit default handling for runner and queue-backed defaults
- `ResolveRepoDefaults(...)` composed from `ResolveBranch(...)` and
  `ResolveRunConfig(...)` rather than duplicating resolution logic

#### Step 3: Migrate existing command consumers

**Files**: `cmd/mato/resolve.go`, `cmd/mato/run.go`, `cmd/mato/commands.go`,
`cmd/mato/main_test.go`

Replace direct config-resolution helpers in package `main` with calls to
`internal/configresolve/`.

Expected changes:

- `newRunCmd()` uses shared branch and run-config resolution.
- `newInitCmd()` uses shared branch resolution.
- `newDoctorCmd()` uses the shared resolver for Docker image/config loading.

Leave repo/path/git validation helpers in `cmd/mato/resolve.go` untouched.

This PR should not add a new user-facing command.

### PR 2: Add `mato config`

Goal: add the read-only config inspection command on top of the shared resolver.

#### Step 4: Add `mato config` command

**Files**: `cmd/mato/commands.go`, `cmd/mato/root.go`

Add `newConfigCmd(repoFlag *string)` with:

- `Use: "config"`
- inherited `--repo` flag from root
- local `--format text|json`
- delegation into a package-level seam such as `configShowFn`

#### Step 5: Add rendering in `internal/configresolve/`

**Files**: `internal/configresolve/`

Keep rendering in `internal/configresolve/` for the first implementation. The
command has one narrow view and a fixed field set, so introducing
`internal/configview/` now would add structure without clear value.

#### Step 6: Command tests

**File**: `cmd/mato/main_test.go`

Add tests for:

- command registration
- `--format` validation
- repo resolution and delegation
- `mato config --format json`
- propagation of resolver errors
- behavior in a git repo without `.mato/`

#### Step 7: Integration tests

**File**: `internal/integration/config_command_test.go` (new)

Cover:

- `mato config` in a real repo
- JSON output
- env overrides
- `.mato.yml` path reporting
- ambiguity error when both config files exist
- operation in a repo with no `.mato/` directory

#### Step 8: Documentation

**Files**: `README.md`, `docs/configuration.md`, `docs/architecture.md`,
`AGENTS.md`

Document:

- the new command
- expected output behavior
- env var naming in text output
- the fact that it reports repo defaults rather than one-shot CLI overrides
- the new `internal/configresolve/` package in the project layout

## 5. File Changes

Create:

```text
internal/configresolve/configresolve.go
internal/configresolve/configresolve_test.go
internal/integration/config_command_test.go
```

Modify:

```text
cmd/mato/commands.go
cmd/mato/root.go
cmd/mato/run.go
cmd/mato/resolve.go
cmd/mato/main_test.go
internal/config/config.go
internal/config/config_test.go
README.md
docs/configuration.md
docs/architecture.md
AGENTS.md
```

## 6. Error Handling

- Invalid `--format` -> usage error.
- Invalid repo path -> existing repo validation error.
- Repo root resolution failure -> existing wrapped error.
- Malformed config file -> fatal error.
- Both config files present -> fatal ambiguity error.
- Invalid env bool/duration -> fatal error, matching current behavior.
- Invalid branch from env/config/flag -> existing branch validation error.

All new errors should follow the repo's normal style: lowercase verb phrase,
wrapped with `%w` where appropriate.

## 7. Testing Strategy

### `internal/config/config_test.go`

- `TestLoad_YAML`
- `TestLoad_YML`
- `TestLoad_None`
- `TestLoad_BothFiles`
- existing parse/normalization coverage remains intact

### `internal/configresolve/configresolve_test.go`

- defaults only -> `SourceDefault`
- repo config present -> `SourceConfig`
- env overrides -> `SourceEnv` plus `EnvVar`
- branch resolution with flag override -> `SourceFlag`
- invalid env bool -> error
- invalid env duration -> error
- invalid config duration -> error
- `ResolveRepoDefaults` reports config path correctly
- `ResolveRepoDefaults` reports actual default `agent_timeout`
- `ResolveRepoDefaults` reports actual default `retry_cooldown`
- `ResolveRepoDefaults` formats duration values from `RunConfig` rather than
  resolving them independently
- `RunConfig.RunOptions()` preserves resolved values correctly

### `cmd/mato/main_test.go`

- `mato config` registration
- `mato config --format json`
- invalid `--format`
- delegation through `configShowFn`
- shared resolver still drives `run` and `init`

### `internal/integration/config_command_test.go`

- command works in a real git repo
- env override reflected in output
- `.mato.yml` surfaced correctly
- both config files produce the documented hard error
- command works without `.mato/`

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Output drifts from actual `run` behavior | Extract and migrate shared resolution before adding the command. |
| Provenance becomes inaccurate | Return source attribution from the resolver API, not as renderer-only logic. |
| Defaults drift from runtime behavior | Resolve actual built-in defaults in `configresolve`, including timeout and retry cooldown defaults currently applied downstream. |
| Config discovery diverges from current behavior | Change `internal/config.Load(...)` itself rather than reimplementing file discovery in the command. |
| `mato config` scope grows too fast | Keep phase 1 read-only and defer `config init` or editing commands. |
| Large single-PR blast radius | Split rollout into extraction/migration first, command second. |

## 9. Why this plan is worth doing

The command itself closes a real operator gap, and the prerequisite extraction
also improves the codebase by consolidating config resolution into a reusable,
testable internal package instead of leaving it embedded in package `main`.
