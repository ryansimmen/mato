# Mato Inter-Agent Messaging

## Overview
Mato uses file-based messaging so concurrent task agents can share intent and reduce avoidable conflicts.
The channel is advisory, not authoritative: task ownership still comes from queue file moves, git remains the source of truth for branches and commits, and merge readiness still comes from moving a task into `ready-to-merge/`.

Both task agents and review agents participate in the messaging system. The review agent sends `progress` and `completion` messages following the same protocol as the task agent. However, the review verdict (approve/reject) is communicated via task file movement and HTML comments (e.g. `<!-- reviewed: ... -->`, `<!-- review-rejection: ... -->`), not via the messaging system.

Messaging is best-effort. If reading or writing messages fails, agents continue the task anyway.
The host runner enables messaging by creating the directories with `messaging.Init(...)`, injecting `MATO_MESSAGING_ENABLED=1` and `MATO_MESSAGES_DIR=/workspace/.tasks/messages`, and cleaning stale presence and old event files on each loop iteration.

## Directory Layout
```text
.tasks/
└── messages/
    ├── events/        # inter-agent event messages
    ├── completions/   # host-written completion details for merged tasks
    └── presence/      # host-managed agent presence files
```

Agents write coordination messages to `events/`. The `completions/` directory holds host-written completion details for merged tasks (see below). The `presence/` directory holds host-written agent presence files; the host writes a presence entry when claiming a task, and `CleanStalePresence` actively cleans up stale entries. Task agents should not edit `presence/` directly.

## Message Format
Event files are JSON objects matching `Message` in `messaging.go`:

```json
{
  "id": "string",
  "from": "string",
  "type": "intent | progress | conflict-warning | completion",
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
- `type`: one of the four allowed message types
- `task`: task file being worked
- `branch`: task branch
- `files`: changed file paths; usually empty for `intent`
- `body`: short human-readable note
- `sent_at`: UTC timestamp serialized from Go `time.Time`

`files` uses `omitempty`, so empty lists may be absent from the JSON.

## Message Types
Only these four message types are valid:
- `intent`: sent by the host (Go) right after a task is claimed, before the agent container starts
- `progress`: sent by the agent at each state machine transition, for observability (one per step: `VERIFY_CLAIM`, `WORK`, `COMMIT`)
- `conflict-warning`: sent by the host after pushing the task branch, with the changed file list
- `completion`: sent by the host after moving the task to `ready-for-review/`, with the final changed file list

No other message types are part of the protocol.

## Type Validation
`WriteMessage` validates the `type` field against `ValidMessageTypes` before writing. If the type is empty or not one of the four accepted values (`intent`, `progress`, `conflict-warning`, `completion`), `WriteMessage` returns an error and the message is not written. This prevents ad-hoc or mistyped message types from polluting the event stream.

Valid types (defined in `messaging.go`):
- `intent`
- `progress`
- `conflict-warning`
- `completion`

## File Claims
The host writes a file-claims index to `.tasks/messages/file-claims.json` before each agent launch. This index tells agents which files are actively claimed by other tasks.

### Who writes it
The host calls `messaging.BuildAndWriteFileClaims(tasksDir, excludeTask)` immediately after claiming a task, before launching the agent container. The just-claimed task is excluded so the agent does not see its own `affects:` entries as conflicts.

### What it contains
The file is a JSON object mapping file paths to claim records:

```json
{
  "pkg/client/http.go": {"task": "add-http-retries.md", "status": "in-progress"},
  "internal/auth/auth.go": {"task": "fix-auth-bug.md", "status": "ready-for-review"},
  "cmd/server/main.go": {"task": "refactor-server.md", "status": "ready-to-merge"}
}
```

Each key is a file path from a task's `affects:` metadata. Each value is a `FileClaim` with the task filename and its current queue status (`in-progress`, `ready-for-review`, or `ready-to-merge`).

### How it is built
`BuildAndWriteFileClaims` scans tasks in `in-progress/`, `ready-for-review/`, and `ready-to-merge/` via `taskfile.CollectActiveAffects(...)`, then builds a map of file path → claim. First writer wins: if multiple tasks claim the same file, only the first is recorded.

### How agents use it
The host injects `MATO_FILE_CLAIMS` pointing to the file-claims path inside the container. Agents can read this file during `VERIFY_CLAIM` to detect potential conflicts with other active tasks and adjust their approach accordingly.

## Agent Checkpoints

### Task Agent

Messaging maps directly to the task-agent state machine. Each step emits a `progress` message for observability:
- **Host (before agent start)**: write one `intent` via `messaging.WriteMessage(...)` after claiming the task
- `VERIFY_CLAIM`: write one `progress`, then read recent `events/*.json` for coordination awareness. If `MATO_PREVIOUS_FAILURES` is set, the agent can review prior failure records to avoid repeating the same mistakes.
- `WORK`: write one `progress`
- `COMMIT`: write one `progress`
- `ON_FAILURE`: no failure message; failure details go into the task file itself
- **Host (after agent exits)**: `postAgentPush` writes one `conflict-warning` and one `completion` after pushing the branch and moving the task to `ready-for-review/`

### Review Agent

The review agent has a simpler message flow than the task agent:
- **Host (before review start)**: no `intent` message is sent for reviews
- `VERIFY_REVIEW`: write one `progress`
- **Host (after review exit)**: `postReviewAction` reads the JSON verdict file at `.tasks/messages/verdict-<filename>.json`, writes the appropriate HTML comment marker to the task file, moves the task, and sends one `completion` message — either `"Review approved, ready for merge"` or `"Review rejected"`

The review verdict is communicated via a JSON file (`{"verdict":"approve"}` or `{"verdict":"reject","reason":"..."}`) written by the review agent to the path specified in `MATO_REVIEW_VERDICT_PATH`. The host reads this file, writes the HTML comment markers for state tracking, and handles all file moves. If no verdict file exists (agent crashed), the host falls back to checking for HTML markers in the task file before recording a review-failure.

This is another reason the channel is advisory: queue transitions and git operations still drive progress even if message I/O is unavailable.

## Guardrails
The protocol is intentionally narrow:
- **Task agent**: maximum 6 messages per task: 1 `intent` + up to 3 `progress` + 1 `conflict-warning` + 1 `completion`. The agent sends at most 4 messages; the host sends `intent`, `conflict-warning`, and `completion` via `postAgentPush`.
- **Review agent**: maximum 2 messages per review: 1 agent `progress` + 1 host `completion`. The agent sends at most 1 message; the host sends `completion` via `postReviewAction`. No `intent` or `conflict-warning` messages are sent for reviews.
- message read/write failures must not block task work
- no ad hoc or extra message types

Treat messages as coordination hints, not as locks, leases, or durable state.

## Completion Details

When the host merge queue successfully squash-merges a task branch, it writes a completion detail file to `.tasks/messages/completions/<task-id>.json`. These files capture what happened during the merge so that dependent tasks can benefit from the context.

### Who writes it

The merge queue (`merge.ProcessQueue`) writes the file immediately after a successful squash-merge commit and push, before moving the task to `completed/`.

### Format

Completion detail files are JSON objects matching `CompletionDetail` in `messaging.go`:

```json
{
  "task_id": "add-http-retries",
  "task_file": "add-http-retries.md",
  "branch": "task/add-http-retries",
  "commit_sha": "a1b2c3d4e5f6...",
  "files_changed": ["pkg/client/http.go", "pkg/client/retry.go"],
  "title": "Add HTTP retries",
  "merged_at": "2026-03-17T21:35:00Z"
}
```

Field meanings:
- `task_id`: the task's `id` from frontmatter (or filename stem)
- `task_file`: original task markdown filename
- `branch`: the task branch that was merged
- `commit_sha`: SHA of the squash-merge commit on the target branch
- `files_changed`: files modified by the squash commit
- `title`: human-readable task title
- `merged_at`: UTC timestamp of the merge

### How dependent tasks use it

When the host claims a task that has `depends_on` entries, `runner.writeDependencyContextFile(...)` reads the completion detail file for each resolved dependency and writes them as a JSON array to `.tasks/messages/dependency-context-<filename>.json`. If any completion files are found, the host injects the file path as the `MATO_DEPENDENCY_CONTEXT` environment variable. The agent prompt reads this file during `VERIFY_CLAIM` so the agent knows what files changed, which commits were created, and what branches were used by prerequisite tasks. The context file is cleaned up after the agent container exits.

### Write and read helpers

- `messaging.WriteCompletionDetail(tasksDir, detail)` atomically writes the JSON file. It sets `merged_at` to the current UTC time if not already provided and validates that `task_id` is non-empty.
- `messaging.ReadCompletionDetail(tasksDir, taskID)` reads and parses the JSON file, returning `os.ErrNotExist` if the file is not found.

## Presence
Presence files live in `.tasks/messages/presence/` and are host-managed.
The host runner calls `messaging.WritePresence(tasksDir, agentID, taskFile, branch)` immediately after claiming a task, writing JSON with `agent_id`, `task`, `branch`, and `updated_at` to `<sanitized-agent-id>.json`. Task agents should not edit `presence/` directly.

`messaging.CleanStalePresence(tasksDir)` removes presence entries for agents that are no longer active. It checks `.tasks/.locks/<agent>.pid` through `identity.IsAgentActive(...)`; if the lock is missing, unreadable, invalid, or points at a dead PID, the presence file is removed on the next cleanup pass. Since the host now actively writes presence data, this cleanup is essential for keeping the presence directory accurate.

## Garbage Collection
`messaging.CleanOldMessages(tasksDir, 24*time.Hour)` garbage-collects event files.
The runner calls it once per main loop iteration, deleting `.tasks/messages/events/*.json` files older than 24 hours.
Age is based on file modification time, not the JSON `sent_at` value. Unreadable entries are skipped silently.

Completion detail files in `completions/` are not garbage-collected; they persist for the lifetime of the task queue so that dependent tasks can read them regardless of when they run.

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
