# Merge Strategies for Concurrent mato Agents

## Current Baseline

Today, mato pushes merge responsibility into the agent prompt.

- `task-instructions.md` Step 7 tells each agent to:
  1. `git fetch origin`
  2. merge `origin/<target>` into its task branch
  3. resolve conflicts if needed
  4. switch to the target branch
  5. merge the task branch into the target branch
  6. `git push origin <target>`
- `main.go` simply embeds that prompt and launches independent agents from isolated temporary clones via `runOnce(...)`.
- The host does **not** coordinate merges. That means the final push to the shared target branch is a race.

The core problem is not just merge conflicts. It is a **push serialization problem**:

- Agent A and agent B can both successfully merge `origin/mato` into their own task branches.
- Both can build a locally valid merge result.
- Only one can push the shared `mato` branch first.
- The loser gets a non-fast-forward error and must retry after incorporating the new tip.

With 5+ concurrent agents, this becomes a thundering herd problem: the system repeatedly asks LLM agents to perform deterministic git reconciliation work that would be safer in host code.

---

## Comparison Summary

| Strategy | Conflict Likelihood | Resolution Complexity | History Cleanliness | Agent Prompt Complexity | Host Code Complexity | Reliability at 5+ Agents | Removes Push Race? |
|---|---|---|---|---|---|---|---|
| 1. Agent-side merge with retry | Medium-High | Medium-High | Noisy; merge commits pile up | High | Low | Medium-Low | No |
| 2. Rebase before push | Medium-High | High | Cleaner, often linear | Very High | Low | Medium-Low | No |
| 3. Squash merge | Medium | Medium | Cleanest per-task history | Medium-High | Low | Medium | No |
| 4. Host-managed serial merge queue | Medium | Medium, centralized | Configurable; usually clean | Low | Medium-High | High | Yes |
| 5. GitHub-style merge queue | Low-Medium operationally; conflicts still exist but are serialized | Medium, mostly host-managed | Very clean and auditable | Low | Very High | Very High | Yes |

Important nuance: **rebase** and **squash** can improve history shape, but by themselves they do not solve the shared-branch race. The only strategies here that actually eliminate the race are the ones that **centralize the final merge/push step**.

---

## 1. Current: Agent-Side Merge with Retry

### How it works

Each agent owns the entire integration workflow:

1. create `task/<name>` from `mato`
2. make changes and commit
3. fetch and merge `origin/mato` into the task branch
4. resolve conflicts
5. merge the task branch into local `mato`
6. push `mato`
7. retry Step 7 up to 3 times if push fails

### Conflict likelihood and resolution complexity

- **Conflict likelihood:** Medium-High.
  - Textual conflicts happen whenever two agents touch the same files or nearby lines.
  - Even without textual conflicts, non-fast-forward push failures are common under concurrency.
- **Resolution complexity:** Medium-High.
  - The agent must understand both the task diff and the concurrent diff from another agent.
  - The retry loop is branch-stateful: after a failed push, the agent has already merged locally, so repeating the flow cleanly is easy to get wrong.
  - Conflict resolution quality depends on the LLM interpreting both sides correctly every time.

### History cleanliness

- **Weakest history of the set.**
- You get:
  - task commits on `task/<name>`
  - merge commits from `origin/mato` into the task branch
  - a merge of `task/<name>` back into `mato`
  - possibly extra merge noise from retries
- The resulting branch history is accurate, but noisy and hard to read.

### Agent prompt complexity

- **High.**
- The existing Step 7 is already one of the most brittle parts of `task-instructions.md`.
- Agents must understand:
  - fetch vs merge behavior
  - conflict markers
  - commit sequencing
  - retry semantics
  - when to stop and mark failure
- This is deterministic orchestration logic, which is a poor fit for prompt-only control.

### Host code complexity (`main.go`)

- **Low.**
- `main.go` barely changes because the host delegates everything to the prompt.
- This is the main advantage of the current design.

### Reliability under high concurrency (5+ agents)

- **Medium-Low.**
- The system technically works, but efficiency degrades badly:
  - one success invalidates every other in-flight local view of `mato`
  - multiple agents can re-run the same integration work repeatedly
  - later agents can burn retries without changing their task diff at all
- The architecture scales poorly because every conflict and every push race is paid for in LLM time.

### What happens when a merge fails?

- If the agent can resolve conflicts, it commits and continues.
- If it cannot, the task follows the existing failure path:
  - append a failure record
  - move the task back to `backlog/`
  - retry later, up to the max retry count
- This can create repeated work if the underlying issue is branch contention rather than a real semantic conflict.

### Impact on `task-instructions.md`

- **No structural change required** because this is the current behavior.
- But the prompt remains long, brittle, and responsible for correctness in the hardest part of the workflow.

### Bottom line

This is the simplest implementation, but it pushes the riskiest coordination work into autonomous agents. It is acceptable for a prototype and weak concurrency, but not a strong long-term architecture.

---

## 2. Rebase Instead of Merge

### How it works

Instead of merging `origin/mato` into `task/<name>`, the agent rebases the task branch onto the latest target branch before integrating:

1. fetch `origin/mato`
2. `git rebase origin/mato`
3. resolve rebase conflicts if they occur
4. switch to target branch and fast-forward/merge the rebased task
5. push

### Conflict likelihood and resolution complexity

- **Conflict likelihood:** still Medium-High.
  - Rebase does **not** reduce overlap between concurrent changes.
  - If two tasks edit the same lines, one of them still has to reconcile that overlap.
- **Resolution complexity:** High.
  - Rebase conflicts are usually harder for agents than merge conflicts.
  - The agent may need `git rebase --continue`, `--skip`, or `--abort` correctly.
  - If the task branch has multiple commits, conflicts can recur commit-by-commit.
  - If another agent pushes while the rebase-based agent is preparing to push, the rebased branch may need to be rebased again.

### History cleanliness

- **Cleaner than merge-based agent integration.**
- Benefits:
  - linear task branch history
  - fewer merge commits
  - easier `git log` reading
- Drawback:
  - rewritten commit SHAs make debugging and retry instructions more subtle
  - if the agent later falls back to merge behavior on the target branch, linearity can still be lost

### Agent prompt complexity

- **Very High.**
- Compared with merge-based retry, the prompt must teach the agent:
  - how to rebase safely
  - how to detect rebase state
  - how to continue or abort
  - how to recover if the rebased branch is invalidated by another push
- This is more precise and less forgiving than the current merge flow.

### Host code complexity (`main.go`)

- **Low.**
- Like the current model, this is mostly a prompt change.
- Host code still does not coordinate branch integration.

### Reliability under high concurrency (5+ agents)

- **Medium-Low.**
- The final push race still exists.
- You may get prettier history, but you still have repeated invalidation of local state as multiple agents race to update `mato`.
- In practice, rebasing under heavy concurrency often feels worse than merging because each retry rewrites history again.

### What happens when a merge/rebase fails?

- The agent must either:
  - resolve conflicts and continue the rebase, or
  - abort the rebase and fail the task
- Failure handling is more delicate than merge handling because the repository is left in a rebase-in-progress state until cleaned up.

### Impact on `task-instructions.md`

- **Large Step 7 rewrite.**
- The prompt must replace merge instructions with detailed rebase instructions, including recovery commands.
- This makes the most complex section of the prompt even more operationally fragile.

### Bottom line

Rebase improves history, but not concurrency control. It is attractive for human-driven branches; it is much less attractive for autonomous agents that must recover from interrupted rebases reliably.

---

## 3. Squash Merge

### How it works

Each task is integrated as a single commit on the target branch instead of preserving the task branch's full commit graph.

Typical agent-side flow:

1. agent does normal work on `task/<name>` and commits as needed
2. fetch latest target branch
3. bring task branch up to date somehow (merge or rebase)
4. switch to target branch
5. `git merge --squash task/<name>`
6. create one commit representing the task result
7. push target branch

### Conflict likelihood and resolution complexity

- **Conflict likelihood:** Medium.
  - Squashing does **not** materially reduce textual conflicts; the same net diff still has to apply.
  - It can slightly simplify reasoning because the final target branch only receives one logical change per task.
- **Resolution complexity:** Medium.
  - Simpler than multi-commit rebases.
  - Still non-trivial if the squash application conflicts.
  - If push fails after the squash commit is created locally, the agent may need to reset the target branch and recreate the squash commit on the new head.

### History cleanliness

- **Very clean.**
- Advantages:
  - one commit per completed task
  - easier auditing of task-level changes
  - less merge noise on `mato`
- Tradeoff:
  - original per-task commit history is not preserved on the target branch
  - debugging the evolution inside a task branch is harder unless the branch refs are retained

### Agent prompt complexity

- **Medium-High.**
- Simpler than rebase, but still more complex than it first appears because the prompt must cover:
  - how to update task branch before squashing
  - how to recreate a squash commit after a non-fast-forward push
  - how to resolve squash conflicts safely
- If agents are free to make multiple commits during task work, the squash step is extra logic rather than a simplification of their workflow.

### Host code complexity (`main.go`)

- **Low.**
- If kept agent-side, host changes are minimal.
- The host still does not own serialization.

### Reliability under high concurrency (5+ agents)

- **Medium.**
- Better history does not mean better concurrency.
- You still have the same shared push race on the target branch.
- The retry story is a little cleaner conceptually than agent-side merge commits, but still wasteful under load.

### What happens when a merge fails?

- If `git merge --squash` conflicts, the agent resolves conflicts or aborts.
- If the final push fails, the agent usually has to:
  - update local target branch to the new remote tip
  - reapply the squash merge
  - recommit
  - push again
- If this fails repeatedly, the task returns to backlog or failed state.

### Impact on `task-instructions.md`

- **Moderate Step 7 rewrite.**
- The prompt becomes more explicit about producing a single final commit on the target branch.
- Still leaves significant git choreography in the prompt.

### Bottom line

Squash merge is attractive for history quality, but by itself it is not a concurrency solution. It is best thought of as a **history policy**, not a race-condition fix.

---

## 4. Host-Managed Serial Merge Queue

### How it works

Agents stop short of modifying the shared target branch.

Proposed flow:

1. agent creates `task/<name>` from `mato`
2. agent makes changes and commits
3. agent pushes **only** `task/<name>` (or another task result ref)
4. agent reports success and exits
5. the mato host process merges queued task branches into `mato` **one at a time**

The key shift is architectural: **the host owns final integration**.

### Conflict likelihood and resolution complexity

- **Conflict likelihood:** Medium.
  - Real code conflicts do not disappear.
  - But only the queue head needs to reconcile against the current target tip.
- **Resolution complexity:** Medium, but centralized.
  - The host can perform deterministic git steps itself.
  - If conflict resolution still needs LLM help, it can launch a dedicated conflict-resolution agent only for the queued branch at the front.
  - Everyone else waits; they do not repeatedly race and invalidate each other.

### History cleanliness

- **Configurable.**
- The queue can use any integration style:
  - regular merge commit
  - fast-forward where possible
  - squash merge
- This is a major advantage: queueing and history policy are separable concerns.

### Agent prompt complexity

- **Low.**
- This is the strongest benefit.
- Agents no longer need to:
  - merge target into task branch
  - resolve integration conflicts during normal task completion
  - retry pushes to the shared target branch
- The prompt becomes focused on task execution, committing, and publishing a task branch/ref.

### Host code complexity (`main.go`)

- **Medium-High.**
- `main.go` would need real orchestration changes, likely including:
  - a queue of completed task branches waiting to merge
  - a merge lock so only one host worker updates `mato` at a time
  - status tracking for queued / merged / blocked tasks
  - possibly a new task state such as `ready-to-merge/` or metadata for branch refs
  - retry / backoff policy in host code, not prompt text
- This is more code, but it is deterministic code in Go rather than fragile prompt logic.

### Reliability under high concurrency (5+ agents)

- **High.**
- This is the first strategy that actually fixes the race.
- Many agents can still work in parallel.
- Only one entity pushes `mato`, so non-fast-forward failures disappear from normal operation.
- Throughput becomes bounded by merge serialization, which is the correct bottleneck because the branch itself is inherently serialized.

### What happens when a merge fails?

- The failed branch stays queued or moves to a blocked state.
- The host can:
  - mark it as needing manual review
  - launch a conflict-resolution subflow
  - skip it temporarily and continue with later items, or stop the queue depending on policy
- Crucially, the task branch still exists. The work is preserved even if integration fails.

### Impact on `task-instructions.md`

- **Large simplification.**
- Step 7 would change from “merge into target and push target” to something like:
  - push `task/<name>` to origin
  - record the branch/ref for the host merge queue
  - do not mark the task complete until host confirms merge, or move it to a queue-managed intermediate state
- Step 8 likely changes too:
  - agents should not move tasks directly to `completed/` if the target branch has not yet incorporated the work
  - the host should own the final `completed/` transition after a successful merge

### Bottom line

This is the best fit for mato's architecture if the goal is reliable concurrency. It moves serialization to the host, where it belongs, and dramatically simplifies the agent prompt.

---

## 5. GitHub-Style Merge Queue

### How it works

This extends Strategy 4 into a fuller queue with explicit branch refs or PR-like objects and optional CI gating between each merge.

Typical model:

1. agent pushes a task branch
2. host creates or updates a PR / queue item
3. queue tests the branch against the latest target tip
4. when the item reaches the front, host merges it
5. CI re-runs or validates post-merge assumptions before the next item proceeds

This is conceptually similar to GitHub Merge Queue / bors / Homu.

### Conflict likelihood and resolution complexity

- **Conflict likelihood:** Low-Medium operationally.
  - Conflicts still happen, but only for the queue head.
  - CI catches integration breakage that isn't a textual merge conflict.
- **Resolution complexity:** Medium.
  - The queue system itself does the hard serialization work.
  - However, setting up and maintaining that queue is much more complex than a local serial merger.

### History cleanliness

- **Excellent.**
- Usually provides:
  - linear or near-linear history
  - explicit queue ordering
  - high auditability of what merged, when, and why
- Best option if you care about PR-style review and CI evidence.

### Agent prompt complexity

- **Low.**
- Agents can stop after pushing a branch and possibly attaching metadata.
- The queue absorbs the integration workflow.

### Host code complexity (`main.go`)

- **Very High.**
- Compared with a local host-managed queue, this adds:
  - PR or synthetic queue-item creation
  - remote branch management
  - CI status polling
  - retry logic tied to CI results
  - failure reporting and queue reordering
  - GitHub auth / `gh` workflow assumptions
- It is a real subsystem, not just a small refactor of the current loop.

### Reliability under high concurrency (5+ agents)

- **Very High** if the surrounding infrastructure exists.
- This is the most proven operational model for shared-branch concurrency.
- But mato does not currently have PR-native orchestration. Adopting this would be a significant product-direction change.

### What happens when a merge fails?

- If merge conflicts occur, the queue item is rejected or marked blocked.
- If CI fails, the item is removed from the queue or paused pending fixes.
- The task branch/PR remains intact, so the work is preserved and diagnosable.

### Impact on `task-instructions.md`

- **Large simplification for agents, large architectural change overall.**
- The prompt would likely become:
  - do the work
  - commit
  - push a task branch
  - optionally annotate status for the host
  - do not merge to target directly
- The complexity moves almost entirely out of the prompt and into host/remote queue management.

### Bottom line

This is the most robust and enterprise-style option, but it is probably too heavy for mato's current local-clone, prompt-driven architecture unless CI-gated queueing becomes a core product goal.

---

## Strategy-by-Strategy Impact on the Existing Prompt

| Strategy | Prompt Impact |
|---|---|
| Current merge with retry | Keep current Step 7. Minimal change, highest ongoing fragility. |
| Rebase | Replace Step 7 with detailed rebase/continue/abort instructions. More brittle than current. |
| Squash | Replace Step 7 with update + squash + recommit instructions. Cleaner history, still prompt-heavy. |
| Host-managed serial queue | Remove most of Step 7. Agents push task branches only. Host becomes source of truth for final merge. |
| GitHub-style merge queue | Same simplification for agents as host-managed queue, but with much larger external workflow assumptions. |

The strongest pattern here is simple: **every strategy that keeps final integration in the prompt leaves mato vulnerable to branch races and prompt brittleness**.

---

## Recommendation for mato

### Recommended architecture: Host-managed serial merge queue

For mato as it exists today, the best architecture is:

1. **Adopt Strategy 4 as the concurrency model**: agents push task branches only; the host merges them serially.
2. **Use squash merge as the default history policy inside that queue** for a single commit per task, unless preserving the branch's internal commit history becomes important.
3. **Treat GitHub-style merge queueing as a future evolution**, not the first step.

### Why this is the best fit

#### 1. It solves the actual bug

The race is caused by multiple agents pushing the same target branch. A host-managed queue removes that race completely. Rebase and squash alone do not.

#### 2. It moves deterministic logic out of the prompt

`task-instructions.md` is currently carrying too much operational responsibility. Merge serialization, retry loops, and branch-state recovery belong in Go code, not LLM instructions.

#### 3. It matches mato's architecture

`main.go` already owns the lifecycle of agent execution:

- create clone
- launch agent in Docker
- manage `.tasks/`
- recover orphaned work

Owning the final merge is a natural extension of that role.

#### 4. It scales better with parallelism

Agents can still do useful work concurrently. The queue only serializes the one resource that is inherently serialized anyway: the target branch tip.

#### 5. It gives cleaner failure handling

When merge fails, the work remains on a durable task branch. That is much safer than forcing the agent to keep retrying local integration until it gives up.

### Suggested implementation shape

A practical mato design would look like this:

1. **Agent responsibilities**
   - claim task
   - create `task/<name>`
   - make changes and commit
   - run tests
   - push `task/<name>` to origin
   - report branch name / task result
   - stop

2. **Host responsibilities**
   - track branches ready to merge
   - merge one branch at a time into `mato`
   - run any required validation after each merge
   - mark task `completed/` only after successful integration
   - mark branch/task blocked if integration fails

3. **Task state changes**
   - consider adding an intermediate state such as `awaiting-merge/` or queue metadata
   - reserve `completed/` for tasks that are actually present on the target branch

4. **History policy**
   - default: `git merge --squash task/<name>` followed by one commit per task
   - optional: regular merge mode if preserving detailed task-branch history becomes valuable

### Final verdict

**Recommendation: move mato to a host-managed serial merge queue, ideally with squash merge as the default integration mode.**

That gives mato the best balance of reliability, implementation cost, prompt simplicity, and readable history. It fixes the concurrency race without forcing mato all the way into a full GitHub/PR/CI merge-queue architecture on day one.
