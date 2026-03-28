# Implementation Plan: State Machine Formalization

## 1. Goal

Centralize scattered `AtomicMove` call sites for task state transitions behind a
validated `TransitionTask()` function with a declarative transition table. This
eliminates duplicated move+record+rollback patterns, prevents invalid state
transitions at runtime, provides a single hook point for the transition journal,
and makes adding new queue states cheaper.

## 2. Scope

### In Scope

- Declarative transition tables (lifecycle + rollback-only)
- Core `TransitionTask()`: validate + ensure destination directory + `AtomicMove`
  + journal
- Higher-level transition helpers for 5 common patterns
- Migration of `AtomicMove` call sites across 7 files (with documented
  exclusions)
- Reduction of 3 function-variable test hooks
- Best-effort append-only transition journal (same PR)
- Unit tests, integration tests, documentation updates

### Out of Scope

- New CLI commands, flags, or frontmatter fields
- New queue directories or states
- Changes to `PollIndex` or `AtomicMove`
- Scheduler-decision logging
- `RetryTask` migration — uses `atomicwrite.WriteFile` + `os.Remove`, not
  `AtomicMove`
- Orphan-collision rename in `resolveOrphanCollision` (`queue.go:150`) — changes
  filename during move, stays as direct `AtomicMove`

## 3. Design

### 3.1 Transition Tables

Two separate maps enforce the distinction between normal lifecycle transitions
and internal compensating rollback moves:

```go
// validTransitions: normal task lifecycle edges.
var validTransitions = map[[2]string]bool{
    // Dependency lifecycle
    {DirWaiting, DirBacklog}:        true,  // promotion
    {DirBacklog, DirWaiting}:        true,  // demotion

    // Claim lifecycle
    {DirBacklog, DirInProgress}:     true,  // claim

    // Agent completion
    {DirInProgress, DirReadyReview}: true,  // push

    // Recovery (normal lifecycle — orphan/stuck/ON_FAILURE)
    {DirInProgress, DirBacklog}:     true,

    // Review lifecycle
    {DirReadyReview, DirReadyMerge}: true,  // approval
    {DirReadyReview, DirBacklog}:    true,  // rejection

    // Merge lifecycle
    {DirReadyMerge, DirCompleted}:   true,  // merge
    {DirReadyMerge, DirBacklog}:     true,  // merge failure

    // Failure edges
    {DirWaiting, DirFailed}:         true,
    {DirBacklog, DirFailed}:         true,
    {DirInProgress, DirFailed}:      true,
    {DirReadyReview, DirFailed}:     true,
    {DirReadyMerge, DirFailed}:      true,
}

// validRollbacks: compensating moves with NO normal lifecycle meaning.
// Only allowed when WithAllowRollback() is set.
var validRollbacks = map[[2]string]bool{
    // Branch marker write rollback
    {DirReadyReview, DirInProgress}: true,

    // Cancel rollback: return task to prior state after failed marker write
    {DirFailed, DirBacklog}:         true,
    {DirFailed, DirWaiting}:         true,
    {DirFailed, DirInProgress}:      true,
    {DirFailed, DirReadyReview}:     true,
    {DirFailed, DirReadyMerge}:      true,
}
```

`in-progress → backlog` is in the lifecycle table because it is used for orphan
recovery, stuck-task recovery, and ON_FAILURE requeue — all documented lifecycle
flows. Claim rollback also uses this edge (with reason `"rollback"`); the edge
is the same, the intent is captured in the journal reason code.

### 3.2 Core Function

```go
func TransitionTask(tasksDir, filename, fromState, toState string, opts ...TransitionOption) error
```

Steps:
1. Check `validTransitions[{from, to}]`. If not found and `WithAllowRollback()`
   is set, check `validRollbacks[{from, to}]`. Otherwise return
   `InvalidTransitionError`.
2. `os.MkdirAll(dstDir, 0o755)` — ensure destination directory exists
   (centralizes what `review.go` and `taskops.go` did per-call-site).
3. `AtomicMove(srcPath, dstPath)` — atomic file move.
4. Unless `WithSkipJournal()`: best-effort `writeJournalEntry`.

### 3.3 Options

```go
func WithAgent(agentID string) TransitionOption
func WithReason(reason string) TransitionOption
func WithDetail(detail string) TransitionOption
func WithSkipJournal() TransitionOption
func WithAllowRollback() TransitionOption
```

### 3.4 Reason Code Constants

```go
const (
    ReasonPromotion      = "promotion"
    ReasonDemotion       = "demotion"
    ReasonClaim          = "claim"
    ReasonPush           = "push"
    ReasonApproval       = "approval"
    ReasonRejection      = "rejection"
    ReasonMerge          = "merge"
    ReasonMergeFailure   = "merge-failure"
    ReasonRetryExhausted = "retry-exhausted"
    ReasonQuarantine     = "quarantine"
    ReasonCycle          = "cycle"
    ReasonDuplicate      = "duplicate"
    ReasonOrphanRecovery = "orphan-recovery"
    ReasonStuckRecovery  = "stuck-recovery"
    ReasonCancelled      = "cancelled"
    ReasonRollback       = "rollback"
)
```

Convention-based (not runtime-validated). All migrated call sites use these
constants, never string literals.

### 3.5 Journal

Unexported writer with lazy directory creation:

1. `os.MkdirAll(<tasksDir>/orchestration-log/, 0o755)` — created on first write.
2. `os.OpenFile(path, O_APPEND|O_CREATE|O_WRONLY, 0o644)`
3. Marshal `transitionRecord` as JSON, write line, close.
4. On error: return silently — journal is a non-critical operational log.
   Warnings would create noise for every transition during disk-full scenarios
   without actionable benefit.

Schema:

```go
type transitionRecord struct {
    At     string `json:"at"`               // UTC RFC3339
    Task   string `json:"task"`             // task filename
    From   string `json:"from"`             // source state directory
    To     string `json:"to"`               // destination state directory
    Agent  string `json:"agent,omitempty"`  // agent ID when applicable
    Reason string `json:"reason,omitempty"` // stable reason code constant
    Detail string `json:"detail,omitempty"` // optional human-readable context
}
```

`Reason` uses the stable constants from §3.4. `Detail` is an optional extension
beyond the proposal's narrower schema, providing human-readable context for
transitions where a stable code alone is insufficient (e.g., merge error text,
parse error messages).

Both forward transitions and rollback transitions are journaled as separate
entries (except cancel, which uses deferred journaling — see §4 B7).

### 3.6 Helpers

**`TransitionWithPostWrite`** — Move, then write. Rollback on write failure.
Rollback automatically sets `WithAllowRollback()` and
`WithReason(ReasonRollback)`.

```go
func TransitionWithPostWrite(tasksDir, filename, fromState, toState string,
    writeFn func(dstPath string) error, opts ...TransitionOption) error
```

**`TransitionWithPreWrite`** — Best-effort write, then move. Write failure logs
warning; move proceeds.

```go
func TransitionWithPreWrite(tasksDir, filename, fromState, toState string,
    writeFn func(srcPath string) error, opts ...TransitionOption) error
```

**`TransitionWithRequiredPreWrite`** — Required write, then move. Write failure
returns error; move skipped.

```go
func TransitionWithRequiredPreWrite(tasksDir, filename, fromState, toState string,
    writeFn func(srcPath string) error, opts ...TransitionOption) error
```

**`TransitionWithRetry`** — Retry loop around `TransitionTask`. Caller specifies
attempts and delay.

```go
func TransitionWithRetry(tasksDir, filename, fromState, toState string,
    attempts int, delay time.Duration, opts ...TransitionOption) error
```

**`TransitionWithBestEffortPostWrite`** — Move, then best-effort write. No
rollback on write failure.

```go
func TransitionWithBestEffortPostWrite(tasksDir, filename, fromState, toState string,
    writeFn func(dstPath string) error, opts ...TransitionOption) error
```

### 3.7 Error Types

```go
var errInvalidTransition = errors.New("invalid state transition")

type InvalidTransitionError struct {
    Filename  string
    FromState string
    ToState   string
}

func (e *InvalidTransitionError) Error() string { ... }
func (e *InvalidTransitionError) Unwrap() error { return errInvalidTransition }
```

Matches the `FailedDirUnavailableError` pattern in `claim.go`. Callers use
`errors.Is(err, errInvalidTransition)`.

### 3.8 `WithAgent`/`WithReason` Usage

| Transition | Agent | Reason |
|---|---|---|
| Claim (backlog→in-progress) | agentID | `claim` |
| Post-agent push (in-progress→ready-for-review) | agentID | `push` |
| Approval (ready-for-review→ready-to-merge) | agentID | `approval` |
| Rejection (ready-for-review→backlog) | agentID | `rejection` |
| Merge success (ready-to-merge→completed) | — | `merge` |
| Merge failure (ready-to-merge→backlog/failed) | — | `merge-failure` |
| Orphan recovery (in-progress→backlog) | — | `orphan-recovery` |
| Stuck task (in-progress→backlog) | agentID | `stuck-recovery` |
| Cancel (any→failed) | — | `cancelled` |
| Dependency promotion | — | `promotion` |
| Dependency demotion | — | `demotion` |
| Retry exhausted | — | `retry-exhausted` |
| Quarantine/cycle/duplicate→failed | — | `quarantine`/`cycle`/`duplicate` |
| Rollback transitions | — | `rollback` |

### 3.9 Test Seam Changes

**Remove** after migration:
- `claimRollbackFn` — replaced by `TransitionTask`
- `retryExhaustedMoveFn` — replaced by `TransitionTask`
- `retryExhaustedRollback` — replaced by `TransitionTask`

**Keep** (test different concerns):
- `linkFn`, `readFileFn`, `openFileFn`, `removeFn`, `writeFileFn` — test
  `AtomicMove` internals
- `appendToFileFn`, `appendCancelledRecordFn` — test write behavior

## 4. Step-by-Step Breakdown

### Phase A: Foundation (no behavioral changes)

**A1: Create `internal/queue/transition.go`**
- Define `validTransitions` and `validRollbacks` tables
- Implement `TransitionTask` with validation, `MkdirAll`, `AtomicMove`, journal
- Implement all 5 transition helpers
- Define `InvalidTransitionError`, `errInvalidTransition`
- Define `TransitionOption` types and reason code constants

**A2: Create `internal/queue/journal.go`**
- Implement `writeJournalEntry` with lazy directory creation
- Wire into `TransitionTask` after successful move

**A3: Create `internal/queue/transition_test.go`**
- Lifecycle transitions succeed (table-driven, all edges)
- Rollback transitions: succeed with `WithAllowRollback()`, fail without
- Invalid transitions: `errors.Is(err, errInvalidTransition)`, no journal entry
- Failed moves: no journal entry
- `ErrDestinationExists` propagation
- Missing source file
- Concurrent race: two goroutines, one succeeds
- Destination directory auto-created when missing
- All 5 helpers: success, failure, rollback scenarios
- `TransitionWithPostWrite` rollback: auto `WithAllowRollback()`, journal entries
  for both forward and rollback
- `TransitionWithRequiredPreWrite`: write fail → no move, no journal
- `WithSkipJournal()`: no journal entry

**A4: Create `internal/queue/journal_test.go`**
- Correct JSONL output with reason codes
- `Detail` field present when set
- Append semantics (file grows)
- Best-effort: journal failure silent
- Lazy directory creation: dir created on first write
- Directory creation failure: silent

### Phase B: Incremental Migration

**B1: reconcile.go (8 `AtomicMove` call sites)**

Plain moves (2):
- `waiting → backlog` promotion (line 245): `TransitionTask` with
  `WithReason(ReasonPromotion)`
- `backlog → waiting` demotion (line 115): `TransitionTask` with
  `WithReason(ReasonDemotion)`

Best-effort pre-write (5):
- Unparseable waiting (line 71): `TransitionWithPreWrite` with
  `WithReason(ReasonQuarantine)`, `WithDetail(parseErr)`
- Unparseable backlog (line 83): same pattern
- Invalid glob backlog (line 98): `TransitionWithPreWrite` with
  `WithReason(ReasonQuarantine)`, `WithDetail(globErr)`
- Duplicate waiting (line 162): `TransitionWithPreWrite` with
  `WithReason(ReasonDuplicate)`, `WithDetail(reason)`
- Invalid glob waiting (line 237): same as glob backlog

Cycle member with idempotency guard (1):
- Cycle member waiting→failed (line 202) — caller preserves existing
  `ContainsCycleFailure` check from `reconcile.go:187-209`:
  - If marker absent: `TransitionWithRequiredPreWrite` with
    `WithReason(ReasonCycle)`. If append fails, move skipped (task stays in
    waiting/).
  - If marker already present (prior pass appended but move failed):
    `TransitionTask` with `WithReason(ReasonCycle)`. Move only, no duplicate
    append.

**B2: claim.go (4 `AtomicMove` call sites, exact flow preserved)**

The claim flow in `SelectAndClaimTask` has a specific ordering that must be
preserved:

1. Dependency demotion (line 239, 1 `AtomicMove`):
   `TransitionTask(backlog→waiting, WithReason(ReasonDemotion))`

2. Claim move (line 262, 1 `AtomicMove`):
   `TransitionTask(backlog→in-progress, WithAgent(agentID), WithReason(ReasonClaim))`

3. Retry-exhausted (line 164, 1 `AtomicMove` + line 168 rollback, 1 `AtomicMove`):
   - `TransitionTask(in-progress→failed, WithReason(ReasonRetryExhausted))`
   - On failure: rollback via `TransitionTask(in-progress→backlog,
     WithReason(ReasonRollback))` — lifecycle edge, no `WithAllowRollback()`
     needed
   - Preserves `FailedDirUnavailableError` semantics

4. Claimed-by header (no `AtomicMove`, but uses rollback on failure):
   `claimPrependFn` with rollback via `TransitionTask(in-progress→backlog,
   WithReason(ReasonRollback))`

5. Branch comment (no `AtomicMove`): best-effort, no rollback. **Unchanged from
   current behavior.**

Remove `claimRollbackFn`, `retryExhaustedMoveFn`, `retryExhaustedRollback`.

**B3: queue.go orphan recovery (1 migrated, 1 excluded)**

Hybrid approach — use `TransitionTask` for the initial move but preserve the
existing `ErrDestinationExists` → collision resolution flow at the caller level:

- Initial move: `TransitionTask(in-progress→backlog,
  WithReason(ReasonOrphanRecovery))`
- If `ErrDestinationExists`: fall through to existing `resolveOrphanCollision`
  (unchanged, stays as direct `AtomicMove` — excluded because it changes
  filename)
- After resolution: append failure record at caller level (best-effort)
- Orphan-collision tests in `queue_test.go` (dedup, renamed recovery) preserved
  unchanged

**B4: runner/task.go (2 `AtomicMove` call sites)**

- Post-agent push (line 195): `TransitionWithPostWrite` with `WithAgent(agentID),
  WithReason(ReasonPush)`
  - `writeFn`: appends branch marker via `appendToFileFn`
  - Rollback: helper auto-sets `WithAllowRollback()` for
    `ready-for-review → in-progress`

- Stuck task (line 223): `TransitionWithBestEffortPostWrite` with
  `WithAgent(agentID), WithReason(ReasonStuckRecovery)`
  - `writeFn` **preserves existing `agentWroteFailureRecord(dst, agentID)` guard**:
    only appends the generic host-side failure record if the agent did not already
    write an `ON_FAILURE` marker. This prevents double-counting retries.

**B5: runner/review.go (5 `AtomicMove` call sites)**

- Quarantine (lines 65, 132): `TransitionWithPreWrite` with
  `WithReason(ReasonQuarantine), WithDetail(parseErr)`

- Review exhausted (lines 84, 155): `TransitionWithPreWrite` with
  `WithReason(ReasonRetryExhausted)`

- Review verdict — approval (from `postReviewAction` → `moveReviewedTask`):
  - `TransitionWithRequiredPreWrite(tasksDir, filename, DirReadyReview,
    DirReadyMerge, writeFn, WithAgent(agentID), WithReason(ReasonApproval))`
  - `writeFn`: appends `<!-- reviewed: ... -->` marker via `appendToFileFn`
  - If marker write fails: record review-failure, return (no move — matches
    current `review.go:383-388`)
  - After successful transition: send completion message (best-effort, matching
    `moveReviewedTask` line 428)

- Review verdict — rejection:
  - `TransitionWithRequiredPreWrite(tasksDir, filename, DirReadyReview,
    DirBacklog, writeFn, WithAgent(agentID), WithReason(ReasonRejection))`
  - `writeFn`: appends `<!-- review-rejection: ... -->` marker via
    `appendToFileFn`
  - If marker write fails: record review-failure, return (no move — matches
    current `review.go:396-401`)
  - After successful transition: send completion message

Existing `os.MkdirAll` calls in review.go removed — `TransitionTask` handles
destination directory creation.

**B6: merge/taskops.go + merge.go (2 `AtomicMove` call sites + callers)**

`failMergeTask` signature changes from `failMergeTask(src, dst, reason string)`
to `failMergeTask(src, toState, reason string)`:
- `toState == ""`: append-only, no move. Unchanged behavior for push-failure case
  where the task stays in `ready-to-merge/` for retry.
- `toState != ""` (`DirBacklog` or `DirFailed`): calls `TransitionWithPreWrite`
  with `WithReason(ReasonMergeFailure), WithDetail(reason)` internally.

`mergeFailureDestination` refactored to return a state string instead of a full
path.

`handleMergeFailure` updated to pass state to refactored `failMergeTask`.

`moveTaskWithRetry` removed. Call sites in `executeMergeRound` replaced with
`TransitionWithRetry(tasksDir, task.name, DirReadyMerge, DirCompleted, 3,
100*time.Millisecond, WithReason(ReasonMerge))`. The existing
duplicate-destination handling in `executeMergeRound` (lines 128-140, 176-186)
is caller-level logic that checks `os.Stat(completedPath)` after a failed move
and removes the ready-to-merge copy — this is preserved at the caller after
replacing `moveTaskWithRetry`.

**B7: cancel.go (2 `AtomicMove` call sites, deferred journal)**

For tasks already in `failed/`: append cancelled marker in place. No state
transition, no journal entry.

For tasks in other states — three distinct post-move phases:

1. `TransitionTask(state→failed, WithSkipJournal())` — move without journaling
2. `ensureMoveSourceRemoved(match.Path, failedPath)` — verification step. If
   this fails, `CancelTask` returns error. No journal entry (correct — the move
   may have been invalidated).
3. `appendCancelledRecordFn(failedPath)` — write cancelled marker. If this fails,
   rollback via `TransitionTask(failed→state, WithAllowRollback(),
   WithSkipJournal())`. No journal entry for either direction (correct — the
   operation didn't persist).
4. On success: `writeJournalEntry(tasksDir, filename, state, DirFailed,
   WithReason(ReasonCancelled))` — exactly one journal entry for the committed
   transition. Uses the same record-construction logic as `TransitionTask`.

### Phase C: Cleanup and Documentation

**C1: Remove obsolete test hooks**
- Remove `claimRollbackFn`, `retryExhaustedMoveFn`, `retryExhaustedRollback`
  from `claim.go`
- Rewrite affected tests in `claim_test.go` to use `TransitionTask` as the test
  seam

**C2: Update documentation**
- `docs/architecture.md`: Add dual transition tables (lifecycle + rollback), API
  description, journal schema with reason codes and detail field, fix
  branch-marker ordering description (code moves first, then writes marker)
- `README.md`: Add `orchestration-log/` to `.mato/` layout section
- Move `docs/proposals/ideas/state-machine-formalization.md` to
  `docs/proposals/implemented/`

**C3: Integration tests**
- Full lifecycle: waiting → backlog → in-progress → ready-for-review →
  ready-to-merge → completed, with journal verification (reason codes)
- Invalid transition: returns `InvalidTransitionError`, task unmoved
- Cycle-failure: marker write fails → task stays in waiting/, no move

## 5. File Changes

### New Files

| File | Description |
|---|---|
| `internal/queue/transition.go` | Tables, core function, helpers, errors, options, reason constants |
| `internal/queue/transition_test.go` | Comprehensive unit tests |
| `internal/queue/journal.go` | Journal writer with lazy directory creation |
| `internal/queue/journal_test.go` | Journal unit tests |

### Modified Files

| File | Changes |
|---|---|
| `internal/queue/reconcile.go` | Replace 8 `AtomicMove` calls; preserve cycle idempotency guard |
| `internal/queue/claim.go` | Replace 4 `AtomicMove` calls; remove 3 function-variable hooks |
| `internal/queue/queue.go` | Replace 1 `AtomicMove` call (hybrid orphan recovery); 1 excluded (rename) |
| `internal/queue/cancel.go` | Replace 2 `AtomicMove` calls; deferred journal |
| `internal/runner/task.go` | Replace 2 `AtomicMove` calls; preserve `agentWroteFailureRecord` guard |
| `internal/runner/review.go` | Replace 5 `AtomicMove` calls; approval/rejection use `RequiredPreWrite` |
| `internal/merge/taskops.go` | Refactor `failMergeTask` signature; remove `moveTaskWithRetry` |
| `internal/merge/merge.go` | Update `handleMergeFailure`, `executeMergeRound` callers |
| `internal/merge/taskops_test.go` | Remove `TestMoveTaskWithRetry`; update `TestMergeFailureDestination` (returns state); update `TestFailMergeTask`; preserve `TestFailMergeTask_AppendOnly` |
| `internal/queue/claim_test.go` | Rewrite hook-based tests |
| `internal/queue/cancel_test.go` | Add journal tests for cancel flow |
| `internal/queue/reconcile_test.go` | Add cycle idempotency regression test |
| `internal/runner/task_test.go` | Add stuck-task guard regression test; minor updates |
| `internal/runner/review_test.go` | Minor updates |
| `docs/architecture.md` | Dual tables, API, journal schema, branch-marker ordering fix |
| `README.md` | Add `orchestration-log/` to layout section |

### Moved Files

| From | To |
|---|---|
| `docs/proposals/ideas/state-machine-formalization.md` | `docs/proposals/implemented/state-machine-formalization.md` |

## 6. Error Handling

- **Invalid transition**: `InvalidTransitionError` wrapping `errInvalidTransition`
- **Rollback without opt-in**: `InvalidTransitionError` (prevents accidental
  misuse of rollback paths)
- **AtomicMove failures**: Propagated unchanged (including `ErrDestinationExists`)
- **Best-effort pre-write failures** (`TransitionWithPreWrite`): Log warning,
  proceed with move
- **Required pre-write failures** (`TransitionWithRequiredPreWrite`): Return
  write error, skip move. Used by cycle-failure and review approval/rejection.
- **Post-write failures** (`TransitionWithPostWrite`): Roll back via
  `TransitionTask(..., WithAllowRollback())`. Hard error if rollback also fails.
- **Best-effort post-write failures** (`TransitionWithBestEffortPostWrite`): Log
  warning, no rollback
- **Cancel verification failure**: `ensureMoveSourceRemoved` error propagated; no
  journal entry
- **Journal write failures**: Silently ignored (non-critical operational log)
- **Retry exhaustion** (`TransitionWithRetry`): Return last error from final
  attempt

## 7. Testing Strategy

### `transition_test.go`

- **Lifecycle transitions**: Table-driven, all edges succeed
- **Rollback transitions**: Succeed with `WithAllowRollback()`, fail without it
- **Invalid transitions**: `errors.Is(err, errInvalidTransition)`, no journal
  entry
- **Failed moves**: No journal entry written
- **`ErrDestinationExists`**: Propagated correctly
- **Missing source file**: Error propagation
- **Concurrent race**: Two goroutines, one succeeds
- **Destination directory**: Auto-created when missing
- **`TransitionWithPostWrite`**: Write ok; write fail + rollback ok (journal
  entries for both forward and rollback); write fail + rollback fail (hard error)
- **`TransitionWithPreWrite`**: Write ok; write fail + move ok
- **`TransitionWithRequiredPreWrite`**: Write ok + move ok; write fail → no move,
  no journal
- **`TransitionWithRetry`**: Transient failure then success; all fail
- **`TransitionWithBestEffortPostWrite`**: Write fail + task stays in destination
- **`WithSkipJournal()`**: No journal entry
- **`in-progress→backlog` without `WithAllowRollback()`**: Succeeds (lifecycle
  edge)
- **`ready-for-review→in-progress` without `WithAllowRollback()`**: Fails
  (rollback-only edge)
- **`failed→*` without `WithAllowRollback()`**: Fails (rollback-only edge)

### `journal_test.go`

- Correct JSONL output with reason codes
- `Detail` field present when set, absent when not
- Append semantics (multiple entries, file grows)
- Best-effort: journal failure silent
- Lazy directory creation: dir created on first write
- Directory creation failure: silent

### `reconcile_test.go` additions

- Cycle idempotency: append ok + move fails + next pass retries without
  duplicating `<!-- cycle-failure: -->` marker

### `cancel_test.go` additions

- Happy-path cancel: exactly one journal entry with reason `"cancelled"`
- `ensureMoveSourceRemoved` failure: no journal entry
- Cancelled-marker failure + rollback: no journal entry for either direction
- Already-failed cancel: no state change, no journal entry

### `task_test.go` additions

- Stuck task: agent already wrote failure record → recovery moves to backlog
  without duplicate generic failure marker

### `review_test.go` additions

- Approval: marker written before move; marker write fails → review-failure
  recorded, no move
- Rejection: same pattern

### Existing Test Preservation

- `TestCancelTask_MoveLeavesDuplicateCopies` unchanged
- `TestFailMergeTask_AppendOnly` unchanged
- Orphan-collision tests in `queue_test.go` unchanged
- Behavioral parity for all non-hook tests
- `claim_test.go`: Hook-based tests rewritten with same behavioral assertions
- `taskops_test.go`: `TestMoveTaskWithRetry` removed, others adapted
- Tests scanning directories may need filter for `orchestration-log/`

### Integration Tests (`internal/integration/`)

- Full lifecycle with journal: waiting → backlog → in-progress → ready-for-review
  → ready-to-merge → completed. Verify journal entries with correct reason codes.
- Invalid transition rejection: confirm `InvalidTransitionError`, task unmoved
- Cycle-failure: marker write fails → task stays in waiting/

## 8. Risks & Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Behavioral regression during migration | High | Migrate one call site at a time. Run full test suite after each step. |
| Transition table missing valid edge | High | Derived from exhaustive call-site catalog. Test asserts table matches architecture docs. |
| Accidental rollback path use | Medium | Separate table with `WithAllowRollback()` opt-in. |
| Cancel semantics change | High | 4-phase decomposition with deferred journal. `TestCancelTask_MoveLeavesDuplicateCopies` catches regressions. |
| Claim ordering change | High | 5-step decomposition matching current `claim.go` ordering exactly. |
| Cycle-failure idempotency regression | High | Caller preserves `ContainsCycleFailure` guard. `ReconcileReadyQueue`-level test. |
| Stuck-task double-count regression | High | Preserves `agentWroteFailureRecord` guard. Explicit regression test. |
| Review marker regression | High | `TransitionWithRequiredPreWrite` preserves write-before-move. Test covers marker fail → no move. |
| Merge adapter mismatch | Medium | `failMergeTask` signature change; `dst==""` test preserved. Caller-level duplicate-destination handling in `executeMergeRound` preserved. |
| Reason code drift | Low | Constants defined in `transition.go`; convention enforced by code review. |
| Journal silent failure | Low | Lazy directory creation prevents first-write failure. Silent errors are intentional design. |

## 9. Open Questions

1. **Pre-check source file existence?** The plan delegates to `AtomicMove`, which
   fails if the source doesn't exist. Adding a pre-check would detect programmer
   errors earlier but adds a TOCTOU window. The plan keeps the current behavior.

2. **Rollback journal entries?** The plan journals rollback transitions with
   `ReasonRollback` for complete audit trail. Cancel uses deferred journal to
   avoid spurious entries. `WithSkipJournal()` is available for other callers.

## 10. Design Evolution

This plan went through 11 review rounds. Key decisions that evolved:

- **Round 2**: Split pre-write helpers into required vs best-effort to preserve
  cycle-failure semantics where the move is skipped if the marker write fails.
- **Round 3**: Decomposed claim flow into individual steps matching exact current
  ordering, instead of collapsing into a single helper.
- **Round 3**: Decomposed cancel flow into distinct phases to preserve
  `ensureMoveSourceRemoved` verification semantics.
- **Round 4**: Added `os.MkdirAll` to `TransitionTask` to preserve destination-dir
  creation that review.go and taskops.go do per-call-site.
- **Round 4**: Added `WithSkipJournal()` and explicit journal API for cancel's
  deferred journaling.
- **Round 5**: Excluded orphan-collision rename from `TransitionTask` (filename
  changes during move). Hybrid approach for orphan recovery.
- **Round 7**: Added lazy journal directory creation. Preserved
  `agentWroteFailureRecord` guard for stuck-task recovery.
- **Round 9**: Split tables into lifecycle + rollback to prevent accidental misuse
  of compensating edges. Stabilized reason codes with separate `detail` field.
- **Round 10**: Fixed `in-progress→backlog` classification (lifecycle, not
  rollback-only). Fixed review approval/rejection to use `RequiredPreWrite`.
