# Debate Position: Top 5 Features for Mato

**Author:** Claude Opus 4.6 Agent
**Date:** July 2025

---

## Opening Statement

After a thorough analysis of mato's architecture — its filesystem-backed state machine, its Docker-isolated agent execution, its squash-merge queue — I've identified the five features that would deliver the highest *compound* impact. My selections prioritize **correctness first, throughput second, operability third**. A task orchestrator that produces wrong results quickly is worse than useless. One that produces correct results slowly will frustrate teams but at least won't ship broken code. One that is correct, fast, *and* operable is what we should be building.

---

## Feature 1: Full DAG Dependency Resolution with Transitive Cycle Detection

### Problem It Solves

The current dependency system in `internal/queue/reconcile.go` is **dangerously shallow**. `resolvePromotableTasks()` checks whether each task's direct dependencies appear in `completed/`, and `dependsOnWaitingTask()` catches *pairwise* circular dependencies between two waiting tasks. But it completely misses:

- **Transitive cycles**: A → B → C → A will not be detected. Task A waits for C, B waits for A, C waits for B. All three sit in `waiting/` forever with no warning. The user sees "0 tasks in backlog" and has no idea why.
- **Diamond dependencies**: A depends on B and C; B and C both depend on D. Today this works by accident (D completes, B and C promote, A promotes). But if D fails, there is no mechanism to cascade-fail A, B, and C. They sit in `waiting/` indefinitely.
- **Dependency ordering**: When multiple tasks become promotable simultaneously, there's no topological sort. Tasks may be promoted in arbitrary order, and a task could be claimed before its transitive dependency chain is fully resolved.

### Proposed Design

Create a new package `internal/dag/` with a clean graph abstraction:

```go
// internal/dag/dag.go
package dag

type Graph struct {
    nodes map[string]*Node
    edges map[string][]string // task ID → dependency IDs
}

type Node struct {
    ID       string
    State    string // "waiting", "backlog", "completed", "failed"
    Priority int
}

// Build constructs a dependency graph from all tasks across all queue directories.
func Build(tasksDir string) (*Graph, error)

// DetectCycles returns all strongly-connected components with len > 1.
// Uses Tarjan's algorithm — O(V+E), single-pass.
func (g *Graph) DetectCycles() [][]string

// TopologicalOrder returns a valid promotion order for tasks whose
// dependencies are all satisfied, respecting priority as a tiebreaker.
func (g *Graph) TopologicalOrder(satisfied func(id string) bool) []string

// TransitiveFailures returns all task IDs that transitively depend on
// a failed task and can never be promoted.
func (g *Graph) TransitiveFailures() []string

// Visualize writes a DOT-format graph to w for debugging.
func (g *Graph) Visualize(w io.Writer) error
```

Integration points:
- `ReconcileReadyQueue()` in `internal/queue/reconcile.go` calls `dag.Build()` on each poll cycle, runs `DetectCycles()` (emitting warnings for cycles), then uses `TopologicalOrder()` to determine promotion order.
- `TransitiveFailures()` auto-moves tasks with failed ancestors to `failed/` with a descriptive failure record: `<!-- failure: orchestrator at <time> step=DEPENDENCY error=transitive dependency <id> failed -->`.
- New CLI command `mato deps [--dot]` renders the dependency graph. Without `--dot`, prints a human-readable tree. With `--dot`, outputs Graphviz DOT format that can be piped to `dot -Tpng`.

### Impact

This is the **highest-impact correctness fix** in the entire backlog. Silent deadlocks from transitive cycles are catastrophic in an autonomous system — no human is watching the queue. Without this, mato cannot reliably handle task graphs with more than ~5 interdependent tasks. Every team scaling beyond trivial workloads will hit this wall.

Additionally, the cascade-failure propagation prevents ghost tasks from accumulating in `waiting/` and confusing the status dashboard. The `mato deps` command gives operators the visibility they need to debug complex task graphs.

### Effort Estimate

**Medium (3-5 days)**. Tarjan's algorithm is well-understood (~100 lines of Go). The graph construction is straightforward — `frontmatter.Parse` already extracts `DependsOn`. The main work is integrating cleanly with `ReconcileReadyQueue()` without breaking the existing atomic-move guarantees, and building the CLI command with DOT output.

---

## Feature 2: Task Cancellation and Force-Abort Mechanism

### Problem It Solves

Today, if an agent goes rogue — spinning on a bad prompt, stuck in an infinite loop, producing garbage commits — there is **no way to stop it** short of SSHing into the host and manually `docker kill`-ing the container. There is no `mato cancel` command. There is no abort signal. The task will run until its 30-minute timeout expires, wasting resources and potentially blocking downstream tasks via the affects-overlap system.

This is not a theoretical concern. In multi-agent systems, approximately 10-20% of agent runs produce no useful output (the agent misunderstands the task, hallucinates requirements, or gets stuck in a loop). Each wasted run costs 30 minutes of wall-clock time and blocks the affected files from other agents.

### Proposed Design

Implement a two-tier cancellation system: **graceful cancel** and **force abort**.

**Signal file protocol** (no new dependencies, works with filesystem-backed architecture):

```
.tasks/signals/
├── cancel-<filename>       # Graceful cancel request
└── abort-<filename>        # Force abort (immediate SIGKILL)
```

New CLI commands:

```bash
mato cancel <task-name>     # Write cancel signal file, SIGTERM container
mato cancel --force <task>  # Write abort signal file, SIGKILL container
mato cancel --all           # Cancel all in-progress tasks
```

Implementation in a new package:

```go
// internal/signal/signal.go
package signal

// RequestCancel creates a cancel signal file for the named task.
func RequestCancel(tasksDir, taskFilename string) error

// RequestAbort creates an abort signal file for the named task.
func RequestAbort(tasksDir, taskFilename string) error

// IsCancelled checks if a cancel or abort signal exists for the task.
func IsCancelled(tasksDir, taskFilename string) bool

// IsAborted checks specifically for a force-abort signal.
func IsAborted(tasksDir, taskFilename string) bool

// CleanSignals removes all signal files for a task (called after transition).
func CleanSignals(tasksDir, taskFilename string) error
```

Integration with the poll loop in `runner.go`:
1. Before each poll iteration, check for signal files matching in-progress tasks.
2. On cancel signal: send `SIGTERM` to the Docker container (graceful shutdown — the 10s WaitDelay already exists). Move task to `backlog/` with `<!-- failure: operator at <time> step=CANCEL error=cancelled by operator -->`.
3. On abort signal: send `SIGKILL` immediately. Move task to `failed/` (no retry — operator explicitly aborted).
4. Clean signal files after state transition.

Additionally, the poll loop should check for cancellation signals *during* `exec.CommandContext` wait, using a goroutine that polls the signals directory every 5 seconds.

**Status integration:** The `mato status` dashboard should show a "CANCELLING" indicator for tasks with pending cancel signals.

### Impact

This is the **most critical operational gap** in mato. Every production orchestrator needs an emergency brake. Without cancellation, operators are helpless when things go wrong, and the only recourse is manual container management — which breaks mato's internal state tracking and can lead to orphaned locks, stuck tasks, and corrupted queue state.

The signal-file approach is elegant because it requires no new IPC mechanism — it uses the same filesystem primitives that power the rest of mato. It's also safe across process restarts: if the orchestrator restarts, it picks up pending signals on the next poll.

### Effort Estimate

**Medium (3-5 days)**. The signal file mechanism is simple. The Docker container interaction is already handled via `exec.CommandContext` with `cancel()`. The main complexity is ensuring clean state transitions under all failure modes (container already exited, signal races with normal completion, etc.) and adding the CLI command.

---

## Feature 3: Glob Pattern Matching for Affects Declarations

### Problem It Solves

The `affects` field currently requires exact file paths: `affects: [pkg/client/http.go, pkg/client/retry.go]`. This is brittle and error-prone for several reasons:

1. **Task authors can't predict all files an agent will touch.** An agent asked to "refactor the HTTP client" might touch `http.go`, `http_test.go`, `retry.go`, `transport.go`, and `README.md`. Listing every file defeats the purpose of autonomous agents.
2. **No directory-level protection.** You can't say "this task owns `pkg/client/`" — you must enumerate every file.
3. **New files aren't covered.** If an agent creates `pkg/client/pool.go`, it won't conflict with any existing affects declaration. Two agents could independently create the same new file.
4. **The overlap check in `overlappingAffects()` uses exact `==` comparison**, making it impossible to express "these two tasks might conflict because they both work in the same package."

### Proposed Design

Extend `overlappingAffects()` in `internal/queue/overlap.go` to support glob patterns. For single-level wildcards (`*`, `?`), use Go's standard `filepath.Match`. For recursive doublestar patterns (`**`), implement a simple recursive matcher or adopt a small library like `doublestar` (zero transitive dependencies):

```go
// internal/queue/overlap.go

// overlappingAffects returns patterns that overlap between two affects lists.
// Supports glob patterns: "pkg/client/*.go", "internal/**/config.go"
func overlappingAffects(a, b []string) []string {
    var overlaps []string
    seen := make(map[string]struct{})
    for _, pa := range a {
        for _, pb := range b {
            key := pa + ":" + pb
            if _, ok := seen[key]; ok {
                continue
            }
            seen[key] = struct{}{}
            if matchAffects(pa, pb) {
                overlaps = append(overlaps, fmt.Sprintf("%s ↔ %s", pa, pb))
            }
        }
    }
    return overlaps
}

// matchAffects returns true if two affects patterns overlap.
// Both sides can be globs. "a/b.go" matches "a/*.go". "a/*.go" matches "a/**".
func matchAffects(a, b string) bool {
    if a == b {
        return true
    }
    // Check if either is a glob and matches the other
    if matchGlob(a, b) || matchGlob(b, a) {
        return true
    }
    return false
}
```

For `**` (doublestar) support, note that Go's `filepath.Match` does **not** handle recursive patterns — a custom recursive matcher or the `doublestar` library (which has no transitive dependencies) is needed.

The file-claims index (`messaging.WriteFileClaimsIndex`) should also use glob matching when comparing claimed files against affects patterns.

**Backward compatibility:** Exact paths continue to work unchanged. A glob pattern is detected by the presence of `*` or `?` characters. No migration needed.

### Impact

This feature has a **multiplicative effect on correctness** as the number of concurrent agents increases. With 3-4 agents, exact-match conflicts are manageable. With 8-10 agents working across a large codebase, the probability of two agents touching overlapping files without matching affects declarations grows rapidly.

Glob support also dramatically **reduces the cognitive burden on task authors**. Instead of guessing which files an agent will modify, they can declare `affects: [pkg/client/**]` and let the conflict detection system handle the rest.

### Effort Estimate

**Small (1-2 days)**. The matching logic is straightforward. `filepath.Match` handles single-star globs natively. The main work is updating `overlappingAffects()`, the file-claims comparison, adding test cases for glob-vs-glob and glob-vs-exact scenarios, and updating `docs/task-format.md`.

---

## Feature 4: Structured Audit Trail with Event Sourcing

### Problem It Solves

Mato's current event system (`internal/messaging/`) is **ephemeral by design**: events in `.tasks/messages/events/` are garbage-collected after 24 hours by `CleanOldMessages()`. Completion details persist in `.tasks/messages/completions/`, but they only capture the final state — not the journey.

When something goes wrong in a multi-agent run (and it *will*), operators need to answer questions like:
- "Why did task X fail 3 times before succeeding?"
- "Which agent was running when the merge conflict occurred?"
- "How long did each task spend in each state?"
- "What was the throughput over the last 24 hours?"

Today, answering these questions requires reading HTML comments embedded in task files, cross-referencing timestamp strings, and manually reconstructing the timeline. This is unacceptable for any system running in production.

### Proposed Design

Create `internal/audit/` with an append-only, structured event log:

```go
// internal/audit/audit.go
package audit

type Event struct {
    Timestamp  time.Time         `json:"timestamp"`
    Type       string            `json:"type"`        // "state_change", "claim", "review", "merge", "failure", "cancel"
    TaskID     string            `json:"task_id"`
    TaskFile   string            `json:"task_file"`
    AgentID    string            `json:"agent_id,omitempty"`
    FromState  string            `json:"from_state,omitempty"`
    ToState    string            `json:"to_state,omitempty"`
    Duration   time.Duration     `json:"duration_ns,omitempty"`
    Detail     map[string]string `json:"detail,omitempty"`
}

type Log struct {
    path string // .tasks/audit/events.jsonl
    mu   sync.Mutex
}

// Append writes an event to the audit log (JSONL format, append-only).
func (l *Log) Append(e Event) error

// Query returns events matching the filter, ordered by timestamp.
func (l *Log) Query(filter Filter) ([]Event, error)

// Stats computes aggregate statistics from the log.
func (l *Log) Stats() (*RunStats, error)

type RunStats struct {
    TotalTasks        int
    Completed         int
    Failed            int
    AvgDuration       time.Duration
    AvgRetries        float64
    ReviewApproveRate float64
    ThroughputPerHour float64
}
```

**Storage format:** JSONL (one JSON object per line) — the simplest possible append-only format. No external dependencies. Atomic appends via `O_APPEND` (already used for failure records). One file per day: `.tasks/audit/2025-07-14.jsonl`.

**Integration points** — emit events at every state transition:
- `queue.AtomicMove()` → `state_change` event
- `queue.SelectAndClaimTask()` → `claim` event
- `runner.postReviewAction()` → `review` event with verdict
- `merge.ProcessQueue()` → `merge` event with commit SHA
- `signal.RequestCancel()` → `cancel` event

**CLI command:**

```bash
mato log                      # Show recent events (last 50)
mato log --task <id>          # Filter by task ID
mato log --since 2h           # Events from last 2 hours
mato log --type failure       # Only failure events
mato log --stats              # Aggregate statistics
mato log --json               # Raw JSONL output for piping
```

### Impact

This is the **observability foundation** that every other monitoring feature builds on. Web dashboards, alerting, metrics — they all need a reliable event source. By implementing this as a simple JSONL file with no external dependencies, we stay true to mato's filesystem-first philosophy while enabling powerful downstream tooling.

The `--stats` flag alone justifies the feature: knowing your review approval rate, average task duration, and throughput-per-hour lets you tune agent prompts, adjust timeouts, and identify systemic problems.

### Effort Estimate

**Medium (3-5 days)**. The append-only log is trivial. The query/filter logic is straightforward (read lines, unmarshal, filter). The main work is instrumenting all state transitions across `queue/`, `runner/`, and `merge/` packages, and building the CLI command with its various flags.

---

## Feature 5: Parallel Review Pipeline

### Problem It Solves

The current review system processes **one task per poll cycle**. In `runner.go`'s `pollLoop`, after running a task agent, the code calls `selectAndLockReview()` which picks a single candidate from `ready-for-review/`, runs the review, processes the verdict, and returns. If 5 tasks are waiting for review, it takes 5 poll cycles — potentially 2.5+ hours with 30-minute review timeouts — to clear the review backlog.

This creates a critical bottleneck as agent count scales. With 4 concurrent task agents, tasks accumulate in `ready-for-review/` faster than the single-threaded review pipeline can process them. The merge queue starves. Throughput plateaus.

The review lock mechanism (`AcquireReviewLock` in `queue/locks.go`) already supports per-task granularity — it was designed for concurrency but is only used sequentially.

### Proposed Design

Convert the review pipeline from sequential to parallel with a configurable concurrency limit:

```go
// internal/runner/review.go

// reviewWorkerPool runs up to maxReviewers concurrent review agents.
func reviewWorkerPool(ctx context.Context, cfg *envConfig, maxReviewers int) {
    sem := make(chan struct{}, maxReviewers)
    candidates := reviewCandidates(cfg.tasksDir)

    var wg sync.WaitGroup
    for _, candidate := range candidates {
        if ctx.Err() != nil {
            break
        }
        // Try to acquire review lock (non-blocking)
        unlock, err := queue.AcquireReviewLock(cfg.tasksDir, candidate.Name, cfg.agentID)
        if err != nil {
            continue // another reviewer has it
        }

        sem <- struct{}{} // acquire semaphore slot
        wg.Add(1)
        go func(task reviewTask, unlock func()) {
            defer wg.Done()
            defer func() { <-sem }()
            defer unlock()

            runReviewAndProcess(ctx, cfg, task)
        }(candidate, unlock)
    }
    wg.Wait()
}
```

**Configuration via environment variable:**
```bash
MATO_MAX_REVIEWERS=3  # Default: 1 (backward compatible)
```

**Key design decisions:**
- Each review agent gets its own Docker container, temporary clone, and agent ID (suffixed with `-review-N`).
- Review agents share no mutable state — each reads the task branch independently.
- The per-task review lock (`AcquireReviewLock`) prevents duplicate reviews of the same task.
- The merge queue remains single-threaded and sequentially ordered — parallelism is only in the review phase.
- On context cancellation (SIGTERM), all review containers receive `SIGTERM` concurrently via `exec.CommandContext`.

**Status integration:** `mato status` already shows "Ready for Review" tasks. Add a "Reviews in Progress" section showing active review agents and their targets.

### Impact

This is the **highest-leverage throughput improvement** available. In steady state with N task agents, the review pipeline must process N tasks per cycle to avoid becoming the bottleneck. With `MATO_MAX_REVIEWERS=N`, review throughput scales linearly with agent count.

The beauty of this design is that it requires minimal architectural change — the review lock mechanism was already built for concurrency. We're simply using it as intended.

### Effort Estimate

**Medium (3-5 days)**. The semaphore-based worker pool is standard Go concurrency. The main challenges are: (1) managing multiple Docker containers safely under cancellation, (2) ensuring review lock cleanup on all exit paths, and (3) preventing resource exhaustion on the host when running N task agents + M review agents simultaneously. Integration tests in `internal/integration/` will need new scenarios for concurrent reviews.

---

## Priority Ranking Summary

| Rank | Feature | Type | Effort | Why This Order |
|------|---------|------|--------|----------------|
| 1 | DAG Dependency Resolution | Correctness | Medium | Silent deadlocks are unacceptable in autonomous systems |
| 2 | Task Cancellation | Operability | Medium | No emergency brake = no production readiness |
| 3 | Glob Affects Matching | Correctness | Small | Prevents undetected file conflicts at scale |
| 4 | Structured Audit Trail | Observability | Medium | Foundation for all future monitoring/debugging |
| 5 | Parallel Review Pipeline | Throughput | Medium | Unlocks linear scaling beyond 2-3 agents |

Total estimated effort: **3-4 weeks** for all five features.

---

## Rebuttal Section

### "You're ignoring the web dashboard — that's what users actually want."

A web dashboard without a reliable data source is a pretty but empty shell. Feature 4 (Structured Audit Trail) is the *prerequisite* for any dashboard. Build the event log first, then the dashboard writes itself — it's just a read-only view over JSONL files. Shipping a dashboard before the audit trail means either (a) building a throwaway data layer that gets replaced, or (b) scraping HTML comments from task files in real-time, which is fragile and slow.

### "Task templates and parameterization would help more users."

Templates are a convenience feature; they reduce boilerplate for task creation. But task creation is a one-time, manual step that takes 30 seconds. The features I've proposed address problems that occur *continuously during execution* — every poll cycle, every state transition, every conflict check. Optimizing the inner loop beats optimizing the setup step every time.

### "The DAG feature is over-engineered — the current system handles dependencies fine."

It handles dependencies fine *for small, linear chains*. The moment you have a diamond dependency or a 3-node cycle, the system fails silently. "Works for simple cases" is not an engineering standard — it's a time bomb. Tarjan's algorithm is 100 lines of Go. The implementation cost is trivial compared to the debugging cost of a single silent deadlock in production.

### "Glob matching is dangerous — it could cause false-positive conflicts."

Yes, `affects: [**]` would conflict with everything. But that's the *correct* behavior — a task that modifies arbitrary files *should* conflict with everything. Glob matching doesn't create false positives; it creates *true positives that exact matching misses*. Task authors who want narrow scope can still use exact paths. Globs are opt-in, not mandatory.

### "Parallel reviews will create resource contention on the host."

The `MATO_MAX_REVIEWERS` cap exists precisely for this reason. The default is 1 — existing behavior, zero risk. Operators who want parallelism explicitly opt in and choose a concurrency level appropriate for their hardware. Docker's own resource management (cgroups, memory limits) provides an additional safety net. If anything, the bigger risk is *not* parallelizing reviews and having the review queue become an unbounded backlog.

### "Why not just add metrics/Prometheus instead of a custom audit trail?"

Because mato runs on developer machines and CI runners, not Kubernetes clusters. Requiring a Prometheus server, Grafana instance, and metrics endpoint would violate mato's core design principle: **everything is a file**. JSONL is the Prometheus of the filesystem. Any team that *does* run Prometheus can trivially write a 20-line exporter that reads the JSONL log. The audit trail is the universal foundation; Prometheus is an optional consumer.

---

## Closing Argument

These five features share a common thread: they make mato **trustworthy at scale**. Today, mato works well for 1-3 agents on small projects. But the moment you push past that — more agents, deeper dependency graphs, larger codebases — you hit correctness gaps (silent deadlocks, undetected conflicts), operability gaps (no kill switch, no audit trail), and throughput ceilings (sequential reviews).

My proposals address all three failure modes with minimal architectural disruption. Every feature builds on mato's existing filesystem-first philosophy. No new databases. No new network protocols. No new dependencies (except possibly one for doublestar globs). Just files, directories, and well-structured Go code.

The alternative — building a dashboard or template system first — is putting lipstick on a system that can silently deadlock. Fix the foundation, then build the facade.
