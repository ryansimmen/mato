# Combined Implementation Plan: State Machine Formalization + `mato log` Command

## 1. Goal

Deliver two sequenced improvements to the mato task orchestrator:

1. **State Machine Formalization** — make queue transitions explicit and
   validated instead of relying on scattered `AtomicMove` call sites.
2. **`mato log` Command** — add a history-oriented command that answers
   "what happened recently?" from durable on-disk sources.

The recommended order remains the same: land the transition core first, migrate
real call sites second, then build `mato log` on top of the settled queue
semantics.

## 2. Current Code State

As of the current codebase:

- There is **no** `internal/queue/transition.go` yet.
- There is **no** `internal/history/` package and **no** `mato log` command.
- Canonical queue transitions still use raw `AtomicMove` across
  `internal/queue/`, `internal/runner/`, and `internal/merge/`.
- The host now maintains runtime metadata in `internal/taskstate/` and cleans it
  up through `internal/runtimecleanup/`; any transition refactor must preserve
  those side effects and boundaries.
- Review flow now uses ephemeral verdict files in
  `.mato/messages/verdict-<task>.json`; those are coordination artifacts, not a
  durable history source.
- Merge flow already writes durable completion-detail records in
  `.mato/messages/completions/*.json`.
- `messaging.ReadAllCompletionDetails()` is not sufficient for `mato log`
  because it still fails fast on non-`IsNotExist` per-file read errors.

## 3. Scope

### In scope

**Phase A — State Machine Formalization**

- Add `State`, `StateFromDir`, `DirFromState`, `ValidateTransition`,
  `TransitionTask`, and exhaustive tests in `internal/queue/`
- Migrate real queue transitions incrementally
- Use `TransitionTask` for same-filename moves and `ValidateTransition` for
  paths that keep bespoke move helpers or change destination filenames
- Preserve existing side effects and ordering, including marker writes,
  `taskstate` updates, session metadata, runtime cleanup, messages, and
  rollback behavior

**Phase B — `mato log` Command**

- Add `log` subcommand with `--limit` and `--format text|json`
- Gather `MERGED`, `FAILED`, and `REJECTED` events from durable on-disk sources
- Reuse existing marker-scanning rules in `internal/taskfile/`
- Add resilient collection, sorting, renderers, CLI tests, integration tests,
  and docs

**Phase C — Transition Journal (follow-up)**

- Add wrapper-level append-only journaling after Phases A and B are stable

### Out of scope

- New queue directories, frontmatter fields, or scheduler semantics
- Modeling `mato retry` as a normal queue move; retry still rewrites content and
  strips failure markers
- Moving business logic (dependency checks, retry budgets, overlap checks,
  branch cleanup, taskstate writes) into `TransitionTask`
- Using `runtime/taskstate/*.json`, verdict files, or `.queue` as phase-1 log
  sources
- Phase-1 `CANCELLED`, `REVIEW_FAILURE`, `TERMINAL_FAILURE`, or
  `CYCLE_FAILURE` event types in `mato log`
- `--since`, `--until`, `--task`, or other advanced history filters

## 4. Design

### 4.1 State Machine Core (`internal/queue/`)

**Why in `internal/queue/`?** The validated core wraps the existing
`AtomicMove` primitive, and `runner` plus `merge` already depend on `queue`.

**New file:** `internal/queue/transition.go`

**New tests:** `internal/queue/transition_test.go`

**Types and functions**

```go
type State string

const (
    StateWaiting     State = State(DirWaiting)
    StateBacklog     State = State(DirBacklog)
    StateInProgress  State = State(DirInProgress)
    StateReadyReview State = State(DirReadyReview)
    StateReadyMerge  State = State(DirReadyMerge)
    StateCompleted   State = State(DirCompleted)
    StateFailed      State = State(DirFailed)
)

func StateFromDir(dir string) (State, error)
func DirFromState(state State) string
func ValidateTransition(from, to State) error
func TransitionTask(tasksDir, filename string, from, to State) error
func IsIllegalTransition(err error) bool
```

**Behavioral rules**

- `StateFromDir` returns an error for unknown directory names.
- `DirFromState` is a small validated-state helper and may panic on unknown
  input to keep misuse obvious.
- `ValidateTransition` checks unknown states, same-state transitions, and edges
  outside the legal table.
- `TransitionTask` validates then moves from
  `filepath.Join(tasksDir, string(from), filename)` to
  `filepath.Join(tasksDir, string(to), filename)`.
- `TransitionTask` is for **same-filename** queue moves only.
- When a caller changes the destination filename, keeps an existing path-based
  helper, or performs a rollback move, it should call `ValidateTransition`
  separately and keep the bespoke move logic.

**Legal edges (14)**

| From | To |
|---|---|
| waiting | backlog |
| waiting | failed |
| backlog | waiting |
| backlog | in-progress |
| backlog | failed |
| in-progress | backlog |
| in-progress | ready-for-review |
| in-progress | failed |
| ready-for-review | backlog |
| ready-for-review | ready-to-merge |
| ready-for-review | failed |
| ready-to-merge | backlog |
| ready-to-merge | completed |
| ready-to-merge | failed |

`completed` and `failed` remain terminal from the state-machine perspective.

**Important non-goals**

- Do not write markers from `TransitionTask`.
- Do not update `taskstate`, session metadata, or runtime cleanup from
  `TransitionTask`.
- Do not hide wrapper-level rollback behavior inside the state machine.

### 4.2 Canonical Transition Inventory

The inventory below matches the current code, including rename-path and
path-helper exceptions.

| # | From -> To | Current location | Current sequence | Migration target |
|---|---|---|---|---|
| 1 | waiting -> failed | `internal/queue/reconcile.go` waiting parse failures | append terminal-failure -> move | `ValidateTransition` + existing sequence |
| 2 | backlog -> failed | `internal/queue/reconcile.go` backlog parse failures | append terminal-failure -> move | `ValidateTransition` + existing sequence |
| 3 | backlog -> failed | `internal/queue/reconcile.go` invalid backlog glob | append terminal-failure -> move | `ValidateTransition` + existing sequence |
| 4 | backlog -> waiting | `internal/queue/reconcile.go` dep-blocked demotion | plain move | `TransitionTask` |
| 5 | waiting -> failed | `internal/queue/reconcile.go` duplicate waiting ID | append terminal-failure -> move | `ValidateTransition` + existing sequence |
| 6 | waiting -> failed | `internal/queue/reconcile.go` cycle quarantine | append cycle-failure -> move | `ValidateTransition` + existing sequence |
| 7 | waiting -> failed | `internal/queue/reconcile.go` invalid waiting glob | append terminal-failure -> move | `ValidateTransition` + existing sequence |
| 8 | waiting -> backlog | `internal/queue/reconcile.go` promotion | plain move | `TransitionTask` |
| 9 | backlog -> waiting | `internal/queue/claim.go` dependency safety net | plain move | `TransitionTask` |
| 10 | backlog -> in-progress | `internal/queue/claim.go` claim path | move -> claimed-by/branch writes -> rollback on failure | `TransitionTask` for forward move; keep rollback |
| 11 | in-progress -> failed | `internal/queue/claim.go` retry exhausted | path helper + rollback to backlog on failure | `ValidateTransition` before existing helper path |
| 12 | in-progress -> backlog | `internal/queue/queue.go` orphan recovery | move -> append failure | `TransitionTask` |
| 13 | in-progress -> backlog | `internal/queue/queue.go` orphan collision rename | move to `*-recovered-<timestamp>` -> append failure later | `ValidateTransition` + raw `AtomicMove` |
| 14 | any non-completed, non-failed state -> failed | `internal/queue/cancel.go` cancel path | move -> append cancelled -> rollback on failure | `ValidateTransition` + existing sequence |
| 15 | in-progress -> ready-for-review | `internal/runner/task.go` review handoff | move -> write branch marker -> rollback on failure | `TransitionTask` for forward move; keep rollback |
| 16 | in-progress -> backlog | `internal/runner/task.go` stuck-task recovery | move -> maybe append failure | `TransitionTask` |
| 17 | ready-for-review -> failed | `internal/runner/review.go` malformed/review-exhausted quarantine | append terminal-failure -> move | `ValidateTransition` + existing sequence |
| 18 | ready-for-review -> ready-to-merge | `internal/runner/review.go` approval | append reviewed -> move -> taskstate/message | `ValidateTransition` + existing sequence |
| 19 | ready-for-review -> backlog | `internal/runner/review.go` rejection | append rejection -> move -> taskstate/message | `ValidateTransition` + existing sequence |
| 20 | ready-to-merge -> completed | `internal/merge/merge.go` success path | write completion detail -> append merged -> moveWithRetry -> cleanup | `ValidateTransition` before existing helper path |
| 21 | ready-to-merge -> backlog/failed | `internal/merge/taskops.go` failure path | choose destination -> append failure -> move | `ValidateTransition` before existing helper path |

**Excluded from the transition layer**

- Rollback moves used for operational recovery
- Same-state cleanup/removal paths
- Already-failed cancel marker-only path
- Retry requeue (`failed -> backlog`) because it rewrites content instead of
  performing a plain move

### 4.3 `mato log` Command

**Command shape**

- Add `newLogCmd(repoFlag *string)` in `cmd/mato/main.go`
- Reuse the existing read-only command pattern used by `status`, `inspect`, and
  `graph`: the CLI resolves the optional `--repo` input, and the history
  package resolves the git top-level plus `.mato/` path internally
- Follow the existing CLI testing pattern with a `logShowFn` function variable

**Package boundary**

- `cmd/mato/main.go` validates flags, resolves the optional repo input, and
  delegates to `internal/history/`
- `internal/history/` resolves the git top-level, derives `tasksDir`, and
  validates that `.mato/` exists before collecting history
- `internal/taskfile/` grows typed marker-parsing helpers that reuse existing
  marker scanning behavior

**Public entry points**

To match the existing read-only command packages, `internal/history/` should
export repo-oriented entry points rather than a `tasksDir`-oriented public API:

```go
func Show(repo string, limit int, format string) error
func ShowTo(w io.Writer, repo string, limit int, format string) error
```

Here `repo` follows the same convention as `status.Show`, `inspect.Show`, and
`graph.Show`: it is the CLI-resolved repo argument or current working
directory, and the package itself resolves the canonical git top-level.

Any lower-level collector helpers may still accept `tasksDir` internally, but
that should not be the exported command-facing API.

**Durable sources for phase 1**

- `MERGED` from `.mato/messages/completions/*.json`
- `FAILED` from `<!-- failure: ... -->` task markers
- `REJECTED` from `<!-- review-rejection: ... -->` task markers

**Explicit non-sources for phase 1**

- `.mato/runtime/taskstate/*.json` (latest runtime snapshot, not append-only)
- `.mato/messages/verdict-*.json` (ephemeral coordination files)
- `.mato/.queue` or other derived snapshots

**Event model**

```go
type Event struct {
    Timestamp    time.Time `json:"timestamp"`
    Type         string    `json:"type"`
    TaskFile     string    `json:"task_file"`
    Title        string    `json:"title,omitempty"`
    CurrentState string    `json:"current_state,omitempty"`
    Branch       string    `json:"branch,omitempty"`
    CommitSHA    string    `json:"commit_sha,omitempty"`
    FilesChanged []string  `json:"files_changed,omitempty"`
    Reason       string    `json:"reason,omitempty"`
    AgentID      string    `json:"agent_id,omitempty"`
}
```

**Marker parser shape**

```go
type MarkerRecord struct {
    Timestamp time.Time
    AgentID   string
    Reason    string
}

func ParseFailureMarkers(data []byte) []MarkerRecord
func ParseReviewRejectionMarkers(data []byte) []MarkerRecord
```

These helpers should build on `forEachMarkerLine` and the existing
`failureReasonFromLine` rules so the command matches the repo's current marker
semantics, including both `error=` and em-dash reason formats.

**Collection strategy**

1. `newLogCmd` resolves `repo` and calls `history.Show(repo, limit, format)` or
   `history.ShowTo(...)`.
2. `history.Show`/`ShowTo` resolve `repoRoot`, derive `tasksDir`, validate
   `.mato/`, then delegate to collector helpers.
3. The collector scans `messages/completions/` with per-file warn-and-skip
   behavior rather than `ReadAllCompletionDetails()`.
4. The collector scans every queue directory in `queue.AllDirs`, reads task
   files, extracts title best-effort, and emits one event per durable marker.
5. For merged events, `CurrentState` uses two-tier resolution: first a unique
   `TaskID` match from frontmatter-derived IDs, then a unique filename match.
   Ambiguous or conflicting matches leave `CurrentState` empty.
6. Sort newest first, then `task_file`, then `type`; apply `--limit` after
   sorting.

**Source-status model**

Each primary source should report one of:

- `absent`: directory missing
- `read`: directory successfully scanned, even if empty
- `failed`: non-`IsNotExist` access error

Return a fatal error only when **no** source reached `read` and **at least one**
source reached `failed`.

**Testing hooks**

`internal/history/` may use package-level `readDirFn`/`readFileFn` hooks for
error-injection tests. Tests that override them should avoid `t.Parallel()`.

### 4.4 Transition Journal (Phase C)

The journal is still a follow-up. If added later, it should:

- emit at wrapper success boundaries, not from `TransitionTask`
- live in a host-owned durable location alongside other durable queue metadata
- complement or eventually replace marker-derived history for `mato log`

The exact file layout is intentionally deferred until Phases A and B are done.

## 5. Recommended Delivery Sequence

### Phase A — State Machine Formalization

**A1. Transition core**

- Add `internal/queue/transition.go`
- Add exhaustive tests in `internal/queue/transition_test.go`
- No call-site migration yet

**A2. Queue package migration**

- Migrate `internal/queue/reconcile.go`, `internal/queue/claim.go`,
  `internal/queue/cancel.go`, and `internal/queue/queue.go`
- Use `TransitionTask` for same-filename moves
- Use `ValidateTransition` for helper-based or renamed-destination paths

**A3. Cross-package migration**

- Migrate `internal/runner/task.go`, `internal/runner/review.go`,
  `internal/merge/merge.go`, and `internal/merge/taskops.go`
- Preserve `taskstate`, session metadata, messaging, runtime cleanup, and git
  side effects in their current wrappers
- Update `docs/architecture.md` and `AGENTS.md`

**A4. Verification**

```bash
go build ./... && go vet ./... && go test -count=1 ./...
```

### Phase B — `mato log` Command

**B1. History core**

- Add typed marker parsing in `internal/taskfile/`
- Add `internal/history/` collector and renderers
- Add unit tests for mixed histories, resilience, sorting, and empty queues

**B2. CLI wiring and docs**

- Add `newLogCmd()` and `logShowFn`
- Add command-level tests in `cmd/mato/main_test.go`
- Add end-to-end integration coverage in `internal/integration/`
- Update `README.md`

### Phase C — Transition Journal (follow-up)

- Define append-only journal format
- Emit journal entries from wrapper success points
- Teach `mato log` to read journal data

## 6. File Changes

### New files

| File | Purpose |
|---|---|
| `internal/queue/transition.go` | State type, edge table, validation, same-filename move helper |
| `internal/queue/transition_test.go` | Exhaustive transition tests |
| `internal/history/` | History collector, rendering, and tests for `mato log` |

### Modified files

| File | Changes |
|---|---|
| `internal/queue/reconcile.go` | Transition validation and/or move helper usage |
| `internal/queue/claim.go` | Transition validation and/or move helper usage |
| `internal/queue/cancel.go` | Transition validation around cancel path |
| `internal/queue/queue.go` | Transition validation for orphan recovery, including rename exception |
| `internal/runner/task.go` | Transition validation for review handoff and stuck-task recovery |
| `internal/runner/review.go` | Transition validation for approval/rejection/quarantine paths |
| `internal/merge/merge.go` | Transition validation for merge success path |
| `internal/merge/taskops.go` | Transition validation for merge failure path |
| `internal/taskfile/metadata.go` | Typed marker parsing helpers for history collection |
| `internal/taskfile/metadata_test.go` | Marker parser tests |
| `cmd/mato/main.go` | `newLogCmd`, `logShowFn`, command registration |
| `cmd/mato/main_test.go` | CLI validation/delegation tests for `mato log` |
| `README.md` | User-facing command docs |
| `docs/architecture.md` | Transition layer and, once shipped, history/log behavior |
| `AGENTS.md` | Small convention update pointing future edge changes at `transition.go` |

## 7. Error Handling

### State Machine

- Illegal transitions return an error that callers can test with
  `IsIllegalTransition`
- `TransitionTask` propagates `AtomicMove` errors, including destination
  collisions
- Existing rollback flows stay in wrapper code
- Validation-only paths keep their existing path-based helpers and warnings

### `mato log`

- Missing `.mato/` is rejected by `internal/history.Show`/`ShowTo` before
  collection begins
- Missing queue subdirectories or missing `messages/completions/` are treated as
  `absent`, not fatal
- Per-file completion or task-file read failures warn and skip
- Malformed markers are skipped silently at the marker-parser level
- Fatal only when no source is readable and at least one source fails

## 8. Testing

### Phase A

- Exhaustive legal/illegal transition coverage
- Filesystem move tests for `TransitionTask`
- Existing queue/runner/merge tests remain the behavioral safety net

### Phase B

- Typed marker parser tests for both failure formats and rejection markers
- Collection tests for mixed events, ordering, limits, empty queues, malformed
  completion JSON, malformed frontmatter with readable markers, and aggregate
  source failure
- CLI tests for flag validation and delegation
- Integration tests for merged, failed, rejected, and empty-history scenarios

## 9. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Wrong edge table or validation behavior | Exhaustive unit tests before migration |
| Call-site migration breaks current side effects | Keep side effects in wrappers; use existing test suite as safety net |
| `RecoverOrphanedTasks` rename path is force-fit into `TransitionTask` | Document it as `ValidateTransition` + raw move only |
| `mato log` accidentally reads ephemeral runtime data | Restrict phase-1 sources to completions and task markers |
| Completion-detail scan aborts on one bad file | Use a custom per-file collector instead of `ReadAllCompletionDetails()` |
| Future queue edge added without state table update | Document `transition.go` as the canonical edge list |

## 10. Resolved Decisions

1. **Retry stays outside the state machine for now.** `RetryTask` is currently a
   write-cleaned-copy-to-`backlog/` operation followed by best-effort removal of
   the `failed/` source, not a same-file queue move. It should remain outside
   `ValidateTransition`/`TransitionTask` unless retry semantics are later
   redesigned around a real move-based transition.
2. **Phase C journal should live under `.mato/messages/history/`.** That keeps
   durable history adjacent to other durable history inputs such as
   `messages/completions/`, avoids conflating append-only history with runtime
   snapshots under `runtime/`, and gives `mato log` a single history-oriented
   subtree to read from. The exact file granularity (single JSONL file vs.
   sharded files) can still be decided when Phase C is implemented.
