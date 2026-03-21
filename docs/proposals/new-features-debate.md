# New Features Debate: Multi-Agent Discussion

**Date:** 2026-03-21
**Participants:**
- 🟣 **Claude Opus 4.6** — "Correctness First" advocate
- 🟢 **GPT-5.4** — "Correct, Operable, Scalable" advocate
- 🔵 **Gemini 3.1 Pro** — "Velocity & Visibility" advocate

---

## Summary of Positions

| Rank | 🟣 Claude Opus 4.6 | 🟢 GPT-5.4 | 🔵 Gemini 3.1 Pro |
|------|---------------------|-------------|---------------------|
| 1 | DAG Dependency Resolution | DAG Scheduler w/ Cycle Detection | Interactive TUI Dashboard |
| 2 | Task Cancellation & Abort | Task Cancellation & Force-Abort | Dynamic Task Environments |
| 3 | Glob Affects Matching | Parallel Review Workers | Smart Glob Conflict Detection |
| 4 | Structured Audit Trail | Structured Audit Log & Run History | Parallel Review Pipeline |
| 5 | Parallel Review Pipeline | Per-Task Resource Profiles | Task Scaffolding & Templates |

### Consensus Features (appeared in 2+ proposals)

| Feature | Advocates | Combined Priority |
|---------|-----------|-------------------|
| **DAG Dependency Resolution** | Opus (#1), GPT (#1) | 🔴 Critical |
| **Task Cancellation** | Opus (#2), GPT (#2) | 🔴 Critical |
| **Parallel Review Pipeline** | Opus (#5), GPT (#3), Gemini (#4) | 🟠 High |
| **Glob Affects Matching** | Opus (#3), Gemini (#3) | 🟡 Medium |
| **Structured Audit Trail** | Opus (#4), GPT (#4) | 🟡 Medium |

### Unique Proposals

| Feature | Advocate | Priority |
|---------|----------|----------|
| **Interactive TUI Dashboard** | Gemini (#1) | 🟡 Medium |
| **Dynamic Task Environments** | Gemini (#2) | 🟡 Medium |
| **Per-Task Resource Profiles** | GPT (#5) | 🟡 Medium |
| **Task Scaffolding & Templates** | Gemini (#5) | ⚪ Low |

---

## Detailed Positions

---

## 🟣 Claude Opus 4.6 — "Correctness First"

### Philosophy

> A task orchestrator that produces wrong results quickly is worse than useless. One that produces correct results slowly will frustrate teams but at least won't ship broken code. One that is correct, fast, *and* operable is what we should be building.

Opus prioritizes **correctness → throughput → operability**, arguing that foundational scheduling correctness must come before any UX improvements.

### Feature 1: Full DAG Dependency Resolution with Transitive Cycle Detection

**Problem:** The current dependency system is dangerously shallow. `resolvePromotableTasks()` checks direct dependencies only, completely missing transitive cycles (A → B → C → A), diamond dependency failures (no cascade-fail), and lacks topological ordering for promotion.

**Proposed Design:**
- New package `internal/dag/` with clean graph abstraction
- Tarjan's algorithm for cycle detection — O(V+E), single-pass
- Topological sort for valid promotion ordering
- Cascade-fail: mark unreachable tasks when a dependency fails
- New CLI: `mato status --deps` shows the dependency graph
- New CLI: `mato doctor` reports cycles, missing deps, orphan references

**Impact:** Prevents silent deadlocks. Without this, users see "0 tasks in backlog" with no explanation. This is the single most dangerous gap in mato's correctness guarantees.

**Effort:** Medium (3–5 days)

### Feature 2: Task Cancellation and Graceful Abort

**Problem:** Once a task enters `in-progress/`, there is no mechanism to stop it. Runaway agents, bad prompts, and obsolete tasks all require an emergency brake. Without cancellation, operators lose control of the system.

**Proposed Design:**
- Cancel markers in `.tasks/.control/cancel-<task-id>` (atomic signal files)
- Runner checks for cancel markers before claim, during review, and in the main loop
- Graceful shutdown: SIGTERM → grace period → SIGKILL
- New CLI: `mato cancel <task-name>` with `--force` flag
- Cancelled tasks get `<!-- cancelled: reason -->` metadata
- Support cancelling by agent ID: `mato cancel --agent <id>`

**Impact:** Basic operational safety. Autonomous systems without a kill switch are liabilities.

**Effort:** Medium (3–5 days)

### Feature 3: Glob Pattern Matching for Affects

**Problem:** Exact string matching in `affects:` is brittle at scale. An agent that creates `internal/foo/bar_test.go` must predict that exact path. Missed conflicts lead to merge failures and wasted work.

**Proposed Design:**
- Integrate `doublestar` or `filepath.Match` into `internal/queue/overlap.go`
- Replace set membership check with glob matching loop
- Support patterns like `internal/queue/**/*.go`, `docs/*.md`
- Maintain backward compatibility: exact strings still work
- Add pattern validation at task load time

**Impact:** Reduces accidental concurrency issues. Makes task definitions more robust and easier to write. Directly prevents wasted compute from merge conflicts.

**Effort:** Small (1–2 days)

### Feature 4: Structured Audit Trail (JSONL Event Log)

**Problem:** Mato generates useful state transitions but has no durable record. Debugging failures, proving operational history, and understanding "what happened" requires manually inspecting scattered files.

**Proposed Design:**
- New package `internal/audit/` with append-only JSONL writer
- Storage: `.tasks/audit/events.jsonl` with daily rotation
- Event types: `task_claimed`, `task_completed`, `review_passed`, `review_rejected`, `merge_completed`, `task_failed`, `task_cancelled`
- Each event includes: timestamp (UTC), agent_id, task_id, from_state, to_state, reason
- New CLI: `mato history [task-id]`, `mato events --since 1h`
- Fits mato's filesystem philosophy — no database required

**Impact:** Foundation for all observability. Enables future dashboards, analytics, and compliance without adding infrastructure. JSONL is greppable, appendable, and simple.

**Effort:** Medium (3–5 days)

### Feature 5: Parallel Review Pipeline

**Problem:** Review is serialized: one review per poll loop iteration. With multiple agents producing work in parallel, the review stage becomes the bottleneck. Tasks pile up in `ready-for-review/` waiting for their turn.

**Proposed Design:**
- Bounded worker pool for review goroutines
- New config: `MATO_REVIEW_CONCURRENCY` (default: 1, preserving current behavior)
- Retain per-task review lock semantics via `AcquireReviewLock`
- Keep merge serialized (one-at-a-time discipline maintained)
- Prioritize oldest or highest-priority reviews first

**Impact:** Directly increases end-to-end throughput. The existing lock infrastructure already supports concurrent reviews — this is the cleanest concurrency win available.

**Effort:** Medium (3–5 days)

### Rebuttals

> "A TUI dashboard should be top priority for visibility."

A dashboard without correct scheduling, audit logs, and cancellation is cosmetic. First make mato reliable and observable at the data level; then the UI becomes easy and honest.

> "Templates are quick wins with high visibility."

Templates help users *create* tasks; they do not help mato *execute* them safely. Authoring convenience is secondary to runtime correctness.

> "DAG resolution is over-engineering for simple task lists."

Until someone creates a transitive cycle and the queue silently deadlocks. The current code has a known blind spot — `dependsOnWaitingTask()` only catches pairwise cycles. This is not theoretical; it's a bug waiting to happen.

---

## 🟢 GPT-5.4 — "Correct, Operable, Scalable"

### Philosophy

> If Mato wants to graduate from a clever orchestrator to a dependable one, it should prioritize: **correct scheduling, operator control, throughput, auditability, and execution policy.**

GPT-5.4 shares Opus's correctness-first stance but adds emphasis on **execution policy** (per-task resource limits) as a production necessity.

### Feature 1: DAG Scheduler with Cycle Detection

**Problem:** Without full DAG resolution and cycle detection, users can create deadlocks, hidden dependency chains, and confusing "why is this task stuck?" situations. This is the biggest correctness gap.

**Proposed Design:**
- Extend `internal/queue/reconcile.go` with DAG builder from task metadata
- Cycle detection via DFS / Kahn's algorithm
- Blocked-reason computation per task
- New CLI: `mato status --graph`, `mato doctor` for diagnostics
- Generated `.tasks/.queue/dependency-graph.json`
- Tasks only promote when all upstream nodes are completed
- Cycles surfaced explicitly, never silent stalls

**Impact:** Foundational. If dependency semantics are incomplete, higher-level features sit on shaky ground. Improves correctness, predictability, and user trust more than any UI or template feature.

**Effort:** Medium (3–5 days)

### Feature 2: Task Cancellation and Force-Abort

**Problem:** Once a task is in flight, operators have no clean way to stop it. Bad prompts, wrong branches, runaway agents, obsolete tasks — all need an emergency brake.

**Proposed Design:**
- Cancellation markers under `.tasks/control/`
- Runner checks before claim, before review, and during task loop
- On cancel: SIGTERM container → grace period → SIGKILL if needed
- Move to `failed/` or `backlog/` depending on stage and flags
- New CLI: `mato cancel <task-id>`, `mato abort <task-id> --force`, `mato cancel --agent <agent-id>`
- Status shows "cancelling", "aborted", "cancel requested by …"

**Impact:** Basic operability. Autonomous systems without a kill switch become liabilities. More important than templates, web UI, or nicer conflict matching.

**Effort:** Medium (3–5 days)

### Feature 3: Parallel Review Workers

**Problem:** The whole pipeline backs up behind review even when execution is parallel. This is the clearest throughput bottleneck.

**Proposed Design:**
- Replace single review selection with bounded worker pool
- New config: `MATO_REVIEW_CONCURRENCY`
- Retain per-task review lock semantics
- Keep merge serialized
- Prioritize oldest-ready or highest-priority review first

**Impact:** Highest-ROI performance feature. More agents are pointless if review remains single-file.

**Effort:** Medium (3–5 days)

### Feature 4: Structured Audit Log and Run History

**Problem:** Operators need durable answers to: *what happened, when, and why?* Without a real audit trail, debugging failures and proving operational history are harder than they should be.

**Proposed Design:**
- New `internal/audit/` package
- Storage: `.tasks/audit/events/*.jsonl` with optional daily rollups
- Event types: task_claimed, task_started, task_progress, task_cancel_requested, review_started, review_passed, review_failed, merge_started, merged, task_failed
- New CLI: `mato history`, `mato history <task-id>`, `mato events --since 1h`
- Append-only JSONL writer, reusing existing message/completion data

**Impact:** Backbone for observability, debugging, compliance, incident review, and future analytics. Unlocks a future API/UI without first building a database.

**Effort:** Medium (3–5 days)

### Feature 5: Per-Task Resource Profiles

**Problem:** A global timeout is too blunt. Some tasks need more time, some need stricter limits, some need different environments. Without per-task resource policy, mato wastes capacity and makes failures harder to reason about.

**Proposed Design:**
- New frontmatter fields: `timeout`, `cpu_limit`, `memory_limit`, `docker_image`, `review_timeout`
- Parse into task metadata, pass through Docker flags in `buildDockerArgs()`
- Validate against repo-level max caps
- Surface effective limits in status and audit log

**Impact:** The right way to scale heterogeneous work safely. Improves fairness, reduces runaway jobs, enables complex tasks without weakening defaults. More valuable than templates because it changes execution quality, not just authoring convenience.

**Effort:** Medium (3–5 days)

### Rebuttals

> "Web UI should be top 5 because users need visibility."

A dashboard without stronger scheduling, audit logs, and cancellation is lipstick on a control-plane gap. Build the data plane first; then the dashboard becomes easy and honest.

> "Templates are faster to ship and more visible."

Yes — and less important. Templates help users create tasks; they do not help mato execute them safely or at scale.

> "Affects globbing is a simpler win."

True, but it solves a narrower paper cut. DAG correctness and cancellation solve system-level failure modes.

> "Resource profiles are overkill for an MVP."

Not for autonomous agents in Docker. Per-task limits are basic production hygiene.

---

## 🔵 Gemini 3.1 Pro — "Velocity & Visibility"

### Philosophy

> We shouldn't just build a better queue; we should build a better *developer experience*. Correctness is a baseline; velocity is the feature.

Gemini challenges the "correctness first" camp, arguing that operational UX and runtime flexibility deliver more practical value than theoretical graph algorithms.

### Feature 1: Interactive TUI Dashboard (`mato monitor`)

**Problem:** `mato status` is a static snapshot of a dynamic system. To understand what's happening *now*, operators frantically `cat` files in `.tasks/` or tail logs. No real-time visibility is a critical operational blind spot.

**Proposed Design:**
- Bubble Tea framework (`github.com/charmbracelet/bubbletea`) for rich TUI
- New command: `mato monitor`
- Left pane: live list of in-progress and ready-for-review tasks with spinners
- Right pane: real-time stream of selected task's messages
- Footer: global metrics (tasks/min, active agents)
- Uses `fsnotify` for instant `.tasks/` directory watching

**Impact:** Transforms mato from a "black box" into a transparent system. Increases operator confidence and drastically reduces time-to-recovery during incidents.

**Effort:** Medium (3–5 days)

### Feature 2: Dynamic Task Environments (Per-Task Docker Images)

**Problem:** Locking all agents into a single Docker image is a severe limitation for polyglot repositories. You cannot orchestrate Node.js frontend tasks, Go backend tasks, and Python data scripts without a bloated "kitchen sink" image.

**Proposed Design:**
- Add `image: string` field to `TaskMeta` frontmatter
- Runner reads `task.Meta.Image` and passes to Docker run command instead of default
- Example: `image: node:20-alpine` in task frontmatter
- Pre-pull images before execution to avoid timeouts

**Impact:** Unlocks true polyglot orchestration. Agents use the best tool for the job without infrastructure headaches.

**Effort:** Small (2 days)

### Feature 3: Intelligent Smart Glob Conflict Detection

**Problem:** `affects` requires exact string matches. This is brittle. If an agent creates a new file at an unpredicted path, mato might schedule a conflicting task, leading to merge conflicts or data loss.

**Proposed Design:**
- Integrate `github.com/bmatcuk/doublestar` into `internal/queue/overlap.go`
- Replace `map[string]struct{}` exact match with glob matching loop
- Support patterns like `internal/queue/**/*.go`
- Pre-compile globs or use trie structure for performance at scale

**Impact:** Significantly reduces accidental concurrency issues. Makes task definitions more robust and easier to write.

**Effort:** Small (1–2 days)

### Feature 4: Parallel Review Pipeline

**Problem:** The `pollLoop` processes reviews sequentially. Multiple agents generate code in parallel, but they all funnel into a single-threaded review gate. A slow review blocks the entire merge train.

**Proposed Design:**
- Dedicated pool of reviewer goroutines managed by semaphore
- Main loop dispatches `ready-for-review` tasks to pool up to `MaxReviewConcurrency`
- Careful concurrency management for file locking

**Impact:** Removes the single biggest structural bottleneck in the system.

**Effort:** Medium (3–5 days)

### Feature 5: Task Scaffolding & Templates (`mato new`)

**Problem:** Creating task files manually — copy-pasting frontmatter, remembering field names, ensuring YAML validity — is high-friction. This discourages developers from breaking work into smaller, atomic tasks, leading to "mega-tasks" harder to review and merge.

**Proposed Design:**
- New directory: `.tasks/templates/` with Go template syntax
- New command: `mato new <template-name> [flags]`
- Example: `mato new bugfix --title="Fix NPE" --affects="pkg/api"`
- Generates properly formatted task file in `waiting/` or `backlog/`

**Impact:** Improves developer velocity and enforces consistency across task definitions.

**Effort:** Small (1–2 days)

### Rebuttals

> "We need full DAG dependency resolution first!"

Most user workflows are linear sequences or simple fan-outs. The current "wait until dependency is complete" logic covers 90% of real-world cases. Fixing bottlenecks yields more than fixing theoretical graph problems.

> "A TUI is just eye candy; we need audit logs."

Logs are for post-mortems; a TUI is for *operations*. When a system is stuck, you want a red status bar, not grep through 50MB of JSON. We can have both, but immediate observability enables faster iteration.

> "Templates can be done with shell scripts."

They *can*, but they *won't*. A first-class `mato new` command is discoverable and self-documenting. Shell scripts are hidden and brittle.

---

## Cross-Debate Analysis

### Where All Three Agree

1. **Parallel Review Pipeline** — All three agents identified review serialization as a key bottleneck. This has the strongest consensus for implementation.

2. **The system needs better observability** — Whether through audit logs (Opus, GPT) or a TUI dashboard (Gemini), all agents agree mato is too opaque during operation.

### Where Two Agree (Strong Signal)

3. **DAG Dependency Resolution** — Both Opus and GPT rank this #1, calling it the most dangerous correctness gap. Gemini disagrees, arguing it's over-engineering for most workflows.

4. **Task Cancellation** — Both Opus and GPT rank this #2, calling it essential operational safety. Gemini omits it entirely, prioritizing velocity features instead.

5. **Glob Affects Matching** — Both Opus and Gemini include this as a practical, low-effort win.

### Key Disagreements

| Topic | Correctness Camp (Opus, GPT) | Velocity Camp (Gemini) |
|-------|------------------------------|------------------------|
| **Top priority** | Fix dependency scheduling bugs | Improve operator experience (TUI) |
| **DAG resolution** | Critical — silent deadlocks are catastrophic | Over-engineering — 90% of workflows are linear |
| **Task cancellation** | Essential operational safety | Not prioritized |
| **TUI dashboard** | Premature without data plane | Most impactful for daily use |
| **Templates** | Authoring sugar, not priority | Reduces friction, improves task quality |

### Recommended Implementation Order

Based on the debate, a pragmatic ordering that balances correctness and velocity:

| Phase | Feature | Effort | Rationale |
|-------|---------|--------|-----------|
| **Phase 1** | Glob Affects Matching | Small (1-2 days) | Quick win, broad consensus, prevents real failures |
| **Phase 1** | Task Cancellation | Medium (3-5 days) | Operational safety — ship the kill switch early |
| **Phase 2** | DAG Dependency Resolution | Medium (3-5 days) | Correctness foundation, prevents silent deadlocks |
| **Phase 2** | Parallel Review Pipeline | Medium (3-5 days) | Throughput bottleneck removal, unanimous agreement |
| **Phase 3** | Structured Audit Trail | Medium (3-5 days) | Observability backbone, enables future features |
| **Phase 3** | Per-Task Docker Images | Small (2 days) | Polyglot support, low risk |
| **Phase 4** | Interactive TUI Dashboard | Medium (3-5 days) | Best built on top of audit trail data |
| **Phase 4** | Per-Task Resource Profiles | Medium (3-5 days) | Production hardening |
| **Phase 5** | Task Templates (`mato new`) | Small (1-2 days) | UX polish, low priority |

**Total estimated effort:** ~6-8 weeks for all features across phases.

---

## Conclusion

The debate reveals a healthy tension between **correctness** (making mato trustworthy) and **velocity** (making mato pleasant to use). The strongest consensus is around:

1. **Parallel Review Pipeline** — the clearest bottleneck, agreed upon by all three agents
2. **Task Cancellation** — essential for operational safety (2 of 3 agents)
3. **DAG Dependencies** — the most dangerous correctness gap (2 of 3 agents)
4. **Glob Matching** — low-effort, high-value quick win (2 of 3 agents)
5. **Structured Audit Trail** — observability foundation (2 of 3 agents)

The recommended approach is to ship quick wins (glob matching, cancellation) first, then address foundational issues (DAG, parallel review), and finally layer on UX improvements (audit trail, TUI, templates).
