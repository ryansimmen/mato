# Resume Work and Review Sessions

## 1. Goal

Reduce retry churn after review rejection and transient agent failures by
preserving both:

1. **Worktree state** — retries should resume from the existing task branch when
   one already exists instead of recreating the branch from the target branch
   tip and overwriting prior work.
2. **Copilot conversation state** — work and review runs should each resume
   their own prior session so the next agent continues from earlier context
   instead of starting from scratch.

This proposal intentionally treats work and review as separate long-lived
sessions. A task may bounce between `backlog/`, `in-progress/`, and
`ready-for-review/`, but the implementation session and the review session
should not be conflated.

## 2. Scope

### In scope

- Reuse an existing `<!-- branch: ... -->` marker when reclaiming a task from
  `backlog/`.
- Resume work from the existing task branch tip when that branch is still
  present in the host repository.
- Persist separate durable session metadata for work and review under `.mato/`.
- Pass durable session identifiers to Copilot CLI via `--resume=<session-id>`
  for both work and review runs.
- Keep work and review sessions distinct for the same task file.
- Record enough metadata to detect safe resume vs fresh-start fallbacks.
- Add tests covering review rejection retries, remote-branch-missing fallback,
  and distinct work/review session handling.

### Out of scope

- Recovering unpublished work from a temp clone after an agent crashes before
  pushing its branch.
- Persisting full terminal logs or full prompt payloads for every session.
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

Review runs have a similar continuity problem at the Copilot level. Each review
run is launched as a new non-interactive Copilot session, so the reviewer does
not retain context about earlier findings, prior branch tips, or why a task was
previously rejected.

The fix requires two independent resume mechanisms:

1. **Git resume** for work runs
2. **Copilot session resume** for both work and review runs

Implementing only one of these would leave substantial retry waste in place.

### 3.2 Session Model

Each task gets up to two durable runtime session records:

- `work` session: the implementing agent's long-lived session
- `review` session: the reviewing agent's long-lived session

These sessions are separate because they have different prompts, goals, models,
and success criteria. Review context is useful for follow-up reviews, but it
should not pollute implementation reasoning. Likewise, implementation context
should not bias the review agent into self-justifying earlier choices.

### 3.3 Storage Location

Store session metadata under a new runtime directory inside `.mato/`:

```text
.mato/messages/sessions/
  work-<task-filename>.json
  review-<task-filename>.json
```

Reasons:

- `.mato/` is already gitignored runtime state.
- `messages/` already contains durable JSON coordination artifacts.
- Session metadata is machine state, not task instructions, so it should not be
  encoded as HTML comments inside task files.

All writes should use the existing atomic write helpers.

### 3.4 Session Record Shape

Minimum schema:

```json
{
  "version": 1,
  "kind": "work",
  "task_file": "add-foo.md",
  "task_branch": "task/add-foo",
  "target_branch": "mato",
  "copilot_session_id": "0cb916db-26aa-40f2-86b5-1ba81b225fd2",
  "status": "active",
  "attempt": 3,
  "created_at": "2026-03-29T12:02:20Z",
  "updated_at": "2026-03-29T12:23:55Z",
  "closed_at": "",
  "last_agent_id": "b8945c30",
  "last_head_sha": "abc123...",
  "model": "gpt-5.4",
  "reasoning_effort": "high",
  "prompt_hash": "sha256:...",
  "last_outcome": "review-rejected"
}
```

Notes:

- `copilot_session_id` is the durable identifier passed to
  `copilot --resume=<id>`.
- `last_head_sha` records the branch tip most recently associated with that
  session.
- `prompt_hash` allows the runner to decide whether a major prompt change should
  force a fresh session.
- `last_outcome` supports debugging and future status/reporting.

### 3.5 Work Resume Rules

#### Branch identity

When a task is reclaimed from `backlog/`, the host should first look for an
existing `<!-- branch: ... -->` marker in the claimed file and reuse that branch
name if present. Only tasks without a branch marker should derive a fresh branch
name from the filename and active-branch disambiguation rules.

#### Branch contents

Before launching the task agent:

1. If the task branch exists in the host repository, fetch and check out that
   branch in the temp clone.
2. Otherwise, create a fresh branch from the target branch as today.

This makes review rejection retries continue from the actual prior task branch
contents instead of redoing the work from the base branch.

#### Copilot session

For work runs:

1. Load or create the durable `work` session record for the task.
2. Pass `--resume=<copilot_session_id>` to the Copilot CLI.
3. Update the session record with the current branch tip, attempt count, prompt
   hash, model, and reasoning-effort values.

This allows the next work run to retain context about previous failures,
rejections, and partial implementation decisions even when the task loops back
through `backlog/`.

### 3.6 Review Resume Rules

For review runs:

1. Load or create the durable `review` session record for the task.
2. Pass `--resume=<copilot_session_id>` to the Copilot CLI.
3. Record both the previously reviewed branch tip and the current branch tip.

The review prompt should make the follow-up-review contract explicit:

- this is a continuation of an earlier review session
- the current branch tip may differ from the one reviewed previously
- re-evaluate the current diff independently
- verify whether earlier rejection findings were addressed
- do not assume earlier rejection reasons still apply unchanged

This keeps the benefits of continuity without encouraging stale anchoring.

### 3.7 Session Lifecycle

#### Work session lifecycle

- Create on the first successful claim before launching `runOnce()`.
- Keep active across:
  - `backlog/`
  - `in-progress/`
  - `ready-for-review/`
  - review rejection back to `backlog/`
- Close when the task reaches:
  - `completed/`
  - `failed/`
  - cancelled terminal state

#### Review session lifecycle

- Create on the first review launch after branch verification succeeds.
- Keep active across:
  - repeated reviews of the same task branch after fixes
  - transient review infrastructure failures
- Close when the task reaches:
  - `completed/`
  - `failed/`
  - cancelled terminal state

### 3.8 Fresh-Start Conditions

The runner should fall back to a fresh start when resume is unsafe or clearly
stale:

- the recorded task branch no longer exists in the host repo
- the task branch was intentionally deleted after merge-conflict handling
- the durable session metadata is missing or corrupt
- the prompt hash changes in a way deemed incompatible with continuation
- the configured model changes in a way deemed incompatible with continuation

The simplest initial rule is:

- reuse session when metadata is valid
- create a new session only when metadata is absent or obviously unusable

More aggressive invalidation rules can be added later if needed.

### 3.9 Implementation Sketch

#### New runtime helper

Add a small package, for example `internal/sessionmeta/`, responsible for:

- loading session records
- creating session records when absent
- updating session records atomically
- marking sessions closed

#### Runner integration

Likely integration points:

- `internal/queue/claim.go`
  - reuse existing branch marker during claim
- `internal/runner/task.go`
  - resume branch checkout for work runs
  - load/update work session metadata
- `internal/runner/review.go`
  - load/update review session metadata
- `internal/runner/config.go`
  - append `--resume=<session-id>` when present

### 3.10 Testing

Add tests for:

- reclaiming a backlog task reuses an existing branch marker
- work run resumes from an existing task branch instead of always creating a
  new branch
- missing remote branch falls back to fresh branch creation
- review rejection followed by retry preserves branch contents across the next
  work run
- work and review use separate durable Copilot session IDs for the same task
- repeated review runs resume the same review session
- corrupted session metadata falls back safely instead of breaking task
  execution

## 4. Risks and Tradeoffs

- Resuming review sessions may bias the reviewer toward earlier findings. The
  prompt must explicitly require reassessment of the current diff.
- Session metadata introduces another runtime artifact to maintain and clean up.
- Branch resume cannot recover work that existed only in a deleted temp clone.
- Long-lived sessions may accumulate stale context over many retries. Prompt and
  model hashes provide a future invalidation hook if needed.

## 5. Expected Outcome

After this change:

- a rejected task keeps its branch identity and resumes from the actual prior
  task branch contents when available
- the next work agent continues the earlier implementation conversation instead
  of starting over
- the next review agent continues the earlier review conversation in a separate
  session instead of reviewing from scratch
- retries become incremental follow-up work instead of repeated cold starts
