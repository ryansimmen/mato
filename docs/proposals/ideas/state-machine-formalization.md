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

Introduce a centralized `TransitionTask()` function with a validated transition
table that replaces bare `AtomicMove` calls for task state changes.

```go
// Declarative transition table
var validTransitions = map[[2]string]bool{
    {"waiting", "backlog"}:              true,
    {"waiting", "failed"}:              true,
    {"backlog", "in-progress"}:         true,
    {"backlog", "waiting"}:             true,
    {"backlog", "failed"}:              true,
    {"in-progress", "ready-for-review"}: true,
    {"in-progress", "backlog"}:         true,
    {"in-progress", "failed"}:          true,
    // ... all valid transitions
}

func TransitionTask(tasksDir, filename, fromState, toState string) error {
    if !validTransitions[[2]string{fromState, toState}] {
        return fmt.Errorf("invalid transition %s → %s for %s", fromState, toState, filename)
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
  `orchestration-log-as-fsm-hook.md`) can emit structured events on each
  transition without modifying every call site.
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
determine:

1. Which transitions require pre-move side effects (e.g. appending failure records
   before moving to `failed/`)?
2. Which require post-move side effects (e.g. writing branch markers after moving
   to `ready-for-review/`)?
3. Can post-move-with-rollback be the universal pattern, or do some transitions
   genuinely need pre-move writes?

This is a 1-2 hour investigation that determines the API shape.

## Design Considerations

- Keep this as an internal simplification, not a generic workflow DSL. The function
  should be a thin validation + move layer, not a framework.
- Migrate call sites incrementally. Start with the simplest transitions (e.g.
  `reconcile.go` promotions) and work toward the more complex ones (claim with
  rollback, merge with retry).
- The existing test coverage (concurrent tests, integration tests) provides safety
  nets for the refactor.
- Consider whether `TransitionTask` should accept optional callbacks for pre/post
  side effects, or whether callers should compose `TransitionTask` with their own
  record-writing logic.

## Relationship to Existing Features

- Enables `orchestration-log-as-fsm-hook` as a transition hook rather than a
  standalone system.
- Prerequisite for any future queue state additions (human-review gates, parallel
  agent support).
- Simplifies the existing codebase by reducing scattered transition logic.
