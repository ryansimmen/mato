# Implementation Plan: Resume Work and Review Sessions

## 1. Goal

Reduce retry churn after review rejection and transient agent failures by fixing
the two cold-start behaviors that waste the most work today:

1. **Worktree continuity**: when a task already has a task branch, the next work
   attempt should resume from that branch tip instead of recreating the branch
   from the target branch tip and force-pushing over prior work.
2. **Session continuity**: work retries and follow-up reviews should each resume
   their own prior Copilot session instead of starting a brand-new session every
   time.

This proposal intentionally treats work and review as separate long-lived
sessions. A task may bounce between `backlog/`, `in-progress/`, and
`ready-for-review/`, but the implementation session and the review session
should not be conflated.

## 2. Scope

### In scope

- Phase 1: fence-aware branch marker parsing across all readers; branch identity
  reuse at claim time; resume from existing task branch tip; single-marker
  normalization; adjusted commit detection with explicit SHA error handling
- Phase 2: explicit review continuity via host-curated prompt context plus small
  runtime metadata with load-modify-save semantics and centralized cleanup
- Phase 3: durable session resume for both work and review via separate session
  records and `copilot --resume=<session-id>`

### Out of scope

- Recovering unpublished work from deleted temp clones
- Sharing one Copilot session across work and review phases
- Persisting full terminal logs or full prompt payloads for every run
- Changing queue states, retry budgets, or review semantics
- Cross-task session reuse

## 3. Design

### 3.1 Phase 1: Git Branch Resume for Work Runs

#### 3.1.1 Fence-Aware Branch Marker Parsing (`internal/taskfile/metadata.go`)

Add `ParseBranchMarkerLine(data []byte) (string, bool)` in
`internal/taskfile/metadata.go`, alongside the existing `forEachMarkerLine`-
based parsers such as `ParseClaimedBy`, `ParseClaimedAt`,
`CountFailureMarkers`, and `ExtractReviewRejections`. Only honor
`<!-- branch: ... -->` on standalone lines outside code fences.

`ParseBranchComment` is retained as a low-level regex helper but is no longer
used by branch-identity readers.

`internal/taskfile/taskfile.go` should remain the thin file-reading wrapper.
`ParseBranch(path string)` should continue to read the file and delegate to the
data-level parser in `metadata.go`.

#### 3.1.2 Branch Marker Replacement (`internal/taskfile/metadata.go`)

Add `ReplaceBranchMarkerLine(data []byte, newBranch string) (result []byte, found bool, replaced bool)`:

- No standalone marker: data unchanged, `found=false`
- Marker matches: data unchanged, `found=true`, `replaced=false`
- Marker differs: first standalone marker replaced in-place, `found=true`,
  `replaced=true`

Implementation detail: `ReplaceBranchMarkerLine` should use `forEachTaskLine`,
not `forEachMarkerLine`. Replacement needs the original line text plus fence
state so the file can be reconstructed with exactly one branch-marker line
updated in place. `forEachMarkerLine` only exposes the trimmed marker text and
is sufficient for parsing, but not for safe in-place replacement.

#### 3.1.3 Migrate All Branch-Identity Readers

| Consumer | Location | Change |
|----------|----------|--------|
| `ParseBranch` | `internal/taskfile/taskfile.go` | keep file read here; delegate `ParseBranchComment` -> `ParseBranchMarkerLine` |
| `readBranchFromFile` | `internal/queue/claim.go` | `ParseBranchComment` -> `ParseBranchMarkerLine` |
| `BuildIndex` | `internal/queue/index.go` | `ParseBranchComment` -> `ParseBranchMarkerLine` |

Downstream consumers automatically become fence-aware: merge, review, and
`CollectActiveBranches`.

Migration safety: `ParseBranchMarkerLine` is strictly more conservative than
`ParseBranchComment`. It rejects matches inside code fences and only processes
standalone marker lines. This should eliminate false positives, not valid cases.

#### 3.1.4 Exported Branch Marker Writer (`internal/queue/claim.go`)

Add `WriteBranchMarker(taskPath, branch string) error`:

1. Read file and parse with `ParseBranchMarkerLine`
2. If marker already matches, return nil
3. If marker differs, call `ReplaceBranchMarkerLine` and atomically write back
4. If no marker exists, insert one after the first claimed-by marker as today

#### 3.1.5 Branch Identity Preservation with Collision Safety (`internal/queue/claim.go`)

In `SelectAndClaimTask`, after the claimed-by marker is written:

1. Read the claimed file and check for an existing standalone branch marker
2. If found and not colliding with another active task, reuse it
3. If found but colliding, derive a new branch name
4. If not found, derive a new branch name
5. When deriving a branch, `WriteBranchMarker` is mandatory and failure rolls
   the claim back to `backlog/`
6. When reusing a matching marker, skip the write

This keeps branch identity stable across review rejection, host recovery, and
other non-terminal retries.

#### 3.1.6 Branch Content Resume (`internal/runner/task.go`)

Prefer reusing the existing `internal/git.EnsureBranch` helper instead of
adding a second branch-selection implementation in `runner/task.go`.

The runner should call `git.EnsureBranch(cloneDir, branch)` after cloning and
before launching the agent. The current `EnsureBranch` API already returns the
source needed for the runner to enforce stricter recorded-branch resume policy,
so Phase 1 should default to a runner-side policy check rather than assuming any
changes to `internal/git/git.go` are required.

If implementation reveals an edge case that the current `EnsureBranch` result
contract cannot express cleanly, then a small targeted `internal/git` change is
still reasonable. But it should not be assumed up front.

Runner policy should distinguish fresh branches from recorded-branch resumes:

- **Fresh branch path**: when the task has no recorded `<!-- branch: ... -->`
  marker, `EnsureBranch` may use its existing generic fallback behavior.
- **Recorded branch path**: when the task already has a recorded branch marker,
  the runner must fail closed rather than silently restarting from `HEAD`.

For recorded branches, the runner should accept only these `EnsureBranch`
results:

- `BranchSourceLocal`
- `BranchSourceRemote`

`BranchSourceRemoteCached` is out of scope for v1 recorded-branch resume. The
first implementation should require a live remote check for recorded branches
and fail closed otherwise. An explicit degraded/offline resume mode can be
considered later if there is a real operator need.

For recorded branches, the runner should reject:

- `BranchSourceHeadRemoteMissing`
- `BranchSourceHeadRemoteUnavailable`

Exception: if mato intentionally deleted the task branch as part of
merge-conflict cleanup, it must also remove the `<!-- branch: ... -->` marker
from the task file during the same host-side transition. That makes the next
claim unambiguously take the fresh-branch path without depending on later
runtime metadata.

Expected behavior therefore becomes:

1. If `origin/<branch>` exists, fetch it and create the local branch from that
   remote tip
2. If the task has no recorded branch and the branch is confirmed absent, create
   a fresh branch from `HEAD`
3. If the task has a recorded branch and the branch is absent or the remote is
   unavailable, return a hard error instead of silently restarting from `HEAD`
4. Ambiguous probe/fetch failures remain hard errors

#### 3.1.7 Starting-Tip Capture (`internal/runner/task.go`)

After branch selection completes, before agent launch, capture the starting tip:

```go
startingTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
```

If this fails, return a hard error before launching the agent. No work has been
done yet, so the task can safely roll back to `backlog/`.

Thread `startingTip` into `postAgentPush`.

#### 3.1.8 Adjusted Commit Detection (`internal/runner/task.go`)

In `postAgentPush`, replace target-branch-relative commit detection with a direct
tip comparison:

```go
currentTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
if strings.TrimSpace(currentTip) == startingTip {
    return nil
}
```

This correctly detects no-op retries on resumed branches.

Keep `targetBranch..HEAD` only for changed-files reporting and related
diagnostics.

#### 3.1.9 Marker Normalization in `moveTaskToReviewWithMarker` (`internal/runner/task.go`)

Replace append-only branch-marker writes with `queue.WriteBranchMarker` so new
retries stop accumulating duplicate `<!-- branch: ... -->` markers.

Legacy files with duplicate markers are tolerated. The new parser reads the
first standalone marker and the replacer updates only that first marker.

#### 3.1.10 Intentional Branch Deletion (`internal/merge/taskops.go`)

When merge-conflict handling intentionally deletes a task branch, Phase 1 must
also clear the task file's branch marker during that same transition.

That keeps the branch-marker contract simple:

- marker present = resume expected
- marker absent = fresh branch allowed

This avoids a Phase 1 mismatch where a deleted branch plus a stale marker would
otherwise trigger the recorded-branch hard-error path.

#### 3.1.11 Testing Strategy for Phase 1

Add unit coverage for:

- fence-aware branch parsing and replacement
- claim-time branch reuse, collision handling, and rollback on write failure
- branch checkout resume from `origin/<branch>`
- fresh-branch fallback when branch is absent
- intentional merge-conflict branch deletion clearing the branch marker
- no-op retry detection via starting-tip comparison
- review-rejection retry preserving branch contents across the next work run

### 3.2 Phase 2: Explicit Review Continuity and Lightweight Runtime Metadata

Phase 2 keeps review continuity explicit and auditable even before durable
review-session resume is added.

#### 3.2.1 Runtime Metadata Package (`internal/taskstate/`)

Add a small host-owned package:

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

Suggested file path:

```text
<tasksDir>/runtime/taskstate/<filename>.json
```

API shape:

- `Load(tasksDir, taskFilename string) (*TaskState, error)`
- `Update(tasksDir, taskFilename string, fn func(*TaskState)) error`
- `Delete(tasksDir, taskFilename string) error`

`Update` should use load-modify-save semantics so work and review paths can
update different fields without clobbering each other.

#### 3.2.2 Review Context Injection (`internal/runner/review.go`)

Add `REVIEW_CONTEXT_PLACEHOLDER` to `internal/runner/review-instructions.md` and
render a host-written block into it before launching the review agent.

The block should include:

- task branch name
- current branch tip SHA
- last reviewed branch tip SHA, if known
- previous review rejection reason, if known
- an explicit reminder to reassess the current diff independently

Example:

```text
Review context:
- task branch: task/add-foo
- current branch tip: abc123
- last reviewed branch tip: def456
- previous rejection: missing unit tests for retry backoff
- review mode: follow-up review; reassess the current diff independently
```

If no prior review context exists, render an explicit neutral block instead of
omitting the section.

#### 3.2.3 Recording State

- On work push: update `LastHeadSHA`, branch, target branch, and outcome
- On review launch/completion: capture and store `LastReviewedSHA` and outcome
- On merge-conflict cleanup that intentionally deletes the task branch: record a
  distinct outcome such as `merge-conflict-cleanup` as supplementary context for
  later runs and diagnostics. Phase 1 correctness should not depend on this
  metadata because the branch marker is already cleared during the same host-side
  transition

These writes are best-effort. Failure should warn and continue.

#### 3.2.4 Cleanup

Delete stale runtime metadata under `.mato/runtime/` when tasks reach terminal
states such as `completed/`, `failed/`, or cancelled terminal paths. Add a
periodic stale sweep as a backstop for terminal transitions that are not
individually instrumented.

This should be a hybrid strategy:

- point cleanup on centralized terminal transitions where the task outcome is
  already known
- periodic stale sweep in `pollCleanup` for fragmented failure paths, crashes,
  and interrupted runs

The sweep is required because terminal `failed/` transitions are currently
spread across queue, review, merge, and cancel paths.

#### 3.2.5 Testing Strategy for Phase 2

Add tests for:

- load/update/delete semantics in `internal/taskstate/`
- review prompt placeholder replacement
- initial vs follow-up review context construction
- missing/corrupt runtime metadata fallback
- stale runtime metadata cleanup

### 3.3 Phase 3: Durable Work and Review Session Resume

Phase 1 fixes the highest-value correctness bug. Phase 2 makes review continuity
explicit and inspectable. Phase 3 adds durable Copilot session resume for both
work and review because branch continuity alone does not eliminate the full
restart cost.

#### 3.3.1 Separate Session Model

Each task gets up to two durable session records:

- `work` session: the implementing agent's long-lived Copilot session
- `review` session: the reviewing agent's long-lived Copilot session

These sessions must remain separate because they have different prompts, goals,
models, and success criteria.

`taskstate` and `sessionmeta` should remain separate packages. `taskstate`
tracks host-owned branch/review continuity state, while `sessionmeta` tracks
Copilot resume state. They have different responsibilities and failure
semantics.

#### 3.3.2 Session Storage Location

Store session metadata under `.mato/runtime/sessionmeta/`:

```text
.mato/runtime/sessionmeta/
  work-<task-filename>.json
  review-<task-filename>.json
```

This keeps session state out of `messages/`, which is already the home for
inter-agent coordination artifacts such as `events/`, `presence/`,
`completions/`, verdict files, file-claims indexes, and dependency-context
files. Sessions are runtime state, not messages, so they fit better under a
dedicated runtime subtree.

Recommended `.mato/` layout after this change:

```text
.mato/
  messages/
    events/
    presence/
    completions/
    verdict-*.json
    file-claims.json
    dependency-context-*.json
  runtime/
    taskstate/
      <task-filename>.json
    sessionmeta/
      work-<task-filename>.json
      review-<task-filename>.json
```

#### 3.3.3 Session Record Shape

Start with a minimal session record. Do not overbuild compatibility fields until
the implementation actually needs them.

Suggested minimum schema:

```json
{
  "version": 1,
  "kind": "work",
  "task_file": "add-foo.md",
  "task_branch": "task/add-foo",
  "copilot_session_id": "0cb916db-26aa-40f2-86b5-1ba81b225fd2",
  "updated_at": "2026-03-29T12:23:55Z",
  "last_head_sha": "abc123..."
}
```

Notes:

- `copilot_session_id` is the durable identifier passed to
  `copilot --resume=<id>`
- `last_head_sha` records the branch tip most recently associated with the
  session
- Additional fields such as `target_branch`, `attempt`, `model`,
  `reasoning_effort`, `prompt_hash`, `status`, or `last_outcome` can be added
  later if resume safety or debugging proves they are necessary
- V1 should not invalidate sessions on prompt or model changes. Compatibility
  fields should wait until a concrete problem appears
- `mato` should treat the stored session record as the source of truth for the
  session ID it intends to reuse, rather than depending on scraping IDs from
  Copilot output

#### 3.3.4 Session Lifecycle

Work session lifecycle:

- create on the first successful claim before launching `runOnce()`
- keep active across `backlog/`, `in-progress/`, `ready-for-review/`, and review
  rejection back to `backlog/`
- close when the task reaches `completed/`, `failed/`, or a cancelled terminal
  path

Review session lifecycle:

- create on the first review launch after branch verification succeeds
- keep active across repeated reviews of the same task after fixes and transient
  review failures
- close when the task reaches `completed/`, `failed/`, or a cancelled terminal
  path

Review-session resume should be independently disableable from work-session
resume. Recommended initial surface:

- `.mato.yaml`: `review_session_resume_enabled: true`
- env override: `MATO_REVIEW_SESSION_RESUME_ENABLED=false`

Do not add a CLI flag initially unless a concrete operator workflow demands it.

#### 3.3.5 Copilot CLI Plumbing

Extend runner launch plumbing so `buildDockerArgs()` can append
`--resume=<session-id>` when a session record exists.

If the Copilot CLI reliably accepts caller-supplied session IDs, prefer letting
`mato` generate and persist the ID up front. That is simpler and more durable
than trying to discover the session ID from Copilot output after the fact.

The runner should:

- load or create the `work` session before launching a task run
- load or create the `review` session before launching a review run
- pass the correct session ID to Copilot for that phase only
- update the session record after each run with at least the current branch tip
  and any additional compatibility fields the implementation actually uses

#### 3.3.6 Review Re-anchoring Guardrail

Even with durable review-session resume, follow-up reviews must not blindly
reuse earlier conclusions.

The prompt should still state:

- this may be a continuation of an earlier review cycle
- the current branch tip may differ from the one reviewed previously
- re-evaluate the current diff independently
- verify whether earlier rejection findings were addressed
- do not assume earlier rejection reasons still apply unchanged

Session resume improves continuity; explicit prompt context prevents stale
anchoring from becoming the only source of truth.

Review-session resume should also remain independently disableable from
work-session resume. If follow-up reviews prove too sticky in practice, mato
should be able to keep work-session resume plus explicit review context while
temporarily disabling durable review-session resume without redesigning the rest
of the system.

#### 3.3.7 Fresh-Start Conditions

Fall back to a fresh session when:

- the stored session metadata is missing or corrupt
- the CLI rejects the resume request
- the prompt contract changes incompatibly
- the configured model changes incompatibly

Fall back to a fresh branch start when:

- the recorded task branch no longer exists on `origin`
- the task branch was intentionally deleted after merge-conflict handling
- branch metadata is absent or unusable

The default policy should remain conservative: resume when metadata is coherent
and the CLI accepts it, otherwise fall back cleanly.

For v1, intentional branch deletion after merge-conflict handling should not
trigger session cleanup by itself. The task is still active and may be retried.
Instead, retain `.mato/runtime/taskstate/` and `.mato/runtime/sessionmeta/`,
record the merge-conflict-cleanup outcome, and allow the next work run to start
from a fresh branch while reusing the existing work session.

#### 3.3.8 Testing Strategy for Phase 3

Add tests for:

- separate work and review session records for the same task
- `buildDockerArgs()` appending `--resume=<id>` only when set
- repeated work retries reusing the same work session ID
- repeated follow-up reviews reusing the same review session ID
- corrupt session metadata falling back safely
- session cleanup on terminal task states

## 4. Step-by-Step Implementation Order

### Phase 1

1. Add `ParseBranchMarkerLine` and `ReplaceBranchMarkerLine` in `metadata.go`
2. Migrate all branch readers to the fence-aware parser
3. Add `WriteBranchMarker`
4. Reuse branch identity during claim with collision handling
5. Reuse `internal/git.EnsureBranch` and add runner-side recorded-branch policy
6. Clear the branch marker when merge-conflict cleanup intentionally deletes the
   task branch
7. Capture `startingTip` before agent launch
8. Replace no-op detection with starting-tip SHA comparison
9. Replace append-only branch writes with marker normalization
10. Add unit and integration tests for branch resume

### Phase 2

11. Add `internal/taskstate/` with `Load`, `Update`, and `Delete`
12. Add `REVIEW_CONTEXT_PLACEHOLDER` and follow-up review guidance
13. Implement `buildReviewContext` and prompt interpolation
14. Record work and review SHAs/outcomes via `taskstate.Update`
15. Add terminal cleanup plus periodic stale cleanup
16. Add unit and integration tests for explicit review continuity

### Phase 3

17. Add a small session metadata helper package, for example
    `internal/sessionmeta/`
18. Add minimal work and review session record creation/load/update/close
    helpers under `.mato/runtime/sessionmeta/` with `mato`-generated session IDs
19. Extend config/env/run-option plumbing for
    `review_session_resume_enabled`
20. Extend runner launch plumbing to support `--resume=<session-id>`
21. Wire work runs to resume the durable work session
22. Wire review runs to resume the durable review session, gated by
    `review_session_resume_enabled`
23. Add point cleanup for terminal paths plus periodic stale sweep for
    `.mato/runtime/taskstate/` and `.mato/runtime/sessionmeta/`
24. Add unit and integration tests for session resume
25. Update architecture docs

## 5. File Changes Summary

### Phase 1

- `internal/taskfile/taskfile_test.go`
- `internal/taskfile/metadata.go`
- `internal/queue/claim.go`
- `internal/queue/claim_test.go`
- `internal/queue/index.go`
- `internal/merge/taskops.go`
- `internal/runner/task.go`
- `internal/runner/task_test.go`
- `internal/integration/resume_test.go`

### Phase 2

- `internal/taskstate/taskstate.go`
- `internal/taskstate/taskstate_test.go`
- `internal/runner/review-instructions.md`
- `internal/runner/review.go`
- `internal/runner/runner.go`
- `internal/runner/task.go`
- terminal cleanup call sites in merge/claim/cancel/review paths
- `internal/runner/review_test.go`
- `internal/runner/runner_test.go`
- `internal/integration/resume_test.go`
- `docs/architecture.md`

### Phase 3

- `internal/sessionmeta/sessionmeta.go`
- `internal/sessionmeta/sessionmeta_test.go`
- `internal/config/config.go`
- `cmd/mato/main.go`
- `cmd/mato/main_test.go`
- `internal/runner/config.go`
- `internal/runner/config_test.go`
- `internal/runner/task.go`
- `internal/runner/review.go`
- `internal/runner/runner.go`
- `internal/runner/task_test.go`
- `internal/runner/review_test.go`
- `internal/integration/resume_test.go`
- `docs/architecture.md`

## 6. Error Handling

| Scenario | Behavior |
|----------|----------|
| No branch marker | Derive and insert; rollback claim on mandatory write failure |
| Marker matches | Reuse and skip write |
| Marker differs | Replace in place; rollback claim on mandatory write failure |
| Recorded branch + `EnsureBranch` returns local/remote | Resume from recorded branch tip |
| Recorded branch + `EnsureBranch` returns head fallback | Hard error; do not silently restart from `HEAD` |
| Intentional branch deletion during merge-conflict cleanup | Delete branch and clear branch marker so the next claim takes the fresh-branch path |
| Fresh branch + `EnsureBranch` returns head fallback | Allowed; create from `HEAD` |
| Recorded branch + remote unavailable but cached remote ref exists | Not enabled by default; possible future degraded/offline resume mode |
| Post-fetch ref missing | Hard error with diagnostics |
| `startingTip` rev-parse fails | Hard error; do not launch agent |
| `currentTip` rev-parse fails | Hard error; preserve clone, retry later |
| HEAD == `startingTip` | No push |
| HEAD != `startingTip` | Push and advance queue state |
| Review context SHA lookup fails | Best-effort; show unknown and continue |
| `taskstate.Load` on missing file | `(nil, nil)` |
| `taskstate.Load` on corrupt JSON | `(nil, error)` |
| `taskstate.Update` on corrupt file | Recreate fresh state and continue |
| Session metadata missing/corrupt | Fresh session fallback |
| Copilot resume rejected | Fresh session fallback |
| Cleanup delete failure | Warn and continue; stale sweep backstops |

## 7. Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Body-embedded branch marker false positives | Fence-aware parser across all readers |
| Collision with another active branch | Reuse only when not colliding; otherwise derive + rewrite |
| Duplicate legacy markers | Read first standalone marker; replace first only |
| Ambiguous remote branch probe | Hard error instead of silent data loss |
| No-op retry misdetected on resumed branch | Starting-tip SHA comparison |
| Review agent anchors too strongly on old findings | Keep explicit follow-up-review guidance in the prompt |
| Work and review session state interfere | Separate session records and session IDs |
| Review-session resume proves too sticky in practice | Gate it independently with config/env kill switch |
| Corrupt runtime/session metadata | Fresh fallback and best-effort recreation |
| Temp-clone-only work remains unrecoverable | Explicitly accepted limitation |

## 8. Version 1 Decisions

1. Recorded-branch resume requires a live remote check in v1.
   `BranchSourceRemoteCached` is not allowed for recorded-branch resume by
   default.
2. V1 session metadata stays minimal.
   Prompt/model compatibility fields and invalidation rules are deferred until a
   concrete resume-safety problem appears.
3. Intentional task-branch deletion after merge-conflict handling must also
   clear the branch marker in v1.
   That makes Phase 1 self-contained and preserves the contract that marker
   presence implies resume intent.
4. Intentional task-branch deletion after merge-conflict handling does not
   trigger session cleanup in v1.
   Keep runtime state, record a distinct merge-conflict-cleanup outcome, and let
   the next work run start from a fresh branch while reusing the work session.
