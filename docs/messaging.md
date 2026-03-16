# Mato Inter-Agent Messaging

## Overview
Mato uses file-based messaging so concurrent task agents can share intent and reduce avoidable conflicts.
The channel is advisory, not authoritative: task ownership still comes from queue file moves, git remains the source of truth for branches and commits, and merge readiness still comes from moving a task into `ready-to-merge/`.

Messaging is best-effort. If reading or writing messages fails, agents continue the task anyway.
The host runner enables messaging by creating the directories with `messaging.Init(...)`, injecting `MATO_MESSAGING_ENABLED=1` and `MATO_MESSAGES_DIR=/workspace/.tasks/messages`, and cleaning stale presence and old event files on each loop iteration.

## Directory Layout
```text
.tasks/
└── messages/
    ├── events/      # inter-agent event messages
    └── presence/    # reserved for host-managed agent presence files
```

Agents write coordination messages to `events/`. The `presence/` directory exists and stale-file cleanup still runs, but no production code currently writes presence files; it is reserved for future host-side tooling. Task agents should not edit `presence/` directly.

## Message Format
Event files are JSON objects matching `Message` in `messaging.go`:

```json
{
  "id": "string",
  "from": "string",
  "type": "intent | conflict-warning | completion",
  "task": "task-file-name.md",
  "branch": "task/some-branch",
  "files": ["path/one", "path/two"],
  "body": "human-readable summary",
  "sent_at": "2024-05-01T12:34:56Z"
}
```

Field meanings:
- `id`: unique message ID
- `from`: sending agent ID
- `type`: one of the three allowed message types
- `task`: task file being worked
- `branch`: task branch
- `files`: changed file paths; usually empty for `intent`
- `body`: short human-readable note
- `sent_at`: UTC timestamp serialized from Go `time.Time`

`files` uses `omitempty`, so empty lists may be absent from the JSON.

## Message Types
Only these three message types are valid:
- `intent`: sent right after a task is claimed to announce planned work
- `conflict-warning`: sent in `PUSH_BRANCH`, before `git push`, with the changed file list
- `completion`: sent in `MARK_READY`, after moving the task to `ready-to-merge/`, with the final changed file list

No other message types are part of the protocol.

## Agent Checkpoints
Messaging maps directly to the task-agent state machine:
- `SELECT_TASK`: no messaging
- `CLAIM_TASK`: read recent `events/*.json`, then write one `intent`
- `CREATE_BRANCH`: no messaging
- `WORK`: no required messaging
- `COMMIT`: no required messaging
- `PUSH_BRANCH`: write one `conflict-warning` before pushing
- `MARK_READY`: move the task file, then write one `completion`
- `ON_FAILURE`: no failure message; failure details go into the task file itself

This is another reason the channel is advisory: queue transitions and git operations still drive progress even if message I/O is unavailable.

## Guardrails
The protocol is intentionally narrow:
- maximum 3 messages per task
- one `intent`, one `conflict-warning`, one `completion`
- message read/write failures must not block task work
- no ad hoc or extra message types

Treat messages as coordination hints, not as locks, leases, or durable state.

## Presence
Presence files live in `.tasks/messages/presence/` and are intended to be host-managed.
`messaging.WritePresence(tasksDir, agentID, taskFile, branch)` can write JSON with `agent_id`, `task`, `branch`, and `updated_at` to `<sanitized-agent-id>.json`, but current production code does not call it.

`messaging.CleanStalePresence(tasksDir)` still removes any presence entries for agents that are no longer active. It checks `.tasks/.locks/<agent>.pid` through `queue.IsAgentActive(...)`; if the lock is missing, unreadable, invalid, or points at a dead PID, the presence file is removed on the next cleanup pass.

## Garbage Collection
`messaging.CleanOldMessages(tasksDir, 24*time.Hour)` garbage-collects event files.
The runner calls it once per main loop iteration, deleting `.tasks/messages/events/*.json` files older than 24 hours.
Age is based on file modification time, not the JSON `sent_at` value. Unreadable entries are skipped silently.

## Reading Semantics
`messaging.ReadMessages(tasksDir, since)` scans every `.json` file in `events/`, unmarshals each file into `Message`, keeps only messages with `sent_at` strictly after `since`, and sorts the result by `sent_at` then `id`.
Consumers should assume messages can be missing, delayed, duplicated by intent, or already deleted.

## Filename Convention
Task agents currently write event files directly from shell with names like `${MSG_ID}.json`, where `MSG_ID` is already embedded inside the JSON payload. That means agent-produced files are typically simple names such as:

```text
20260101T000000Z-agent-7-intent.json
```

The Go helper `messaging.WriteMessage(...)` is still available for host-side tooling and tests. When that helper writes an event, the filename is:

```text
<timestamp>-<from>-<type>-<id>.json
```

Example:

```text
20240501T123456.123456789Z-agent-1-intent-abc12345.json
```

Go-helper construction details:
- `timestamp` uses UTC format `20060102T150405.000000000Z`
- `from`, `type`, and `id` are sanitized to `[a-zA-Z0-9._-]`
- invalid characters become `-`
- leading/trailing `-`, `_`, and `.` are trimmed
- empty parts fall back to `unknown` or `message`

Readers only require a `.json` file; the Go helper naming scheme is available for tooling, but it is not the canonical runtime convention for agent-written messages.
