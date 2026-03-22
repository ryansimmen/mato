# `mato doctor` — Multi-Agent Debate & Consensus Plan

## Participants

- **Claude Opus 4.6** — Emphasizes read-only diagnostics and conservative repairs
- **GPT-5.4** — Emphasizes structured reporting and machine-consumable output
- **Gemini 3.1 Pro** — Emphasizes operator UX and practical fix mode

---

## Round 1: Check Categories

### Claude Opus 4.6

`mato doctor` should be **primarily read-only** with a **very conservative `--fix`**. The core value is diagnosing problems, not auto-repairing them. I'd group checks into:

1. **Git repository** — is the repo valid, what branch are we on
2. **Host tools** — copilot, git, gh, git-upload-pack, git-receive-pack
3. **Docker** — daemon reachable
4. **Queue layout** — all 7 state directories exist
5. **Task parsing** — all .md files have valid YAML frontmatter
6. **Locks & orphans** — stale .pid files, stuck in-progress tasks
7. **Dependency integrity** — cycles, unknown IDs, ambiguous IDs

Critically: do **not** treat normal scheduling state as warnings. Tasks waiting for dependencies is normal. Affects-based deferrals are normal queue flow. These are info, not warnings.

### GPT-5.4

Agreed on the categories. I'd add one clarification: the tool discovery currently in `discoverHostTools()` (runner/tools.go) fails fast on the first missing tool. For `mato doctor`, we need to check **all** tools and report everything that's wrong, not just the first failure. This requires refactoring tool discovery to return structured findings.

I'd also add `--format text|json` from day one. JSON output makes `mato doctor` useful in CI pipelines and monitoring scripts. Without it, we're limited to human eyeballs.

### Gemini 3.1 Pro

Both good. My addition: the **fix mode** should cover the obvious safe operations:
- Create missing queue directories
- Remove stale `.pid` lock files
- Remove stale `review-*.lock` files
- Recover orphaned in-progress tasks (already implemented in `RecoverOrphanedTasks()`)

These are all idempotent and safe. Auto-fixing malformed YAML, dependency cycles, or missing tools is **not safe** and should never be attempted.

### **Round 1 Consensus**: Seven check categories (git, tools, Docker, queue layout, task parsing, locks/orphans, dependency integrity). Normal scheduling state (unsatisfied deps, affects deferrals) is info, not warnings.

---

## Round 2: Output Format

### GPT-5.4

I propose this text output format:
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

Section-level summary (`[OK]`, `[WARN]`, `[ERROR]`, `[FIXED]`), one-line header with aggregate counts, details indented below each section.

### Claude Opus 4.6

I like the structure. No emoji — the codebase uses plain `fmt.Printf` and `fmt.Fprintf` everywhere, not emoji. `[OK]`, `[WARN]`, `[ERROR]`, `[FIXED]` are clean and greppable.

JSON output should be a flat structure matching the text output — a `Report` with ordered `CheckReport` entries, each containing `Finding` items.

### Gemini 3.1 Pro

Agreed. One addition: for the `--fix` flag, findings that were fixed should show `[FIXED]` instead of the original severity. Findings that are fixable but weren't fixed (because `--fix` wasn't passed) should be marked with a hint like `(fixable with --fix)`.

### **Round 2 Consensus**: `[OK]`/`[WARN]`/`[ERROR]`/`[FIXED]` indicators. One-line summary header. Details indented per section. JSON mode with `--format json`. Fixable findings annotated with hint when `--fix` not active.

---

## Round 3: Fix Mode

### Gemini 3.1 Pro

Safe auto-fixes with `--fix`:

| Fix | How |
|-----|-----|
| Create missing queue directories | `os.MkdirAll` for each of the 7 state dirs + `.locks/` + `messages/` subdirs |
| Remove stale agent `.pid` files | Reuse `queue.CleanStaleLocks()` |
| Remove stale review `*.lock` files | Reuse `queue.CleanStaleReviewLocks()` |
| Recover orphaned in-progress tasks | Reuse `queue.RecoverOrphanedTasks()` |
| Remove stale in-progress duplicates | Already handled by `RecoverOrphanedTasks()` |

### Claude Opus 4.6

I agree with this list. Explicitly **NOT** fixable:
- Malformed task YAML — human must edit the file
- Missing host tools — human must install them
- Docker daemon down — human must start it
- Dependency cycles — requires task redesign
- Unknown dependency IDs — human must fix the reference

The `--fix` flag should be conservative and idempotent. Running `mato doctor --fix` twice in a row should be safe.

### GPT-5.4

One nuance: `RecoverOrphanedTasks()` mutates files (appends failure records). That's technically destructive — it changes task content. But it's already the standard recovery mechanism used during normal `mato run` startup. I'm comfortable including it in `--fix`.

### **Round 3 Consensus**: Five safe fix operations. All use existing code paths. Explicitly NOT fixable: YAML, tools, Docker, git, dependencies.

---

## Round 4: Package Structure

### Claude Opus 4.6

New `internal/doctor/` package with three files:
- `doctor.go` — orchestration, `Run()` function, report model
- `checks.go` — individual check implementations
- `render.go` — text and JSON rendering

This keeps the diagnostic logic separate from `queue` (which handles state mutation) and `runner` (which handles agent lifecycle).

### GPT-5.4

Agreed on the new package. But we also need small refactors in existing packages:

1. **`internal/runner/tools.go`**: Refactor `discoverHostTools()` into a structured `InspectHostTools()` that returns all findings. Keep `discoverHostTools()` as a wrapper that converts findings to the existing fail-fast error.

2. **`internal/queue/diagnostics.go`**: Add a read-only `DiagnoseDependencies()` function that returns structured issues. Then `ReconcileReadyQueue()` calls it internally instead of duplicating the warning logic.

### Gemini 3.1 Pro

I agree. The refactors are minimal — we're not rewriting existing code, just extracting structured return values from logic that already exists.

### **Round 4 Consensus**: New `internal/doctor/` package. Small refactors in `internal/runner/tools.go` and `internal/queue/` for reusable structured diagnostics.

---

## Round 5: Check Interface Design

### Claude Opus 4.6

Keep it simple. The mato codebase doesn't use heavyweight interfaces. A tiny internal struct is sufficient:

```go
type checkDef struct {
    name string
    run  func(*context) CheckReport
}
```

No public interface. The check list is a private slice in `checks.go`. This matches the codebase style — small focused functions, no abstraction layers.

### GPT-5.4

Agreed. A public `Check` interface would be over-engineering for 7 checks. The `checkDef` pattern lets us easily add/remove checks and filter with `--only`.

### Gemini 3.1 Pro

Same. Keep it internal, keep it simple.

### **Round 5 Consensus**: Internal `checkDef` struct with name + function. No public interface.

---

## Round 6: Exit Codes

### GPT-5.4

Exit codes need to be deterministic and machine-readable:

| Exit Code | Meaning |
|-----------|---------|
| `0` | Healthy — no warnings or errors remain (or all were fixed) |
| `1` | Warnings remain, no errors |
| `2` | One or more errors remain |

Normal Cobra usage/runtime errors stay on the existing error path.

### Claude Opus 4.6

This requires a small change in `cmd/mato/main.go` — the doctor command needs to exit with a health status code without printing `mato error:` for a normal unhealthy report. The `Report` struct should have an `ExitCode() int` method.

### Gemini 3.1 Pro

Agreed. Standard practice for health check commands.

### **Round 6 Consensus**: Exit 0 (healthy/all fixed), 1 (warnings), 2 (errors). `Report.ExitCode()` method.

---

## Round 7: Testing Strategy

### Claude Opus 4.6

Use real temp directories via `t.TempDir()` and `testutil.SetupRepoWithTasks()`. The mato codebase doesn't use mocks — it uses function variable hooks for test injection. Follow that pattern.

Test cases:
- Healthy repo → exit 0, all `[OK]`
- Missing queue dir → error, `--fix` creates it → `[FIXED]`
- Malformed task file → error
- Stale `.pid` lock → warning, `--fix` removes → `[FIXED]`
- Stale review lock → warning, `--fix` removes → `[FIXED]`
- Orphaned in-progress task → warning, `--fix` recovers → `[FIXED]`
- Dependency: self-cycle, circular, unknown ID, ambiguous ID
- Text rendering covers all severity levels
- JSON rendering produces valid JSON

### GPT-5.4

Add function vars for external probes (Docker check, tool inspection) so tests can run without Docker or real tool binaries.

### Gemini 3.1 Pro

One integration test: create a repo with stale lock + orphaned task, run doctor with fix, assert filesystem repaired and exit code downgraded.

### **Round 7 Consensus**: Real temp dirs, function vars for external probes, comprehensive unit tests, one integration test.

---

## Round 8: CLI Flags

### GPT-5.4

```
mato doctor [flags]

Flags:
  --repo <path>        Path to git repository (default: current directory)
  --tasks-dir <path>   Path to tasks directory (default: <repo>/.tasks)
  --fix                Auto-repair safe issues
  --format text|json   Output format (default: text)
  --only <name>        Only run specific checks (repeatable: git, tools, docker, queue, tasks, locks, deps)
```

### Claude Opus 4.6

`--repo` and `--tasks-dir` should mirror the existing root command and `mato status` flags for consistency.

### Gemini 3.1 Pro

Agreed. The `--only` flag is useful for CI — you might want `mato doctor --only docker --format json` in a pipeline.

### **Round 8 Consensus**: Six flags. `--repo`, `--tasks-dir`, `--fix`, `--format`, `--only`.

---

## Round 9: Reuse of Existing Code

### Claude Opus 4.6

**Reuse directly:**
- `frontmatter.ParseTaskFile()`
- `queue.BuildIndex()`
- `queue.RecoverOrphanedTasks()`
- `queue.CleanStaleLocks()`
- `queue.CleanStaleReviewLocks()`
- `lockfile.IsHeld()`
- `queue.ParseClaimedBy()`
- `identity.IsAgentActive()`

**Refactor for reuse:**
- `internal/runner/tools.go` → structured `InspectHostTools()` returning `ToolReport`
- `internal/queue/reconcile.go` → structured `DiagnoseDependencies()` returning `DependencyDiagnostics`

**Do NOT reuse directly:**
- `ReconcileReadyQueue()` for read-only checks — it mutates queue state and prints warnings directly. Use the new `DiagnoseDependencies()` instead.

### GPT-5.4 & Gemini 3.1 Pro

Agreed.

### **Round 9 Consensus**: Reuse 8 existing functions directly. Refactor 2 functions for structured output. Do not reuse mutation functions for read-only diagnostics.

---

## CONSENSUS PLAN

All three agents agree on the following implementation plan.

### 1. Files to Create

| File | Purpose |
|------|---------|
| `internal/doctor/doctor.go` | Report model, `Run()` orchestration, `ExitCode()` |
| `internal/doctor/checks.go` | Individual check implementations (7 checks) |
| `internal/doctor/render.go` | Text and JSON rendering |
| `internal/doctor/doctor_test.go` | Unit tests for checks and rendering |
| `internal/queue/diagnostics.go` | Read-only `DiagnoseDependencies()` |
| `internal/queue/diagnostics_test.go` | Tests for dependency diagnostics |

### 2. Files to Modify

| File | Change |
|------|--------|
| `cmd/mato/main.go` | Add `newDoctorCmd()`, wire exit codes |
| `internal/runner/tools.go` | Add `InspectHostTools()` returning `ToolReport`, refactor `discoverHostTools()` to use it |
| `README.md` | Document `mato doctor` command |

### 3. Data Model

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

type CheckReport struct {
    Name     string    `json:"name"`
    Findings []Finding `json:"findings"`
}

type Report struct {
    RepoRoot string        `json:"repo_root"`
    TasksDir string        `json:"tasks_dir"`
    Checks   []CheckReport `json:"checks"`
    Summary  Summary       `json:"summary"`
}

type Summary struct {
    Warnings int `json:"warnings"`
    Errors   int `json:"errors"`
    Fixed    int `json:"fixed"`
}

type Options struct {
    Fix    bool
    Format string   // "text" or "json"
    Only   []string // optional check group filter
}

func Run(repoRoot, tasksDir string, opts Options) (Report, error)

func (r Report) ExitCode() int
```

### 4. Check List with Severities

#### A. Git Repository (`git`)
| Finding | Severity |
|---------|----------|
| Path is not a git repository | ERROR |
| `git rev-parse --show-toplevel` fails | ERROR |
| Resolved repo root | INFO |
| Current branch / detached HEAD | INFO |

#### B. Host Tools (`tools`)
| Finding | Severity |
|---------|----------|
| Missing required tool: copilot, git, git-upload-pack, git-receive-pack, gh | ERROR |
| Missing optional: git templates dir, system certs dir, gh config dir | WARN |
| Resolved tool paths | INFO |

#### C. Docker (`docker`)
| Finding | Severity |
|---------|----------|
| Docker CLI missing or daemon unavailable | ERROR |
| Docker reachable | INFO |

#### D. Queue Layout (`queue`)
| Finding | Severity | Fixable |
|---------|----------|---------|
| `.tasks` path unreadable | ERROR | No |
| Missing required queue directory | ERROR | Yes — `--fix` creates it |
| Per-directory task counts | INFO | — |

#### E. Task Parsing (`tasks`)
| Finding | Severity |
|---------|----------|
| Malformed YAML frontmatter in any queue directory | ERROR |
| Total parsed task count | INFO |

#### F. Locks & Orphans (`locks`)
| Finding | Severity | Fixable |
|---------|----------|---------|
| Stale agent `.pid` file | WARN | Yes — `--fix` removes |
| Stale `review-*.lock` file | WARN | Yes — `--fix` removes |
| In-progress task claimed by dead agent | WARN | Yes — `--fix` recovers |
| Stale duplicate (in-progress + later-state copy) | WARN | Yes — `--fix` removes |
| Active agent registration count | INFO | — |

#### G. Dependency Integrity (`deps`)
| Finding | Severity |
|---------|----------|
| Ambiguous ID (in completed + non-completed) | WARN |
| Self-dependency | WARN |
| Circular dependency between waiting tasks | WARN |
| Unknown dependency ID reference | WARN |
| Promotable waiting task count | INFO |

### 5. Internal Structure

```go
// internal/doctor/checks.go

type context struct {
    repoRoot string
    tasksDir string
    opts     Options
    idx      *queue.PollIndex
}

type checkDef struct {
    name string
    run  func(*context) CheckReport
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
```

```go
// internal/queue/diagnostics.go

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
    DependsOn string
}

type DependencyDiagnostics struct {
    Issues     []DependencyIssue
    Promotable int
}

func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

```go
// internal/runner/tools.go (addition)

type ToolFinding struct {
    Name     string
    Path     string
    Required bool
    Found    bool
    Message  string
}

type ToolReport struct {
    Findings []ToolFinding
}

func InspectHostTools() ToolReport
```

### 6. CLI Command Spec

```
mato doctor [flags]

Flags:
  --repo <path>        Path to git repository (default: current directory)
  --tasks-dir <path>   Path to tasks directory (default: <repo>/.tasks)
  --fix                Auto-repair safe issues (stale locks, orphaned tasks, missing dirs)
  --format text|json   Output format (default: text)
  --only <name>        Run only specified checks (repeatable: git, tools, docker, queue, tasks, locks, deps)
```

Exit codes: `0` (healthy / all fixed), `1` (warnings remain), `2` (errors remain).

### 7. Test Plan

**Unit tests** (`internal/doctor/doctor_test.go`):

| Test | Coverage |
|------|----------|
| `TestDoctor_HealthyRepo` | All checks OK, exit 0 |
| `TestDoctor_MissingQueueDir` | Error finding, --fix creates dir, re-run shows fixed |
| `TestDoctor_MalformedTask` | Error finding for bad YAML |
| `TestDoctor_StalePIDLock` | Warning, --fix removes |
| `TestDoctor_StaleReviewLock` | Warning, --fix removes |
| `TestDoctor_OrphanedInProgress` | Warning, --fix recovers to backlog |
| `TestDoctor_SelfDependency` | Warning for self-dep |
| `TestDoctor_CircularDependency` | Warning for cycle |
| `TestDoctor_UnknownDependencyID` | Warning for unknown ref |
| `TestDoctor_AmbiguousID` | Warning for ID in completed + non-completed |
| `TestDoctor_OnlyFilter` | --only runs subset of checks |
| `TestRenderText` | Text output format correct |
| `TestRenderJSON` | Valid JSON, matches Report structure |

**Unit tests** (`internal/queue/diagnostics_test.go`):

| Test | Coverage |
|------|----------|
| `TestDiagnoseDependencies_Healthy` | No issues, correct promotable count |
| `TestDiagnoseDependencies_Cycles` | Detects circular deps |
| `TestDiagnoseDependencies_UnknownIDs` | Detects missing references |

**Integration test** (`internal/integration/doctor_test.go`):
- Create repo with stale lock + orphaned task
- Run doctor with fix
- Assert filesystem repaired and exit code downgraded

**Test doubles**: Function vars for `InspectHostTools()` and Docker check, matching codebase's "function hook" testing style.

### 8. Effort Estimate

| Task | Effort |
|------|--------|
| Report model + `Run()` orchestration | 2 hours |
| Text + JSON rendering | 1.5 hours |
| Git + Tools + Docker checks | 2 hours |
| Queue layout + Task parsing checks | 1.5 hours |
| Locks/orphans check + fix mode | 1.5 hours |
| Dependency diagnostics refactor | 2 hours |
| CLI wiring + exit codes | 1 hour |
| Unit tests | 3 hours |
| Integration test | 1 hour |
| Documentation | 30 min |
| **Total** | **~2 days** |

### 9. Open Questions (Deferred)

1. **`mato doctor --watch`**: Continuously monitor health. Not needed for v1.
2. **Remote checks**: Verify GitHub API access, check repo permissions. Adds network dependency — defer.
3. **Docker image pull check**: Verify the configured Docker image is pullable. Slow, might need `--thorough` flag.
4. **Per-check timeout**: Some checks (Docker, git) could hang. Add timeouts in v2 if needed.
