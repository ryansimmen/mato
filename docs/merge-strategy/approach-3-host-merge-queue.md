# Approach 3: Host-Managed Merge Queue for `mato`

## Executive summary

Move responsibility for merging into the target branch out of the Dockerized agent and into the `mato` host process.

Agents should only:
1. claim a task
2. create or resume a task branch from the target branch
3. do the work
4. commit
5. push the **task branch** to `origin`
6. hand the task back to the host for integration

The host should then run a **single-writer merge queue** per target branch:
- discover tasks that were pushed successfully
- order them deterministically
- merge them **one at a time** in a temporary integration clone
- run validation before push
- push the updated target branch
- move the task to `completed/` only after the merge succeeds

This makes the agent prompt much simpler and makes merge policy, retries, validation, and rollback consistent across all tasks.

---

## Why change the current design?

Today the agent prompt carries too much git orchestration logic:
- fetch and merge target into task branch
- resolve conflicts
- switch branches
- merge into the target branch
- push the target branch
- retry non-fast-forward failures

That is exactly the kind of deterministic control flow the host is better at.

A host-managed merge queue gives `mato` these advantages:
- **simpler agent prompt**
- **one merge policy** for every task
- **one validation path** between merges
- **clearer observability** for conflicts and retries
- **safer multi-process behavior** because only the host merges to the target branch

The main cost is that the host loses the agent's rich task-local context during conflict resolution. That tradeoff is real and should shape the conflict policy.

---

## Recommended queue/state model

The current directories are:

```text
.tasks/
├── backlog/
├── in-progress/
├── completed/
└── failed/
```

For a host-managed merge queue, `completed/` must continue to mean **merged into the target branch**, not merely "agent finished coding". Otherwise dependency logic and human expectations become ambiguous.

### Recommended revised state model

```text
.tasks/
├── backlog/
├── in-progress/
├── ready-to-merge/   # new: agent pushed branch, host has not integrated it yet
├── completed/        # merged into target successfully
├── failed/
└── .locks/
```

If `mato` also adopts the previously discussed `waiting/` directory for dependency scheduling, then conflict requeues should go to `waiting/` rather than directly to `backlog/`. For this document, `ready-to-merge/` is the key addition.

### Runtime metadata the host should track

The task file should remain the source of truth, with lightweight runtime comments added by the agent and host. Recommended additions:

```html
<!-- task-branch: task/add-retry-logic -->
<!-- branch-head: abc1234 -->
<!-- branch-pushed-at: 2026-03-14T12:34:56Z -->
<!-- merge-attempts: 1 -->
<!-- merged-by: host-1234 merged-at: 2026-03-14T12:36:10Z merge-commit: def5678 -->
<!-- merge-failure: host-1234 at 2026-03-14T12:35:20Z — conflict against mato@fedcba9 -->
```

The host should prefer **task-file metadata plus queue directory location** over scanning raw git refs. Branches alone are not enough to tell whether a task is ready, stale, superseded, reverted, or already integrated.

---

## 1. Revised agent workflow

### New contract

Agents should no longer merge into `TARGET_BRANCH_PLACEHOLDER`.

They should only:
1. claim the task
2. create the task branch from the current target branch
3. do the work
4. commit
5. push the task branch to `origin`
6. write branch metadata into the task file
7. move the task file to `ready-to-merge/`
8. exit

### Recommended agent flow

```text
backlog/ -> in-progress/ -> ready-to-merge/
```

### Important details

- The branch name can stay `task/<sanitized-filename>`.
- The agent should verify that `git push origin <task-branch>` succeeded.
- The agent should record the pushed branch name and `HEAD` SHA in the task file.
- The agent should **not** check out the target branch again.
- The agent should **not** merge or push the target branch.
- The agent should keep the failure flow for ordinary task failures, but merge failures now belong to the host.

### Why this is much simpler

The agent no longer needs logic for:
- rebasing/merging against a moving target branch
- retrying non-fast-forward pushes
- conflict resolution against unrelated concurrent work
- deciding when a merge is safe to publish

That materially shrinks the prompt and reduces the chance that an LLM gets stuck in git-state repair loops.

---

## 2. Host merge loop

The merge loop should run in the host, not in the container, and it should merge **sequentially** per target branch.

### Key principle

Only one host process should be allowed to merge into a given target branch at a time, even though many host processes may continue launching agents concurrently.

### Recommended loop shape

Do not limit merge processing to only "immediately after `runOnce()` returns". The host should merge whenever there is pending merge work.

Recommended `run()` shape:

```text
for each poll iteration:
  1. recover orphaned in-progress tasks
  2. clean stale agent locks
  3. reconcile ready task queue            # if dependency scheduling exists
  4. process ready-to-merge queue          # NEW
  5. if there are ready backlog tasks and pending-merge pressure is acceptable:
       runOnce(...)
  6. process ready-to-merge queue again    # NEW, catch freshly pushed branch
  7. sleep only if no agent work and no merge work was performed
```

### Why merge before and after `runOnce()`?

- **Before**: drains existing pending branches even when no new agent just finished.
- **After**: quickly integrates the branch that the just-finished agent pushed.
- **When backlog is empty**: the host must still merge queued branches instead of idling.

### Merge loop algorithm

For each pass:

1. Try to acquire a **branch-specific merge lock**.
2. Scan `.tasks/ready-to-merge/` and parse metadata into `MergeCandidate` objects.
3. Order the candidates deterministically.
4. Pick the next candidate.
5. Create or refresh a temporary integration clone.
6. `git fetch origin`.
7. Check out the latest target branch.
8. Attempt a **non-committed merge** of the task branch into the target branch:
   - `git merge --no-ff --no-commit origin/<task-branch>`
9. If Git reports conflicts, abort the merge and requeue according to policy.
10. If the merge applies cleanly, run validation.
11. If validation passes, create the merge commit and push the target branch.
12. Mark the task `completed/`, append merge metadata, and optionally delete the remote task branch.
13. Re-scan the queue, because the next candidate order may have changed.

### Why `--no-commit`?

It lets the host validate the merged worktree **before** publishing a merge commit. That is much cleaner than pushing first and reverting later.

### Use a temporary integration clone

Do not merge directly in `repoRoot`.

Use the same temp-clone idea as `runOnce()`:
- create a fresh clone of `repoRoot`
- fetch the task branch and target branch
- merge and validate there
- push the target branch back to `origin` (`repoRoot`)

This keeps the host merge loop isolated from the user's working tree and from any concurrent host process manipulating its own temp clone.

### Branch cleanup

After a successful merge, the host should usually delete the remote task branch:

```bash
git push origin --delete task/<name>
```

That keeps the repository tidy. If branch retention is desirable for audit/debugging, make deletion configurable.

---

## 3. Queue ordering

The merge queue should not be "whatever branch finished first". Completion order alone is too naive.

### Recommended ordering

Order merge candidates by:

1. **dependency order** among pending merge candidates
2. **normalized host priority** from task frontmatter
3. **completion/push time** (`branch-pushed-at`) — oldest first
4. **stable lexical tie-breaker** (task ID or filename)

### Why dependency order first?

If task B depends on task A, and both branches are waiting in `ready-to-merge/`, merging B first is the wrong result even if B finished earlier.

The host should therefore build a small DAG over the pending merge set and topologically sort it before applying priority.

### Why priority second?

If the task system already has frontmatter priority, the merge queue should reuse it. Do **not** invent a second meaning for priority only at merge time.

The host should normalize the frontmatter into one internal rank and use that same rank consistently for:
- ready-task selection
- merge ordering
- requeue policy

### Why push time third?

Among otherwise equivalent candidates, merge the oldest pushed branch first to reduce branch staleness and lower the chance of future conflicts.

### Why not pure FIFO?

FIFO is simple, but it ignores:
- urgent fixes
- dependency relationships
- tasks deliberately prioritized by the operator

### Why not pure priority?

Pure priority can starve older ready branches and increase conflict rates. The age tie-breaker keeps the queue moving.

---

## 4. Conflict handling

This is the most important design choice, because the host does **not** have the agent's full task context.

### Options

| Option | Behavior | Pros | Cons |
|---|---|---|---|
| Requeue for rework | Move task out of `ready-to-merge/`, preserve branch metadata, let an agent refresh it | Safest, keeps agent-in-the-loop | More retries, slower throughput |
| Host auto-resolution | Host tries to resolve conflicts itself (rules, strategy flags, or an LLM) | Highest throughput if it works | Highest risk of wrong merge |
| Skip and continue | Leave this candidate blocked for now and merge later candidates first | Prevents one conflict from stalling queue | Queue state becomes more complex |
| Immediate fail | Move task straight to `failed/` after first conflict | Simple | Too harsh for ordinary drift |

### Recommended policy

**Recommended MVP:**
1. **Do not auto-resolve textual conflicts on the host.**
2. **Record the merge conflict as host-owned failure metadata.**
3. **Move the task out of `ready-to-merge/` for rework** (prefer `waiting/` with cooldown; use `backlog/` if no waiting queue exists).
4. **Preserve the remote task branch** and record it in the task file.
5. **Continue with later merge candidates** instead of stalling the entire queue.

### Why preserve the existing branch?

The branch already contains the task's work. Throwing it away would force a new agent to recreate work from scratch.

The better model is:
- host says: "this branch no longer merges cleanly"
- next agent resumes from that branch
- agent rebases/refreshes/tests the branch
- agent pushes the updated branch back to `ready-to-merge/`

This still keeps the agent prompt much simpler than today's full merge-to-target dance.

### Requeue metadata

When requeueing after conflict, append something like:

```html
<!-- merge-failure: host-1234 at 2026-03-14T12:35:20Z — conflict against mato@fedcba9 -->
<!-- resume-branch: task/add-retry-logic -->
```

If the conflict count crosses a threshold, move the task to `failed/` rather than looping forever.

### Future enhancement: `git rerere`

A reasonable future improvement is enabling `git rerere` in the host integration clone so Git can reuse previously recorded conflict resolutions. That is safer than inventing new LLM-driven conflict resolution in v1.

---

## 5. Validation between merges

### Should the host run tests between each merge?

**Yes, by default.**

The host merge queue is exactly where integration validation belongs. The agent already tested the task branch in isolation; the host should test the **combined result** on top of the latest target branch.

### Recommended validation model

For each clean tentative merge:

1. merge task branch into target with `--no-commit`
2. run the repository's configured validation commands
3. only create/push the merge commit if validation passes

### What should the host run?

Use a host-configured validation command list, not hard-coded repo heuristics.

Examples:
- `go test ./...`
- `make test`
- `make lint test`
- `npm test`

The best implementation is a small config surface, for example:
- CLI flag: `--validate-cmd`
- repeated flag: `--validate-cmd "go test ./..." --validate-cmd "make lint"`
- or repo config file later

### Why not rely only on agent-side tests?

Because the failure mode we care about is **integration breakage**:
- two valid branches that fail together
- build breaks caused by target-branch drift
- runtime or semantic conflicts that only appear after merge

### Can the host detect semantic conflicts?

Only partially.

The host can reliably detect:
- textual merge conflicts
- compile failures
- failing automated tests
- failing lint/static analysis

The host cannot guarantee detection of:
- missing business logic interactions
- subtle behavior regressions not covered by tests
- production-only failures

So the correct stance is:
- **use the best existing test suite available**
- **treat host validation as a gate, not a proof of correctness**
- **keep optional downstream CI or smoke tests for extra confidence**

### Recommended validation tiers

| Tier | When | Purpose |
|---|---|---|
| Agent validation | before push of task branch | prove task branch is locally sound |
| Host pre-push validation | after tentative merge, before pushing target | catch integration failures |
| Optional external CI | after target push | catch slower or environment-specific failures |

### Throughput tradeoff

Running validation after every merge reduces throughput, but skipping it removes the main value of the host-managed queue. The queue should optimize for correctness first.

---

## 6. Implementation in Go (`main.go` and `runOnce()`)

### `runOnce()` changes

`runOnce()` can stay structurally similar:
- create temp clone
- launch Docker container
- mount `.tasks/`
- run the embedded prompt
- clean up clone

The major change is not Docker orchestration; it is the **agent contract**.

### Recommended `runOnce()` behavior

Keep `runOnce()` as a fire-and-forget agent launcher. Do **not** make it parse container stdout to discover success.

Instead, let the host discover results through shared state:
- task file moved to `ready-to-merge/`
- branch metadata recorded in the task file

That is more robust than scraping logs.

### New/changed functions in `main.go`

Recommended helpers:

```go
type MergeCandidate struct {
    TaskPath      string
    TaskID        string
    Filename      string
    Branch        string
    HeadSHA       string
    PushedAt      time.Time
    Priority      int
    DependsOn     []string
    FailureCount  int
}

type MergeResult struct {
    Status      MergeStatus
    MergeCommit string
    Reason      string
}
```

Suggested functions:

- `processMergeQueue(repoRoot, tasksDir, branch, hostID, validationCmds) (didWork bool, err error)`
- `discoverMergeCandidates(tasksDir) ([]MergeCandidate, error)`
- `orderMergeCandidates([]MergeCandidate) ([]MergeCandidate, error)`
- `tryAcquireMergeLock(tasksDir, branch, hostID) (release func(), acquired bool, err error)`
- `mergeCandidate(repoRoot, branch string, c MergeCandidate, validationCmds []string) (MergeResult, error)`
- `validateMergedWorktree(cloneDir string, cmds []string) error`
- `markMergeSucceeded(tasksDir string, c MergeCandidate, mergeCommit, hostID string) error`
- `markMergeRejected(tasksDir string, c MergeCandidate, reason string, requeue bool) error`
- `parseTaskMetadata(path string) (...)`
- `parseMergeMetadata(path string) (...)`

### Replace `hasAvailableTasks()` with fuller queue accounting

The current `hasAvailableTasks()` is too weak. The host should know at least:
- ready backlog count
- ready-to-merge count
- whether merge backpressure should pause new agents

A function like this is more appropriate:

```go
type QueueState struct {
    ReadyTaskCount       int
    ReadyToMergeCount    int
    MergeBackpressure    bool
}
```

### Interaction with the polling loop

Recommended host behavior:

```text
recoverOrphanedTasks()
cleanStaleLocks()
reconcileReadyQueue()          # if dependency queue exists
processMergeQueue()
if QueueState allows new agent launch:
  runOnce()
  processMergeQueue()
wait if nothing happened
```

### Backpressure is important

If agents finish branches faster than the host can merge them, pending branches grow stale and conflicts increase.

So the host should eventually add a cap such as:
- `maxPendingMerges`
- pause new agent launches when `ready-to-merge/` exceeds that cap
- resume launching when the merge queue drains

That keeps the queue from becoming a conflict factory.

### Interaction with `receive.denyCurrentBranch=updateInstead`

The setting can remain.

- Agents now push only task branches, which do **not** update the checked-out branch in `repoRoot`.
- The host is the only component that pushes the target branch.
- `updateInstead` still matters because the host pushes the checked-out target branch back into `repoRoot`.

This is strictly easier to reason about than many agents pushing the target branch concurrently.

---

## 7. Impact on `task-instructions.md`

This change removes the most fragile part of the prompt.

### What becomes simpler?

The prompt no longer needs to teach the agent to:
- merge `origin/TARGET_BRANCH_PLACEHOLDER` into its branch
- resolve merge conflicts against other agents' work
- switch back to the target branch
- merge the task branch into the target branch
- push the target branch
- retry non-fast-forward pushes

That is a major simplification, not a small edit.

### Recommended revised Step 7

````markdown
### Step 7: Push Your Task Branch and Hand Off to the Host

The `mato` host is responsible for merging into `TARGET_BRANCH_PLACEHOLDER`.
You must only push your task branch.

```bash
git push origin <task-branch>
BRANCH_SHA=$(git rev-parse HEAD)
PUSHED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
echo "<!-- task-branch: <task-branch> -->" >> ".tasks/in-progress/<filename>"
echo "<!-- branch-head: $BRANCH_SHA -->" >> ".tasks/in-progress/<filename>"
echo "<!-- branch-pushed-at: $PUSHED_AT -->" >> ".tasks/in-progress/<filename>"
```

Verify the push succeeded. Do **not** merge into `TARGET_BRANCH_PLACEHOLDER`.
Do **not** check out `TARGET_BRANCH_PLACEHOLDER` again.
Do **not** push `TARGET_BRANCH_PLACEHOLDER`.

After the push succeeds, move the task file to `ready-to-merge/`:

```bash
mv ".tasks/in-progress/<filename>" ".tasks/ready-to-merge/<filename>"
```

You are done once the branch is pushed and the task file is handed off to the host.
````

### What else changes in the prompt?

Step 8 should no longer say "Mark Complete" by moving directly to `completed/`.
That move now belongs to the host after a successful merge. The agent's final state is `ready-to-merge/`.

---

## 8. Multi-process considerations

This design only works well if multiple `mato` hosts coordinate merges explicitly.

### Current situation

Today multiple hosts can run safely because task claiming is atomic and each agent works in its own temp clone. But target-branch merges are still effectively multi-writer from the agent side.

### New requirement

With a host-managed merge queue, task execution can stay multi-process, but **merging to one target branch must become single-writer**.

### Recommended lock

Add a branch-specific lock file under `.tasks/.locks/`, for example:

```text
.tasks/.locks/merge-mato.lock
```

Contents should include at least:
- host ID
- PID
- timestamp
- target branch name

### Lock acquisition strategy

Use one of these:

1. **Preferred:** create-with-`O_EXCL` lock file plus PID liveness checks, matching the current lock style.
2. **Alternative:** OS file locking (`flock`) if you are comfortable with platform-specific behavior.

For consistency with the existing codebase, the `O_EXCL` + PID metadata approach fits well.

### Stale lock recovery

The merge lock should be recoverable the same way agent PID locks are:
- if PID is dead, another host may remove the stale lock
- if PID is alive, other hosts skip merge processing for now

### Important limitation

The lock only coordinates **mato hosts**. It does **not** stop:
- a human pushing directly to the target branch
- another tool updating the repo outside mato

So the host merge code must still fetch the latest target before each merge and handle push rejection or drift even when the lock works perfectly.

### Recommended non-blocking behavior

If a host cannot acquire the merge lock, it should:
- skip merge processing for that iteration
- continue doing non-merge work such as queue reconciliation or agent launching, subject to merge backpressure

That keeps the system concurrent without letting multiple hosts publish the target branch simultaneously.

---

## 9. Rollback strategy

### Best rollback is "do not push a bad merge"

The primary rollback strategy should be **pre-push validation** in the temp integration clone. If validation fails before push, the host should simply abort the merge and requeue the task. No revert is needed.

### But what if a bad merge is pushed anyway?

That can still happen if:
- the validation suite missed the bug
- a later CI system catches a slower failure
- an operator decides the merge was wrong

### Recommended requirements for reversible merges

Always create a merge commit for queue merges (`--no-ff`).

That gives the host a concrete commit to revert later:

```bash
git revert -m 1 <merge-commit>
```

### Recommended rollback flow

1. acquire the merge lock
2. create a temp integration clone
3. check out the target branch
4. revert the specific queue merge commit
5. run validation on the reverted state
6. push the revert commit to the target branch
7. append rollback metadata to the task file
8. move the task back to `waiting/` or `backlog/` for rework, preserving `resume-branch`

### Caveat: later merges may already depend on the bad merge

This is the hardest rollback case.

If merges B and C landed after bad merge A, an automatic revert of A may:
- conflict
- partially undo assumptions relied on by B/C
- create a cascade of breakage

So the recommended policy is:
- **automatic revert is safe by default only for immediately detected failures**
- for later-discovered failures after more queue progress, prefer **operator-supervised revert** or a new repair task

### Metadata needed for rollback

The host should record at least:
- source task branch
- source head SHA
- merge commit SHA
- merged-at time
- host ID that performed the merge

Without that metadata, rollback and audit become much harder.

---

## 10. Tradeoffs

### What gets better

### Simpler agent prompt

This is the biggest win. Agents stop acting like ad-hoc merge bots and go back to being task executors.

### More deterministic integration policy

The host decides:
- merge order
- validation commands
- retry limits
- conflict policy
- rollback behavior

That is much easier to reason about than every agent improvising its own git recovery flow.

### Better observability

The host can expose queue state clearly:
- pending merges
- blocked-by-conflict tasks
- validation failures
- merged commit history

### Safer target branch handling

Only the host pushes the target branch. That is a large reduction in concurrency risk.

### What gets worse

### Conflict resolution loses task context

This is the core drawback.

The agent that wrote the code knows:
- what it was trying to achieve
- which side of a conflict matters semantically
- what tests it already considered

The host merge loop knows none of that. So when conflicts happen, the host is much more limited.

### The host becomes more complex

Complexity moves out of the prompt and into Go code:
- merge queue discovery
- ordering
- locking
- validation
- retry logic
- rollback metadata
- new queue states

This is the right kind of complexity, but it is still real complexity.

### Merge throughput is serialized

Task execution remains parallel, but final publication to a branch is serialized. That can become a bottleneck if:
- validation is slow
- merge queue depth grows
- many tasks touch the same hot files

### More branch lifecycle management

The system now needs clear policy for:
- deleting merged task branches
- preserving conflicted branches
- resuming stale branches
- pruning abandoned branches

### New state transitions

The filesystem queue gets another lifecycle stage (`ready-to-merge/`), and possibly `waiting/` if dependency/backoff handling also lands. Operators now need to understand both "agent finished" and "merged" as separate states.

---

## Recommended MVP decision

If `mato` adopts host-managed merging, the safest first version is:

1. add `ready-to-merge/`
2. simplify the agent so it only pushes task branches
3. add a branch-scoped host merge lock
4. merge in a temp integration clone
5. run configured validation before pushing target
6. on conflict or failed validation, requeue the task for branch refresh
7. skip blocked candidates and continue merging later ones
8. record merge metadata and use `--no-ff` merge commits for rollbackability

That gives `mato` a real integration queue without asking the host to invent semantic conflict resolutions it is not equipped to make.

## Final recommendation

**Yes — move target-branch merging to the host.**

But do it with a real queue model, not just "after `runOnce()` returns, push whatever branch exists".

The design should include:
- a new `ready-to-merge/` state
- deterministic queue ordering
- a single-writer merge lock per target branch
- pre-push validation in a temp clone
- explicit requeue policy for conflicts
- metadata sufficient for audit and rollback

That preserves the best part of the current architecture — cheap parallel task execution in isolated clones — while removing the most brittle part: asking each agent to perform its own merge choreography against a moving shared branch.
