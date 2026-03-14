# Merge Conflict Prevention for mato — Approach 2

## Current State

Today, mato prevents agents from claiming the **same task file**, but it does **not** prevent them from editing the same repository file.

From `main.go` and `task-instructions.md`, the current flow is:

1. The host keeps a shared `.tasks/` queue and launches multiple agents in parallel.
2. Each agent claims a task by moving it from `backlog/` to `in-progress/`.
3. Each agent works in its own temporary clone and dedicated task branch.
4. The agent commits locally.
5. Only in Step 7 does the agent fetch `origin/TARGET_BRANCH`, merge, and discover whether another task already changed the same files.

This means conflicts are detected **late**, after the expensive work is already done. The goal of this approach is to move conflict handling earlier: ideally at scheduling time, or at least before an agent edits a contested file.

---

## What “Prevention” Should Mean in mato

For mato, prevention does **not** need to guarantee that conflicts are mathematically impossible. That would require the host to fully control every write, which does not fit mato’s current autonomous-agent model.

A practical prevention strategy should instead:

- prevent the **common** case of two agents editing the same file or module
- keep most non-overlapping tasks fully parallel
- work across multiple independent mato processes
- degrade safely if metadata is missing or incomplete
- still rely on post-merge tests for semantic conflicts that Git cannot detect

The key distinction is:

- **Host-managed prevention** is stronger and more reliable.
- **Agent-cooperative prevention** is useful, but only advisory.

That makes some strategies much more attractive than others.

---

## Comparative Summary

| Strategy | Integration with current flow | Implementation complexity | Conflict reduction in practice | Parallelism impact | Cooperation needed |
|---|---|---:|---|---|---|
| **1. File-level advisory locks** | Add a shared file-claim registry under `.tasks/`; agents check/claim before editing | Medium | Medium for direct same-file conflicts; low for semantic conflicts | Low-Medium | **Agent cooperation required** |
| **2. Task-level file manifests** | Add `affects:` metadata to task files; host avoids launching overlapping tasks together | Medium | High for predictable overlap | Low for disjoint work; Medium when manifests are broad | Mostly host-managed; task author cooperation |
| **3. Sequential mode for overlapping tasks** | Keep overlapping tasks in `waiting/` until the active one completes | Low once manifests exist | High for hotspot modules/files | Medium-High for overlapping areas only | **Fully host-managed** once manifests exist |
| **4. Workspace partitioning** | Change task authoring guidance so concurrent tasks target different modules | Low technical / Medium process | Medium-High if followed consistently | Low overall; planning overhead instead | Task author / operator cooperation |
| **5. Post-merge validation** | Run tests after merge to target branch | Low-Medium | Does not prevent text conflicts; catches semantic conflicts | Medium if full suite runs after every merge | **Fully host-managed** |

---

## 1. File-Level Advisory Locks

### Idea

Before editing `path/to/file.go`, an agent checks a shared registry and tries to claim that file. If another live agent already holds the claim, it avoids editing the file, pauses, or fails the task back to the queue.

This could live in either:

- `.tasks/messages/claims/` if mato adopts message-based coordination, or
- a dedicated lock area such as `.tasks/file-locks/`

For mato, a dedicated lock area is the cleaner source of truth. Messages are good for audit/history; lock files are better for current state.

### Integration with mato’s Existing Flow

This fits naturally into **Step 5: Work on the Task** in `task-instructions.md`.

Revised agent flow:

1. Read task instructions.
2. Before editing a file, attempt to acquire a lock for that path.
3. If lock succeeds, proceed.
4. If lock fails because another active agent owns it, either:
   - skip that file and choose another implementation path if possible, or
   - stop and return the task to `backlog/` with a conflict note.
5. Release locks when the task is complete or on failure.

The host can support this by:

- creating `.tasks/file-locks/`
- reusing the existing `.tasks/.locks/<agent>.pid` heartbeat to garbage-collect stale file locks
- optionally mounting a tiny helper CLI or shell script so agents do not have to reimplement lock logic themselves

### Recommended Lock Representation

Use one lock file per claimed repo path, for example:

```text
.tasks/file-locks/pkg__client__http.go.lock
```

Contents could be JSON or plain text:

```json
{
  "agent_id": "a1b2c3d4",
  "task": "fix-client-timeout",
  "path": "pkg/client/http.go",
  "claimed_at": "2026-03-14T12:00:00Z"
}
```

The important part is that acquisition must be **atomic**. In Go, that means `os.OpenFile(..., O_CREATE|O_EXCL, ...)` or an atomic `mkdir`. Plain “check then write” is not enough.

### Implementation Complexity

**Medium.** The file-system pieces are straightforward, but the reliability challenge is behavioral:

- agents must remember to acquire locks before editing
- they must release locks on success and failure
- they may discover new files to edit midway through a task
- broad edits like refactors may require many locks

This is simpler if mato provides a helper command such as:

```bash
mato lock acquire pkg/client/http.go
mato lock release pkg/client/http.go
```

Without a helper, the prompt and shell snippets become fragile.

### How Much It Reduces Conflicts

**Moderate reduction** for direct, literal same-file conflicts.

It works well for:

- `main.go`
- `go.mod`
- shared config files
- a single package file that multiple tasks may touch

It does **not** solve:

- undeclared edits to nearby files
- semantic conflicts across different files
- agents ignoring the lock protocol

So it is a useful safety net, but not the strongest primary strategy.

### Tradeoffs

- **Pros:** fine-grained; preserves parallelism when tasks touch different files
- **Cons:** only advisory; adds prompt complexity; stale locks must be cleaned up; agents can still diverge from plan

### Host-Managed or Agent-Cooperative?

**Agent-cooperative.** The host can provide infrastructure and cleanup, but it cannot fully enforce “claim before every edit” in mato’s current model.

### Verdict

Useful as a **secondary** safety layer, especially for high-contention files. Not strong enough to be the main prevention mechanism by itself.

---

## 2. Task-Level File Manifests

### Idea

Each task declares the files or directories it is likely to touch, for example:

```yaml
---
affects:
  - pkg/client/*.go
  - pkg/client/testdata/**
---
```

The host uses that metadata to avoid running overlapping tasks at the same time.

This is the cleanest way to move conflict prevention **upstream**, before an agent even starts work.

### Integration with mato’s Existing Flow

This belongs in the host scheduler, not in the agent.

A practical integration would be:

1. Extend task files with YAML frontmatter containing `affects:`.
2. Add a `waiting/` directory, similar to the previously recommended dependency model.
3. In `run()`, before deciding whether work is available, add a scheduler pass such as:
   - parse task manifests in `waiting/` and `backlog/`
   - identify which tasks overlap with tasks already in `in-progress/`
   - only keep non-conflicting tasks in `backlog/`
   - leave overlapping tasks in `waiting/`
4. Agents continue to claim tasks exactly as they do now; they simply see a safer backlog.

This fits mato’s philosophy well because the host already owns:

- task discovery
- task state transitions
- agent liveness

It is therefore the right layer to own scheduling decisions too.

### How to Detect Overlap

Exact glob-intersection is harder than it looks, so mato should use a pragmatic conservative rule:

1. Expand globs against the current repo tree.
2. Compare the matched file sets.
3. If a pattern matches no current files but refers to a directory subtree, treat overlaps in the same subtree as conflicting.
4. Treat certain files as always high-contention if listed anywhere, e.g.:
   - `go.mod`
   - `go.sum`
   - `main.go`
   - root config files
   - generated artifacts

This will occasionally over-serialize work, but that is preferable to underestimating overlap.

### Implementation Complexity

**Medium.** Most of the work is in parsing metadata and building a conservative overlap detector.

The good news is that mato already appears to be moving toward structured task metadata in other design work, so `affects:` is a natural extension rather than a one-off feature.

### How Much It Reduces Conflicts

**High reduction** for predictable merge conflicts.

Why it works well:

- most tasks already have a likely area of impact
- conflicts usually cluster around a few hotspot modules, not the whole repo
- the host can prevent overlapping tasks from ever starting together

This likely prevents the majority of same-file and same-module merge conflicts before work begins.

### Tradeoffs

- **Pros:** strong prevention; host-managed; keeps agent prompt simpler; works across multiple mato processes
- **Cons:** manifests can be incomplete or wrong; broad globs reduce parallelism; authors must think about impact area up front

### Host-Managed or Agent-Cooperative?

**Mostly host-managed.** Execution agents do not need to cooperate at runtime. The main cooperation is from task authors, who must supply reasonable manifests.

### Verdict

This should be the **primary prevention mechanism**. It gives the host enough information to make good concurrency decisions without requiring agents to behave perfectly.

---

## 3. Sequential Mode for Overlapping Tasks

### Idea

If two tasks affect the same file set or module, do not run them concurrently. Keep one task active and the others waiting.

This is the simplest reliable answer for known overlap.

### Integration with mato’s Existing Flow

Sequential mode is best implemented as a scheduling consequence of manifests:

- tasks with no overlap remain eligible for `backlog/`
- overlapping tasks stay in `waiting/`
- when the active task completes, the next waiting task becomes eligible

The host loop would effectively maintain “concurrency groups” by module or manifest overlap.

This can be explicit or implicit:

- **Implicit:** derived from `affects:` overlap
- **Explicit:** task metadata like `serialize_with: [client]`

Implicit scheduling is better for an MVP because it avoids introducing another concept unless needed.

### Implementation Complexity

**Low once manifests exist.** The hard part is overlap detection. Once mato knows that Task A and Task B overlap, keeping one in `waiting/` is simple.

### How Much It Reduces Conflicts

**High** for overlapping work, because it eliminates concurrency where conflict is most likely.

This is especially valuable for:

- multiple tasks in the same package
- schema or migration work
- shared entrypoints like `main.go`
- repo-wide refactors

### Tradeoffs

The obvious cost is reduced parallelism. But that cost is localized:

- disjoint modules still run in parallel
- only hotspot areas become serialized

That is the right tradeoff. The goal is not “maximum concurrency at all times”; it is “maximum safe concurrency.”

### Host-Managed or Agent-Cooperative?

**Fully host-managed**, assuming manifests are present.

### Verdict

This is not a separate alternative so much as the **enforcement policy** that makes task manifests useful. If mato adds manifests, it should also add sequential scheduling for overlap.

---

## 4. Workspace Partitioning

### Idea

Design tasks so they naturally land in different parts of the repo. In other words: reduce conflicts by writing better tasks.

Examples:

- “Update `pkg/client/` retry logic” and “Improve `docs/` examples” can run together.
- “Refactor `pkg/client/` error handling” and “Add new `pkg/client/` timeout behavior” should probably not run together.

### Integration with mato’s Existing Flow

This requires almost no code, but it should influence task creation conventions.

Recommended guidelines for task authors:

1. **Prefer module-scoped tasks.** Write tasks around one package or feature area.
2. **Avoid broad “cleanup” tasks** that touch many unrelated files.
3. **Separate refactors from feature work.** Refactors are conflict magnets.
4. **Reserve shared files for integration tasks.** Files like `main.go`, `go.mod`, top-level configs, and shared schemas should be touched by fewer concurrent tasks.
5. **Split vertical work into leaves plus integration.** Let agents work on leaf modules in parallel, then create one explicit integration task afterward.

This can also be reinforced by a task template that includes an `affects:` field by default.

### Implementation Complexity

**Low technical complexity, medium operational complexity.** The code changes are minimal; the real work is enforcing better task-writing discipline.

### How Much It Reduces Conflicts

**Moderate to high**, depending on how consistently the backlog is curated.

If the backlog is well-partitioned, conflict prevention becomes much easier because the scheduler sees fewer ambiguous tasks.

### Tradeoffs

- **Pros:** improves throughput and predictability without much code
- **Cons:** depends on human discipline; task authors may misjudge impact area; some work is inherently cross-cutting

### Host-Managed or Agent-Cooperative?

Neither, strictly speaking. This is mostly **task-author cooperation** with some optional host validation.

### Verdict

Workspace partitioning is good operating hygiene. It should support the technical strategy, but it should not be the only line of defense.

---

## 5. Post-Merge Validation

### Idea

Even if Git merges cleanly, the result may still be wrong. One task may change an interface while another updates a caller; both merge without conflict, but the combined behavior fails tests.

Post-merge validation is therefore necessary even in a prevention-oriented design.

### Integration with mato’s Existing Flow

Today, agents already run tests during Step 5. The missing piece is validation **after merge to the shared target branch**.

Possible integration options:

1. **After every successful merge/push**, the host checks out the target branch and runs the repo’s normal test suite.
2. If the suite is too expensive, run targeted tests based on `affects:` first, then full-suite periodically.
3. If validation fails:
   - stop promoting overlapping tasks from `waiting/`
   - create a repair task or mark the branch unhealthy
   - optionally block new merges until green again

This is especially important because mato’s agents merge independently into the same target branch.

### Implementation Complexity

**Low to medium.** The mechanics are simple; the main cost is runtime and policy decisions.

### How Much It Reduces Conflicts

It does **not** reduce text conflicts directly.

What it does do is catch:

- semantic conflicts across files
- interface drift
- broken assumptions between tasks
- tests that pass in isolation but fail after integration

So this is a **detection and containment** layer, not true prevention.

### Tradeoffs

- **Pros:** catches the problems prevention misses; fully host-controlled; increases confidence in the shared branch
- **Cons:** slower throughput if the full suite runs after every merge; failures may still require manual or follow-up-task repair

### Host-Managed or Agent-Cooperative?

**Fully host-managed** if mato runs the validation step itself.

### Verdict

Mandatory as the final safety net. Even perfect file-level prevention cannot catch all semantic integration failures.

---

## Can These Strategies Be Combined?

Yes — and they should be.

These strategies are not competitors so much as layers at different points in the lifecycle:

1. **Before scheduling:** workspace partitioning + task manifests
2. **At scheduling time:** sequential mode for overlapping tasks
3. **During execution:** advisory file locks for surprise overlap
4. **After integration:** post-merge validation

That layered design is important because each strategy catches a different failure mode.

### Best Combination for mato

The most practical combination is:

#### 1. Primary layer: **Task-level manifests (`affects:`)**
This gives the host early visibility into likely overlap.

#### 2. Enforcement layer: **Sequential mode for overlapping tasks**
This keeps the safe parallelism mato wants while serializing only the risky subset.

#### 3. Operational layer: **Workspace partitioning guidelines**
This improves manifest accuracy and reduces scheduler ambiguity.

#### 4. Safety layer: **Post-merge validation**
This catches semantic conflicts and clean-merge failures.

#### 5. Optional later layer: **File-level advisory locks**
Useful for dynamic or under-declared edits, but best added after host-managed scheduling exists.

### Why This Order Is Best

Because mato’s agents are autonomous, the most dependable controls are the ones the **host** can enforce without asking the agent to remember more rules.

That means:

- host-managed scheduling should do most of the prevention work
- agent-level locking should be a refinement, not the foundation

If only one prevention feature is implemented first, it should be **task manifests plus overlap-based scheduling**.

---

## Realistic Conflict Rate for Typical mato Tasks

A precise number depends on backlog quality, but for mato’s expected workload the realistic answer is:

**Most tasks probably do not conflict.**

Why:

- many tasks touch 1-3 files
- agents usually work in separate feature areas
- repos have natural module boundaries
- merge conflicts are usually caused by a few shared hotspots, not random file overlap everywhere

A realistic rule-of-thumb is:

| Task pairing | Likely text-conflict rate |
|---|---:|
| Different modules / clearly disjoint tasks | **0-5%** |
| Same module, different files | **5-15%** |
| Same module, likely same file or shared interface | **15-40%** |
| Cross-cutting refactors / config / `go.mod` / entrypoints | **40%+** |

Those are directional bands, not hard measurements, but they match how conflicts usually cluster in real repositories.

### Key Implication

Because the conflict distribution is **lumpy**, mato should not serialize everything.

Instead, it should:

- keep obviously disjoint tasks parallel
- aggressively prevent overlap in hotspot areas

That is exactly what manifests + sequential mode accomplish.

---

## When Should mato Prevent Conflicts vs. Just Handle Them at Merge Time?

### Prefer Prevention When

Prevention is worth it when the expected cost of a late conflict is high.

That includes:

- tasks touching the same package or shared module
- edits to known hotspot files (`main.go`, `go.mod`, configs, schemas)
- refactors
- generated code
- expensive tasks where discovering conflict late wastes a lot of work
- long-running tasks where repeated rebasing/merging would be noisy and error-prone

In these cases, reduced parallelism is cheaper than duplicated work and conflict resolution.

### Prefer Merge-Time Handling When

Letting agents merge and resolve conflicts at the end is still reasonable when:

- tasks are clearly isolated
- expected overlap is low
- tasks are small and quick
- the repo’s test suite is good enough to catch accidental integration breakage
- the operational overhead of manifests/locking would exceed the likely savings

For example, a docs-only task and a package-local test task can usually run concurrently without special machinery.

### Practical Rule

Use prevention for **known overlap** and merge-time handling for **low-probability overlap**.

That keeps mato simple while still solving the real problem.

---

## Recommended Proposal

### Recommendation

For mato, the best conflict-prevention design is:

1. **Add task manifests with `affects:` frontmatter**
2. **Add `waiting/` plus host-side overlap detection**
3. **Run overlapping tasks sequentially**
4. **Publish task-authoring guidelines that encourage workspace partitioning**
5. **Always run post-merge validation on the target branch**
6. **Optionally add advisory file locks later for high-contention files or manifest escapes**

### Why This Is the Best Fit

This approach matches mato’s existing architecture:

- shared filesystem state under `.tasks/`
- host-managed queue transitions
- multiple independent mato processes
- autonomous agents that are better guided by the host than relied upon for strict coordination

### Suggested Rollout

#### Phase 1: Host-managed prevention
- add YAML `affects:` metadata
- add `waiting/`
- teach the host to keep overlapping tasks out of `backlog/`

#### Phase 2: Operational hardening
- add post-merge validation
- add task templates and authoring guidelines for partitioned work

#### Phase 3: Fine-grained advisory protection
- add `.tasks/file-locks/`
- provide a helper command for acquire/release
- update agent instructions to use it for high-contention or newly discovered files

---

## Final Assessment

If mato wants to prevent merge conflicts **before they happen**, the strongest answer is not “teach agents to resolve conflicts better.” It is:

- let the **host** predict overlap early,
- only run overlapping work sequentially,
- and use tests to catch the semantic conflicts that scheduling cannot predict.

In short:

- **Best primary strategy:** task-level manifests + host scheduling
- **Best enforcement:** sequential mode for overlapping tasks
- **Best safety net:** post-merge validation
- **Best optional enhancement:** advisory file locks

That combination preserves most of mato’s parallelism while removing the highest-value conflict cases before agents waste time on them.
