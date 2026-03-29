# Combined Implementation Plan: State Machine Formalization + `mato log` Command

## 1. Goal

Deliver two tightly sequenced improvements to the mato task orchestrator:

1. **State Machine Formalization** — Centralize validation and execution of task queue state transitions so the queue state diagram is explicit, testable, and reusable instead of being enforced by scattered `AtomicMove` call sites.
2. **`mato log` Command** — Add a history-oriented CLI command that answers "what happened recently?" across the queue by gathering merged, failed, and rejected events from durable on-disk sources.

**Sequencing:** Phase A (state machine) first, Phase B (`mato log`) second. The state machine reduces transition-logic duplication and creates a hook for a future journal. `mato log` phase 1 reads existing durable artifacts.

## 2. Scope

### In scope

**Phase A — State Machine Formalization:**
- `State` type, legal-edge validation, `ValidateTransition`, `TransitionTask`, `StateFromDir`, `DirFromState` in `internal/queue/`
- Incremental migration of all canonical `AtomicMove`-based queue transitions
- Exhaustive unit tests

**Phase B — `mato log` Command:**
- `log` subcommand with `--limit N`, `--format text|json`
- Event types: MERGED, FAILED, REJECTED
- Per-file warn-and-skip with aggregate failure rule
- Every durable marker instance as a separate event
- Both failure marker formats (`step=... error=...` and `— reason`)
- Two-tier `CurrentState` resolution (history-local ID + filename indexes)
- Stable sorting, function-variable hooks, renderers, tests, docs

**Phase C — Transition Journal (Follow-up):**
- Append-only journal at wrapper level, future `mato log` enrichment

### Out of scope

- New queue directories, frontmatter fields, or queue model changes
- Database replacement, workflow DSL, scheduler logging
- Phase-1 `terminal-failure`, `cycle-failure`, `cancelled` events
- `--since`, `--until`, `--task` filters
- Dependency/overlap/retry-budget logic in state machine

## 3. Design

### 3.1 State Machine Core (`internal/queue/`)

**Why in `internal/queue/`?** The validated core needs `AtomicMove` (queue.go:208-259). `runner` and `merge` already import `queue`. No import cycles.

**New files:** `transition.go`, `transition_test.go`

**Types:** `State` string type. Constants: `StateWaiting`, `StateBacklog`, `StateInProgress`, `StateReadyReview`, `StateReadyMerge`, `StateCompleted`, `StateFailed`.

**Legal edges (14):**

| From | To |
|---|---|
| waiting | backlog |
| waiting | failed |
| backlog | waiting |
| backlog | in-progress |
| backlog | failed |
| in-progress | backlog |
| in-progress | ready-for-review |
| in-progress | failed |
| ready-for-review | backlog |
| ready-for-review | ready-to-merge |
| ready-for-review | failed |
| ready-to-merge | backlog |
| ready-to-merge | completed |
| ready-to-merge | failed |

**Illegal:** `completed` and `failed` have no outgoing edges. Operator retry is outside the model.

**Functions:** `ValidateTransition(from, to) error`, `TransitionTask(tasksDir, filename, from, to) error` (validate+`AtomicMove`), `StateFromDir(dir) (State, error)`, `DirFromState(s) string` (panics on unknown input — only called with validated constants).

**Rollback moves** are operational recovery inside wrappers, not validated transitions.

**Usage pattern:**
- **Plain moves**: `TransitionTask` directly.
- **Side-effect ordering**: `ValidateTransition` first, then bespoke sequence.

### 3.2 Canonical Transition Inventory

| # | From → To | Package/File | Class | Side-effect ordering |
|---|---|---|---|---|
| 1 | waiting → backlog | queue/reconcile.go | Plain move | `TransitionTask` |
| 2a-d | waiting → failed | queue/reconcile.go | Write-then-move | Append marker → `AtomicMove` (unparseable, duplicate ID, cycle, invalid glob) |
| 3a-b | backlog → failed | queue/reconcile.go | Write-then-move | Append marker → `AtomicMove` (unparseable, invalid glob) |
| 4 | backlog → waiting | queue/reconcile.go | Plain move | `TransitionTask` (dep-blocked demotion) |
| 5 | backlog → waiting | queue/claim.go | Plain move | `TransitionTask` (claim dep safety net) |
| 6 | backlog → in-progress | queue/claim.go | Move-then-post-write-with-rollback | `AtomicMove` → prepend claimed-by → rollback |
| 7 | in-progress → failed | queue/claim.go | Move-with-rollback | `AtomicMove` → rollback to backlog → `FailedDirUnavailableError` |
| 8 | in-progress → backlog | queue/queue.go | Move-then-post-write | `AtomicMove` → append failure (best-effort, orphan recovery) |
| 9 | any-except-failed → failed | queue/cancel.go | Move-then-post-write-with-rollback | `AtomicMove` → append cancelled → rollback |
| 10 | in-progress → ready-for-review | runner/task.go | Move-then-post-write-with-rollback | `AtomicMove` → append branch → rollback |
| 11 | in-progress → backlog | runner/task.go | Move-then-conditional-post-write | `AtomicMove` → conditional append failure |
| 12 | ready-for-review → ready-to-merge | runner/review.go | Write-then-move | Append reviewed → `AtomicMove` |
| 13 | ready-for-review → backlog | runner/review.go | Write-then-move | Append rejection → `AtomicMove` |
| 14 | ready-for-review → failed | runner/review.go | Write-then-move | Append terminal-failure → `AtomicMove` |
| 15 | ready-to-merge → completed | merge/merge.go + taskops.go | Multi-step-then-move-with-retry | Write completion detail → append merged → moveWithRetry |
| 16 | ready-to-merge → backlog/failed | merge/taskops.go + merge.go | Compute-dest-then-append-then-move | shouldFailTask → failMergeTask (append → move) |

**Excluded:** duplicate deletion, message writes, git ops, same-state renames, retry requeue, cancel marker-only (already-failed), rollback moves.

### 3.3 `mato log` Command

**Package:** `internal/history/`

**CLI:** `newLogCmd(repoFlag *string)` in `cmd/mato/main.go`. Repo resolved via `requireTasksDir` (reuses `internal/dirs/`). Format validation (`text|json`), `--limit >= 0` validation (negative rejected). `logShowFn` function variable.

**Entry points:** `Show(repo, limit, format) error` (stdout), `ShowTo(w, repo, limit, format) error` (testable).

**Event model:**

```go
type Event struct {
    Timestamp    time.Time `json:"timestamp"`
    Type         string    `json:"type"`
    TaskFile     string    `json:"task_file"`
    Title        string    `json:"title,omitempty"`
    CurrentState string    `json:"current_state,omitempty"`
    CommitSHA    string    `json:"commit_sha,omitempty"`
    Branch       string    `json:"branch,omitempty"`
    FilesChanged []string  `json:"files_changed,omitempty"`
    Reason       string    `json:"reason,omitempty"`
    AgentID      string    `json:"agent_id,omitempty"`
}
```

**Function hooks:** `readFileFn = os.ReadFile`, `readDirFn = os.ReadDir`. Tests must not use `t.Parallel()` when overriding these package-level variables.

**Data collection algorithm:**

1. Resolve `tasksDir` from repo path using `internal/dirs/`.

2. **File discovery + content cache:**
   - Track source accessibility: `queueSourceOK` and `completionsSourceOK` booleans.
   - For each dir in `queue.AllDirs`, call `readDirFn`.
     - Not exists (`os.IsNotExist`): treat as empty (confirmed empty counts as accessible).
     - Other error: warn to stderr, skip. Does NOT count as accessible.
   - For each `.md` entry, store `discoveredFile{dir, filename, fullPath}`. NOT deduplicated.
   - Read each file via `readFileFn`, cache contents in `fullPath → []byte` map. If read fails: warn, mark as unreadable.
   - A queue source counts as accessible if: (a) at least one queue directory was confirmed empty, OR (b) at least one task file from any queue directory was successfully read.

3. **Build state-resolution indexes:**
   - **Filename index** (`map[string][]string`): group by filename → directories. Unique → known. Multiple → indeterminate, warn once.
   - **ID index** (`map[string][]string`): from cached file contents, parse `frontmatter.ParseTaskData`. If `id` is non-empty, map `taskID → append directory`. Unique → known. Multiple → indeterminate. Unparseable frontmatter: no ID entry, but file still contributes markers.

4. **Merged-event `CurrentState` two-tier resolution:**
   - For each `CompletionDetail`: first ID-based lookup (if `TaskID` non-empty and unique in ID index → that directory). Then filename fallback (if `TaskFile` unique in filename index → that directory). If both resolve to different directories → empty (conflicting). If neither resolves → empty.

5. **Collect MERGED events:**
   - `readDirFn` on `messages/completions/`. Not exists: zero events (confirmed empty, accessible). Other error: warn, NOT accessible.
   - Per `.json`: `readFileFn` + `json.Unmarshal`. Fail: warn, skip.
   - MERGED event with `Timestamp` from `MergedAt`, `CurrentState` from step 4.
   - Completions source counts as accessible if: (a) directory listing confirmed empty, OR (b) at least one completion JSON was successfully read.

6. **Collect FAILED and REJECTED events:**
   - Iterate every `discoveredFile`. Use cached content. Skip unreadable.
   - Parse title via `frontmatter.ParseTaskData`. Fail: `Title` empty. **Markers still extracted** — extraction operates on raw bytes.
   - `ExtractFailureEvents(data)` → FAILED. `ExtractReviewRejectionEvents(data)` → REJECTED.
   - Each marker → separate event. `CurrentState` from filename index (indeterminate for duplicates).

7. **Aggregate failure check:**
   - If `!queueSourceOK && !completionsSourceOK`: return error `"could not access any history sources"`.
   - If at least one source was accessible (even if empty), proceed with results.

8. Assign insertion-order index.

9. Merge all events.

10. Sort: timestamp desc, task_file asc, type asc, insertion-order asc.

11. Apply limit.

12. Render.

**Marker extraction helpers** (new in `internal/taskfile/metadata.go`):

```go
type MarkerEvent struct {
    Timestamp time.Time
    AgentID   string
    Reason    string
    RawLine   string
}

func ExtractFailureEvents(data []byte) []MarkerEvent
func ExtractReviewRejectionEvents(data []byte) []MarkerEvent
```

Both `step=... error=...` and `— reason` formats. `forEachMarkerLine`. Silent skip for unparseable.

**Text output:**
```
2026-03-23 10:15:00  MERGED    add-retry-logic   abc1234  (2 files changed)
2026-03-23 10:10:00  FAILED    broken-task       agent interrupted
2026-03-23 10:05:00  REJECTED  fix-login-bug     missing test coverage
```

**Error handling summary:**
- Missing `messages/completions/` → empty (accessible)
- Unreadable/malformed completion JSON → warn, skip
- Missing queue subdirectory (`os.IsNotExist`) → empty (accessible)
- Queue subdirectory read error (non-ENOENT) → warn, skip (NOT accessible for that dir)
- Unreadable task file → warn, skip
- Malformed markers → silently skipped
- Malformed frontmatter → `Title` empty, markers still extracted
- Missing repo → fail
- All sources empty but accessible → empty result (not an error)
- ALL primary sources inaccessible (no queue source AND no completions source accessible) → fail with descriptive error
- Duplicate filename → `CurrentState` indeterminate, warn, all copies scanned
- `--limit < 0` → rejected

**Durability caveat:** Failure history only durable until `mato retry` strips markers.

### 3.4 Transition Journal (Phase C)

Wrapper-level journaling after full success. `.mato/orchestration-log/transitions.jsonl`. Best-effort, append-only.

## 4. Steps

### Phase A: State Machine Formalization

**A1:** Create `internal/queue/transition.go` + `transition_test.go`. State type, constants, legal edges, `ValidateTransition`, `TransitionTask`, `StateFromDir`, `DirFromState`. Tests: all 14 legal edges, illegal edges, file-move, round-trips.

**A2:** Migrate reconcile (#1, #2a-d, #3a-b, #4). Plain moves → `TransitionTask`. Write-then-move → `ValidateTransition` before existing sequence.

**A3:** Migrate orphan recovery (#8). `ValidateTransition` before existing move → append.

**A4:** Migrate claim (#5, #6, #7). Plain moves → `TransitionTask`. Others → `ValidateTransition` before existing sequence.

**A5:** Migrate cancel (#9). `ValidateTransition` for non-failed. Already-failed marker-only unchanged.

**A6:** Migrate task runner (#10, #11). `ValidateTransition` before existing sequences.

**A7:** Migrate review (#12, #13, #14). `ValidateTransition` before existing sequences.

**A8:** Migrate merge (#15, #16). `ValidateTransition` before existing sequences.

**A9:** Full verification: `go build ./... && go vet ./... && go test -count=1 ./...`

### Phase B: `mato log` Command

**B1:** `MarkerEvent`, `ExtractFailureEvents`, `ExtractReviewRejectionEvents` in `taskfile/metadata.go`. Tests.

**B2:** `internal/history/` package: `Event`, `discoveredFile`, function hooks, file discovery with content cache, state indexes (filename + ID), two-tier resolution, `CollectMergedEvents`, `CollectMarkerEvents`, `CollectAll` (with aggregate failure check), `Show`, `ShowTo`. Tests: sort, limits, missing dirs, unreadable files via hooks, `CurrentState` accuracy, duplicates, aggregate failure, malformed-frontmatter marker extraction.

**B3:** `render.go`: `RenderText`, `RenderJSON`. Tests.

**B4:** CLI: `logShowFn`, `newLogCmd`, validation tests (`--format`, `--limit < 0`, `logShowFn` delegation).

**B5:** Integration tests in `internal/integration/`.

**B6:** Docs: `README.md`, `docs/architecture.md`, `AGENTS.md` (single sentence: update `transition.go` when adding edges), move proposals to `implemented/` with supersession header.

### Phase C: Transition Journal (Follow-up)

C1: Journal writer. C2: Wrapper calls. C3: Log enrichment.

## 5. File Changes

### New files

| File | Purpose |
|---|---|
| `internal/queue/transition.go` | State, edges, validation, TransitionTask |
| `internal/queue/transition_test.go` | Tests |
| `internal/history/history.go` | Event, discovery, indexes, collectors, Show |
| `internal/history/history_test.go` | Tests |
| `internal/history/render.go` | Renderers |
| `internal/history/render_test.go` | Tests |
| `docs/proposals/state-machine-and-log-command.md` | Combined plan |

### Modified files

| File | Changes |
|---|---|
| `cmd/mato/main.go` | `logShowFn`, `newLogCmd`, register |
| `internal/taskfile/metadata.go` | `MarkerEvent`, extraction functions |
| `internal/taskfile/metadata_test.go` | Tests |
| `internal/queue/reconcile.go` | Validation calls |
| `internal/queue/claim.go` | Validation calls |
| `internal/queue/cancel.go` | Validation call |
| `internal/queue/queue.go` | Validation call |
| `internal/runner/task.go` | Validation calls |
| `internal/runner/review.go` | Validation calls |
| `internal/merge/taskops.go` | Validation calls |
| `internal/merge/merge.go` | Validation call |
| `README.md` | Command reference |
| `docs/architecture.md` | Transition layer |
| `AGENTS.md` | Edge-update convention |

### Moved files

| From | To |
|---|---|
| `docs/proposals/ideas/add-log-command.md` | `docs/proposals/implemented/add-log-command.md` |
| `docs/proposals/ideas/state-machine-formalization.md` | `docs/proposals/implemented/state-machine-formalization.md` |

## 6. Error Handling

### State Machine
- Illegal transition: descriptive error
- AtomicMove failure: wrapped with context
- Rollback: wrappers preserve current behavior
- Invalid state: `StateFromDir` returns error; `DirFromState` panics on unknown (only called with validated constants)

### `mato log`
- Per-file failures: warn and skip
- Aggregate failure: if no primary source accessible (neither confirmed-empty nor file-read-success), fail
- Missing-but-not-errored directories: empty, not error
- Full enumeration in section 3.3

## 7. Testing

### State Machine
- 14 legal edges, illegal edges, `TransitionTask` moves, `StateFromDir`/`DirFromState` round-trips
- Migration: existing tests must pass

### `mato log`
- Marker extraction: both formats, code fences, multiple, empty
- Collection: sort stability, insertion-order tie-break
- `CurrentState`: ID match, filename fallback, both disagree → empty, absent → empty, duplicate → empty
- Duplicate files: both copies' markers emitted
- Error paths: hook-injected read/readdir failures (no `t.Parallel()`)
- Aggregate failure: all sources errored → command fails; empty-but-accessible → empty result
- Malformed frontmatter: title empty, markers still extracted
- Limit, rendering, CLI validation
- Integration: end-to-end

## 8. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Migration breaks behavior | Incremental, existing tests |
| Wrappers bypass validation | Code review |
| Marker parsing fragility | Reuse prefixes/scanner, both formats, tests |
| State constants diverge | Round-trip tests |
| Future edge not added | AGENTS.md convention |
| Content cache memory | Acceptable: task files are small markdown |
| Duplicate files | All copies scanned, state indeterminate |

## 9. Open Questions

1. **`failed → backlog` retry:** Should operator retry eventually be modeled as a validated transition, or permanently remain a specialized requeue helper? Currently excluded because it strips markers and rewrites content rather than performing a simple move. User input welcome.
