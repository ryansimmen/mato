# DAG Dependency Resolution — Multi-Agent Debate & Consensus Plan

## Participants

- **Claude Opus 4.6** — Emphasizes separation of concerns and pure graph algorithms
- **GPT-5.4** — Emphasizes practical integration and backward compatibility
- **Gemini 3.1 Pro** — Emphasizes operator experience and status visibility

---

## Round 1: Graph Representation & Architecture

### Claude Opus 4.6

The current implementation in `reconcile.go` is fragile. It mixes file I/O, policy logic, and ad-hoc graph traversal. I propose a dedicated **`internal/dag/`** package. This package should define a `Graph` struct that accepts a list of tasks and their dependencies. It should be pure (no file I/O), testable in isolation, and responsible solely for graph theory operations: topological sorting, cycle detection, and transitive closure.

### GPT-5.4

I understand the desire for purity, but is a new package necessary? We already have `PollIndex` in `internal/queue` which acts as our state cache. We could just add a `BuildDependencyGraph()` method to `PollIndex`. Creating `internal/dag` might force us to duplicate state or map back and forth between "DAG nodes" and "task structs." Let's keep it close to the data in `internal/queue`.

### Gemini 3.1 Pro

I agree with Opus on separation. `queue` is already heavy with filesystem management and locking. However, GPT has a point about data duplication. The `dag` package shouldn't own the *tasks*, just the *relationships*. It should take a lightweight input (map of `ID → []DependsOn`) and return actionable insights. Crucially, we need this structure to be exportable or queryable by `internal/status`. Currently, `mato status` is blind to *why* a task is waiting. If we bury the logic in `queue` private methods, the CLI can't report "Blocked by X."

### Claude Opus 4.6 (response)

Precisely. `internal/dag` will allow `internal/status` to import the logic without importing the heavy `queue` mutation logic. The `Graph` struct can be initialized with a simple map — no need to pass full `TaskMeta` objects.

### **Round 1 Consensus**: Create a new `internal/dag/` package for pure graph operations. It takes lightweight input (ID → dependency list), not full task objects. Both `internal/queue` and `internal/status` consume it.

---

## Round 2: Algorithm Choice

### Claude Opus 4.6

I advocate for **Kahn's algorithm**. It naturally produces a topological sort and clearly identifies cycles — if the algorithm terminates without processing all nodes, the remaining nodes are in cycles. It's O(V+E), straightforward to implement, and widely understood.

### GPT-5.4

Kahn's is the right choice. The current `dependsOnWaitingTask()` function does recursive DFS with a visited set — it's O(N²) in the worst case because it's called per-dependency per-task with fresh visited sets. Kahn's processes the entire graph in one pass.

One important clarification: "batch promotion" is **not** about promoting an entire chain at once. If A→B→C and A completes, only B becomes promotable (C still waits for B). But if A and B both completed, then C is immediately promotable. Kahn's handles this correctly — it finds all nodes whose in-degree drops to zero.

### Gemini 3.1 Pro

Agreed on Kahn's. One advantage: Kahn's gives us the cycle detection for free. Any nodes remaining in the graph after Kahn's terminates are part of cycles. We can extract the strongly connected components (SCCs) from the remaining nodes to report the exact cycle paths.

For SCC extraction: a simple DFS on the residual graph is sufficient — we don't need the full Tarjan's algorithm since Kahn's has already partitioned the graph.

### **Round 2 Consensus**: Use Kahn's algorithm for topological sort + cycle detection. O(V+E) complexity. Extract cycle paths from residual graph after Kahn's terminates.

---

## Round 3: Batch Promotion

### GPT-5.4

Should all promotable tasks be promoted in one pass, or keep the current iterative (one-poll-cycle-at-a-time) approach?

The current system promotes tasks one poll cycle at a time. In a chain A→B→C, if A completes in cycle 1, B gets promoted in cycle 2 (~10s later), not immediately. For a 5-task chain, that's 50 seconds of promotion latency.

I propose: `resolvePromotableTasks()` should use the DAG to compute the **complete set of promotable tasks** in one pass. This means all tasks whose dependencies are satisfied get promoted in the same `ReconcileReadyQueue()` call.

### Claude Opus 4.6

This is already how it works, actually. `resolvePromotableTasks()` iterates all waiting tasks and checks if all their deps are in `completedIDs`. The issue is that it only checks direct deps against the completed set — it doesn't "cascade" within waiting tasks. If B just got promoted (moved to backlog, not yet completed), C still can't be promoted because B isn't in `completedIDs` yet.

The real improvement from Kahn's is that we avoid the O(N²) `dependsOnWaitingTask()` calls and get clearer cycle detection. The promotion frontier (tasks with all deps completed) is computed correctly in one pass.

### Gemini 3.1 Pro

Right. The "batch" improvement is about efficiency and clarity, not about promoting chains faster. The actual promotion speed is bounded by execution time, not promotion latency.

### **Round 3 Consensus**: Use Kahn's to compute the complete promotion frontier in one pass. This is an efficiency and clarity improvement, not a behavioral change. Promotion is still bounded by actual task completion.

---

## Round 4: Cycle Handling

### Gemini 3.1 Pro

Currently, cycles are only logged to stderr via `logCircularDependency()`. In an unattended CI/CD run, that warning is invisible. Cyclic tasks sit in `waiting/` forever.

I propose:
1. **Detection**: Kahn's residual graph identifies all cyclic nodes
2. **Action**: Move cyclic tasks to `failed/` immediately with a failure record: `<!-- failure: circular dependency detected with [list of cycle members] -->`
3. **Status**: If not moved, `mato status` must show them as "BLOCKED (Cycle)"

### Claude Opus 4.6

Moving to `failed/` is aggressive but correct. A cycle cannot resolve itself — no amount of waiting will help. The task author must fix the dependency graph. This is analogous to how malformed task files are already moved to `failed/`.

However, we should add a specific failure message format so tasks can potentially be recovered after the cycle is fixed. The `max_retries` logic should not count cycle failures as retries — they're a different class of error.

### GPT-5.4

I agree with moving to `failed/`, but let's be careful: we should only move tasks if we're confident the cycle is real. Since we're processing all waiting tasks at once with the DAG, the cycle detection is definitive — it's not a heuristic.

One edge case: what if task A depends on B, B is in `in-progress/`, and A depends on C which depends on A? The cycle is A→C→A, not involving B. We need to make sure we only flag the cycle members, not tasks with legitimate pending deps.

### Claude Opus 4.6

Correct. The DAG should be built only from `waiting` tasks. Tasks in other states are either "satisfied" (completed) or "in flight" (in-progress, ready-for-review, ready-to-merge) — neither belongs in the cycle analysis. A waiting task depending on an in-flight task is simply "blocked," not cyclic.

### **Round 4 Consensus**: Move cycle members to `failed/` with descriptive failure record. Only analyze cycles within `waiting/` tasks. Cycle failures are not counted against `max_retries`. Cycle paths are reported in both the failure record and `mato status`.

---

## Round 5: Transitive Dependency Context

### Gemini 3.1 Pro

Agents currently only see direct dependencies via `MATO_DEPENDENCY_CONTEXT`. If Task C depends on B, which depends on A, C only gets B's completion info — not A's. This means C can't see foundational changes from A.

I propose computing the **transitive closure** of dependencies and including all ancestors in the context file.

### GPT-5.4

That's useful but the JSON could get large for deep chains. I'd support this with a practical limit: include transitive deps up to some reasonable depth (e.g., 10 levels), or include all but cap the JSON file size.

### Claude Opus 4.6

The `dag.Ancestors()` function makes this trivial. The context file is typically small (a few KB per dep), so even a 10-task chain would be ~50KB — well within reason. I'd include all transitive deps without artificial limits. If it ever becomes a problem, we can add a cap later.

The more important question is: should we change the JSON format? Currently, `writeDependencyContextFile()` produces an array of direct dep info. For transitive deps, we should distinguish direct vs. transitive:

```json
{
  "direct": [{"id": "B", "branch": "task/B", ...}],
  "transitive": [{"id": "A", "branch": "task/A", ...}]
}
```

### GPT-5.4

Good point. Backward compatibility matters here — if any agent code already parses the current format, we shouldn't break it. But since `MATO_DEPENDENCY_CONTEXT` is consumed by the agent's prompt instructions (not by mato's own code), the format change should be safe as long as the prompt is updated.

### **Round 5 Consensus**: Include transitive dependencies in context file. Use `dag.Ancestors()`. Separate `direct` and `transitive` arrays in JSON. No artificial depth limit.

---

## Round 6: Status Integration

### Gemini 3.1 Pro

`mato status` is currently blind to dependency state. It lists tasks by directory but doesn't explain *why* a task is in `waiting/`. With the DAG, we can categorize waiting tasks:

| Category | Meaning | Display |
|----------|---------|---------|
| **Ready** | All deps satisfied | "Ready for promotion" |
| **Blocked** | Unsatisfied deps | "Blocked by: [list]" |
| **Cyclic** | Part of a cycle | "Circular dependency: [cycle path]" |

### Claude Opus 4.6

`internal/status` should import `internal/dag` to build the graph and categorize tasks. The status command already reads task files — it just needs to run the DAG analysis and annotate the output.

### GPT-5.4

This is the most user-visible improvement. Someone running `mato status` and seeing "Blocked by: setup-database, init-config" is far more useful than just seeing a count of waiting tasks.

### **Round 6 Consensus**: `mato status` categorizes waiting tasks as Ready/Blocked/Cyclic using the DAG. Shows specific blocking dependency IDs.

---

## Round 7: Backward Compatibility

### GPT-5.4

The YAML `depends_on` format is unchanged. The DAG is purely an internal optimization. Key compatibility requirements:

1. Tasks with no dependencies still promote immediately
2. Tasks with satisfied dependencies promote on the next reconcile call
3. Self-dependencies are still detected and warned about
4. Unknown dependency IDs are still warned about
5. The `dependsOnWaitingTask()` function can be removed once the DAG replaces it

### Claude Opus 4.6

One behavioral change: cyclic tasks will now be moved to `failed/` instead of silently sitting in `waiting/`. This is intentional and correct, but it's a change. We should document it in the changelog.

### Gemini 3.1 Pro

Another minor change: warning messages to stderr will have slightly different wording since they'll come from the DAG analysis rather than the ad-hoc DFS. Tests that match on stderr output need updating.

### **Round 7 Consensus**: No YAML format changes. Behavioral change: cyclic tasks → `failed/` (documented). Warning message wording may change. Remove `dependsOnWaitingTask()` after DAG integration.

---

## Round 8: Testing Strategy

### Claude Opus 4.6

The `internal/dag/` package should be exhaustively tested since it's pure logic with no I/O. Test topologies:

| Topology | Description |
|----------|-------------|
| Empty graph | No tasks |
| Single node, no deps | Trivially ready |
| A→B | Simple chain |
| A→B→C | Deep chain |
| A→C, B→C | Fan-in |
| A→B, A→C | Fan-out |
| A→B→C, A→C | Diamond |
| A→A | Self-cycle |
| A→B→A | Two-node cycle |
| A→B→C→A | Three-node cycle |
| Mixed: some cyclic, some ready | Partial graph failure |
| A→? | Dependency on unknown ID |
| Large graph (100+ nodes) | Performance/correctness at scale |

### GPT-5.4

Integration tests in `internal/integration/`:
- 3-task chain: create A, B (depends A), C (depends B). Verify promotion order.
- Cycle: create A→B→A. Verify both move to `failed/`.
- Diamond: create A→C, B→C. Verify C waits for both.

### Gemini 3.1 Pro

Status integration test: create waiting tasks with various dep states, verify `mato status` output shows correct categorization (Ready/Blocked/Cyclic).

### **Round 8 Consensus**: 13+ unit test topologies in `internal/dag/`, 3+ integration tests in `internal/integration/`, status display test.

---

## Round 9: Performance

### GPT-5.4

The current `dependsOnWaitingTask()` is O(N²) because it's called for every dep of every waiting task with a fresh visited set. Kahn's algorithm is O(V+E) where V is the number of waiting tasks and E is the number of dependency edges.

For typical mato usage (10-50 tasks), both are negligible. For large campaigns (100+ tasks), the DAG is significantly better. But the main win is correctness and clarity, not performance.

### Claude Opus 4.6

Agreed. The DAG is built once per reconcile call from the `PollIndex`, which is already cached. No additional file I/O beyond what already happens.

### Gemini 3.1 Pro

One note: `Ancestors()` for transitive context is O(V+E) per call (one BFS/DFS traversal). Called once per task claim, not per poll cycle — acceptable.

### **Round 9 Consensus**: Kahn's is O(V+E), replacing O(N²) DFS. Main win is correctness, not performance. Acceptable for all realistic workloads.

---

## CONSENSUS PLAN

All three agents agree on the following implementation plan.

### 1. New Package: `internal/dag/`

A pure graph logic package with no file I/O or mato-specific types.

**File:** `internal/dag/dag.go`

```go
package dag

// Node represents a task in the dependency graph.
type Node struct {
    ID        string
    DependsOn []string
}

// Graph holds the dependency graph and analysis results.
type Graph struct {
    nodes    map[string]Node
    inDegree map[string]int
    adjList  map[string][]string // reverse edges: dep → dependents
}

// New creates a dependency graph from waiting tasks.
// completedIDs are dependencies considered satisfied.
// Returns a Graph ready for analysis.
func New(tasks []Node, completedIDs map[string]struct{}) *Graph

// Ready returns task IDs with all dependencies satisfied
// (all DependsOn entries are in completedIDs or not in the graph).
// Results are sorted lexicographically for determinism.
func (g *Graph) Ready() []string

// Blocked returns a map of task ID → list of unsatisfied dependency IDs.
// Only includes tasks that are not ready and not in a cycle.
func (g *Graph) Blocked() map[string][]string

// Cycles returns groups of task IDs involved in circular dependencies.
// Each group is a cycle (e.g., ["A", "B", "C"] for A→B→C→A).
// Uses Kahn's algorithm: nodes remaining after topological sort are cyclic.
func (g *Graph) Cycles() [][]string

// Ancestors returns all transitive dependency IDs for a given task,
// traversing the full dependency chain. Results sorted lexicographically.
func (g *Graph) Ancestors(id string) []string

// Analyze runs the full DAG analysis and returns a structured result.
func (g *Graph) Analyze() Analysis

// Analysis holds the complete DAG analysis results.
type Analysis struct {
    Ready   []string            // Task IDs ready for promotion
    Blocked map[string][]string // Task ID → unsatisfied deps
    Cycles  [][]string          // Groups of cyclic task IDs
}
```

### 2. Files to Create

| File | Purpose |
|------|---------|
| `internal/dag/dag.go` | Graph struct, `New()`, `Ready()`, `Blocked()`, `Cycles()`, `Ancestors()`, `Analyze()` |
| `internal/dag/dag_test.go` | Exhaustive topology tests (13+ test cases) |

### 3. Files to Modify

| File | Change |
|------|--------|
| `internal/queue/reconcile.go` | Replace `resolvePromotableTasks()` internals with `dag.New()` + `dag.Ready()`. Replace cycle detection with `dag.Cycles()`. Move cyclic tasks to `failed/`. Remove `dependsOnWaitingTask()` and `logCircularDependency()`. |
| `internal/queue/reconcile_test.go` | Update tests for new cycle-to-failed behavior, remove tests for removed functions |
| `internal/runner/task.go` | Update `writeDependencyContextFile()` to use `dag.Ancestors()` for transitive context. Update JSON format to separate direct/transitive. |
| `internal/status/status_gather.go` | Import `internal/dag`, categorize waiting tasks as Ready/Blocked/Cyclic |
| `docs/task-format.md` | Document cycle-to-failed behavior |
| `docs/architecture.md` | Document DAG-based dependency resolution |

### 4. Algorithm Pseudocode

```
function AnalyzeDAG(waitingTasks, completedIDs):
    // Build graph
    for each task in waitingTasks:
        for each dep in task.DependsOn:
            if dep not in completedIDs and dep in waitingTasks:
                add edge dep → task
                increment inDegree[task]

    // Kahn's algorithm
    queue = all nodes with inDegree == 0
    ready = []
    while queue not empty:
        node = dequeue()
        if all node.DependsOn in completedIDs or already processed:
            ready.append(node)
        for each dependent of node:
            decrement inDegree[dependent]
            if inDegree[dependent] == 0:
                enqueue(dependent)

    // Remaining nodes are cyclic
    cycles = extract SCCs from unprocessed nodes

    // Blocked = not ready, not cyclic, has unsatisfied deps
    blocked = {}
    for each unprocessed non-cyclic node:
        blocked[node] = list of unsatisfied deps

    return Analysis{ready, blocked, cycles}
```

### 5. Integration Points

#### In `ReconcileReadyQueue()` (`internal/queue/reconcile.go`):
```go
// Build DAG from waiting tasks
waitingTasks := idx.TasksByState(DirWaiting)
dagNodes := make([]dag.Node, 0, len(waitingTasks))
for _, snap := range waitingTasks {
    dagNodes = append(dagNodes, dag.Node{
        ID:        snap.Meta.ID,
        DependsOn: snap.Meta.DependsOn,
    })
}
g := dag.New(dagNodes, safeCompleted)
analysis := g.Analyze()

// Move cyclic tasks to failed/
for _, cycle := range analysis.Cycles {
    for _, taskID := range cycle {
        // find task file, append failure record, move to failed/
    }
}

// Promote ready tasks (filtered by active overlap)
for _, taskID := range analysis.Ready {
    // existing overlap check + AtomicMove to backlog/
}

// Log blocked tasks (optional, for debugging)
for taskID, deps := range analysis.Blocked {
    // stderr warning for unknown deps
}
```

#### In `writeDependencyContextFile()` (`internal/runner/task.go`):
```go
// Build DAG to find transitive ancestors
g := dag.New(allWaitingNodes, completedIDs)
ancestors := g.Ancestors(taskID)

// Write context with direct + transitive separation
context := DependencyContext{
    Direct:     directDepInfo,
    Transitive: transitiveDepInfo,
}
```

#### In status gathering (`internal/status/`):
```go
// Build DAG for waiting task categorization
g := dag.New(waitingNodes, completedIDs)
analysis := g.Analyze()

// Annotate waiting tasks in status output
for _, id := range analysis.Ready {
    // mark as "Ready for promotion"
}
for id, deps := range analysis.Blocked {
    // mark as "Blocked by: [deps]"
}
for _, cycle := range analysis.Cycles {
    // mark as "Circular dependency"
}
```

### 6. Test Plan

**Unit tests** (`internal/dag/dag_test.go`):

| Test | Topology | Assertion |
|------|----------|-----------|
| `TestNew_EmptyGraph` | No tasks | Ready=[], Blocked={}, Cycles=[] |
| `TestReady_NoDeps` | A (no deps) | Ready=[A] |
| `TestReady_SimpleChain` | A→B, A completed | Ready=[B] |
| `TestReady_DeepChain` | A→B→C, A completed | Ready=[B], Blocked={C: [B]} |
| `TestReady_FanIn` | A→C, B→C, both completed | Ready=[C] |
| `TestReady_FanOut` | A→B, A→C, A completed | Ready=[B, C] |
| `TestReady_Diamond` | A→C, B→C, A→B, A completed | Ready=[B], Blocked={C: [B]} |
| `TestCycles_SelfDep` | A→A | Cycles=[[A]] |
| `TestCycles_TwoNode` | A→B→A | Cycles=[[A, B]] |
| `TestCycles_ThreeNode` | A→B→C→A | Cycles=[[A, B, C]] |
| `TestCycles_Mixed` | A→B→A, C (no deps) | Cycles=[[A, B]], Ready=[C] |
| `TestBlocked_UnknownDep` | A→? | Blocked={A: [?]} |
| `TestAncestors_Chain` | A→B→C | Ancestors(C)=[A, B] |
| `TestAncestors_Diamond` | A→C, B→C | Ancestors(C)=[A, B] |
| `TestDeterministic` | Multiple runs | Same output each time |

**Integration tests** (`internal/integration/`):

| Test | Scenario |
|------|----------|
| `TestDAG_ChainPromotion` | 3-task chain: A, B→A, C→B. Verify promotion order matches completion order. |
| `TestDAG_CycleToFailed` | 2 tasks: A→B, B→A. Verify both moved to `failed/` with failure record. |
| `TestDAG_DiamondFanIn` | A→C, B→C. Verify C waits for both A and B. |

### 7. Migration / Compatibility Notes

- **No YAML format changes.** `depends_on` syntax is unchanged.
- **Behavioral change:** Cyclic tasks now move to `failed/` instead of sitting in `waiting/` forever with a stderr warning. This is intentional — cycles cannot self-resolve.
- **Cycle failures are not counted against `max_retries`.** The failure record uses a distinct format: `<!-- failure: circular dependency detected with [IDs] -->`.
- **Dependency context JSON format change:** Adds `direct` and `transitive` arrays. Since this is consumed by agent prompts (not mato code), the change is safe.
- **Removed functions:** `dependsOnWaitingTask()` and `logCircularDependency()` are replaced by DAG analysis.
- **Warning message wording:** May change slightly due to different analysis path. Tests matching on exact stderr strings need updating.

### 8. Effort Estimate

| Task | Effort |
|------|--------|
| `internal/dag/dag.go` — Graph struct + Kahn's algorithm | 3 hours |
| `internal/dag/dag.go` — `Ancestors()` + `Analyze()` | 1.5 hours |
| `internal/dag/dag_test.go` — 15+ test cases | 3 hours |
| Refactor `internal/queue/reconcile.go` to use DAG | 2 hours |
| Update `internal/runner/task.go` for transitive context | 1.5 hours |
| Update `internal/status/` for task categorization | 1.5 hours |
| Update existing tests | 1.5 hours |
| Integration tests | 2 hours |
| Documentation updates | 1 hour |
| **Total** | **~3-4 days** |

### 9. Open Questions (Deferred)

1. **`mato graph` command**: Export dependency graph as DOT format for Graphviz visualization. Natural extension of the DAG package.
2. **Priority-aware ordering**: Within the ready set, should tasks be ordered by priority? Currently handled by `DeferredOverlappingTasksDetailed()` — may need coordination.
3. **Cross-state dependency tracking**: Should dependencies on `in-progress` tasks be reported differently than dependencies on `waiting` tasks? Currently both block, but the distinction could be useful in status output.
4. **Cascade failure**: If task A fails, should all tasks transitively depending on A be automatically moved to `failed/`? Aggressive but potentially useful. Defer to v2.
5. **`depends_on` with completion conditions**: Currently, a dependency is satisfied when the task reaches `completed/`. Should there be a way to specify "depends on A being in ready-to-merge" for review coordination? Defer — adds complexity.

### 10. Step-by-Step Implementation Order

1. **Create `internal/dag/`**: Implement pure graph logic and exhaustive tests. No integration yet.
2. **Refactor `internal/queue/reconcile.go`**: Switch to DAG-based promotion and cycle detection. Update existing reconcile tests.
3. **Update `internal/runner/task.go`**: Enable transitive dependency context with direct/transitive separation.
4. **Update `internal/status/`**: Categorize waiting tasks as Ready/Blocked/Cyclic in status output.
5. **Integration tests**: End-to-end verification of chain promotion, cycle-to-failed, and diamond fan-in.
6. **Documentation**: Update task-format.md and architecture.md.
