# Orchestration Log as State Machine Hook

**Priority:** Medium
**Effort:** Low
**Depends on:** `state-machine-formalization`

## Problem

When mato skips, defers, retries, or merges work, the durable state is spread
across task files (HTML comment markers), completion records in
`messages/completions/`, and transient stdout/stderr output. Users can usually
tell *what* happened by inspecting task markers and queue directories, but it is
harder to answer *why* the host made a specific decision.

The existing `orchestration-log` proposal addresses this gap but as a standalone
system with per-poll-cycle JSONL files, a broad event schema, retention controls,
and CLI integration. That scope creates a risk of becoming a second source of
truth alongside task-file markers and messaging JSON.

## Idea

Instead of a standalone logging system, emit structured log entries as a
side-effect of the `TransitionTask()` function from the state machine
formalization proposal. This gives durable decision history "for free" without
introducing a separate event bus, schema, or storage model.

### How It Would Work

When `TransitionTask()` successfully moves a task between states, it appends a
single JSON line to `.mato/orchestration-log/transitions.jsonl`:

```json
{"at":"2026-03-26T15:04:06Z","task":"add-retry-logic.md","from":"backlog","to":"in-progress","agent":"a1b2c3d4"}
{"at":"2026-03-26T15:11:10Z","task":"add-retry-logic.md","from":"in-progress","to":"ready-for-review","agent":"a1b2c3d4"}
{"at":"2026-03-26T15:12:30Z","task":"add-retry-logic.md","from":"ready-for-review","to":"ready-to-merge"}
{"at":"2026-03-26T15:15:00Z","task":"add-retry-logic.md","from":"ready-to-merge","to":"completed"}
```

### Event Schema

Each line is a JSON object with:

| Field | Type | Description |
| --- | --- | --- |
| `at` | string | UTC RFC3339 timestamp |
| `task` | string | Task filename |
| `from` | string | Source state directory |
| `to` | string | Destination state directory |
| `agent` | string | Agent ID (when applicable) |
| `reason` | string | Optional reason code (e.g. `deps_satisfied`, `review_rejected`, `merge_conflict`, `retry_exhausted`) |

The schema is intentionally narrow: it records *what moved where and when*, not
every scheduler decision or poll-cycle detail. Decision context (which tasks were
deferred, which dependencies blocked) is already available through `mato inspect`
and `mato status`.

### What this is not

- Not a per-poll-cycle event file. One append-only log file, not thousands.
- Not a standalone event bus. No separate event types, no event consumers, no
  subscription model.
- Not a replacement for task-file markers. The `<!-- failure: -->`,
  `<!-- reviewed: -->`, and `<!-- merged: -->` markers stay as the authoritative
  task-level state record.
- Not a replacement for `messages/events/`. Inter-agent messaging serves a
  different purpose (coordination between concurrent agents).

### What this enables

- `mato log` can read `transitions.jsonl` to show a chronological task history
  without scanning every task file in `completed/` and `failed/`.
- `mato inspect` can show the transition history for a specific task.
- Operators can `tail -f` the log to watch host behavior in real time.
- External monitoring can parse the JSONL stream.

## Design Considerations

- The log file should be gitignored (it is machine-local, high-volume operational
  data).
- Writes should be best-effort: if the log write fails, the transition still
  succeeds. The log is advisory, not authoritative.
- Add basic retention: rotate or truncate when the file exceeds a size threshold
  (e.g. 10 MB).
- The `reason` field should use a fixed set of known codes, not free-form text.
- Since this depends on `state-machine-formalization`, it should be implemented
  as a follow-up after `TransitionTask()` lands.

## Relationship to Existing Features

- Supersedes the broader `orchestration-log` proposal with a narrower, lower-risk
  approach.
- Complements `add-log-command` by providing the data source it needs.
- Built on top of `state-machine-formalization` — the hook point is the
  `TransitionTask()` function.
