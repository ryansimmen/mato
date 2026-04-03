# `mato doctor` Config Validation

## 1. Problem

`mato doctor` currently validates Docker-related config only as a side effect of
resolving the Docker image for the `docker` check.

That leaves an awkward gap:

- `mato config` can show effective settings, but it is not framed as a health
  check
- `mato doctor` is the natural place to surface invalid config, but today it is
  intentionally narrow
- broadening Docker-image resolution to validate every run setting would change
  existing `doctor` behavior in surprising ways, especially for queue-focused
  runs

Examples of settings operators may want `doctor` to flag explicitly:

- invalid branch names
- invalid reasoning-effort values
- invalid duration values such as `agent_timeout` or `retry_cooldown`
- malformed env-backed booleans such as
  `MATO_REVIEW_SESSION_RESUME_ENABLED=maybe`

## 2. Goal

Add an explicit config-validation path to `mato doctor` so invalid effective
repo settings are reported deliberately, without coupling unrelated checks to
full run-config resolution.

## 3. Non-Goals

- Do not make the `docker` check responsible for validating all config.
- Do not make queue-only runs such as `mato doctor --only queue,tasks,deps`
  fail on unrelated config issues.
- Do not add config editing or mutation commands.
- Do not duplicate precedence logic outside `internal/configresolve`.

## 4. Proposed UX

Introduce a new `config` doctor check.

Default behavior:

- `mato doctor` includes the `config` check in normal full runs.
- `mato doctor --only config` runs config validation by itself.
- `mato doctor --only docker` continues validating only Docker-relevant config.
- `mato doctor --only queue,tasks,deps` remains free of unrelated config
  validation.

This keeps `doctor` behavior explicit and composable.

## 5. Design

### 5.1 Reuse `internal/configresolve`

The `config` check should reuse the shared config-resolution package introduced
for `mato config`.

That package already knows:

- config-file discovery rules
- env-var precedence
- built-in defaults
- parsing rules for bools and durations

The doctor config check should build on those helpers rather than re-encoding
validation logic in `internal/doctor`.

### 5.2 Separate validation surfaces

Split config validation into two layers:

1. Docker-only resolution for the `docker` check.
2. Full effective-config validation for the new `config` check.

That means:

- `ResolveDoctorDockerImage(...)` stays narrow
- a new helper such as `ValidateRepoConfig(...)` or
  `ResolveDoctorConfig(...)` can validate all effective settings with source
  attribution

### 5.3 What the `config` check validates

The check should validate the effective values mato would actually use after
flag/env/config/default resolution, excluding one-shot command flags that are
not part of standing repo defaults.

Suggested fields:

- `branch`
- `docker_image`
- `task_model`
- `review_model`
- `review_session_resume_enabled`
- `task_reasoning_effort`
- `review_reasoning_effort`
- `agent_timeout`
- `retry_cooldown`

Validation rules:

- branch must pass existing branch-name validation
- reasoning effort must be one of `low`, `medium`, `high`, `xhigh`
- durations must parse and be positive
- env bools must parse cleanly
- config-load ambiguity and parse errors remain fatal to the check

### 5.4 Reporting model

The `config` check should emit findings with clear attribution, for example:

- `config.invalid_branch`
- `config.invalid_duration`
- `config.invalid_bool`
- `config.parse_error`

Where useful, findings should include:

- setting name
- effective source (`env`, `config`, or `default`)
- env var name when applicable
- config file path when applicable

Example text output:

```text
[ERROR] config
  - invalid branch "foo..bar" from .mato.yaml
  - invalid MATO_AGENT_TIMEOUT "bad": time: invalid duration
```

## 6. CLI and Output Changes

Add `config` to the documented valid `--only` check names:

```text
mato doctor --only config
mato doctor --only config,docker
```

JSON output should include a normal doctor check entry named `config` so the
schema remains stable.

## 7. Implementation Notes

Likely changes:

- `internal/doctor/`:
  - add `config` to valid check names
  - add a `checkConfig(...)`
- `internal/configresolve/`:
  - expose a validation-oriented helper for effective repo defaults
  - keep Docker-only resolution separate
- `cmd/mato/commands.go`:
  - wire updated `--only` help text if needed
- tests and docs

## 8. Tests

Add coverage for at least:

- `mato doctor` reporting invalid branch from `.mato.yaml`
- invalid env bool and duration values in the `config` check
- `mato doctor --only docker` not failing on unrelated invalid settings
- `mato doctor --only queue,tasks,deps` remaining free of config coupling
- JSON output including the `config` check
- clear source attribution in findings when env vars are responsible

## 9. Why This Is Worth Doing

This makes config validation an explicit doctor capability instead of an
incidental side effect. It gives operators one clear health-check surface for
bad settings while preserving the current narrow behavior of queue-only and
Docker-only doctor runs.
