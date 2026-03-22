# `mato doctor` — Implementation Plan

## Summary

Add a `mato doctor` command that performs structured health checks across repo,
runtime, queue, and dependency state, then renders a text or JSON report with a
machine-usable exit code.

The first version should prioritize accurate diagnosis and conservative repair
boundaries over ambitious automation.

Estimated effort: ~2-3 days.

## Goals

- Give users one command to diagnose common setup and queue-state problems.
- Reuse existing queue/index logic where possible.
- Support CI-friendly exit codes and JSON output.
- Keep `--fix` narrow, explicit, and safe enough to trust.

## Non-Goals

- Not a live dashboard.
- Not a full queue reconciler.
- Not a replacement for `mato status`.
- Not a "fix everything automatically" command.

## Command Shape

Implement `doctor` as a normal Cobra subcommand, similar to `status`.

This should **not** be bolted onto the root command's custom flag parsing in
`cmd/mato/main.go`, because that parsing exists only to forward arbitrary flags
to the Copilot CLI for `mato run`.

### CLI

```text
mato doctor [flags]

Flags:
  --repo <path>        Path to repository root or working directory (default: current directory)
  --tasks-dir <path>   Path to tasks directory (default: <repo>/.tasks)
  --fix                Apply a narrow set of safe repairs
  --format text|json   Output format (default: text)
  --only <name>        Run only specific checks; repeatable
```

Valid `--only` values should be validated up front and rejected if unknown.

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | No remaining warnings or errors |
| `1` | Warnings remain, no errors |
| `2` | One or more errors remain |

The command should return a health-oriented exit status without wrapping normal
unhealthy output in `mato error:`.

## Report Model

The JSON and text renderers should share one structured report model.

```go
type Severity string

const (
    SeverityInfo    Severity = "info"
    SeverityWarning Severity = "warning"
    SeverityError   Severity = "error"
)

type Finding struct {
    Code     string   `json:"code"`
    Severity Severity `json:"severity"`
    Message  string   `json:"message"`
    Path     string   `json:"path,omitempty"`
    Fixable  bool     `json:"fixable,omitempty"`
    Fixed    bool     `json:"fixed,omitempty"`
}

type CheckReport struct {
    Name     string    `json:"name"`
    Findings []Finding `json:"findings"`
}

type Summary struct {
    Warnings int `json:"warnings"`
    Errors   int `json:"errors"`
    Fixed    int `json:"fixed"`
}

type Report struct {
    RepoInput string        `json:"repo_input"`
    RepoRoot  string        `json:"repo_root,omitempty"`
    TasksDir  string        `json:"tasks_dir"`
    Checks    []CheckReport `json:"checks"`
    Summary   Summary       `json:"summary"`
    ExitCode  int           `json:"exit_code"`
}
```

### Rendering rule

Avoid a separate category status enum like `[FIXED]` that conflicts with
severity ordering. A category can contain:

- fixed findings,
- remaining warnings,
- and remaining errors.

So text rendering should do one of these:

1. show worst remaining severity for the category and annotate individual fixed
   findings, or
2. show a category suffix like `(1 fixed)` without inventing a new category
   state.

Recommendation: use the first option.

## Check Categories

The initial command should run these categories:

1. `git`
2. `tools`
3. `docker`
4. `queue`
5. `tasks`
6. `locks`
7. `deps`

They should run sequentially and share lazily computed context where possible.

## Core Design Decisions

### 1. Keep repo resolution diagnostic, not fatal

Do not resolve `--repo` to git top-level before doctor starts.

Why:

- `mato doctor --repo /not-a-repo` should produce a git finding, not a command
  error.
- Other checks may still be meaningful even if the path is not a git repo.

Recommended flow:

- treat `--repo` or cwd as the raw input path,
- run git checks against that path,
- store resolved top-level in the report only if the git check succeeds.

### 2. Reuse one `PollIndex`

Checks that need queue/task state should share a single `queue.BuildIndex()`
result rather than re-parsing files independently.

That index already provides:

- parsed task snapshots,
- parse failures,
- queue-state organization,
- completed/non-completed/all IDs,
- and overlap-related state.

This keeps doctor aligned with runtime behavior.

### 3. Fix mode stays narrow

`--fix` should only cover repairs that are clearly bounded and have obvious
intent:

- create missing queue/messaging directories,
- remove stale lock files,
- recover orphaned in-progress tasks.

Everything else stays read-only.

## Check Details

### A. Git (`git`)

Checks:

- input path exists and is readable,
- `git rev-parse --show-toplevel` succeeds,
- resolved repo root,
- current branch or detached HEAD.

Severity:

- not a repo -> `error`
- other successful facts -> `info`

### B. Host Tools (`tools`)

The report should reflect what `mato run` actually needs, not only what is easy
to probe.

Required checks:

- `copilot`
- `git`
- `git-upload-pack`
- `git-receive-pack`
- `gh`

Optional checks:

- git templates dir
- system certs dir
- gh config dir
- `~/.copilot` presence, because the runner mounts it directly in
  `internal/runner/config.go`

Refactor `internal/runner/tools.go` to expose a structured inspection helper so
doctor can report all findings without duplicating runner logic.

### C. Docker (`docker`)

Checks:

- `docker` CLI exists,
- `docker info` succeeds within a 5-second timeout.

Use a function variable hook for test injection.

### D. Queue Layout (`queue`)

Verify the full expected queue and messaging layout, not just the top-level
`messages/` directory.

Expected directories:

- the 7 queue-state directories,
- `.locks/`,
- `messages/events/`,
- `messages/presence/`,
- `messages/completions/`.

If `--fix` is enabled, missing directories can be created with `os.MkdirAll`.

### E. Task Parsing (`tasks`)

Use `queue.BuildIndex()` and its parse-failure reporting rather than manually
calling `frontmatter.ParseTaskFile()` for every task a second time.

Checks:

- any parse failures across queue directories -> `error`
- total parsed task count -> `info`

### F. Locks and Orphans (`locks`)

Checks:

- stale agent `.pid` files,
- stale review lock files,
- orphaned in-progress tasks,
- stale in-progress duplicate when the task already advanced.

Important nuance:

`queue.RecoverOrphanedTasks()` is not a harmless cleanup. It moves tasks and
appends failure records, which affects retry history. That is acceptable for
`--fix`, but it must be described as a state-changing repair, not a cosmetic
cleanup.

### G. Dependency Integrity (`deps`)

Use a shared read-only dependency diagnostic helper in `internal/queue/` so
doctor, reconcile, and status can agree.

Checks:

- ambiguous IDs,
- self-dependencies,
- cycles,
- unknown dependency references,
- promotable waiting task count.

This plan should not depend on "use `internal/dag/` if it exists at build time."
That is not a realistic Go integration strategy. Instead:

- implement diagnostics against current queue behavior now, and
- refactor to reuse `internal/dag` later when that package actually lands.

## `--fix` Behavior

### Fixable in v1

| Repair | Notes |
|--------|-------|
| Create missing queue/messaging directories | Idempotent |
| Remove stale agent lock files | Idempotent |
| Remove stale review lock files | Idempotent |
| Recover orphaned tasks | State-changing but bounded |

### Not fixable in v1

- malformed YAML,
- missing tools,
- Docker unavailable,
- dependency cycles,
- unknown dependency IDs,
- ambiguous IDs.

### Reporting requirement

Current queue repair helpers mostly print to stdout/stderr and do not return a
structured list of what they changed. That is not good enough for precise
doctor reporting.

So the plan should include one of these refactors:

1. add doctor-specific scan-and-repair helpers that return structured results,
   or
2. refactor existing queue repair helpers to return structured actions/errors.

Without that, `--format json` plus `--fix` will be hard to report accurately.

## Package Structure

### Files to create

| File | Purpose |
|------|---------|
| `internal/doctor/doctor.go` | Report model, options, orchestration |
| `internal/doctor/checks.go` | Check implementations and shared context |
| `internal/doctor/render.go` | Text and JSON rendering |
| `internal/doctor/doctor_test.go` | Command/report/check tests |
| `internal/queue/diagnostics.go` | Shared dependency diagnostics |
| `internal/queue/diagnostics_test.go` | Dependency diagnostic tests |

### Files to modify

| File | Change |
|------|--------|
| `cmd/mato/main.go` | Add `newDoctorCmd()` as a normal subcommand |
| `internal/runner/tools.go` | Add structured host-tool inspection |
| `internal/queue/queue.go` and/or `internal/queue/locks.go` | Add structured repair reporting or doctor-specific helpers |
| `README.md` | Document `mato doctor` |
| `docs/configuration.md` | Document command behavior and JSON output |

## Internal Orchestration Shape

```go
type Options struct {
    Fix    bool
    Format string
    Only   []string
}

type checkContext struct {
    repoInput string
    repoRoot  string
    tasksDir  string
    opts      Options
    idx       *queue.PollIndex
}

type checkDef struct {
    name string
    run  func(*checkContext) CheckReport
}
```

The context can lazily build and cache `PollIndex` the first time a queue-based
check needs it.

## Test Plan

### Unit tests (`internal/doctor/doctor_test.go`)

| Test | Coverage |
|------|----------|
| `TestDoctor_HealthyRepo` | No findings beyond info |
| `TestDoctor_NotAGitRepo` | Git error becomes report finding, not command failure |
| `TestDoctor_MissingQueueDirs` | Missing directories reported; `--fix` creates them |
| `TestDoctor_MalformedTask` | Parse failure surfaced from `PollIndex` |
| `TestDoctor_OnlyFilter` | Valid subset runs |
| `TestDoctor_OnlyFilter_InvalidName` | Unknown check name rejected |
| `TestDoctor_DockerTimeout` | Timeout path covered |
| `TestDoctor_JSONIncludesExitCode` | JSON shape is explicit |
| `TestDoctor_FixReporting` | Fixed findings recorded without corrupting severity logic |

### Dependency diagnostics tests (`internal/queue/diagnostics_test.go`)

| Test | Coverage |
|------|----------|
| `TestDiagnoseDependencies_AmbiguousID` | Completed + non-completed collision |
| `TestDiagnoseDependencies_SelfDependency` | Self-dep |
| `TestDiagnoseDependencies_Cycle` | Waiting-task cycle |
| `TestDiagnoseDependencies_UnknownDependency` | Missing reference |
| `TestDiagnoseDependencies_PromotableCount` | Matches current reconcile behavior |

### Integration tests (`internal/integration/doctor_test.go`)

| Test | Coverage |
|------|----------|
| `TestDoctor_FixStaleLocks` | Removes stale locks |
| `TestDoctor_FixOrphanedTask` | Repairs orphaned in-progress task and reports it |
| `TestDoctor_JSONWithFix` | Valid JSON output even when fixes run |

## Implementation Order

1. Add `newDoctorCmd()` as a normal Cobra subcommand.
2. Create `internal/doctor` report model and renderer.
3. Add structured host-tool inspection in `internal/runner/tools.go`.
4. Add queue/dependency diagnostics helpers that reuse `queue.BuildIndex()`.
5. Add or refactor structured repair helpers for locks/orphans.
6. Implement `git`, `tools`, `docker`, `queue`, `tasks`, `locks`, and `deps`
   checks.
7. Add JSON output and exit-code wiring.
8. Add tests.
9. Update docs.

## Deferred Follow-Ups

1. **`--thorough` mode** for slower checks like image pullability or remote API
   access.
2. **Watch mode** if there is future demand, though that may overlap too much
   with `status --watch`.
3. **Deeper DAG-backed dependency diagnostics** once `internal/dag` exists and
   has stabilized.
