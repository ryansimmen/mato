# `mato doctor` Config Validation — Implementation Plan

## Summary

Add an explicit `config` check to `mato doctor` that validates effective
repository defaults using the same precedence and parsing rules as `mato config`
and `mato run`, without widening the existing Docker-only behavior of the
`docker` check.

Estimated effort: ~1 day.

## Problem

`mato doctor` currently validates Docker-related config only as a side effect of
resolving the Docker image for the `docker` check.

That leaves a gap:

- `mato config` can show effective settings, but it is not a health check
- `mato doctor` is the natural place to surface invalid config, but today it is
  intentionally narrow
- broadening Docker-image resolution to validate every run setting would change
  existing `doctor` behavior in surprising ways, especially for queue-focused
  runs such as `mato doctor --only queue,tasks,deps`

Examples operators may want `doctor` to flag explicitly:

- invalid branch names
- invalid reasoning-effort values
- invalid duration values such as `agent_timeout` or `retry_cooldown`
- malformed env-backed booleans such as
  `MATO_REVIEW_SESSION_RESUME_ENABLED=maybe`

## Goals

- Add a first-class `config` health check to `mato doctor`
- Reuse `internal/configresolve` for precedence, parsing, and source attribution
- Keep Docker-only doctor runs narrow
- Keep queue-only doctor runs free of unrelated config coupling
- Preserve stable doctor JSON structure by adding `config` as a normal check

## Non-Goals

- Do not make the `docker` check responsible for validating all config
- Do not make queue-only runs such as `mato doctor --only queue,tasks,deps`
  fail on unrelated config issues
- Do not add config editing or mutation commands
- Do not duplicate precedence logic outside `internal/configresolve`

## Current State

### Existing `doctor` flow

- `cmd/mato/commands.go` currently calls `validateRepoPath(repoInput)` before
  `doctor.Run()`, so non-repo inputs fail in Cobra setup instead of surfacing as
  doctor findings.
- `cmd/mato/commands.go` pre-resolves Docker config only when the selected check
  set includes `docker` or when all checks run via `doctorNeedsDockerConfig()`.
- That setup calls `configresolve.ResolveDoctorDockerImage(repoRoot)`, which is
  intentionally narrow and already ignores unrelated invalid run settings.
- In default full runs, that pre-resolution still means malformed config can
  abort the command before a `config` check has a chance to report it.
- `internal/doctor/checks.go` defines the ordered check list and the valid
  `--only` names.
- `internal/doctor/doctor.go` eagerly resolves repo root for task-dir derivation,
  then runs only the selected checks and reports skipped checks explicitly.

### Existing config-resolution behavior

- `internal/configresolve.ResolveRepoDefaults(repoRoot)` already loads repo
  config, resolves env/config/default precedence, parses bools and durations,
  validates reasoning effort values, and returns source attribution.
- `ResolveBranch(load, flagValue)` resolves branch precedence but does not
  validate branch syntax.
- Branch validation currently lives in `cmd/mato/resolve.go` as
  `validateBranch()`, backed by `git check-ref-format --branch`.

That last point matters: the config-check implementation cannot simply “reuse
existing branch validation” from `internal/configresolve` today because that
validation is still command-local.

## Desired UX

Default behavior:

- `mato doctor` includes `config` in full runs
- `mato doctor --only config` runs config validation by itself
- `mato doctor --only config,docker` runs both explicit config validation and the
  existing Docker reachability/image checks
- `mato doctor --only docker` continues validating only Docker-relevant config
- `mato doctor --only queue,tasks,deps` remains free of unrelated config
  validation

Example text output:

```text
[ERROR] config
  - /repo/.mato.yaml: invalid branch "foo..bar" (error)
  - invalid MATO_AGENT_TIMEOUT "bad": time: invalid duration (error)
```

JSON output should include a normal doctor check entry named `config`.

## Design

### 1. Keep two validation surfaces

Maintain two separate entry points in `internal/configresolve`:

1. `ResolveDoctorDockerImage(...)` for the `docker` check only
2. A new full-config validation helper for the `config` check

This preserves current behavior:

- command setup still resolves Docker image narrowly when needed
- `docker` remains independent of unrelated config errors
- full effective-config validation happens only in the new `config` check

### 2. Let `doctor` own repo and config failures when `config` is selected

Update `cmd/mato/commands.go` so the doctor command no longer hard-fails before
the report can be rendered:

- stop using `validateRepoPath(repoInput)` in `newDoctorCmd()` for doctor runs
- continue resolving the raw repo input path, but let `doctor.Run()` and the
  selected checks report repo-health problems in-band
- replace the current `doctorNeedsDockerConfig()` behavior with a narrower
  pre-resolution helper: only pre-resolve Docker image in Cobra setup when the
  selected checks include `docker` and do not include `config`

That yields the desired ownership split:

- `mato doctor --only docker` keeps the current narrow Docker-only behavior
- `mato doctor` and `mato doctor --only config` let the `config` check own
  malformed-config reporting
- `mato doctor --only config` on a non-repo can emit `config.no_repo` instead of
  failing in Cobra setup

`--only` input order should not matter. Execution order is defined by the doctor
check registration list, not by the order of names passed to `--only`, so
`--only docker,config` and `--only config,docker` should behave identically.

When the `config` check is selected, config loading and validation should happen
inside `doctor.Run()`. If full-config validation succeeds, the resolved Docker
image should be stored in shared doctor context for `checkDocker` to reuse. A
minimal implementation is to extend `checkContext` with a cached resolved-config
result or a dedicated `dockerImage string` field populated by `checkConfig`.
`checkDocker` should prefer that cached value before falling back to
`cc.opts.DockerImage` or its current env/default fallback path. If full-config
validation fails fatally, `doctor` should still render the report and the
`config` check should be the authoritative source of that failure. In that case,
the later `docker` check should still perform Docker CLI and daemon reachability
checks, but it should skip image-availability inspection and avoid falling back
to independent env/default image resolution. This prevents misleading
`docker.image_missing` findings after a config parse/load failure.

Important distinction:

- cache and reuse the resolved effective Docker image whenever config
  discovery/loading succeeded and `docker_image` itself was resolved, even if
  other config findings remain
- only skip image-availability inspection when config load/parse failures prevent
  trustworthy Docker-image resolution

Recommended shared context fields:

- `resolvedDockerImage string`
- `dockerImageResolved bool`
- `configValidationFatal bool`

Decision table for Docker-image resolution:

| Selected checks | Config runs? | Docker runs? | Image source for `checkDocker` | Image inspection? |
| --- | --- | --- | --- | --- |
| all checks | yes | yes | cached image from `checkConfig` when resolvable | yes, unless config load/parse failed fatally |
| `config` | yes | no | none | not applicable |
| `docker` | no | yes | existing narrow Docker-only path via command setup / `cc.opts.DockerImage` | yes |
| `config,docker` | yes | yes | cached image from `checkConfig` when resolvable | yes, unless config load/parse failed fatally |
| `queue,tasks,deps` | no | no | none | not applicable |

### 3. Add a validation-oriented helper in `internal/configresolve`

Add a helper such as `ValidateRepoDefaults(repoRoot string)` that:

- loads repo-local config via `config.Load(repoRoot)`
- resolves effective values for:
  - `branch`
  - `docker_image`
  - `task_model`
  - `review_model`
  - `review_session_resume_enabled`
  - `task_reasoning_effort`
  - `review_reasoning_effort`
  - `agent_timeout`
  - `retry_cooldown`
- validates those resolved values
- returns both the resolved effective values and structured findings with source
  attribution instead of only returning the first error

For example, the helper result should look more like:

```go
type RepoValidationResult struct {
	Resolved RepoDefaults
	Issues   []ValidationIssue
}
```

The exact shape can vary, but `checkConfig` must be able to reuse the resolved
effective values, especially `docker_image`, without rerunning precedence logic.

The helper should still treat config discovery/load failures as fatal helper
errors, because those prevent meaningful effective-config validation:

- both `.mato.yaml` and `.mato.yml` present
- YAML parse failure
- unknown config keys

Those fatal helper errors should map to one `config.parse_error` finding in the
doctor layer rather than crashing the whole command.

### 4. Move branch validation out of `cmd/mato`

Extract branch validation into a reusable internal package so both command code
and config validation use the same rule.

Minimal approach:

- add a small helper package, likely `internal/git`, wrapping
  `git check-ref-format --branch`
- move the test hook currently in `cmd/mato/resolve.go` behind that shared
  helper
- update `cmd/mato` init-path validation to call the shared helper
- let `internal/configresolve` call the same helper when validating effective
  `branch`

This avoids re-implementing branch syntax rules and removes the current
command-layer-only validation trap.

### 5. Return structured validation findings

Add a small validation result type in `internal/configresolve`, for example:

```go
type ValidationIssue struct {
	Code       string
	Setting    string
	Message    string
	Source     Source
	EnvVar     string
	ConfigPath string
}
```

The exact shape can vary, but it should support doctor output like:

- `config.invalid_branch`
- `config.invalid_reasoning_effort`
- `config.invalid_duration`
- `config.invalid_bool`
- `config.parse_error`

`docker_image` itself should not gain new syntax validation here. The config
check should resolve and attribute the effective value, while image-reference
usability remains the `docker` check's responsibility through Docker probing.

The helper should carry enough metadata for the doctor check to render clear
messages without duplicating precedence logic, and it should also carry back the
resolved values needed by later doctor checks.

### 6. Keep doctor responsible for rendering/reporting

`internal/doctor` should translate config-validation results into normal doctor
findings:

- severity should be `error` for invalid effective config
- `Path` should be set when the issue comes from a config file
- message text should not repeat the config path when `Path` is already set
- message text should include source attribution for env/default-backed issues

Examples:

- `/repo/.mato.yaml: invalid branch "foo..bar"`
- `invalid MATO_REVIEW_SESSION_RESUME_ENABLED "maybe": must be true or false`

Defaults should generally validate cleanly, but keeping source attribution in the
result makes the helper robust if defaults evolve.

## Implementation Steps

### 1. Change doctor command setup ownership

Update:

- `cmd/mato/commands.go`

Changes:

- remove the eager git-repository validation path from `newDoctorCmd()` so doctor
  can report repo problems in-band
- replace `doctorNeedsDockerConfig()` with a helper that pre-resolves Docker
  image only for runs that need `docker` but do not run `config`
- when `config` is selected, let `doctor.Run()` own config loading and any
  resulting parse/ambiguity findings

This is the key change that makes `config.parse_error` and `config.no_repo`
reachable on the default CLI path.

### 2. Extend doctor check registration

Update:

- `internal/doctor/checks.go`
- `internal/doctor/doctor.go`

Changes:

- add `config` to the ordered `checks` list
- add `config` to `validCheckNames`
- place `config` before `docker` so full doctor runs surface configuration health
  before Docker probing
- extend shared doctor context so `checkConfig` can cache the resolved Docker
  image for `checkDocker`

Recommended order:

- `git`
- `tools`
- `config`
- `docker`
- `queue`
- `tasks`
- `locks`
- `hygiene`
- `deps`

### 3. Implement `internal/doctor/check_config.go`

Add a new check implementation that:

- returns early with a `config.no_repo` error finding if repo root is not known
  and the `git` check is not selected
- skips emitting `config.no_repo` when `git` is also selected, since repo
  resolution failure is already reported there
- calls the new `configresolve` validation helper when `cc.repoRoot` is present
- converts fatal helper errors into a `config.parse_error` finding
- converts validation issues into normal `doctor.Finding` entries
- caches the resolved effective Docker image in shared doctor context whenever
  config loading succeeds and `docker_image` is resolvable, even if other config
  findings exist, so the later `docker` check uses the same effective value

This check should be read-only and should not participate in `--fix`. No config
finding should ever be marked `Fixable: true`.

### 4. Add shared branch validation

Refactor branch validation out of `cmd/mato/resolve.go` into a shared internal
package function, then update:

- `cmd/mato/resolve.go`
- any branch-validation tests in `cmd/mato/main_test.go`
- new configresolve tests

The existing behavior and error wording should remain effectively the same so
CLI behavior does not regress.

### 5. Add config-validation helper in `internal/configresolve`

Implement the validation helper with these rules:

- branch: resolve with `ResolveBranch(...)`, then validate using the shared
  branch validator
- reasoning effort: reuse current allowed set `low|medium|high|xhigh`
- durations: parse and require positive values for both `agent_timeout` and
  `retry_cooldown`
- env bools: parse accepted true/false spellings and reject malformed values
- config-load ambiguity and YAML/unknown-key parse failures: return fatal helper
  error

Important detail:

- the helper should accumulate multiple validation issues when possible instead
  of stopping after the first invalid setting

That is a change from `ResolveRunConfig(...)`, which currently returns on the
first bool/duration/reasoning-effort failure. The new helper should preserve the
existing resolution logic but collect issues so `mato doctor` can show all bad
effective settings in one run.

### 6. Preserve narrow Docker resolution semantics

Keep Docker resolution behavior narrow even though command setup changes:

- `mato doctor --only docker` should still use narrow Docker-image resolution and
  should not validate unrelated config
- `mato doctor --only config` should not pre-resolve Docker image in command
  setup at all
- full runs should let the `config` check resolve the effective Docker image and
  share it with `docker` whenever config loading succeeds and `docker_image` is
  resolvable, even if other config findings remain
- `ResolveDoctorDockerImage(...)` should remain unchanged in scope
- `checkDocker` should read the cached resolved Docker image from shared doctor
  context in full runs before consulting `cc.opts.DockerImage`
- when full-run config validation fails before caching an image, `checkDocker`
  should still verify Docker CLI/daemon reachability but should skip
  image-availability inspection rather than falling back to unrelated
  env/default resolution

The new config check owns full effective-config validation inside `doctor.Run`;
Docker-only runs keep the existing narrow resolution path.

### 7. Update command help and docs

Update:

- `cmd/mato/commands.go`
- `docs/configuration.md`
- `README.md`
- optionally `docs/architecture.md` if the queue-only preflight description needs
  to mention the explicit `config` split

Changes:

- add `config` to documented valid `--only` names
- document `mato doctor --only config`
- clarify that queue-only runs skip unrelated config validation while full runs
  include it
- clarify that `docker` remains Docker-config-only

## Testing Plan

### `internal/configresolve`

Add unit tests covering:

- valid effective config produces no validation issues
- invalid branch from `.mato.yaml`
- invalid branch from `MATO_BRANCH`
- invalid env bool for `MATO_REVIEW_SESSION_RESUME_ENABLED`
- invalid env duration for `MATO_AGENT_TIMEOUT`
- invalid config duration for `agent_timeout`
- invalid env or config duration for `retry_cooldown`
- invalid task/review reasoning effort values
- multiple invalid settings reported together in one validation result
- fatal ambiguity/parse errors returned when both config filenames exist or YAML
  is malformed
- `ResolveDoctorDockerImage()` still ignores unrelated invalid settings

### `internal/doctor`

Add unit tests covering:

- valid config produces no `config` error findings
- full `Run()` includes `config` in the check list
- `--only config` runs only `config`
- invalid branch from `.mato.yaml` appears as `config.invalid_branch`
- invalid env bool and duration appear with env-var attribution
- in a full run, `docker` uses the effective Docker image cached by `config`
  rather than re-resolving independently
- in a full run with non-fatal config findings unrelated to `docker_image`,
  `docker` still performs image-availability checks against the cached effective
  Docker image
- in a full run with fatal config parse failure, `docker` still reports CLI or
  daemon health but does not emit image-availability findings
- JSON output includes a `config` check entry
- bad repo with `--only config` reports a config-specific error finding rather
  than crashing
- bad repo in a full run does not duplicate both `git.not_a_repo` and
  `config.no_repo`
- fatal config parse failure in a full `mato doctor` run becomes a
  `config.parse_error` finding instead of a command error

### `cmd/mato`

Add or update tests covering:

- doctor `--only` help text now includes `config`
- `--only docker,config` and `--only config,docker` take the same resolution path
- the new Docker pre-resolution helper returns false for `[]string{"config"}`
- the new Docker pre-resolution helper returns true for
  `[]string{"docker"}` and false for full runs that include `config`
- `newDoctorCmd` no longer rejects a non-git repo before `doctor.Run()` can
  report it
- branch-validation tests still pass after extracting shared validation logic

### Integration expectations

At minimum, verify these end-to-end behaviors through existing command tests or
new integration tests:

- `mato doctor` reports invalid branch from `.mato.yaml`
- `mato doctor` with malformed `.mato.yaml` renders a report containing
  `config.parse_error` instead of failing before output
- `mato doctor --only docker` does not fail on unrelated invalid settings
- `mato doctor --only queue,tasks,deps` remains free of config coupling
- `mato doctor --format json` includes the `config` check in schema-stable output

## Risks And Mitigations

### Risk: widening Docker behavior accidentally

Mitigation:

- keep `ResolveDoctorDockerImage()` separate
- do not call full-config validation from command setup
- add explicit regression tests for `--only docker`

### Risk: duplicate branch-validation logic

Mitigation:

- move branch validation into a shared internal helper instead of copying the
  `cmd/mato` implementation

### Risk: losing multi-error visibility by reusing first-error APIs

Mitigation:

- keep existing resolution helpers for run/config commands
- add a separate aggregation-oriented validation helper for doctor

## Acceptance Criteria

- `mato doctor` includes a `config` check in normal runs
- malformed repo config in a full `mato doctor` run is reported as
  `config.parse_error` in the report rather than as a command-level failure
- `mato doctor --only config` validates effective repo defaults without Docker
  probing
- `mato doctor --only config` on a non-repo can report `config.no_repo`
- `mato doctor --only docker` continues to validate only Docker-relevant config
- queue-only doctor runs remain uncoupled from unrelated config validation
- invalid effective config is reported with source attribution in text and JSON
- branch validation is shared rather than duplicated
