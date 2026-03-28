---
id: add-task-inspect-command
priority: 24
affects:
   - cmd/mato/main.go
   - cmd/mato/main_test.go
   - internal/inspect/
   - internal/queue/index.go
   - internal/queue/index_test.go
   - internal/taskfile/metadata.go
   - internal/taskfile/metadata_test.go
   - README.md
   - docs/architecture.md
---

> **Status: Implemented** — This proposal has been fully implemented.
> The text below describes the original design; see the source code for the
> current implementation.

# Add `mato inspect` for single-task root-cause explanation

## Goal

Add a read-only `mato inspect <task-ref>` command that resolves one task and
explains its current queue state, the immediate reason it is in that state,
and the next action needed for it to advance. The command should answer
concrete operator questions such as "Why is this task still waiting?", "Why
is it in backlog but not next?", "Why did it fail?", and "Was it previously
rejected by review?"

## Scope

In scope:

- New Cobra subcommand:
   `mato inspect <task-ref> [--repo <path>] [--format text|json]`
- Resolution of a single task reference across all queue states and
   parse-failed files.
- Text and JSON outputs backed by a shared inspection model.
- Root-cause explanations for:
    - dependency-blocked waiting tasks
    - backlog tasks that are still dependency-blocked before reconcile moves
      them back to `waiting/` (for example after `mato retry` or a manual edit)
    - waiting or backlog tasks that are structurally invalid before reconcile
       moves them out (`cycle`, `self-cycle`, duplicate waiting id, invalid glob,
       parse failure)
    - conflict-deferred backlog tasks
    - runnable backlog tasks that are not first in claim order
    - ready-for-review tasks that are already invalid before the next review
      pass (`parse failure`, review retry budget exhausted)
    - in-progress, ready-for-review, ready-to-merge, completed, and failed tasks
   - historical review rejection context for tasks requeued to `backlog/`
   - malformed task files that are visible in the index but not yet reconciled
      out
- Documentation and test coverage.

Out of scope:

- Multi-task inspection, search, filtering, or watch mode.
- Event/log browsing; that remains separate from any future `log` command.
- Retrying, moving, or otherwise mutating task files.
- Rendering the full dependency graph or recent messages for a task.
- Inventing a new queue snapshot format; the command should reuse
   `queue.BuildIndex(...)`, `queue.ComputeRunnableBacklogView(...)`, and
   current diagnostics helpers.

## Design

### Command boundary

Add `newInspectCmd()` to `cmd/mato/main.go`, following the existing `status`,
`graph`, and `doctor` subcommand pattern:

- `Args: cobra.ExactArgs(1)`
- `--repo` and `--format` flags
- `--format` validation limited to `text|json`, checked in `RunE` like the
   existing commands
- command delegates to a new `internal/inspect` package through a package-level
   seam such as `var inspectShowFn = inspect.Show`, mirroring existing seams
   like `doctorRunFn`

Keep the root command unchanged aside from registering the subcommand.

### New package: `internal/inspect`

Create a dedicated package instead of extending `internal/status`:

- `status` is an aggregate dashboard with many sections and shared gather
   logic.
- `inspect` is single-task analysis and explanation.
- separating them avoids coupling the dashboard model to per-task reasoning
   branches.

Recommended public surface:

```go
func Show(repoRoot, taskRef, format string) error
func ShowTo(w io.Writer, repoRoot, taskRef, format string) error
```

Internally, the package should:

1. resolve the canonical repo root with `git.Output(..., "rev-parse",
    "--show-toplevel")`
2. derive `tasksDir := filepath.Join(repoRoot, dirs.Root)`
3. build one `*queue.PollIndex` via `queue.BuildIndex(tasksDir)`
4. resolve exactly one task candidate
5. compute a shared `InspectResult`
6. render the result as text or JSON

### Shared inspection model

Use one internal result type for both renderers so text and JSON cannot drift.
Keep the model narrow and causal.

Suggested shape:

```go
type InspectResult struct {
      TaskID       string
      Filename     string
      Title        string
      State        string
      Status       string
      Reason       string
      NextStep     string

      Branch       string
      ClaimedBy    string
      ClaimedAt    time.Time

      QueuePosition int
      QueueTotal    int

      BlockingTask         *BlockingTask
      BlockingDependencies []BlockingDependency
      ConflictingAffects   []string

       FailureKind           string
       FailureCount          int
       ReviewFailureCount    int
       MaxRetries            int
       LastFailureReason     string
       LastCycleReason       string
      LastTerminalReason    string
      ReviewRejectionReason string

      ParseError string
}
```

Implementation note: the JSON view type should not expose bare zero
`time.Time` fields with `omitempty`. Optional timestamps such as `claimed_at`
should be encoded through an omittable representation such as a pointer or a
preformatted string field so absent data is actually omitted.

The exact field names can be tuned during implementation, but the key
requirement is that the model carries structured blockers and secondary
context instead of forcing the renderer to parse human text.

### Task reference resolution

Resolve the target from the already-built index instead of rescanning
directories.

Candidate search should include:

- every `TaskSnapshot` from `idx.TasksByState(...)`
- every `ParseFailure` from `idx.ParseFailures()`

Accepted references:

- exact filename, with or without `.md`
- filename stem
- explicit frontmatter `id` for successfully parsed tasks only

Resolution rule:

- collect every candidate whose filename, stem, or explicit `id` matches the
   user input
- parse-failed files participate only through filename and stem matching,
   because `ParseFailure` does not currently retain a trustworthy explicit `id`
- if zero matches, return a clear `task not found` error
- if more than one match, return an ambiguity error that lists
   `state/filename` plus the task `id`
- only proceed when exactly one candidate remains

Do not silently prefer one state over another. The queue can legitimately
contain ambiguous aliases, and inspect should surface that instead of
guessing.

### Reuse of existing queue analysis

Build the explanation from current primitives rather than inventing a second
scheduler:

- `queue.BuildIndex(tasksDir)`:
    source of state, priority, branch, claim metadata, failure counts,
    review-failure counts, terminal/cycle reasons, cached invalid-glob errors,
    and parse failures
- `queue.DiagnoseDependencies(tasksDir, idx)`:
    source of waiting-task dependency blockage reasons, ambiguous dependency
    detection, cycle detection, and duplicate waiting ID detection
- `queue.ComputeRunnableBacklogView(tasksDir, idx)`:
    source of backlog dependency-blocked explanations, conflict-deferred
    explanations, and runnable backlog ordering, matching current claim logic
- `frontmatter.ExtractTitle(...)`:
    source of the displayed task title, matching other commands

### Minimal additions to existing metadata/index code

The inspect command needs one piece of metadata the index does not currently
retain: the latest review rejection reason for a task that was sent back to
`backlog/`.

Add a canonical helper in `internal/taskfile/metadata.go`, alongside the
existing `LastFailureReason`, `LastCycleFailureReason`, and
`LastTerminalFailureReason`, for example:

```go
func LastReviewRejectionReason(data []byte) string
```

`taskfile.ExtractReviewRejections(data)` already exists for callers that need
the full preserved rejection marker history; the new helper should complement
that by returning only the latest rejection reason in a renderer-friendly form.

Then extend `queue.TaskSnapshot` and `queue.ParseFailure` with a matching
field populated during `BuildIndex`. This keeps inspect on the existing
"read each task once" path instead of reopening task files ad hoc.

No new filesystem cache or manifest should be introduced.

### Status classification rules

Use queue state plus existing metadata to derive a narrow current status, and
treat review rejection as secondary historical context rather than the primary
blocker.

Primary status rules:

- parse failure in any state:
   `status = "invalid"`
   Explain the parse error and how to fix it.
- any task snapshot with `GlobError != nil` while it is still in `waiting/`
   or `backlog/`:
   `status = "invalid"`
   Explain the invalid glob syntax and that reconcile will move the task to
   `failed/` once processed.
- waiting task that is the non-retained duplicate for a duplicate waiting
   `id`, or that is part of a cycle/self-cycle before reconcile moves it:
   `status = "invalid"`
   Explain the structural problem and the corrective action.
- retained waiting copy when a duplicate exists elsewhere:
   do not mark it invalid solely because a duplicate peer exists; continue with
   normal dependency analysis, because reconcile keeps the retained copy and
   fails the duplicate.
- waiting task blocked by dependencies:
    `status = "blocked"`
    Use `diag.Analysis.Blocked[taskID]` for dependency causes, including:
    - `BlockedByWaiting`
    - `BlockedByUnknown`
    - `BlockedByExternal`
    - `BlockedByAmbiguous`
- backlog task present in `view.DependencyBlocked`:
   `status = "blocked"`
   Explain which dependencies are unsatisfied and that reconcile will move the
   task back to `waiting/` on the next pass.
- backlog in `view.Deferred`:
    `status = "deferred"`
    Explain the conflicting task, its directory, and conflicting `affects`
    entries.
- backlog in `view.Runnable`:
    `status = "runnable"`
    Use the `view.Runnable` ordering to compute queue position.
    If position is `1`, reason should say the task is next claim candidate.
    If position is greater than `1`, reason should say how many runnable tasks
    are ahead.
- in-progress:
    `status = "running"`
    Explain who claimed the task and, when present, since when.
- ready-for-review task whose `ReviewFailureCount >= MaxRetries`:
   `status = "invalid"`
   Explain that the review retry budget is exhausted and that the next review
   selection pass will move the task to `failed/`.
- ready-for-review:
    `status = "ready_for_review"`
    Explain that the task branch exists and is waiting for the review agent.
- ready-to-merge:
   `status = "ready_to_merge"`
   Explain that review passed and the task is waiting for host-side merge.
- completed:
   `status = "completed"`
   Explain that no further action is required.
- failed:
   `status = "failed"`
   Classify as `retry`, `cycle`, or `terminal` using the same logic already
   used by `status_json.go`, and surface the strongest available reason.

Secondary context rules:

- if `ReviewRejectionReason` is present, include it as historical context in
   both text and JSON, but do not let it override a current deferral or
   queue-position explanation
- for blocked dependencies, enrich `dag.BlockedByExternal` with actual queue
   state when possible by deriving a local `id -> state/filename` map from the
   index, similar to how `internal/status` builds state maps today. That lets
   inspect say `dependency X is in failed/` rather than the weaker `external`.

### Output contract

Text output:

- keep it compact and stable
- always print:
   - task
   - state
   - status
   - reason
   - next step
- print optional sections only when present:
   - queue position
   - blocking dependencies
   - blocking task
   - conflicting affects
   - claim info
   - failure budget/reason
   - review rejection history
   - parse error

Example:

```text
Task: add-retry-logic
State: backlog
Status: deferred
Reason: overlaps with in-progress task fix-http-client.md on pkg/client/http.go
Next step: wait for fix-http-client.md to leave active states
Review history: previously rejected - missing coverage for retry backoff
```

JSON output:

- encode the same facts as structured fields
- do not make consumers parse the human `reason`
- optional fields are omitted via `omitempty`; do not emit `null`

Required JSON fields:

- `task_id`
- `filename`
- `title`
- `state`
- `status`
- `reason`
- `next_step`

Optional JSON fields:

- `queue_position`
- `queue_total`
- `branch`
- `claimed_by`
- `claimed_at`
- `review_failure_count`
- `blocking_task`
- `blocking_dependencies`
- `conflicting_affects`
- `failure_kind`
- `failure_count`
- `max_retries`
- `last_failure_reason`
- `last_cycle_reason`
- `last_terminal_reason`
- `review_rejection_reason`
- `parse_error`

## Step-by-Step Breakdown

1. Add metadata support for review rejection summaries.
    - Implement `LastReviewRejectionReason(data []byte)` in
       `internal/taskfile/metadata.go`.
    - Add focused unit tests in `internal/taskfile/metadata_test.go`.
    - Extend `queue.TaskSnapshot` and `queue.ParseFailure` to carry the latest
       review rejection reason.
    - Populate the new field during `queue.BuildIndex(...)`.
    - Add or adjust queue index tests to confirm the value is preserved for
       normal snapshots and parse failures.

2. Implement the new inspection package.
    - Create `internal/inspect/` with the shared result model and the public
       `Show` and `ShowTo` entry points.
    - Build a single `queue.BuildIndex(...)` snapshot per command invocation.
    - Add candidate collection and exact-one-match resolution over snapshots
       and parse failures.
    - Return deterministic not-found and ambiguity errors.

3. Implement state-to-explanation mapping.
    - Build one dependency diagnostic snapshot via
       `queue.DiagnoseDependencies(tasksDir, idx)`.
    - Build one runnable backlog view via
        `queue.ComputeRunnableBacklogView(tasksDir, idx)`.
    - Derive the `InspectResult` for each state using the classification rules
       above.
    - Prefer the most actionable current reason:
        - invalid structural issue over generic state
        - cycle/duplicate/invalid waiting issue over blocked dependency text
        - invalid backlog glob over deferred/runnable classification
        - backlog dependency blockage over deferred/runnable classification
        - deferral over generic backlog
        - runnable queue position for non-deferred backlog tasks
        - exhausted review retry budget over generic ready-for-review state
    - Attach review rejection as historical context when present.
    - Derive `NextStep` from the root cause rather than repeating the state.

4. Add renderers.
    - Implement concise text rendering.
    - Implement JSON rendering from the same `InspectResult`.
    - Keep the renderers thin; all reasoning should already be in the result
       builder.

5. Wire the CLI command.
    - Register `newInspectCmd()` in `cmd/mato/main.go`.
    - Add a package-level `inspectShowFn` seam so `cmd/mato/main_test.go` can
       assert normal flag parsing and delegation without shelling out.
    - Match the error-handling style used by `status`, `graph`, and `doctor`.
    - Validate `--format` in `RunE`.
    - Pass the single positional task reference through unchanged.

6. Add tests.
    - `cmd/mato/main_test.go`:
       - subcommand registration
       - format validation
       - exact-arg enforcement
       - normal flag parsing and delegation through `inspectShowFn`
    - `internal/inspect/inspect_test.go`:
        - waiting task blocked by dependency in `waiting/`
        - waiting task blocked by dependency in `failed/`
        - waiting task with unknown dependency
        - waiting task with ambiguous dependency id
       - waiting task that is part of a self-cycle before reconcile
       - waiting task that is part of a multi-node cycle before reconcile
        - waiting task with invalid glob syntax before reconcile
        - backlog task with invalid glob syntax before reconcile
        - backlog task that is still dependency-blocked before reconcile moves
          it back to `waiting/`
        - backlog task deferred by active overlap
        - backlog task runnable and first in queue
        - backlog task runnable but behind higher-priority runnable tasks
        - backlog task with preserved review rejection plus current deferral,
           ensuring deferral remains primary
        - backlog task with preserved review rejection plus runnable position,
           ensuring review context remains secondary
        - in-progress task with claim metadata
        - ready-for-review task with exhausted review retry budget pending
          quarantine to `failed/`
        - ready-for-review and ready-to-merge tasks
        - failed task with retry, cycle, and terminal failure variants
        - malformed task surfaced from `ParseFailure`, including a
          ready-for-review parse failure
        - duplicate waiting ID surfaced as invalid for the duplicate file
        - retained waiting copy of a duplicate id still follows normal dependency
           analysis
        - unknown task ref
        - ambiguous task ref across multiple matches
        - JSON output field coverage for blocked, deferred, failed, invalid-glob,
          review-history, and exhausted-review-budget cases
       - explicit JSON assertion that absent `claimed_at` is omitted rather than
          serialized as a zero timestamp
    - `internal/integration/inspect_test.go`:
       - command-level smoke test from a real repo root
       - one text case and one JSON case to ensure wiring and repo resolution
          work end-to-end

7. Update documentation.
    - `README.md`: add `mato inspect` to the command set with one text example
       and one JSON example.
    - `docs/architecture.md`: document `inspect` as a read-only command that
       reuses `BuildIndex`, `ComputeRunnableBacklogView`, dependency diagnostics,
       and overlap deferral data for single-task troubleshooting.

## File Changes

Create:

```text
internal/inspect/inspect.go
internal/inspect/inspect_test.go
internal/integration/inspect_test.go
```

Possible additional split if the package gets crowded:

```text
internal/inspect/render_text.go
internal/inspect/inspect_json.go
```

Start with the smallest sensible file set; split only if the implementation
becomes hard to read.

Modify:

```text
cmd/mato/main.go
cmd/mato/main_test.go
internal/queue/index.go
internal/queue/index_test.go
internal/taskfile/metadata.go
internal/taskfile/metadata_test.go
README.md
docs/architecture.md
```

## Error Handling

- Reject invalid `--format` values in `RunE` before any work begins.
- Treat repo root resolution failures the same way as other read-only
   commands: return the underlying error.
- Missing queue subdirectories are not special-cased; the existing index
   builder already tolerates them, so a missing queue simply leads to either a
   valid result or a not-found error.
- Unknown task references should return a single clear error.
- Ambiguous task references should return an error that lists all matching
   candidates.
- Parse-failed tasks should still be inspectable if their filename or stem
   matches.
- The command must remain read-only: no task moves, no marker writes, no
   retries.
- Do not emit separate warnings for unrelated `BuildIndex` warnings; only
   surface warnings that directly explain the matched task.

## Testing Strategy

- Follow the repository's existing table-driven test style where multiple
   state variants share the same harness.
- Prefer `testutil.SetupRepo` and the queue helpers already used by `status`
   tests and integration tests.
- Cover both result-building and renderer output so the explanation text
   cannot regress silently.
- Verification after implementation:
   - `go test -race ./internal/taskfile/...`
   - `go test -race ./internal/queue/...`
   - `go test -race ./internal/inspect/...`
   - `go test -race ./cmd/mato/...`
   - `go test -race ./internal/integration/...`
   - `go build ./... && go test -count=1 ./...`

## Risks & Mitigations

- Ambiguous alias matching may surprise users.
   Mitigation: never guess; return all candidate matches and require a unique
   ref.
- Inspect reasoning could drift from scheduler behavior.
   Mitigation: derive explanations only from `BuildIndex` / `PollIndex`,
   `DiagnoseDependencies`, and `ComputeRunnableBacklogView`.
- Re-reading task files in inspect could create inconsistent output or
   unnecessary I/O.
   Mitigation: extend `BuildIndex` once for review rejection summaries and
   reuse that snapshot everywhere.
- Backlog tasks can carry historical review feedback that is not their current
   blocker.
   Mitigation: keep review rejection as secondary context and preserve a strict
   current-cause precedence order.
- Text and JSON outputs could diverge over time.
   Mitigation: build one shared `InspectResult` and keep renderers
   presentation-only.

## Open Questions

None for the first implementation. This plan assumes that ambiguous task
references should fail rather than preferring one candidate, that parse-failed
tasks are only addressable by filename/stem, and that review rejection is
historical context unless it is itself the most relevant secondary
explanatory detail in the rendered output.
