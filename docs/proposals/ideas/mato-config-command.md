# `mato config` - Implementation Plan

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
- why are these model, timeout, and Docker defaults in effect?

This proposal adds that visibility while also cleaning up the current config
resolution architecture so one shared internal package owns precedence logic and
source attribution.

## 2. Scope

### In scope

- New top-level `mato config` command group.
- Phase 1: read-only `mato config show` behavior.
- New shared internal config-resolution package extracted out of `cmd/mato/`.
- Explicit source attribution for resolved settings.
- Text and JSON output.
- Preservation of current config discovery behavior:
  - load from repo root
  - accept `.mato.yaml` and `.mato.yml`
  - error when both exist
- Migration of existing command-layer resolution to the shared package.
- Unit tests, command tests, and integration coverage.
- Documentation updates.

### Out of scope

- Editing `.mato.yaml` in place.
- Global config files.
- Interactive config editing prompts.
- `mato config init`, `set`, `unset`, or `edit` subcommands.
- Simulating every possible one-shot invocation flag combination.
- Changes to runner behavior beyond consuming the same resolved values it uses
  today.

## 3. Design

### CLI shape

Phase 1 should use a small nested command tree:

```text
mato config [--repo <path>] [--format text|json]
mato config show [--repo <path>] [--format text|json]
```

Behavior:

- `mato config` defaults to `show`.
- `mato config show` is the explicit form.
- `--repo` follows the existing repo-aware command pattern.
- `--format` supports `text` and `json`.

Use a real `config` command with a `show` subcommand from day one, even though
`show` is the only implemented action initially. The parent `config` command
should delegate to `show` when invoked without a subcommand. This preserves a
natural home for future commands such as `config init` without requiring a later
breaking reshape of the CLI.

The command should report the effective repository defaults mato would use in
the absence of one-shot subcommand flags. That means it should show env var,
config file, and built-in default resolution, but not pretend that ad hoc flags
such as `mato run --branch foo` are part of standing repo configuration.

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
internal package. `mato config show` should be implemented on top of that same
shared package, and existing commands should be migrated to it so precedence and
validation logic stay in one place.

### New package: `internal/configresolve/`

Create a new package that owns repo-local config resolution and provenance.

Use the name `configresolve` as-is. The package has a narrow, concrete purpose:
resolve effective config values plus their sources.

Responsibilities:

- load repo config metadata using the same discovery rules as today
- resolve effective values using the current precedence chain
- expose the source of each resolved value
- provide helpers reusable by `run`, `init`, `doctor`, and `mato config show`

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

Suggested resolved-value types:

```go
type ResolvedString struct {
    Value  string `json:"value"`
    Source Source `json:"source"`
}

type ResolvedBool struct {
    Value  bool   `json:"value"`
    Source Source `json:"source"`
}

type ResolvedDuration struct {
    Value  string `json:"value"`
    Source Source `json:"source"`
}
```

The exact type layout can be tuned during implementation, but provenance must
be returned as data rather than inferred later.

### Config loading metadata

The current `internal/config.Load(dir)` API returns only `Config`, which is not
enough for `mato config show` because the command must report:

- whether a config file exists
- which path was loaded
- the current ambiguity error when both `.mato.yaml` and `.mato.yml` exist

Extend `internal/config/` with a metadata-returning API, for example:

```go
type LoadResult struct {
    Config Config
    Path   string
    Exists bool
}

func LoadWithMeta(dir string) (LoadResult, error)
```

`Load(dir)` can remain as a thin compatibility wrapper around `LoadWithMeta`
that returns only the `Config`.

### Shared resolved view

The new resolver should expose a stable model for repo defaults.

Suggested shape:

```go
type RepoDefaults struct {
    RepoRoot     string `json:"repo_root"`
    ConfigPath   string `json:"config_path,omitempty"`
    ConfigExists bool   `json:"config_exists"`

    Branch                     ResolvedString   `json:"branch"`
    DockerImage                ResolvedString   `json:"docker_image"`
    TaskModel                  ResolvedString   `json:"task_model"`
    ReviewModel                ResolvedString   `json:"review_model"`
    ReviewSessionResumeEnabled ResolvedBool     `json:"review_session_resume_enabled"`
    TaskReasoningEffort        ResolvedString   `json:"task_reasoning_effort"`
    ReviewReasoningEffort      ResolvedString   `json:"review_reasoning_effort"`
    AgentTimeout               ResolvedDuration `json:"agent_timeout"`
    RetryCooldown              ResolvedDuration `json:"retry_cooldown"`
}
```

This is the model `mato config show` should render in text or JSON.

Use this top-level JSON shape directly rather than wrapping fields in a nested
`settings` object. The command has a fixed, narrow field set, so a flat JSON
structure is simpler for humans and easier to consume in shell tooling.

For text rendering, use a fixed explicit ordered field list rather than ad hoc
hand-written print statements spread through the renderer. That keeps output
deterministic while still making the display order an intentional product
choice. Recommended text order:

1. `branch`
2. `docker_image`
3. `task_model`
4. `review_model`
5. `review_session_resume_enabled`
6. `task_reasoning_effort`
7. `review_reasoning_effort`
8. `agent_timeout`
9. `retry_cooldown`

### Resolver API boundaries

Recommended package surface:

```go
// Load repo config metadata and resolve standing repo defaults.
func ResolveRepoDefaults(repoRoot string) (*RepoDefaults, error)

// Resolve branch for commands that accept a flag override.
func ResolveBranch(fileCfg config.Config, flagValue string) (ResolvedString, error)

// Resolve runner options for commands that accept flag overrides.
func ResolveRunOptions(flags RunFlags, fileCfg config.Config) (ResolvedRunOptions, error)
```

`ResolvedRunOptions` should carry both the concrete `runner.RunOptions` value
and provenance metadata for every config-backed field in `runner.RunOptions`,
not only the subset currently shown by `mato config`. That keeps the resolver
complete and avoids having provenance become a partial, command-specific side
channel.

The command layer should keep command-specific validation such as mutually
exclusive flags, but config-source precedence and parsing should move out of
`cmd/mato/resolve.go`.

### `mato config show` output contract

Text output should optimize for fast debugging.

Example:

```text
Repo: /work/repo
Config file: /work/repo/.mato.yml

branch: main                         (config)
docker_image: ghcr.io/acme/mato:dev  (env)
task_model: claude-opus-4.6          (default)
review_model: gpt-5.4                (config)
review_session_resume_enabled: true  (default)
task_reasoning_effort: high          (config)
review_reasoning_effort: high        (config)
agent_timeout: 45m0s                 (config)
retry_cooldown: 2m0s                 (default)
```

If no config file exists:

```text
Repo: /work/repo
Config file: none
```

JSON output should expose the same facts in structured form and preserve source
attribution. Durations should be encoded as strings so the output stays stable
and human-readable.

### Existing command migration

This proposal should migrate current command-layer resolution to the new shared
package in a focused way:

- `mato run`
  - use shared branch resolution
  - use shared run-options resolution
- `mato init`
  - use shared branch resolution
- `mato doctor`
  - use the shared resolver only for Docker-image and config-loading behavior
  - do not broaden this into a larger doctor refactor

Other helpers in `cmd/mato/resolve.go` that are unrelated to config resolution,
such as repo path and git validation, can stay where they are.

### Error handling

- Invalid `--format` remains a usage error handled in the command layer.
- Repo path validation remains unchanged.
- Config parse errors remain fatal.
- `.mato.yaml` / `.mato.yml` ambiguity remains fatal.
- Invalid env var bool/duration values should continue returning clear errors,
  matching current behavior.
- `mato config show` remains read-only.

## 4. Step-by-Step Breakdown

### Step 1: Extend `internal/config/` with metadata loading

**Files**: `internal/config/config.go`, `internal/config/config_test.go`

Add `LoadWithMeta(dir string) (LoadResult, error)` while preserving `Load(dir)`
as a convenience wrapper. Tests should cover `.mato.yaml`, `.mato.yml`, neither,
and both.

### Step 2: Create `internal/configresolve/`

**Files**: `internal/configresolve/configresolve.go`,
`internal/configresolve/configresolve_test.go`

Add:

- source enum
- resolved value types
- repo-default resolution
- branch resolution with provenance
- run-option resolution with provenance and concrete `runner.RunOptions`

This package should absorb the current precedence and parsing logic from
`cmd/mato/resolve.go`.

### Step 3: Migrate command-layer config resolution

**Files**: `cmd/mato/resolve.go`, `cmd/mato/run.go`, `cmd/mato/commands.go`

Replace direct use of config-resolution helpers in package `main` with calls to
`internal/configresolve/`.

Expected changes:

- `newRunCmd()` uses shared branch and run-option resolution.
- `newInitCmd()` uses shared branch resolution.
- `newDoctorCmd()` resolves Docker image/config loading through the shared
  package instead of its current special-case branch.

Leave repo/path/git validation helpers in `cmd/mato/resolve.go` untouched.

### Step 4: Add `mato config` command

**Files**: `cmd/mato/commands.go`, `cmd/mato/root.go`

Add `newConfigCmd(repoFlag *string)` with a nested `show` subcommand:

- parent `Use: "config"`
- child `Use: "show"`
- parent `RunE` delegates to the same implementation as `show`
- parent uses the existing inherited `--repo` flag from root
- child owns `--format text|json`
- delegation into a package-level seam such as `configShowFn`

Keep the implementation minimal, but use the nested structure now so follow-up
commands can slot in without later CLI churn.

### Step 5: Add rendering package or keep rendering local

**Files**: `internal/configresolve/`

Keep the renderer in `internal/configresolve/` for the first implementation.
The command has one narrow view and a fixed field set, so introducing
`internal/configview/` now would add structure without clear value. If rendering
grows later, split it in a follow-up.

### Step 6: Command tests

**File**: `cmd/mato/main_test.go`

Add tests for:

- command registration
- `--format` validation
- repo resolution and delegation
- `mato config` defaulting to `show`
- propagation of resolver errors

### Step 7: Integration tests

**File**: `internal/integration/config_command_test.go` (new)

Cover:

- `mato config` in a real repo
- JSON output
- env overrides
- `.mato.yml` path reporting
- ambiguity error when both config files exist

### Step 8: Documentation

**Files**: `README.md`, `docs/configuration.md`, `docs/architecture.md`,
`AGENTS.md`

Document:

- the new command
- expected output behavior
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

- `TestLoadWithMeta_YAML`
- `TestLoadWithMeta_YML`
- `TestLoadWithMeta_None`
- `TestLoadWithMeta_BothFiles`
- existing parse/normalization coverage remains intact

### `internal/configresolve/configresolve_test.go`

- defaults only -> `SourceDefault`
- repo config present -> `SourceConfig`
- env overrides -> `SourceEnv`
- branch resolution with flag override -> `SourceFlag`
- invalid env bool -> error
- invalid env duration -> error
- invalid config duration -> error
- `ResolveRepoDefaults` reports config path correctly

### `cmd/mato/main_test.go`

- `mato config` registration
- `mato config --format json`
- `mato config show --format json`
- invalid `--format`
- delegation through `configShowFn`
- shared resolver still drives `run` and `init`

### `internal/integration/config_command_test.go`

- command works in a real git repo
- env override reflected in output
- `.mato.yml` surfaced correctly
- both config files produce the documented hard error

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Output drifts from actual `run` behavior | Migrate existing commands to the shared resolver instead of adding a parallel path. |
| Provenance becomes inaccurate | Return source attribution from the resolver API, not as renderer-only logic. |
| Config discovery diverges from current behavior | Extend `internal/config` rather than reimplementing file discovery in the command. |
| `mato config` scope grows too fast | Keep phase 1 read-only and defer `config init` or editing commands. |
| Migration causes regressions in `run` or `doctor` | Keep tests around existing resolution behavior and add focused integration coverage. |

## 9. Why this plan is worth doing

The command itself closes a real operator gap, and the prerequisite extraction
also improves the codebase by consolidating config resolution into a reusable,
testable internal package instead of leaving it embedded in package `main`.
