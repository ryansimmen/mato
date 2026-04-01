# Proposal: Bounded Execution Mode (`--once` / `--until-idle`)

## 1. Goal

Add flags to `mato run` that process the current queue and exit deterministically
instead of polling forever. This opens mato to CI/CD pipelines, cron jobs, and
scripted workflows that need a clean host-run lifecycle instead of an
unbounded daemon.

Today `mato run` is a long-running polling loop that exits only on `SIGINT` /
`SIGTERM`. The only bounded mode is `--dry-run`, which validates queue setup
without launching containers. There is no way to say "process available work,
then exit cleanly."

Bounded execution does not change mato's queue semantics by itself: claim,
review, and merge phases still run in their normal order. It only gives those
flows a deterministic exit contract.

## 2. Problem

- Ephemeral CI runners (GitHub Actions, GitLab CI) are penalized or killed for
  unbounded background processes. Deterministic exit is a standard requirement
  for embedding task runners in automation.
- Cron-style orchestration ("process the queue every hour") requires a bounded
  run that exits with a meaningful status code.
- Script-driven workflows need a way to say "do all available work, then let me
  inspect the result before continuing."
- Running `mato run` in the background and killing it externally works but loses
  the clean exit contract (merge draining, lock cleanup).

## 3. Current Code State

As of the current codebase:

- `pollLoop` in `internal/runner/runner.go` already computes `didWork` and a
  limited `isIdle` signal on every iteration.
- The loop currently exits only on `ctx.Done()` (signal). No mode flag exists.
- `--dry-run` validates without executing, demonstrating precedent for bounded
  run modes.
- `RunOptions` in `internal/runner/runner.go` already carries all resolved
  configuration; adding a mode enum is straightforward.
- The current `isIdle` check does **not** track claimable backlog explicitly.
  It only looks at pause state, whether a task was claimed this iteration, and
  whether review / merge work remains.
- Review selection runs from the iteration's pre-claim poll snapshot, so a task
  pushed to `ready-for-review/` by `runOnce(...)` is not reviewed until a later
  iteration.

## 4. Scope

### In scope

- Add `--once` flag to `mato run`: execute exactly one poll iteration, then
  exit.
- Add `--until-idle` flag to `mato run`: keep polling until no claimable
  backlog, no pending reviews, and no ready-to-merge tasks remain, then exit.
- `--once` and `--until-idle` are mutually exclusive with each other and with
  `--dry-run`.
- Exit code `0` when bounded execution finishes without host-side
  infrastructure errors; non-zero when startup fails or the bounded run hits a
  host/runtime failure.
- Documentation updates: README, `docs/configuration.md`, `docs/architecture.md`.
- Unit tests for the new exit conditions.
- Integration tests verifying bounded runs complete and exit.

### Out of scope

- Parallel agent execution or multi-slot scheduling.
- New queue directories or scheduler semantics.
- Timeout flags for bounded modes (CI runners already have their own timeouts).
- Webhook or notification on exit.

## 5. Design

### 5.1 Command shape

```text
mato run [--repo PATH] [--branch NAME] [--once | --until-idle] [existing flags...]
```

Behavior:

- `--once`: run exactly one `pollIterate` cycle, then return. This means “one
  host poll iteration,” not “drain one task all the way to completion.” The
  iteration may claim at most one backlog task, may process one existing review
  from the pre-iteration snapshot, and may process ready merges. If no work is
  available, it still performs one housekeeping / reconcile pass and exits with
  code `0`.
- `--until-idle`: keep running `pollIterate` until the bounded-idle predicate is
  true (defined below), then exit with code `0`.
- `--dry-run` remains a separate CLI-routed code path that calls
  `runner.DryRun(...)` instead of `runner.Run(...)`. Mutual exclusivity between
  `--dry-run`, `--once`, and `--until-idle` is enforced in
  `cmd/mato/main.go`; `RunMode` only models the execution modes inside
  `runner.Run(...)` (`Daemon`, `Once`, `UntilIdle`).
- Both modes perform the same startup sequence as the normal long-running mode
  (branch setup, queue creation, lock registration, cleanup).
- Both modes perform the same cleanup on exit (lock removal, deferred cleanup).
- Newly finished work is **not** re-reviewed in the same iteration. If `--once`
  claims a task and the agent pushes it to `ready-for-review/`, that task is
  left for a later run.

Example from a single backlog task to merge:

1. First `mato run --once`: claim + run, leaving the task in `ready-for-review/`
2. Second `mato run --once`: review, leaving the task in `ready-to-merge/`
3. Third `mato run --once`: merge

For automation that wants to drain work end-to-end, `--until-idle` is the
better default.

### 5.2 Exit contract

The existing `isIdle` predicate is not sufficient for bounded mode. Today it
accounts for:

- whether a task was claimed this iteration
- `hasReviewTasks`: tasks in `ready-for-review/`
- `hasReadyMerge`: tasks in `ready-to-merge/`
- `pauseActive`: repo is paused

For bounded execution we need a stronger post-iteration predicate based on the
filesystem state after the iteration has finished.

This bounded-idle predicate intentionally diverges from today's lightweight
heartbeat `isIdle` check. The current heartbeat treats `pauseActive` as always
non-idle; bounded execution needs a more precise definition so a paused but
otherwise empty queue can still exit.

Define:

- `hasClaimableBacklog`: at least one backlog task is immediately claimable for
  a new work run after reconcile, dependency filtering, affects deferral,
  failed-dir exclusion, and retry-cooldown checks.
- `hasReviewTasks`: at least one parseable review task remains whose review
  retry budget is not exhausted.
- `hasReadyMerge`: at least one task remains in `ready-to-merge/`.
- `pauseActive`: the repo is paused.

For `--until-idle`, exit when all of the following are true:

- no claimable backlog tasks
- no pending review tasks
- no ready-to-merge tasks
- and either the repo is not paused, or it is paused with no pending actionable
  work

Paused semantics:

- paused + pending claimable / review / merge work => not idle
- paused + no claimable / review / merge work => idle and allowed to exit

For `--once`, exit after one full `pollIterate` completes regardless of
remaining work.

Implementation note: `hasClaimableBacklog` must not be approximated with the
current `claimedTask` boolean or with raw backlog file presence. Backlog tasks
in retry cooldown are not immediately actionable and should not keep
`--until-idle` running forever.

If all remaining backlog tasks are currently in retry cooldown, `--until-idle`
treats the queue as idle and exits. A later invocation can process those tasks
once their cooldown has elapsed.

Implementation note: the cleanest way to compute `hasClaimableBacklog` is to
factor a pure "candidate is immediately claimable" helper out of
`queue.SelectAndClaimTask(...)` so the idle check and the claim path share the
same cooldown / retry-exhaustion logic without side effects.

Implementation note: an explicit `hasInProgressTasks` check is not required in
the current design because `pollClaimAndRun(...)` is synchronous; by the time
`pollIterate(...)` returns, no work agent is still running. If mato adds
parallel or asynchronous execution later, bounded idle must incorporate
in-flight work explicitly.

Exit-status rule:

- exit `0` when the bounded run completes without host-side infrastructure
  failures
- task-level outcomes such as agent failure records, review rejection, retry
  exhaustion, or merge-conflict requeue remain normal queue transitions and do
  **not** change the process exit code by themselves
- non-zero is reserved for startup failures and host/runtime poll failures such
  as Docker/setup failures or poll-cycle infrastructure errors already tracked
  by `pollHadError` (or equivalent future bounded-run error accounting)

### 5.3 Changes

| File | Change |
| --- | --- |
| `cmd/mato/main.go` | Add `--once` and `--until-idle` flags to `run`, keep `--dry-run` as a separate route to `runner.DryRun(...)`, and validate mutual exclusivity |
| `internal/runner/runner.go` | Add `RunMode` to `RunOptions` (enum: `RunModeDaemon`, `RunModeOnce`, `RunModeUntilIdle`), track bounded-run infrastructure failures, and check `RunMode` in `pollLoop` to decide whether to exit after iteration |
| `internal/queue/claim.go` and/or `internal/queue/runnable_backlog.go` | Factor a reusable side-effect-free claimability check so `--until-idle` uses the same cooldown / retry / exclusion rules as claim selection |
| `README.md` | Document the new flags |
| `docs/configuration.md` | Add flags to the CLI reference table |
| `docs/architecture.md` | Document the bounded execution exit contract |

### 5.4 Estimated scope

~200–250 LOC of production code including flag wiring, `RunMode`, bounded-run
error aggregation, and claimable-idle checks. ~150–250 LOC of tests.

## 6. Risks

- **Exit semantics ambiguity**: if `--until-idle` does not drain reviews and
  merges before exiting, CI users will be surprised by orphaned work. The design
  above addresses this by including all pending work in the idle check.
- **`--once` expectation mismatch**: users may read "once" as "finish one task"
  rather than "run one poll iteration." The CLI help and docs must state that
  newly completed work may advance only to `ready-for-review/` in that run.
- **Pause interaction**: a paused repo with pending actionable work must not be
  considered idle, but a paused and otherwise empty queue should still be able
  to exit. The proposal adopts that narrower rule explicitly.
- **Cooldown interaction**: retry-cooldowned backlog tasks are not immediately
  actionable. If bounded idle treats them as runnable backlog, `--until-idle`
  may spin until the cooldown expires. The design therefore uses
  `hasClaimableBacklog`, not raw backlog presence.
- **Exit status semantics**: today many runtime problems are warnings rather
  than returned errors. Bounded mode needs deliberate aggregation so CI status
  reflects host/runtime failures without treating normal queue outcomes as
  process failures.
- **Future interaction with parallel execution**: if parallel agent execution is
  added later, the exit contract must account for in-flight concurrent agents.
  Bounded execution's clean exit-contract design is prerequisite scaffolding for
  that future work.

## 7. Evidence

- **Repo evidence** (observed): `pollLoop` already computes `didWork` and
  a limited `isIdle` signal per iteration in `internal/runner/runner.go`.
- **Repo evidence** (observed): `--dry-run` establishes precedent for bounded
  run modes in `docs/configuration.md`.
- **Repo evidence** (observed): review selection uses the iteration's existing
  poll snapshot, so newly pushed `ready-for-review/` tasks are not reviewed in
  the same iteration.
- **External evidence**: ephemeral CI runners (GitHub Actions, GitLab CI) require
  deterministic process exit for job completion.
- **Debate evidence**: unanimous recommendation across three analytical lenses
  (Simplicity & Scope Discipline, Architecture & Technical Risk, User Value &
  Product Direction) in a structured feature research debate.
