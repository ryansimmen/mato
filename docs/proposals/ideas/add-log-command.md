# Add `mato log` command for chronological task history

## Goal

Add a history-oriented command that answers "what happened recently?" across the
queue without requiring operators to manually inspect task files and message
directories.

This is an operational history view, not a perfect immutable audit log.

## Why now

`mato status` and `mato inspect` are snapshot-oriented: they explain the current
queue and the current state of one task. There is still no command that shows a
cross-task, chronological history of durable task events.

Users currently have to manually inspect `messages/completions/`, task-file
markers, and `failed/` to answer simple questions such as:

- what merged most recently?
- which task failed last night?
- what was the latest review rejection across the queue?

The codebase already persists enough durable data to make a useful phase-1
history command possible without waiting for a broader orchestration journal.

## Related proposals

- `state-machine-formalization.md`: may later enable a richer transition journal,
  but it is not a prerequisite for shipping `mato log`.

## Success criteria

Success means:

- one command surfaces recent merged, failed, and rejected events in newest-first
  order from durable sources
- text output quickly answers recent operator questions without digging through
  raw files
- JSON output exposes a stable schema suitable for scripts and dashboards
- malformed or unreadable individual records do not make the whole command
  unusable
- the command is explicit about phase-1 durability limits instead of implying a
  complete forever-history where the codebase does not currently preserve one

## Scope

### In scope

- Add a `log` subcommand to `cmd/mato/main.go`:
  ```
  mato log [--repo <path>] [--limit N] [--format text|json]
  ```
- Phase-1 event types:
  - `MERGED`
  - `FAILED`
  - `REJECTED`
- Gather merged events from `messages/completions/`
- Gather failure and rejection events by scanning task files across queue
  directories
- Normalize all gathered records into one event shape and sort newest first
- Default limit: 20 events; `--limit 0` means unlimited
- Text and JSON output
- Tests and user-facing docs

### Out of scope

- per-poll-cycle scheduler-decision logging
- explaining why a task was skipped in a specific cycle
- a generic event bus or general-purpose orchestration log
- waiting for the state-machine refactor before shipping a useful history view
- phase-1 support for `<!-- terminal-failure: ... -->`,
  `<!-- cycle-failure: ... -->`, or `<!-- cancelled: ... -->` unless the event
  taxonomy is explicitly broadened

## Relationship to existing commands

`mato log` should be a history view, not a second snapshot view:

- `mato status` answers "what is true right now?"
- `mato inspect` answers "why is this one task in its current state?"
- `mato log` should answer "what happened recently across the queue?"

That distinction should stay sharp in the implementation and user-facing docs.

## Event model

`mato log` should be a true history command. It should emit every durable event
instance from the selected sources, not only the latest outcome per task.

Example: if a task is rejected twice, fails once, and later merges, the log
should show all four events in chronological order. Collapsing to "latest state
per task" would make retries and review churn invisible, which defeats the main
value of a history command.

### Source of truth table

| Event type | Durable source | Timestamp source | Notes |
| --- | --- | --- | --- |
| `MERGED` | `messages/completions/*.json` | `merged_at` | One completion-detail JSON per merged task ID; includes commit SHA and changed files |
| `FAILED` | standalone `<!-- failure: ... -->` markers in task files | `at` field inside marker | Must scan queue task files, not just `failed/`; retryable failures may later be followed by success |
| `REJECTED` | standalone `<!-- review-rejection: ... -->` markers in task files | `at` field inside marker | Must scan queue task files, not just `failed/`; rejected tasks usually return to `backlog/` |

The important implementation detail is that failure and rejection history is not
confined to `failed/`. A task can carry those markers while still sitting in
`backlog/`, `ready-for-review/`, `ready-to-merge/`, or eventually `completed/`.

### Durability caveat

Phase 1 should describe its durability limits honestly:

- merged events are durable completion-detail records
- review rejections are relatively durable because retry cleanup preserves
  `<!-- review-rejection: ... -->` markers
- plain `<!-- failure: ... -->` history is only durable until the operator runs
  `mato retry`, because retry cleanup strips failure-related markers from the
  task file before requeueing it

That means `mato log` phase 1 is best understood as a useful operational history
view over current durable sources, not a complete immutable audit trail of every
failure that has ever happened.

## Data collection and normalization

1. Resolve repo root and `.mato/` task directory.
2. Read completion details from `messages/completions/`.
3. Scan task files across the canonical queue directories for standalone
   `<!-- failure: ... -->` and `<!-- review-rejection: ... -->` markers.
4. Normalize records into one event shape with common fields such as:
   - `timestamp`
   - `type`
   - `task_file`
   - `title` (when available)
   - `current_state` (when available)
5. Add type-specific fields:
   - merged: `commit_sha`, `files_changed`, `branch`
   - failed/rejected: `reason`, `agent_id`
6. Sort newest first.

### Sorting and stability rules

- Primary sort: `timestamp` descending
- Tie-breaks: `task_file` ascending, then `type`, then source-order within the
  originating file or directory scan
- Output should be stable across repeated runs over the same on-disk data

### Error handling rules

- Missing `messages/completions/` should be treated as empty history, not an
  error
- Malformed individual completion JSON files should emit a warning to stderr and
  be skipped
- Malformed individual marker lines should be skipped; if practical, warn once
  per affected file rather than once per malformed line
- The command should fail only for repo-resolution errors or broad directory
  access failures that prevent meaningful output

### Phase-1 source boundaries

To keep scope sharp, phase 1 should only use durable on-disk artifacts that
already exist:

- completion detail JSON files
- task-file markers

It should not infer historical events from the current queue directory alone,
from transient in-memory state, or from ephemeral host stdout/stderr output.

## Output

Text output should stay compact and scan-friendly:

```text
2026-03-23 10:15:00  MERGED    add-retry-logic   abc1234  (2 files changed)
2026-03-23 10:10:00  FAILED    broken-task       agent interrupted
2026-03-23 10:05:00  REJECTED  fix-login-bug     missing test coverage
2026-03-23 10:00:00  MERGED    update-readme     def5678  (1 file changed)
```

JSON output should be an array of event objects with stable, documented field
names:

```json
[
  {
    "timestamp": "2026-03-23T10:15:00Z",
    "type": "MERGED",
    "task_file": "add-retry-logic.md",
    "title": "Add retry logic",
    "branch": "task/add-retry-logic",
    "commit_sha": "abc1234",
    "files_changed": ["internal/queue/claim.go", "internal/queue/claim_test.go"]
  },
  {
    "timestamp": "2026-03-23T10:10:00Z",
    "type": "FAILED",
    "task_file": "broken-task.md",
    "reason": "agent interrupted",
    "agent_id": "agent-1"
  }
]
```

If future event types are added, they should be additive extensions to the JSON
schema rather than renaming existing fields.

## Implementation steps

1. Add `newLogCmd()` in `cmd/mato/main.go` with `--limit` and `--format` flags.
2. Introduce a small internal history model and renderer package or helper.
3. Reuse completion-detail reading for merged events.
4. Add task-file marker parsing helpers for extracting failure and rejection
   events with timestamps and agent IDs.
5. Scan task files across queue directories rather than only inspecting
   `failed/`.
6. Normalize, sort, and render the result set.
7. Update `README.md` and command reference docs.

## Alternatives considered

- Extend `mato status`: rejected because status is intentionally snapshot-based
- Extend `mato inspect`: rejected because inspect is intentionally single-task
- Wait for a future transition journal: rejected for phase 1 because durable
  sources already exist and provide immediate user value

## Open questions

- Should phase 1 include `terminal-failure`, `cycle-failure`, and `cancelled`
  records as additional event types, or stay narrowly scoped to merge/failure/
  rejection history?
- Should the JSON shape include `current_state` for marker-derived events when
  the task file is still present?
- Should future iterations add `--since`, `--until`, or `--task <ref>` filters,
  or keep phase 1 intentionally small?

## Rollout

- Ship `mato log` based on existing durable sources first
- Keep the history model narrow and stable
- Document that failure history before `mato retry` is visible only while the
  corresponding failure markers still exist on disk
- If the state-machine formalization later introduces an append-only transition
  journal, allow `mato log` to incorporate it as an additive enhancement rather
  than a prerequisite

## Acceptance criteria

- `mato log` with a mix of merged, failed, and rejected events renders newest
  first in text format
- `mato log --limit N` returns only the newest `N` events
- `mato log --limit 0` returns all available events
- `mato log --format json` emits the documented JSON array shape
- `mato log` handles missing history sources gracefully
- malformed individual records are skipped without making the whole command fail

## Follow-up

If `state-machine-formalization.md` later adds a transition journal, `mato log`
can optionally expand from terminal-ish outcomes into richer task-transition
history. That should remain a follow-up improvement, not a requirement for phase
1.
