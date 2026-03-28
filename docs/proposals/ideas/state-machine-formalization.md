# State Machine Formalization

**Priority:** High
**Effort:** Medium

## Goal

Centralize validation and execution of task queue state transitions so the queue
state diagram is explicit, testable, and reusable instead of being enforced by
scattered `AtomicMove` call sites.

## Why now

The current filesystem queue works, but its transition logic is spread across
many packages and many subtly different move+record+rollback sequences. That has
three growing costs:

- queue invariants live in programmer discipline instead of a validated core
- each new transition shape tends to duplicate error-handling and rollback logic
- future work such as richer task history or new queue states gets harder as the
  number of bespoke call sites grows

## Related proposals

- `add-log-command.md`: can and should ship independently. A future transition
  journal may enrich `mato log`, but `mato log` must not be blocked on this
  refactor.

## Success criteria

Success means:

- canonical task-file transitions between queue directories go through one
  validated core and a small number of thin specialized wrappers
- illegal edges fail clearly in tests and at runtime instead of silently
  depending on call-site correctness
- duplicated move+record+rollback logic shrinks at migrated call sites
- no user-facing queue model changes are required to get these benefits

## Problem

Task state transitions are implicit. The 7 queue states (`waiting`, `backlog`,
`in-progress`, `ready-for-review`, `ready-to-merge`, `completed`, `failed`) and
12+ transitions between them are scattered across 6+ files with 20+
independent `AtomicMove` call sites. Each call site independently re-implements
the move+record+rollback pattern with bespoke error handling and function-
variable hooks.

Examples of duplication:

- `claim.go` does `backlog -> in-progress`, then writes `claimed-by`, then rolls
  back if the write fails
- `task.go` does `in-progress -> ready-for-review`, writes a branch marker, then
  rolls back to `in-progress` if that post-move write fails
- `review.go` writes approval or rejection markers, then moves
  `ready-for-review -> ready-to-merge` or `ready-for-review -> backlog`
- `taskops.go` appends a merge failure record and moves
  `ready-to-merge -> backlog|failed`, while `moveTaskWithRetry` separately wraps
  `ready-to-merge -> completed` in retry logic
- `reconcile.go` and `review.go` repeatedly append terminal/cycle failure
  records and move tasks to `failed/` with nearly identical warning paths
- `cancel.go` moves a task to `failed/`, writes a cancelled marker, and rolls
  back if the write fails

Invalid transitions like `waiting -> completed` are prevented only by
programmer discipline, not by the type system or a validation layer. Nothing
enforces the state diagram documented in `docs/architecture.md`.

## Scope

### In scope

- A centralized transition-validation layer for canonical queue states
- A small shared core for validated task moves that still uses `AtomicMove`
  underneath
- Thin wrappers for the transition classes that need post-write, pre-write,
  rollback, warning, or retry behavior
- Incremental migration of existing call sites to the validated model
- A future hook point for best-effort transition journaling

### Out of scope

- New queue directories, frontmatter fields, or CLI flags
- Replacing the filesystem-backed queue with a database or in-memory scheduler
- A generic workflow engine or DSL
- Requiring a transition journal in the initial refactor
- Reclassifying higher-level business rules such as dependency checks,
  overlap checks, or retry-budget decisions as "state machine" logic

## What counts as a transition

This proposal is about task-file moves between the canonical queue directories:

- `waiting`
- `backlog`
- `in-progress`
- `ready-for-review`
- `ready-to-merge`
- `completed`
- `failed`

Examples that are in scope:

- `waiting -> backlog`
- `backlog -> in-progress`
- `in-progress -> ready-for-review`
- `ready-for-review -> backlog`
- `ready-to-merge -> completed`

Examples that are not themselves state transitions:

- deleting stale duplicate task copies
- writing message files, completion details, or verdict files
- deleting or recreating Git branches
- renaming a recovered orphan to a different filename within the same logical
  state

Not every `AtomicMove` in the codebase must be forced through the same
transition abstraction. The goal is to formalize queue-state changes, not to
wrap every file operation indiscriminately.

## Initial legal edge set

The first version should encode the legal queue edges mato already uses today.
At a minimum, that set is:

- `waiting -> backlog`
- `waiting -> failed`
- `backlog -> waiting`
- `backlog -> in-progress`
- `backlog -> failed`
- `in-progress -> backlog`
- `in-progress -> ready-for-review`
- `in-progress -> failed`
- `ready-for-review -> backlog`
- `ready-for-review -> ready-to-merge`
- `ready-for-review -> failed`
- `ready-to-merge -> backlog`
- `ready-to-merge -> completed`
- `ready-to-merge -> failed`

And the initial illegal-edge policy should be equally explicit:

- `completed` has no normal outgoing edges
- `failed` has no normal outgoing edges through the validated move primitive
- edges such as `waiting -> completed` or `backlog -> ready-to-merge` should fail
  validation directly

One deliberate boundary question remains: operator retry currently requeues a
failed task by writing cleaned content to `backlog/` and removing the `failed/`
copy, rather than by a simple move. That may remain a specialized requeue path
even if the logical state change is later documented as `failed -> backlog`.

## Transition inventory to capture

Before implementation, inventory each existing queue transition and classify it.
That inventory should live in the working notes or implementation PR even if the
final code API stays small.

| Class | Example edges | Current call sites | Notes |
| --- | --- | --- | --- |
| Plain move | `waiting -> backlog`, `backlog -> waiting` | `reconcile.go` | Validate edge, move, warn on failure |
| Move then post-write with rollback | `backlog -> in-progress`, `in-progress -> ready-for-review`, `* -> failed` on cancel | `claim.go`, `task.go`, `cancel.go` | Move first, then write marker, then roll back if required |
| Write record then move | `ready-for-review -> failed`, `waiting -> failed`, `backlog -> failed`, `ready-to-merge -> backlog|failed` | `review.go`, `reconcile.go`, `taskops.go` | Marker append often happens before move |
| Write marker then move | `ready-for-review -> ready-to-merge`, `ready-for-review -> backlog` | `review.go` | Marker is part of the semantic result |
| Move with retry/backoff | `ready-to-merge -> completed`, retry-exhausted fail path | `taskops.go`, `claim.go` | Retry policy stays wrapper-specific |

The spike should answer:

1. Which transitions require pre-move side effects?
2. Which require post-move side effects?
3. Which require rollback if follow-up work fails?
4. Which side effects are required versus best-effort?
5. Which call sites are lowest risk to migrate first?

## Preferred design direction

The preferred design is a small validated core plus thin specialized wrappers.

### Validated core

The core should answer one question reliably: "is this queue edge legal, and if
so can we move the task file there atomically?"

Sketch:

```go
type State string

const (
    StateWaiting        State = "waiting"
    StateBacklog        State = "backlog"
    StateInProgress     State = "in-progress"
    StateReadyReview    State = "ready-for-review"
    StateReadyMerge     State = "ready-to-merge"
    StateCompleted      State = "completed"
    StateFailed         State = "failed"
)

func ValidateTransition(from, to State) error
func TransitionTask(tasksDir, filename string, from, to State) error
```

Whether the exact exported surface ends up looking like this is less important
than preserving the layering: validation first, atomic task move second.

### Thin specialized wrappers

Wrappers should own transition-specific behavior that the core should not try to
generalize away completely, for example:

- claim a backlog task and stamp `claimed-by`
- move an in-progress task to review and append the branch marker
- record merge failure and route to backlog or failed
- cancel a task by moving it to failed and appending `cancelled`

These wrappers can share helper code, but they should remain explicit about side
effect ordering and rollback rules.

## API options considered

### Option A: one thin `TransitionTask()` helper

Pros:

- smallest surface area
- easy to validate legal edges

Cons:

- too weak for the real call-site diversity
- wrappers would immediately spring up around it anyway

### Option B: validated core plus small wrappers

Pros:

- keeps validation centralized
- preserves explicit behavior for rollback-heavy transitions
- fits the current codebase without inventing a framework

Cons:

- still leaves a handful of wrapper entry points to maintain

**Recommendation:** choose this option.

### Option C: generic state-machine framework / workflow DSL

Pros:

- theoretically flexible

Cons:

- over-designed for mato's fixed queue
- increases abstraction cost without clear user value
- risks obscuring side-effect ordering that is currently important

This option should be rejected.

## Invariants and guarantees

The design should preserve these rules:

- only declared queue edges are legal
- `AtomicMove` remains the underlying move primitive
- a successful task-file move remains the source of truth for queue state
- wrappers, not the core, define side-effect ordering and rollback policy
- validation does not replace higher-level checks such as dependency
  satisfaction, overlap filtering, branch existence, or retry budgets
- future journaling must be best-effort and must not become a second source of
  truth for current task state

## Migration plan

Migrate incrementally rather than trying to replace every call site at once.

1. Inventory existing transition call sites and classify them.
2. Introduce the validated core and exhaustive tests for legal and illegal
   edges.
3. Migrate the simplest reconcile flows first:
   - `waiting -> backlog`
   - `backlog -> waiting`
   - `waiting/backlog -> failed` quarantine paths
4. Migrate wrapper-heavy but contained flows next:
   - claim path
   - review approve/reject path
   - cancel path
5. Migrate merge and recovery flows last:
   - merge failure routing
   - `ready-to-merge -> completed`
   - orphan recovery and retry-exhausted routing where appropriate
6. Remove duplicated helper code and now-redundant function-variable hooks once
   migrated behavior is covered elsewhere.

The existing unit, race, and integration tests should be the main safety net for
the refactor.

## Open questions

- Should `failed -> backlog` operator retry be modeled as a first-class validated
  transition, or remain a specialized requeue helper because it strips markers
  and rewrites content?
- Should the `State` type and validation table live in `internal/queue`, or in a
  smaller dedicated internal package consumed by `queue`, `runner`, and `merge`?
- Should wrappers return richer typed errors for illegal transitions versus
  side-effect failures, or is contextual `fmt.Errorf(... %w)` enough?
- Which existing function-variable hooks disappear naturally once wrappers exist,
  and which still provide useful test seams even after centralization?

## Alternatives considered

- Leave the current scattered `AtomicMove` usage in place: lowest effort, but it
  keeps validation and rollback logic fragmented
- Add a transition journal first: attractive for observability, but it does not
  solve the core duplication and validation problem
- Build a generic workflow DSL: too abstract for a fixed seven-state queue

## Follow-up: Transition journal

Once task transitions are centralized, mato can optionally append a durable,
append-only transition journal as a side effect of successful transitions.

The key point is scope: this is task history, not scheduler-decision logging. It
records which task moved between which states and when. It does not try to
answer every "why was this task skipped this cycle?" question.

### Sketch

Append one JSON line per successful transition to
`.mato/orchestration-log/transitions.jsonl`:

```json
{"at":"2026-03-26T15:04:06Z","task":"add-retry-logic.md","from":"backlog","to":"in-progress","agent":"a1b2c3d4"}
{"at":"2026-03-26T15:11:10Z","task":"add-retry-logic.md","from":"in-progress","to":"ready-for-review","agent":"a1b2c3d4"}
{"at":"2026-03-26T15:12:30Z","task":"add-retry-logic.md","from":"ready-for-review","to":"ready-to-merge"}
{"at":"2026-03-26T15:15:00Z","task":"add-retry-logic.md","from":"ready-to-merge","to":"completed"}
```

### Narrow schema

Each line should stay small and stable:

| Field | Type | Description |
| --- | --- | --- |
| `at` | string | UTC RFC3339 timestamp |
| `task` | string | Task filename |
| `from` | string | Source state directory |
| `to` | string | Destination state directory |
| `agent` | string | Agent ID when applicable |
| `reason` | string | Optional fixed reason code |

### Design constraints

- best-effort only: if the journal write fails, the transition still succeeds
- gitignore it: this is local operational history, not repository state
- keep it append-only and narrow; do not turn it into a second source of truth
- use it to power future task-history views such as `mato log`, not as a
  general event bus

## Acceptance criteria

- the proposal defines the legal queue edges in one validated place
- migrated call sites no longer hard-code queue-state moves ad hoc when they are
  truly queue transitions
- illegal transitions have direct unit-test coverage
- rollback-heavy wrappers have direct unit-test coverage for happy path and
  follow-up failure cases
- the refactor does not change the user-facing seven-state queue model
