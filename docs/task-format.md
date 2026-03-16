# mato Task File Format Reference
## Overview
A mato task file is a markdown `.md` file with optional YAML frontmatter.
The frontmatter is scheduler metadata for the host; the markdown body is the agent's actual instructions.
Plain markdown files with no frontmatter are fully supported.

Conceptually, a task file has three layers:
1. optional YAML frontmatter
2. runtime metadata in HTML comments
3. markdown body instructions

## File Structure
```md
---
id: add-http-retries
priority: 10
depends_on: [setup-http-client]
affects:
  - pkg/client/http.go
  - pkg/client/retry.go
tags: [backend, reliability]
estimated_complexity: medium
max_retries: 3
---
<!-- claimed-by: agent-7  claimed-at: 2026-01-01T00:00:00Z -->
<!-- failure: mato-recovery at 2026-01-01T00:05:00Z — agent was interrupted -->
# Add HTTP retries
Implement retry handling for transient 5xx responses.
```

Notes:
- If present, frontmatter must be the first block and must be closed by a second `---` line.
- Runtime metadata is stored as full-line HTML comments and is auto-managed.
- The markdown body starts after the frontmatter block.
- Agents are instructed to ignore frontmatter and these HTML comments when reading the task.
- The parser strips full-line HTML comment lines from the body it returns.

## Frontmatter Fields
Supported keys come from `TaskMeta`. Unknown keys are currently ignored.
Strings may be quoted with `'` or `"`.
Arrays may be written as inline lists (`[a, b]`) or block lists.

| Field | Type | Default | Reference |
| --- | --- | --- | --- |
| `id` | string | filename without `.md` | Stable task ID. If omitted, `my-task.md` becomes `my-task`. Use this in `depends_on`. Completed deps match either explicit `id` or filename stem. |
| `priority` | int | `50` | Lower numbers are higher priority. `.queue` is generated from `backlog/` sorted by priority ascending, then filename ascending. |
| `depends_on` | string array | empty | IDs that must be completed before a waiting task can be promoted into `backlog/`. No dependencies means the task is immediately ready. |
| `affects` | string array | empty | Expected touched paths. Backlog overlap prevention compares entries by exact string match and defers the lower-priority conflicting task to `waiting/`. |
| `tags` | string array | empty | Free-form categorization labels. Parsed today, but not used by queue reconciliation. |
| `estimated_complexity` | string | empty | Human hint for task size. Use `simple`, `medium`, or `complex` by convention; current parsing does not enforce these values. |
| `max_retries` | int | `3` | Maximum number of `<!-- failure: ... -->` records before the task is moved to `failed/`. Enforced by both the agent prompt (during `CLAIM_TASK` and `ON_FAILURE`) and the host merge queue. |

### Frontmatter syntax examples
Inline arrays:
```yaml
depends_on: [setup-http-client, add-config]
tags: [backend, reliability]
```
Block arrays:
```yaml
affects:
  - pkg/client/http.go
  - pkg/client/retry.go
```
Scalars:
```yaml
id: add-http-retries
priority: 10
estimated_complexity: medium
max_retries: 3
```

## Runtime Metadata
mato and its agents write runtime state as HTML comments. These lines are bookkeeping, not instructions.
Do not edit them manually.

Expected comment patterns:
```html
<!-- claimed-by: agent-7  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/add-http-retries -->
<!-- failure: agent-7 at 2026-01-01T00:03:00Z step=WORK error=tests_failed files_changed=queue.go,queue_test.go -->
<!-- merged: merge-queue at 2026-01-01T00:10:00Z -->
```

What they mean:
- `claimed-by` records which agent owns an in-progress task.
- `branch:` records the pushed task branch name after a successful agent push; the merge queue reads this first and falls back to the filename-derived branch when absent.
- `failure:` records a failed attempt; failure lines are counted for retry handling.
- Recovery and merge logic may append `failure:` lines such as `mato-recovery` or `merge-queue`.
- `merged:` records that the merge queue successfully squashed the task branch into the target branch.
- The host parses `claimed-by` and strips full-line HTML comments from the task body before agent-facing interpretation.

## Examples
### Full task file with all fields
```md
---
id: add-http-retries
priority: 10
depends_on:
  - setup-http-client
affects: [pkg/client/http.go, pkg/client/retry.go]
tags: [backend, reliability]
estimated_complexity: medium
max_retries: 3
---
# Add HTTP retries
Implement retry handling for transient 5xx responses.
Add or update tests.
```

### Minimal task file
```md
---
priority: 20
---
# Update help output
Document the new CLI flags in the help text.
```

### Task with no frontmatter
```md
# Clean up status output
Simplify the status summary formatting.
```

## Where to Place Tasks
- Put tasks with dependencies in `waiting/`.
- Put tasks without dependencies in `backlog/`.
- `waiting/` is also where backlog tasks are sent when they conflict with a higher-priority task's `affects` entries.
- mato writes `.queue` from the current `backlog/`, ordered by priority and then filename.

## Backward Compatibility
Plain markdown task files work fine. If frontmatter is missing, mato applies these defaults:
- `id`: filename without `.md`
- `priority`: `50`
- `depends_on`: empty
- `affects`: empty
- `tags`: empty
- `estimated_complexity`: empty
- `max_retries`: `3`

That means older task files can stay as simple markdown instructions with no metadata at all.
