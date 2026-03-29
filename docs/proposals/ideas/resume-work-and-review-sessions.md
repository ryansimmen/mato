# Resume Work and Review Retries

## 1. Goal

Reduce retry churn after review rejection and transient agent failures by fixing
the two cold-start behaviors that waste the most work today:

1. **Worktree continuity**: when a task already has a task branch, the next work
   attempt should resume from that branch tip instead of recreating the branch
   from the target branch tip and force-pushing over prior work.
2. **Review continuity**: follow-up reviews should receive explicit host-curated
   context about the previous review and the current branch tip, rather than
   relying on an opaque long-lived reviewer conversation.

This proposal intentionally treats Git branch resume as the primary fix.
Conversation resume is optional and should be added only where it clearly helps
and can be implemented reliably.

## 2. Scope

### In scope

- Reuse an existing `<!-- branch: ... -->` marker when reclaiming a task from
  `backlog/`.
- Resume work from the existing task branch tip when that branch still exists in
  the host repository.
- Preserve review continuity by injecting explicit previous-review context into
  follow-up review runs.
- Record enough lightweight runtime metadata to support safe branch resume and
  better review prompts.
- Optionally add durable work-session resume later, behind a small isolated
  abstraction.
- Add tests covering review rejection retries, missing-branch fallback, and
  follow-up review context.

### Out of scope

- Recovering unpublished work from a temp clone after an agent crashes before
  pushing its branch.
- Making long-lived review conversations part of the core architecture.
- Persisting full terminal logs or full prompt payloads for every run.
- Sharing one Copilot session across work and review phases.
- Changing queue states, retry budgets, or review semantics.
- Cross-task session reuse.

## 3. Design

### 3.1 Problem Summary

Today, rejected tasks preserve review feedback and usually preserve the branch
name marker in the task file, but the next work attempt still starts from a new
branch created from the target branch tip. The host then force-pushes that new
branch over the old branch name. In practice this loses prior branch contents
even though the task metadata suggests continuity.

That branch reset is the highest-value bug to fix.

Review runs also restart from scratch, but mato already has a strong host-curated
context model: the host injects prior failure records, dependency context, and
review feedback into new runs. Follow-up review should extend that pattern
instead of introducing hidden reviewer memory as a core dependency.

### 3.2 Architecture Decision

Adopt a staged architecture:

1. **Phase 1: Git resume for work runs**
   - Always prefer resuming the existing task branch when safe.
   - Ship this without introducing any new runtime metadata file.
2. **Phase 2: explicit review continuity**
   - Pass previous review metadata and branch-tip information into the review
     prompt.
   - Introduce a small runtime metadata file only for the review-context fields
     that cannot be derived from the task file alone.
3. **Phase 3: optional work-session resume**
   - Add durable Copilot work-session resume only if the CLI semantics are
     reliable and the host can obtain and persist stable session identifiers.

Do **not** make persistent review-session resume part of the initial design.

### 3.3 Work Resume Rules

#### Branch identity

When a task is reclaimed from `backlog/`, the host should first look for an
existing `<!-- branch: ... -->` marker in the claimed file and reuse that branch
name if present. Only tasks without a branch marker should derive a fresh branch
name from the filename and active-branch disambiguation rules.

This keeps the branch name stable across review rejection, host recovery, and
other non-terminal retries.

#### Branch contents

Before launching the task agent:

1. If the task branch exists on `origin`, fetch it and check out the clone as
   that branch tip.
2. Otherwise, create a fresh branch from the target branch as today.

Concretely, the work-run setup should prefer something equivalent to:

```text
git fetch origin <task-branch>
git checkout -B <task-branch> origin/<task-branch>
```

and only fall back to:

```text
git checkout -b <task-branch>
```

when the task branch does not exist.

This makes review rejection retries continue from the actual prior task branch
contents instead of redoing the work from the base branch.

### 3.4 Review Continuity Rules

For review runs, preserve continuity through explicit host-curated context
rather than by defaulting to a resumed long-lived reviewer conversation.

Each review run should know:

- the current task branch name
- the current branch tip SHA
- the last reviewed branch tip SHA, if any
- the previous review rejection reason, if any
- that this may be a follow-up review of a previously rejected task

The delivery mechanism should be prompt interpolation, consistent with the
existing runner pattern that replaces placeholders in the embedded review
prompt. Add a structured placeholder such as `REVIEW_CONTEXT_PLACEHOLDER` and
render a short host-written block into it before launching the review agent.

Example rendered block:

```text
Review context:
- task branch: task/add-foo
- current branch tip: abc123
- last reviewed branch tip: def456
- previous rejection: missing unit tests for retry backoff
- review mode: follow-up review; reassess the current diff independently
```

If no prior review context exists, render an explicit neutral block instead of
omitting the section entirely.

Example neutral block:

```text
Review context:
- task branch: task/add-foo
- current branch tip: abc123
- review mode: initial review; no prior review history for this task
```

The review prompt should make the contract explicit:

- this may be a continuation of an earlier review cycle
- the current branch tip may differ from the one reviewed previously
- re-evaluate the current diff independently
- verify whether earlier rejection findings were addressed
- do not assume earlier rejection reasons still apply unchanged

This keeps continuity visible and auditable while avoiding hidden reviewer state.

### 3.5 Phase 2 Runtime Metadata

To support branch resume and explicit review continuity, store a small host-owned
runtime record under `.mato/`.

This metadata is **not required for Phase 1**. Branch resume in Phase 1 can rely
entirely on the existing `<!-- branch: ... -->` marker plus Git branch
existence checks.

Suggested location:

```text
.mato/runtime/
  task-state-<task-filename>.json
```

Suggested minimal schema:

```json
{
  "version": 1,
  "task_file": "add-foo.md",
  "task_branch": "task/add-foo",
  "target_branch": "mato",
  "last_head_sha": "abc123...",
  "last_reviewed_sha": "def456...",
  "last_outcome": "review-rejected",
  "updated_at": "2026-03-29T12:23:55Z"
}
```

Notes:

- `last_head_sha` records the branch tip most recently associated with the task.
- `last_reviewed_sha` records the branch tip last seen by the review path.
- `last_outcome` supports fallback decisions and debugging.
- `last_reviewed_sha` should be captured by the host at review-launch time,
  before the review agent runs and before the ephemeral verdict file is later
  deleted.
- This is runtime state, not task instructions, so it should not be encoded in
  HTML comments inside task files.
- All writes should use the existing atomic write helpers.

This record is intentionally small. It should not attempt to mirror every run
parameter or become a general event log.

### 3.6 Optional Work-Session Resume

Durable Copilot session resume may still be useful for implementation retries,
but it should be an optional extension rather than a prerequisite for the main
fix.

If added later, constrain it to work runs first:

- keep work and review sessions separate
- persist only a work-session identifier plus minimal compatibility metadata
- fall back to a fresh work session whenever resume is unavailable or unsafe

Do not depend on work-session resume for correctness. Branch resume should carry
the architectural weight even if conversation resume is absent.

### 3.7 Fresh-Start Conditions

The runner should fall back to a fresh branch start when resume is unsafe or
stale:

- the recorded task branch no longer exists on `origin`
- the task branch was intentionally deleted after merge-conflict handling
- the runtime metadata is missing or corrupt
- the task file has no branch marker and no runtime branch record

If optional work-session resume exists, it should also fall back to a fresh
conversation when:

- the stored session identifier is missing or invalid
- the CLI rejects the resume request
- the model or prompt contract changed incompatibly

The default policy should be conservative:

- branch resume when the branch still exists and the metadata is coherent
- otherwise fresh branch creation
- review continuity from explicit host-curated context regardless of whether any
  Copilot session resume exists

When Phase 2 runtime metadata exists, it should be cleaned up when the task
reaches a terminal state such as `completed/`, `failed/`, or operator-cancelled.
Missing cleanup should be treated as non-fatal, but stale runtime files should
not accumulate across finished task lifecycles.

### 3.8 Implementation Sketch

#### Task claim path

`internal/queue/claim.go`

- reuse an existing branch marker during claim when present
- only derive a new branch name when no marker exists

#### Work runner path

`internal/runner/task.go`

- before launching the task agent, check whether `origin/<task-branch>` exists
- if it exists, fetch and check out that branch tip in the temp clone
- otherwise create a fresh branch from the target branch

Phase 1 ends here; no new runtime metadata is required for this step.

#### Review runner path

`internal/runner/review.go`

- render a structured `REVIEW_CONTEXT_PLACEHOLDER` block into the review prompt
- record the current reviewed branch tip SHA
- pass previous review context into the review run
- keep the prompt explicit about reassessing the current diff independently

The host should resolve the current branch tip SHA immediately before launching
the review agent and persist it as `last_reviewed_sha` when Phase 2 metadata is
enabled.

#### Runtime helper

Add a small host-owned package, for example `internal/runtime/` or
`internal/taskstate/`, responsible for:

- loading the task-state record
- creating it when absent
- updating it atomically

This helper should remain intentionally narrow and should not become a general
session-management subsystem unless optional work-session resume proves out.

Its lifecycle should include best-effort deletion when tasks reach terminal
states.

### 3.9 Testing

Add tests for:

- reclaiming a backlog task reuses an existing branch marker
- work run resumes from an existing task branch instead of always creating a new
  branch
- missing branch falls back to fresh branch creation
- review rejection followed by retry preserves branch contents across the next
  work run

Phase 2 should additionally test:

- follow-up review receives explicit prior-review context via the rendered
  prompt block
- `last_reviewed_sha` is captured from the branch tip at review launch
- corrupted runtime metadata falls back safely instead of breaking task
  execution
- runtime metadata files are deleted when tasks reach terminal states

If optional work-session resume is later added, test it separately rather than
making it part of the branch-resume test matrix.

## 4. Risks and Tradeoffs

- The main remaining limitation is unchanged: branch resume cannot recover work
  that existed only in a deleted temp clone.
- Explicit review context is less magical than persistent reviewer memory, but
  it is more inspectable and better aligned with mato's current architecture.
- Adding a small runtime metadata file introduces another artifact to maintain,
  but it is much simpler than a full two-session subsystem.
- Optional work-session resume may still be worth adding later, but only after
  validating the CLI behavior and failure modes.

## 5. Expected Outcome

After this change:

- a rejected task keeps its branch identity and resumes from the actual prior
  task branch contents when available
- work retries become incremental branch follow-up work instead of repeated cold
  starts from the target branch tip
- follow-up review receives explicit prior-review context without depending on a
  hidden long-lived reviewer session
- the architecture stays consistent with mato's host-curated context model
