 # Proposal: `mato log`

> **Status: Implemented** — This proposal has been fully implemented.
> **Read-only:** This file is a historical snapshot. Update the source code and living docs instead.
> The text below describes the original design; see the source code for
> the current implementation.

## 1. Goal

Add a history-oriented `mato log` command that answers a simple operational
question:

"What happened recently?"

The command should read durable on-disk data, present recent task outcomes in a
compact text view by default, and support JSON output for scripting.

Phase 1 is intentionally a partial durable-outcomes view, not a complete audit
log.

## 2. Problem

Today, mato has useful runtime views, but it does not have a durable history
command.

- `mato status` is a dashboard, not a history interface
- recent messages are ephemeral and may be cleaned up
- merge completion details exist, but there is no user-facing command that turns
  them into a coherent timeline
- failure and rejection markers exist in task files, but they are not collected
  or rendered anywhere as recent history

As a result, a user who asks "what happened in the last hour?" or "which tasks
were recently merged, failed, or rejected?" has to inspect multiple filesystem
locations manually.

That is a real usability gap for operators of the queue, even if phase 1 only
covers a subset of outcome types.

## 3. Current Code State

As of the current codebase:

- there is no `internal/history/` package
- there is no `mato log` command in `cmd/mato/main.go`
- durable merge history already exists in `.mato/messages/completions/*.json`
- durable task-file markers already exist for failure and review rejection
- retry currently strips failure markers when requeuing a task from `failed/`
  back to `backlog/`, so phase-1 failure history is intentionally incomplete
- `internal/taskfile/` already contains marker scanning helpers that can be
  reused or extended
- `messaging.ReadAllCompletionDetails()` is close to what `mato log` needs, but
  it still fails fast on non-`IsNotExist` per-file read errors
- `mato status` shows a small recent snapshot, but not a durable history view

## 4. Scope

### In scope

- add a `log` subcommand
- support `--limit` and `--format text|json`
- collect recent `MERGED`, `FAILED`, and `REJECTED` events from durable sources
- reuse existing task marker semantics rather than inventing a new format
- sort events newest first
- add unit tests, CLI tests, integration tests, and user-facing docs
- ship a partial durable-outcomes view rather than a complete audit log

### Out of scope

- queue transition refactors of any kind
- new queue directories or scheduler semantics
- advanced filtering such as `--since`, `--until`, or `--task`
- using runtime snapshots under `.mato/runtime/` as history input
- using ephemeral verdict files or event messages as phase-1 history input
- adding new durable journal files in phase 1
- phase-1 `CANCELLED`, `REVIEW_FAILURE`, `TERMINAL_FAILURE`, or
  `CYCLE_FAILURE` event types

## 5. Design

### 5.1 Command shape

Add a new read-only command:

```text
mato log [--repo PATH] [--limit N] [--format text|json]
```

Behavior:

- default format is `text`
- default limit is `20`
- `--limit=0` means unlimited
- negative `--limit` values are invalid
- output is newest first

The command should follow the same pattern as existing read-only commands:

- CLI resolves the optional `--repo` argument
- the history package resolves the canonical git top-level internally
- command tests use a package-level function variable for delegation testing

### 5.2 Package boundary

Add a new package:

```go
package history

func Show(repo string, limit int, format string) error
func ShowTo(w io.Writer, repo string, limit int, format string) error
```

Responsibilities:

- `cmd/mato/main.go` validates flags and delegates
- `internal/history/` resolves `repoRoot`, derives `tasksDir`, validates that
  `.mato/` exists, collects events, sorts them, and renders output
- `internal/taskfile/` exposes typed marker parsing helpers that reuse current
  marker scanning behavior

### 5.3 Durable sources for phase 1

Phase-1 `mato log` should read only durable sources.

Included sources:

- `MERGED` from `.mato/messages/completions/*.json`
- `FAILED` from `<!-- failure: ... -->` task markers
- `REJECTED` from `<!-- review-rejection: ... -->` task markers

Explicit non-sources:

- `.mato/runtime/taskstate/*.json`
- `.mato/messages/verdict-*.json`
- `.mato/messages/events/*.json`
- `.mato/.queue`

This keeps the command grounded in data that persists across normal cleanup and
restart behavior.

### 5.4 Event model

```go
type Event struct {
    Timestamp    time.Time `json:"timestamp"`
    Type         string    `json:"type"`
    TaskFile     string    `json:"task_file"`
    Title        string    `json:"title,omitempty"`
    Branch       string    `json:"branch,omitempty"`
    CommitSHA    string    `json:"commit_sha,omitempty"`
    FilesChanged []string  `json:"files_changed,omitempty"`
    Reason       string    `json:"reason,omitempty"`
    AgentID      string    `json:"agent_id,omitempty"`
}
```

Semantics:

- `MERGED` events are sourced from completion-detail JSON
- `FAILED` events are sourced from task-file failure markers
- `REJECTED` events are sourced from task-file review-rejection markers
- phase 1 does not attempt to infer or render a current queue state for merged
  tasks

### 5.5 Marker parser shape

Add typed helpers in `internal/taskfile/`:

```go
type MarkerRecord struct {
    Timestamp time.Time
    AgentID   string
    Reason    string
}

func ParseFailureMarkers(data []byte) []MarkerRecord
func ParseReviewRejectionMarkers(data []byte) []MarkerRecord
```

These helpers should build on existing marker scanning behavior so `mato log`
matches current repository semantics, including:

- ignoring marker-like text in prose or fenced code blocks
- preserving the current standalone-marker rules
- parsing both `error=` style failure reasons and em-dash reason formats

### 5.6 Collection strategy

1. `newLogCmd` resolves `repo` and calls `history.Show(...)` or `ShowTo(...)`.
2. `history.Show` resolves `repoRoot`, derives `tasksDir`, and validates that
   `.mato/` exists.
3. The collector scans `messages/completions/` with per-file warn-and-skip
   behavior rather than failing the entire command on one unreadable file.
4. The collector scans every queue directory in `queue.AllDirs` once, reads
   task files, extracts titles best-effort, and emits one event per durable
   marker.
5. Events are sorted newest first, then by `task_file`, then by `type`.
6. `--limit` is applied after sorting.

### 5.7 Source-status behavior

Each primary source should report one of:

- `absent`: directory missing
- `read`: directory successfully scanned, even if empty
- `failed`: non-`IsNotExist` access error

Return a fatal error only when:

- no source reached `read`, and
- at least one source reached `failed`

This keeps `mato log` resilient in partially initialized or partially corrupted
repos.

### 5.8 Rendering

Text output should be compact and timeline-oriented.

Example:

```text
2026-03-20T18:41:10Z  MERGED    add-log-command.md         3f1c9a2  task/add-log-command
2026-03-20T18:12:04Z  REJECTED  tighten-review-tests.md    missing integration coverage
2026-03-20T17:55:31Z  FAILED    cleanup-reconcile.md       tests_failed
```

Guidelines:

- keep text output stable and easy to scan
- include the most important fields first
- omit empty optional fields rather than printing placeholders
- keep default text output narrower than JSON output
- when no events exist, text output should render a stable empty state such as
  `(no history)`
- JSON output should emit the full event objects
- when no events exist, JSON output should be `[]`

## 6. Delivery Plan

### Phase 1: History core

- add typed marker parsing in `internal/taskfile/`
- add `internal/history/` collection and rendering
- add unit tests for parsing, sorting, limits, empty inputs, and error
  resilience

### Phase 2: CLI wiring

- add `newLogCmd()` in `cmd/mato/main.go`
- add a `logShowFn` test hook
- add command-level tests in `cmd/mato/main_test.go`

### Phase 3: Integration and docs

- add end-to-end integration coverage for merged, failed, rejected, and empty
  history cases
- update `README.md` with command usage and examples
- update `docs/architecture.md` if needed to document history sources

## 7. File Changes

### New files

| File | Purpose |
|---|---|
| `internal/history/` | History collector, renderers, and tests for `mato log` |

### Modified files

| File | Changes |
|---|---|
| `internal/taskfile/metadata.go` | Typed marker parsing helpers for history collection |
| `internal/taskfile/metadata_test.go` | Marker parser tests |
| `cmd/mato/main.go` | `newLogCmd`, `logShowFn`, command registration |
| `cmd/mato/main_test.go` | CLI validation and delegation tests for `mato log` |
| `README.md` | User-facing command docs |
| `docs/architecture.md` | Optional documentation of durable history sources |

## 8. Error Handling

- missing `.mato/` should be rejected before collection begins
- missing queue subdirectories or missing `messages/completions/` should be
  treated as `absent`, not fatal
- unreadable or malformed individual completion files should warn and skip
- unreadable individual task files should warn and skip
- malformed markers should be skipped silently at the parser layer
- invalid `--format` values should fail fast at CLI validation
- negative `--limit` values should fail fast at CLI validation

## 9. Testing

### Unit tests

- parse failure markers in both supported reason formats
- parse review-rejection markers
- ignore marker-like text in prose and fenced code blocks
- collect mixed merged, failed, and rejected histories
- verify newest-first ordering
- verify `--limit`
- reject negative `--limit` values
- verify resilient handling of unreadable files and malformed JSON

### CLI tests

- validate flag handling
- validate delegation to `logShowFn`
- validate text and JSON format selection

### Integration tests

- merged task appears in `mato log`
- failed task appears in `mato log`
- rejected task appears in `mato log`
- empty history renders cleanly

## 10. Risks and Mitigations

| Risk | Mitigation |
|---|---|
| `mato log` accidentally reads ephemeral runtime data | Restrict phase-1 sources to completions and task markers |
| One unreadable completion file breaks the whole command | Use per-file warn-and-skip collection |
| Marker parsing diverges from existing semantics | Build on current `internal/taskfile/` scanning helpers |
| Users interpret phase 1 as a complete audit log | Document that phase 1 is a partial durable-outcomes view |

## 11. Defaults

1. Default `--limit` is `20`.
2. Default text output should stay compact and should render `(no history)` when
   no events exist; JSON output remains the richer machine-readable format.
3. Empty JSON output should be `[]`.
4. `--limit=0` means unlimited, and negative limits are invalid.
5. `CANCELLED` stays out of phase 1 and can be added in a follow-up once the
   basic command ships.
