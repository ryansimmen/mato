# `mato doctor` — Implementation Plan

> **Status: Implemented** — This proposal has been fully implemented.
> **Read-only:** This file is a historical snapshot. Update the source code and living docs instead.
> The text below describes the original design; see the source code for
> the current implementation.

## Summary

Add a `mato doctor` command that performs structured health checks across 7
categories (git, host tools, Docker, queue layout, task parsing, locks/orphans,
dependency integrity) and renders a text or JSON report with machine-usable exit
codes. An optional `--fix` flag auto-repairs safe, idempotent issues.

Estimated effort: ~2 days.

## Goals

- Give users one command to diagnose common setup and queue-state problems.
- Reuse existing queue/index logic where possible.
- Support CI-friendly exit codes and JSON output.
- Keep `--fix` narrow, explicit, and safe enough to trust.

## Non-Goals

- Not a live dashboard or replacement for `mato status`.
- Not a full queue reconciler.
- Not a "fix everything automatically" command.

## CLI Specification

Implement `doctor` as a normal Cobra subcommand, similar to `status`. This
should **not** be bolted onto the root command's custom flag parsing in
`cmd/mato/main.go`, because that parsing exists only to forward arbitrary flags
to the Copilot CLI for `mato run`.

```text
mato doctor [flags]

Flags:
  --repo <path>        Path to git repository (default: current directory)
  --tasks-dir <path>   Path to tasks directory (default: <repo>/.tasks)
  --fix                Auto-repair safe issues (stale locks, orphaned tasks, missing dirs)
  --format text|json   Output format (default: text)
  --only <name>        Run only specified checks (repeatable: git, tools, docker, queue, tasks, locks, hygiene, deps)
```

`--repo` and `--tasks-dir` mirror the existing root command and `mato status`
for consistency.

### Exit Codes

| Code | Meaning |
|------|---------|
| `0`  | No remaining warnings or errors after any fixes are applied |
| `1`  | One or more warnings remain after fixes, no errors |
| `2`  | One or more errors remain after fixes |

Exit codes reflect the state **after** fixes are applied. If `--fix` repairs
all issues, the exit code is `0` even though findings existed before the run.
If `--fix` repairs some issues but warnings or errors remain, the exit code
reflects the remaining severity.

The command exits with a health status code without printing `mato error:` for a
normal unhealthy report. To achieve this, the doctor `RunE` returns a typed
`ExitError` that carries the exit code:

```go
// cmd/mato/main.go

type ExitError struct {
	Code int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit %d", e.Code)
}
```

`main()` checks for `ExitError` via `errors.As` and calls `os.Exit(e.Code)`
without printing the `mato error:` prefix. All other errors continue to
print the prefix and exit 1.

The doctor `RunE` uses the two-return-value `Run()` to distinguish hard
failures from health findings:

```go
report, err := doctor.Run(ctx, repoInput, tasksDir, opts)
if err != nil {
	return err // hard failure → "mato error: ..." + exit 1
}
// render report to stdout
if report.ExitCode != 0 {
	return ExitError{Code: report.ExitCode} // health status → silent exit 1 or 2
}
return nil // healthy → exit 0
```

### `--only` Validation

Unknown `--only` values are rejected by `Run()` before any checks execute,
returning a report with a single error finding. This keeps validation
consistent for both text and JSON consumers — cobra usage output is not
printed.

Non-selected checks are included in the report with `status: "skipped"` and
an empty findings list. This is intentional for JSON schema stability: consumers
can rely on a fixed set of check names in every report regardless of `--only`
filtering. Do not "optimize" by omitting non-selected checks from output.

`--only` controls which checks appear as `"ran"` in the report, not which
internal steps execute. Repo resolution is an internal prerequisite that
always runs when `tasksDir` must be derived (i.e., `--tasks-dir` not
explicitly set), even if `git` is not in `--only`. For example,
`mato doctor --only queue` still resolves the repo root to derive
`<repo>/.tasks`, but the git check appears as `"skipped"` in the report.

If a prerequisite fails and a dependent check cannot proceed, the failure
is attached to the dependent check — not the hidden prerequisite. For
example, `mato doctor --only queue --repo /not-a-repo` (no `--tasks-dir`)
produces a `queue.no_tasks_dir` error finding on the queue check, not a
silent skip. This ensures every `--only` report fully explains its exit code.

### `--format` Validation

Invalid `--format` values are rejected in the cobra `RunE` function (not in
`Run()`), consistent with how `mato status` validates `--interval` in its
`RunE`. This is a flag-parsing concern, not a domain concern. Because doctor
sets `SilenceUsage: true` and `SilenceErrors: true` (matching root and status
commands), cobra does not print usage on error — the error surfaces as
`mato error: ...` via `main()`:

```go
if format != "text" && format != "json" {
	return fmt.Errorf("--format must be text or json, got %s", format)
}
```

## Design Decisions

### 1. Repo resolution is diagnostic, not fatal

`mato doctor --repo /not-a-repo` should produce a git error finding, not a
command-level error. The git check populates `repoRoot` in the shared context
only on success.

`tasksDir` is resolved independently of git when `--tasks-dir` is explicitly
provided. If `--tasks-dir` is not set, it defaults to `<repoRoot>/.tasks`,
which requires a valid repo. This means:

- `mato doctor --repo /bad/path` — git fails, `tasksDir` is empty (not
  derivable), filesystem checks are skipped.
- `mato doctor --repo /bad/path --tasks-dir /my/.tasks` — git fails but
  queue/tasks/locks/deps checks proceed against the explicit path.
- `mato doctor` — git resolves cwd, `tasksDir` defaults from repo root.

### 2. Shared `PollIndex`

Checks that need queue/task state share a single `queue.BuildIndex()` result
via a lazily-built field on the shared context. This avoids re-parsing files
and keeps doctor aligned with runtime behavior. `BuildIndex()` also validates
affects entries via `frontmatter.ValidateAffectsGlobs()`, recording invalid
globs (broken syntax or glob combined with trailing `/`) as `BuildWarning`
entries — doctor surfaces these in the task parsing check as
`tasks.invalid_glob` warnings. These same entries trigger runtime
quarantine in `ReconcileReadyQueue()`.

The index is built once and reflects the filesystem state at build time. If
`--fix` mutates the filesystem (e.g., `RecoverOrphanedTasks` moves tasks from
in-progress to backlog), later checks use the pre-fix index. This is
acceptable because:

- `RecoverOrphanedTasks` moves tasks between non-waiting directories, so
  dependency analysis (which only examines waiting and completed tasks) is
  unaffected.
- `CleanStaleLocks` and `CleanStaleReviewLocks` remove lock files, which are
  not part of the index.
- Created missing directories would be empty, contributing no tasks to the
  index regardless.

### 3. Fix mode stays narrow

`--fix` only covers repairs that are clearly bounded, idempotent, and have
obvious intent. Everything else stays read-only.

### 4. DAG-based dependency analysis via `internal/dag`

Dependency diagnostics use the `internal/dag` package (`dag.Analyze()` with
Kahn's algorithm + Tarjan's SCC detection) via `DiagnoseDependencies()` in
`internal/queue/diagnostics.go`. The old `dependsOnWaitingTask()` and
`logCircularDependency()` helpers have been removed. The DAG package provides
richer blocking-reason classification (`BlockedByWaiting`, `BlockedByUnknown`,
`BlockedByExternal`, `BlockedByAmbiguous`) that doctor can surface directly.

### 5. Function variable hooks for external probes

Docker and tool inspection use package-level function variables in
`internal/doctor/checks.go` for test injection, matching the existing
`claimPrependFn` pattern in `internal/queue/claim.go`.

### 6. Non-zero exit codes for health status

`mato status` always exits 0 if it can successfully gather and display data,
regardless of queue health — it is purely informational. `mato doctor`
deliberately departs from this convention: exit code reflects remaining
severity (0/1/2 for ok/warnings/errors). This is a diagnostic tool, not a
dashboard, and CI consumers need machine-readable health signals.

## Output Format

### Text rendering

One-line summary header, then per-category sections. The category-level
indicator reflects the worst **remaining** severity of any finding in that
category. Fixed findings are annotated individually.

```text
mato doctor: 1 error, 2 warnings, 1 fixed

[OK] git
  - repo root: /repo
  - current branch: mato

[WARN] tools
  - gh config dir not found at /home/me/.config/gh (warning)

[ERROR] tasks
  - waiting/broken.md: parse frontmatter: yaml: line 3: ... (error)

[WARN] locks (1 fixed)
  - removed stale agent lock .tasks/.locks/dead-agent.pid (fixed)
  - stale review lock .tasks/.locks/review-abc.lock (fixable with --fix)
```

Rules:

- Category indicator is the worst remaining (non-fixed) severity.
- If all findings in a category were fixed, the category shows `[OK]` with a
  `(N fixed)` suffix.
- Checks that were not executed (filtered by `--only` or skipped due to
  missing context like `tasksDir`) show `[SKIP]` with no findings.
- Fixable-but-not-fixed findings show `(fixable with --fix)` when `--fix` is
  not active.
- No emoji. `[OK]`, `[WARN]`, `[ERROR]`, `[SKIP]` are greppable and match
  the codebase's plain `fmt.Printf` style.

### JSON rendering

The `Report` struct is serialized directly via `encoding/json`. The `exit_code`
field is included so JSON consumers don't need to inspect process exit status
separately. JSON consumers should parse stdout only; when `--fix` is active,
repair progress messages may appear on stderr by design.

## Check Categories

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
| Missing `~/.copilot` dir (bind-mounted unconditionally by Docker args) | ERROR |
| Missing optional: git templates dir, system certs dir, gh config dir | WARN |
| Resolved tool paths | INFO |

Uses the new `InspectHostTools()` function, which probes the same tools as
`discoverHostTools()` but collects structured findings instead of failing fast.
`InspectHostTools()` is a **parallel implementation**, not a refactor of
`discoverHostTools()` — the existing function is left unchanged to avoid
regressions in agent startup.

**`~/.copilot` severity**: Docker args in `internal/runner/config.go`
bind-mount `~/.copilot` unconditionally (not behind a `hasX` guard like
`ghConfigDir`). A missing `~/.copilot` directory will cause `docker run`
to fail at agent startup, so doctor treats this as ERROR rather than WARN.

### C. Docker (`docker`)

| Finding | Severity |
|---------|----------|
| Docker CLI missing | ERROR |
| Docker daemon unreachable (timeout or error) | ERROR |
| Docker reachable | INFO |

The Docker probe runs with a **5-second context timeout** to prevent hanging
when the daemon is unresponsive:

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
| `tasksDir` not resolvable (repo resolution failed, no explicit `--tasks-dir`) | ERROR | No |
| `.tasks` path unreadable | ERROR | No |
| Missing queue directory (7 state dirs + `.locks/` + `messages/events/` + `messages/presence/` + `messages/completions/`) | ERROR | Yes |
| Directory-level read error (e.g., permission denied) from `BuildWarnings()` | ERROR | No |
| Per-directory task counts | INFO | — |

Fix: `os.MkdirAll(dir, 0o755)` for each missing directory.

The 7 state dirs come from `queue.AllDirs`. The messaging directory names
(`messages/events`, `messages/presence`, `messages/completions`) are currently
hardcoded strings in `internal/messaging/messaging.go` with no exported
constant. Add an exported `MessagingDirs` slice to the `messaging` package
(analogous to `queue.AllDirs`) so doctor doesn't duplicate the list.

Note: runtime also writes flat files directly under `messages/` — including
`file-claims.json`, `dependency-context-*.json`, and `verdict-*.json`. The
directory check covers only the three subdirectories because those are the
only ones created by `messaging.Init()`. The flat files are created
on-demand by agent processes and do not need pre-existing directories.

`BuildIndex()` records directory-level filesystem errors (e.g., permission
denied when listing directory contents) as `BuildWarning` entries, separate
from file-level `ParseFailure` entries. It also records glob validation
errors (invalid glob syntax in affects entries) as `BuildWarning` entries.
The queue check surfaces directory-level errors; glob validation warnings
are handled by the tasks check (see below).

### E. Task Parsing (`tasks`)

| Finding | Severity |
|---------|----------|
| Malformed YAML frontmatter in any queue directory | ERROR |
| Invalid glob syntax in affects entry (quarantined at runtime) | WARN |
| Total parsed task count | INFO |

Uses parse failures from the shared `PollIndex` rather than re-parsing files.
Glob validation errors are surfaced via `idx.BuildWarnings()` — `BuildIndex()`
validates affects entries using `frontmatter.ValidateAffectsGlobs()` and
records invalid globs as build warnings alongside directory-level errors.

**Runtime impact of invalid globs**: Invalid glob syntax is not just a
passive warning. `ReconcileReadyQueue()` actively quarantines tasks with
invalid globs: backlog tasks are moved to `failed/`, and waiting tasks that
would otherwise be promotable are quarantined instead of promoted.
`CountPromotableWaitingTasks()` also excludes them. `HasActiveOverlap()`
treats invalid globs conservatively via static-prefix comparison (never a
false negative, only false positives). Doctor surfaces these as warnings so
users can fix the glob syntax before runtime quarantines the task.

### F. Locks & Orphans (`locks`)

| Finding | Severity | Fixable |
|---------|----------|---------|
| Stale agent `.pid` file (process dead) | WARN | Yes |
| Stale `review-*.lock` file (process dead) | WARN | Yes |
| In-progress task claimed by dead agent | WARN | Yes |
| Stale duplicate (in-progress + later-state copy) | WARN | Yes |
| Active agent registration count | INFO | — |

Note: `RecoverOrphanedTasks()` is not a cosmetic cleanup — it moves tasks and
appends failure records, which affects retry history. This is acceptable for
`--fix` but must be described accurately in output.

**Cycle handling**: `ReconcileReadyQueue()` already acts on cycles at
runtime — it appends `<!-- cycle-failure: ... -->` markers via
`taskfile.AppendCycleFailureRecord()` and moves cycle members to `failed/`.
Doctor's deps check reports cycles as warnings but does **not** auto-fix
them via `--fix`. Cycle resolution requires human action: break the circular
dependency by editing task frontmatter, then move the task back from
`failed/` to `waiting/` or `backlog/`. The runtime reconciler handles the
quarantine; doctor handles the diagnosis.

### G. Dependency Integrity (`deps`)

| Finding | Severity |
|---------|----------|
| Ambiguous ID (completed + non-completed collision) | WARN |
| Duplicate waiting task ID | WARN |
| Self-dependency | WARN |
| Circular dependency between waiting tasks | WARN |
| Unknown dependency ID reference | WARN |
| Blocked by external state (in-progress, failed, etc.) | INFO |
| Blocked by ambiguous ID | INFO |
| Deps-satisfied waiting task count | INFO |

Uses `DiagnoseDependencies()` from `internal/queue/diagnostics.go`, which
wraps `dag.Analyze()` from `internal/dag/`. The DAG analysis provides
structured `BlockDetail` entries with typed reasons (`BlockedByWaiting`,
`BlockedByUnknown`, `BlockedByExternal`, `BlockedByAmbiguous`) that doctor
maps directly to finding codes.

**Deps-satisfied vs promotable**: `Analysis.DepsSatisfied` lists tasks whose
dependency references all resolve to completed IDs. This is only the
dependency gate — a task is not truly promotable until it also passes
active-affects overlap filtering (`HasActiveOverlap()`) and glob validation
(`ValidateAffectsGlobs()`). Doctor reports the deps-satisfied count as an
INFO finding labeled "deps-satisfied" (not "promotable") to avoid implying
the task will be promoted. The fully-filtered promotable count is available
via `CountPromotableWaitingTasks()` but is not surfaced by doctor since it
conflates dependency, overlap, and glob concerns.

## Fix Mode

### Safe fixes (idempotent)

| Fix | Implementation |
|-----|----------------|
| Create missing queue/messaging directories | `os.MkdirAll(dir, 0o755)` |
| Remove stale agent `.pid` files | `queue.CleanStaleLocks(tasksDir)` |
| Remove stale `review-*.lock` files | `queue.CleanStaleReviewLocks(tasksDir)` |
| Recover orphaned in-progress tasks | `queue.RecoverOrphanedTasks(tasksDir)` |

### Not fixable (require human action)

- Malformed task YAML
- Invalid glob syntax in affects (runtime quarantines these to `failed/`)
- Missing host tools
- Docker daemon down
- Dependency cycles (runtime quarantines cycle members to `failed/` with
  `<!-- cycle-failure: ... -->` markers; human must break the cycle in
  frontmatter and move the task back)
- Unknown dependency IDs

### Structured repair reporting

`CleanStaleLocks` and `CleanStaleReviewLocks` are completely silent — they
return nothing and produce no output. `RecoverOrphanedTasks` prints progress
messages to stdout (e.g., "Recovered orphaned task X back to backlog") and
warnings to stderr. None of the three return structured results.

For doctor, implement scan-and-fix helpers in `internal/doctor/checks.go` that:

1. Detect the condition (scan lock files, check PID liveness) — this is the
   read-only diagnostic.
2. If `--fix` is not active, mark findings as `Fixable: true`.
3. If `--fix` is active, call the existing repair function, then **re-scan**
   to verify the condition is resolved. Mark `Fixed: true` only if the
   re-scan confirms the issue is gone.

Re-scanning is cheap (an `os.Stat` per lock file or task file) and necessary
because the existing repair helpers can silently fail:
`CleanStaleLocks` and `CleanStaleReviewLocks` discard `os.Remove` errors,
and `RecoverOrphanedTasks` can fail on `AtomicMove` or `os.OpenFile`. If a
repair silently fails, the finding stays `Fixable: true, Fixed: false` and
contributes to the exit code — which is correct, since exit codes are defined
as the state after fixes.

**Fix-mode output and JSON safety**: `RecoverOrphanedTasks()` prints progress
messages directly to stdout and warnings to stderr. The stdout writes would
corrupt `--format json` output if interleaved.

Rather than redirecting global `os.Stdout` (which is brittle and race-prone
in tests), apply a small upstream fix: change the three `fmt.Printf` calls in
`RecoverOrphanedTasks()` to `fmt.Fprintf(os.Stderr, ...)`. Progress messages
belong on stderr anyway — this is arguably a bug fix, not a behavioral change.
`CleanStaleLocks` and `CleanStaleReviewLocks` produce no output, so no change
is needed for those.

With all repair output on stderr, the doctor report flow is straightforward:

1. Run all diagnostic checks, collecting findings.
2. If `--fix`, call repair functions (their output goes to stderr).
3. Write the report (text or JSON) to stdout.

No global stdio swapping, no buffering tricks.

## Data Model

```go
// internal/doctor/doctor.go

type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type CheckStatus string

const (
	CheckRan     CheckStatus = "ran"
	CheckSkipped CheckStatus = "skipped"
)

type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Path     string   `json:"path,omitempty"`
	Fixable  bool     `json:"fixable,omitempty"`
	Fixed    bool     `json:"fixed,omitempty"`
}
```

**Finding codes** follow the pattern `<category>.<issue>` using snake_case.
These are stable identifiers for JSON consumers. Examples:

| Code | Category | Meaning |
|------|----------|---------|
| `git.not_a_repo` | git | Path is not a git repository |
| `git.repo_root` | git | Resolved repo root (INFO) |
| `tools.missing_required` | tools | Required tool not found |
| `tools.missing_copilot_dir` | tools | `~/.copilot` directory not found (bind-mounted unconditionally) |
| `tools.missing_optional` | tools | Optional directory not found |
| `docker.cli_missing` | docker | Docker CLI not in PATH |
| `docker.daemon_unreachable` | docker | Docker daemon timeout or error |
| `queue.no_tasks_dir` | queue | tasksDir not resolvable (no repo, no explicit flag) |
| `queue.missing_dir` | queue | Expected directory does not exist |
| `queue.read_error` | queue | Directory-level filesystem error |
| `tasks.parse_error` | tasks | Malformed YAML frontmatter |
| `tasks.invalid_glob` | tasks | Invalid glob syntax in affects entry |
| `locks.stale_pid` | locks | Agent PID file for dead process |
| `locks.stale_review` | locks | Review lock for dead process |
| `locks.orphaned_task` | locks | In-progress task with dead agent |
| `locks.stale_duplicate` | locks | In-progress copy of later-state task |
| `deps.ambiguous_id` | deps | Task ID in both completed and non-completed |
| `deps.duplicate_id` | deps | Duplicate waiting task ID |
| `deps.self_dependency` | deps | Task depends on itself |
| `deps.cycle` | deps | Circular dependency between waiting tasks |
| `deps.unknown_id` | deps | Dependency references non-existent task ID |
| `deps.blocked_external` | deps | Dependency blocked by non-completed, non-waiting state |
| `deps.blocked_ambiguous` | deps | Dependency blocked by ambiguous ID |

```go
type CheckReport struct {
	Name     string      `json:"name"`
	Status   CheckStatus `json:"status"`
	Findings []Finding   `json:"findings"`
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

type Options struct {
	Fix    bool
	Format string   // "text" or "json"
	Only   []string // optional check name filter
}

// Run executes all configured checks and returns a report. The context
// is threaded to checks that perform external probes (e.g., Docker) so
// that Ctrl+C cancellation works cleanly.
//
// repoInput is the raw --repo flag value (or empty for cwd); the git
// check resolves it into the actual root and stores it in the report.
//
// Returns an error only for hard failures (canceled context, internal
// setup errors). Health findings — including errors — belong in the
// report, not in the returned error.
func Run(ctx context.Context, repoInput, tasksDir string, opts Options) (Report, error)
```

### Dependency diagnostics (`internal/queue/diagnostics.go`)

This file already exists (added in commit 9aece17). The types and function
are implemented and in use by `ReconcileReadyQueue()`.

```go
type DependencyIssueKind string

const (
	DependencyAmbiguousID DependencyIssueKind = "ambiguous_id"
	DependencyDuplicateID DependencyIssueKind = "duplicate_id"
	DependencySelfCycle   DependencyIssueKind = "self_dependency"
	DependencyCycle       DependencyIssueKind = "cycle"
	DependencyUnknownID   DependencyIssueKind = "unknown_dependency"
)

type DependencyIssue struct {
	Kind      DependencyIssueKind
	TaskID    string
	Filename  string
	DependsOn string // the problematic dependency reference
}

type DependencyDiagnostics struct {
	// Analysis is the underlying dag.Analysis result.
	Analysis dag.Analysis

	// Issues contains structured diagnostic issues sorted by
	// (Kind, TaskID, DependsOn).
	Issues []DependencyIssue

	// RetainedFiles maps each retained waiting task ID to its filename.
	// When duplicate waiting IDs exist, only the first file seen is
	// retained.
	RetainedFiles map[string]string
}

// DiagnoseDependencies builds the inputs to dag.Analyze() from the PollIndex,
// runs the analysis, and produces structured diagnostic issues. It is a
// read-only function with no file I/O beyond what the index already captured.
func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

Doctor's `checkDependencies` maps `DependencyIssue` entries to `Finding`
structs. It also iterates `Analysis.Blocked` to surface `BlockedByExternal`
and `BlockedByAmbiguous` entries as INFO findings, giving visibility into
why tasks are not promotable beyond the issue-level warnings.

The deps-satisfied count (for the INFO finding) is derived from
`len(Analysis.DepsSatisfied)`. This is the pre-filter count — it reflects
only the dependency gate, not the full promotability check which also
requires passing active-affects overlap and glob validation filters. Doctor
reports this as "deps-satisfied" to avoid confusion with the fully-filtered
count from `CountPromotableWaitingTasks()`.

### Tool inspection (`internal/runner/tools.go`)

```go
type ToolFinding struct {
	Name     string
	Path     string // resolved path, empty if not found
	Required bool
	Found    bool
	Message  string
}

type ToolReport struct {
	Findings []ToolFinding
}

// InspectHostTools probes all host tools and directories, returning
// structured findings for every item regardless of success or failure.
// This is a parallel implementation that mirrors the checks in
// discoverHostTools() but collects findings instead of failing fast.
// discoverHostTools() is NOT modified — it remains the production
// code path for agent startup.
func InspectHostTools() ToolReport
```

## Package Structure

### Internal orchestration

```go
// internal/doctor/checks.go

type checkContext struct {
	ctx       context.Context
	repoInput string
	repoRoot  string // populated by checkGit on success
	tasksDir  string // set from --tasks-dir or derived from repoRoot
	opts      Options
	idx       *queue.PollIndex // lazily built, shared across checks
}

// hasRepo returns true if the git check resolved a valid repo root.
func (c *checkContext) hasRepo() bool {
	return c.repoRoot != ""
}

// hasTasksDir returns true if tasksDir is resolved — either explicitly
// provided via --tasks-dir or derived from a valid repo root. When this
// returns false, filesystem checks (queue, tasks, locks, deps) emit a
// no_tasks_dir error finding rather than silently skipping.
func (c *checkContext) hasTasksDir() bool {
	return c.tasksDir != ""
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

// Function variable hooks for test injection.
var inspectHostToolsFn = runner.InspectHostTools

var dockerProbe = func(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run()
}
```

### Files to create

| File | Purpose |
|------|---------|
| `internal/doctor/doctor.go` | Report model, `Options`, `Run()`, rendering dispatch |
| `internal/doctor/checks.go` | `checkDef`, 7 check implementations, function variable hooks |
| `internal/doctor/render.go` | Text and JSON rendering |
| `internal/doctor/doctor_test.go` | Unit tests for checks and rendering |

### Files to modify

| File | Change |
|------|--------|
| `cmd/mato/main.go` | Add `ExitError` type, `newDoctorCmd()` as normal subcommand, exit code wiring |
| `internal/queue/queue.go` | Change 3 `fmt.Printf` calls in `RecoverOrphanedTasks()` to `fmt.Fprintf(os.Stderr, ...)` |
| `internal/messaging/messaging.go` | Add exported `MessagingDirs` slice; refactor `Init()` to use it |
| `README.md` | Document `mato doctor` command |

### Files that already exist (from commit 9aece17)

| File | Status |
|------|--------|
| `internal/dag/dag.go` | Complete — `Analyze()`, Kahn's + Tarjan's SCC, `BlockDetail` types |
| `internal/dag/dag_test.go` | Complete |
| `internal/queue/diagnostics.go` | Complete — `DiagnoseDependencies()`, issue types, `RetainedFiles` |
| `internal/queue/diagnostics_test.go` | Complete |

Note: `internal/runner/tools.go` gets the new `InspectHostTools()` function
and types added alongside the existing code. `discoverHostTools()` is **not**
modified.

The `ReconcileReadyQueue()` refactor (to call `DiagnoseDependencies()`) is
already complete — no additional changes needed there.

## Existing Code Reuse

**Reuse directly** (no changes needed):

- `frontmatter.ParseTaskFile()` — task parsing
- `frontmatter.ValidateAffectsGlobs()` — glob syntax validation
- `queue.BuildIndex()` — build `PollIndex`
- `queue.DiagnoseDependencies()` — DAG-based dependency analysis (already exists)
- `queue.RecoverOrphanedTasks()` — fix orphaned tasks
- `queue.CleanStaleLocks()` — fix stale `.pid` files
- `queue.CleanStaleReviewLocks()` — fix stale review locks
- `lockfile.IsHeld()` — check lock liveness
- `queue.ParseClaimedBy()` — extract agent from claimed-by comment
- `identity.IsAgentActive()` — check if agent process is alive
- `dag.Analyze()` — DAG analysis (used via `DiagnoseDependencies()`)

**New code needed** (small additions):

- `internal/runner/tools.go` → new `InspectHostTools()` as a parallel
  implementation alongside `discoverHostTools()` (existing function unchanged)

**Already completed** (by commit 9aece17):

- `internal/queue/diagnostics.go` — `DiagnoseDependencies()` with DAG-based
  analysis, structured `DependencyIssue` types, `DependencyDiagnostics` return
- `internal/dag/dag.go` — `dag.Analyze()` with Kahn's algorithm + Tarjan's SCC
- `ReconcileReadyQueue()` refactor to use `DiagnoseDependencies()` — done
- Removal of `dependsOnWaitingTask()` and `logCircularDependency()` — done

**Do NOT reuse for read-only checks:**

- `ReconcileReadyQueue()` — it mutates queue state. Use `DiagnoseDependencies()`
  instead.

## Test Plan

### Unit tests (`internal/doctor/doctor_test.go`)

| Test | Coverage |
|------|----------|
| `TestDoctor_HealthyRepo` | All checks OK, exit 0 |
| `TestDoctor_NotAGitRepo` | Git error finding, not command failure |
| `TestDoctor_MissingQueueDir` | Error finding; `--fix` creates dir; re-run shows fixed |
| `TestDoctor_MalformedTask` | Error finding for bad YAML |
| `TestDoctor_StalePIDLock` | Warning; `--fix` removes |
| `TestDoctor_StaleReviewLock` | Warning; `--fix` removes |
| `TestDoctor_OrphanedInProgress` | Warning; `--fix` recovers to backlog |
| `TestDoctor_SelfDependency` | Warning for self-dep |
| `TestDoctor_CircularDependency` | Warning for cycle |
| `TestDoctor_UnknownDependencyID` | Warning for unknown ref |
| `TestDoctor_AmbiguousID` | Warning for ID in completed + non-completed |
| `TestDoctor_DuplicateWaitingID` | Warning for duplicate waiting task ID |
| `TestDoctor_InvalidGlobSyntax` | Warning for invalid glob in affects |
| `TestDoctor_OnlyFilter` | `--only` runs subset of checks, others have `status: "skipped"` |
| `TestDoctor_OnlyFilter_InvalidName` | Unknown check name rejected |
| `TestDoctor_OnlyFilter_PrereqFailure` | `--only queue` with bad repo and no `--tasks-dir` produces `queue.no_tasks_dir` error |
| `TestDoctor_DockerTimeout` | 5-second timeout path covered |
| `TestDoctor_FixReporting` | Fixed findings don't corrupt category severity |
| `TestDoctor_FixJSONValid` | `--fix` with `--format json` produces valid JSON (no stderr interleave) |
| `TestDoctor_BuildWarnings` | Directory-level read error surfaced in queue check |
| `TestDoctor_JSONIncludesExitCode` | JSON shape includes exit_code field |
| `TestDoctor_ContextCancellation` | Cancelled context stops checks cleanly |
| `TestRenderText` | Text output format correct for all severity levels |
| `TestRenderJSON` | Valid JSON, matches `Report` structure |
| `TestExitCode` | Exit 0/1/2 for healthy/warnings/errors |

### Dependency diagnostics tests (`internal/queue/diagnostics_test.go`)

These tests already exist (added in commit 9aece17). No additional tests
needed for doctor — the existing test coverage includes healthy repos,
self-cycles, circular deps, unknown IDs, ambiguous IDs, and duplicate
waiting IDs.

### Integration test (`internal/integration/doctor_test.go`)

- Create repo with stale `.pid` lock + orphaned in-progress task.
- Run doctor without `--fix`, verify exit code 1 (warnings).
- Run doctor with `--fix`, verify filesystem repaired and findings marked fixed.
- Run doctor again, verify exit code 0.

### Test doubles

Function vars `inspectHostToolsFn` and `dockerProbe` in
`internal/doctor/checks.go`. Tests override via assignment and restore
originals via `t.Cleanup()`. All tests use `t.TempDir()` and
`testutil.SetupRepoWithTasks()`. No mock libraries.

## Implementation Order

| Step | Task | Depends On |
|------|------|------------|
| 1 | `internal/doctor/doctor.go` — report model, `Options`, `Run()` skeleton | — |
| 2 | `internal/doctor/render.go` — text and JSON rendering | Step 1 |
| 3 | `internal/doctor/checks.go` — `checkDef`, `checkGit`, `checkQueueLayout` (with fix) | Step 1 |
| 4 | `internal/runner/tools.go` — add `InspectHostTools()` alongside existing code | — |
| 5 | `internal/doctor/checks.go` — `checkTools`, `checkDocker` (with 5s timeout) | Steps 3, 4 |
| 6 | `internal/doctor/checks.go` — `checkTaskParsing`, `checkLocksAndOrphans` (with fix) | Step 3 |
| 7 | `internal/doctor/checks.go` — `checkDependencies` | Step 3 |
| 8 | `cmd/mato/main.go` — `newDoctorCmd()`, exit code wiring | Steps 1, 2 |
| 9 | Unit tests | Steps 1–7 |
| 10 | Integration test | Steps 1–8 |
| 11 | `README.md` documentation | Step 8 |

Steps 1–3 and 4 can proceed in parallel.

Note: `internal/queue/diagnostics.go` and `internal/dag/dag.go` (with tests)
already exist. Step 7 consumes them directly — no additional dependency
analysis code is needed.

## Effort Estimate

| Task | Effort |
|------|--------|
| Report model + `Run()` orchestration | 2 hours |
| Text + JSON rendering | 1.5 hours |
| Git + Tools + Docker checks (incl. timeout) | 2 hours |
| Queue layout + Task parsing checks (incl. glob validation) | 1.5 hours |
| Locks/orphans check + fix mode | 1.5 hours |
| Dependency check (consuming existing `DiagnoseDependencies()`) | 1 hour |
| CLI wiring + exit codes | 1 hour |
| Unit tests | 3 hours |
| Integration test | 1 hour |
| Documentation | 30 min |
| **Total** | **~2 days** |

Note: The dependency diagnostics infrastructure (`DiagnoseDependencies()`,
`dag.Analyze()`) and the `ReconcileReadyQueue()` refactor are already
complete, reducing the original ~2.5 day estimate.

## Open Questions

1. **`mato doctor --watch`**: Continuous monitoring mode. Not needed for v1 —
   doctor's value is point-in-time diagnosis.
2. **`--thorough` mode**: Slower checks like Docker image pullability or GitHub
   API access. Adds network dependency and latency — defer.
3. **Glob conflict diagnostics**: Consider surfacing glob-vs-glob overlap
   warnings in the deps check, using `affectsMatch()` from
   `internal/queue/overlap.go` to detect potentially conflicting affects
   patterns across waiting tasks.
