---
id: add-pause-resume-command
priority: 45
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/runner/
  - internal/status/
  - internal/doctor/
  - internal/pause/
  - internal/integration/
  - README.md
  - docs/architecture.md
  - docs/configuration.md
tags: [feature, cli]
estimated_complexity: medium
---

> **Status: Implemented** — This proposal has been fully implemented.
> The text below describes the implemented design; see the source code for the
> current behavior.

# Add `mato pause` and `mato resume` Commands

## 1. Goal

Add a durable repo-local pause mode that stops new agent and review work without
stopping the orchestrator, while letting already-running agent tasks finish and
letting ready-to-merge work continue.

Operators need a way to temporarily halt new task claims and review launches - for
example, when debugging a live queue, coordinating a manual merge, or
investigating unexpected behavior - without disrupting the running orchestrator
process. Pause mode works as follows:

- `mato pause` stops the next claim/review cycle by writing a durable `.mato/.paused`
  sentinel.
- The running orchestrator detects the sentinel and skips claim and review phases while
  continuing to drain the merge queue and refreshing `.queue`.
- `mato resume` removes the sentinel, restoring normal polling behavior within one poll
  interval.

Exposing pause state in `mato status` and `mato doctor` ensures operators can
immediately see why queue progress has stopped.

## 2. Scope

### In scope

- New `mato pause` / `mato resume` Cobra subcommands with `--repo`.
- A shared `internal/pause` package owning all `.mato/.paused` sentinel semantics.
- Runner gating that skips task-claim and review-agent phases while paused but continues
  cleanup, reconcile, queue-manifest refresh (with all existing exclusions), and merge
  processing.
- Status text and JSON reporting for pause state.
- Doctor hygiene warnings for long-lived, malformed, or unreadable pause state.
- Documentation and test coverage for the new operational behavior.

### Out of scope

- Pausing, cancelling, or killing already-running agent containers.
- Per-task or per-agent pause controls.
- Config/env-driven pause defaults.
- Automatic pause expiry, scheduled resumes, or maintenance windows.
- Pause metadata beyond the original pause timestamp.
- Changing merge-queue behavior during pause.
- Converting pause into a new queue directory/state.

## 3. Design

### 3.1 Pause sentinel (`internal/pause`)

Use a durable sentinel file at `<repo>/.mato/.paused` (i.e.,
`filepath.Join(tasksDir, ".paused")`). Not a PID-tied lock in `.mato/.locks/`; pause
is an operator toggle that must survive orchestrator restart and must not disappear
because the creating process exits.

The sentinel contains a **single RFC3339 UTC timestamp line** representing when the
repository entered pause mode. A missing file means unpaused. When reading, trim
leading/trailing whitespace and newlines before RFC3339 parsing to tolerate the
trailing `\n` written on create.

Introduce `internal/pause/pause.go` (package `pause`). The helper owns:

- **Unexported function variables** (used only within `internal/pause` tests):
  - `statFn = os.Stat`
  - `readFileFn = os.ReadFile`
  - `writeFileFn = atomicwrite.WriteFile` (type `func(string, []byte) error`)
- **`sentinelPath(tasksDir string) string`** — `filepath.Join(tasksDir, ".paused")`.
- **Exported types**: `ProblemKind`, `State`, `PauseResult`, `ResumeResult`.
- **Exported functions**: `Read`, `Pause`, `Resume`.

**`ProblemKind`** (exported):

```go
type ProblemKind int

const (
    ProblemNone       ProblemKind = iota
    ProblemUnreadable
    ProblemMalformed
)
```

**`State`** (exported):

```go
// State holds the result of reading the .mato/.paused sentinel.
// Since is always UTC when ProblemKind == ProblemNone and Active == true.
type State struct {
    Active      bool
    Since       time.Time   // UTC; zero if Active==false or ProblemKind != ProblemNone
    ProblemKind ProblemKind
    Problem     string      // human-readable detail; non-empty when ProblemKind != ProblemNone
}
```

**`PauseResult`** (exported):

```go
// PauseResult describes what Pause() did. Since is always UTC-normalized.
type PauseResult struct {
    AlreadyPaused bool
    Repaired      bool
    Since         time.Time
}
```

**`ResumeResult`** (exported):

```go
// ResumeResult describes what Resume() did.
type ResumeResult struct {
    WasActive bool
}
```

**Decision tree for `Read`**:

| `statFn` result | `readFileFn` result | Content | Returns |
|---|---|---|---|
| `ErrNotExist` | — | — | `State{Active: false}`, nil |
| Other error | — | — | zero `State`, wrapped error |
| success | error | — | `State{Active: true, ProblemKind: ProblemUnreadable, Problem: "unreadable: <err>"}`, nil |
| success | success | not RFC3339 after trim | `State{Active: true, ProblemKind: ProblemMalformed, Problem: "invalid timestamp: <content>"}`, nil |
| success | success | valid RFC3339 after trim | `State{Active: true, Since: <ts UTC-normalized>}`, nil |

Non-`ErrNotExist` stat failures return hard errors. Once the file is known to exist,
read/parse failures fold into `State.ProblemKind` with nil error.

**`Pause(tasksDir string, now time.Time) (PauseResult, error)` semantics**:
- Missing: write `now.UTC().Format(time.RFC3339) + "\n"` via `writeFileFn`; return
  `PauseResult{Since: now.UTC()}`.
- Valid (parses cleanly): return `PauseResult{AlreadyPaused: true, Since: <parsed,
  UTC-normalized>}`, file unchanged.
- Unreadable or malformed: overwrite via `writeFileFn`; return
  `PauseResult{Repaired: true, Since: now.UTC()}`. Write failure → wrapped error naming
  both detection kind and write error.

**`Resume(tasksDir string) (ResumeResult, error)` semantics**:
- `os.Remove(sentinelPath)`. `ErrNotExist` → `ResumeResult{WasActive: false}`, nil.
  Success → `ResumeResult{WasActive: true}`, nil. Other error → wrapped error.

### 3.2 Initialization precondition

`pause` and `resume` require `<repo>/.mato/` to exist. If it is missing, return:

```
.mato/ directory not found - run 'mato init' first
```

Add `requireTasksDir(tasksDir string) error` to `cmd/mato/main.go`. It should check
`os.Stat(tasksDir)`, verify the path is a directory, and return a clear error.
`main()` will render that as `mato error: ...`. Call it before any pause interaction.

### 3.3 CLI surface

Add `newPauseCmd()` and `newResumeCmd()` to `cmd/mato/main.go`, registered from
`newRootCmd()`, following the `newCancelCmd()` / `newRetryCmd()` patterns:

```go
cobra.Command{
    Use:   "pause",
    Short: "Pause new task claims and review launches",
    Args:  usageNoArgs,
    RunE: ...
}
```

Both commands should use `configureCommand(cmd)` and follow this flow:
`resolveRepo` → `resolveRepoRoot` → `requireTasksDir` → `pause.Pause` /
`pause.Resume` → print result.

**User-facing output**:

| Scenario | Output |
|---|---|
| `mato pause` (first time) | `Paused since <RFC3339>` |
| `mato pause` (already paused, valid) | `Already paused since <RFC3339>` |
| `mato pause` (malformed/unreadable → repaired) | `Repaired pause sentinel. Paused since <RFC3339>` |
| `mato resume` (paused) | `Resumed` |
| `mato resume` (not paused) | `Not paused` |

No `--branch`, `--reason`, or `--fix` flags.

### 3.4 Runner behavior

**Test seams added to `internal/runner/runner.go`**

Following the existing `appendToFileFn` function-variable pattern:

```go
// pauseReadFn reads pause state. Var allows test injection.
var pauseReadFn = pause.Read

// pollWriteManifestFn refreshes .queue from a precomputed backlog view.
// Var allows test injection.
var pollWriteManifestFn = pollWriteManifest

// pollClaimAndRunFn runs the claim-and-run phase. Var allows test injection.
var pollClaimAndRunFn = pollClaimAndRun

// pollReviewFn runs the review phase. Var allows test injection.
var pollReviewFn = pollReview

// pollMergeFn runs the merge phase. Var allows test injection.
var pollMergeFn = pollMerge

// nowFn returns the current time. Var allows test injection for heartbeat timing.
var nowFn = time.Now
```

`pollIterate` calls `pollWriteManifestFn`, the phase functions, `pauseReadFn`, and
`nowFn` through these variables, making phase invocation and time advancement
deterministic under test.

**Separation of manifest writing from claiming**

Today `pollClaimAndRun()` (runner.go lines 633-683) performs both manifest writing
and task claiming. To keep `.queue` fresh while paused:

- Extract `pollWriteManifest(tasksDir string, failedDirExcluded map[string]struct{}, idx *queue.PollIndex) (queue.RunnableBacklogView, bool)` from `pollClaimAndRun`. Computes the view, merges `failedDirExcluded` into the exclusion set (preserving both overlap deferrals and the livelock-prevention set), calls `queue.WriteQueueManifestFromView`, returns the view and error flag. Runs unconditionally every iteration.
- Modify `pollClaimAndRun` to accept a pre-computed `view queue.RunnableBacklogView`,
  skipping manifest computation.

**Poll loop per-iteration flow** (inside `pollIterate`):

```
1.  pollCleanup(tasksDir)
2.  idx, reconcileErr  := pollReconcile(tasksDir)
3.  view, manifestErr  := pollWriteManifestFn(tasksDir, failedDirExcluded, idx)
4.  ps1, err           := pauseReadFn(tasksDir)
    (non-nil err → log warning, treat as paused)
5.  if !ps1.Active:
        claimed, claimErr = pollClaimAndRunFn(ctx, ...)
6.  *** if claimed && ctx.Err() != nil:
        return iterationResult{pauseActive: ps1.Active, ...}
        skipping review and merge (preserves existing cancellation guard) ***
7.  ps2, err           := pauseReadFn(tasksDir)
    (non-nil err → log warning, treat as paused)
8.  if !ps2.Active:
        reviewProcessed = pollReviewFn(ctx, ...)
9.  mergeCount         := pollMergeFn(repoRoot, tasksDir, branch)
10. now := nowFn()
    heartbeat/paused-heartbeat output based on priorPausedState and ps1/ps2
    iterationResult.pauseActive = ps2.Active
```

The double-read (steps 4 and 7) means a `mato resume` that lands between the claim
gate and the review gate takes effect within the same iteration.

`iterationResult.pauseActive` is set to `ps2.Active` (last observed pause state). When
pause toggles mid-iteration (unpaused at step 4, paused at step 7), `pauseActive` is
`true` so the next iteration's `priorPausedState` is `true` and the first paused
heartbeat emits immediately.

**`iterationResult`** (unexported):

```go
type iterationResult struct {
    claimedTask     bool
    reviewProcessed bool
    mergeCount      int
    pollHadError    bool
    pauseActive     bool
}
```

**Extracting the per-iteration seam**

```go
func pollIterate(
    ctx context.Context,
    env envConfig,
    run runContext,
    repoRoot, tasksDir, branch, agentID string,
    cooldown time.Duration,
    hb *idleHeartbeat,
    failedDirExcluded map[string]struct{},
    priorPausedState bool,
) iterationResult
```

`pollLoop` initializes `priorPaused := false`, calls `pollIterate` each cycle, and
passes `result.pauseActive` as `priorPausedState` on the next call.

**Heartbeat behavior while paused**

Add `pausedMessage(now time.Time, priorPaused bool) string` method to `idleHeartbeat`:
- `!priorPaused` (entering pause this iteration): reset `lastHeartbeatTime`; return
  immediate paused message.
- `priorPaused` (already paused): return non-empty only after `heartbeatInterval` has
  elapsed since `lastHeartbeatTime`; returns `""` otherwise.
- Message: `[mato] paused — run 'mato resume' to continue`.
- Pause-problem warnings share the same `lastHeartbeatTime` field — they are on exactly
  the same cadence as paused heartbeats.

Transition handling inside `pollIterate`:
- **Entering pause** (`!priorPausedState && ps1.Active`): `hb.recordActivity(now)` to
  reset idle counters; `hb.pausedMessage(now, false)` for immediate message.
- **Leaving pause** (`priorPausedState && !ps2.Active`): `hb.recordActivity(now)` so
  the next unpaused idle cycle starts clean.
- **While paused**: suppress idle message; `hb.pausedMessage(now, true)` for throttled
  heartbeat.

**Unreadable/malformed warnings in runner**: If `ps1.ProblemKind != pause.ProblemNone`
or `ps2.ProblemKind != pause.ProblemNone`: treat as paused, emit
`warning: pause sentinel: <ps.Problem>` throttled via `pausedMessage` cadence.

**`failedDirExcluded` preservation**: `pollWriteManifest` merges `failedDirExcluded`
unconditionally into the exclusion set, ensuring the manifest remains accurate while
paused.

### 3.5 Status observability

**`statusData` extension** (`internal/status/status_gather.go`):

```go
pauseState pause.State
```

**Injectable seam** in `status_gather.go`:

```go
// pauseReadFn reads pause state. Var allows test injection.
var pauseReadFn = pause.Read
```

In `gatherStatus()`, call `pauseReadFn(tasksDir)`:
- Nil error: store `data.pauseState`; append `Problem` to `data.warnings` if
  `ProblemKind != ProblemNone`.
- Non-nil error: set `data.pauseState = pause.State{Active: true, ProblemKind:
  pause.ProblemUnreadable, Problem: "stat error: <err>"}` and append to
  `data.warnings`. This ensures `mato status` reports `paused.active: true`
  consistently with the runner's "treat as paused for safety" semantics.

When `mato status` runs on a non-initialized repo, `pause.Read` returns `ErrNotExist`
from `statFn` → `State{Active: false}` → no special handling needed.

**Text rendering** (`internal/status/status_render.go`): Add pause-state line inside
`renderQueueOverview`:

```
  pause state:    paused since 2026-03-23T10:00:00Z
  pause state:    not paused
  pause state:    paused (problem: invalid timestamp: "foo")
```

Merge-queue line remains unchanged and separate so `paused + merge active` is
simultaneously visible.

**JSON output** (`internal/status/status_json.go`):

```go
// PausedJSON holds the machine-readable pause state.
type PausedJSON struct {
    Active bool   `json:"active"`
    Since  string `json:"since,omitempty"` // RFC3339; omitted when not paused or ProblemKind != ProblemNone
}

// Add to StatusJSON:
Paused PausedJSON `json:"paused"`
```

In `statusDataToJSON()`:
- `Active: false` → `PausedJSON{Active: false}`.
- `Active: true, ProblemKind: ProblemNone` → `PausedJSON{Active: true, Since: "..."}`.
- `Active: true, ProblemKind != ProblemNone` → `PausedJSON{Active: true}` (problem
  already in `"warnings"`).

Both text and JSON derive from `statusData.pauseState` to prevent drift.

### 3.6 Doctor hygiene integration

Add pause-sentinel scanning to `checkHygiene()` in `internal/doctor/checks.go`. Do not
add a new check category — fold into the existing `hygiene` check which already handles
operator-forgotten runtime artifacts.

Add `scanPauseSentinel(tasksDir string, readFn func(string) (pause.State, error)) []Finding`
called from `checkHygiene()` with `pause.Read` as the default. Doctor tests inject
controlled states via this parameter. This intentionally differs from the other
`checkHygiene` helpers, which take `fix bool`: pause findings are never fixable,
so the injected `readFn` is the more useful seam here.

| Finding code | Severity | Condition | Fixable |
|---|---|---|---|
| `hygiene.paused` | Warning | `ProblemKind == ProblemNone`, valid timestamp, age > 24h | No |
| `hygiene.invalid_pause_file` | Warning | `ProblemKind == ProblemMalformed` | No |
| `hygiene.pause_unreadable` | Warning | `ProblemKind == ProblemUnreadable` OR non-nil error from `readFn` | No |

`hygiene.paused` message includes the recorded timestamp and computed age:
```
queue has been paused since 2026-03-23T10:00:00Z (47h ago)
```

All pause findings have `Fixable: false` — auto-resume would be too surprising for an
operator toggle.

## 4. Step-by-Step Breakdown

Steps are ordered so each step depends only on already-completed prior steps.

**Step 1 — Introduce `internal/pause` package**

Create `internal/pause/pause.go` and `internal/pause/pause_test.go`.

- Implement `statFn`, `readFileFn`, `writeFileFn` (unexported function variables),
  `ProblemKind`, `State`, `PauseResult`, `ResumeResult`, `sentinelPath`, `Read`,
  `Pause`, `Resume`.
- Unit tests (table-driven, `t.TempDir()`, fixed `time.Time` values for deterministic
  timestamp assertions):
  - `Read` on missing file → `State{Active: false}`, nil error.
  - `Read` on valid sentinel → `State{Active: true, Since: fixedTime.UTC(), ProblemKind: ProblemNone}`.
  - `Read` with `readFileFn` injected to return error → `State{Active: true, ProblemKind: ProblemUnreadable}`.
  - `Read` with `readFileFn` injected to return malformed content → `State{Active: true, ProblemKind: ProblemMalformed}`.
  - `Read` with `statFn` injected to return non-`ErrNotExist` error → non-nil error.
  - `Pause(tasksDir, fixedTime)` on missing → creates file, `PauseResult{Since: fixedTime.UTC()}`.
  - `Pause` on valid sentinel → `AlreadyPaused: true`, original timestamp preserved.
  - `Pause` on malformed sentinel (injected via `readFileFn`) → `Repaired: true, Since: fixedTime.UTC()`.
  - `Pause` on malformed with `writeFileFn` injected to fail → wrapped error naming both detection kind and write error.
  - `Resume` on valid sentinel → `WasActive: true`, file removed.
  - `Resume` on missing sentinel → `WasActive: false`, nil error.

**Step 2 — Wire CLI commands in `cmd/mato/main.go`**

- Add `requireTasksDir(tasksDir string) error` checking `os.Stat(tasksDir)`.
- Implement `newPauseCmd()` using `resolveRepo` → `resolveRepoRoot` →
  `requireTasksDir` → `pause.Pause`.
- Implement `newResumeCmd()` using the same resolution chain → `pause.Resume`.
- Register both from `newRootCmd()`.
- Tests in `cmd/mato/main_test.go` using `testutil.SetupRepoWithTasks`:
  - `pause` creates sentinel; output starts with `"Paused since "`.
  - `pause` (already valid) output starts with `"Already paused since "`.
  - `pause` repairs malformed sentinel; output starts with `"Repaired"`.
  - `resume` removes valid sentinel; prints `"Resumed"`.
  - `resume` when not paused prints `"Not paused"`, exits 0.
  - Both fail clearly when `.mato/` is missing.
  - Both reject extra positional arguments.
  - Both fail clearly with invalid `--repo`.

**Step 3 — Refactor runner for testable per-iteration seam (behavior-neutral)**

This step does not change externally observable behavior; it prepares `runner.go` for
pause gating and deterministic testing.

- Add `pauseReadFn`, `pollWriteManifestFn`, `pollClaimAndRunFn`, `pollReviewFn`,
  `pollMergeFn`, and `nowFn` function variables.
- Extract `pollWriteManifest(tasksDir string, failedDirExcluded map[string]struct{}, idx *queue.PollIndex) (queue.RunnableBacklogView, bool)`.
- Modify `pollClaimAndRun` to accept pre-computed `view queue.RunnableBacklogView`.
- Add `pausedMessage(now time.Time, priorPaused bool) string` method to `idleHeartbeat`.
- Define `iterationResult`.
- Extract `pollIterate(ctx, env, run, repoRoot, tasksDir, branch, agentID, cooldown, hb *idleHeartbeat, failedDirExcluded, priorPausedState bool) iterationResult`.
- Slim `pollLoop` to initialize state (`priorPaused := false`), call `pollIterate`, and
  manage backoff/sleep.
- Update any existing `runner_test.go` tests that call `pollClaimAndRun` directly to
  pass the pre-computed view.
- Confirm `go build ./...` and `go test -count=1 ./internal/runner/...` pass with no
  behavior change.

**Step 4 — Gate runner work on pause state**

- In `pollIterate`, call `pauseReadFn` at steps 4 and 7 via the function variable.
- Preserve the post-claim cancellation guard at step 6 (current runner.go lines
  751–756).
- Use `nowFn()` for all time calls in heartbeat logic.
- Implement pause-entry/exit transitions and heartbeat output inside `pollIterate`.
- Skip `pollClaimAndRunFn` when `ps1.Active`; skip `pollReviewFn` when `ps2.Active`;
  always call `pollMergeFn`.
- Throttle problem warnings via `pausedMessage` cadence (shared `lastHeartbeatTime`).
- Tests via `pollIterate` (all seams injected via function variables):
  - Paused: `pollWriteManifestFn` runs, `pollClaimAndRunFn` NOT called, `pollReviewFn`
    NOT called, `pollMergeFn` called.
  - Paused: `failedDirExcluded` exclusions preserved in manifest.
  - `claimedTask && ctx.Err() != nil` skips review/merge (cancellation guard).
  - `priorPausedState: false`, `pauseReadFn` returns paused → immediate paused
    heartbeat emitted.
  - `priorPausedState: true`, `nowFn` advanced < `heartbeatInterval` → no message.
  - `priorPausedState: true`, `nowFn` advanced >= `heartbeatInterval` → heartbeat
    emitted.
  - `ProblemKind != ProblemNone` → runner stays paused; warning throttled per
    `heartbeatInterval`.
  - `priorPausedState: true`, `pauseReadFn` returns unpaused → `hb.recordActivity`
    called.

**Step 5 — Surface pause state in status**

- Add `pauseReadFn` function variable and `pauseState pause.State` field to
  `status_gather.go`. This is an intentional test seam even though the status
  package does not currently use function variables elsewhere; it keeps the
  hard-error path testable without permission-sensitive filesystem setups.
- In `gatherStatus`, call `pauseReadFn(tasksDir)`; handle nil-error (store + append
  `Problem` to warnings if `ProblemKind != ProblemNone`) and non-nil-error (set
  `State{Active: true, ProblemKind: ProblemUnreadable, Problem: ...}` + append warning).
- Add pause-state line to `renderQueueOverview` in `status_render.go`.
- Add `PausedJSON` struct and `Paused PausedJSON` field to `StatusJSON` in
  `status_json.go`; populate in `statusDataToJSON`.
- Tests (injecting via `pauseReadFn`):
  - `gatherStatus` populates `pauseState` for missing/valid/malformed sentinels.
  - Text output shows pause-state line.
  - JSON has `paused.active: false` when not paused (schema stability).
  - JSON has `paused.active: true` + `since` when paused.
  - JSON has `paused.active: true` without `since` when `ProblemKind != ProblemNone`.
  - Non-nil error → `paused.active: true` + warning.
  - Warning in `data.warnings` for all problem states.
  - Text and JSON derive from the same source.

**Step 6 — Extend doctor hygiene**

- Add `scanPauseSentinel(tasksDir string, readFn func(string) (pause.State, error)) []Finding`
  to `internal/doctor/checks.go`.
- Call from `checkHygiene()` passing `pause.Read` as default.
- Tests (injecting controlled states via `readFn`):
  - No finding when not paused.
  - No finding when paused recently (< 24h).
  - `hygiene.paused` when paused > 24h; message includes timestamp and age.
  - `hygiene.invalid_pause_file` for `ProblemMalformed`.
  - `hygiene.pause_unreadable` for `ProblemUnreadable`.
  - `hygiene.pause_unreadable` when `readFn` returns non-nil error.
  - `Fixable: false` on all pause findings.
  - Findings appear under the `hygiene` category in doctor output.

**Step 7 — Update user-facing documentation**

- `README.md`: Add `mato pause` / `mato resume` to the commands section; document
  `.mato/.paused` in Queue Layout table; note that merge processing continues while
  paused.
- `docs/configuration.md`: Document pause and resume subcommands with `--repo` flag;
  describe pause-state reporting in `mato status`.
- `docs/architecture.md`: Document the pause gate in the polling-loop description;
  document `.mato/.paused` as an optional operator sentinel in the filesystem layout
  section.

**Step 8 — Add integration-level test coverage**

In `internal/integration/` (package `integration_test`), add a focused test
following the existing integration style:

```go
func TestPauseResume_StatusReflectsPauseState(t *testing.T) {
    repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

    // Pause the repo.
    if _, err := pause.Pause(tasksDir, time.Now().UTC()); err != nil {
        t.Fatalf("Pause: %v", err)
    }

    // Verify status reports paused.
    var buf bytes.Buffer
    if err := status.ShowJSON(&buf, repoRoot); err != nil {
        t.Fatalf("ShowJSON: %v", err)
    }
    var result status.StatusJSON
    if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
        t.Fatalf("Unmarshal: %v", err)
    }
    if !result.Paused.Active {
        t.Errorf("expected paused.active=true after pause, got false")
    }

    // Resume and verify.
    buf.Reset()
    if _, err := pause.Resume(tasksDir); err != nil {
        t.Fatalf("Resume: %v", err)
    }
    if err := status.ShowJSON(&buf, repoRoot); err != nil {
        t.Fatalf("ShowJSON: %v", err)
    }
    if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
        t.Fatalf("Unmarshal: %v", err)
    }
    if result.Paused.Active {
        t.Errorf("expected paused.active=false after resume, got true")
    }
}
```

**Step 9 — Final validation**

```bash
go build ./... && go vet ./... && go test -count=1 ./...
```

## 5. File Changes

```text
internal/pause/pause.go          [NEW]
  Package pause. ProblemKind, State, PauseResult, ResumeResult types.
  statFn, readFileFn, writeFileFn unexported function variables.
  sentinelPath, Read, Pause, Resume functions.

internal/pause/pause_test.go     [NEW]
  Table-driven unit tests for full sentinel lifecycle, all error paths.
  Fixed-time timestamp assertions. Injection of statFn/readFileFn/writeFileFn.

cmd/mato/main.go                 [MODIFY]
  Add requireTasksDir helper.
  Add newPauseCmd() and newResumeCmd().
  Register both from newRootCmd().

cmd/mato/main_test.go            [MODIFY]
  Tests for command wiring, preconditions, idempotence, repair, output shape.

internal/runner/runner.go        [MODIFY]
  Add pauseReadFn, pollWriteManifestFn, pollClaimAndRunFn, pollReviewFn,
  pollMergeFn, nowFn vars.
  Extract pollWriteManifest().
  Modify pollClaimAndRun() to accept pre-computed view.
  Add pausedMessage() to idleHeartbeat.
  Define iterationResult.
  Extract pollIterate() with priorPausedState parameter.
  Slim pollLoop().
  Preserve post-claim ctx.Err() cancellation guard inside pollIterate.
  Gate claim/review phases on pause state.
  Add paused-heartbeat and transition logic.
  Throttle pause warnings via pausedMessage cadence.

internal/runner/runner_test.go   [MODIFY]
  Update tests for new pollClaimAndRun signature (pass pre-computed view).
  Add paused-iteration tests via pollIterate seam with all seams injected.

internal/status/status_gather.go [MODIFY]
  Add pauseReadFn function variable.
  Add pauseState pause.State to statusData.
  Call pauseReadFn in gatherStatus.
  Handle both nil-error and non-nil-error paths.

internal/status/status_render.go [MODIFY]
  Add pause-state line in renderQueueOverview.

internal/status/status_json.go   [MODIFY]
  Add PausedJSON struct.
  Add Paused PausedJSON field to StatusJSON.
  Populate in statusDataToJSON.

internal/status/status_gather_test.go [MODIFY]
  Tests for pause state gathering via pauseReadFn injection.
  Tests for nil-error, non-nil-error, and ProblemKind paths.

internal/status/status_render_test.go [MODIFY]
  Tests for pause-state line in text output.

internal/status/status_test.go   [MODIFY]
  End-to-end tests covering paused state in text and JSON output.

internal/doctor/checks.go        [MODIFY]
  Add scanPauseSentinel(tasksDir string, readFn func(string)(pause.State,error)) []Finding.
  Call from checkHygiene() with pause.Read as default.

internal/doctor/checks_test.go   [MODIFY]
  Tests for all pause hygiene findings via readFn injection.
  Coverage for stale, malformed, unreadable, stat-error, and Fixable: false.

internal/integration/pause_resume_test.go [NEW]
  Add TestPauseResume_StatusReflectsPauseState using pause and status packages.

README.md                        [MODIFY]
  Document pause/resume commands in commands section.
  Document .mato/.paused in Queue Layout table.
  Note merge continues while paused.

docs/configuration.md            [MODIFY]
  Document pause and resume subcommands and pause-state reporting.

docs/architecture.md             [MODIFY]
  Document pause gate in polling-loop description.
  Document .mato/.paused as optional operator sentinel in filesystem layout.
```

## 6. Error Handling

- Wrap all file I/O errors with path and operation:
  `fmt.Errorf("read pause sentinel %s: %w", path, err)`.
- `writeFileFn` (defaulting to `atomicwrite.WriteFile`) for all sentinel creation and
  repair — never partial writes.
- Missing sentinel (`ErrNotExist` from `statFn`) → `State{Active: false}`, nil error.
- Non-`ErrNotExist` stat failure → wrapped error; consumers treat as paused for safety.
- File read failure → `State{Active: true, ProblemKind: ProblemUnreadable, ...}`, nil
  error.
- Malformed content → `State{Active: true, ProblemKind: ProblemMalformed, ...}`, nil
  error.
- `Resume` with `ErrNotExist` → `ResumeResult{WasActive: false}`, nil.
- `requireTasksDir` fails with a clear message when `.mato/` is missing or is not a
  directory.
- Malformed sentinel + repair write failure → wrapped error naming both detection kind
  and write error.
- Runner: non-nil `pauseReadFn` error → log warning to stderr, treat as paused.
- Runner: `ProblemKind != ProblemNone` → throttled stderr warning via `pausedMessage`
  cadence (shared `lastHeartbeatTime`).
- Status: non-nil `pauseReadFn` error → set `State{Active: true, ProblemKind:
  ProblemUnreadable, Problem: "stat error: <err>"}` + append to `data.warnings`.
- Doctor: all pause findings have `Fixable: false`; non-nil `readFn` error →
  `hygiene.pause_unreadable`.

## 7. Testing Strategy

**`internal/pause` unit tests** (`pause_test.go`):
- Table-driven, `t.TempDir()`, fixed `time.Time` values for deterministic assertions.
- All I/O failure scenarios use `statFn`/`readFileFn`/`writeFileFn` injection.
- Covers full decision tree for `Read`, `Pause`, `Resume` including all edge cases.

**`cmd/mato` command tests** (`main_test.go`):
- Use `testutil.SetupRepoWithTasks` for initialized `.mato/` directory.
- Output shape assertions (prefix matching, not exact timestamps).
- Cover all output scenarios, precondition validation, argument rejection.

**`internal/runner` iteration tests** (`runner_test.go`):
- Via `pollIterate` seam with all phase function variables and `nowFn` injected.
- Deterministic phase-invocation assertions (claim/review/merge called or not called).
- Heartbeat-throttle assertions via `nowFn` time advancement.
- Post-claim cancellation guard test.
- Race detector always enabled (`go test -race`).

**`internal/status` tests**:
- Via `pauseReadFn` injection.
- All three pause states (unpaused, paused-valid, paused-malformed) plus non-nil error.
- Text line coverage; JSON schema stability (`active: false` when not paused); warning
  surfacing; text/JSON alignment from same source.

**`internal/doctor` tests**:
- Via `readFn` injection for all problem kinds.
- All finding codes; `Fixable: false` everywhere; findings under `hygiene`.

**Integration tests** (`internal/integration/`):
- `TestPauseResume_StatusReflectsPauseState` via `pause` and `status` packages.

**Validation**:
```bash
go build ./... && go vet ./... && go test -count=1 ./...
```

## 8. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Race between `mato pause` and poll loop | Gate claim and review separately at phase boundaries; pause takes effect within one poll interval; running tasks never interrupted. |
| Stale `.queue` while paused | `pollWriteManifest` runs unconditionally every iteration and merges both `view.Deferred` and `failedDirExcluded` into the exclusion set. |
| Confusing operator output while paused | Pause-state lines in `mato status`; paused heartbeat in runner; idle messaging suppressed while paused. |
| Text/JSON status drift | Both derive from `statusData.pauseState`; parity tests enforce alignment. |
| Accidental auto-resume via `mato doctor --fix` | All pause findings have `Fixable: false`. |
| Sentinel semantics drift across packages | All sentinel access through `internal/pause`; consumers never construct the path directly. |
| Untestable runner heartbeat transitions | Pause transition logic lives inside `pollIterate`; `nowFn` injection for time control; direct `priorPausedState` injection. |
| Post-claim cancellation regression | Existing guard (`if claimedTask && ctx.Err() != nil`) preserved at step 6 in `pollIterate`. |
| Permission-based tests fragile in containers | All failure paths use function-variable injection instead of `chmod 0o000`. |
| `pollClaimAndRun` signature change breaks existing tests | Existing tests updated atomically in Step 3 to pass pre-computed view. |

## 9. Open Questions

None required before implementation. The intended semantics are:

- `pause` halts new task claims and new AI review runs.
- Merge processing (draining `ready-to-merge/`) continues.
- Pause metadata is limited to the original pause timestamp.
- A "drain mode" that continues review agents while paused is explicitly out of scope
  and requires a deliberate scope change if desired later.
