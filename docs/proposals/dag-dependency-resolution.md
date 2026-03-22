# DAG-Based Dependency Resolution — Implementation Plan

## Summary

Introduce a shared dependency-analysis helper that centralizes waiting-task
dependency reasoning for queue reconciliation, status reporting, and future
graph-oriented features.

The goal is not to change healthy scheduling semantics. The goal is to replace
duplicated recursive checks with one deterministic analysis pass that:

- preserves current promotion behavior for acyclic graphs,
- reports cycles explicitly,
- explains blocked tasks more clearly, and
- creates a foundation for future graph tooling.

Estimated effort: ~3.5 days.

## Effort Breakdown

| Task | Effort |
|------|--------|
| `internal/dag/dag.go` — Kahn's + SCC + `Analyze()` | 3 hours |
| `internal/dag/dag_test.go` — unit tests (deps-satisfied, blocked, cycles, determinism) | 3 hours |
| `internal/queue/reconcile.go` — refactor to use `DiagnoseDependencies()`, add cycle-to-failed | 5 hours |
| `internal/queue/diagnostics.go` — `DiagnoseDependencies()` wrapper exposing `Analysis` | 2 hours |
| `internal/queue/diagnostics_test.go` + reconcile test updates | 3 hours |
| `internal/status/status.go` — cycle-aware summaries | 2 hours |
| Status rendering + tests | 3 hours |
| Integration tests | 2 hours |
| Documentation updates | 1 hour |
| **Total** | **~3.5 days** |

## Current State

Today `internal/queue/reconcile.go` already does more than a simple direct-dep
check:

- it removes ambiguous completed IDs via `safeCompleted`,
- it checks transitive waiting-task dependencies via `dependsOnWaitingTask()`,
- it promotes every currently promotable waiting task in one reconcile pass,
- and it logs cycle warnings.

So the proposal is **not** starting from zero. The plan should build on this
rather than replacing it with looser semantics.

## Goals

- Centralize dependency analysis so queue and status agree.
- Preserve current readiness rules for acyclic graphs.
- Detect and surface cycles explicitly instead of leaving them as stderr-only
  warnings.
- Keep ambiguous-ID handling consistent everywhere.
- Make future commands like `mato graph` easy to add.

## Non-Goals

- No change to `depends_on` frontmatter syntax.
- No cascade failure of downstream dependents in v1.
- No broad rewrite of dependency-context JSON in the same step.
- No attempt to auto-promote entire dependency chains in one reconcile pass.

## Key Behavioral Decision

### Preserve current promotion semantics

This is the most important constraint.

If `A` is completed, `B` depends on `A`, and `C` depends on `B`, one reconcile
should make `B` promotable and leave `C` blocked. `C` should not become ready in
the same reconcile just because the graph can see that `B` is also promotable.

That keeps behavior aligned with the current implementation and existing tests.

In other words:

- **completed dependencies** satisfy readiness,
- **waiting dependencies** do not,
- even if those waiting dependencies are themselves currently promotable.

## Design

### New package: `internal/dag/`

Create a small pure-logic package with no filesystem I/O. It should accept a
snapshot of waiting tasks plus supporting state from the caller.

This package should not know about queue directories directly. Callers in
`internal/queue` and `internal/status` translate filesystem state into graph
inputs.

### Input model

The analysis must be rich enough to avoid re-deriving queue semantics in every
caller.

```go
package dag

type Node struct {
    ID        string
    DependsOn []string
}

type BlockReason int

const (
    BlockedByWaiting   BlockReason = iota // dependency is itself in waiting/
    BlockedByUnknown                      // dependency ID not found anywhere
    BlockedByExternal                     // dependency exists in a non-completed, non-waiting state (e.g. failed, in-progress)
    BlockedByAmbiguous                    // dependency ID exists in both completed/ and a non-completed directory
)

type BlockDetail struct {
    DependencyID string
    Reason       BlockReason
}

type Analysis struct {
    // DepsSatisfied lists task IDs whose dependencies are all in completedIDs.
    // This does NOT mean the task is promotable — the caller must still verify
    // there is no active-affects overlap (!idx.HasActiveOverlap()) before
    // promoting.
    DepsSatisfied []string

    // Blocked maps a task ID to the specific dependencies preventing promotion
    // and the reason each one blocks. A task that is a cycle member (appears in
    // Cycles) does NOT appear in Blocked — cycle members are handled separately
    // via cycle-to-failed. Tasks downstream of a cycle DO appear in Blocked
    // with BlockedByWaiting referencing the cycle member.
    Blocked map[string][]BlockDetail

    // Cycles contains the strongly connected components (size > 1, or size 1
    // with a self-edge) found in the waiting subgraph.
    Cycles [][]string
}

// Analyze determines which waiting tasks have all dependencies satisfied.
//
// completedIDs should be the caller's safeCompleted set (ambiguous IDs already
// removed). knownIDs is the full set of task IDs across all directories —
// needed to distinguish BlockedByUnknown (ID not found anywhere) from
// BlockedByExternal (ID exists in a non-completed, non-waiting state like
// failed/ or in-progress/). ambiguousIDs is the set of IDs that appear in both
// completed/ and a non-completed directory — these are excluded from
// completedIDs by the caller, and Analyze tags them as BlockedByAmbiguous
// rather than BlockedByExternal so the blocking reason is self-documenting.
func Analyze(waiting []Node, completedIDs, knownIDs, ambiguousIDs map[string]struct{}) Analysis
```

**Naming note:** `DepsSatisfied` rather than `Ready` avoids confusion with
actual promotability. A task can have all dependencies satisfied yet still be
blocked by active-affects overlap (`idx.HasActiveOverlap()` returns `true`).
Callers that need the full promotion check (e.g., `ReconcileReadyQueue`) must
filter out overlapping tasks after the DAG analysis.

**Why `knownIDs`:** Without this parameter, `Analyze()` cannot distinguish
between a dependency that references a nonexistent task (`BlockedByUnknown`)
and one that references a real task in `failed/` or `in-progress/`
(`BlockedByExternal`). The caller already has this information via
`idx.AllIDs()` — passing it through avoids coupling the DAG package to
filesystem concepts while preserving diagnostic precision.

**Why `ambiguousIDs`:** Without this parameter, a dependency blocked because
it was ambiguous (exists in both `completed/` and a non-completed directory)
would be indistinguishable from a normal `BlockedByExternal` inside the
`Analysis`. By accepting the ambiguous set explicitly, `Analyze()` can tag
these as `BlockedByAmbiguous`, making the `Analysis` self-contained —
callers inspecting `Blocked` see the real reason without needing side-channel
information from the diagnostics layer.

### Scope of the graph

The graph contains only waiting tasks.

- Dependencies in `completed/` are satisfied input state.
- Dependencies in `waiting/` are graph edges.
- Dependencies in `backlog/`, `in-progress/`, `ready-for-review/`,
  `ready-to-merge/`, or `failed/` are **not** graph edges. They remain blocked
  external dependencies from the caller's point of view.

This matches current queue behavior and avoids blurring "waiting blocker" with
"in-flight blocker."

### Node identity and ID semantics

The DAG package operates in task-ID space: `Node.ID` is the value from
`meta.ID`, and `depends_on` references resolve against these IDs. This matches
the current behavior in `reconcile.go:48` where `waitingDeps` is keyed by
`snap.Meta.ID`.

**Stem-vs-ID asymmetry.** `PollIndex` registers both the filename stem and
`meta.ID` in `completedIDs` and `allIDs` (`index.go:141-189`), so a
`depends_on` reference matching either value resolves against completed tasks.
But the waiting graph only keys by `meta.ID` — a `depends_on` reference to a
filename stem that differs from `meta.ID` would not find the waiting task as a
graph edge.

v1 preserves this asymmetry: waiting-edge resolution uses `meta.ID` only.
Normalizing to also register stems in the waiting lookup would introduce a new
ambiguity class — one task's filename stem could collide with another task's
explicit `id`, even when their `meta.ID` values are unique. Handling that
correctly would require alias-collision detection and resolution logic that
adds complexity for marginal benefit. The asymmetry is a pre-existing behavior,
not a regression. Users who want consistent resolution should set explicit `id`
values matching their `depends_on` references.

Unifying stem and `meta.ID` resolution across waiting and completed is a
worthwhile follow-up once alias collision semantics are fully designed.

**Duplicate waiting IDs.** If two files in `waiting/` share the same `meta.ID`
(e.g., `foo.md` with `id: bar` alongside `bar.md` which defaults to `id: bar`),
the current code silently overwrites one in `waitingDeps`. This is a
pre-existing bug, not introduced by this proposal. `DiagnoseDependencies`
should detect this collision and emit a `DependencyDuplicateID` issue. The
`dag.Analyze()` function itself assumes unique node IDs — the caller is
responsible for deduplication or error reporting before building the node list.
If duplicates are detected, `DiagnoseDependencies` should skip the duplicate
node (keeping the first by filename sort order, consistent with
`ListTaskFiles()` in `internal/queue/taskfiles.go`) and emit the issue, rather
than passing conflicting nodes into the graph.

## Readiness Rules

`Analyze()` should preserve the current reconcile logic for acyclic graphs.
For cyclic cases (including self-dependencies), the behavior changes
intentionally — see Backward Compatibility.

1. A self-dependency is a self-cycle (SCC of size 1 with a self-edge). It
   appears in `Analysis.Cycles`, not in `Analysis.Blocked`. The reconcile
   layer moves it to `failed/` via the cycle-to-failed sequence. Previously,
   self-dependent tasks stayed in `waiting/` indefinitely — this is a
   behavioral change (see Backward Compatibility).
2. A dependency on a waiting task blocks readiness.
3. A dependency on a completed ID satisfies readiness.
4. A dependency on an ambiguous ID (in both `completed/` and a non-completed
   directory) blocks readiness (`BlockedByAmbiguous`). Ambiguous IDs must be
   excluded from `completedIDs` before analysis.
5. A dependency on a non-completed ID that exists in `knownIDs` (and is not
   ambiguous) blocks readiness (`BlockedByExternal`).
6. A dependency on an ID not found in `knownIDs` blocks readiness
   (`BlockedByUnknown`).

The caller is responsible for passing `safeCompleted` as `completedIDs`,
`idx.AllIDs()` as `knownIDs`, and the set of IDs found in both
`idx.CompletedIDs()` and `idx.NonCompletedIDs()` as `ambiguousIDs`.

## Cycle Handling

Cycles are unsolvable — a cycle in `waiting/` will never self-resolve. Leaving
cyclic tasks in `waiting/` forever is worse than failing them, because they
silently block downstream dependents with no signal to the user.

v1 moves true cycle members to `failed/` during reconcile. The key constraint
is precision: **only actual cycle members fail, not downstream nodes.**

### Classification

After Kahn's algorithm, nodes remaining in the residual graph are **not**
automatically all cycle members. Some may simply be downstream of a cycle.
Exact classification requires SCC (strongly connected component) detection on
the residual waiting-only subgraph:

- SCC of size > 1 → all members are cycle participants → fail them.
- SCC of size 1 with a self-edge → self-cycle → fail it.
- SCC of size 1 without a self-edge → downstream of a cycle → leave blocked.

### Failure semantics

- Cycle failures use a dedicated marker prefix `<!-- cycle-failure:` to
  distinguish them from agent failures (`<!-- failure:`). This mirrors the
  existing `<!-- review-failure:` convention. Because `CountFailureMarkers` in
  `internal/taskfile/metadata.go` only counts lines matching `<!-- failure:`,
  cycle-failure markers are naturally excluded from retry budget counting
  without any change to the counting logic.
- The marker format is:
  `<!-- cycle-failure: mato at <RFC3339-timestamp> — circular dependency -->`
- Cycle failures do **not** consume normal retry budget. They are a structural
  problem, not a transient agent failure.

### Cycle-to-failed sequence

The failure record must be appended **before** the task is moved to `failed/`.
This ensures the reason is preserved in the file regardless of whether the move
succeeds:

1. Check whether the file already contains a `<!-- cycle-failure:` marker
   (using a helper analogous to `taskfile.ContainsFailureFrom`). If present,
   skip the append — the record was written on a prior pass where the move
   failed.
2. Append `<!-- cycle-failure: mato at <timestamp> — circular dependency -->`
   to the task file in `waiting/` using `O_APPEND` (consistent with existing
   failure record writes).
3. `AtomicMove(waitingPath, failedPath)`.
4. If the move fails (e.g., `ErrDestinationExists`), warn on stderr and
   continue. The idempotency check in step 1 prevents duplicate records on the
   next reconcile pass.

This matches the pattern used by `recoverStuckTask` in `internal/runner/task.go`,
which appends a failure record before moving to `failed/`.

- Downstream tasks remain in `waiting/` with their existing `depends_on`. Once
  the cycle is broken (user edits or removes the cyclic dependency), downstream
  tasks become promotable through normal reconciliation.

### Partial cycle resolution

When all cycle members are moved to `failed/` in a single reconcile pass, the
cycle is fully resolved. But if only some members are moved (e.g., due to
`AtomicMove` failures), the remaining members are no longer detected as a cycle
on the next pass — they become blocked-by-external because their dependency now
lives in `failed/`. This is correct: the cycle is structurally broken once any
member leaves `waiting/`. The remaining members already have their cycle-failure
records appended (step 2 of the cycle-to-failed sequence happens before step 3),
so the record is preserved even though the move failed. However, since they are
no longer cycle members on subsequent passes, they will not be automatically
retried for move — they remain in `waiting/` blocked by the failed dependency.
The user must resolve the situation (e.g., fix the dependency, or manually move
the task).

### Invariant: cycles cannot reach in-progress

Cyclic tasks are detected among `waiting/` tasks only. A task in a cycle can
never have been promoted to `backlog/` or claimed into `in-progress/`, because
promotion requires all dependencies to be satisfied — which is impossible for a
cycle member. Therefore, cycle-to-failed never needs to contend with an
in-flight agent.

## Algorithm Notes

Kahn's algorithm is still a good fit for partitioning the waiting graph, but the
proposal must not overclaim what the residual graph means.

Important correction:

- Nodes remaining after Kahn are **not automatically all cycle members**.
- Some may simply be downstream of a cycle.

So if we need exact cycle-member sets, we must run SCC detection or equivalent
on the residual waiting-only subgraph.

### Recommended approach

1. Build waiting-task adjacency and indegree.
2. Run Kahn to determine which nodes are not blocked by waiting-task edges.
3. Use readiness rules plus `completedIDs`, `knownIDs`, and `ambiguousIDs` to populate `DepsSatisfied` and `Blocked`.
4. Run SCC detection on the residual waiting subgraph and keep only SCCs that
   are truly cyclic:
   - size > 1, or
   - size == 1 with a self-edge.
5. Classify remaining residual nodes (downstream of cycles) as blocked.

This avoids misclassifying nodes that are only downstream of a cycle.

### SCC implementation

Implement Tarjan's algorithm from scratch in `internal/dag/`. The Go standard
library has no graph algorithms, and adding `gonum` for a single function would
be disproportionate given the codebase's minimal-dependency philosophy. Tarjan's
is ~50 lines of Go and well-understood. The implementation needs only a
stack-based DFS tracking `index` and `lowlink` per node, yielding SCCs when
`lowlink == index`.

### Deterministic output

All output slices must be sorted to ensure stable results across runs and
reliable test assertions. Go map iteration is intentionally non-deterministic,
so every slice derived from map keys or values must be explicitly sorted.

Specific ordering requirements:

- `DepsSatisfied`: sorted lexicographically by task ID.
- `Cycles`: each inner slice sorted lexicographically by task ID; outer slice
  sorted by the first element of each inner slice.
- `Blocked` values: each `[]BlockDetail` sorted by `DependencyID`.
- `Issues` (in `DependencyDiagnostics`): sorted by `(Kind, TaskID, DependsOn)`
  to ensure stable warning output order.

## Status Integration

`internal/status/status.go` already shows useful per-dependency state, so the
plan should extend that behavior rather than replace it.

### Recommendation

- Keep the current dependency list with each dependency's actual state.
- Add explicit cycle labeling for tasks identified by the DAG analysis.
- Optionally add a short blocked summary such as `blocked by waiting: task-b` or
  `cycle: task-a -> task-b -> task-a`.

That gives users more signal without regressing existing visibility into whether
the blocker is waiting, in progress, failed, or missing.

### Known gap: status I/O duplication

`internal/status/status.go` performs its own full filesystem scan and task
parsing via `taskStatesByID()`, independent of the `PollIndex` that the queue
package builds. This proposal does not consolidate that I/O — status will call
`dag.Analyze()` for cycle information but still do its own scan for
per-dependency state display. Consolidating status onto `PollIndex` (e.g., by
exposing an `IDStates() map[string]string` method) is a worthwhile follow-up
but is out of scope for v1.

### Known gap: `taskStatesByID` duplicate-ID overwrites

`taskStatesByID()` in `internal/status/status.go:257-259` registers both
filename stem and `meta.ID` in a flat map, with later directories overwriting
earlier ones. If two tasks share an ID (or a stem collides with another task's
`meta.ID`), the displayed dependency state may be misleading. This pre-dates
the DAG proposal and is not addressed in v1, but should be noted as a known
limitation.

### Cycle-failed task rendering

`status_render.go:218-224` renders failed tasks using `countFailureRecords`
(which calls `taskfile.CountFailureMarkers`, counting only `<!-- failure:`
lines) and `lastFailureReason` (which calls `taskfile.LastFailureReason`,
scanning for `<!-- failure:` lines). A cycle-failed task has zero
`<!-- failure:` markers — only `<!-- cycle-failure:` markers — so without
changes it would render as `0/3 retries exhausted` with no reason displayed.

The status rendering must be updated to:

1. Detect cycle-failed tasks by checking for `<!-- cycle-failure:` markers
   (add a `taskfile.ContainsCycleFailure(data []byte) bool` helper, or a
   `CountCycleFailureMarkers` function).
2. Render cycle-failed tasks with a distinct message, e.g.,
   `circular dependency detected` instead of `N/M retries exhausted`.
3. Extract the cycle-failure reason for display (analogous to
   `lastFailureReason` but scanning `<!-- cycle-failure:` lines).

This rendering change should be included in the status rendering work, not
deferred.

## Reconcile Integration

### `internal/queue/reconcile.go`

Refactor reconcile to reuse shared analysis, but do not broaden readiness.

### Call graph

`ReconcileReadyQueue` is the sole entry point. It calls
`DiagnoseDependencies()`, which internally calls `dag.Analyze()`. The
`DependencyDiagnostics` return value exposes the underlying `Analysis` so
reconcile can act on it directly without a second `dag.Analyze()` call.

```
ReconcileReadyQueue
  └─ DiagnoseDependencies          (internal/queue/diagnostics.go)
       └─ dag.Analyze              (internal/dag/dag.go)
       returns DependencyDiagnostics { Analysis, Issues, ... }
  ├─ use diag.Analysis.Cycles      → move cycle members to failed/
  ├─ use diag.Analysis.DepsSatisfied + !HasActiveOverlap → promote
  └─ use diag.Issues               → emit structured warnings
```

`CountPromotableWaitingTasks` follows the same path for consistency.

### Recommended shape

1. Move unparseable tasks to `failed/` (unchanged from today).
2. Call `DiagnoseDependencies(tasksDir, idx)` — this internally builds
   `safeCompleted`, `ambiguousIDs`, `knownIDs`, and the `dag.Node` list,
   then calls `dag.Analyze()`.
3. Move `diag.Analysis.Cycles` members to `failed/` using the cycle-to-failed
   sequence (append `<!-- cycle-failure:` record, then `AtomicMove`). Cycle
   failures do not consume retry budget.
4. Promote `diag.Analysis.DepsSatisfied` tasks where
   `!idx.HasActiveOverlap(snap.Meta.Affects)` (no overlap with in-flight tasks).
5. Emit structured warnings from `diag.Issues` for:
    - self-dependencies (already moved to `failed/` in step 3, but logged),
    - cycles (already moved to `failed/` in step 3, but logged),
    - unknown dependencies,
    - ambiguous IDs,
    - duplicate waiting IDs.

The current helper functions (`dependsOnWaitingTask`, inline warning logic) can
be removed only after their behavior is fully covered by tests.

## Dependency Diagnostics

To keep status and reconcile aligned, dependency diagnostics should share a
single read-only helper in `internal/queue/diagnostics.go`. This helper wraps
`dag.Analyze()` and translates graph results into a flat issue list usable by
both `ReconcileReadyQueue()` warnings and `mato doctor` (when implemented).

`DiagnoseDependencies` is responsible for building the inputs to `dag.Analyze()`
from the `PollIndex`:

- `safeCompleted`: copy of `idx.CompletedIDs()` with ambiguous IDs removed
  (same logic as current `resolvePromotableTasks`).
- `ambiguousIDs`: IDs present in both `idx.CompletedIDs()` and
  `idx.NonCompletedIDs()`.
- `knownIDs`: `idx.AllIDs()`.
- `waiting []dag.Node`: built from `idx.TasksByState(queue.DirWaiting)`.

It also generates `DependencyIssue` entries for ambiguous IDs for warning
output, complementing the `BlockedByAmbiguous` reason that `dag.Analyze()`
now carries in `Analysis.Blocked`. The diagnostics-layer issue provides
human-readable context (task filename, dependency reference) while the
`BlockReason` makes the `Analysis` self-contained. Self-cycle and cycle issues
are derived from `Analysis.Cycles`: an SCC of size 1 with a self-edge produces
a `DependencySelfCycle` issue, while an SCC of size > 1 produces a
`DependencyCycle` issue for each member. Unknown-dependency issues are derived
from `Analysis.Blocked` entries with `BlockedByUnknown` reason.

```go
type DependencyIssueKind string

const (
	DependencyAmbiguousID  DependencyIssueKind = "ambiguous_id"
	DependencyDuplicateID  DependencyIssueKind = "duplicate_id"
	DependencySelfCycle    DependencyIssueKind = "self_dependency"
	DependencyCycle        DependencyIssueKind = "cycle"
	DependencyUnknownID    DependencyIssueKind = "unknown_dependency"
)

type DependencyIssue struct {
	Kind      DependencyIssueKind
	TaskID    string
	Filename  string
	DependsOn string // the problematic dependency reference
}

type DependencyDiagnostics struct {
	// Analysis is the underlying DAG result. Exposed so callers like
	// ReconcileReadyQueue can act on DepsSatisfied and Cycles directly
	// without a redundant dag.Analyze() call.
	Analysis dag.Analysis

	Issues []DependencyIssue
}

func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

`ReconcileReadyQueue()` calls `DiagnoseDependencies()` once per reconcile pass.
It reads `diag.Analysis` for promotion and cycle-to-failed decisions, and
iterates `diag.Issues` for warning output. A future `mato doctor` command
(see `docs/proposals/mato-doctor.md`) would call `DiagnoseDependencies()` and
convert issues into `Finding` objects. Both would share the same underlying
`dag.Analyze()` call — no duplicate graph traversal.

## Runner Dependency Context

The earlier draft tried to redesign the dependency-context JSON. That adds more
risk than value to the DAG work.

### Recommendation for v1

Keep the existing dependency-context file format in
`internal/runner/task.go:243` unchanged.

If transitive dependency context is added later, use one of these approaches as
a separate follow-up:

1. add a second file for transitive context, or
2. switch to a new object format and update all prompt/test expectations in the
   same PR.

This plan does **not** include that change by default.

## Files to Create or Modify

| File | Change |
|------|--------|
| `internal/dag/dag.go` | New shared analysis helper (`Analyze`, `BlockReason` incl. `BlockedByAmbiguous`, `BlockDetail`, `Analysis`, Tarjan's SCC) |
| `internal/dag/dag_test.go` | Unit tests for deps-satisfied/blocked/cycle analysis |
| `internal/queue/reconcile.go` | Replace duplicated recursive dependency checks with `DiagnoseDependencies()`; add cycle-to-failed move logic |
| `internal/queue/diagnostics.go` | New `DiagnoseDependencies()` wrapper that calls `dag.Analyze()` and exposes `Analysis` |
| `internal/queue/diagnostics_test.go` | Unit tests for diagnostics wrapper |
| `internal/queue/queue_test.go` | Preserve current promotion semantics + add cycle cases |
| `internal/taskfile/metadata.go` | Add `AppendCycleFailureRecord`, `ContainsCycleFailure`, and `CountCycleFailureMarkers` helpers |
| `internal/status/status.go` | Add cycle-aware waiting-task summaries without removing existing state detail |
| `internal/status/status_render.go` | Render cycle-failed tasks with distinct message instead of `N/M retries exhausted`; detect `<!-- cycle-failure:` markers |
| `internal/runner/task.go` | No change in v1 unless transitive context is explicitly split into a follow-up |
| `docs/architecture.md` | Document shared DAG analysis |
| `docs/task-format.md` | Document user-visible cycle behavior if behavior changes |
| `docs/messaging.md` | Only update if dependency-context file format changes in a later phase |
| `README.md` | Brief note if status output changes materially |

## Test Plan

### Unit tests (`internal/dag/dag_test.go`)

| Test | Coverage |
|------|----------|
| `TestAnalyze_Empty` | No tasks |
| `TestAnalyze_NoDeps` | Task with no dependencies appears in `DepsSatisfied` |
| `TestAnalyze_Chain` | `A completed`, `B -> A`, `C -> B` -> `B deps satisfied`, `C blocked` |
| `TestAnalyze_FanIn` | One unsatisfied dep keeps node blocked |
| `TestAnalyze_SelfDependency` | Self-dep appears in `Cycles` (SCC size 1 with self-edge), not in `DepsSatisfied` or `Blocked` |
| `TestAnalyze_UnknownDependency` | Missing dep (not in `knownIDs`) blocks with `BlockedByUnknown` |
| `TestAnalyze_ExternalDependency` | Dep in `knownIDs` but not in `completedIDs` or waiting blocks with `BlockedByExternal` |
| `TestAnalyze_AmbiguousCompletedExcluded` | Caller-supplied `safeCompleted` behavior preserved |
| `TestAnalyze_AmbiguousDependency` | Dep in `ambiguousIDs` blocks with `BlockedByAmbiguous`, not `BlockedByExternal` |
| `TestAnalyze_CycleMembersOnly` | 2-node cycle: members identified without misclassifying downstream nodes |
| `TestAnalyze_LongCycle` | 3+ node cycle (`A -> B -> C -> A`): all three are cycle members, downstream `D -> C` stays blocked |
| `TestAnalyze_Deterministic` | Stable output ordering: `DepsSatisfied` sorted, each `Cycles` slice sorted, outer `Cycles` sorted by first element, `BlockDetail` slices sorted by `DependencyID` |
| `TestAnalyze_BlockReasons` | `Blocked` entries carry correct `BlockReason` for each case (waiting, unknown, external, ambiguous) |

### Diagnostics tests (`internal/queue/diagnostics_test.go`)

| Test | Coverage |
|------|----------|
| `TestDiagnoseDependencies_DuplicateWaitingID` | Two waiting files with same `meta.ID` produce `DependencyDuplicateID` issue; duplicate node is skipped |
| `TestDiagnoseDependencies_StemAlias` | `depends_on` referencing a filename stem resolves for completed targets but not for waiting targets (asymmetry preserved) |
| `TestDiagnoseDependencies_AmbiguousID` | Ambiguous ID produces both `DependencyAmbiguousID` issue and `BlockedByAmbiguous` in `Analysis.Blocked` |

### Queue tests

| Test | Coverage |
|------|----------|
| `TestReconcileReadyQueue_ChainPromotionSemantics` | One-hop promotion preserved |
| `TestReconcileReadyQueue_AmbiguousCompletedDoesNotSatisfy` | Existing safety preserved |
| `TestReconcileReadyQueue_CycleMovesToFailed` | 2-node cycle members moved to `failed/` with `<!-- cycle-failure:` marker in file |
| `TestReconcileReadyQueue_LongCycleMovesToFailed` | 3+ node cycle (`A -> B -> C -> A`) — all three moved to `failed/`, downstream stays in `waiting/` |
| `TestReconcileReadyQueue_CycleDoesNotConsumeRetryBudget` | `CountFailureMarkers` returns 0 for task with only cycle-failure records |
| `TestReconcileReadyQueue_DownstreamOfCycleRemainsWaiting` | Non-cycle nodes blocked by cycle stay in `waiting/` |
| `TestReconcileReadyQueue_PartialCycleMoveIdempotent` | If `AtomicMove` fails for one cycle member, cycle-failure record is still present and task stays in `waiting/` blocked |
| `TestCountPromotableWaitingTasks_MatchesAnalyze` | Reconcile/count alignment |

### Status tests

| Test | Coverage |
|------|----------|
| `TestWaitingTasksStatus_PreservesPerDependencyStates` | Existing state detail preserved |
| `TestWaitingTasksStatus_CycleLabel` | Cyclic tasks surfaced clearly |
| `TestWaitingTasksStatus_DownstreamOfCycleNotMarkedCyclic` | No false cycle labeling |
| `TestFailedTaskRendering_CycleFailure` | Cycle-failed task renders `circular dependency detected` instead of `0/N retries exhausted` |
| `TestFailedTaskRendering_MixedFailures` | Task with both `<!-- failure:` and `<!-- cycle-failure:` markers renders correctly (retry count from agent failures only, cycle reason displayed) |

### Integration tests

| Test | Coverage |
|------|----------|
| `TestDAG_ChainPromotion` | `A -> B -> C` promotes one hop at a time |
| `TestDAG_CycleMovesToFailed` | 2-node cycle: members in `failed/`, downstream stays in `waiting/` |
| `TestDAG_LongCycleMovesToFailed` | 3+ node cycle (`A -> B -> C -> A`): all cycle members in `failed/`, downstream `D -> C` in `waiting/` |
| `TestDAG_CycleStatus` | Cycle visible in status output |
| `TestDAG_AmbiguousID` | Ambiguous completed/non-completed ID remains blocked |

## Backward Compatibility

### Preserved

- `depends_on` syntax unchanged.
- Acyclic promotion semantics unchanged.
- Existing dependency-context JSON unchanged in v1.
- Existing per-dependency state reporting in status preserved.

### Intentional improvements

- Cycles become visible in status/diagnostics instead of stderr-only warnings.
- Cyclic tasks are moved to `failed/` with dedicated failure records that do not
  consume retry budget.
- Internal dependency logic becomes shared rather than duplicated.

### Behavioral change: cycle-to-failed

Previously, cyclic waiting tasks (including self-dependent tasks) remained in
`waiting/` indefinitely with only a stderr warning. Now they are moved to
`failed/` during reconcile. This is a deliberate improvement: cycles cannot
self-resolve, and silent indefinite blocking is worse than explicit failure.
Users can fix the cycle by editing the task's `depends_on` and moving it back
to `waiting/`.

This applies to both multi-node cycles (`A -> B -> A`) and self-cycles
(`A -> A`). Self-dependencies are treated as self-cycles (SCC of size 1 with a
self-edge) and follow the same cycle-to-failed sequence. The existing test
`TestReconcileReadyQueue_DetectsSelfDependency` must be updated to expect the
task in `failed/` rather than `waiting/`.

## Implementation Order

1. Create `internal/dag` with deterministic analysis + SCC-based cycle
   detection, fully covered by unit tests.
2. Add `DiagnoseDependencies()` in `internal/queue/diagnostics.go` wrapping
   `dag.Analyze()` and exposing the underlying `Analysis`.
3. Refactor `internal/queue/reconcile.go` to use `DiagnoseDependencies()` for
   promotion, cycle-to-failed behavior, and structured warnings, while
   preserving existing acyclic semantics.
4. Update `internal/status/` to surface cycle information while preserving
   current dependency state detail.
5. Update docs.

## Deferred Follow-Ups

1. **`mato graph` command**: export DOT or other graph output after the shared
   analysis layer exists.
2. **Transitive dependency context**: useful, but separate from the core DAG
   refactor.
3. **Consolidate status I/O onto `PollIndex`**: eliminate the duplicate
   filesystem scan in `internal/status/status.go` by exposing an
   `IDStates() map[string]string` method on `PollIndex`.
4. **Unify stem and `meta.ID` resolution for waiting edges**: currently
   waiting-edge resolution uses `meta.ID` only, while completed-dep resolution
   uses both stem and `meta.ID`. Unifying requires alias-collision detection
   (one task's stem can collide with another's explicit `id`) and should be
   designed as a separate follow-up.
