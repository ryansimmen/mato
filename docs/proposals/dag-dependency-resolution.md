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

Estimated effort: ~3-5 days.

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

### v1 recommendation: detect and report first, fail later only if desired

The safest first step is to centralize detection and expose cycle information in
status and diagnostics. That alone is a meaningful improvement.

If we do choose to move cyclic tasks to `failed/`, the plan must be more
precise than the earlier draft:

- only actual cycle members should fail,
- downstream tasks blocked by a cycle should remain blocked, not be marked
  cyclic,
- failure records must not consume normal retry budget unless the retry model is
  updated explicitly.

Because of that nuance, this plan recommends a phased rollout:

### Phase 1

- Add explicit cycle detection.
- Surface cycles in `mato status` and dependency diagnostics.
- Keep reconcile behavior read-only with respect to cycles.

### Phase 2

- Optionally move true cycle members to `failed/` once classification and
  failure-record semantics are fully specified and tested.

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
4. If exact cycle groups are needed, run SCC detection on the waiting subgraph
   and keep only SCCs that are truly cyclic:
   - size > 1, or
   - size == 1 with a self-edge.

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
4. Promote only `analysis.Ready` tasks that also pass `idx.HasActiveOverlap()`.
5. Reuse the same analysis results to emit structured warnings for:
   - self-dependencies,
   - cycles,
   - unknown dependencies,
   - ambiguous IDs.

The current helper functions can be removed only after their behavior is fully
covered by tests.

## Dependency Diagnostics

To keep status and reconcile aligned, ambiguous-ID and dependency diagnostics
should share a single read-only helper, likely in `internal/queue/`.

Example:

```go
type DependencyDiagnostics struct {
    Ready         []string
    CycleMembers  map[string][]string
    UnknownDeps   map[string][]string
    SelfDependent map[string]bool
    AmbiguousIDs  []string
}

func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics
```

`ReconcileReadyQueue()` and any future `mato doctor` dependency checks should
reuse this logic instead of drifting apart.

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
| `TestReconcileReadyQueue_CycleWarningOrClassification` | Cycle surfaced consistently |
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
| `TestDAG_CycleStatus` | Cycle visible in status output |
| `TestDAG_AmbiguousID` | Ambiguous completed/non-completed ID remains blocked |

If the project later chooses cycle-to-failed behavior, add dedicated
integration tests for retry counting, failure records, and downstream tasks.

## Backward Compatibility

### Preserved

- `depends_on` syntax unchanged.
- Acyclic promotion semantics unchanged.
- Existing dependency-context JSON unchanged in v1.
- Existing per-dependency state reporting in status preserved.

### Intentional improvements

- Cycles become visible in status/diagnostics instead of stderr-only warnings.
- Internal dependency logic becomes shared rather than duplicated.

### Possible later behavioral change

If a later phase moves cycle members to `failed/`, that should be documented as
an explicit behavior change with retry semantics spelled out.

## Implementation Order

1. Create `internal/dag` with deterministic analysis tests.
2. Refactor `internal/queue/reconcile.go` to use shared analysis while keeping
   existing promotion behavior.
3. Add or refactor dependency diagnostics in `internal/queue/` so status and
   doctor can reuse the same reasoning.
4. Update `internal/status/` to surface cycle information while preserving
   current dependency state detail.
5. Update docs.
6. Consider cycle-to-failed only as a separate follow-up once exact cycle
   classification is proven.

## Deferred Follow-Ups

1. **`mato graph` command**: export DOT or other graph output after the shared
   analysis layer exists.
2. **Transitive dependency context**: useful, but separate from the core DAG
   refactor.
3. **Cycle-to-failed behavior**: only after SCC-based classification and retry
   semantics are fully specified.
