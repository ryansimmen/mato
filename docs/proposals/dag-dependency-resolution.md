# DAG-Based Dependency Resolution

## Summary

Replace the ad-hoc recursive DFS in `internal/queue/reconcile.go` with a proper DAG resolver using Kahn's algorithm in a new `internal/dag/` package. This gives O(V+E) promotion resolution (down from O(N²)), definitive cycle detection that moves cyclic tasks to `failed/`, transitive dependency context for agents, and actionable dependency status in `mato status`.

## Motivation

Three problems with the current implementation:

1. **Performance.** `dependsOnWaitingTask()` does recursive DFS with a fresh `visited` set per call per dependency per waiting task. For N waiting tasks with M deps each, this is O(N·M·N) worst case.

2. **Silent cycles.** `logCircularDependency()` prints to stderr, but cyclic tasks sit in `waiting/` forever. In unattended CI/CD, the warning is invisible and the tasks never resolve.

3. **Opaque blocking.** `mato status` lists waiting tasks with their dependencies and per-dep state indicators, but cannot distinguish "blocked by an in-progress task" from "part of a cycle" — the information needed to take action.

## Design

### New package: `internal/dag/`

A pure graph logic package. No file I/O, no mato-specific types, no imports from `internal/queue` or `internal/frontmatter`. Both `internal/queue` and `internal/status` import it.

Input is a lightweight list of `Node{ID, DependsOn}` plus a `completedIDs` set. Output is an `Analysis` struct with three partitions of the waiting task set: ready, blocked, and cyclic.

### Graph scope

The graph contains **only waiting tasks**. Completed dependencies are pre-resolved: they are checked against the `completedIDs` set before graph construction. A waiting task whose `DependsOn` entries are all in `completedIDs` (or reference unknown/non-waiting IDs) has in-degree zero and is immediately ready — it never participates in Kahn's traversal.

Only waiting-to-waiting edges appear in the graph. Dependencies on tasks in other states (in-progress, ready-for-review, etc.) are "in flight" — they make a task blocked, not cyclic.

### Cycle handling

Kahn's algorithm identifies cycles definitively: any nodes remaining after the topological sort terminates are in cycles. Cyclic tasks are moved to `failed/` with a descriptive failure record:

```
<!-- failure: circular dependency detected with [task-a, task-b] -->
```

This is analogous to how malformed task files are already moved to `failed/`. Cycle failures are **not** counted against `max_retries` — they are a structural error, not a runtime failure.

### Cascade failure policy (v1)

**Decision: NO cascade failure in v1.** If task A fails (moved to `failed/`), tasks that depend on A remain in `waiting/`. Since A will never reach `completed/`, its dependents will never promote. This is correct behavior — the dependency is unsatisfied, and the operator must fix A or remove the dependency. Cascade failure (auto-failing dependents of a failed task) is deferred to a future version.

### Transitive dependency context

`dag.Ancestors()` computes the full transitive closure for a task. The dependency context JSON file adds a `transitive` field alongside the existing flat array, preserving backward compatibility:

```json
[
  {"task_id": "B", "branch": "task/B", ...},
  {"task_id": "A", "branch": "task/A", ...}
]
```

becomes:

```json
[
  {"task_id": "B", "branch": "task/B", ...},
  {"task_id": "A", "branch": "task/A", ...}
],
"transitive": [
  {"task_id": "A", "branch": "task/A", ...}
]
```

Wait — that's not valid JSON at the top level. The correct approach: the existing format is a JSON array. We keep it as-is (all deps, direct first, then transitive appended) and write a **separate companion field**. Since the current format is a bare array consumed by agent prompts, changing it to an object would break existing prompts. Instead:

**Option chosen: additive separate file.** The existing array at `dependency-context-<task>.json` remains a flat array of `CompletionDetail` objects (direct deps only, unchanged). A new file `dependency-context-<task>-transitive.json` contains transitive-only deps. This avoids any format break. Agents that don't know about transitive deps continue working. Agents that do can read both files.

Alternatively, if we prefer a single file: wrap in an object with `"direct"` and `"transitive"` keys, and update the agent prompt instructions to parse the new format. Since the prompt is embedded in `internal/runner/` and shipped with mato (not user-maintained), this is also safe. **Recommend single-file object format** since we control the prompt:

```json
{
  "direct": [
    {"task_id": "B", "branch": "task/B", "commit_sha": "abc123", ...}
  ],
  "transitive": [
    {"task_id": "A", "branch": "task/A", "commit_sha": "def456", ...}
  ]
}
```

The task instructions prompt (`internal/runner/task-instructions.md`) must be updated to document the new shape.

## API Surface

**File:** `internal/dag/dag.go`

```go
package dag

// Node represents a task in the dependency graph.
type Node struct {
	ID        string
	DependsOn []string
}

// Analysis holds the complete DAG analysis results.
type Analysis struct {
	// Ready contains task IDs with all dependencies satisfied.
	// Sorted lexicographically for determinism.
	Ready []string

	// Blocked maps task ID to its list of unsatisfied dependency IDs.
	// Excludes tasks that are ready or cyclic.
	Blocked map[string][]string

	// Cycles contains groups of task IDs involved in circular
	// dependencies. Each group is one cycle.
	Cycles [][]string
}

// Analyze builds a dependency graph from waiting tasks and runs Kahn's
// algorithm to partition them into ready, blocked, and cyclic sets.
// completedIDs are dependencies considered already satisfied.
func Analyze(tasks []Node, completedIDs map[string]struct{}) Analysis

// Ancestors returns all transitive dependency IDs for the given task,
// traversing the full dependency chain through the provided task set.
// Results are sorted lexicographically. Returns nil if id is not found.
func Ancestors(tasks []Node, id string) []string
```

`Analyze` is the primary API. Callers should not need to compute ready, blocked, and cyclic sets separately. `Ancestors` is a standalone function used only by `writeDependencyContextFile()` at claim time, not during reconciliation.

Note: `Analyze` and `Ancestors` are package-level functions, not methods on a struct. The graph is an internal implementation detail — callers pass inputs and get results. If future needs require caching the graph (e.g., calling `Ancestors` for multiple tasks), we can introduce a `Graph` struct then.

## Algorithm

```
function Analyze(waitingTasks, completedIDs):
    // Phase 1: Build adjacency list and in-degree map.
    // Only add edges for waiting-to-waiting dependencies.
    waitingIDs = set of all task IDs in waitingTasks
    adjList    = map[string][]string{}   // dep → dependents (reverse edges)
    inDegree   = map[string]int{}
    depsOf     = map[string][]string{}   // task → its DependsOn list

    for each task in waitingTasks:
        inDegree[task.ID] = 0
        depsOf[task.ID] = task.DependsOn

    for each task in waitingTasks:
        for each dep in task.DependsOn:
            if dep in completedIDs:
                continue                 // already satisfied
            if dep not in waitingIDs:
                continue                 // in-flight or unknown — makes task blocked, not graphed
            adjList[dep] = append(adjList[dep], task.ID)
            inDegree[task.ID]++

    // Phase 2: Kahn's topological sort.
    queue = all task IDs where inDegree == 0
    sorted = []

    while queue not empty:
        node = dequeue()
        sorted = append(sorted, node)
        for each dependent in adjList[node]:
            inDegree[dependent]--
            if inDegree[dependent] == 0:
                enqueue(dependent)

    // Phase 3: Classify results.
    ready   = []
    blocked = map[string][]string{}

    for each node in sorted:
        allSatisfied = true
        for each dep in depsOf[node]:
            if dep not in completedIDs and dep not in waitingIDs:
                allSatisfied = false     // dep is in-flight or unknown
        if allSatisfied:
            ready = append(ready, node)
        else:
            unsatisfied = [dep for dep in depsOf[node] if dep not in completedIDs]
            blocked[node] = unsatisfied

    // Nodes not in sorted are cyclic.
    processedSet = set(sorted)
    cycleMembers = [id for id in waitingIDs if id not in processedSet]
    cycles = extractCycles(cycleMembers, depsOf)

    return Analysis{ready, blocked, cycles}
```

Cycle extraction: walk the residual subgraph (only cycle members and their mutual edges) with iterative DFS to find connected components. Full Tarjan's is unnecessary since Kahn's already isolated the cyclic partition.

**Complexity:** O(V+E) where V = number of waiting tasks, E = number of dependency edges between them.

## Integration Points

### `internal/queue/reconcile.go`

Replace the body of `resolvePromotableTasks()`:

```go
import "mato/internal/dag"

func resolvePromotableTasks(tasksDir string, idx *PollIndex) []promotableTask {
	idx = ensureIndex(tasksDir, idx)

	completedIDs := idx.CompletedIDs()
	nonCompletedIDs := idx.NonCompletedIDs()

	safeCompleted := make(map[string]struct{}, len(completedIDs))
	for id := range completedIDs {
		safeCompleted[id] = struct{}{}
	}
	for id := range nonCompletedIDs {
		delete(safeCompleted, id)
	}

	waitingTasks := idx.TasksByState(DirWaiting)
	nodes := make([]dag.Node, 0, len(waitingTasks))
	snapByID := make(map[string]*TaskSnapshot, len(waitingTasks))
	for _, snap := range waitingTasks {
		nodes = append(nodes, dag.Node{
			ID:        snap.Meta.ID,
			DependsOn: snap.Meta.DependsOn,
		})
		snapByID[snap.Meta.ID] = snap
	}

	analysis := dag.Analyze(nodes, safeCompleted)

	var result []promotableTask
	for _, id := range analysis.Ready {
		snap := snapByID[id]
		if snap == nil {
			continue
		}
		if idx.HasActiveOverlap(snap.Meta.Affects) {
			continue
		}
		result = append(result, promotableTask{
			name: snap.Filename, path: snap.Path, meta: snap.Meta,
		})
	}
	return result
}
```

Replace cycle handling in `ReconcileReadyQueue()`:

```go
// Replace the logCircularDependency/dependsOnWaitingTask block with:
analysis := dag.Analyze(nodes, safeCompleted)

for _, cycle := range analysis.Cycles {
	cycleDesc := strings.Join(cycle, ", ")
	for _, taskID := range cycle {
		snap := snapByID[taskID]
		if snap == nil {
			continue
		}
		failedPath := filepath.Join(tasksDir, DirFailed, snap.Filename)
		if err := AtomicMove(snap.Path, failedPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move cyclic task %s to failed/: %v\n",
				snap.Filename, err)
			continue
		}
		record := fmt.Sprintf("\n<!-- failure: circular dependency detected with [%s] -->\n", cycleDesc)
		appendFailureRecord(failedPath, record)
	}
}

for taskID, deps := range analysis.Blocked {
	snap := snapByID[taskID]
	if snap == nil {
		continue
	}
	for _, dep := range deps {
		if _, ok := knownIDs[dep]; !ok {
			fmt.Fprintf(os.Stderr, "warning: waiting task %s depends on unknown task ID %q\n",
				snap.Filename, dep)
		}
	}
}
```

**Remove entirely:** `dependsOnWaitingTask()` and `logCircularDependency()`. These are replaced by `dag.Analyze()` and are not called from anywhere else.

### `internal/runner/task.go`

Update `writeDependencyContextFile()` to include transitive deps:

```go
import "mato/internal/dag"

func writeDependencyContextFile(tasksDir string, claimed *queue.ClaimedTask) string {
	meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath)
	if err != nil || len(meta.DependsOn) == 0 {
		return ""
	}

	// Collect all waiting tasks to build the dependency graph for ancestors.
	// (In practice, most deps are already completed at claim time.)
	directSet := make(map[string]struct{}, len(meta.DependsOn))
	for _, dep := range meta.DependsOn {
		directSet[dep] = struct{}{}
	}

	var directDetails, transitiveDetails []messaging.CompletionDetail
	for _, dep := range meta.DependsOn {
		detail, err := messaging.ReadCompletionDetail(tasksDir, dep)
		if err != nil {
			continue
		}
		directDetails = append(directDetails, *detail)
	}

	// Compute transitive ancestors beyond direct deps.
	// Build nodes from all completed tasks that have completion details.
	// For v1, only include transitive deps that have completion details.
	ancestors := dag.Ancestors(buildNodeList(tasksDir, meta), meta.ID)
	for _, anc := range ancestors {
		if _, isDirect := directSet[anc]; isDirect {
			continue
		}
		detail, err := messaging.ReadCompletionDetail(tasksDir, anc)
		if err != nil {
			continue
		}
		transitiveDetails = append(transitiveDetails, *detail)
	}

	if len(directDetails) == 0 && len(transitiveDetails) == 0 {
		return ""
	}

	type depContext struct {
		Direct     []messaging.CompletionDetail `json:"direct"`
		Transitive []messaging.CompletionDetail `json:"transitive,omitempty"`
	}
	ctx := depContext{Direct: directDetails, Transitive: transitiveDetails}
	data, err := json.Marshal(ctx)
	if err != nil {
		return ""
	}

	depCtxPath := filepath.Join(tasksDir, "messages",
		"dependency-context-"+claimed.Filename+".json")
	if err := os.WriteFile(depCtxPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write dependency context file: %v\n", err)
		return ""
	}
	return depCtxPath
}
```

The embedded task instructions (`internal/runner/task-instructions.md`) must document the `{"direct": [...], "transitive": [...]}` format.

### `internal/status/status.go`

Update `waitingTasksStatus()` to categorize tasks using the DAG:

```go
import "mato/internal/dag"

func waitingTasksStatus(tasksDir string) ([]waitingTaskSummary, error) {
	idx := queue.BuildIndex(tasksDir)
	completedIDs := idx.CompletedIDs()
	waitingTasks := idx.TasksByState(queue.DirWaiting)

	nodes := make([]dag.Node, 0, len(waitingTasks))
	for _, snap := range waitingTasks {
		nodes = append(nodes, dag.Node{
			ID:        snap.Meta.ID,
			DependsOn: snap.Meta.DependsOn,
		})
	}

	analysis := dag.Analyze(nodes, completedIDs)

	// Build categorized output using analysis.Ready, analysis.Blocked, analysis.Cycles
	// ... annotate each waiting task as Ready/Blocked/Cyclic with specific dep info
}
```

The `renderDependencyBlocked()` function in `status_render.go` should display three categories:

| Category    | Display                                      |
|-------------|----------------------------------------------|
| Ready       | "Ready for promotion"                        |
| Blocked     | "Blocked by: setup-database, init-config"    |
| Cyclic      | "Circular dependency: [task-a, task-b]"      |

## Test Plan

### Unit tests (`internal/dag/dag_test.go`)

Table-driven tests with `t.Run`:

| Test                             | Topology              | Expected                                       |
|----------------------------------|-----------------------|------------------------------------------------|
| `TestAnalyze_EmptyGraph`         | No tasks              | Ready=[], Blocked={}, Cycles=[]                |
| `TestAnalyze_NoDeps`             | A (no deps)           | Ready=[A]                                      |
| `TestAnalyze_SingleDepCompleted` | A→B, B completed      | Ready=[A]                                      |
| `TestAnalyze_SimpleChain`        | A→B→C, A completed    | Ready=[B], Blocked={C: [B]}                    |
| `TestAnalyze_FanIn`              | A→C, B→C, both done   | Ready=[C]                                      |
| `TestAnalyze_FanOut`             | A→B, A→C, A completed | Ready=[B, C]                                   |
| `TestAnalyze_Diamond`            | A→C, B→C, A→B, A done | Ready=[B], Blocked={C: [B]}                    |
| `TestAnalyze_SelfCycle`          | A→A                   | Cycles=[[A]]                                   |
| `TestAnalyze_TwoNodeCycle`       | A→B, B→A              | Cycles=[[A, B]]                                |
| `TestAnalyze_ThreeNodeCycle`     | A→B→C→A               | Cycles=[[A, B, C]]                             |
| `TestAnalyze_MixedCycleAndReady` | A→B→A, C (no deps)    | Cycles=[[A, B]], Ready=[C]                     |
| `TestAnalyze_UnknownDep`         | A→X (X absent)        | Blocked={A: [X]}                               |
| `TestAnalyze_InFlightDep`        | A→B, B in-progress    | Blocked={A: [B]} (B not in waiting or completed)|
| `TestAnalyze_Deterministic`      | Multiple nodes        | Same output across 100 runs                    |
| `TestAnalyze_LargeGraph`         | 100+ nodes, chain     | Correct ready/blocked sets, runs in <10ms      |
| `TestAncestors_Chain`            | A→B→C                 | Ancestors(C)=[A, B]                            |
| `TestAncestors_Diamond`          | A→C, B→C, A→B        | Ancestors(C)=[A, B]                            |
| `TestAncestors_NotFound`         | A→B                   | Ancestors(X)=nil                               |

### Integration tests (`internal/integration/`)

| Test                           | Scenario                                                                 |
|--------------------------------|--------------------------------------------------------------------------|
| `TestDAG_ChainPromotion`       | 3-task chain: A, B→A, C→B. Complete A, verify B promotes. Complete B, verify C promotes. |
| `TestDAG_CycleToFailed`        | A→B, B→A. Run reconcile, verify both moved to `failed/` with failure record. |
| `TestDAG_DiamondFanIn`         | A→C, B→C. Complete A only, verify C stays in waiting. Complete B, verify C promotes. |
| `TestDAG_StatusCategorization` | Mix of ready/blocked/cyclic waiting tasks. Verify `mato status` output. |

### Prompt validation

The updated `task-instructions.md` format for dependency context must be validated by an integration test that asserts the JSON structure matches the documented schema.

## Backward Compatibility

| Area                    | Change                                                                 | Impact   |
|-------------------------|------------------------------------------------------------------------|----------|
| `depends_on` YAML       | No change                                                              | None     |
| Promotion behavior      | Identical for acyclic graphs                                           | None     |
| Cyclic tasks            | Moved to `failed/` instead of sitting in `waiting/` forever            | Intentional behavioral change |
| `max_retries`           | Cycle failures not counted                                             | None     |
| Dependency context JSON | Changes from flat array to `{"direct": [...], "transitive": [...]}` object | Breaking for agents parsing old format; mitigated by updating embedded prompt |
| stderr warnings         | Different wording from DAG analysis path                               | Tests matching exact stderr strings need updating |
| Removed functions       | `dependsOnWaitingTask()`, `logCircularDependency()` deleted entirely   | Internal only; no public API |

## Effort Estimate

| Task                                                    | Effort   |
|---------------------------------------------------------|----------|
| `internal/dag/dag.go` — `Analyze()` with Kahn's        | 3 hours  |
| `internal/dag/dag.go` — `Ancestors()`                   | 1 hour   |
| `internal/dag/dag_test.go` — 17+ test cases             | 3 hours  |
| Refactor `internal/queue/reconcile.go`                  | 2 hours  |
| Update `internal/runner/task.go` for transitive context | 1.5 hours|
| Update `internal/status/` for categorization            | 1.5 hours|
| Update existing tests in `queue` and `status`           | 1.5 hours|
| Integration tests                                       | 2 hours  |
| Documentation (`task-format.md`, `architecture.md`)     | 1 hour   |
| Update `task-instructions.md` prompt                    | 0.5 hours|
| **Total**                                               | **~3-4 days** |

## Implementation Order

1. **Create `internal/dag/dag.go` and `internal/dag/dag_test.go`.** Implement `Analyze()` and `Ancestors()` with exhaustive unit tests. No integration yet — this is pure logic.

2. **Refactor `internal/queue/reconcile.go`.** Replace `resolvePromotableTasks()` internals with `dag.Analyze()`. Add cycle-to-failed logic in `ReconcileReadyQueue()`. Delete `dependsOnWaitingTask()` and `logCircularDependency()`. Update `reconcile_test.go`.

3. **Update `internal/runner/task.go`.** Change `writeDependencyContextFile()` to use `dag.Ancestors()` and emit the `{"direct", "transitive"}` JSON format. Update `task-instructions.md`.

4. **Update `internal/status/`.** Use `dag.Analyze()` in `waitingTasksStatus()` to categorize tasks as Ready/Blocked/Cyclic. Update `renderDependencyBlocked()` to display categories.

5. **Integration tests.** Chain promotion, cycle-to-failed, diamond fan-in, status categorization.

6. **Documentation.** Update `docs/task-format.md` (cycle-to-failed behavior) and `docs/architecture.md` (DAG-based resolution).

## Open Questions

1. **`mato graph` command.** Export dependency graph as DOT format for Graphviz visualization. Natural extension of `internal/dag/`.

2. **Priority-aware ready ordering.** Within the ready set from `Analyze()`, should tasks be ordered by priority? Currently handled downstream by `BacklogByPriority()` after promotion — may not need DAG involvement.

3. **In-flight vs. waiting distinction in status.** Should `mato status` differentiate "blocked by in-progress task" from "blocked by another waiting task"? The information is available from `PollIndex` state lookups. Low effort to add.

4. **`depends_on` with completion conditions.** Specifying "depends on A being in ready-to-merge" for review coordination. Deferred — adds significant complexity.
