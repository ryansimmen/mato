# State Machine Formalization

**Priority:** High
**Effort:** Medium

## Problem

Task state transitions are implicit. The 7 queue states (`waiting`, `backlog`,
`in-progress`, `ready-for-review`, `ready-to-merge`, `completed`, `failed`) and
12+ transitions between them are scattered across 6+ files with 20+ independent
`AtomicMove` call sites. Each call site independently re-implements the
move+record+rollback pattern with bespoke error handling and function-variable
hooks.

Examples of duplication:

- `claim.go` does AtomicMove to `in-progress/`, then `claimPrependFn`, then
  rollback via `claimRollbackFn` — with 3 separate function variables just to
  make it testable.
- `task.go` does AtomicMove to `ready-for-review/`, writes a branch marker, then
  has a hand-rolled rollback with AtomicMove back to `in-progress/` if the marker
  write fails.
- `merge/taskops.go` has its own `failMergeTask` that appends a failure record
  and AtomicMoves, while `moveTaskWithRetry` wraps AtomicMove in a retry loop.
- `runner/review.go` quarantines parse failures with AtomicMove + stderr warning
  + optional record append — duplicated at multiple call sites.

Invalid transitions like `waiting → completed` are prevented only by programmer
discipline, not by the type system or a validation layer. Nothing enforces the
state diagram documented in `docs/architecture.md`.

## Idea

Introduce a centralized `TransitionTask()` flow with a validated transition
table that replaces bare `AtomicMove` calls for task state changes.

The important change is centralization and validation, not the exact function
signature. The simplest version is `validate + AtomicMove`, but the real codebase
also has transitions with post-move writes, rollback on post-move failure,
warnings, and retry behavior. The proposal should leave room for a thin helper
plus transition-specific wrappers where needed.

```go
// Declarative transition table
var validTransitions = map[[2]string]bool{
    {"waiting", "backlog"}:               true,
    {"waiting", "failed"}:                true,
    {"backlog", "in-progress"}:           true,
    {"backlog", "waiting"}:               true,
    {"backlog", "failed"}:                true,
    {"in-progress", "ready-for-review"}:  true,
    {"in-progress", "backlog"}:           true,
    {"in-progress", "failed"}:            true,
    // ... all valid transitions
}

func TransitionTask(tasksDir, filename, fromState, toState string) error {
    if !validTransitions[[2]string{fromState, toState}] {
        return fmt.Errorf("invalid transition %s -> %s for %s", fromState, toState, filename)
    }
    return AtomicMove(
        filepath.Join(tasksDir, fromState, filename),
        filepath.Join(tasksDir, toState, filename),
    )
}
```

### What this changes

- **Validates transitions**: illegal state changes fail at runtime with a clear
  error instead of silently corrupting queue state.
- **Consolidates rollback patterns**: the duplicated move+record+rollback logic
  across `claim.go`, `task.go`, `review.go`, and `taskops.go` can share a common
  abstraction.
- **Reduces function-variable hooks**: `claimRollbackFn`, `retryExhaustedMoveFn`,
  and similar test hooks exist because there is no centralized transition function
  to test against. A single `TransitionTask` function is a natural test seam.
- **Provides a hook point**: an orchestration log (see
  follow-up section below) can emit structured events on each transition
  without modifying every call site.
- **Makes future state additions cheap**: adding a new queue state becomes a table
  entry plus one subcommand, instead of 3-5 new `AtomicMove` call sites with
  bespoke error handling.

### What this does not change

- No new user-facing concepts, CLI flags, or frontmatter fields.
- No new queue directories. The 7 existing states stay as-is.
- The filesystem-backed queue model stays. `TransitionTask` uses `AtomicMove`
  internally.
- The `PollIndex` snapshot model stays.

## Pre-Implementation Spike

Before designing the API, catalog every existing `AtomicMove` call site and
group each one into a small number of transition classes:

1. **Plain move**: validate the state edge and move the file.
2. **Move + post-write + rollback**: move first, write metadata, roll back if the
   write fails.
3. **Record + move**: append or persist some record before moving to the next
   state.
4. **Move + retry/backoff**: transitions that already wrap `AtomicMove` in retry
   logic.
5. **Quarantine + warn**: transitions that move a task while also reporting a
   non-fatal operator warning.

That inventory should answer:

1. Which transitions require pre-move side effects?
2. Which require post-move side effects?
3. Which require rollback if a follow-up write fails?
4. Can one helper cover most transitions, or should a small validated core be
   wrapped by specialized helpers?
5. Which call sites are lowest risk to migrate first?

This is a short spike, but it determines whether `TransitionTask()` stays a thin
primitive or grows a small options struct.

## Follow-up: Transition Journal

Once task transitions are centralized, mato can optionally append a durable,
append-only transition journal as a side effect of successful transitions.

The key point is scope: this is **task history**, not scheduler-decision logging.
It records which task moved between which states and when. It does not try to
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

- Best-effort only: if the journal write fails, the transition still succeeds.
- Gitignore it: this is local operational history, not repository state.
- Keep it append-only and narrow; do not turn it into a second source of truth.
- Use it to power future task-history views such as `mato log`, not as a general
  event bus.

## Design Considerations

- Keep this as an internal simplification, not a generic workflow DSL. The function
  should be a thin validation + move layer, not a framework.
- Migrate call sites incrementally. Start with the simplest transitions (e.g.
  `reconcile.go` promotions) and work toward the more complex ones (claim with
  rollback, merge with retry).
- The existing test coverage (concurrent tests, integration tests) provides safety
  nets for the refactor.
- Keep "task history" and "scheduler-decision history" separate. This proposal is
  only about the former.

## Relationship to Existing Features

- Enables a later transition journal and a richer `mato log` without introducing
  a standalone orchestration event system.
- Prerequisite for any future queue state additions (human-review gates, parallel
  agent support).
- Simplifies the existing codebase by reducing scattered transition logic.
