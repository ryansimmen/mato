# `mato doctor` — Implementation Plan

## Summary

Add a `mato doctor` command that performs structured health checks across 7
categories (git, host tools, Docker, queue layout, task parsing, locks/orphans,
dependency integrity) and renders a text or JSON report with machine-usable exit
codes. An optional `--fix` flag auto-repairs safe, idempotent issues.

Estimated effort: ~2.5 days.

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
  --only <name>        Run only specified checks (repeatable: git, tools, docker, queue, tasks, locks, deps)
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

### `--only` Validation

Unknown `--only` values are rejected by `Run()` before any checks execute,
returning a report with a single error finding. This keeps validation
consistent for both text and JSON consumers — cobra usage output is not
printed.

### `--format` Validation

Invalid `--format` values are rejected in the cobra `RunE` function (not in
`Run()`), consistent with how `mato status` validates `--interval` in its
`RunE`. This is a flag-parsing concern, not a domain concern, so a standard
cobra error with usage output is appropriate:

```go
if format != "text" && format != "json" {
	return fmt.Errorf("--format must be text or json, got %s", format)
}
```

## Design Decisions

### 1. Repo resolution is diagnostic, not fatal

`mato doctor --repo /not-a-repo` should produce a git error finding, not a
command-level error. The git check populates `repoRoot` in the shared context
only on success; other checks proceed regardless.

### 2. Shared `PollIndex`

Checks that need queue/task state share a single `queue.BuildIndex()` result
via a lazily-built field on the shared context. This avoids re-parsing files
and keeps doctor aligned with runtime behavior.

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

### 4. No `internal/dag` dependency

Dependency diagnostics use the existing `dependsOnWaitingTask()` logic from
`reconcile.go` for cycle detection. When `internal/dag/` lands, the diagnostics
helper can be upgraded to use it — but doctor ships independently.

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
- Fixable-but-not-fixed findings show `(fixable with --fix)` when `--fix` is
  not active.
- No emoji. `[OK]`, `[WARN]`, `[ERROR]` are greppable and match the codebase's
  plain `fmt.Printf` style.

### JSON rendering

The `Report` struct is serialized directly via `encoding/json`. The `exit_code`
field is included so JSON consumers don't need to inspect process exit status
separately.

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
| Missing optional: git templates dir, system certs dir, gh config dir | WARN |
| Missing optional: `~/.copilot` dir (net-new check, not in current `discoverHostTools()`) | WARN |
| Resolved tool paths | INFO |

Uses the new `InspectHostTools()` function, which probes the same tools as
`discoverHostTools()` but collects structured findings instead of failing fast.
`InspectHostTools()` is a **parallel implementation**, not a refactor of
`discoverHostTools()` — the existing function is left unchanged to avoid
regressions in agent startup.

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

`BuildIndex()` records directory-level filesystem errors (e.g., permission
denied when listing directory contents) as `BuildWarning` entries, separate
from file-level `ParseFailure` entries. The queue check surfaces these via
`idx.BuildWarnings()`.

### E. Task Parsing (`tasks`)

| Finding | Severity |
|---------|----------|
| Malformed YAML frontmatter in any queue directory | ERROR |
| Total parsed task count | INFO |

Uses parse failures from the shared `PollIndex` rather than re-parsing files.

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

### G. Dependency Integrity (`deps`)

| Finding | Severity |
|---------|----------|
| Ambiguous ID (completed + non-completed collision) | WARN |
| Self-dependency | WARN |
| Circular dependency between waiting tasks | WARN |
| Unknown dependency ID reference | WARN |
| Promotable waiting task count | INFO |

Uses `DiagnoseDependencies()` from `internal/queue/diagnostics.go`.

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
- Missing host tools
- Docker daemon down
- Dependency cycles
- Unknown dependency IDs

### Structured repair reporting

`CleanStaleLocks` and `CleanStaleReviewLocks` are completely silent — they
return nothing and produce no output. `RecoverOrphanedTasks` prints progress
messages to stdout (e.g., "Recovered orphaned task X back to backlog") and
warnings to stderr. None of the three return structured results.

For doctor, implement scan-and-fix helpers in `internal/doctor/checks.go` that:

1. Detect the condition (scan lock files, check PID liveness) — this is the
   read-only diagnostic.
2. If `--fix` is active, call the existing repair function and mark findings as
   `Fixed: true`.
3. If `--fix` is not active, mark findings as `Fixable: true`.

This avoids refactoring the existing repair helpers while giving doctor
structured reporting. The diagnostic scan is independent of the repair action.

**Stdout/stderr capture during `--fix`**: `RecoverOrphanedTasks()` prints
directly to stdout and stderr, which would corrupt `--format json` output.
During `--fix`, doctor temporarily redirects `os.Stdout` and `os.Stderr` to
`os.DevNull` around calls to repair functions that produce output, then
restores them. Since `os.Stdout` is `*os.File`, the redirect opens
`os.DevNull` as a file rather than using `io.Writer`:

```go
devNull, _ := os.Open(os.DevNull)
defer devNull.Close()
saved := os.Stdout
os.Stdout = devNull
defer func() { os.Stdout = saved }()
```

The doctor report itself is written to the original stdout after all checks
and fixes complete. `CleanStaleLocks` and `CleanStaleReviewLocks` produce no
output, so no capture is needed for those.

## Data Model

```go
// internal/doctor/doctor.go

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
```

**Finding codes** follow the pattern `<category>.<issue>` using snake_case.
These are stable identifiers for JSON consumers. Examples:

| Code | Category | Meaning |
|------|----------|---------|
| `git.not_a_repo` | git | Path is not a git repository |
| `git.repo_root` | git | Resolved repo root (INFO) |
| `tools.missing_required` | tools | Required tool not found |
| `tools.missing_optional` | tools | Optional directory not found |
| `docker.cli_missing` | docker | Docker CLI not in PATH |
| `docker.daemon_unreachable` | docker | Docker daemon timeout or error |
| `queue.missing_dir` | queue | Expected directory does not exist |
| `queue.read_error` | queue | Directory-level filesystem error |
| `tasks.parse_error` | tasks | Malformed YAML frontmatter |
| `locks.stale_pid` | locks | Agent PID file for dead process |
| `locks.stale_review` | locks | Review lock for dead process |
| `locks.orphaned_task` | locks | In-progress task with dead agent |
| `locks.stale_duplicate` | locks | In-progress copy of later-state task |
| `deps.ambiguous_id` | deps | Task ID in both completed and non-completed |
| `deps.self_dependency` | deps | Task depends on itself |
| `deps.cycle` | deps | Circular dependency between waiting tasks |
| `deps.unknown_id` | deps | Dependency references non-existent task ID |

```go
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

type Options struct {
	Fix    bool
	Format string   // "text" or "json"
	Only   []string // optional check name filter
}

// Run executes all configured checks and returns a report. The context
// is threaded to checks that perform external probes (e.g., Docker) so
// that Ctrl+C cancellation works cleanly.
func Run(ctx context.Context, repoRoot, tasksDir string, opts Options) Report
```

### Dependency diagnostics (`internal/queue/diagnostics.go`)

```go
type DependencyIssueKind string

const (
	DependencyAmbiguousID DependencyIssueKind = "ambiguous_id"
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
	Issues     []DependencyIssue
	Promotable int // number of waiting tasks with all deps satisfied
}

// DiagnoseDependencies performs read-only dependency analysis. When idx
// is nil, a temporary index is built internally. Reuses the existing
// resolvePromotableTasks() for the promotable count and
// dependsOnWaitingTask() for cycle detection; both are unexported but
// accessible within the queue package. Upgradeable to internal/dag
// when that package lands.
func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

This extracts the warning logic from `ReconcileReadyQueue()` into structured
return values. `ReconcileReadyQueue()` can then call `DiagnoseDependencies()`
internally and emit warnings from the structured results, eliminating
duplicated logic.

**Cycle deduplication**: `logCircularDependency()` in `reconcile.go`
deduplicates cycle warnings using a canonical-pair key (`min(a,b) + "\x00" +
max(a,b)`). `DiagnoseDependencies()` uses the same canonical-pair pattern
with its own `seen` map, appending to the issues slice instead of printing
to stderr.

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
	tasksDir  string
	opts      Options
	idx       *queue.PollIndex // lazily built, shared across checks
}

// hasRepo returns true if the git check resolved a valid repo root.
// Checks that depend on a valid repo (queue, tasks, locks, deps) should
// skip gracefully when this returns false.
func (c *checkContext) hasRepo() bool {
	return c.repoRoot != ""
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
| `internal/queue/diagnostics.go` | `DiagnoseDependencies()`, issue types |
| `internal/queue/diagnostics_test.go` | Dependency diagnostic tests |

### Files to modify

| File | Change |
|------|--------|
| `cmd/mato/main.go` | Add `ExitError` type, `newDoctorCmd()` as normal subcommand, exit code wiring |
| `internal/queue/reconcile.go` | Refactor warning logic to call `DiagnoseDependencies()` |
| `internal/messaging/messaging.go` | Add exported `MessagingDirs` slice; refactor `Init()` to use it |
| `README.md` | Document `mato doctor` command |

Note: `internal/runner/tools.go` gets the new `InspectHostTools()` function
and types added alongside the existing code. `discoverHostTools()` is **not**
modified.

## Existing Code Reuse

**Reuse directly** (no changes needed):

- `frontmatter.ParseTaskFile()` — task parsing
- `queue.BuildIndex()` — build `PollIndex`
- `queue.RecoverOrphanedTasks()` — fix orphaned tasks
- `queue.CleanStaleLocks()` — fix stale `.pid` files
- `queue.CleanStaleReviewLocks()` — fix stale review locks
- `lockfile.IsHeld()` — check lock liveness
- `queue.ParseClaimedBy()` — extract agent from claimed-by comment
- `identity.IsAgentActive()` — check if agent process is alive

**Refactor for structured output** (small additions):

- `internal/runner/tools.go` → new `InspectHostTools()` as a parallel
  implementation alongside `discoverHostTools()` (existing function unchanged)
- `internal/queue/reconcile.go` → extract dep warning logic into `DiagnoseDependencies()`

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
| `TestDoctor_OnlyFilter` | `--only` runs subset of checks |
| `TestDoctor_OnlyFilter_InvalidName` | Unknown check name rejected |
| `TestDoctor_DockerTimeout` | 5-second timeout path covered |
| `TestDoctor_FixReporting` | Fixed findings don't corrupt category severity |
| `TestDoctor_FixJSONNotCorrupted` | `--fix` with `--format json` produces valid JSON (stdout capture works) |
| `TestDoctor_BuildWarnings` | Directory-level read error surfaced in queue check |
| `TestDoctor_JSONIncludesExitCode` | JSON shape includes exit_code field |
| `TestDoctor_ContextCancellation` | Cancelled context stops checks cleanly |
| `TestRenderText` | Text output format correct for all severity levels |
| `TestRenderJSON` | Valid JSON, matches `Report` structure |
| `TestExitCode` | Exit 0/1/2 for healthy/warnings/errors |

### Dependency diagnostics tests (`internal/queue/diagnostics_test.go`)

| Test | Coverage |
|------|----------|
| `TestDiagnoseDependencies_Healthy` | No issues, correct promotable count |
| `TestDiagnoseDependencies_SelfCycle` | Detects self-dependency |
| `TestDiagnoseDependencies_CircularDeps` | Detects circular dependencies |
| `TestDiagnoseDependencies_UnknownIDs` | Detects missing references |
| `TestDiagnoseDependencies_AmbiguousIDs` | Detects completed + non-completed collision |

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
| 7 | `internal/queue/diagnostics.go` — `DiagnoseDependencies()`; refactor `ReconcileReadyQueue()` | — |
| 8 | `internal/doctor/checks.go` — `checkDependencies` | Steps 3, 7 |
| 9 | `cmd/mato/main.go` — `newDoctorCmd()`, exit code wiring | Steps 1, 2 |
| 10 | Unit tests | Steps 1–8 |
| 11 | Integration test | Steps 1–9 |
| 12 | `README.md` documentation | Step 9 |

Steps 1–3, 4, and 7 can proceed in parallel.

## Effort Estimate

| Task | Effort |
|------|--------|
| Report model + `Run()` orchestration | 2 hours |
| Text + JSON rendering | 1.5 hours |
| Git + Tools + Docker checks (incl. timeout) | 2 hours |
| Queue layout + Task parsing checks | 1.5 hours |
| Locks/orphans check + fix mode | 1.5 hours |
| Dependency diagnostics (`DiagnoseDependencies()`) | 2 hours |
| `ReconcileReadyQueue()` refactor + regression testing | 2 hours |
| CLI wiring + exit codes | 1 hour |
| Unit tests | 3 hours |
| Integration test | 1 hour |
| Documentation | 30 min |
| **Total** | **~2.5 days** |

## Open Questions

1. **`mato doctor --watch`**: Continuous monitoring mode. Not needed for v1 —
   doctor's value is point-in-time diagnosis.
2. **`--thorough` mode**: Slower checks like Docker image pullability or GitHub
   API access. Adds network dependency and latency — defer.
3. **Deeper DAG-backed diagnostics**: Upgrade `DiagnoseDependencies()` to use
   `internal/dag` once that package exists and stabilizes.
