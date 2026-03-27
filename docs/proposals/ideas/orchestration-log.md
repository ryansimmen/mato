# Orchestration Log

**Priority:** High
**Effort:** Medium
**Inspired by:** Squad's `.squad/orchestration-log/`

## Problem

When mato skips, defers, retries, or merges work, the durable state is spread across
task files, queue directories, completion records, and transient stdout logs. Users
can usually tell *what* happened, but it is much harder to answer *why* the host
made a specific decision during a given poll cycle.

Examples:

- Why was this backlog task not claimed even though it looked runnable?
- Which `affects` conflict caused a task to be deferred?
- Why did review not run for a ready task in this cycle?
- Why did the merge queue skip one task and take another?

## Idea

Add a durable orchestration log under `.mato/orchestration-log/` that records the
host's decision-making for each poll cycle in a structured, machine-readable form.

This is not a replacement for task metadata. It is an operator-facing event trail
that explains scheduler and host behavior over time.

## How It Would Work

### Event Files

For each poll cycle, mato writes a timestamped JSONL file:

```text
.mato/orchestration-log/2026-03-26T15-04-05Z.jsonl
```

Each line is a single event object.

### Example Events

```json
{"type":"cycle_started","at":"2026-03-26T15:04:05Z","queue":{"backlog":4,"waiting":2,"in_progress":1}}
{"type":"task_deferred","at":"2026-03-26T15:04:05Z","task_id":"add-auth-tests","reason":"affects_overlap","conflicts_with":["add-login-endpoints"]}
{"type":"task_blocked","at":"2026-03-26T15:04:05Z","task_id":"add-login-endpoints","reason":"dependency_waiting","depends_on":["setup-auth-models"]}
{"type":"task_claimed","at":"2026-03-26T15:04:06Z","task_id":"setup-auth-models","branch":"task/setup-auth-models","agent_id":"a1b2c3d4"}
{"type":"review_completed","at":"2026-03-26T15:11:10Z","task_id":"fix-timeout","verdict":"rejected","reason":"missing regression test"}
{"type":"merge_selected","at":"2026-03-26T15:12:30Z","task_id":"update-readme","reason":"highest_priority_ready_to_merge"}
```

### Event Types

Initial coverage could include:

- `cycle_started`
- `task_blocked`
- `task_deferred`
- `task_claimed`
- `task_run_finished`
- `review_started`
- `review_completed`
- `merge_selected`
- `merge_skipped`
- `merge_completed`
- `cleanup_recovered`
- `cleanup_lock_removed`

### CLI Integration

This log should power future operator commands rather than forcing users to read raw
JSONL directly. Likely follow-ons:

- extend `mato log` with an orchestration mode
- let `mato inspect` show the latest orchestration events for a task
- optionally expose a `--format json` stream for external tooling

## Design Considerations

- Default to JSONL so logs are append-friendly and easy to parse.
- Keep logs local and operational by default; unlike task files or decisions, these
  should probably be gitignored because they are high-volume and machine-specific.
- Redact absolute host paths and other machine-local details where possible.
- Add retention controls so old logs are rotated or compacted automatically.
- The event schema should be stable enough for tooling but narrow enough to avoid
  turning this into a second source of truth.
- Prefer logging decisions from existing queue analysis paths rather than
  re-deriving scheduler logic just for logging.

## Relationship to Existing Features

- Complements `add-log-command`: task history tells you what happened; an
  orchestration log tells you why the host decided it.
- Complements `add-task-inspect-command` by giving it causal events to surface.
- Fits naturally with `observability-telemetry.md`: telemetry is for metrics and
  tracing backends; orchestration logs are for local, durable debugging.
