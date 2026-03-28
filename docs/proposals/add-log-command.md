# Implementation Plan: `mato log` Command

## 1. Goal

Add a `mato log` subcommand that displays a cross-task, chronological history of
durable task outcomes. Unlike `mato status` (snapshot of current queue state) and
`mato inspect` (deep-dive into a single task), `mato log` answers time-oriented
questions: "what merged recently?", "which task failed last night?", "what was
the latest review rejection?".

Phase 1 focuses on three durable event sources that already exist in the
codebase:

- **MERGED** events from `messages/completions/*.json` files
- **FAILED** events from failure-class markers in task files in `failed/`
- **REJECTED** events from `<!-- review-rejection: ... -->` markers in task files
  across all queue directories

## 2. Scope

### In scope

- New `internal/tasklog/` package with gather, render (text), and JSON output
  logic
- New exported helper functions in `internal/taskfile/metadata.go` for per-marker
  data extraction
- New `mato log` cobra subcommand in `cmd/mato/main.go`
- Flags: `--repo` (inherited persistent flag), `--limit N` (default 20,
  0=unlimited), `--format text|json`
- Three event types: MERGED, FAILED, REJECTED
- FAILED events cover all failure-class markers: `failure`, `cancelled`,
  `terminal-failure`, `cycle-failure`
- One event per durable marker (not one per task file)
- Sorted newest-first, fully deterministic
- Unit tests for tasklog package and new taskfile helpers
- CLI tests in `cmd/mato/main_test.go`
- README.md update

### Out of scope

- Per-poll-cycle host decision logging
- Why-was-task-skipped explanations
- `--watch` mode (follow-up)
- Filtering by event type or task name (follow-up)
- Refactoring `queue/claim.go:lastFailureTime` (cleanup follow-up)
- Using `queue.BuildIndex` / `PollIndex` (see Section 3.4)

## 3. Design

### 3.1 Event Model

```go
type EventType string

const (
    EventMerged   EventType = "MERGED"
    EventFailed   EventType = "FAILED"
    EventRejected EventType = "REJECTED"
)

type Event struct {
    Timestamp    time.Time `json:"timestamp"`
    Type         EventType `json:"type"`
    TaskID       string    `json:"task_id"`
    TaskFile     string    `json:"task_file"`
    State        string    `json:"state,omitempty"`
    Detail       string    `json:"detail,omitempty"`
    CommitSHA    string    `json:"commit_sha,omitempty"`
    FilesChanged []string  `json:"files_changed,omitempty"`
    Reason       string    `json:"reason,omitempty"`
    markerOrder  int       // unexported; for deterministic sorting
}
```

**Field semantics:**

- `Timestamp` — event time
- `Type` — MERGED, FAILED, or REJECTED
- `TaskID` — from frontmatter `id` or `frontmatter.TaskFileStem(filename)`
- `TaskFile` — bare filename
- `State` — queue directory (empty for MERGED)
- `Detail` — failure sub-type for FAILED: `"failure"`, `"cancelled"`,
  `"terminal-failure"`, `"cycle-failure"`. Empty for MERGED/REJECTED.
- `CommitSHA` — merge commit SHA (MERGED only)
- `FilesChanged` — changed file paths as `[]string` (MERGED only). Matches
  `CompletionDetail.FilesChanged` and `CompletionJSON.FilesChanged`.
- `Reason` — failure reason or rejection feedback
- `markerOrder` — 0-based position among valid markers within the file
  (unexported)

### 3.2 Data Sources

**MERGED events:** One per `CompletionDetail` from
`messaging.ReadAllCompletionDetails(tasksDir)`. If `ReadAllCompletionDetails`
returns an error, emit a warning to `os.Stderr` and continue with zero MERGED
events — following the exact pattern in `status.gatherStatus`
(`internal/status/status_gather.go:140-143`). Maps:

- `MergedAt` → `Timestamp`, `TaskID` → `TaskID`, `TaskFile` → `TaskFile`
- `CommitSHA` → `CommitSHA`, `FilesChanged` → `FilesChanged` (preserved as
  `[]string`)
- `State: ""`, `Detail: ""`, `markerOrder: 0`

**FAILED events:** One per failure-class marker in `failed/`. Per file:

1. Read contents. On I/O error: warning to `os.Stderr`, skip.
2. `taskfile.ExtractAllFailedOutcomes(data)` → `[]FailedOutcome`. Returns all
   failure-class markers with timestamp, reason, sub-type, and 0-based index.
   Markers without valid timestamps are skipped.
3. If empty: skip file.
4. Parse frontmatter for TaskID (silent fallback to filename stem).
5. Emit one FAILED event per `FailedOutcome`: `State: "failed"`,
   `Detail: subType`, `markerOrder: outcome.Index`.

**REJECTED events:** One per rejection marker across all `queue.AllDirs`. Per
file per directory:

1. Read contents. On I/O error: warning to `os.Stderr`, skip.
2. `taskfile.ExtractAllReviewRejections(data)` → `[]MarkerEvent`. Skip if empty.
3. Parse frontmatter for TaskID (silent fallback).
4. Emit one REJECTED per marker: `State: dir`, `markerOrder: marker.Index`.

**Missing subdirectories:** `os.IsNotExist` from `queue.ListTaskFiles()` or
directory reads → empty events, no error, no warning.

**Non-`IsNotExist` directory errors:** Warning to `os.Stderr`, return empty
events for that source.

### 3.3 Architecture

- `internal/tasklog/tasklog.go` — Package comment, types, public API (`Show`,
  `ShowTo`), JSON renderer
- `internal/tasklog/gather.go` — `gatherEvents()`, source gatherers, helpers
- `internal/tasklog/render.go` — Text rendering with color
- `internal/tasklog/tasklog_test.go` — Unit tests

### 3.4 Why Not PollIndex

`PollIndex` stores summary metadata but not raw marker lines or per-marker
timestamps. Direct file reads are acceptable for a manually-invoked, read-only
command.

### 3.5 New Helpers in `internal/taskfile/metadata.go`

**`ExtractMarkerTimestamp(line string) (time.Time, bool)`** — Finds `" at "`,
extracts substring to next space, parses RFC3339.

**`MarkerEvent` struct:**

```go
type MarkerEvent struct {
    Timestamp time.Time
    Reason    string
    Index     int // 0-based position among valid markers
}
```

**`FailedOutcome` struct:**

```go
type FailedOutcome struct {
    Timestamp time.Time
    Reason    string
    SubType   string // "failure", "cancelled", "terminal-failure", "cycle-failure"
    Index     int    // 0-based position among valid failure-class markers
}
```

**`ExtractAllReviewRejections(data []byte) []MarkerEvent`** — Returns
`<!-- review-rejection: -->` markers as `[]MarkerEvent`. Uses
`forEachMarkerLine`, `ExtractMarkerTimestamp`, `failureReasonFromLine`. Skips
invalid timestamps.

**`ExtractAllFailedOutcomes(data []byte) []FailedOutcome`** — Scans all
failure-class markers in file order:

- `<!-- failure: -->` (excluding `<!-- review-failure: -->`) → `"failure"`,
  reason from `failureReasonFromLine`
- `<!-- cancelled: -->` → `"cancelled"`, reason `"cancelled"`
- `<!-- terminal-failure: -->` → `"terminal-failure"`, reason from
  `failureReasonFromLine`
- `<!-- cycle-failure: -->` → `"cycle-failure"`, reason from
  `failureReasonFromLine`

Returns all with valid timestamps. Uses `forEachMarkerLine`.

### 3.6 CLI Registration

`newLogCmd(repoFlag *string)` in `cmd/mato/main.go`:

- `Use: "log"`, `Short: "Show chronological task history"`, `Args: usageNoArgs`
- Flags: `--limit` (int, default 20), `--format` (string, default "text")
- Validation: format "text"|"json"; limit >= 0
- Hook: `var logShowFn = tasklog.ShowTo`
- `RunE` calls `logShowFn(os.Stdout, repo, format, limit)`

### 3.7 Repo Resolution

`ShowTo` resolves the repo root first, matching `status.ShowTo` and
`inspect.ShowTo`:

```go
func ShowTo(w io.Writer, repoRoot, format string, limit int) error {
    if format != "text" && format != "json" {
        return fmt.Errorf("unsupported format %q", format)
    }
    resolvedRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
    if err != nil { return err }
    repoRoot = strings.TrimSpace(resolvedRoot)
    tasksDir := filepath.Join(repoRoot, dirs.Root)
    // gatherEvents and render...
}
```

No explicit `.mato/` existence check — matches existing commands which let
internal functions handle missing dirs gracefully.

### 3.8 Sorting

Fully deterministic 6-key ordering:

1. **Timestamp** descending (newest first)
2. **TaskFile** ascending
3. **State** ascending
4. **TaskID** ascending
5. **Type** ascending (FAILED < MERGED < REJECTED)
6. **markerOrder** descending (later in file = newer = sorts first)

**Intent:** Keys 1-4 are the primary chronological/identity keys. Key 5 (Type)
provides deterministic ordering for the rare case of mixed-type events at the
same timestamp on the same file. Key 6 (markerOrder descending) ensures
same-type markers from the same file sort newest-first.

### 3.9 Text Rendering

```
2026-03-23 10:15:00  MERGED    add-retry-logic     abc1234  (2 files changed)
2026-03-23 10:12:00  FAILED    broken-task         agent interrupted
2026-03-23 10:10:00  FAILED    broken-task         tests_failed
2026-03-23 10:08:00  FAILED    cycle-task          circular dependency  [cycle-failure]
2026-03-23 10:07:00  FAILED    cancelled-task      cancelled
2026-03-23 10:05:00  REJECTED  fix-login-bug       missing test coverage
2026-03-23 10:00:00  MERGED    update-readme       def5678  (1 file changed)
```

Per-type columns:

- **MERGED:**
  `<timestamp>  MERGED    <TaskID>     <shortSHA>  (<len(FilesChanged)> file(s) changed)`
- **FAILED:** `<timestamp>  FAILED    <TaskID>     <reason>` + optional
  `  [<sub-type>]` for non-`"failure"` sub-types
- **REJECTED:** `<timestamp>  REJECTED  <TaskID>     <reason>`

Colors: green MERGED, red FAILED, yellow REJECTED.
Empty: dim `(no events)`.
Timestamps: UTC `2006-01-02 15:04:05`.
Short SHA: first 7 chars.

## 4. Step-by-Step Breakdown

### Step 1: Add new helpers to `internal/taskfile/metadata.go`

**Depends on:** nothing

- Add `ExtractMarkerTimestamp`, `MarkerEvent`, `FailedOutcome`,
  `ExtractAllReviewRejections`, `ExtractAllFailedOutcomes`.
- Tests in `metadata_test.go`:
  - `TestExtractMarkerTimestamp`: valid/invalid/empty.
  - `TestExtractAllReviewRejections`: 0/1/3 markers; code fences;
    same-timestamp index.
  - `TestExtractAllFailedOutcomes`: each sub-type alone; mixed; no markers;
    review-failure excluded; invalid timestamps skipped; cancelled reason.

### Step 2: Create `internal/tasklog/` — types and gather

**Depends on:** Step 1

- `tasklog.go`: types, `Show`, `ShowTo`.
- `gather.go`: `gatherEvents` (calls three source gatherers, merges, sorts,
  limits) + `gatherMergedEvents` (handles `ReadAllCompletionDetails` error as
  warning) + `gatherFailedEvents` + `gatherRejectedEvents` + `taskIDFromFile`.

### Step 3: Text renderer

**Depends on:** Step 2

- `render.go`: `renderText`, colors, pluralize, empty state.

### Step 4: JSON renderer

**Depends on:** Step 2

- `renderJSON` in `tasklog.go`. Empty → `[]`.

### Step 5: Register CLI

**Depends on:** Steps 2-4

- `newLogCmd`, `logShowFn`, `root.AddCommand`.

### Step 6: Unit tests for `internal/tasklog/`

**Depends on:** Steps 1-4

- Save/restore `color.NoColor`. Tests:
  1. TestGatherEvents_Mixed
  2. TestGatherEvents_Limit
  3. TestGatherEvents_UnlimitedZero
  4. TestShowTo_JSON — valid JSON, `files_changed` is `[]string`.
  5. TestShowTo_EmptyQueue
  6. TestGatherEvents_CancelledInFailed
  7. TestGatherEvents_TerminalFailureInFailed
  8. TestGatherEvents_CycleFailureInFailed
  9. TestGatherEvents_MultipleFailureMarkersSameTask
  10. TestGatherEvents_MultipleRejections
  11. TestGatherEvents_TieBreaking
  12. TestGatherEvents_RejectedInCompleted
  13. TestGatherEvents_MalformedFrontmatter
  14. TestGatherEvents_DuplicateTimestampsSameFile
  15. TestGatherEvents_DuplicateFilenamesAcrossDirs
  16. TestGatherEvents_MergedSameTimestampDifferentTaskID
  17. TestShowTo_InvalidFormat
  18. TestGatherEvents_MissingSubdirectory
  19. TestShowTo_SubdirectoryRepoResolution
  20. TestGatherEvents_WarningsToStderr
  21. TestGatherEvents_CompletionReadError

### Step 7: CLI tests in `cmd/mato/main_test.go`

**Depends on:** Step 5

1. TestLogCmd_InvalidFormat
2. TestLogCmd_NegativeLimit
3. TestLogCmd_NoExtraArgs
4. TestLogCmd_DelegatesToLogShow
5. TestLogCmd_DefaultLimit
6. TestLogCmd_DefaultFormat
7. TestLogCmd_Registered

### Step 8: Update README.md

Add `mato log` section mirroring existing command documentation style (usage,
description, flags, example output).

### Step 9: Verify

`go build ./... && go vet ./... && go test -count=1 ./...`

## 5. File Changes

| File                               | Action | Description                                                                                            |
| ---------------------------------- | ------ | ------------------------------------------------------------------------------------------------------ |
| `internal/taskfile/metadata.go`    | Modify | Add `ExtractMarkerTimestamp()`, `MarkerEvent`, `FailedOutcome`, `ExtractAllReviewRejections()`, `ExtractAllFailedOutcomes()` |
| `internal/taskfile/metadata_test.go` | Modify | Add tests for new functions                                                                            |
| `internal/tasklog/tasklog.go`      | Create | Event types, public API, JSON renderer                                                                 |
| `internal/tasklog/gather.go`       | Create | Event gathering                                                                                        |
| `internal/tasklog/render.go`       | Create | Text rendering                                                                                         |
| `internal/tasklog/tasklog_test.go` | Create | Unit tests                                                                                             |
| `cmd/mato/main.go`                | Modify | Add `newLogCmd()`, `logShowFn`, `root.AddCommand()`                                                    |
| `cmd/mato/main_test.go`           | Modify | CLI tests                                                                                              |
| `README.md`                       | Modify | Add `mato log`                                                                                         |

## 6. Error Handling

- **Git resolution failure:** Return git error (same as `status`/`inspect`).
- **`ReadAllCompletionDetails` error:** Warning to `os.Stderr`, continue with
  zero MERGED events (matching `status.gatherStatus` pattern).
- **Missing queue subdirectory:** Empty events, no error, no warning.
- **Non-`IsNotExist` directory errors:** Warning to `os.Stderr`, empty events
  for that source.
- **Unparseable completion JSON:** Warning to `os.Stderr` (by
  `ReadAllCompletionDetails`), skip.
- **Unparseable frontmatter:** Silent fallback to filename stem. Markers still
  processed.
- **File I/O error during task read:** Warning to `os.Stderr`, skip.
- **Malformed marker timestamps:** Skipped per marker.
- **Invalid `--limit`:** `UsageError`.
- **Invalid `--format`:** `UsageError` (CLI); `fmt.Errorf` (`ShowTo`).

## 7. Testing Strategy

Unit tests: `metadata_test.go`, `tasklog_test.go` (table-driven, `t.TempDir()`).
CLI tests: `main_test.go` (hookable function pattern). No integration tests
needed.

## 8. Risks & Mitigations

| Risk                                    | Mitigation                                                                                |
| --------------------------------------- | ----------------------------------------------------------------------------------------- |
| O(total task files) scan                | Acceptable for manual command                                                             |
| O(total completions) read               | Bounded. `ReadAllCompletionDetails` reads all, sorts; limit applied globally after merge. |
| `" at "` convention                     | Stable internal contract                                                                  |
| REJECTED for completed tasks            | Correct — shows history                                                                   |
| Multiple FAILED markers per task        | Intentional — full history                                                                |
| Not using PollIndex                     | Intentional                                                                               |
| `ReadAllCompletionDetails` hard error   | Handled as warning, continue with empty MERGED events                                     |
