---
id: add-cancel-command
priority: 26
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/queue/cancel.go
  - internal/queue/cancel_test.go
  - internal/taskfile/metadata.go
  - internal/taskfile/metadata_test.go
  - internal/status/status_gather.go
  - internal/status/status_render.go
  - internal/status/status_json.go
  - README.md
  - docs/architecture.md
  - docs/task-format.md
tags: [feature, cli, ux]
estimated_complexity: medium
---
# Add `mato cancel` to permanently withdraw tasks from the queue

## Goal

Add a `mato cancel <task-name>...` command that moves one or more tasks to
`failed/` with a `<!-- cancelled: -->` marker, preventing further execution
while keeping the task retryable via `mato retry`. This is the complement to
`mato retry`: retry requeues a failed task, cancel withdraws a queued task.

## Problem

There is no operator command to say "stop working on this task." The only
ways to remove a task from the active queue are to let it complete, let it
exhaust retries, or manually move files between directories. None of these
express deliberate cancellation:

- Deleting the file loses task history and can confuse dependency resolution.
- Manually moving to `failed/` without a marker makes the cancellation
  indistinguishable from an actual failure.
- Waiting for retry exhaustion wastes agent compute on work the operator has
  already decided should not run.

## Scope

### In scope

- New Cobra subcommand:
  `mato cancel <task-name>... [--repo <path>]`
- Multi-directory task resolution across all queue states.
- New `<!-- cancelled: -->` HTML comment marker type.
- Integration with `StripFailureMarkers` so `mato retry` reverses
  cancellation.
- Downstream dependency warnings when cancelling a task that has dependents
  in `waiting/`.
- Status classification of cancelled tasks as distinct from other failure
  types.
- Documentation and test coverage.

### Out of scope

- `--cascade` flag for transitive cancellation of dependent tasks.
- Killing or stopping running agent containers.
- New `cancelled/` queue directory.
- Cancelling tasks that are already in `completed/`.
- Automatic undo or uncancel (that is `mato retry`).
- Doctor hygiene checks for stale cancelled tasks.
- Interactive confirmation prompts.

## Design

### Cancellation marker

Introduce a new HTML comment marker:

```
<!-- cancelled: operator at 2026-03-26T15:00:00Z -->
```

The marker uses the same format conventions as existing markers:

- actor is always `operator` since this is a human-initiated action
- timestamp is RFC3339 UTC
- one marker per cancellation event

The marker is distinct from `<!-- failure: -->` so it does not count against
`max_retries`. It is distinct from `<!-- terminal-failure: -->` so status
can classify it separately.

### Cancellable states

| Source state | Action |
|---|---|
| `waiting/` | Move to `failed/`, append cancelled marker |
| `backlog/` | Move to `failed/`, append cancelled marker |
| `in-progress/` | Move to `failed/`, append cancelled marker, warn that the agent container may still be running |
| `ready-for-review/` | Move to `failed/`, append cancelled marker |
| `ready-to-merge/` | Move to `failed/`, append cancelled marker |
| `failed/` | Append cancelled marker only (task is already terminal; marker prevents accidental retry without checking intent) |
| `completed/` | Refuse with a clear error: task has already been merged |

For `in-progress/` tasks, the move is safe because the runner already
handles missing task files gracefully:

- `postAgentPush` (`internal/runner/task.go:110`) checks whether the task
  file is still in `in-progress/` before pushing or moving it. If not, the
  agent's work is silently discarded.
- `recoverStuckTask` (`internal/runner/task.go:201`) performs the same
  check. If the file is gone, recovery is skipped.

The agent container will finish its work but the host will not act on it.
The container will exit naturally and the runner will clean up its lock and
presence files on the next poll cycle.

### Task resolution

Cancel needs to locate a task across all seven queue directories plus parse
failures. Use the same resolution approach proposed for `mato inspect`:

- Search all `TaskSnapshot` entries from a `PollIndex` by filename stem
  (with or without `.md` suffix) and by explicit frontmatter `id`.
- Search `ParseFailure` entries by filename and stem only.
- Require exactly one match. If zero, return a clear not-found error. If
  more than one, return an ambiguity error listing all candidates with their
  `state/filename`.

If `mato inspect` lands first, cancel should reuse its resolution logic. If
cancel lands first, the resolution should be built as a reusable helper in
`internal/queue/` so inspect can adopt it later.

### Retry interaction

`mato retry` on a cancelled task should work identically to retrying a
failed task:

1. `StripFailureMarkers` strips the `<!-- cancelled: -->` marker along with
   all other marker types.
2. The cleaned content is written to `backlog/`.
3. The `failed/` copy is removed.

This requires adding `"<!-- cancelled:"` to `failureMarkerPrefixes` in
`internal/taskfile/metadata.go`.

### Downstream dependency warnings

When a cancelled task has an `id` that appears in any waiting task's
`depends_on`, the command should print a warning listing affected downstream
tasks. This is informational only — cancel does not cascade.

Example output:

```
cancelled: add-auth-models.md (was in backlog/)
  warning: 2 tasks depend on add-auth-models:
    waiting/add-login-endpoints.md
    waiting/add-auth-tests.md
  these tasks will remain blocked until add-auth-models is retried
```

### Multiple task names

Like `mato retry`, cancel accepts multiple positional arguments and
processes them independently. If some tasks fail to resolve or cannot be
cancelled, the command reports per-task errors but continues processing the
remaining tasks. It returns the first error encountered.

### Status integration

Extend the existing failure classification in `mato status` to distinguish
cancelled tasks from other failure types:

- Text view: show cancelled tasks in the failed section with
  `(cancelled)` instead of `(terminal)`, `(cycle)`, or `(retry)`.
- JSON view: add `"failure_kind": "cancelled"` to the `FailedTaskJSON`
  type, alongside existing values `"terminal"`, `"cycle"`, and `"retry"`.

Detection: a task in `failed/` that contains a `<!-- cancelled: -->` marker
is classified as cancelled. If both failure and cancelled markers are
present (e.g., a task that failed, was retried, and then cancelled), the
cancelled marker takes precedence because it was the deliberate terminal
action.

## Step-by-Step Breakdown

1. **Add cancelled marker support to `internal/taskfile/metadata.go`.**
   - Add `cancelledMarkerPrefix` constant: `"<!-- cancelled:"`.
   - Add `AppendCancelledRecord(path string) error` that writes
     `<!-- cancelled: operator at TIMESTAMP -->` via `atomicwrite.AppendToFile`.
   - Add `ContainsCancelledMarker(data []byte) bool` for status
     classification.
   - Add `"<!-- cancelled:"` to `failureMarkerPrefixes` so
     `StripFailureMarkers` removes it on retry.
   - Add unit tests for append, detection, stripping, and idempotency.

2. **Add multi-directory task resolution to `internal/queue/`.**
   - Add a `ResolveTask(tasksDir, taskRef string) (TaskMatch, error)` helper
     that builds a `PollIndex`, searches all snapshots and parse failures by
     filename stem and explicit `id`, and returns exactly one match or a
     descriptive error.
   - `TaskMatch` should carry the matched filename, current directory/state,
     and the full file path.
   - Add unit tests for exact match, stem match, `.md` suffix handling,
     not-found, and ambiguous-match cases.

3. **Implement cancel logic in `internal/queue/cancel.go`.**
   - Add `CancelTask(tasksDir, taskName string) (CancelResult, error)`.
   - Use `ResolveTask` to find the target.
   - Refuse `completed/` tasks with a clear error.
   - For `failed/` tasks, append the cancelled marker without moving.
   - For all other states, append the cancelled marker and atomically move
     to `failed/`.
   - Compute downstream dependency warnings by scanning waiting tasks for
     `depends_on` references to the cancelled task's ID.
   - `CancelResult` should carry the source state, filename, and any
     downstream warnings for the caller to print.
   - Add unit tests for each source state, completed refusal, downstream
     warnings, and task-not-found.

4. **Wire the CLI command in `cmd/mato/main.go`.**
   - Add `newCancelCmd()` following the `retry` command pattern.
   - `Args: cobra.MinimumNArgs(1)`, `--repo` flag.
   - Iterate over positional arguments, call `queue.CancelTask` for each.
   - Print result and warnings per task.
   - Report per-task errors but continue with remaining tasks.
   - Add a `cancelTaskFn` seam for test injection.
   - Add command tests for: single cancel, multi-cancel, partial failure,
     downstream warnings, completed refusal, missing task, and
     missing `.mato/` directory.

5. **Update status classification for cancelled tasks.**
   - Extend `classifyFailedTask` (or equivalent) in
     `internal/status/status_gather.go` to check for `<!-- cancelled: -->`
     markers and return `"cancelled"` as the failure kind.
   - Update text rendering to show `(cancelled)` for these tasks.
   - Update JSON output to include `"cancelled"` as a `failure_kind` value.
   - Add/adjust status tests for cancelled task display.

6. **Update documentation.**
   - `README.md`: add `mato cancel` to the command set with usage example.
   - `docs/task-format.md`: add `<!-- cancelled: -->` to the runtime
     metadata section.
   - `docs/architecture.md`: document cancel as an operator-initiated
     terminal transition.

7. **Validate.**
   - `go build ./... && go vet ./... && go test -count=1 ./...`

## File Changes

Create:

```text
internal/queue/cancel.go
internal/queue/cancel_test.go
```

Modify:

```text
cmd/mato/main.go
cmd/mato/main_test.go
internal/taskfile/metadata.go
internal/taskfile/metadata_test.go
internal/queue/resolve.go           (or inline in cancel.go if small enough)
internal/status/status_gather.go
internal/status/status_render.go
internal/status/status_json.go
internal/status/status_test.go      (or status_gather_test.go / status_render_test.go)
README.md
docs/task-format.md
docs/architecture.md
```

## Error Handling

- Wrap file I/O errors with the task name and operation context.
- Use `AtomicMove` for all file moves; never partially move task state.
- Use `atomicwrite.AppendToFile` for marker writes to avoid corrupting task
  content.
- Missing `.mato/` should fail with a clear initialization error, matching
  `pause`/`resume` semantics.
- Unknown task references return a single not-found error.
- Ambiguous task references return all candidates.
- Completed tasks return a clear refusal, not a generic error.
- Per-task errors in multi-cancel do not abort remaining tasks.

## Testing Strategy

- **Marker tests** in `internal/taskfile/metadata_test.go`:
  - `AppendCancelledRecord` writes correct format.
  - `ContainsCancelledMarker` detects presence and absence.
  - `StripFailureMarkers` removes cancelled markers.
  - Cancelled marker does not affect `CountFailureMarkers`.

- **Resolution tests** in `internal/queue/`:
  - Task found by stem.
  - Task found by stem with `.md` appended.
  - Task found by explicit `id`.
  - Task not found returns clear error.
  - Ambiguous match returns all candidates.

- **Cancel logic tests** in `internal/queue/cancel_test.go`:
  - Cancel from each non-completed state.
  - Cancel of already-failed task (marker appended, no move).
  - Cancel of completed task refused.
  - Downstream dependency warnings populated correctly.
  - Cancel of task with no dependents produces no warnings.
  - Cancel of unknown task returns not-found.

- **Command tests** in `cmd/mato/main_test.go`:
  - Subcommand registration.
  - Single and multi-task cancel.
  - Partial failure continues with remaining tasks.
  - Missing `.mato/` produces initialization error.

- **Status tests**:
  - Cancelled task shows `(cancelled)` in text.
  - Cancelled task shows `failure_kind: "cancelled"` in JSON.
  - Cancelled marker takes precedence over failure markers.

- **Validation**: `go build ./... && go vet ./... && go test -count=1 ./...`

## Risks & Mitigations

- **Race with running agent on in-progress cancel**: The agent may push
  commits or write markers between the cancel move and its own cleanup.
  Mitigation: the runner already handles missing in-progress files gracefully
  — agent work is silently discarded. Print a warning so the operator knows
  the container may still be running.

- **Race with runner poll loop**: The runner might claim or move the task
  between resolution and the cancel move. Mitigation: `AtomicMove` will fail
  if the source file is gone, and the command should report the failure
  clearly rather than corrupting state.

- **Cancelled marker proliferation**: Multiple cancel/retry cycles could
  accumulate markers. Mitigation: `StripFailureMarkers` removes all markers
  on retry, and `ContainsCancelledMarker` only needs to detect presence, not
  count.

- **Downstream tasks silently stuck**: Cancelling a task with dependents
  leaves those dependents blocked forever. Mitigation: print explicit
  warnings listing affected tasks. A future `--cascade` flag could automate
  transitive cancellation.

## Relationship to Existing Features

- Complements `mato retry`: retry requeues failed tasks, cancel withdraws
  queued tasks. Together they give operators full lifecycle control.
- Shares task resolution logic with the proposed `mato inspect` command.
  Whichever lands first should build the resolver as a reusable helper.
- Pairs with the proposed `mato pause`/`mato resume`: pause halts all new
  work, cancel targets specific tasks. They are complementary granularity
  levels.
- Uses existing terminal state (`failed/`) rather than introducing a new
  queue directory, keeping the existing two-terminal-state model intact.

## Open Questions

None for the first implementation. This plan assumes:

- Cancelled tasks go to `failed/` with a marker, not a new directory.
- `in-progress/` cancel is allowed with a warning.
- No cascading cancellation of dependents.
- `completed/` tasks cannot be cancelled.
- The `operator` actor name is hardcoded since cancel is always human-initiated.
