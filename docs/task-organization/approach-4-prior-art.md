# Approach 4: prior art for task dependencies and ordering

This document reviews how established systems model task dependencies, readiness, ordering, retries, and failure propagation, then translates the useful parts into design guidance for `mato`.

## Context: what mato does today

From `main.go` and `task-instructions.md`, mato currently behaves like a simple file queue:

- task files live under `.tasks/`
- agents only look at `.tasks/backlog/`
- claiming is done by atomic `mv` from `backlog/` to `in-progress/`
- completion is represented by moving to `completed/`
- failure is represented by moving back to `backlog/` with appended `<!-- failure: ... -->` metadata, or eventually to `failed/`
- the host process recovers orphaned tasks and cleans stale agent locks

Two consequences matter for dependency design:

1. **The agent is intentionally dumb about scheduling.** The prompt explicitly tells it to only inspect `backlog/` and claim one file.
2. **The host already owns queue semantics.** That means dependency resolution should also live on the host side, not inside the agent prompt.

That is the right split. LLM agents in Docker should get a single, ready-to-run task. The host should decide whether a task is runnable.

---

## 1. Make / Makefile

### How dependencies are declared

Make declares dependencies directly on targets:

```make
build: compile test
```

Make also supports **order-only prerequisites**:

```make
artifact: input | output-dir
```

Normal prerequisites mean both:

- `artifact` must wait for `input`
- if `input` changes, `artifact` is out of date

Order-only prerequisites mean:

- `artifact` must wait for `output-dir`
- but changes to `output-dir` do not force a rebuild

### How the system determines “ready” tasks

Make starts from a goal target, recursively resolves prerequisites, and runs targets in a topological order implied by the dependency graph. A target is ready when its prerequisites have been updated or are already up to date.

With `-j`, Make can run multiple ready targets in parallel, bounded by job slots.

### How it handles failures and retries

- a failing recipe usually stops the build
- `-k/--keep-going` lets independent branches continue
- Make has no real built-in retry model
- “up to date” is largely timestamp- or file-state-based, not attempt-based

### What mato can borrow

1. **Explicit direct dependencies.** A task should name the tasks it directly depends on.
2. **Topological scheduling.** The host can compute the ready set by checking whether all prerequisites are complete.
3. **Order-only dependencies.** This is a very useful idea for mato. Some tasks only need sequencing, not semantic dependency. Example: “publish release notes” may need to happen after “cut release branch”, but a retry on release-note wording should not invalidate the branch-cutting task.
4. **Bounded parallelism.** Like Make job slots, mato should cap concurrent ready-task execution.

### What does not translate well to mato

- timestamp-based rebuild logic
- implicit rules and suffix rules
- the idea that a downstream task becomes stale whenever an input changes

Mato tasks are usually **one-shot knowledge work**, not deterministic artifact builds. The right question is not “is this out of date?” but “have the prerequisite tasks completed successfully for this queue run?”

### Mato lesson

Borrow Make’s **graph and ordering model**, not its file freshness model.

**Primary references:** GNU Make manual on goals, prerequisite types, and parallel execution.

---

## 2. GitHub Actions

### How dependencies are declared

GitHub Actions uses `jobs.<job_id>.needs`:

```yaml
jobs:
  build:
  test:
    needs: build
  deploy:
    needs: [build, test]
```

It also supports **matrix strategies**, where one logical job expands into many concrete jobs.

### How the system determines “ready” tasks

A job is ready when:

- all jobs in `needs` have finished
- they succeeded, unless an `if:` condition says otherwise
- its matrix expansion has been materialized into concrete job instances

This creates a DAG of jobs, with fan-out and fan-in handled naturally.

### How it handles failures and retries

- by default, if a needed job fails or is skipped, downstream jobs are skipped
- `if: ${{ always() }}` can override that behavior
- matrix jobs support `fail-fast`
- `continue-on-error` allows selective tolerance
- retries are not the main DAG mechanism; failure policy is mostly about propagation and cancellation

### What mato can borrow

1. **`needs` as the default dependency model.** Simple and explicit.
2. **Default rule: downstream runs only after upstream success.** This should be mato’s default too.
3. **Optional trigger rules.** A very small subset is useful for mato:
   - `all_success` (default)
   - `all_done` (cleanup/reporting tasks)
4. **Matrix expansion as a host-side preprocessing step.** If mato ever needs “do the same task for N services/files/modules”, the host should expand that into multiple task files plus a fan-in task, rather than making agents interpret a matrix.
5. **Fail-fast on sibling work.** If a fan-out branch shows the whole batch is invalid, mato may want an option to stop scheduling peers.

### What does not translate well to mato

- expression-heavy YAML conditions
- passing outputs through a runtime expression system
- complex matrix include/exclude rules in the queue itself

Those are powerful, but they make scheduling rules harder to understand. Mato should prefer simpler, host-validated metadata.

### Mato lesson

Use Actions as a model for a **simple DAG with fan-out/fan-in and clear failure propagation**, but avoid importing the whole expression language.

**Primary references:** GitHub Actions docs on `needs` and matrix strategies.

---

## 3. Taskfile (`taskfile.dev`)

### How dependencies are declared

Taskfile uses a `deps:` field:

```yaml
tasks:
  build:
    deps: [assets]
```

A key detail from Taskfile’s docs: **dependencies run in parallel**. If you need serial behavior, you should call tasks explicitly in order rather than list them as deps.

### How the system determines “ready” tasks

A task is ready when its dependencies have been run successfully, subject to Taskfile’s skip/up-to-date checks (`status`, `sources`, `generates`, `method`, `run`).

Because `deps:` is parallel by design, Taskfile treats dependencies as prerequisite branches, not as an ordered list.

### How it handles failures and retries

- failed dependencies stop the parent task
- `failfast` can stop further dependency execution sooner
- Taskfile supports several “should this run?” mechanisms, but not a rich workflow retry state machine

### What mato can borrow

1. **Do not overload dependency lists with sequencing.** `deps` should mean “prerequisites”, not “run these in this order”.
2. **Differentiate between graph edges and serial scripts.** For mato, that suggests separate concepts such as:
   - `depends_on`: hard prerequisite edges
   - `after`: order-only sequencing
3. **Keep the model human-sized.** Taskfile’s appeal is that its dependency model is easy to read.
4. **Optional deduplication semantics.** Taskfile’s `run: once` idea maps loosely to “do not schedule the same task twice for the same queue state.”

### What does not translate well to mato

- up-to-date checks based on source files and generated files
- assuming dependencies are short-lived local shell commands
- encouraging parallel deps that secretly rely on each other

LLM task agents are slower, less deterministic, and more failure-prone than shell commands. Mato needs stronger state tracking than Taskfile does.

### Mato lesson

Take Taskfile’s semantic distinction seriously: **dependencies are prerequisites, not an execution order list**.

**Primary references:** Taskfile guide on dependencies and Taskfile schema reference.

---

## 4. Bazel / Buck

### How dependencies are declared

Bazel and Buck use explicit rule declarations, usually with attributes like `deps`, `srcs`, and `data`. The important conceptual rule is:

- declare **direct** dependencies explicitly
- compute transitive closure in the scheduler
- do not rely on undeclared transitive dependencies

Bazel is especially explicit about the distinction between **actual dependencies** and **declared dependencies**. Correctness requires actual dependencies to be a subset of declared dependencies.

### How the system determines “ready” tasks

These systems build a dependency graph, then an execution/action graph. A node is ready when:

- its direct inputs are available
- upstream actions are complete
- it is known to need execution

They aggressively use graph knowledge to do incremental execution and parallel scheduling.

### How it handles failures and retries

- missing or undeclared dependencies are treated as correctness problems
- failed actions block downstream actions
- incremental reruns only re-execute affected parts of the graph
- some infrastructure-level retries may happen, but correctness comes from strict graph declaration, not from hope

### What mato can borrow

1. **Strict graph validation.** On the host, mato should reject:
   - unknown dependency IDs
   - self-dependencies
   - cycles
   - duplicate task IDs
2. **Direct dependencies only.** Each task should list only what it directly needs. The host computes the transitive closure.
3. **Separation of declaration from execution.** Task files describe the graph; the host scheduler computes readiness.
4. **Do not infer dependencies from task text.** Bazel/Buck work because dependencies are explicit. Mato should do the same.
5. **Graph-first thinking.** Scheduling should use a materialized DAG, not repeated directory scans plus filename heuristics.

### What does not translate well to mato

- full hermetic build semantics
- content-addressed remote caches
- action graphs with artifact invalidation
- exact reproducibility assumptions

Mato tasks are often editorial, design, or code-modification tasks involving judgment. That makes them much less like pure build actions.

### Mato lesson

The biggest lesson is not “be like a build system”; it is **be strict about explicit dependencies and graph validation**.

**Primary references:** Bazel docs on dependencies and Buck2 docs on key concepts, build files, and architecture.

---

## 5. Airflow / Prefect

### How dependencies are declared

Airflow declares edges explicitly (`>>`, `<<`, `set_upstream`, `set_downstream`). Prefect supports both explicit waiting and implicit dependency edges based on futures/data flow.

These systems treat a workflow as a DAG plus a scheduler state machine.

### How the system determines “ready” tasks

A task becomes ready when its upstream conditions are satisfied.

By default this usually means:

- all upstream tasks completed successfully

But these systems also support richer policies, such as cleanup/reporting work that should run after upstream completion even if one branch failed.

Airflow’s task-instance lifecycle is instructive:

- `none`
- `scheduled`
- `queued`
- `running`
- `success`
- `failed`
- `skipped`
- `upstream_failed`
- `up_for_retry`

Prefect adds a strong idea that downstream work waits on **state**, not just on the existence of upstream output.

### How it handles failures and retries

- per-task retry counts
- retry delays / backoff
- explicit state for “failed but retryable”
- downstream behavior based on trigger rules and upstream state

This is much richer than Make or Taskfile.

### What mato can borrow

1. **A richer host-side state model.** Mato’s filesystem layout can stay simple, but the scheduler should think in richer states:
   - `pending` or `blocked`
   - `ready`
   - `in_progress`
   - `completed`
   - `failed`
   - `upstream_failed`
   - `retry_scheduled`
2. **Trigger rules, but only a tiny subset.** Mato likely only needs:
   - `all_success` (default)
   - `all_done` (for cleanup/reporting)
3. **Retries belong to the scheduler.** Agents should not decide retry policy; the host should.
4. **Backoff matters.** Re-running a failing LLM task instantly is usually worse than waiting.
5. **State visibility matters.** Downstream tasks should be blocked because of a known upstream state, not because they happen not to be in `backlog/`.

### What does not translate well to mato

- schedule calendars and data intervals
- sensors and long-lived waiting tasks
- Python-native DAG authoring inside the queue
- orchestration features designed for a service/UI-first platform

### Mato lesson

Airflow/Prefect show that the real power is in the **scheduler state machine**, not just the graph syntax.

**Primary references:** Airflow docs on DAGs and tasks; Prefect docs on tasks, concurrency, and retries.

---

## 6. Kubernetes Jobs

### How dependencies are declared

Kubernetes `Job` does **not** natively model inter-job DAG dependencies the way Actions or Airflow do. Sequencing is usually handled by a higher-level controller, an external orchestrator, or inside a single Pod via **init containers**.

Init containers are a useful dependency pattern:

- they run sequentially
- each must succeed before the next starts
- the main container does not start until all init containers succeed

### How the system determines “ready” tasks

For a Job:

- the controller tracks active, succeeded, and failed Pods
- the Job is complete when required completions are reached
- retries happen until `backoffLimit` is exhausted

For init containers:

- the next stage is ready only when the prior setup stage completed successfully

### How it handles failures and retries

- failed Pods are retried up to `backoffLimit`
- Job status tracks progress and outcome
- init container failure blocks main container startup
- Jobs can also be suspended and resumed

### What mato can borrow

1. **Setup vs main work.** Some mato tasks need preconditions checked on the host before agent launch. That is basically an init-container idea.
2. **Retry budgets should be explicit.** Mato already has a max retry count; this should become first-class task metadata eventually.
3. **Completion tracking should be explicit.** Do not infer “done” from absence; record state deliberately.
4. **Suspend/resume is useful.** A task with unresolved dependencies or manual hold should be suspendable without looking like a failure.

### What does not translate well to mato

- Pod-level restarts and container lifecycle semantics
- cluster scheduling concepts
- using restart behavior as the main dependency mechanism

Kubernetes is strong on execution control, but weak as direct prior art for inter-task DAGs unless another controller is layered on top.

### Mato lesson

Kubernetes Jobs are most relevant as a model for **retry budgets, completion accounting, and staged preconditions**, not as a full dependency language.

**Primary references:** Kubernetes docs on Jobs and init containers.

---

## 7. Simple file-based queues (Maildir, spool directories)

### How dependencies are declared

Traditional spool systems usually do **not** encode DAG dependencies in the queue primitive itself. They focus on reliable state transitions:

- temporary staging
- ready for pickup
- claimed/seen/processing
- done or archived

Maildir is the cleanest example:

- `tmp/` for incomplete writes
- `new/` for ready items
- `cur/` for claimed/seen items

General Unix spool directories follow the same spirit: put future work in a spool area, then let a worker claim and process it.

### How the system determines “ready” tasks

The main rule is simple: an item is ready when it has been atomically moved into the ready location.

This is the core file-queue trick:

- write in a staging location
- `rename`/`link` into the ready location only when complete
- workers claim with another atomic move

### How it handles failures and retries

Patterns vary, but common ones are:

- leave the claimed file in place for inspection
- move it back to ready after annotating failure
- move it to a dead-letter/error directory
- use lock files or per-item ownership markers
- use janitors to recover stale claimed work

### What mato can borrow

Mato already borrows a lot of this, and it should keep doing so:

1. **Atomic rename as the claim primitive.** This is excellent.
2. **Directory-based lifecycle states.** Easy to inspect and debug.
3. **Temporary/staging writes.** New tasks should appear atomically when fully written.
4. **A stale-work janitor.** Mato already has orphan recovery and stale lock cleanup; that is exactly right.
5. **Failure metadata stored with the item.** Also already present.

### What does not translate well to mato

- encoding ordering in filename sort order alone
- lock-file-heavy coordination when rename is enough
- expecting the queue directories themselves to represent a DAG

Spool queues are good at **state transitions**, not dependency reasoning.

### Mato lesson

Keep the file-queue mechanics. Add a host-side dependency layer above them.

**Primary references:** Maildir man page and Filesystem Hierarchy Standard documentation for `/var/spool`.

---

## Common patterns across these systems

Despite very different domains, these systems agree on a few things.

### 1. Dependencies should be explicit

Every successful system has an explicit declaration mechanism:

- `make`: prerequisites
- GitHub Actions: `needs`
- Taskfile: `deps`
- Bazel/Buck: `deps`
- Airflow: upstream/downstream edges

The systems that scale best are the ones that do **not** rely on implicit or inferred dependencies.

### 2. “Ready” is host/scheduler logic, not worker logic

Workers do work. Schedulers decide readiness.

That matches mato’s architecture. The agent should not inspect the whole queue and reason about graph state. The host should present it with only ready tasks.

### 3. The default trigger rule is “all prerequisites succeeded”

This is the dominant default almost everywhere. Cleanup/reporting exceptions exist, but they are exceptions.

### 4. Failure propagates downstream unless explicitly overridden

A failed prerequisite usually means downstream work is:

- skipped
- blocked
- marked upstream-failed
- or delayed until retry succeeds

That is better than letting downstream tasks guess whether to continue.

### 5. Concurrency comes from the ready set, not from list order

The graph defines what can run. A scheduler may then choose among all ready nodes, often with a concurrency cap.

### 6. Strict validation prevents subtle bugs

Build systems especially show that undeclared dependencies are poison. For mato, silent transitive assumptions would produce flaky queue behavior and confusing agent failures.

### 7. State needs to be richer than just “waiting/running/done”

Airflow, Prefect, and Kubernetes all show the value of distinguishing:

- blocked by dependency
- queued/ready
- running
- retry pending
- permanently failed
- upstream failed

### 8. Atomic state transitions matter

File-based systems teach the reliability lesson: use atomic rename and append-only metadata where possible.

---

## Best practices for a file-based task queue with dependencies

For mato specifically, the best hybrid looks like this:

### 1. Keep `backlog/` as the **ready queue**

This is the most important compatibility point with the current agent prompt.

If agents only scan `backlog/`, then tasks with unmet dependencies should **not** be placed there.

That implies a host-side distinction between:

- tasks known to the system
- tasks ready for agents to claim

The simplest approach is to add another host-managed state such as:

- `.tasks/pending/` or `.tasks/blocked/` for tasks waiting on dependencies
- `.tasks/backlog/` for ready tasks only

### 2. Use machine-readable metadata, but keep it agent-safe

The current prompt already tells the agent to ignore top-of-file `<!-- ... -->` metadata. That is a good compatibility hook.

Mato can use that for host-parsed dependency metadata without making prompts more complex.

Example shape:

```markdown
<!--
mato:
  id: add-task-graph
  depends_on:
    - add-task-metadata-parser
    - add-queue-state-index
  after:
    - docs-outline
  priority: 50
  max_retries: 3
  trigger: all_success
-->

# Add task graph support

Implement ...
```

A YAML front matter block could also work, but comment metadata fits the current conventions better.

### 3. Separate hard dependencies from ordering-only edges

This is one of the clearest cross-system lessons.

Recommended host-side concepts:

- `depends_on`: downstream may run only if upstream completed successfully
- `after`: downstream may run only after upstream finished, but upstream is not treated as a semantic input

Start with `depends_on` if you want the smallest MVP. Add `after` only if real use cases appear.

### 4. Validate the graph before scheduling

On every scheduler pass, or at least whenever tasks are added/changed, mato should validate:

- all task IDs are unique
- all referenced dependencies exist
- no task depends on itself
- no cycles exist
- trigger rules are valid

Failures here should be surfaced as queue configuration errors, not runtime agent failures.

### 5. Compute readiness entirely on the host

For each known task, the host should compute something like:

- `ready`
- `blocked_on_dependencies`
- `in_progress`
- `completed`
- `failed`
- `upstream_failed`
- `retry_wait`

Only `ready` tasks go into `backlog/`.

### 6. Use deterministic ordering within the ready set

Most DAG systems say “dependencies determine what can run”; a scheduler still needs a tie-breaker among ready tasks.

For mato, a practical order is:

1. highest explicit priority
2. oldest creation time / queue insertion time
3. lowest topological depth or explicit user order
4. filename as final stable tie-breaker

That gives predictability without encoding dependencies into filenames.

### 7. Keep retries host-controlled and visible

Retry policy should not live in prompts. The host should decide:

- max retries
- backoff delay
- whether a failure is retryable
- when a task becomes permanently failed

Downstream tasks should remain blocked until the dependency is either completed or permanently failed.

### 8. Support fan-out and fan-in, but as explicit tasks

Borrowing from GitHub Actions and Airflow:

- one task may unlock many ready tasks
- one aggregate task may depend on many prerequisites

If mato later wants matrix-like workflows, the host should expand them into ordinary task files before agents see them.

---

## Anti-patterns to avoid

### 1. Letting agents resolve dependencies themselves

This violates mato’s current design and will produce ambiguous prompts. Agents should receive one task, not perform graph planning.

### 2. Using filename sort order as the dependency system

`001-setup.md`, `002-build.md`, `003-test.md` is fine as a visual convention, but it is a weak scheduler. It breaks under fan-out, fan-in, retries, and partial failure.

### 3. Relying on transitive dependencies without declaring direct ones

This is the Bazel lesson. If task `C` directly needs work from `A`, it should say so, even if `B` also depends on `A`.

### 4. Mixing dependency state with claim state

“Not in backlog” should not be the only representation of “blocked”. Otherwise you cannot tell whether a task is blocked, retry-delayed, misconfigured, or simply missing.

### 5. Encoding too much logic in a mini language

GitHub Actions-style expressions are powerful, but they quickly become opaque. Mato should prefer a tiny, validated schema.

### 6. Making retries immediate and blind

An LLM task that just failed due to unclear requirements, repo state, or flaky tooling often needs backoff or human inspection, not an instant identical rerun.

### 7. Treating order-only edges as hard dependencies by default

That forces unnecessary blocking and makes workflows brittle.

### 8. Rebuilding build-system semantics wholesale

Mato does not need artifact hashing, sandboxed action graphs, or a workflow DSL. The right design is smaller.

---

## Recommended design principles for mato

### 1. Keep the agent contract simple

Agents should continue to do exactly one thing:

- claim a ready task from `backlog/`
- execute it
- move it to `completed/` or back for retry/failure

No DAG logic belongs in the prompt.

### 2. Make the host the source of truth for graph state

The host should scan all known task metadata, build a DAG, validate it, and decide which tasks belong in `backlog/`.

### 3. Add explicit task IDs and direct dependencies

A filename is not enough. Dependencies should target stable IDs, not display names.

### 4. Treat `backlog/` as ready-only, not all-pending

This is the cleanest way to preserve today’s agent behavior.

### 5. Start with one default trigger rule

For the first version, use:

- `depends_on` + `all_success`

Then, only if needed, add:

- `after`
- `all_done`

### 6. Add one more scheduler-visible state before “ready”

Whether it is a directory (`pending/`, `blocked/`) or a host index, mato needs a distinction between:

- known but not runnable
- runnable now

Without that distinction, dependencies will remain awkward.

### 7. Validate aggressively, fail early

Bad dependency metadata should fail queue validation before any container is launched.

### 8. Make ordering deterministic but secondary

Dependencies decide correctness. Priority and ordering only decide which ready task goes first.

### 9. Keep state transitions atomic and inspectable

Retain the Maildir/spool model:

- atomic move into ready queue
- atomic claim to in-progress
- explicit completed/failed locations
- append-only failure metadata where practical

### 10. Prefer explicit expansion over clever runtime behavior

If users want a batch of similar tasks, generate multiple explicit task files and a join task. Do not ask agents to interpret a batch spec dynamically.

---

## Practical recommendation: an MVP dependency model for mato

If mato wants the smallest useful system, the MVP should be:

1. **Metadata at the top of each task file** with:
   - `id`
   - `depends_on`
   - optional `priority`
   - optional `max_retries`
2. **Host-side DAG validation**
3. **A non-ready holding state** (`pending/` or `blocked/`)
4. **`backlog/` contains only ready tasks**
5. **Default trigger rule: all dependencies must be completed successfully**
6. **Downstream tasks of a permanently failed dependency become `upstream_failed` or remain blocked for manual intervention**

If mato wants one advanced feature beyond that, the best next feature is:

- **order-only sequencing** (`after`), borrowed from Make

That adds useful expressive power without requiring a full workflow language.

---

## Bottom line

The best prior art is a blend, not a copy:

- from **Make**: topological ordering and order-only prerequisites
- from **GitHub Actions**: explicit `needs`, fan-out/fan-in, simple failure propagation
- from **Taskfile**: do not confuse prerequisites with serial order
- from **Bazel/Buck**: strict direct dependency declaration and graph validation
- from **Airflow/Prefect**: scheduler-owned state and retry logic
- from **Kubernetes Jobs**: explicit retry budgets and staged preconditions
- from **Maildir/spool queues**: atomic file-state transitions and recovery

For mato, the right design is:

> **a host-managed DAG scheduler layered on top of the existing file queue, with agents only ever seeing ready tasks.**

That keeps the LLM prompt simple, preserves the nice operational properties of the current file-based queue, and adds the minimum dependency machinery needed for correct ordering.

---

## References consulted

- GNU Make manual: goals, prerequisite types, parallel execution  
  https://www.gnu.org/software/make/manual/
- GitHub Actions: using jobs in a workflow, matrix strategies  
  https://docs.github.com/en/actions/
- Taskfile guide and schema reference  
  https://taskfile.dev/docs/
- Bazel dependencies  
  https://bazel.build/concepts/dependencies
- Buck2 concepts and architecture  
  https://buck2.build/docs/
- Apache Airflow DAGs and tasks  
  https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/
- Prefect tasks, concurrency, retries  
  https://docs.prefect.io/v3/
- Kubernetes Jobs and init containers  
  https://kubernetes.io/docs/concepts/workloads/
- Maildir man page and FHS `/var/spool` docs  
  https://manpages.ubuntu.com/manpages/bionic/man5/maildir.5.html  
  https://refspecs.linuxfoundation.org/FHS_3.0/fhs/ch05s14.html
