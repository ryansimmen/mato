# Implementation Plan: `mato log` Command

## 1. Goal

Add a `mato log` subcommand that answers "what happened recently?" by
displaying a chronological, newest-first history of durable task events
(merged, failed, rejected) across the queue. This fills the gap between
`mato status` (snapshot) and `mato inspect` (single-task deep dive) by
providing a cross-task timeline view.

## 2. Scope

### In scope

- New `log` subcommand with `--repo`, `--limit`, and `--format` flags
- Three phase-1 event types: `MERGED`, `FAILED`, `REJECTED`
- Gathering MERGED events from `messages/completions/*.json` with per-file
  resilience
- Gathering FAILED and REJECTED events by scanning task-file markers across all
  queue directories
- Normalizing all events into a common shape, sorting newest-first
- Text and JSON output renderers
- New `internal/history/` package with `Show`/`ShowTo` entry points
- New marker-parsing functions in `internal/taskfile/`
- Unit tests in `internal/history/` and `internal/taskfile/`
- CLI wiring tests in `cmd/mato/main_test.go`
- Documentation updates to `README.md`

### Out of scope

- Per-poll-cycle scheduler logs
- Generic event bus or orchestration journal
- `terminal-failure`, `cycle-failure`, or `cancelled` event types
- `--since`, `--until`, or `--task` filters
- Waiting for state-machine refactor
- Using `queue.BuildIndex()`

## 3. Design

### Event model

```go
type Event struct {
    Timestamp    time.Time `json:"timestamp"`
    Type         string    `json:"type"`                      // "MERGED", "FAILED", "REJECTED"
    TaskFile     string    `json:"task_file"`
    Title        string    `json:"title,omitempty"`
    CurrentState string    `json:"current_state,omitempty"`   // queue directory; set only for FAILED/REJECTED

    // MERGED-specific
    Branch       string   `json:"branch,omitempty"`
    CommitSHA    string   `json:"commit_sha,omitempty"`
    FilesChanged []string `json:"files_changed,omitempty"`

    // FAILED/REJECTED-specific
    Reason  string `json:"reason,omitempty"`
    AgentID string `json:"agent_id,omitempty"`
}
```

Design decisions:

- Flat struct with `omitempty` keeps JSON clean
- `CurrentState` only for FAILED/REJECTED events
- `Title` from completion-detail JSON for MERGED; from
  `frontmatter.ExtractTitle()` for markers, with
  `frontmatter.TaskFileStem(filename)` fallback

### Package boundary

- **`internal/taskfile/`**: `MarkerRecord`, `ParseFailureMarkers`,
  `ParseReviewRejectionMarkers`, `parseMarkerLine`
- **`internal/history/`**: `Show`/`ShowTo`, `Collect`, collection helpers,
  renderers. Imports: `taskfile`, `messaging` (type only), `queue`,
  `frontmatter`, `dirs`, `git`.
- **`cmd/mato/main.go`**: `newLogCmd()`, `logShowFn` hook

### Source-status model

Each data source (completions directory, queue directories) reports one of
three statuses:

| Status   | Meaning                                                                 | Example                                       |
| -------- | ----------------------------------------------------------------------- | --------------------------------------------- |
| `absent` | Directory does not exist. Acceptable but does not count as a read.      | `messages/completions/` not yet created        |
| `read`   | Directory was successfully scanned. May have returned zero events.      | `backlog/` exists and was readable, even empty |
| `failed` | Directory had a non-NotExist access error. Events from source are lost. | `failed/` exists but has permission denied     |

**Fatal threshold**: The command returns an error when NO source achieved
`read` status AND at least one source had `failed` status:

- All sources absent → success (empty history — legitimate fresh queue)
- Some absent, some read → success
- Some read, some failed → success (partial history with warnings)
- All absent, none failed → success (empty history)
- None read, at least one failed → **fatal error**

### Data collection pipeline

```
1. ShowTo: validate format
2. ShowTo: resolvedRoot = strings.TrimSpace(git.Output(repoRoot, "rev-parse", "--show-toplevel"))
3. ShowTo: tasksDir = filepath.Join(resolvedRoot, dirs.Root)
4. ShowTo: verify tasksDir exists and is directory
5. Collect: collectMergedEvents(tasksDir) → ([]Event, sourceStatus)
6. Collect: collectMarkerEvents(tasksDir) → ([]Event, anyRead bool, anyFailed bool)
7. Collect: check fatal threshold
8. Collect: merge + sort.SliceStable (timestamp desc, task_file asc, type asc)
9. Collect: apply limit
10. ShowTo: render
```

### Merged-event collection

`collectMergedEvents(tasksDir) → (events []Event, status sourceStatus)`:

1. `completionsDir := filepath.Join(tasksDir, "messages", "completions")`
2. `entries, err := os.ReadDir(completionsDir)`:
   - `os.IsNotExist(err)` → return `(nil, absent)`
   - Other error → warn to stderr, return `(nil, failed)`
   - Success → status will be `read`
3. For each `.json` entry:
   - Read bytes; on error: warn to stderr, skip
   - Unmarshal into `messaging.CompletionDetail`; on error: warn to stderr, skip
   - Skip if `detail.MergedAt.IsZero()` or `detail.TaskFile == ""` (warn to
     stderr)
   - Convert to `Event{Type: "MERGED", Timestamp: detail.MergedAt, ...}`
4. Return `(events, read)`

This differs from `ReadAllCompletionDetails()` which returns a hard error on
unreadable files (`internal/messaging/messaging.go:427`). The log command
requires per-record resilience per the idea document.

### Marker-derived event collection

`collectMarkerEvents(tasksDir) → (events []Event, anyRead bool, anyFailed bool)`:

1. For each dir in `queue.AllDirs`:
   - `dirPath := filepath.Join(tasksDir, dir)`
   - `queue.ListTaskFiles(dirPath)`:
     - `os.IsNotExist` → skip silently, continue
     - Other error → warn to stderr, set `anyFailed = true`, continue
     - Success → set `anyRead = true`
   - For each `.md` file:
     - Read raw bytes; on error: warn to stderr, skip
     - Extract title: try `frontmatter.ParseTaskData(data, path)` for body,
       then `frontmatter.ExtractTitle(name, body)`; if parse fails, use
       `frontmatter.TaskFileStem(name)`
     - `taskfile.ParseFailureMarkers(data)` → FAILED events with
       `CurrentState = dir`
     - `taskfile.ParseReviewRejectionMarkers(data)` → REJECTED events with
       `CurrentState = dir`
2. Return `(events, anyRead, anyFailed)`

Task files with malformed frontmatter still yield marker events — parsing
operates on raw `[]byte`, not on parsed frontmatter output. This mirrors how
`queue.BuildIndex()` extracts marker metadata before attempting frontmatter
parse (`internal/queue/index.go:200-230`).

### Marker parsing rules

`parseMarkerLine(line, prefix string) (MarkerRecord, bool)` accepts only when
ALL of the following are true:

1. Line starts with `prefix` (e.g. `"<!-- failure:"`)
2. Line ends with `"-->"`
3. Non-empty agent ID (first whitespace token after prefix)
4. Valid RFC3339 timestamp after ` at ` keyword
5. Non-empty reason from `failureReasonFromLine(line)`

### Sorting and stability

- Primary: Timestamp descending
- Tie-break 1: TaskFile ascending
- Tie-break 2: Type ascending (FAILED < MERGED < REJECTED)
- `sort.SliceStable` preserves source order for identical keys

## 4. Step-by-Step Breakdown

### Step 1: Add marker parsing helpers to `internal/taskfile/`

**Files modified:** `internal/taskfile/metadata.go`,
`internal/taskfile/metadata_test.go`

- `MarkerRecord` struct: `AgentID string`, `Timestamp time.Time`,
  `Reason string`
- Unexported `parseMarkerLine(line, prefix string) (MarkerRecord, bool)`:
  - Requires `-->` suffix, non-empty agent ID, valid RFC3339 timestamp,
    non-empty reason
- `ParseFailureMarkers(data []byte) []MarkerRecord`:
  - `forEachMarkerLine`, matches `failurePrefix` but not `reviewFailureStr`
  - Returns source-order records
- `ParseReviewRejectionMarkers(data []byte) []MarkerRecord`:
  - `forEachMarkerLine`, matches `reviewRejectionStr`
  - Returns source-order records

**Tests** (table-driven):

- Standard `step=...error=` format → extracted
- Em-dash `— reason` format → extracted
- Review-rejection → extracted
- Missing `-->` → skipped
- Missing agent ID → skipped
- Missing/unparseable timestamp → skipped
- Empty reason → skipped
- Code-fence markers → ignored
- Empty input → empty slice
- Multiple markers → source order
- `review-failure` NOT in `ParseFailureMarkers`

**Dependencies:** None.

### Step 2: Create `internal/history/` package

**Files created:** `internal/history/history.go`,
`internal/history/history_test.go`

**`history.go`:**

- Package comment:
  `// Package history gathers and renders chronological task event history for mato log.`
- `Event` struct, `Options{Limit int}`
- `Show(repoRoot string, limit int, format string) error` →
  `ShowTo(os.Stdout, ...)`
- `ShowTo(w io.Writer, repoRoot string, limit int, format string) error`:
  - Validate format ("text"/"json")
  - `resolvedRoot := strings.TrimSpace(git.Output(...))`
  - `tasksDir := filepath.Join(resolvedRoot, dirs.Root)`
  - Check tasksDir: NotExist →
    `".mato/ directory not found - run 'mato init' first"`, not a dir → error,
    other stat error → error
  - `Collect(tasksDir, Options{Limit: limit})`
  - Render
- `Collect`, `collectMergedEvents`, `collectMarkerEvents` — as designed above
- `renderText(w io.Writer, events []Event)`:
  - No events: `fmt.Fprintln(w, "No events found.")`
  - Format:
    `2026-03-23 10:15:00  MERGED    add-retry-logic   abc1234  (2 files changed)`
  - Timestamp: `event.Timestamp.Format("2006-01-02 15:04:05")`
  - Type: left-padded to consistent width
  - Task: `frontmatter.TaskFileStem(event.TaskFile)`
  - MERGED detail: `<sha[:7]>  (<N> file(s) changed)`
  - FAILED/REJECTED detail: reason text
- `renderJSON(w io.Writer, events []Event) error`:
  - `json.NewEncoder(w)` with 2-space indent
  - Empty events → `[]`

**`history_test.go`** (uses `t.TempDir()`):

- `TestCollect_MergedEvents`
- `TestCollect_FailedEvents`
- `TestCollect_RejectedEvents`
- `TestCollect_MixedAndSorted`
- `TestCollect_EmptyQueue`
- `TestCollect_MissingCompletionsDir`
- `TestCollect_MissingQueueDir`
- `TestCollect_MalformedCompletionJSON`
- `TestCollect_CompletionMissingRequiredFields`
- `TestCollect_MalformedFrontmatter`
- `TestCollect_LimitApplied`
- `TestCollect_LimitZeroUnlimited`
- `TestCollect_AllSourcesFailed`
- `TestCollect_CompletionAbsentAllQueueDirsFailed`
- `TestCollect_AllSourcesAbsent`
- `TestCollect_UnreadableCompletionFile` (guarded: skip on Windows)
- `TestRenderText_Format`
- `TestRenderText_NoEvents`
- `TestRenderJSON_Format`
- `TestRenderJSON_Empty`

**Dependencies:** Step 1.

### Step 3: Add `newLogCmd()` to `cmd/mato/main.go`

**Files modified:** `cmd/mato/main.go`, `cmd/mato/main_test.go`

**`main.go`:**

- `var logShowFn = history.Show`
- `newLogCmd(repoFlag *string) *cobra.Command`:
  - `Use: "log"`, `Short: "Show recent task history"`
  - `Args: usageNoArgs`
  - `--limit` (int, default 20), `--format` (string, default "text")
  - `RunE`: validate format, validate limit >= 0, `resolveRepo`,
    `logShowFn(repo, limit, format)`
  - `configureCommand(cmd)`
- Register in `newRootCmd()`

**`main_test.go`:**

- `TestLogCmd_NoExtraArgs`
- `TestLogCmd_InvalidFormat`
- `TestLogCmd_NegativeLimit`
- `TestLogCmd_DelegatesToLogShow_Defaults`
- `TestLogCmd_DelegatesToLogShow_CustomFlags`

**Dependencies:** Step 2.

### Step 4: Update documentation

**Files modified:** `README.md`

- Add `mato log` to command reference
- Flags: `--limit N` (default 20, 0 for unlimited), `--format text|json`
- Durability caveat about failure markers and `mato retry`

**Dependencies:** Steps 1-3.

## 5. File Changes Summary

| File                              | Action | Description                                                                    |
| --------------------------------- | ------ | ------------------------------------------------------------------------------ |
| `internal/taskfile/metadata.go`   | Modify | Add `MarkerRecord`, `ParseFailureMarkers`, `ParseReviewRejectionMarkers`, `parseMarkerLine` |
| `internal/taskfile/metadata_test.go` | Modify | Table-driven tests for new parsing functions                                |
| `internal/history/history.go`     | Create | Event model, Show/ShowTo, Collect, collection helpers, renderers               |
| `internal/history/history_test.go` | Create | Comprehensive unit tests                                                      |
| `cmd/mato/main.go`               | Modify | `newLogCmd`, `logShowFn`, command registration                                 |
| `cmd/mato/main_test.go`          | Modify | CLI wiring tests                                                               |
| `README.md`                       | Modify | Command reference update                                                       |

## 6. Error Handling

| Scenario                                            | Behavior                                            |
| --------------------------------------------------- | --------------------------------------------------- |
| Repo root resolution fails                          | Fatal error                                         |
| `.mato/` not found                                  | Fatal: ".mato/ directory not found - run 'mato init' first" |
| `.mato/` is not a directory                         | Fatal error                                         |
| `messages/completions/` absent                      | Source status: absent. No events, no error.          |
| `messages/completions/` non-NotExist error          | Source status: failed. Warning to stderr.            |
| Individual completion JSON unreadable               | Warning, skip                                       |
| Individual completion JSON malformed                | Warning, skip                                       |
| Completion record missing MergedAt or TaskFile      | Warning, skip                                       |
| No source achieved `read` AND any source `failed`   | Fatal: "could not read any history sources"          |
| All sources absent (fresh queue)                    | Success with empty history                           |
| Individual queue dir absent                         | Skip silently                                       |
| Individual queue dir non-NotExist error             | Source: failed for that dir. Warning to stderr.      |
| Task file unreadable                                | Warning, skip                                       |
| Task file malformed frontmatter                     | Markers extracted; title = filename stem             |
| Individual marker malformed                         | Skipped silently                                    |
| `--format` invalid                                  | Usage error                                         |
| `--limit` negative                                  | Usage error                                         |

## 7. Testing Strategy

### `internal/taskfile/metadata_test.go`

- `TestParseFailureMarkers`: all formats, edge cases
- `TestParseReviewRejectionMarkers`: standard, malformed

### `internal/history/history_test.go`

- Collection: all event types individually and mixed
- Sorting and tie-breaking
- Limiting: limit=N, limit=0
- Error resilience: missing dirs, malformed JSON, missing fields, malformed
  frontmatter
- Fatal threshold: all-failed, absent+failed, all-absent
- Rendering: text, JSON, empty

### `cmd/mato/main_test.go`

- Args, format, limit validation
- Hook delegation with default and custom flags

## 8. Risks & Mitigations

| Risk                               | Mitigation                                                              |
| ---------------------------------- | ----------------------------------------------------------------------- |
| Large queue → slow scan            | Acceptable for phase 1. Future: `--since`.                              |
| Failure markers stripped by retry   | Documented limitation.                                                 |
| Marker format evolves              | Parser skips malformed.                                                 |
| Custom completion scan diverges    | Same dir/JSON shape; per-file resilience required.                      |

## 9. Decisions

1. **`current_state`**: FAILED/REJECTED only (omitempty); omitted for MERGED.
2. **Phase-1 types**: closed to MERGED/FAILED/REJECTED.
3. **Marker warnings**: parser silent; file-level warnings to stderr.
4. **Negative `--limit`**: usage error.
5. **Repo resolution**: internal to `history.ShowTo()`.
6. **Completion scan**: custom per-file, not `ReadAllCompletionDetails()`.
7. **Fatal threshold**: error when no source `read` AND any source `failed`.
8. **Completion validation**: skip zero `MergedAt` or empty `TaskFile`.
9. **Marker acceptance**: require `-->`, agent ID, RFC3339 timestamp, non-empty
   reason.
10. **Warning format**: `fmt.Fprintf(os.Stderr, "warning: ...\n")` per repo
    convention.
