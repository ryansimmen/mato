# Implementation Plan: Resume Work and Review Sessions

## 1. Goal

Reduce retry churn after review rejection and transient agent failures by fixing
two cold-start behaviors:

1. **Worktree continuity**: When a task already has a task branch, the next work
   attempt resumes from that branch tip instead of creating a fresh branch.
2. **Review continuity**: Follow-up reviews receive explicit host-curated context
   about the previous review.

Two independently shippable phases. Phase 3 (optional work-session resume) is
out of scope.

## 2. Scope

### In scope

- Phase 1: Fence-aware branch marker parsing across all readers; branch identity
  reuse at claim time; resume from existing task branch tip; single-marker
  normalization; adjusted commit detection with explicit SHA error handling
- Phase 2: Review context injection; runtime metadata with load-modify-save;
  centralized cleanup

### Out of scope

- Recovering unpublished work from deleted temp clones
- Persistent reviewer conversations / cross-task session reuse
- Changing queue states, retry budgets, or review semantics
- Phase 3 (optional work-session resume)

## 3. Design

### 3.1 Phase 1: Git Branch Resume for Work Runs

#### 3.1.1 Fence-Aware Branch Marker Parsing (taskfile/taskfile.go)

Add `ParseBranchMarkerLine(data []byte) (string, bool)` using
`forEachMarkerLine`. Only honors `<!-- branch: ... -->` on standalone lines
outside code fences. Follows the pattern of `ParseClaimedBy`,
`CountFailureMarkers`, etc.

`ParseBranchComment` retained as low-level regex helper but no longer called by
any branch-identity reader.

#### 3.1.2 Branch Marker Replacement (taskfile/taskfile.go)

Add `ReplaceBranchMarkerLine(data []byte, newBranch string) (result []byte, found bool, replaced bool)`:

- No standalone marker → data unchanged, found=false
- Marker matches → data unchanged, found=true, replaced=false
- Marker differs → first standalone marker replaced in-place, found=true,
  replaced=true

#### 3.1.3 Migrate All Branch-Identity Readers

| Consumer | Location | Change |
|----------|----------|--------|
| `ParseBranch` | taskfile/metadata.go | `ParseBranchComment` → `ParseBranchMarkerLine` |
| `readBranchFromFile` | queue/claim.go:117 | `ParseBranchComment` → `ParseBranchMarkerLine` |
| `BuildIndex` | queue/index.go:~200 | `ParseBranchComment` → `ParseBranchMarkerLine` |

Downstream consumers automatically become fence-aware: merge.go:92,
review.go:164, CollectActiveBranches.

Migration safety: `ParseBranchMarkerLine` is strictly more conservative than
`ParseBranchComment` — it rejects matches inside code fences and only processes
standalone marker lines. This cannot break correct behavior; it only eliminates
false positives from body-embedded markers.

#### 3.1.4 Exported Branch Marker Writer (queue/claim.go)

Add `WriteBranchMarker(taskPath, branch string) error`:

1. Read → `ParseBranchMarkerLine`
2. Matches → nil (no-op)
3. Differs → `ReplaceBranchMarkerLine` + `atomicwrite.WriteFile`
4. Not found → `writeBranchComment` (insert after claimed-by)

#### 3.1.5 Branch Identity Preservation with Collision Safety (queue/claim.go)

In `SelectAndClaimTask`, after claimed-by write:

1. Read file, check for standalone branch marker
2. Found + no collision with `activeBranches` → reuse (skip write)
3. Found + collision → derive new name
4. Not found → derive new name
5. Deriving: **MANDATORY** `WriteBranchMarker` — failure triggers claim rollback
6. Reusing: best-effort skip (marker already correct)

**Mandatory vs. best-effort boundary**: When the selected branch differs from
the file's current marker (or no marker exists), the marker write is mandatory.
Without this, downstream consumers (index, merge, review) would see the wrong
branch. When the existing marker already matches, no write is needed.

#### 3.1.6 Branch Content Resume (runner/task.go)

Add `checkoutOrCreateBranch(cloneDir, branch string) error`:

1. **Probe**: `git ls-remote --exit-code --heads origin refs/heads/<branch>`
   - Exit 0 → exists
   - Exit 2 → confirmed absent
   - Other → hard error

2. **If exists**:
   ```
   git fetch origin <branch>
   // Validate: refs/remotes/origin/<branch> exists after fetch
   git checkout -B <branch> origin/<branch>
   ```
   The explicit `origin/<branch>` start-point ensures the local branch is reset
   to the remote task branch tip, not the clone's current HEAD. Matches the
   pattern in `internal/git/git.go:138`.

3. **If absent**: `git checkout -b <branch>` (fresh from HEAD = target branch)

4. **If ambiguous**: hard error (prevents silent data loss)

Function variable: `var checkoutOrCreateBranchFn = checkoutOrCreateBranch`

#### 3.1.7 Starting-Tip Capture (runner/task.go)

After `checkoutOrCreateBranch`, before agent launch:

```go
startingTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
if err != nil {
    return fmt.Errorf("capture starting tip after branch checkout: %w", err)
}
startingTip = strings.TrimSpace(startingTip)
```

**Hard error**: If `rev-parse` fails, `runOnce` returns an error. The task stays
in in-progress/ and `recoverStuckTask` moves it back to backlog/. This is safe
because no agent was launched and no work was done.

Thread `startingTip` to `postAgentPush` as a parameter.

#### 3.1.8 Adjusted Commit Detection (runner/task.go)

In `postAgentPush`, replace `git log targetBranch..HEAD`:

```go
currentTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
if err != nil {
    return fmt.Errorf("determine current branch tip after agent exit: %w", err)
}
if strings.TrimSpace(currentTip) == startingTip {
    return nil  // no new commits
}
```

**Hard error**: If `rev-parse` fails, `postAgentPush` returns an error. The
caller (`runOnce`) preserves the clone and returns the error. `recoverStuckTask`
then moves the task back to backlog/.

Keep `targetBranch..HEAD` for changed-files detection only (conflict warnings,
completion messages).

#### 3.1.9 Marker Normalization in moveTaskToReviewWithMarker (runner/task.go)

Replace `appendToFileFn` with `writeBranchMarkerFn` (function variable →
`queue.WriteBranchMarker`). Tests override for failure injection, matching the
existing `appendToFileFn` pattern.

#### 3.1.10 Legacy Multi-Marker Files

New code produces single markers. Legacy files with pre-existing duplicate
markers (from prior append-only behavior): `ParseBranchMarkerLine` returns the
first standalone marker; `ReplaceBranchMarkerLine` replaces that first marker.
No retroactive deduplication.

#### 3.1.11 Testing Strategy for Phase 1

**`internal/taskfile/taskfile_test.go`** (9 tests):

- `TestParseBranchMarkerLine_StandaloneLine`
- `TestParseBranchMarkerLine_InsideCodeFence`
- `TestParseBranchMarkerLine_InsideProseText`
- `TestParseBranchMarkerLine_MultipleStandaloneMarkers`
- `TestParseBranchMarkerLine_NoMarker`
- `TestReplaceBranchMarkerLine_NoExisting`
- `TestReplaceBranchMarkerLine_SameValue`
- `TestReplaceBranchMarkerLine_DifferentValue`
- `TestReplaceBranchMarkerLine_InsideCodeFence`

**`internal/queue/claim_test.go`** (6 tests):

- `TestSelectAndClaimTask_ReusesExistingBranchMarker`
- `TestSelectAndClaimTask_DerivesBranchWhenNoMarker`
- `TestSelectAndClaimTask_IgnoresBranchMarkerInCodeFence`
- `TestSelectAndClaimTask_NormalizesMarkerOnCollision`
- `TestSelectAndClaimTask_MarkerWriteFailure_RollsBack`
- `TestSelectAndClaimTask_SkipsWriteWhenMarkerMatches`

**`internal/runner/task_test.go`** (12 tests):

- `TestCheckoutOrCreateBranch_ExistingBranch` — verifies HEAD == origin tip
- `TestCheckoutOrCreateBranch_ConfirmedAbsentBranch`
- `TestCheckoutOrCreateBranch_AmbiguousFetchFailure`
- `TestCheckoutOrCreateBranch_PostFetchRefValidation`
- `TestRunOnce_StartingTipCaptureFails`
- `TestRunOnce_ResumedBranchNoNewCommit`
- `TestRunOnce_ResumedBranchWithNewCommit`
- `TestRunOnce_FreshBranchWithNewCommit`
- `TestRunOnce_FreshBranchNoCommit`
- `TestMoveTaskToReviewWithMarker_WriteFails_RollsBack`
- `TestMoveTaskToReviewWithMarker_ExistingMarkerReplaced`
- `TestMoveTaskToReviewWithMarker_NoExistingMarker`

**`internal/integration/resume_test.go`** (1 test):

- `TestReviewRejectionRetryPreservesBranch`

### 3.2 Phase 2: Review Continuity + Runtime Metadata

#### 3.2.1 Runtime Metadata Package (`internal/taskstate/`)

```go
type TaskState struct {
    Version         int    `json:"version"`
    TaskFile        string `json:"task_file"`
    TaskBranch      string `json:"task_branch"`
    TargetBranch    string `json:"target_branch"`
    LastHeadSHA     string `json:"last_head_sha"`
    LastReviewedSHA string `json:"last_reviewed_sha"`
    LastOutcome     string `json:"last_outcome"`
    UpdatedAt       string `json:"updated_at"`
}
```

**API**:

- `Load(tasksDir, taskFilename string) (*TaskState, error)`:
  - File not found → `(nil, nil)`
  - File exists, valid JSON → `(*TaskState, nil)`
  - File exists, corrupt JSON → `(nil, error)` — caller decides
- `Update(tasksDir, taskFilename string, fn func(*TaskState)) error`:
  1. `os.MkdirAll(filepath.Dir(stateFilePath(...)), 0o755)` — lazy dir creation
  2. Load existing (if valid); create empty `TaskState{}` if missing or corrupt
  3. Apply caller's mutation function
  4. Set Version=1, UpdatedAt=UTC RFC3339
  5. `atomicwrite.WriteFile`
- `Delete(tasksDir, taskFilename string) error` — returns error, caller handles

**State file path**: `<tasksDir>/runtime/task-state-<filename>.json`

**Load-modify-save semantics**: Work and review paths write to the same state
file but update different fields. `Update` always loads first, preventing one
writer from clobbering the other's fields.

**Lifecycle example**:

1. Push → `Update(fn: set LastHeadSHA, TaskBranch, TargetBranch, LastOutcome="pushed")`
2. Review rejects → `Update(fn: set LastReviewedSHA, LastOutcome="review-rejected")` — preserves LastHeadSHA
3. Rework + push → `Update(fn: set LastHeadSHA, LastOutcome="pushed")` — preserves LastReviewedSHA
4. Follow-up review sees LastReviewedSHA from step 2

#### 3.2.2 Review Context Injection

Add `REVIEW_CONTEXT_PLACEHOLDER` to `internal/runner/review-instructions.md`
between `## Paths` and `## Folder Structure`.

Add follow-up review guidance at end of `## STATE: REVIEW`:

- This may be a continuation of an earlier review cycle
- The current branch tip may differ from the one reviewed previously
- Re-evaluate the current diff independently
- Verify whether earlier rejection findings were addressed
- Do not assume earlier rejection reasons still apply unchanged

**`buildReviewContext(repoRoot string, task *queue.ClaimedTask, tasksDir string) string`**:

- **Current tip**: `git.Output(repoRoot, "rev-parse", "--short", task.Branch)`
  — resolves against the **host repository**, where the task branch exists as a
  local branch (pushed by `postAgentPush`, verified by `VerifyReviewBranch`).
  This runs before the review clone is created, matching the existing prompt
  construction pattern in `runReview`.
- **Prior SHA**: from `taskstate.Load` → `LastReviewedSHA` — omitted on error
- **Rejection**: from `taskfile.LastReviewRejectionReason` — independent of
  runtime state

**Fallback matrix**:

| Runtime state | Rejection | Mode | Shows prior SHA? |
|--------------|-----------|------|-----------------|
| Valid | Yes | follow-up | Yes |
| Valid | No | initial | Yes |
| Corrupt/missing | Yes | follow-up | No |
| Corrupt/missing | No | initial | No |

In `runReview`:

```go
reviewCtx := buildReviewContext(env.repoRoot, task, env.tasksDir)
run.prompt = strings.ReplaceAll(run.prompt, "REVIEW_CONTEXT_PLACEHOLDER", reviewCtx)
```

#### 3.2.3 Recording State

**Review**: Capture `reviewedSHA` in `pollReview` before `runReview`. Thread to
`postReviewAction` (add parameter). Update with load-modify-save. Best-effort.

**Work**: In `postAgentPush`, after push. Update with load-modify-save.
Best-effort.

#### 3.2.4 Centralized Cleanup

**Point cleanup** (4 sites, AFTER confirmed terminal transition):

- `merge.go` `executeMergeRound`: → completed/
- `claim.go` `handleRetryExhaustedTask`: → failed/
- `cancel.go` `CancelTask`: → failed/
- `review.go` `reviewCandidates`: unparseable/review-exhausted → failed/

**Sweep-only paths** (covered by periodic stale sweep, not point cleanup):

- Reconcile-to-failed (unparseable, invalid glob, cycles, duplicates)
- Merge failure exhaustion
- Any other future terminal transition

**Periodic stale sweep** in `pollCleanup`:

- Scans `<tasksDir>/runtime/` using `os.ReadDir`
- Extracts task filename from state file naming convention
- Checks ALL non-terminal dirs: waiting/, backlog/, in-progress/,
  ready-for-review/, ready-to-merge/
- Deletes only when confirmed absent from ALL
- On ambiguous stat error → preserves (conservative)

The periodic sweep is the backstop for any terminal transition not covered by
point cleanup. It catches all paths without requiring changes to every transition
site.

#### 3.2.5 Testing Strategy for Phase 2

**`internal/taskstate/taskstate_test.go`** (10 tests):

- `TestLoadUpdate_RoundTrip`
- `TestUpdate_CreatesNew`
- `TestUpdate_PreservesExistingFields`
- `TestUpdate_RejectedThenPushed_PreservesReviewedSHA`
- `TestUpdate_CorruptedExistingFile`
- `TestUpdate_CreatesRuntimeDir`
- `TestLoad_MissingFile`
- `TestLoad_CorruptedFile` — returns (nil, error)
- `TestDelete_ExistingFile`
- `TestDelete_MissingFile`

**`internal/runner/review_test.go`** (7 tests):

- `TestBuildReviewContext_InitialReview`
- `TestBuildReviewContext_FollowUpReview`
- `TestBuildReviewContext_WithPriorRejection_NoState`
- `TestBuildReviewContext_CorruptedState_WithRejection`
- `TestBuildReviewContext_CorruptedState_NoRejection`
- `TestBuildReviewContext_MissingSHA`
- `TestReviewPrompt_PlaceholderReplaced`

**`internal/runner/runner_test.go`** (4 tests):

- `TestCleanStaleRuntimeState_RemovesOrphanedFiles`
- `TestCleanStaleRuntimeState_PreservesActiveInBacklog`
- `TestCleanStaleRuntimeState_PreservesActiveInWaiting`
- `TestCleanStaleRuntimeState_PreservesOnAmbiguousError`

**Integration** (3 tests):

- `TestFollowUpReviewReceivesContext`
- `TestRuntimeMetadataCleanup_Merge`
- `TestCorruptedRuntimeMetadata_Fallback`

## 4. Step-by-Step Implementation Order

### Phase 1 — 9 steps

| Step | Description | Files | Deps |
|------|-------------|-------|------|
| 1 | `ParseBranchMarkerLine` + `ReplaceBranchMarkerLine` | taskfile/taskfile.go, _test.go | — |
| 2 | Migrate `ParseBranch`, `readBranchFromFile`, `BuildIndex` | taskfile/metadata.go, queue/claim.go, queue/index.go | 1 |
| 3 | `WriteBranchMarker` (exported) | queue/claim.go | 1 |
| 4 | Branch identity + collision + mandatory write | queue/claim.go, _test.go | 1-3 |
| 5 | `checkoutOrCreateBranch` with explicit `origin/<branch>` | runner/task.go | — |
| 6 | Starting-tip capture (hard error) + replace checkout | runner/task.go | 5 |
| 7 | Adjusted commit detection (hard error) | runner/task.go, _test.go | 6 |
| 8 | `writeBranchMarkerFn` + normalization in review path | runner/task.go, _test.go | 1, 3 |
| 9 | Integration test | integration/resume_test.go | 1-8 |

### Phase 2 — 9 steps

| Step | Description | Files | Deps |
|------|-------------|-------|------|
| 10 | `internal/taskstate/` with `Update` + lazy dir | taskstate/*.go | — |
| 11 | `REVIEW_CONTEXT_PLACEHOLDER` + guidance | runner/review-instructions.md | — |
| 12 | `buildReviewContext` + prompt replacement | runner/review.go, _test.go | 10-11 |
| 13 | Record work state via `Update` | runner/task.go | 10 |
| 14 | Capture reviewed SHA + record review state | runner/review.go, runner.go | 10 |
| 15 | Point cleanup at 4 terminal transitions | merge.go, claim.go, cancel.go, review.go | 10 |
| 16 | Periodic stale cleanup | runner/runner.go, _test.go | 10 |
| 17 | Integration tests | integration/resume_test.go | 10-16 |
| 18 | Update docs | docs/architecture.md | 10-16 |

## 5. File Changes Summary

### Phase 1 (9 files)

| File | Action | Description |
|------|--------|-------------|
| `internal/taskfile/taskfile.go` | Modify | `ParseBranchMarkerLine`, `ReplaceBranchMarkerLine` |
| `internal/taskfile/taskfile_test.go` | Modify | Fence-aware + replacement tests |
| `internal/taskfile/metadata.go` | Modify | `ParseBranch` → fence-aware |
| `internal/queue/claim.go` | Modify | `WriteBranchMarker`; `readBranchFromFile` migrated; branch reuse + collision + mandatory |
| `internal/queue/claim_test.go` | Modify | 6 new tests |
| `internal/queue/index.go` | Modify | Branch parsing migrated |
| `internal/runner/task.go` | Modify | `checkoutOrCreateBranch`; starting-tip (hard error); commit detection (hard error); `writeBranchMarkerFn` |
| `internal/runner/task_test.go` | Modify | 12 new tests |
| `internal/integration/resume_test.go` | Create | Full lifecycle |

### Phase 2 (14 files)

| File | Action | Description |
|------|--------|-------------|
| `internal/taskstate/taskstate.go` | Create | TaskState, Load/Update/Delete, lazy dir |
| `internal/taskstate/taskstate_test.go` | Create | 10 tests |
| `internal/runner/review-instructions.md` | Modify | Placeholder + guidance |
| `internal/runner/review.go` | Modify | `buildReviewContext`; record state; cleanup |
| `internal/runner/runner.go` | Modify | Thread SHA; stale cleanup |
| `internal/runner/task.go` | Modify | Record work state |
| `internal/merge/merge.go` | Modify | Delete state |
| `internal/queue/claim.go` | Modify | Delete state |
| `internal/queue/cancel.go` | Modify | Delete state |
| `internal/runner/review_test.go` | Modify | 7 tests |
| `internal/runner/task_test.go` | Modify | Work state tests |
| `internal/runner/runner_test.go` | Modify | 4 tests |
| `internal/integration/resume_test.go` | Modify | 3 integration tests |
| `docs/architecture.md` | Modify | Branch resume + runtime metadata |

## 6. Error Handling

| Scenario | Behavior |
|----------|----------|
| No branch marker | Derive + insert (mandatory); rollback on failure |
| Marker matches | Skip write |
| Marker differs | Replace (mandatory); rollback on failure |
| `ls-remote` 0/2/other | exists/absent/hard error |
| Post-fetch ref missing | Hard error with diagnostics |
| `startingTip` rev-parse fails | **Hard error** — runOnce returns; recoverStuckTask handles |
| `currentTip` rev-parse fails | **Hard error** — postAgentPush returns; clone preserved |
| HEAD == startingTip | No push |
| HEAD ≠ startingTip | Push + move |
| Review context rev-parse fails | **Best-effort** — shows "unknown"; review proceeds |
| `Load` on missing file | (nil, nil) |
| `Load` on corrupt JSON | (nil, error) |
| `Update` on corrupt file | Creates fresh, applies mutation |
| `Update` I/O failure | Warning, continue |
| `Delete` failure | Return error; caller warns; sweep catches |
| Stale sweep stat error | Preserve |

## 7. Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Body-embedded marker | Fence-aware parser across all readers |
| Collision with active branch | Check + fallback + mandatory write |
| Stale/conflicting markers | Replace-in-place normalization |
| Mandatory write fails | Claim rollback |
| Ambiguous fetch failure | Hard error from ls-remote |
| No-op retry misdetected | Starting-tip SHA comparison |
| rev-parse failure | Hard error for control flow; best-effort for review context |
| Work overwrites review state | Load-modify-save (`Update`) |
| Stale runtime files | Point cleanup + periodic sweep |
| Waiting/ state loss | Sweep checks all non-terminal dirs |
| Corrupt runtime state | Task-file data independent; Update recreates fresh |
| Runtime dir missing | Lazy `os.MkdirAll` in `Update` |
| Rollback test coverage | `writeBranchMarkerFn` variable |

## 8. Open Questions

1. **Function variable for `checkoutOrCreateBranch`?** Yes — follows codebase
   convention.
2. **Sweep frequency?** Every poll cycle; fast operation.
3. **Deprecate `ParseBranchComment`?** Not now; retained as low-level helper.
4. **State history depth?** Last-only; extend if needed.
