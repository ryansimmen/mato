# Make `depends_on` Authoritative Across Scheduling

> **Status: Implemented** — This proposal has been fully implemented.
> The text below describes the implemented design; see the source code for the
> current behavior.

`depends_on` currently has two different meanings depending on where a task
file sits. Tasks in `waiting/` are dependency-gated, but a task placed in
`backlog/` can still be claimed even when its declared dependencies are not
satisfied. That makes directory placement part of dependency truth and creates
a footgun: the same frontmatter can be enforced or ignored depending on which
folder the file happens to be in.

This is not limited to manual misplacement. Many normal flows can put a task
back into `backlog/` without re-checking dependencies first: orphan recovery,
stuck-task recovery, review rejection, merge requeue, `mato retry`, and other
rollback paths. The bug is therefore systemic, not just operator error.

## Goal

Make `depends_on` authoritative everywhere. A task must not be claimable until
every declared dependency resolves to an unambiguous task in `completed/`,
regardless of whether the file starts in `waiting/`, is requeued to
`backlog/`, or is moved manually by an operator.

## Non-goals

- no changes to `depends_on` syntax or matching rules
- no change to the rule that only `completed/` satisfies dependencies
- no auto-promotion of entire dependency chains in one reconcile pass
- no change to `affects` conflict behavior for runnable backlog tasks

## Decision

Use `waiting/` as the single canonical state for dependency-blocked tasks.
If a backlog task has unsatisfied or ambiguous dependencies, the host should
move it back to `waiting/` during reconciliation instead of leaving it in
`backlog/` and trying to hide it from `.queue`.

Why this is the right shape:

- it keeps directory semantics easy to explain: `waiting/` means
  dependency-blocked, `backlog/` means runnable except for `affects` deferral
- it reuses the existing dependency diagnostics, status output, and promotion
  flow instead of creating a second hidden blocked state inside `backlog/`
- it keeps `.queue` simple: it continues to mean "runnable backlog after
  `affects` exclusions", not "backlog minus several invisible filters"
- it gives operators a self-healing path for misfiled tasks instead of
  silently ignoring frontmatter

## Intended behavior

- A task in `backlog/` with unmet `depends_on` entries is not claimable.
- Reconcile demotes such tasks from `backlog/` to `waiting/` before manifest
  generation.
- A task only returns to `backlog/` through the normal waiting-to-backlog
  promotion path once every dependency is satisfied and no active `affects`
  overlap blocks it.
- `SelectAndClaimTask(...)` still re-checks dependency satisfaction before
  moving a task to `in-progress/` so stale indexes, manual edits, or a stale
  `.queue` file cannot bypass the invariant.
- helpers that answer "is work available?" or derive runnable backlog order
  must reflect the same dependency gate rather than raw backlog presence.
- Dependency satisfaction continues to mean: each `depends_on` entry matches
  exactly one task already in `completed/` by explicit `id` or filename stem.
- Dependencies referencing tasks in `waiting/`, `backlog/`, `in-progress/`,
  `ready-for-review/`, `ready-to-merge/`, or `failed/` remain blocked.
- Ambiguous completed-ID matches remain blocking, not partially satisfied.
- If a task is both dependency-blocked and `affects`-overlapping, dependency
  blocking wins and the task belongs in `waiting/`, not deferred `backlog/`.

## Design

### 1. Add a read-only dependency gate for backlog tasks

Introduce a helper that can answer "are this task's dependencies currently
satisfied?" for any indexed task, not just tasks already in `waiting/`. It
should reuse the same completed-ID, known-ID, and ambiguous-ID rules as
`DiagnoseDependencies()` so reconcile, claim selection, and status all speak
the same language.

This helper should be able to distinguish:

- satisfied
- blocked by another waiting or backlog task
- blocked by an external non-completed task state
- blocked by an unknown dependency
- blocked by an ambiguous completed-ID match

It does not need to treat backlog tasks as graph nodes for promotion ordering.
It only needs to decide whether a task is allowed to remain in `backlog/`.

### 2. Demote misplaced backlog tasks during reconcile

Extend `ReconcileReadyQueue(...)` with an early pass over `backlog/`:

- skip malformed backlog files that are already quarantined elsewhere in
  reconcile
- for each parseable backlog task with non-empty `depends_on`, evaluate
  dependency satisfaction
- if any dependency is unsatisfied, atomically move the task from `backlog/`
  to `waiting/`
- emit a clear warning explaining why the task was demoted

If any backlog task is moved, rebuild the index before continuing with
duplicate detection, cycle handling, waiting-task diagnostics, and
waiting-to-backlog promotion. That keeps the rest of the reconcile pass working
from a coherent snapshot. The rebuild should happen at most once per reconcile
cycle, not once per demoted task.

This early demotion pass is what closes the correctness gap for all of the
non-dependency backlog entry paths. It centralizes the fix instead of trying to
teach each requeue or rollback path to reimplement dependency logic.

### 3. Keep claim-time enforcement as a hard safety net

`ReconcileReadyQueue(...)` should do the normal cleanup, but
`SelectAndClaimTask(...)` must also enforce the invariant. Before claiming a
backlog candidate:

- evaluate its dependencies using the same helper or equivalent indexed data
- if dependencies are no longer satisfied, move the task back to `waiting/`
  and continue
- never allow a dependency-blocked task to reach `in-progress/`

This closes the race where `.queue` is stale, a task is edited manually between
reconcile and claim, or a caller invokes claim selection without a fresh
reconcile pass.

The implementation should reuse the existing indexed claim path rather than add
an unrelated second code path. `SelectAndClaimTask(...)` already accepts a
`*PollIndex`, and the runner already builds that snapshot immediately before
reconcile and claim selection.

### 4. Use one shared effective runnable backlog view

With demotion-to-waiting as the chosen behavior:

- `.queue` continues to list runnable backlog tasks only
- `WriteQueueManifest(...)` does not need a second dependency-specific
  exclusion model
- `mato status` continues to show dependency-blocked tasks in the waiting
  section and conflict-blocked tasks in the deferred backlog section
- queue-only preflight checks via `mato doctor --only queue,tasks,deps` and
  `--dry-run` should diagnose misplaced dependency-blocked backlog tasks
  explicitly in read-only mode; they must not present those tasks as runnable
  just because the current snapshot still shows them in `backlog/`

Read-only views need one explicit shared model. In the mutating runner path,
reconcile can physically move blocked tasks back to `waiting/` before
`WriteQueueManifest(...)` and claim selection run. In read-only flows such as
`mato status`, queue-only doctor runs (`mato doctor --only queue,tasks,deps`),
`--dry-run`, and `HasAvailableTasks(...)`, the host cannot rely on that move
having already happened.

Implement one shared "effective runnable backlog" helper that derives:

- backlog tasks that are not dependency-blocked
- backlog tasks that are not `affects`-deferred
- the same ordering claim selection uses

That helper should back status, dry-run, and availability checks so they all
answer the same question. As part of this change, `HasAvailableTasks(...)`
stops being a raw backlog existence check and becomes a dependency-aware
availability query.

Operator-facing requeue paths such as `RetryTask(...)` may also warn when a
task is being placed into `backlog/` even though it will be demoted on the next
reconcile pass, but correctness must not depend on such warnings.

### 5. Preserve existing scheduling semantics

This proposal should not change any of the current rules that are already
correct:

- only `completed/` satisfies dependencies
- promotion remains one hop per reconcile cycle
- `affects` conflicts still keep tasks in `backlog/` but exclude them from
  `.queue`
- cycle handling and duplicate waiting-ID handling remain centered on
  `waiting/`

A backlog task that depends on another non-completed backlog task should
therefore be demoted to `waiting/`; it must not become runnable just because
its dependency is already present somewhere else in the queue.

## Test plan

Add coverage for:

- backlog task with unmet dependency is moved to `waiting/` during reconcile
- backlog task with dependency on `in-progress/`, `failed/`, or an unknown ID
  is demoted and reported as blocked
- backlog task with an ambiguous completed-ID match is demoted and not
  claimable
- backlog task still listed in a stale `.queue` file is rejected by claim-time
  enforcement and cannot reach `in-progress/`
- a demoted task becomes promotable again only after its dependencies are
  completed
- `mato retry` of a task with unmet dependencies results in the task returning
  to `waiting/` on the next reconcile pass
- existing waiting-task promotion behavior remains unchanged
- conflict-deferred backlog tasks still stay in `backlog/` and are excluded
  only for `affects`, not for dependencies
- a task that is both dependency-blocked and `affects`-overlapping is moved to
  `waiting/`, not left in `backlog/` as deferred
- read-only views (`mato status`, `--dry-run`, `HasAvailableTasks(...)`) do
  not report dependency-blocked backlog tasks as runnable
- queue-only doctor runs via `mato doctor --only queue,tasks,deps` surface the
  same effective dependency-blocked state as status and dry-run
- status, doctor, and dry-run output continue to present dependency-blocked
  tasks through the waiting model

## Documentation updates

Update `README.md`, `docs/task-format.md`, `docs/architecture.md`, and
`docs/configuration.md` to make the invariant explicit:

- `depends_on` is authoritative regardless of directory placement
- `waiting/` is the canonical dependency-blocked state
- `backlog/` is for runnable tasks plus `affects`-deferred tasks, not
  dependency-blocked tasks
- manual placement in `backlog/` does not override dependency rules
- queue-focused read-only validation should remain aligned with the documented
  preflight command `mato doctor --only queue,tasks,deps`

## Acceptance criteria

This proposal is complete when:

- no task with unsatisfied `depends_on` can be claimed from `backlog/`
- reconcile automatically returns misplaced dependency-blocked tasks to
  `waiting/`
- claim-time enforcement closes the stale-manifest and manual-edit race
- read-only views and availability checks do not over-report runnable backlog
  work
- docs describe one unambiguous model for `waiting/`, `backlog/`, and `.queue`

## Verification

Run `go build ./... && go test -count=1 ./...`.
