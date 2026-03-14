# Task Prioritization and Ready-Queue Mechanics for `mato`

## Current Behavior

Today `mato` treats `.tasks/backlog/` as a flat pool:

- `run()` calls `recoverOrphanedTasks()`, `cleanStaleLocks()`, and then `hasAvailableTasks()`.
- `hasAvailableTasks()` only checks whether any `.md` file exists in `backlog/`.
- The embedded `task-instructions.md` tells agents to list `backlog/` and try `mv` on any file they find.
- There is no dependency model, no priority model, and no deterministic pick order.

That is simple, but it means agents can pick work in the wrong order, start dependent tasks too early, and repeatedly grab low-value work while higher-value work waits.

---

## Goals

A prioritization design for `mato` should provide:

1. **Readiness enforcement** — tasks with unmet dependencies should not be worked accidentally.
2. **Deterministic ordering** — agents should converge on the same “best next task” order.
3. **Crash safety** — host or agent crashes should self-heal on the next loop.
4. **Multi-agent friendliness** — more than one container can claim work concurrently without central locking beyond the filesystem.
5. **Operational simplicity** — the queue should still be inspectable with ordinary files.
6. **Scalability** — should stay reasonable for both 5 tasks and 500 tasks.

---

## Common Metadata Model

All four strategies become much cleaner if task files support frontmatter. Recommended shape:

```yaml
---
id: api-pagination
priority: 90
depends_on:
  - db-indexes
  - auth-cleanup
---
# Add API pagination
...
```

### Recommended rules

- `id`: stable identifier, used by dependencies. Prefer this over raw filenames.
- `priority`: numeric, where **higher number means more important**. Recommended default: `50`.
  - Accept `high`/`medium`/`low` as a convenience if desired, but normalize internally to numbers.
- `depends_on`: list of task IDs.
- Missing frontmatter means: `priority=50`, `depends_on=[]`.

### Why numeric priority is better than only `high/medium/low`

Three bands are easy to read, but too coarse once there are many ready tasks. Numeric priority gives room for “urgent but not stop-the-world” without inventing many words.

### Tie-breaking rule

Priority should not encode strict sequencing by itself. If task B must follow task A, use `depends_on`.

For remaining ties, use a deterministic sort key:

1. `priority` descending
2. filename ascending

Filename is a good final tie-breaker because it is stable, visible, and requires no extra persisted state.

---

## Strategy 1: Host-Side Ready Queue

**Idea:** `mato` computes readiness on the host. Only ready tasks live in `.tasks/backlog/`. Tasks with unmet dependencies live in `.tasks/waiting/` (or `blocked/`). When dependencies complete, the host moves them into `backlog/`.

### Implementation complexity (`main.go`)

**Medium-High.** This is the first strategy that truly changes the queue model.

Likely host changes:

- Add a new queue directory such as `.tasks/waiting/`.
- Replace `hasAvailableTasks()` with something more like `reconcileReadyQueue()`:
  - scan `waiting/`, `backlog/`, `in-progress/`, `completed/`, `failed/`
  - parse frontmatter from task files
  - build a set of completed task IDs
  - decide which tasks are ready
  - move ready tasks into `backlog/`
  - move newly blocked tasks out of `backlog/` into `waiting/`
  - return the number of ready backlog tasks
- Add helpers such as:
  - `parseTaskMetadata(path string)`
  - `taskIDForFile(path string)`
  - `dependenciesMet(task, completedSet)`
  - `effectiveReadyTasks(tasksDir)`
- Update tests around `hasAvailableTasks()` and `recoverOrphanedTasks()`.

Important design point: **do not keep the ready queue only in memory**. The directories should remain the source of truth, and reconciliation should be idempotent.

### Prompt complexity (`task-instructions.md`)

**Low.** Agents can stay dumb:

- list `.tasks/backlog/`
- claim a file with `mv`
- do not inspect `waiting/`

If host-side ordering is also wanted, instructions can stay simple if the host provides an ordered manifest (for example `.tasks/backlog/.queue-order`). Without such a manifest, agents can only use filename order.

### Reliability

**High for readiness, medium for ordering.**

Strengths:

- Agents cannot accidentally start blocked tasks if blocked tasks never appear in `backlog/`.
- `os.Rename` within the same `.tasks/` mount is atomic, so moving between `waiting/`, `backlog/`, and `in-progress/` is crash-safe at the file level.
- If the host crashes mid-reconcile, the next `mato` loop can rescan directories and fix the queue.

Weaknesses:

- Readiness is enforced, but **priority is not**, unless the host also provides an ordering artifact.
- If ordering depends only on filename sort, humans must encode order in filenames or accept weak priority behavior.

### Scalability

**Good.** One host loop scan over 500 task files is cheap in Go. This scales better than making every agent reread every task.

### How agents know what to pick

Options under this strategy:

1. **Filename sorting only** — simple, but pushes scheduling policy into naming conventions.
2. **Host-written queue manifest** — better; agents read ordered filenames from a generated file.
3. **Single `next` file** — not recommended for multi-agent operation because it implies one global cursor and becomes awkward when several agents claim work concurrently.

### Bottom line

Strong readiness enforcement, weak ordering unless paired with another mechanism.

---

## Strategy 2: Agent-Side Dependency Checking

**Idea:** All tasks remain visible. Agents read metadata, verify dependencies themselves, and only claim a task whose dependencies are complete.

### Implementation complexity (`main.go`)

**Low.** The host can stay close to current behavior.

Possible host changes are limited to:

- optionally adding metadata helpers for future validation
- maybe exposing `completed/` more clearly in prompt wording
- leaving `hasAvailableTasks()` almost unchanged

### Prompt complexity (`task-instructions.md`)

**High.** The prompt must now teach the agent to be its own scheduler:

- list all backlog tasks
- read frontmatter from each candidate
- resolve `depends_on`
- inspect `.tasks/completed/` to decide whether deps are met
- skip blocked tasks
- sort ready tasks
- try claims in that sorted order

That is much more cognitive load than the current prompt, and it puts a lot of correctness pressure on the model.

### Reliability

**Low-Medium.** This is the least enforceable option.

Failure modes:

- an agent ignores or misunderstands dependency metadata
- an agent forgets to sort by priority
- two agents do redundant metadata scans over the same large backlog
- prompt drift causes inconsistent behavior across model versions

The claim step is still protected by atomic `mv`, so only one agent will win a given file, but the **choice quality** is advisory, not guaranteed.

### Scalability

**Weakest of the four strategies.** With 500 tasks and multiple agents, every agent repeatedly scans and parses the whole backlog. That is unnecessary duplicated work.

### How agents know what to pick

They must read frontmatter and decide for themselves. Filename sorting becomes only a fallback.

### Bottom line

This is cheap to implement in Go, but expensive in prompt complexity and least reliable. It is fine for a prototype, not ideal for core scheduling.

---

## Strategy 3: Priority Field in Task Files

**Idea:** Task files contain `priority`, and agents sort candidates by that priority.

### Implementation complexity (`main.go`)

**Low-Medium.** If priority is purely agent-driven, `main.go` barely changes. If the host validates or surfaces priority, some parsing helpers are needed.

Likely host changes:

- none for a minimal version, beyond maybe renaming `hasAvailableTasks()` later
- optional metadata parser if you want host-side observability or linting

### Prompt complexity (`task-instructions.md`)

**Medium.** Agents must:

- read frontmatter from every backlog file
- normalize priority values
- sort candidates before attempting `mv`

This is easier than full dependency evaluation, but still more complex than today.

### Reliability

**Medium.** Better than pure agent-side dependency checking because the logic is simpler, but still advisory.

Main risk: if an agent ignores priority, the host cannot stop it.

This strategy also does **nothing** for readiness. A high-priority task with unmet dependencies can still be chosen too early unless dependencies are handled separately.

### Scalability

**Okay for small backlogs, weaker at large scale.** Reading priorities from 5 tasks is trivial. Reading them from 500 tasks in every agent is wasteful but still probably tolerable. The bigger concern is inconsistency, not raw CPU.

### How agents know what to pick

Read frontmatter, sort by priority descending, then filename ascending.

### Bottom line

Useful as a building block, but incomplete by itself. Priority without readiness is not enough.

---

## Strategy 4: Hybrid — Host Manages Readiness, Agent Sorts by Priority

**Idea:** The host enforces dependency readiness, and agents only choose among already-ready tasks by priority.

This combines the strengths of strategies 1 and 3 while avoiding the biggest weakness of strategy 2.

### Implementation complexity (`main.go`)

**Medium.** More than priority-only, less risky than full host-side ordering with a complex broker.

Recommended host changes:

- Add `.tasks/waiting/`.
- Introduce `reconcileReadyQueue(tasksDir)` and call it from `run()`.
- Parse metadata for `id`, `priority`, and `depends_on`.
- Keep only ready tasks in `backlog/`.
- Leave priority ordering to the agent prompt.
- Optionally write a debug artifact such as `.tasks/queue-state.json` or `.tasks/backlog/.queue-order` so humans can inspect why a task is waiting.

This replaces the current mental model of “backlog means everything” with “backlog means ready now.” That is a meaningful but clean improvement.

### Prompt complexity (`task-instructions.md`)

**Medium-Low.** Agents no longer need to reason about dependency graphs. They only need to:

- list `.tasks/backlog/`
- read priority from ready tasks
- sort by priority descending, filename ascending
- attempt claims in that order

This is still simple enough for the prompt to be explicit and testable.

### Reliability

**High overall.**

- Readiness is enforced by the host, so agents cannot accidentally start blocked work.
- Priority is advisory, but only within the much smaller ready set.
- If an agent ignores priority, the system degrades gracefully rather than breaking dependency correctness.
- If the host crashes mid-move, the next reconcile pass can rebuild readiness from directory state and metadata.

If stronger enforcement is wanted later, the host can start writing an ordered queue manifest without changing the readiness model.

### Scalability

**Best practical tradeoff.**

- Host performs one O(n) scan.
- Agents only inspect ready tasks, not the whole universe.
- Works fine at 5 tasks and remains reasonable at 500 tasks.

### How agents know what to pick

Recommended rule:

1. List ready `.md` files in `.tasks/backlog/`
2. Read `priority` from each
3. Sort by priority descending, then filename ascending
4. Attempt `mv` in that order until one succeeds

A single `next` file is still not ideal in a parallel system. Multiple agents need an ordered candidate set, not a global mutable cursor.

### Bottom line

This is the best fit for `mato` right now.

---

## Comparative Summary

| Strategy | `main.go` complexity | Prompt complexity | Reliability | Scalability | Best for |
|---|---|---:|---:|---:|---|
| Host-side ready queue | Medium-High | Low | High readiness, medium ordering | Good | Strong dependency enforcement |
| Agent-side dependency checking | Low | High | Low-Medium | Weakest | Fast prototype only |
| Priority field only | Low-Medium | Medium | Medium | Medium | Lightweight ranking only |
| Hybrid | Medium | Medium-Low | High | Good | Recommended default |

---

## Recommended Ready-Queue Mechanics

### Default policy

Use **host-managed readiness** plus **agent-managed priority ordering**.

### Directory model

Recommended queue layout:

```text
.tasks/
├── waiting/        # not ready yet: unmet deps or retry cooldown
├── backlog/        # ready to claim now
├── in-progress/    # claimed by an active agent
├── completed/      # finished successfully
├── failed/         # exceeded retry budget or manually abandoned
└── .locks/
```

`waiting/` is preferable to `blocked/` because it can also hold tasks temporarily delayed by retry backoff, not only hard dependency blocks.

### Definition of “ready”

A task is ready when:

- it is not already in `in-progress/`, `completed/`, or `failed/`
- every `depends_on` ID exists in `completed/`
- it is not in a retry cooldown window

### Proposed host loop behavior in `run()`

Yes — the host loop should actively manage the ready queue **between container launches**.

Recommended loop shape:

1. `recoverOrphanedTasks(tasksDir)`
2. `cleanStaleLocks(tasksDir)`
3. `readyCount := reconcileReadyQueue(tasksDir)`
4. if `readyCount == 0`, wait/poll
5. else `runOnce(...)`
6. immediately run `reconcileReadyQueue(tasksDir)` again after agent exit so newly-unblocked dependents move into `backlog/` before the next launch

That is better than treating `hasAvailableTasks()` as a passive existence check. Queue reconciliation becomes an explicit scheduler step.

### What should replace `hasAvailableTasks()`

`hasAvailableTasks()` is too weak for the new model. Replace it with one of:

- `reconcileReadyQueue(tasksDir) (int, error)`
- or `readyTaskCount(tasksDir) (int, error)` after a separate reconcile step

The key is that availability should be based on **ready** tasks, not merely files present.

---

## Requeue Behavior for Failed High-Priority Tasks

A high-priority task that fails should **keep its base priority**, but it should not monopolize the entire system.

Recommended behavior:

1. Append the failure record, as the current prompt already does.
2. Move the task to `waiting/`, not directly to `backlog/`.
3. Apply retry backoff based on failure count, for example:
   - 1st retry: immediately or after 1 minute
   - 2nd retry: 5 minutes
   - 3rd retry: 15 minutes
4. When backoff expires and dependencies are still met, the host promotes it back to `backlog/`.
5. After max retries, move it to `failed/` and leave dependents in `waiting/`.

This preserves business priority without letting one broken urgent task starve everything else.

### Why not lower the stored priority?

Priority should describe importance, not temporary scheduler frustration. Backoff is a better knob than mutating the task’s semantic importance.

---

## Handling Priority Ties

Use a deterministic rule:

1. higher `priority` first
2. filename ascending

If humans need stronger manual ordering among same-priority tasks, they should declare dependencies or use sortable filenames. Do not rely on filesystem directory iteration order.

---

## Interaction with `recoverOrphanedTasks()`

This is an important change.

### Current behavior

`recoverOrphanedTasks()` appends a failure record and moves orphaned tasks from `in-progress/` back to `backlog/`.

### Proposed behavior under host-managed readiness

Recovered orphaned tasks should move to `waiting/`, not directly to `backlog/`.

Why:

- the task may no longer be ready once the queue is re-evaluated
- it may need retry cooldown
- keeping recovery separate from readiness makes the system more self-healing

Recommended flow:

1. detect orphaned task in `in-progress/`
2. append recovery failure record
3. move to `waiting/`
4. let `reconcileReadyQueue()` decide whether it returns to `backlog/`

This keeps orphan recovery and scheduling cleanly separated.

### Active-agent behavior stays the same

The current logic that skips tasks claimed by still-active agents should remain. That is orthogonal and still correct.

---

## Should the Host Also Write an Ordered Manifest?

Optional, but useful.

A file like `.tasks/backlog/.queue-order` or `.tasks/queue-state.json` would help with:

- debugging why a task is waiting
- explaining the current ready order
- reducing ambiguity in prompts

However, it should be treated as **derived state**, not the source of truth. The source of truth should remain:

- task file metadata
- queue directories
- completion state

A single `.next` pointer is not recommended because `mato` is multi-agent. A ranked list is better than a single cursor.

---

## Recommendation

Adopt **Strategy 4: Hybrid**.

### Why this is the best fit

- It fixes the real correctness problem: agents starting tasks whose dependencies are not done.
- It keeps the prompt understandable: agents only rank ready work, they do not compute dependency graphs.
- It scales better than agent-side scanning.
- It fits the existing filesystem-oriented architecture and the current `recoverOrphanedTasks()` / `cleanStaleLocks()` loop.
- It leaves room to add a host-written queue manifest later if priority enforcement needs to become stronger.

### Suggested rollout

#### Phase 1

- add frontmatter support (`id`, `priority`, `depends_on`)
- add `.tasks/waiting/`
- replace `hasAvailableTasks()` with host reconciliation
- update prompt so agents sort ready backlog tasks by priority

#### Phase 2

- add queue-state/manifest output for observability
- add retry cooldown metadata if starvation becomes visible
- optionally add validation for missing dependency IDs or cycles

This gives `mato` a real scheduler without abandoning its simple file-based model.
