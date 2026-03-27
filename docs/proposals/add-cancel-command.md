---
id: add-cancel-command
priority: 26
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/queue/cancel.go
  - internal/queue/cancel_test.go
  - internal/queue/resolve.go
  - internal/queue/resolve_test.go
  - internal/queue/index.go
  - internal/taskfile/metadata.go
  - internal/taskfile/metadata_test.go
  - internal/frontmatter/frontmatter.go
  - internal/frontmatter/frontmatter_test.go
  - internal/inspect/inspect.go
  - internal/inspect/inspect_test.go
  - internal/status/status.go
  - internal/status/status_render.go
  - internal/status/status_json.go
  - internal/status/status_test.go
  - internal/integration/
  - README.md
  - docs/architecture.md
  - docs/task-format.md
  - .github/skills/mato/SKILL.md
tags: [feature, cli, ux]
estimated_complexity: medium
---
# Add `mato cancel` Command

## 1. Goal

Add a `mato cancel <task-ref>...` command that withdraws one or more tasks from
the active queue by atomically moving them to `failed/` and appending a
`<!-- cancelled: -->` marker. This is the deliberate complement to `mato retry`:
retry requeues a failed task; cancel permanently withdraws a queued task.

The feature gives operators full lifecycle control without requiring manual file
manipulation, preserving task history and making cancellation intent explicit and
distinguishable from organic failures.

---

## 2. Scope

### In scope

- New Cobra subcommand: `mato cancel <task-ref>... [--repo <path>]`
- Multi-directory task resolution across all seven queue states plus parse
  failures; resolution by filename, filename stem, or explicit frontmatter `id`
- New `<!-- cancelled: -->` HTML comment marker type
- Integration with `StripFailureMarkers` so `mato retry` reverses cancellation
- Downstream dependency warnings when a cancelled task has dependents in
  `waiting/` or is still sitting dependency-blocked in `backlog/`
- `cancelled` status classification in both `mato status` and `mato inspect`,
  distinct from `terminal`, `cycle`, and `retry`
- Extraction of task-resolution logic from `internal/inspect/` into a reusable
  `queue.ResolveTask` helper shared by inspect and cancel
- Full test coverage (unit, integration lifecycle) and documentation updates

### Out of scope

- `--cascade` flag for transitive cancellation of dependents
- Killing or stopping running agent containers
- New `cancelled/` queue directory
- Cancelling tasks already in `completed/`
- Automatic undo / uncancel (that is `mato retry`)
- Doctor hygiene checks for stale cancelled markers
- Interactive confirmation prompts

---

## 3. Design

### 3.1 Cancellation Marker

Introduce a new HTML comment marker:

```
<!-- cancelled: operator at 2026-03-26T15:00:00Z -->
```

Conventions:
- Actor is always `operator` — cancel is always human-initiated.
- Timestamp is RFC3339 UTC, matching all existing marker formats.
- One marker per cancellation event; retrying strips all markers; re-cancelling
  appends another.

The marker is distinct from `<!-- failure: -->` so it does not count against
`max_retries`, and distinct from `<!-- terminal-failure: -->` so `mato status`
and `mato inspect` can classify it separately.

**`ContainsCancelledMarker` — standalone-line detection:**

```go
// ContainsCancelledMarker reports whether data contains a <!-- cancelled: -->
// marker as a standalone line (after TrimSpace). Inline occurrences embedded
// within prose do not match. A full-line example comment in the task body
// would also match; this is the same limitation accepted by
// ContainsCycleFailure and ContainsTerminalFailure.
func ContainsCancelledMarker(data []byte) bool {
    for _, line := range strings.Split(string(data), "\n") {
        if strings.HasPrefix(strings.TrimSpace(line), cancelledMarkerPrefix) {
            return true
        }
    }
    return false
}
```

This is consistent with `CountFailureMarkers`, `CountCycleFailureMarkers`, and
other line-aware functions in `internal/taskfile/metadata.go`. It is slightly
stricter than today's `ContainsCycleFailure`/`ContainsTerminalFailure` helpers,
which use substring detection, but it avoids inline false-positives while
accepting that a standalone example line would still match.

**Marker registration in two places:**

1. `failureMarkerPrefixes` in `internal/taskfile/metadata.go` — so
   `StripFailureMarkers` removes it on retry (making `mato retry` work with no
   changes to the retry command itself).
2. `managedCommentPrefixes` in `internal/frontmatter/frontmatter.go` — so the
   frontmatter parser strips it from the body field, consistent with all other
   scheduler-written runtime markers. `docs/task-format.md` states
   `.github/skills/mato/SKILL.md` must be kept in sync, so it is also updated.

### 3.2 Cancellable States and Transactional Move-Then-Append

| Source state | Action |
|---|---|
| `waiting/` | Move to `failed/`, then append cancelled marker; rollback on append failure |
| `backlog/` | Move to `failed/`, then append cancelled marker; rollback on append failure |
| `in-progress/` | Move to `failed/`, append marker, rollback on failure; warning: container may still be running |
| `ready-for-review/` | Move to `failed/`, then append cancelled marker; rollback on append failure |
| `ready-to-merge/` | Move to `failed/`, append marker, rollback on failure; warning: merge queue may still merge the branch |
| `failed/` | Append cancelled marker only (already terminal; no move needed) |
| `completed/` | Refuse: `cannot cancel <task>: task has already been merged` |

**Transactional move-then-append** (non-`failed/` states), following the pattern
established by `moveTaskToReviewWithMarker` in `internal/runner/task.go:171–193`:

```
1. AtomicMove(sourcePath → failedPath)
   → fail: return error immediately (no marker written)

2. appendCancelledRecordFn(failedPath)
   → success: return result
   → fail:
       a. attempt AtomicMove(failedPath → sourcePath)  [rollback]
          → rollback success:
              return fmt.Errorf("write cancelled marker: %w (rolled back to <state>/)", err)
          → rollback fail:
              fmt.Fprintf(os.Stderr,
                  "error: cancelled marker write failed and rollback to <state>/ also failed: %v\n",
                  rollbackErr)
              return fmt.Errorf("write cancelled marker: %w (rollback failed: %v)", err, rollbackErr)
```

This design aims to keep the repository free of half-cancelled states (tasks in
`failed/` without the `cancelled` marker). In the extremely unlikely event that
both the append and the rollback fail, the task is left in `failed/` without a
marker; this partial state is noted in §8 Risks.

For `in-progress/` tasks: the runner's `postAgentPush` and `recoverStuckTask` in
`internal/runner/task.go` both gate on the task file still being present in
`in-progress/` before acting; if the file is gone, the agent's work is silently
discarded.

For `ready-to-merge/` tasks: `loadMergeCandidates` in `internal/merge/merge.go`
does not re-check file existence before calling `mergeReadyTask`. Since the merge
is branch-based, a git branch merge can complete even after the task file is moved
away. This is an accepted narrow race; the operator warning informs the user.

### 3.3 Task Resolution — Extract to `internal/queue/`

`mato inspect` (already implemented in `internal/inspect/inspect.go`) contains
a private `resolveCandidate` function that finds a task across all queue
directories by filename stem, `.md` filename, or explicit frontmatter `id`.
Cancel needs the same resolution.

The logic is extracted to `internal/queue/resolve.go` as an exported function.
`mato inspect` is then updated to delegate to it. `mato cancel` uses it directly.

**New type:**

```go
// TaskMatch is the result of a successful ResolveTask call.
type TaskMatch struct {
    Filename     string
    State        string         // directory name: "waiting", "backlog", etc.
    Path         string         // full filesystem path
    Snapshot     *TaskSnapshot  // nil if parse failure
    ParseFailure *ParseFailure  // nil if valid snapshot
}
```

**New function:**

```go
// ResolveTask finds a single task across all queue directories.
// taskRef may be a filename ("task.md"), a stem ("task"), or an explicit
// frontmatter id. The raw ref is used for ID matching (not the ".md"-appended
// form), so that IDs containing "/" are matched correctly.
// Returns exactly one TaskMatch, or an error if zero or more than one match
// is found.
func ResolveTask(idx *PollIndex, taskRef string) (TaskMatch, error)
```

Resolution rules:
1. `TrimSpace` and reject empty string.
2. Compute `filenameRef` (append `.md` if missing) and `stemRef`.
3. For each `TaskSnapshot` in `AllDirs`: match if `filename == filenameRef`,
   `filename == rawRef`, `stem == rawRef`, `stem == stemRef`, or
   `meta.ID == rawRef`.
4. For each `ParseFailure`: match on filename and stem variants only (no ID
   matching, as metadata is unavailable).
5. Zero matches: `fmt.Errorf("task not found: %s", ref)`.
6. Multiple matches: list all as `state/filename (id: …)` and return ambiguity
   error.

To preserve today's inspect ambiguity wording, `internal/queue/resolve.go` should
also carry a small unexported helper equivalent to inspect's current
`candidateID`: it should prefer the explicit frontmatter `id` when present and
otherwise fall back to the filename stem. `ResolveTask` uses that helper when
formatting ambiguous-match errors.

`internal/inspect/inspect.go` retains its internal `candidate` struct (which has
inspect-specific fields). Its `resolveCandidate` is refactored to call
`queue.ResolveTask` and convert the result. Private helpers that move to
`resolve.go` are removed from inspect. **Existing inspect tests serve as
regression coverage for the extraction.**

### 3.4 `Cancelled` Field in Index

Add `Cancelled bool` to both `TaskSnapshot` and `ParseFailure` in
`internal/queue/index.go`. In `BuildIndex`, compute:

```go
// After os.ReadFile, before frontmatter.ParseTaskData:
cancelled := false
if readErr == nil {
    cancelled = taskfile.ContainsCancelledMarker(data)
}
```

Propagate to both code paths:
- File-read failure branch: `ParseFailure{..., Cancelled: false}`
- Frontmatter-parse failure branch: `ParseFailure{..., Cancelled: cancelled}`
- Success branch: `snap.Cancelled = cancelled`

The `taskEntry` type in `internal/status/status.go` gains a `cancelled bool`
field, populated from `snap.Cancelled` for `TaskSnapshot` entries and from
`pf.Cancelled` for `ParseFailure` entries in `listTasksFromIndex`.

### 3.5 Cancel Logic — `internal/queue/cancel.go`

```go
// appendCancelledRecordFn is the function used to append the cancelled marker.
// Tests replace it to inject failure deterministically, following the existing
// linkFn/openFileFn/readFileFn pattern in internal/queue/queue.go.
var appendCancelledRecordFn = taskfile.AppendCancelledRecord

// CancelResult carries the outcome of a single CancelTask call.
type CancelResult struct {
    Filename   string
    PriorState string   // directory name the task was in before cancellation
    Warnings   []string // dependent task paths like waiting/foo.md, deduplicated
}

// CancelTask cancels the named task reference.
func CancelTask(tasksDir, taskRef string) (CancelResult, error)
```

Algorithm:
1. TrimSpace `taskRef`; reject empty string.
2. `BuildIndex(tasksDir)` → `idx`.
3. `ResolveTask(idx, taskRef)` → `match`.
4. If `match.State == DirCompleted`: return
   `fmt.Errorf("cannot cancel %s: task has already been merged", stem)`.
5. Compute `failedPath := filepath.Join(tasksDir, DirFailed, match.Filename)`.
6. If `match.State != DirFailed`: transactional move-then-append with rollback
   (§3.2). On both success and rollback paths, the error semantics are as
   described in §3.2.
7. If `match.State == DirFailed`: `appendCancelledRecordFn(match.Path)` only.
8. Scan parseable `waiting/` tasks plus parseable `backlog/` tasks that are
   currently dependency-blocked according to
   `DependencyBlockedBacklogTasksDetailed(tasksDir, idx)`. Match references to
   the cancelled task's stem or `meta.ID`. Collect unique dependent task paths
   (`waiting/foo.md`, `backlog/bar.md`) into `CancelResult.Warnings`.
9. Return result.

### 3.6 Retry Interaction

No changes are needed to `RetryTask`. Because `cancelledMarkerPrefix` is added
to `failureMarkerPrefixes`, `StripFailureMarkers` already removes it.
`mato retry` on a cancelled task works identically to retrying any other failed
task.

### 3.7 Downstream Dependency Warnings

`CancelTask` scans parseable `waiting/` tasks and parseable backlog tasks that
are currently dependency-blocked for `Meta.DependsOn` entries containing the
cancelled task's stem or `meta.ID`. A task is counted once even if its
`depends_on` references the same task by both identifiers. Parse-failed tasks
cannot participate in the scan because `Meta.DependsOn` is unavailable.

Warnings are returned in `CancelResult.Warnings`; the CLI formats and prints them:

```
cancelled: add-auth-models.md (was in backlog/)
  warning: 2 task(s) depend on add-auth-models:
    waiting/add-login-endpoints.md
    backlog/add-auth-tests.md
  these tasks will remain blocked until add-auth-models is retried
```

### 3.8 `mato inspect` Classification for Cancelled Tasks

Two changes to `internal/inspect/inspect.go`:

**`buildFailedResult`** (snapshot in `failed/`) — add cancelled case first, setting
all three output fields:

```go
func buildFailedResult(result *inspectResult, snap *queue.TaskSnapshot) {
    result.Status = "failed"
    switch {
    case snap.Cancelled:
        result.FailureKind = "cancelled"
        result.Reason = "task was deliberately cancelled by an operator"
        result.NextStep = "use mato retry to requeue if you want to run it again"
    case snap.LastTerminalFailureReason != "":
        // ... existing terminal case ...
    case snap.LastCycleFailureReason != "":
        // ... existing cycle case ...
    default:
        // ... existing retry case ...
    }
}
```

**Parse-failure classification block** — add `pf.Cancelled` check first,
setting `FailureKind` **only** (parse-error `Reason`/`NextStep` are preserved as
the primary explanation):

```go
if pf.Cancelled {
    result.FailureKind = "cancelled"
} else if pf.LastTerminalFailureReason != "" {
    result.FailureKind = "terminal"
} else if pf.LastCycleFailureReason != "" {
    result.FailureKind = "cycle"
} else if pf.FailureCount > 0 {
    result.FailureKind = "retry"
}
```

For a cancelled parse-failed task: `Status = "invalid"`, `Reason` = parse error
details, `FailureKind = "cancelled"`. The operator sees both: the frontmatter is
broken and the task was deliberately cancelled.

### 3.9 `mato status` Classification for Cancelled Tasks

Updated failure-kind switch in `statusDataToJSON` in `status_json.go`, with
`cancelled` checked first:

```go
switch {
case task.cancelled:
    ft.FailureKind = "cancelled"
case task.lastTerminalFailureReason != "":
    ft.FailureKind = "terminal"
case task.lastCycleFailureReason != "":
    ft.FailureKind = "cycle"
default:
    ft.FailureKind = "retry"
}
```

Text rendering in `renderFailedTasks` shows `(cancelled)` for cancelled tasks.
Both `TaskSnapshot`-backed and `ParseFailure`-backed entries in the failed tasks
list use the `cancelled bool` from `taskEntry`.

### 3.10 CLI Command Shape

Follows the exact pattern of `newRetryCmd` in `cmd/mato/main.go`:

```go
// cancelTaskFn is the function used by newCancelCmd. Tests replace it to
// verify CLI flag parsing and delegation.
var cancelTaskFn = queue.CancelTask

func newCancelCmd() *cobra.Command {
    var cancelRepo string
    cmd := &cobra.Command{
        Use:   "cancel <task-ref> [task-ref...]",
        Short: "Withdraw tasks from the queue by moving them to failed/",
        Args:  usageMinimumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            repo, err := resolveRepo(cancelRepo)
            if err != nil { return err }
            repoRoot, err := resolveRepoRoot(repo)
            if err != nil { return err }
            tasksDir := filepath.Join(repoRoot, dirs.Root)

            var firstErr error
            for _, ref := range args {
                result, err := cancelTaskFn(tasksDir, ref)
                if err != nil {
                    fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
                    if firstErr == nil { firstErr = err }
                    continue
                }
                stem := strings.TrimSuffix(result.Filename, ".md")
                fmt.Printf("cancelled: %s (was in %s/)\n", result.Filename, result.PriorState)
                if result.PriorState == queue.DirInProgress {
                    fmt.Fprintf(os.Stderr,
                        "warning: agent container for %s may still be running\n", stem)
                }
                if result.PriorState == queue.DirReadyMerge {
                    fmt.Fprintf(os.Stderr,
                        "warning: merge queue may still merge %s's branch\n", stem)
                }
                if len(result.Warnings) > 0 {
                    fmt.Printf("  warning: %d task(s) depend on %s:\n",
                        len(result.Warnings), stem)
                    for _, w := range result.Warnings {
                        fmt.Printf("    %s\n", w)
                    }
                    fmt.Printf("  these tasks will remain blocked until %s is retried\n", stem)
                }
            }
            if firstErr != nil {
                return &SilentError{Err: firstErr, Code: 1}
            }
            return nil
        },
    }
    configureCommand(cmd)
    cmd.Flags().StringVar(&cancelRepo, "repo", "",
        "Path to the git repository (default: current directory)")
    return cmd
}
```

The command is wired in `newRootCmd()` with `root.AddCommand(newCancelCmd())`.

---

## 4. Step-by-Step Breakdown

Steps are ordered so each depends only on already-completed work.

**Step 1 — Add cancelled marker support to `internal/taskfile/metadata.go`**

- Add `cancelledMarkerPrefix = "<!-- cancelled:"` constant.
- Add `AppendCancelledRecord(path string) error` writing
  `\n<!-- cancelled: operator at TIMESTAMP -->\n` via `atomicwrite.AppendToFile`.
- Add `ContainsCancelledMarker(data []byte) bool` using line-aware detection.
- Append `cancelledMarkerPrefix` to `failureMarkerPrefixes`.
- Unit tests in `metadata_test.go`:
  - `TestAppendCancelledRecord_Format`
  - `TestContainsCancelledMarker_Present`, `_Absent`, `_InBodyTextInline`
    (inline in prose → no match)
  - `TestStripFailureMarkers_RemovesCancelled`
  - `TestCountFailureMarkers_IgnoresCancelled`

*Depends on: nothing.*

**Step 2 — Add `"<!-- cancelled:"` to `managedCommentPrefixes` in `internal/frontmatter/frontmatter.go`**

- Append `"<!-- cancelled:"` to the `managedCommentPrefixes` slice.
- Add test in `frontmatter_test.go`:
  `TestParsedBodyStrips_CancelledMarker` — a task file body containing a
  `<!-- cancelled: -->` comment has it stripped from the parsed `Body` field.

*Depends on: nothing.*

**Step 3 — Add `Cancelled bool` to `TaskSnapshot` and `ParseFailure` in `internal/queue/index.go`**

- Add `Cancelled bool` to `TaskSnapshot` struct.
- Add `Cancelled bool` to `ParseFailure` struct.
- In `BuildIndex`, compute `cancelled = taskfile.ContainsCancelledMarker(data)`
  immediately after `os.ReadFile` (when `readErr == nil`) and propagate to both
  the parse-failure path and the successful snapshot path.
- Add tests in `internal/queue/index_test.go` covering both snapshot-backed and
  parse-failure-backed `Cancelled` propagation.

*Depends on: Step 1.*

**Step 4 — Extract task resolution into `internal/queue/resolve.go` and refactor `internal/inspect/inspect.go`**

- Create `internal/queue/resolve.go` with `TaskMatch` struct and
  `ResolveTask(idx *PollIndex, taskRef string) (TaskMatch, error)`.
- Implement matching logic identical to inspect's current `resolveCandidate`:
  unexported `matchesRef`, `matchesParseFailure`, `taskMatchID`, and
  `stateOrder` helpers.
- Create `internal/queue/resolve_test.go` (table-driven `TestResolveTask_*`):
  `StemMatch`, `MDSuffixMatch`, `ExplicitIDMatch`, `ExplicitIDWithSlash`,
  `IDDifferentFromStem`, `NotFound`, `AmbiguousMatch`, `EmptyRef`.
- Update `internal/inspect/inspect.go`: replace the body of `resolveCandidate`
  with a call to `queue.ResolveTask(idx, taskRef)` and a conversion from
  `TaskMatch` to the internal `candidate` type; remove the now-dead private
  `matchesRef` / `matchesParseFailure` helpers. **Existing inspect tests serve
  as regression coverage.**

*Depends on: nothing.*

**Step 5 — Implement cancel logic in `internal/queue/cancel.go`**

- Add `appendCancelledRecordFn` package-level variable.
- Implement `CancelResult` struct and `CancelTask` function with transactional
  move-then-append and rollback.
- Create `internal/queue/cancel_test.go` (table-driven `TestCancelTask_*`):
  - `WaitingTask`, `BacklogTask`, `InProgressTask`, `ReadyForReview`,
    `ReadyToMerge`: verify file in `failed/` with marker, source absent.
  - `AlreadyFailed`: marker appended, file stays in `failed/`, no move error.
  - `ParseFailedTask`: parse-failed task cancelled and moved.
  - `CompletedRefused`: clear error returned.
  - `DestinationCollision`: `failed/<filename>` already exists;
    `ErrDestinationExists` wrapped as clear error; no marker written.
  - `AppendFailsAfterMove`: `appendCancelledRecordFn` set to return error;
    rollback succeeds; task back in source location; error returned.
  - `AppendFailsAfterMove_RollbackFails`: `appendCancelledRecordFn` returns
    error and `linkFn` fails on the rollback call; verify combined error and
    task left in `failed/` without marker.
  - `DownstreamWarnings`: cancelled task has dependents in `waiting/` and/or
    dependency-blocked `backlog/`.
  - `DownstreamDeduplication`: `depends_on` lists both stem and ID of same
    cancelled task; counted once.
  - `NoDownstreamWarnings`: no blocked dependents.
  - `ReCancelFailedTask`: cancelling an already-failed task twice appends
    another cancelled marker.
  - `TaskNotFound`: not-found error.
  - `EmptyName`: empty task ref rejected.
  - `ExplicitIDResolution`: cancel by frontmatter `id` different from filename
    stem.
  - `TestRetryTask_StrippedCancelledMarker`: places a cancelled task in
    `failed/`, calls `RetryTask`, verifies `<!-- cancelled: -->` marker is
    stripped and task moves to `backlog/`. This complements (not duplicates) the
    existing tests in `retry_test.go`.

*Depends on: Steps 1, 3, 4.*

**Step 6 — Wire the CLI command in `cmd/mato/main.go` and `cmd/mato/main_test.go`**

- Add `var cancelTaskFn = queue.CancelTask`.
- Add `newCancelCmd()` and wire with `root.AddCommand(newCancelCmd())` in
  `newRootCmd()`.
- Add command tests in `main_test.go`:
  - `TestCancelCmd_Registered`
  - `TestCancelCmd_SingleTask`
  - `TestCancelCmd_MultiTask`
  - `TestCancelCmd_PartialFailure` (returns `*SilentError`, matching `retry`)
  - `TestCancelCmd_CompletedRefusal`
  - `TestCancelCmd_MissingMatoDir` (no `.mato/` directory → "task not found"
    error; `BuildIndex` handles missing dirs gracefully; `ResolveTask` returns
    not-found)
  - `TestCancelCmd_InProgressWarning`
  - `TestCancelCmd_ReadyToMergeWarning`
  - `TestCancelCmd_DownstreamWarnings`
  - `TestCancelCmd_UsesRepoRootFromSubdir`

*Depends on: Step 5.*

**Step 7 — Update status classification in `internal/status/`**

- `internal/status/status.go`: add `cancelled bool` to `taskEntry`; update
  `listTasksFromIndex` to populate from `snap.Cancelled` (snapshots) and
  `pf.Cancelled` (parse failures).
- `internal/status/status_render.go`: update `renderFailedTasks` to check
  `task.cancelled` first and render `(cancelled)`.
- `internal/status/status_json.go`: update failure-kind switch in
  `statusDataToJSON` to check `task.cancelled` first.
- Tests in `status_test.go`:
  - `TestRenderFailedTasks_Cancelled`
  - `TestStatusDataToJSON_CancelledKind`
  - `TestCancelledPrecedence` (both failure and cancelled markers present)
  - `TestCancelledParseFailure` (parse-failed cancelled task in `failed/`)

*Depends on: Step 3.*

**Step 8 — Update `mato inspect` classification in `internal/inspect/inspect.go`**

- Update `buildFailedResult`: add `case snap.Cancelled:` before terminal/cycle/
  retry, setting all three fields.
- Update parse-failure classification block: add `if pf.Cancelled` check first,
  setting `FailureKind` only (`Reason`/`NextStep` stay as parse-error guidance).
- Tests in `inspect_test.go`:
  - `TestInspectCmd_CancelledTask`: cancelled snapshot in `failed/` produces
    correct `FailureKind`, `Reason`, `NextStep`.
  - `TestInspectCmd_CancelledParseFailureTask`: cancelled parse-failed task in
    `failed/` shows `Status = "invalid"`, parse error in `Reason`,
    `FailureKind = "cancelled"`.

*Depends on: Steps 3, 4.*

**Step 9 — Add integration lifecycle test in `internal/integration/`**

- Add a new test file in `internal/integration/` (package `integration_test`)
  covering the full cancel/retry lifecycle: place task in `backlog/` → cancel →
  verify `mato status` shows `failure_kind: "cancelled"` → verify `mato inspect`
  shows `failure_kind: "cancelled"` → retry → verify task back in `backlog/`.

*Depends on: Steps 5, 6, 7.*

**Step 10 — Update documentation**

- `README.md`: add `mato cancel` to the CLI commands section with usage example
  and a note that `mato retry` reverses it.
- `docs/task-format.md`: add `<!-- cancelled: operator at RFC3339 -->` to the
  runtime metadata section alongside existing markers.
- `docs/architecture.md`: document cancel as an operator-initiated terminal
  transition from any non-completed state to `failed/`.
- `.github/skills/mato/SKILL.md`: add `<!-- cancelled: -->` to the runtime
  metadata section (required to keep in sync with `docs/task-format.md`).

*Depends on: nothing (logically after implementation is stable).*

**Step 11 — Validate**

```
go build ./...
go vet ./...
go test -race -count=1 ./...
```

*Depends on: Steps 1–10.*

---

## 5. File Changes

### Create

| File | Purpose |
|---|---|
| `internal/queue/resolve.go` | `TaskMatch` type and `ResolveTask` function |
| `internal/queue/resolve_test.go` | Unit tests for `ResolveTask` |
| `internal/queue/cancel.go` | `CancelResult` type, `CancelTask` function, `appendCancelledRecordFn` seam |
| `internal/queue/cancel_test.go` | Unit tests for cancel logic + retry lifecycle test |

### Modify

| File | Change |
|---|---|
| `internal/taskfile/metadata.go` | Add `cancelledMarkerPrefix`, `AppendCancelledRecord`, `ContainsCancelledMarker`; append to `failureMarkerPrefixes` |
| `internal/taskfile/metadata_test.go` | Marker tests including inline false-positive guard |
| `internal/frontmatter/frontmatter.go` | Add `"<!-- cancelled:"` to `managedCommentPrefixes` |
| `internal/frontmatter/frontmatter_test.go` | Test that cancelled marker is stripped from parsed body |
| `internal/queue/index.go` | Add `Cancelled bool` to `TaskSnapshot` and `ParseFailure`; populate in `BuildIndex` |
| `internal/queue/index_test.go` | Add cancelled-marker propagation tests for snapshots and parse failures |
| `internal/inspect/inspect.go` | Delegate `resolveCandidate` to `queue.ResolveTask`; remove dead helpers; update `buildFailedResult` and parse-failure classification |
| `internal/inspect/inspect_test.go` | Cancelled task and cancelled parse-failure inspect tests |
| `internal/status/status.go` | Add `cancelled bool` to `taskEntry`; update `listTasksFromIndex` |
| `internal/status/status_render.go` | Update `renderFailedTasks` to handle `cancelled` |
| `internal/status/status_json.go` | Update failure-kind switch to prioritize `cancelled` |
| `internal/status/status_test.go` | Add cancelled classification tests |
| `internal/integration/` | Lifecycle integration test |
| `cmd/mato/main.go` | Add `cancelTaskFn`, `newCancelCmd()`, wire in `newRootCmd()` |
| `cmd/mato/main_test.go` | Add cancel command tests |
| `README.md` | Add `mato cancel` to CLI commands section |
| `docs/task-format.md` | Add `<!-- cancelled: -->` to runtime metadata section |
| `docs/architecture.md` | Document operator-initiated cancel transition |
| `.github/skills/mato/SKILL.md` | Add `<!-- cancelled: -->` to runtime metadata section |

---

## 6. Error Handling

- **Empty task reference**: `fmt.Errorf("task name must not be empty")`.
- **Task not found / ambiguous**: forwarded from `queue.ResolveTask` unchanged.
- **Completed task**: `fmt.Errorf("cannot cancel %s: task has already been merged", stem)`.
- **Destination collision** (`ErrDestinationExists`): `fmt.Errorf("cannot cancel %s: already exists in failed/", stem)`. No marker written.
- **TOCTOU** (source file moved between resolve and cancel): `AtomicMove` returns
  file-not-found; error returned with context; no marker written.
- **Append fails, rollback succeeds**: `fmt.Errorf("write cancelled marker to %s: %w (rolled back to %s/)", failedPath, err, match.State)`.
- **Append fails, rollback also fails**: stderr warning with both errors plus
  `fmt.Errorf("write cancelled marker: %w (rollback failed: %v)", err, rollbackErr)`.
  Task left in `failed/` without marker (partial state; operator can re-run).
- **Failed-state append fails**: error returned; no move made.
- **Missing `.mato/` directory**: `BuildIndex` handles missing directories
  gracefully (skips them); `ResolveTask` returns "task not found". No separate
  initialization error.
- **Multi-cancel partial failure**: per-task errors printed to stderr with the
  `mato error:` prefix; first error returned as `*SilentError`, matching
  existing multi-item CLI commands.

---

## 7. Testing Strategy

### Unit tests — `internal/taskfile/metadata_test.go`

- `TestAppendCancelledRecord_Format`
- `TestContainsCancelledMarker_Present`, `_Absent`, `_InBodyTextInline`
- `TestStripFailureMarkers_RemovesCancelled`
- `TestCountFailureMarkers_IgnoresCancelled`

### Unit tests — `internal/frontmatter/frontmatter_test.go`

- `TestParsedBodyStrips_CancelledMarker`

### Unit tests — `internal/queue/resolve_test.go`

- `TestResolveTask_StemMatch`, `_MDSuffixMatch`, `_ExplicitIDMatch`,
  `_ExplicitIDWithSlash`, `_IDDifferentFromStem`, `_NotFound`, `_AmbiguousMatch`,
  `_EmptyRef`

### Unit tests — `internal/queue/index_test.go`

- `TestBuildIndex_CancelledSnapshot`
- `TestBuildIndex_CancelledParseFailure`

### Unit tests — `internal/queue/cancel_test.go`

- `TestCancelTask_WaitingTask`, `_BacklogTask`, `_InProgressTask`,
  `_ReadyForReview`, `_ReadyToMerge`
- `TestCancelTask_AlreadyFailed`, `_ParseFailedTask`, `_CompletedRefused`
- `TestCancelTask_DestinationCollision`
- `TestCancelTask_AppendFailsAfterMove` (rollback succeeds)
- `TestCancelTask_AppendFailsAfterMove_RollbackFails`
- `TestCancelTask_DownstreamWarnings`, `_DownstreamDeduplication`,
  `_NoDownstreamWarnings`
- `TestCancelTask_ReCancelFailedTask`
- `TestCancelTask_TaskNotFound`, `_EmptyName`, `_ExplicitIDResolution`,
  `_ExplicitIDWithSlash`
- `TestRetryTask_StrippedCancelledMarker` (cancel/retry lifecycle)

### Command tests — `cmd/mato/main_test.go`

- `TestCancelCmd_Registered`, `_SingleTask`, `_MultiTask`, `_PartialFailure`
- `TestCancelCmd_CompletedRefusal`, `_MissingMatoDir`
- `TestCancelCmd_InProgressWarning`, `_ReadyToMergeWarning`
- `TestCancelCmd_DownstreamWarnings`, `_UsesRepoRootFromSubdir`

### Status tests — `internal/status/status_test.go`

- `TestRenderFailedTasks_Cancelled`
- `TestStatusDataToJSON_CancelledKind`
- `TestCancelledPrecedence`
- `TestCancelledParseFailure`

### Inspect tests — `internal/inspect/inspect_test.go`

- `TestInspectCmd_CancelledTask`
- `TestInspectCmd_CancelledParseFailureTask`

### Integration tests — `internal/integration/`

- Full `cancel → mato status (cancelled) → mato inspect (cancelled) → mato retry → backlog` lifecycle.

### Integration validation

```
go build ./...
go vet ./...
go test -race -count=1 ./...
```

---

## 8. Risks & Mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| Race: agent finishes `in-progress/` task after cancel | Low–medium | Runner's `postAgentPush` and `recoverStuckTask` gate on file presence in `in-progress/`; agent work silently discarded. Operator warning printed. |
| Race: merge queue merges `ready-to-merge/` task after cancel | Low | Merge is branch-based; `loadMergeCandidates` does not re-check file existence before `mergeReadyTask`. Git merge can complete; subsequent `moveTaskWithRetry` to `completed/` fails. Warning printed. Future enhancement: check `lockfile.IsHeld(mergeLockPath)` and refuse when merge lock is active. |
| Append and rollback both fail (partial cancel state) | Extremely low | Task left in `failed/` without cancelled marker. Combined error returned. Operator can re-run `mato cancel`. |
| Destination collision in `failed/` | Very low | `ErrDestinationExists` caught; clear error returned; no marker written. |
| Standalone-line false positive in `ContainsCancelledMarker` | Very low (accepted) | Same trade-off as other line-aware marker counters. Inline prose unaffected. |
| Downstream tasks silently stuck | Medium (by design) | Explicit per-cancel warnings listing affected tasks. `--cascade` documented as a future enhancement. |
| Breaking `inspect` during `resolveCandidate` extraction | Low | Existing inspect tests provide full regression coverage. Refactoring is purely structural. |

---

## 9. Open Questions

None. All decisions follow established codebase conventions:
- Line-aware marker detection matches `CountFailureMarkers` pattern.
- Transactional move-then-append with rollback follows `moveTaskToReviewWithMarker`
  in `internal/runner/task.go`.
- CLI structure matches `newRetryCmd`.
- Error handling matches the multi-item retry loop.
- State transitions use `AtomicMove` consistently.
- Test injection via function variable follows the `linkFn`/`openFileFn` pattern.
