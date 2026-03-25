---
id: make-depends-on-authoritative-across-scheduling
priority: 22
affects:
  - internal/queue/claim.go
  - internal/queue/diagnostics.go
  - internal/queue/reconcile.go
  - internal/queue/claim_test.go
  - README.md
  - docs/task-format.md
  - docs/architecture.md
tags: [feature, scheduling, correctness]
estimated_complexity: medium
---
# Make depends_on authoritative across scheduling

Today `depends_on` only gates promotion from `waiting/` into `backlog/`.
A task that is manually placed in `backlog/` can still be claimed even when
its dependencies are not satisfied. That makes directory placement part of
dependency truth and creates a surprising footgun: declared dependencies are
not authoritative across scheduling.

## Steps to fix

1. Make scheduling honor `depends_on` for all claimable tasks, not only tasks
   currently in `waiting/`.

2. Define the intended behavior clearly before implementation:
   - a backlog task with unmet dependencies must not be claimable
   - decide whether such tasks stay in `backlog/` but are excluded from the
     queue manifest, or are automatically moved back to `waiting/`
   - document the choice and keep the behavior deterministic

3. Extend the relevant queue logic so claim selection checks dependency
   satisfaction before a task can move to `in-progress/`:
   - claim path in `SelectAndClaimTask`
   - dependency diagnostics/index helpers so backlog tasks can be evaluated
     consistently with waiting tasks
   - status/doctor output so the reason is visible to operators

4. Keep the model easy to explain:
   - `depends_on` should be authoritative regardless of directory placement
   - `waiting/` remains the recommended place for blocked tasks, but not the
     only place where dependency enforcement exists

5. Add tests:
   - backlog task with unmet dependency is skipped or moved according to the
     chosen design
   - backlog task becomes claimable once dependencies are completed
   - queue manifest excludes dependency-blocked backlog tasks when relevant
   - status/doctor surfaces the dependency-blocked reason
   - existing waiting-task promotion behavior remains correct

6. Update `README.md`, `docs/task-format.md`, and `docs/architecture.md` to
   explain the new invariant and any changes to directory semantics.

7. Run `go build ./... && go test -count=1 ./...` to verify.
