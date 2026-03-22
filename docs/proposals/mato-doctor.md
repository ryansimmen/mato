# `mato doctor` — Implementation Plan

## Summary

Add a `mato doctor` diagnostic command that inspects system health across 7
categories (git, host tools, Docker, queue layout, task parsing, locks/orphans,
dependency integrity) and reports findings with `[OK]`/`[WARN]`/`[ERROR]`
indicators. An optional `--fix` flag auto-repairs safe, idempotent issues like
missing directories, stale locks, and orphaned tasks. Exit codes (0/1/2) make
the command usable in CI pipelines and monitoring scripts.

Estimated effort: ~2 days.

## Motivation

When `mato run` fails, diagnosing the root cause requires checking multiple
subsystems manually: is Docker running? Are host tools installed? Is the queue
directory structure intact? Are there stale locks from killed agents? `mato
doctor` consolidates these checks into a single command with structured output
and optional automated repair.

## Design

The command runs 7 independent check categories in sequence. Each category
produces a `CheckReport` containing zero or more `Finding` items. The report
is rendered as text (default) or JSON, and the process exits with a code
reflecting the worst remaining severity.

Key design decisions:

- **Read-only by default.** The `--fix` flag enables safe repairs only.
  Unfixable issues (bad YAML, missing tools, Docker down, dependency cycles)
  always require human intervention.
- **Not a linter.** Normal scheduling state (unsatisfied deps, affects-based
  deferrals) is informational, not a warning. Doctor reports problems, not
  status.
- **Function variable hooks** for external probes (Docker, tool discovery) to
  enable testing without Docker or real binaries. These are package-level vars
  in `internal/doctor/checks.go`, matching the existing `claimPrependFn`
  pattern in `internal/queue/claim.go`.
- **5-second context timeout** on the Docker probe to avoid hanging UX when
  `docker info` blocks.
- **DAG proposal interaction**: if `internal/dag/` exists at build time,
  `checkDependencies` should delegate to it for cycle detection. Otherwise,
  implement simpler inline cycle detection using the existing
  `dependsOnWaitingTask` approach from `reconcile.go`. The doctor command does
  not block on the DAG proposal.

## CLI Specification

```
mato doctor [flags]

Flags:
  --repo <path>        Path to git repository (default: current directory)
  --tasks-dir <path>   Path to tasks directory (default: <repo>/.tasks)
  --fix                Auto-repair safe issues (stale locks, orphaned tasks, missing dirs)
  --format text|json   Output format (default: text)
  --only <name>        Run only specified checks (repeatable: git, tools, docker, queue, tasks, locks, deps)
```

Flags `--repo` and `--tasks-dir` mirror the existing root command and `mato
status` for consistency.

### Exit Codes

| Code | Meaning |
|------|---------|
| `0`  | Healthy — no warnings or errors remain (or all were fixed) |
| `1`  | One or more warnings remain, no errors |
| `2`  | One or more errors remain |

The doctor command exits with a health status code without printing `mato
error:` for a normal unhealthy report. The `Report` struct provides an
`ExitCode() int` method; `cmd/mato/main.go` calls `os.Exit()` with this value.

### Output Format

One-line summary header, then per-category sections. The category-level
indicator (`[OK]`/`[WARN]`/`[ERROR]`/`[FIXED]`) reflects the worst severity of
any finding in that category. Findings that were fixed show `[FIXED]` at the
category level. Fixable findings annotated with `(fixable with --fix)` when
`--fix` is not active.

```
mato doctor: 1 error, 2 warnings, 1 fixed

[OK] git
  - repo root: /repo
  - current branch: mato

[WARN] tools
  - gh config dir not found at /home/me/.config/gh

[ERROR] tasks
  - waiting/broken.md: parse frontmatter: yaml: line 3: ...

[FIXED] locks
  - removed stale agent lock .tasks/.locks/dead-agent.pid
```

No emoji. `[OK]`, `[WARN]`, `[ERROR]`, `[FIXED]` are greppable and match the
codebase's plain `fmt.Printf` style.

## Check Categories

Each category contains multiple individual findings. The category-level
indicator is the worst severity of any finding within it. INFO findings don't
affect the indicator.

### A. Git Repository (`git`)

| Finding | Severity |
|---------|----------|
| Path is not a git repository | ERROR |
| `git rev-parse --show-toplevel` fails | ERROR |
| Resolved repo root | INFO |
| Current branch / detached HEAD | INFO |

### B. Host Tools (`tools`)

| Finding | Severity |
|---------|----------|
| Missing required tool: copilot, git, git-upload-pack, git-receive-pack, gh | ERROR |
| Missing optional: git templates dir, system certs dir, gh config dir | WARN |
| Resolved tool paths | INFO |

Uses the new `InspectHostTools()` function in `internal/runner/tools.go`,
which checks all tools and returns structured findings instead of failing fast
on the first missing tool.

### C. Docker (`docker`)

| Finding | Severity |
|---------|----------|
| Docker CLI missing | ERROR |
| Docker daemon unreachable (`docker info` fails or times out) | ERROR |
| Docker reachable | INFO |

The Docker probe runs `docker info` with a **5-second context timeout** via
`exec.CommandContext`. This prevents hanging when the Docker daemon is
unresponsive. The probe function is a package-level variable for test injection:

```go
// internal/doctor/checks.go

var dockerProbe = func(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    return exec.CommandContext(ctx, "docker", "info").Run()
}
```

### D. Queue Layout (`queue`)

| Finding | Severity | Fixable |
|---------|----------|---------|
| `.tasks` path unreadable | ERROR | No |
| Missing required queue directory (any of 7 state dirs + `.locks/` + `messages/`) | ERROR | Yes — `--fix` creates via `os.MkdirAll` |
| Per-directory task counts | INFO | — |

### E. Task Parsing (`tasks`)

| Finding | Severity |
|---------|----------|
| Malformed YAML frontmatter in any queue directory | ERROR |
| Total parsed task count | INFO |

Uses `frontmatter.ParseTaskFile()` for each `.md` file. Unlike
`discoverHostTools()`, this checks all tasks rather than failing on the first
error.

### F. Locks & Orphans (`locks`)

| Finding | Severity | Fixable |
|---------|----------|---------|
| Stale agent `.pid` file (process dead) | WARN | Yes — `--fix` removes via `CleanStaleLocks()` |
| Stale `review-*.lock` file (process dead) | WARN | Yes — `--fix` removes via `CleanStaleReviewLocks()` |
| In-progress task claimed by dead agent | WARN | Yes — `--fix` recovers via `RecoverOrphanedTasks()` |
| Stale duplicate (in-progress + later-state copy) | WARN | Yes — `--fix` removes (handled by `RecoverOrphanedTasks()`) |
| Active agent registration count | INFO | — |

### G. Dependency Integrity (`deps`)

| Finding | Severity |
|---------|----------|
| Ambiguous ID (exists in both completed and non-completed directories) | WARN |
| Self-dependency | WARN |
| Circular dependency between waiting tasks | WARN |
| Unknown dependency ID reference (not found in any queue directory) | WARN |
| Promotable waiting task count | INFO |

Uses the new `DiagnoseDependencies()` function in `internal/queue/`.

## Fix Mode

The `--fix` flag enables safe, idempotent auto-repairs. Running `mato doctor
--fix` twice produces the same result. All fix operations reuse existing
code paths:

| Fix | Implementation |
|-----|----------------|
| Create missing queue directories | `os.MkdirAll(dir, 0o755)` for each of 7 state dirs + `.locks/` + `messages/` |
| Remove stale agent `.pid` files | `queue.CleanStaleLocks(tasksDir)` |
| Remove stale `review-*.lock` files | `queue.CleanStaleReviewLocks(tasksDir)` |
| Recover orphaned in-progress tasks | `queue.RecoverOrphanedTasks(tasksDir)` |

Explicitly **NOT** fixable (always require human action):

- Malformed task YAML
- Missing host tools
- Docker daemon down
- Dependency cycles
- Unknown dependency IDs

## Data Model

```go
// internal/doctor/doctor.go

// Severity represents the severity level of a diagnostic finding.
type Severity string

const (
    SeverityInfo    Severity = "info"
    SeverityWarning Severity = "warning"
    SeverityError   Severity = "error"
)

// Finding is a single diagnostic observation within a check category.
type Finding struct {
    Code     string   `json:"code"`
    Severity Severity `json:"severity"`
    Message  string   `json:"message"`
    Path     string   `json:"path,omitempty"`
    Fixable  bool     `json:"fixable,omitempty"`
    Fixed    bool     `json:"fixed,omitempty"`
}

// CheckReport holds the results of a single check category.
type CheckReport struct {
    Name     string    `json:"name"`
    Findings []Finding `json:"findings"`
}

// Report holds the complete diagnostic output.
type Report struct {
    RepoRoot string        `json:"repo_root"`
    TasksDir string        `json:"tasks_dir"`
    Checks   []CheckReport `json:"checks"`
    Summary  Summary       `json:"summary"`
}

// Summary holds aggregate counts across all checks.
type Summary struct {
    Warnings int `json:"warnings"`
    Errors   int `json:"errors"`
    Fixed    int `json:"fixed"`
}

// Options configures a doctor run.
type Options struct {
    Fix    bool
    Format string   // "text" or "json"
    Only   []string // optional check name filter
}

// Run executes all configured checks and returns a report.
func Run(repoRoot, tasksDir string, opts Options) (Report, error)

// ExitCode returns the process exit code for this report:
// 0 if healthy (no warnings or errors remain), 1 if warnings remain, 2 if errors remain.
func (r Report) ExitCode() int
```

### Dependency Diagnostics (in `internal/queue/`)

This function lives in `internal/queue/` because it reads queue state via
`PollIndex`. It takes the tasks directory and a `*PollIndex` as inputs and
returns structured issues. When `idx` is nil, a temporary index is built
internally (matching the convention used by `ReconcileReadyQueue` and
`resolvePromotableTasks`).

```go
// internal/queue/diagnostics.go

// DependencyIssueKind classifies a dependency problem.
type DependencyIssueKind string

const (
    DependencyAmbiguousID DependencyIssueKind = "ambiguous_id"
    DependencySelfCycle   DependencyIssueKind = "self_dependency"
    DependencyCycle       DependencyIssueKind = "cycle"
    DependencyUnknownID   DependencyIssueKind = "unknown_dependency"
)

// DependencyIssue describes a single dependency problem for a task.
type DependencyIssue struct {
    Kind      DependencyIssueKind
    TaskID    string
    Filename  string
    DependsOn string // the problematic dependency reference
}

// DependencyDiagnostics holds the results of dependency analysis.
type DependencyDiagnostics struct {
    Issues     []DependencyIssue
    Promotable int // number of waiting tasks with all deps satisfied
}

// DiagnoseDependencies performs read-only dependency analysis across
// all queue directories. It detects ambiguous IDs, self-dependencies,
// circular dependencies, and unknown dependency references. When idx
// is nil, a temporary index is built internally.
func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

The implementation extracts the warning logic currently in
`ReconcileReadyQueue()` (lines 102–137 of `reconcile.go`) into structured
return values. `ReconcileReadyQueue()` can then call `DiagnoseDependencies()`
internally and emit its existing `fmt.Fprintf` warnings from the structured
results, eliminating duplicated logic.

### Tool Inspection (in `internal/runner/`)

```go
// internal/runner/tools.go (addition)

// ToolFinding describes the result of probing a single host tool or directory.
type ToolFinding struct {
    Name     string
    Path     string // resolved path, empty if not found
    Required bool
    Found    bool
    Message  string // human-readable detail
}

// ToolReport holds the results of probing all host tools.
type ToolReport struct {
    Findings []ToolFinding
}

// InspectHostTools probes all host tools and directories, returning
// structured findings for every item regardless of success or failure.
func InspectHostTools() ToolReport
```

`discoverHostTools()` is refactored to call `InspectHostTools()` and convert
the result to the existing fail-fast error behavior. The package-level var
for test injection:

```go
// internal/doctor/checks.go

var inspectHostToolsFn = runner.InspectHostTools
```

## Package Structure

### Files to Create

| File | Purpose |
|------|---------|
| `internal/doctor/doctor.go` | `Report` model, `Run()` orchestration, `ExitCode()`, `Options` |
| `internal/doctor/checks.go` | `checkDef` struct, check list, 7 check implementations, function var hooks |
| `internal/doctor/render.go` | Text and JSON rendering |
| `internal/doctor/doctor_test.go` | Unit tests for checks and rendering |
| `internal/queue/diagnostics.go` | `DiagnoseDependencies()`, `DependencyIssue`, `DependencyDiagnostics` |
| `internal/queue/diagnostics_test.go` | Tests for dependency diagnostics |

### Files to Modify

| File | Change |
|------|--------|
| `cmd/mato/main.go` | Add `newDoctorCmd()`, wire to root, handle `ExitCode()` via `os.Exit()` |
| `internal/runner/tools.go` | Add `InspectHostTools()` and `ToolReport`; refactor `discoverHostTools()` to use it |
| `internal/queue/reconcile.go` | Refactor warning logic in `ReconcileReadyQueue()` to call `DiagnoseDependencies()` |
| `README.md` | Document `mato doctor` command |

### Internal Check Structure

```go
// internal/doctor/checks.go

type checkContext struct {
    repoRoot string
    tasksDir string
    opts     Options
    idx      *queue.PollIndex // lazily built, shared across checks that need it
}

type checkDef struct {
    name string
    run  func(*checkContext) CheckReport
}

var checks = []checkDef{
    {"git", checkGit},
    {"tools", checkTools},
    {"docker", checkDocker},
    {"queue", checkQueueLayout},
    {"tasks", checkTaskParsing},
    {"locks", checkLocksAndOrphans},
    {"deps", checkDependencies},
}

// Function variable hooks for external probes (test injection).
var inspectHostToolsFn = runner.InspectHostTools
var dockerProbe = func(ctx context.Context) error {
    ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    return exec.CommandContext(ctx, "docker", "info").Run()
}
```

## Existing Code Reuse

**Reuse directly** (no changes needed):

- `frontmatter.ParseTaskFile()` — task parsing check
- `queue.BuildIndex()` — build `PollIndex` for dep/lock/task checks
- `queue.RecoverOrphanedTasks()` — fix orphaned in-progress tasks
- `queue.CleanStaleLocks()` — fix stale `.pid` files
- `queue.CleanStaleReviewLocks()` — fix stale review locks
- `queue.ListTaskFiles()` — enumerate `.md` files in directories
- `lockfile.IsHeld()` — check if a lock file is live
- `queue.ParseClaimedBy()` — extract agent from claimed-by comment
- `identity.IsAgentActive()` — check if agent process is alive

**Refactor for structured output** (small additions):

- `internal/runner/tools.go` → new `InspectHostTools()` returning `ToolReport`
- `internal/queue/reconcile.go` → extract dep warning logic into `DiagnoseDependencies()`

**Do NOT reuse for read-only checks:**

- `ReconcileReadyQueue()` — it mutates queue state (moves files, prints
  warnings). Use `DiagnoseDependencies()` instead.

## Test Plan

### Unit Tests (`internal/doctor/doctor_test.go`)

| Test | Coverage |
|------|----------|
| `TestDoctor_HealthyRepo` | All checks OK, exit 0 |
| `TestDoctor_MissingQueueDir` | Error finding; `--fix` creates dir, re-run shows `[FIXED]` |
| `TestDoctor_MalformedTask` | Error finding for bad YAML |
| `TestDoctor_StalePIDLock` | Warning; `--fix` removes |
| `TestDoctor_StaleReviewLock` | Warning; `--fix` removes |
| `TestDoctor_OrphanedInProgress` | Warning; `--fix` recovers to backlog |
| `TestDoctor_SelfDependency` | Warning for self-dep |
| `TestDoctor_CircularDependency` | Warning for cycle |
| `TestDoctor_UnknownDependencyID` | Warning for unknown ref |
| `TestDoctor_AmbiguousID` | Warning for ID in completed + non-completed |
| `TestDoctor_OnlyFilter` | `--only` runs subset of checks |
| `TestDoctor_DockerTimeout` | Verifies 5-second timeout on Docker probe |
| `TestRenderText` | Text output format correct for all severity levels |
| `TestRenderJSON` | Valid JSON, matches `Report` structure |
| `TestExitCode` | Exit 0/1/2 for healthy/warnings/errors |

### Unit Tests (`internal/queue/diagnostics_test.go`)

| Test | Coverage |
|------|----------|
| `TestDiagnoseDependencies_Healthy` | No issues, correct promotable count |
| `TestDiagnoseDependencies_SelfCycle` | Detects self-dependency |
| `TestDiagnoseDependencies_CircularDeps` | Detects circular dependencies |
| `TestDiagnoseDependencies_UnknownIDs` | Detects missing references |
| `TestDiagnoseDependencies_AmbiguousIDs` | Detects IDs in completed + non-completed |

### Integration Test (`internal/integration/doctor_test.go`)

- Create repo with stale `.pid` lock + orphaned in-progress task
- Run doctor without `--fix`, verify exit code 1 (warnings)
- Run doctor with `--fix`, verify filesystem repaired
- Run doctor again, verify exit code 0 (all fixed)

### Test Doubles

Function vars for `inspectHostToolsFn` and `dockerProbe` in
`internal/doctor/checks.go`, matching the codebase's function hook pattern
(see `claimPrependFn` in `internal/queue/claim.go:46`). Tests override these
vars and restore originals via `t.Cleanup()`.

All tests use `t.TempDir()` and `testutil.SetupRepoWithTasks()` for
filesystem setup. No mock libraries.

## Implementation Order

| Step | Task | Depends On |
|------|------|------------|
| 1 | `internal/doctor/doctor.go` — `Report`, `Finding`, `CheckReport`, `Options`, `ExitCode()`, `Run()` skeleton | — |
| 2 | `internal/doctor/render.go` — text and JSON rendering | Step 1 |
| 3 | `internal/doctor/checks.go` — `checkDef`, `checkGit`, `checkQueueLayout` (with fix) | Step 1 |
| 4 | `internal/runner/tools.go` — add `InspectHostTools()`, refactor `discoverHostTools()` | — |
| 5 | `internal/doctor/checks.go` — `checkTools`, `checkDocker` (with 5s timeout) | Steps 3, 4 |
| 6 | `internal/doctor/checks.go` — `checkTaskParsing`, `checkLocksAndOrphans` (with fix) | Step 3 |
| 7 | `internal/queue/diagnostics.go` — `DiagnoseDependencies()`; refactor `ReconcileReadyQueue()` | — |
| 8 | `internal/doctor/checks.go` — `checkDependencies` | Steps 3, 7 |
| 9 | `cmd/mato/main.go` — `newDoctorCmd()`, exit code wiring | Steps 1, 2 |
| 10 | Unit tests for `internal/queue/diagnostics_test.go` | Step 7 |
| 11 | Unit tests for `internal/doctor/doctor_test.go` | Steps 1–8 |
| 12 | Integration test `internal/integration/doctor_test.go` | Steps 1–9 |
| 13 | `README.md` — document `mato doctor` | Step 9 |

Steps 1–3 and 4 and 7 can proceed in parallel. Steps 10–12 can run after
their dependencies complete.

## Effort Estimate

| Task | Effort |
|------|--------|
| Report model + `Run()` orchestration | 2 hours |
| Text + JSON rendering | 1.5 hours |
| Git + Tools + Docker checks (incl. timeout) | 2 hours |
| Queue layout + Task parsing checks | 1.5 hours |
| Locks/orphans check + fix mode | 1.5 hours |
| Dependency diagnostics refactor (`DiagnoseDependencies()`) | 2 hours |
| CLI wiring + exit codes | 1 hour |
| Unit tests | 3 hours |
| Integration test | 1 hour |
| Documentation | 30 min |
| **Total** | **~2 days** |

## Open Questions

1. **`--format json`**: Include from day one or defer? JSON makes `mato doctor`
   useful in CI, but adds ~1 hour. Recommendation: include it — the rendering
   is trivial with `encoding/json`.
2. **`mato doctor --watch`**: Continuous monitoring mode. Not needed for v1;
   the value of doctor is point-in-time diagnosis, not ongoing monitoring
   (that's what `mato status --watch` is for).
3. **Remote checks**: Verify GitHub API access, check repo permissions. Adds
   network dependency and latency — defer to a future `--thorough` flag.
4. **Docker image pull check**: Verify the configured Docker image is pullable.
   Slow (network I/O) — defer to `--thorough`.
