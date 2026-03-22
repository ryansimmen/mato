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

Estimated effort: ~3 days.

## Effort Breakdown

| Task | Effort |
|------|--------|
| `internal/dag/dag.go` — Kahn's + SCC + `Analyze()` | 3 hours |
| `internal/dag/dag_test.go` — unit tests (ready, blocked, cycles, determinism) | 3 hours |
| `internal/queue/reconcile.go` — refactor to use `dag.Analyze()`, add cycle-to-failed | 3 hours |
| `internal/queue/diagnostics.go` — `DiagnoseDependencies()` wrapper | 2 hours |
| `internal/queue/diagnostics_test.go` + reconcile test updates | 3 hours |
| `internal/status/status.go` — cycle-aware summaries | 2 hours |
| Status rendering + tests | 2 hours |
| Integration tests | 2 hours |
| Documentation updates | 1 hour |
| **Total** | **~3 days** |

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

type Analysis struct {
    Ready   []string
    Blocked map[string][]string
    Cycles  [][]string
}

func Analyze(waiting []Node, completedIDs map[string]struct{}) Analysis
```

### Scope of the graph

The graph contains only waiting tasks.

- Dependencies in `completed/` are satisfied input state.
- Dependencies in `waiting/` are graph edges.
- Dependencies in `backlog/`, `in-progress/`, `ready-for-review/`,
  `ready-to-merge/`, or `failed/` are **not** graph edges. They remain blocked
  external dependencies from the caller's point of view.

This matches current queue behavior and avoids blurring "waiting blocker" with
"in-flight blocker."

## Readiness Rules

`Analyze()` should preserve the current reconcile logic exactly:

1. A self-dependency is not ready.
2. A dependency on a waiting task blocks readiness.
3. A dependency on a completed ID satisfies readiness.
4. A dependency on any non-completed or unknown ID blocks readiness.
5. Ambiguous IDs must be excluded from the completed set before analysis.

The caller is responsible for passing `safeCompleted`, not raw `completedIDs`.

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

- Cycle failures use a dedicated failure reason (`"circular dependency"`) in the
  failure record appended to the task file.
- Cycle failures do **not** consume normal retry budget. They are a structural
  problem, not a transient agent failure.
- Downstream tasks remain in `waiting/` with their existing `depends_on`. Once
  the cycle is broken (user edits or removes the cyclic dependency), downstream
  tasks become promotable through normal reconciliation.

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
3. Use readiness rules plus `safeCompleted` to populate `Ready` and `Blocked`.
4. Run SCC detection on the residual waiting subgraph and keep only SCCs that
   are truly cyclic:
   - size > 1, or
   - size == 1 with a self-edge.
5. Classify remaining residual nodes (downstream of cycles) as blocked.

This avoids misclassifying nodes that are only downstream of a cycle.

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

## Reconcile Integration

### `internal/queue/reconcile.go`

Refactor reconcile to reuse shared analysis, but do not broaden readiness.

Recommended shape:

1. Build `safeCompleted` exactly as today.
2. Build waiting-task `dag.Node` list.
3. Call `dag.Analyze(nodes, safeCompleted)`.
4. Move `analysis.Cycles` members to `failed/` with `"circular dependency"`
   failure records. Cycle failures do not consume retry budget.
5. Promote only `analysis.Ready` tasks that also pass `idx.HasActiveOverlap()`.
6. Reuse `DiagnoseDependencies()` to emit structured warnings for:
   - self-dependencies,
   - cycles (already handled, but logged),
   - unknown dependencies,
   - ambiguous IDs.

The current helper functions can be removed only after their behavior is fully
covered by tests.

## Dependency Diagnostics

To keep status and reconcile aligned, dependency diagnostics should share a
single read-only helper in `internal/queue/diagnostics.go`. This helper wraps
`dag.Analyze()` and translates graph results into a flat issue list usable by
both `ReconcileReadyQueue()` warnings and `mato doctor`.

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

func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

`ReconcileReadyQueue()` calls `DiagnoseDependencies()` internally and emits
warnings from the structured results, replacing the current inline warning
logic. `mato doctor` calls `DiagnoseDependencies()` and converts issues into
`Finding` objects. Both share the same underlying `dag.Analyze()` call.

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
| `internal/dag/dag.go` | New shared analysis helper |
| `internal/dag/dag_test.go` | Unit tests for ready/blocked/cycle analysis |
| `internal/queue/reconcile.go` | Replace duplicated recursive dependency checks with `dag.Analyze()` |
| `internal/queue/queue_test.go` | Preserve current promotion semantics + add cycle cases |
| `internal/status/status.go` | Add cycle-aware waiting-task summaries without removing existing state detail |
| `internal/status/status_render.go` | Render cycle information clearly |
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
| `TestAnalyze_NoDeps` | Ready task |
| `TestAnalyze_Chain` | `A completed`, `B -> A`, `C -> B` -> `B ready`, `C blocked` |
| `TestAnalyze_FanIn` | One unsatisfied dep keeps node blocked |
| `TestAnalyze_SelfDependency` | Self-dep not ready, flagged |
| `TestAnalyze_UnknownDependency` | Missing dep blocks |
| `TestAnalyze_AmbiguousCompletedExcluded` | Caller-supplied `safeCompleted` behavior preserved |
| `TestAnalyze_CycleMembersOnly` | Cycle members identified without misclassifying downstream nodes |
| `TestAnalyze_Deterministic` | Stable output ordering |

### Queue tests

| Test | Coverage |
|------|----------|
| `TestReconcileReadyQueue_ChainPromotionSemantics` | One-hop promotion preserved |
| `TestReconcileReadyQueue_AmbiguousCompletedDoesNotSatisfy` | Existing safety preserved |
| `TestReconcileReadyQueue_CycleMovesToFailed` | Cycle members moved to `failed/` with correct failure reason |
| `TestReconcileReadyQueue_CycleDoesNotConsumeRetryBudget` | Cycle failure records don't count as agent retries |
| `TestReconcileReadyQueue_DownstreamOfCycleRemainsWaiting` | Non-cycle nodes blocked by cycle stay in `waiting/` |
| `TestCountPromotableWaitingTasks_MatchesAnalyze` | Reconcile/count alignment |

### Status tests

| Test | Coverage |
|------|----------|
| `TestWaitingTasksStatus_PreservesPerDependencyStates` | Existing state detail preserved |
| `TestWaitingTasksStatus_CycleLabel` | Cyclic tasks surfaced clearly |
| `TestWaitingTasksStatus_DownstreamOfCycleNotMarkedCyclic` | No false cycle labeling |

### Integration tests

| Test | Coverage |
|------|----------|
| `TestDAG_ChainPromotion` | `A -> B -> C` promotes one hop at a time |
| `TestDAG_CycleMovesToFailed` | Cycle members in `failed/`, downstream stays in `waiting/` |
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

Previously, cyclic waiting tasks remained in `waiting/` indefinitely with only
a stderr warning. Now they are moved to `failed/` during reconcile. This is a
deliberate improvement: cycles cannot self-resolve, and silent indefinite
blocking is worse than explicit failure. Users can fix the cycle by editing the
task's `depends_on` and moving it back to `waiting/`.

## Implementation Order

1. Create `internal/dag` with deterministic analysis + SCC-based cycle
   detection, fully covered by unit tests.
2. Refactor `internal/queue/reconcile.go` to use `dag.Analyze()` for promotion
   and cycle-to-failed behavior while preserving existing acyclic semantics.
3. Add `DiagnoseDependencies()` in `internal/queue/diagnostics.go` so status
   and doctor can reuse the same reasoning.
4. Update `internal/status/` to surface cycle information while preserving
   current dependency state detail.
5. Update docs.

## Deferred Follow-Ups

1. **`mato graph` command**: export DOT or other graph output after the shared
   analysis layer exists.
2. **Transitive dependency context**: useful, but separate from the core DAG
   refactor.
