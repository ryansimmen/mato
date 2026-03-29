# Implementation Plan: State Machine Formalization

## 1. Goal

Centralize validation and execution of task queue state transitions so the queue
state diagram is explicit, testable, and reusable. Currently, 18+ transitions
across 6+ files use `AtomicMove` directly with bespoke move+record+rollback
patterns. This refactor introduces a validated core that checks transitions
against a declared edge set, with existing wrapper functions migrated to use it.

## 2. Scope

### In scope

- A new `internal/queue/transition.go` file containing:
  - A `State` type aliasing queue directory name strings, with constants
    derived from existing `DirXxx` constants
  - A declared legal-edge table
  - `ValidateTransition(from, to State) error` function
  - `TransitionTask(tasksDir, filename string, from, to State) error` function
    that validates then calls `AtomicMove`
- A new `internal/queue/transition_test.go` file with exhaustive tests for
  legal and illegal edges
- An explicit classified transition inventory (Appendix A)
- Incremental migration of existing call sites to use the validated core
- No new wrapper abstractions — existing functions are modified in place

### Out of scope

- New queue directories, frontmatter fields, or CLI flags
- Replacing the filesystem-backed queue with a database
- A generic workflow engine or DSL
- The transition journal implementation (follow-up work; journaling should fire
  at wrapper completion points, not inside `TransitionTask`)
- Reclassifying business rules (dependency checks, overlap checks, retry
  budgets) as state machine logic

## 3. Design

### 3.1 State Type and Constants

Place in `internal/queue/transition.go` within the existing `queue` package.

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
```

This derives from the existing `DirXxx` constants (re-exported from
`internal/dirs`), preserving the single source of truth. No string literals
are duplicated.

A package-level `allStates` set is maintained for validation:

```go
var allStates = map[State]struct{}{
    StateWaiting: {}, StateBacklog: {}, StateInProgress: {},
    StateReadyReview: {}, StateReadyMerge: {},
    StateCompleted: {}, StateFailed: {},
}
```

### 3.2 Legal Edge Table

```go
var legalEdges = map[State]map[State]struct{}{
    StateWaiting:     {StateBacklog: {}, StateFailed: {}},
    StateBacklog:     {StateWaiting: {}, StateInProgress: {}, StateFailed: {}},
    StateInProgress:  {StateBacklog: {}, StateReadyReview: {}, StateFailed: {}},
    StateReadyReview: {StateBacklog: {}, StateReadyMerge: {}, StateFailed: {}},
    StateReadyMerge:  {StateBacklog: {}, StateCompleted: {}, StateFailed: {}},
    StateCompleted:   {},
    StateFailed:      {},
}
// Note: failed -> backlog (operator retry) is deliberately excluded.
// RetryTask uses write+remove (not AtomicMove) and strips failure markers.
// It remains a specialized helper outside the transition layer.
```

### 3.3 Validation Function

```go
var errIllegalTransition = errors.New("illegal state transition")

func ValidateTransition(from, to State) error
```

Behavior:

- Returns error wrapping `errIllegalTransition` if `from` is not in `allStates`.
- Returns error wrapping `errIllegalTransition` if `to` is not in `allStates`.
- Returns error wrapping `errIllegalTransition` if `from == to`.
- Returns error wrapping `errIllegalTransition` if the edge is not in
  `legalEdges`.
- Returns `nil` for legal edges.

### 3.4 TransitionTask Function

```go
func TransitionTask(tasksDir, filename string, from, to State) error
```

1. Calls `ValidateTransition(from, to)`. Returns error immediately if invalid.
2. Builds `src = filepath.Join(tasksDir, string(from), filename)` and
   `dst = filepath.Join(tasksDir, string(to), filename)`.
3. Calls `AtomicMove(src, dst)`.
4. Returns any error from the move.

This function does NOT execute side effects, implement rollback, or fire any
hooks. Side effects and rollback remain the caller's responsibility.

### 3.5 IsIllegalTransition Helper

```go
func IsIllegalTransition(err error) bool {
    return errors.Is(err, errIllegalTransition)
}
```

## 4. Step-by-Step Breakdown

### Step 1: Produce classified transition inventory

**Dependencies:** None

**Deliverable:** A classified inventory of all queue-state transitions,
included as Appendix A. This satisfies the proposal requirement at
`docs/proposals/ideas/state-machine-formalization.md` lines 154–175.

### Step 2: Create `internal/queue/transition.go`

**Dependencies:** Step 1

**File:** `internal/queue/transition.go` (new)

Create:

- `State` type and 7 state constants (derived from `DirXxx`)
- `allStates` set (unexported)
- `legalEdges` map (unexported)
- `errIllegalTransition` sentinel (unexported)
- `ValidateTransition(from, to State) error`
- `TransitionTask(tasksDir, filename string, from, to State) error`
- `IsIllegalTransition(err error) bool`

### Step 3: Create `internal/queue/transition_test.go`

**Dependencies:** Step 2

**File:** `internal/queue/transition_test.go` (new)

Tests:

- `TestValidateTransition_LegalEdges` — table-driven, all 14 legal edges
  return nil.
- `TestValidateTransition_IllegalEdges` — table-driven, all 35 non-legal state
  pairs (7 same-state + 28 cross-state illegal) return `errIllegalTransition`.
- `TestValidateTransition_UnknownState` — unrecognized strings in `from` and
  `to` positions.
- `TestTransitionTask_Success` — temp dirs via `t.TempDir()`, place task file,
  call `TransitionTask`, verify moved.
- `TestTransitionTask_IllegalEdge` — verifies error returned, file not moved.
- `TestTransitionTask_MoveFailure` — source file missing, verifies error
  propagation.
- `TestTransitionTask_DestinationExists` — verifies `ErrDestinationExists`
  propagation.
- `TestIsIllegalTransition` — error predicate with wrapped and unwrapped
  errors.

### Step 4: Migrate `reconcile.go` plain moves

**Dependencies:** Step 3

**File:** `internal/queue/reconcile.go` (modified)

Replace 6 bare `AtomicMove` calls with `TransitionTask`:

| Transition | Approx. line | Replacement |
|------------|-------------|-------------|
| waiting → backlog (promotion) | ~245 | `TransitionTask(tasksDir, snap.Filename, StateWaiting, StateBacklog)` |
| backlog → waiting (demotion) | ~115 | `TransitionTask(tasksDir, snap.Filename, StateBacklog, StateWaiting)` |
| waiting → failed (cycle) | ~202 | `TransitionTask(tasksDir, snap.Filename, StateWaiting, StateFailed)` |
| waiting → failed (dup/glob) | ~162, ~237 | `TransitionTask(tasksDir, snap.Filename, StateWaiting, StateFailed)` |
| backlog → failed (parse/glob) | ~83, ~98 | `TransitionTask(tasksDir, pf.Filename, StateBacklog, StateFailed)` |

Error handling unchanged: warning on failure, no rollback.

**Existing tests exercising these paths:**
`TestReconcileReadyQueue_*` in `reconcile_test.go`.

### Step 5: Migrate `cancel.go`

**Dependencies:** Step 3

**File:** `internal/queue/cancel.go` (modified)

Replace `AtomicMove(match.Path, failedPath)` at L58 with:

```go
TransitionTask(tasksDir, match.Filename, State(match.State), StateFailed)
```

Keep existing rollback logic (L69–73) and `appendCancelledRecordFn` hook
unchanged. The `ErrDestinationExists` check (L59) still works because
`TransitionTask` propagates `AtomicMove` errors. Add handling for
`IsIllegalTransition` as a defense-in-depth guard.

**Existing tests exercising this path:**
`TestCancelTask_MovesTaskToFailed` (all states) in `cancel_test.go`.

### Step 6: Migrate `claim.go`

**Dependencies:** Step 3

**File:** `internal/queue/claim.go` (modified)

1. **backlog → in-progress** (~L262): Replace `AtomicMove(src, dst)` with
   `TransitionTask(tasksDir, name, StateBacklog, StateInProgress)`. Keep
   post-move `claimPrependFn` and rollback via `claimRollbackFn`.

2. **backlog → waiting** (~L239): Replace `AtomicMove(src, waitingPath)` with
   `TransitionTask(tasksDir, name, StateBacklog, StateWaiting)`.

3. **in-progress → failed** (retry-exhausted, ~L164): Add
   `ValidateTransition(StateInProgress, StateFailed)` before invoking
   `retryExhaustedMoveFn`. The hook is preserved for testability; it still
   receives raw paths.

**Existing tests exercising these paths:**
`TestSelectAndClaimTask_Normal`, `TestSelectAndClaimTask_ClaimPrependFailure_*`,
`TestSelectAndClaimTask_RetryExhausted*` in `claim_test.go`.

### Step 7: Migrate `runner/task.go`

**Dependencies:** Step 3

**File:** `internal/runner/task.go` (modified)

1. **in-progress → ready-for-review** (`moveTaskToReviewWithMarker`, ~L195):
   Replace `queue.AtomicMove(claimed.TaskPath, readyPath)` with
   `queue.TransitionTask(tasksDir, claimed.Filename, queue.StateInProgress, queue.StateReadyReview)`.
   Keep rollback.

2. **in-progress → backlog** (`recoverStuckTask`, ~L223): Replace
   `queue.AtomicMove(claimed.TaskPath, dst)` with
   `queue.TransitionTask(tasksDir, claimed.Filename, queue.StateInProgress, queue.StateBacklog)`.
   Keep post-move failure record.

**Existing tests exercising these paths:**
`TestMoveTaskToReviewWithMarker_*`, `TestRecoverStuckTask_*` in
`runner/task_test.go`.

### Step 8: Migrate `runner/review.go`

**Dependencies:** Step 3

**File:** `internal/runner/review.go` (modified)

1. **ready-for-review → ready-to-merge/backlog** (`moveReviewedTask`, ~L424):
   Replace `queue.AtomicMove(task.TaskPath, dst)` with
   `queue.TransitionTask(tasksDir, task.Filename, queue.StateReadyReview, queue.State(disp.dir))`.
   `disp.dir` is `queue.DirReadyMerge` or `queue.DirBacklog`.

2. **ready-for-review → failed** (quarantine in `reviewCandidates`): Replace
   `queue.AtomicMove(...)` calls with `queue.TransitionTask(...)` using
   `queue.StateReadyReview` → `queue.StateFailed`.

**Existing tests exercising these paths:**
`TestPostReviewAction_*`, `TestReviewCandidates_*` in `runner/review_test.go`.

### Step 9: Migrate merge package

**Dependencies:** Step 3

**Files:** `internal/merge/merge.go` and `internal/merge/taskops.go` (both
modified)

Three validation points cover all merge transitions. `failMergeTask` and
`moveTaskWithRetry` remain path-based helpers because they serve as generic
path movers and existing tests (`taskops_test.go`) rely on that looser
contract. Validation happens at the boundary callers where queue-state context
is available.

**9a. `handleMergeFailure` in `taskops.go` (L17–25):**

`handleMergeFailure` already calls `mergeFailureDestination(tasksDir,
task.path, task.name)` which computes the destination directory (`DirBacklog`
or `DirFailed`). Extract a small helper `mergeFailureDestDir(taskPath string)
string` that returns the raw directory name. Call
`queue.ValidateTransition(queue.StateReadyMerge, queue.State(destDir))` in
`handleMergeFailure` before calling `failMergeTask`.

This is the validation site for the `ready-to-merge → backlog|failed` merge-
failure path (`executeMergeRound → handleMergeFailure → failMergeTask`).

**9b. `loadMergeCandidates` in `merge.go` (~L86):**

Before the `failMergeTask(path, backlogDst, reason)` call, add:

```go
if err := queue.ValidateTransition(queue.StateReadyMerge, queue.StateBacklog); err != nil {
    // defense-in-depth; log and continue
}
```

This is the validation site for the `ready-to-merge → backlog` parse-failure
requeue path.

**9c. `executeMergeRound` in `merge.go` (~L129, ~L176):**

Before `moveTaskWithRetry(task.path, completedPath)` calls, add:

```go
if err := queue.ValidateTransition(queue.StateReadyMerge, queue.StateCompleted); err != nil {
    // defense-in-depth; log and continue
}
```

This is the validation site for the `ready-to-merge → completed` success path.

**Existing tests unchanged:** `failMergeTask` and `moveTaskWithRetry` remain
path-based, so `taskops_test.go` tests continue to work. Validation calls are
at the caller level.

**Existing tests exercising these paths:**
`TestProcessQueue_*` in `merge_test.go`;
`TestFailMergeTask_*`, `TestMoveTaskWithRetry_*` in `taskops_test.go`.

### Step 10: Migrate `queue.go` RecoverOrphanedTasks

**Dependencies:** Step 3

**File:** `internal/queue/queue.go` (modified)

Replace `AtomicMove(src, dst)` at ~L91 in `RecoverOrphanedTasks` with
`TransitionTask(tasksDir, name, StateInProgress, StateBacklog)`.

Keep collision resolution logic (`ErrDestinationExists` handling) and failure
record write unchanged.

**Existing tests exercising this path:**
`TestRecoverOrphanedTasks_*` in `queue_test.go`.

### Step 11: Run full test suite

**Dependencies:** Steps 4–10

```bash
go build ./... && go vet ./... && go test -race -count=1 ./...
```

## 5. File Changes

| File | Action | Description |
|------|--------|-------------|
| `internal/queue/transition.go` | Create | State type (derived from DirXxx), constants, legal edges, ValidateTransition, TransitionTask, IsIllegalTransition |
| `internal/queue/transition_test.go` | Create | Exhaustive legal/illegal edge tests (14 legal + 35 illegal), TransitionTask filesystem tests, error predicate tests |
| `internal/queue/reconcile.go` | Modify | Replace 6 bare AtomicMove calls with TransitionTask |
| `internal/queue/cancel.go` | Modify | Replace 1 AtomicMove call with TransitionTask |
| `internal/queue/claim.go` | Modify | Replace 2 AtomicMove calls with TransitionTask, add ValidateTransition before hook-based move |
| `internal/queue/queue.go` | Modify | Replace 1 AtomicMove call in RecoverOrphanedTasks with TransitionTask |
| `internal/runner/task.go` | Modify | Replace 2 AtomicMove calls with TransitionTask |
| `internal/runner/review.go` | Modify | Replace 3+ AtomicMove calls with TransitionTask |
| `internal/merge/merge.go` | Modify | Add ValidateTransition calls before moveTaskWithRetry and failMergeTask |
| `internal/merge/taskops.go` | Modify | Add ValidateTransition in handleMergeFailure; extract mergeFailureDestDir helper |

## 6. Error Handling

- **Illegal transitions**: `ValidateTransition` returns errors wrapping
  `errIllegalTransition`. Callers check with `IsIllegalTransition(err)`.
- **Unknown states**: Wraps `errIllegalTransition` with descriptive message.
- **Same-state transitions**: Wraps `errIllegalTransition` with "(same state)"
  note.
- **Move failures**: `TransitionTask` propagates `AtomicMove` errors unchanged,
  including `ErrDestinationExists`.
- **Rollback**: Existing rollback patterns preserved. `TransitionTask` does NOT
  implement rollback.
- **Side effects**: `TransitionTask` does NOT execute side effects. Side
  effects remain at call sites in their existing order.

## 7. Testing Strategy

### New unit tests (transition_test.go)

- **Exhaustive legal edge coverage**: All 14 legal edges return nil.
- **Exhaustive illegal edge coverage**: All 35 non-legal state pairs (including
  same-state and cross-state illegal) return `errIllegalTransition`.
- **TransitionTask filesystem tests**: Temp dirs, verify moves, verify illegal
  edges block moves, verify move failure and `ErrDestinationExists`
  propagation.
- **IsIllegalTransition predicate**: Wrapped and unwrapped error matching.

### Existing test suite (safety net)

All existing tests pass without modification because:

1. `TransitionTask` composes `ValidateTransition` + `AtomicMove`, producing
   identical filesystem behavior for legal edges.
2. Function-variable hooks preserved at all call sites.
3. Side-effect ordering unchanged.
4. Merge helpers remain path-based; validation at caller level.

### Race detection

All test runs include `-race` flag:
`go test -race -count=1 ./...`

## 8. Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| Wrong `State` constant for `from` at migrated sites | Full test suite run after each step; existing tests cover all key transitions |
| `TransitionTask` builds different paths | Path construction matches existing pattern: `filepath.Join(tasksDir, string(state), filename)` |
| Function-variable hooks bypass validation | `ValidateTransition` called separately before hook invocation |
| `State(match.State)` invalid in CancelTask | L40 guard excludes completed; remaining states in edge table; ValidateTransition adds defense-in-depth |
| RetryTask bypasses edge table | Deliberate; write+remove, not AtomicMove; documented in source |
| Merge validation gap | Three validation points cover all merge paths: handleMergeFailure, loadMergeCandidates, executeMergeRound |
| Performance | Two map lookups per transition; negligible vs filesystem I/O |

## 9. Open Questions

1. **Should `failed → backlog` be a legal edge?** Currently, `RetryTask` uses
   write+remove, not a simple move. *Recommendation: exclude for now. Add the
   edge if future retry logic changes to a simple move.*

2. **Should `State` live in `internal/dirs`?** The `dirs` package is a minimal
   constants-only package. Adding validation logic there would change its
   character. `merge` already imports `queue`. *Recommendation: keep in
   `queue`, derived from `DirXxx` constants.*

3. **Should wrappers return typed errors?** The sentinel `errIllegalTransition`
   with `%w` wrapping is consistent with `ErrDestinationExists`.
   *Recommendation: sentinel + fmt.Errorf wrapping is enough.*

4. **Which function-variable hooks can be removed?** All existing hooks test
   side-effect behavior independent of transition validation.
   *Recommendation: keep all hooks.*

## Appendix A: Classified Transition Inventory

### Class 1: Plain move (no side effects on moved file, no rollback)

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 1 | waiting | backlog | reconcile.go | ReconcileReadyQueue | TransitionTask in reconcile.go |
| 2 | backlog | waiting | reconcile.go | ReconcileReadyQueue | TransitionTask in reconcile.go |
| 3 | backlog | waiting | claim.go | SelectAndClaimTask | TransitionTask in claim.go |

### Class 2: Write marker then move (marker before move, warn-only on failure)

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 4 | waiting | failed | reconcile.go | ReconcileReadyQueue (cycle/dup/glob) | TransitionTask in reconcile.go |
| 5 | backlog | failed | reconcile.go | ReconcileReadyQueue (parse/glob) | TransitionTask in reconcile.go |
| 6 | ready-for-review | failed | review.go | reviewCandidates (budget/parse) | TransitionTask in review.go |

### Class 3: Write marker then move (marker before move, then messaging)

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 7 | ready-for-review | ready-to-merge | review.go | postReviewAction (approval) | TransitionTask in review.go |
| 8 | ready-for-review | backlog | review.go | postReviewAction (rejection) | TransitionTask in review.go |

### Class 4: Write record then move (failure record before move)

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 9 | ready-to-merge | backlog/failed | taskops.go | failMergeTask | ValidateTransition in handleMergeFailure (taskops.go) |

### Class 5: Move then post-write with rollback

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 10 | backlog | in-progress | claim.go | SelectAndClaimTask | TransitionTask in claim.go |
| 11 | in-progress | ready-for-review | task.go | moveTaskToReviewWithMarker | TransitionTask in task.go |
| 12 | any* | failed | cancel.go | CancelTask | TransitionTask in cancel.go |

### Class 6: Move then post-write without rollback (warn only)

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 13 | in-progress | backlog | task.go | recoverStuckTask | TransitionTask in task.go |
| 14 | in-progress | backlog | queue.go | RecoverOrphanedTasks | TransitionTask in queue.go |

### Class 7: Move with retry

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 15 | ready-to-merge | completed | taskops.go | moveTaskWithRetry | ValidateTransition in executeMergeRound (merge.go) |

### Class 8: Hook-based move with separate validation

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 16 | in-progress | failed | claim.go | handleRetryExhaustedTask | ValidateTransition in claim.go |

### Class 9: Specialized non-move (excluded from transition layer)

| # | From | To | Current location | Function | Validation site |
|---|------|----|------------------|----------|-----------------|
| 17 | failed | backlog | retry.go | RetryTask (write+remove) | Excluded |
