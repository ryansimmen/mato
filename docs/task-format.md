# mato Task File Format Reference

<!-- NOTE: The task format spec is also duplicated in .github/skills/mato/SKILL.md
     (distributed standalone via scripts/install-skill.sh). Keep both in sync. -->

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
<!-- claimed-by: agent-7  claimed-at: 2026-01-01T00:00:00Z -->
---
id: add-http-retries
priority: 10
depends_on: [setup-http-client]
affects:
  - pkg/client/http.go
  - pkg/client/retry.go
max_retries: 3
---
<!-- failure: mato-recovery at 2026-01-01T00:05:00Z — agent was interrupted -->
# Add HTTP retries
Implement retry handling for transient 5xx responses.
```

Notes:
- If present, frontmatter must be closed by a second `---` line. The parser skips leading empty lines and full-line HTML comments (e.g. `<!-- claimed-by: ... -->`) before looking for the opening `---`, since claim metadata may be prepended above the frontmatter block.
- Runtime metadata is stored as full-line HTML comments and is auto-managed.
- The markdown body starts after the frontmatter block.
- Agents are instructed to ignore frontmatter and these HTML comments when reading the task.
- The parser strips only **scheduler-managed** HTML comment lines from the body it returns. The managed prefixes are: `claimed-by`, `branch`, `failure`, `review-failure`, `review-rejection`, `reviewed`, `cancelled`, `cycle-failure`, `terminal-failure`, and `merged`. All other HTML comments (e.g. `<!-- TODO: ... -->` or `<!-- example -->`) are preserved in the body so task authors can use them freely in instructions.

## Frontmatter Fields
Supported keys come from `TaskMeta`. Unknown keys are currently ignored.
Strings may be quoted with `'` or `"`.
Arrays may be written as inline lists (`[a, b]`) or block lists.

Most tasks need only markdown instructions plus a few common scheduler fields
(`priority`, `depends_on`, and `affects`).

### Common scheduler fields

| Field | Type | Default | Reference |
| --- | --- | --- | --- |
| `id` | string | filename without `.md` | Stable task ID. If omitted, `my-task.md` becomes `my-task`. Use this in `depends_on`. Completed deps match either explicit `id` or filename stem. Unknown frontmatter keys are ignored. |
| `priority` | int | `50` | Lower numbers are higher priority. The host derives claim order from the effective runnable backlog, sorted by priority ascending and then filename ascending; `.queue` exports that same derived order for inspection. |
| `depends_on` | string array | empty | IDs that must be completed before a task is claimable. `depends_on` is authoritative regardless of directory placement: tasks with unmet dependencies belong in `waiting/`, and if one is found in `backlog/` the host moves it back to `waiting/` during reconcile and again at claim time as a safety net. No dependencies means the task is immediately ready. Circular dependencies (including self-dependencies) are detected and the affected tasks are moved to `failed/` with a `<!-- cycle-failure: -->` marker. |
| `affects` | string array | empty | Expected touched paths. Prefer precise file paths (e.g. `pkg/client/http.go`) over broad globs or directory prefixes — use globs only when the task genuinely spans many files in a directory. Include likely test files when the task will add or update tests (e.g. `pkg/client/http_test.go`), and include documentation files when the task changes user-visible behavior (e.g. `docs/configuration.md`). Overlap prevention compares entries and excludes the lower-priority conflicting task from `.queue` (it stays in `backlog/` until the conflict clears). Exact strings are compared literally; an entry ending with `/` is treated as a directory prefix that matches any path underneath it (e.g. `pkg/client/` conflicts with `pkg/client/http.go`). Entries containing glob metacharacters (`*`, `?`, `[`, `{`) are matched as glob patterns using `doublestar` syntax — `*` matches within a single path segment, `**` matches across path separators, `?` matches a single character, `[abc]` matches character classes, and `{a,b}` supports brace expansion (e.g. `internal/runner/*.go` conflicts with `internal/runner/task.go`). Combining glob metacharacters with a trailing `/` is invalid and treated as a fatal task error: the queue moves such tasks to `failed/`, and `mato doctor` reports them at error severity (exit code 2). Unsafe path entries — absolute paths (e.g. `/etc/passwd`) and path-traversal entries that escape the repository root (e.g. `../../secret`) — are stripped during parsing and reported by `mato doctor` at error severity (code `tasks.unsafe_affects`). The stripped entries are recorded in structured metadata so diagnostics can report exactly which entries were removed and why. |

For queue-focused preflight checks on task metadata and dependency integrity, use
`mato doctor --only queue,tasks,deps`.

### Advanced scheduler fields

| Field | Type | Default | Reference |
| --- | --- | --- | --- |
| `max_retries` | int | `3` | Maximum number of `<!-- failure: ... -->` records before the task moves to `failed/`. Must be a non-negative integer (≥ 0); negative values are rejected at parse time. A task with `max_retries: 3` is moved to `failed/` once it accumulates 3 failure records (i.e. `failures >= max_retries`). The host merge queue reads this per-task from frontmatter (authoritative). The agent receives the same resolved value via `MATO_MAX_RETRIES` as a safety net. |

### Frontmatter syntax examples
Inline arrays:
```yaml
depends_on: [setup-http-client, add-config]
```
Block arrays:
```yaml
affects:
  - pkg/client/http.go
  - pkg/client/retry.go
```
Directory prefix (trailing `/` matches any file under the path):
```yaml
affects:
  - pkg/client/
```
Glob patterns (metacharacters match files by pattern):
```yaml
affects:
  - internal/runner/*.go        # all .go files directly in internal/runner/
  - internal/**/*_test.go       # all test files anywhere under internal/
  - internal/{runner,queue}/*.go # .go files in runner/ or queue/
```
Scalars:
```yaml
id: add-http-retries
priority: 10
max_retries: 3
```

### Example: strong `affects` list

A well-populated `affects` list names implementation files, their corresponding
tests, and any docs affected by the change:

```yaml
affects:
  - internal/queue/reconcile.go
  - internal/queue/reconcile_test.go
  - internal/integration/reconcile_test.go
  - docs/architecture.md
```

Avoid overly broad entries like `internal/queue/` or `internal/**/*.go` when you
can enumerate the specific files. Broad patterns block other tasks from touching
*any* file under that path, even unrelated ones.

## Runtime Metadata
mato and its agents write runtime state as HTML comments. These lines are bookkeeping, not instructions.
Do not edit them manually.

Expected comment patterns:
```html
<!-- claimed-by: agent-7  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/add-http-retries -->
<!-- failure: agent-7 at 2026-01-01T00:03:00Z step=WORK error=tests_failed files_changed=queue.go,queue_test.go -->
<!-- review-failure: review-agent-3 at 2026-01-01T00:05:00Z step=DIFF error=could_not_fetch_branch -->
<!-- review-rejection: review-agent-3 at 2026-01-01T00:06:00Z — tests do not cover the retry backoff logic; add unit tests for exponential delays -->
<!-- reviewed: review-agent-3 at 2026-01-01T00:07:00Z — approved -->
<!-- cancelled: operator at 2026-01-01T00:07:30Z -->
<!-- cycle-failure: mato at 2026-01-01T00:08:00Z — circular dependency -->
<!-- terminal-failure: mato at 2026-01-01T00:09:00Z — unparseable frontmatter: yaml: line 2: did not find expected ',' or ']' -->
<!-- merged: merge-queue at 2026-01-01T00:10:00Z -->
```

What they mean:
- `claimed-by` records which agent owns an in-progress task.
- `branch:` records the task branch identity selected by the host. On claim, mato reuses an existing standalone branch marker when safe; otherwise it derives a sanitized branch name from the filename and may append a disambiguation suffix when another active task already uses that branch. The merge queue reads this marker first and falls back to the filename-derived branch when absent. Only complete markers with the closing `-->` are recognized; unterminated or malformed branch comments are ignored.
- `failure:` records a failed task agent attempt; failure records are counted against the task's `max_retries` budget. Recovery and merge logic may also append `failure:` records (e.g. `mato-recovery` or `merge-queue`).
- `review-failure:` records a review infrastructure failure (e.g. network blip during `git fetch`, diff timeout). These are tracked separately from task failure records and do **not** count against the task's `max_retries` budget. Only review-failure records are counted for the review retry budget.
- `review-rejection:` records feedback from the review agent when rejecting a task. Format: `<!-- review-rejection: <agent-id> at <timestamp> — <feedback> -->`. Review rejections do **not** count against `max_retries`. The feedback is passed to the implementing agent via the `MATO_REVIEW_FEEDBACK` environment variable on the next attempt.
- `reviewed:` records that the review agent approved the task. Format: `<!-- reviewed: <agent-id> at <timestamp> — approved -->`. The host writes this after reading the review agent's verdict, then moves the task to `ready-to-merge/`.
- `cancelled:` records that an operator deliberately withdrew the task from the queue with `mato cancel`. Format: `<!-- cancelled: operator at <timestamp> -->`. Cancelled markers do **not** count against the task's `max_retries` budget. `mato retry` removes them and requeues the task to `backlog/`.
- `cycle-failure:` records that the task was detected as part of a circular dependency during dependency resolution. Format: `<!-- cycle-failure: mato at <timestamp> — circular dependency -->`. The task is moved to `failed/` when this marker is appended. Cycle-failure markers do **not** count against the task's `max_retries` budget. To recover, fix the `depends_on` entries to break the cycle and move the task back to `waiting/`.
- `terminal-failure:` records that the host automatically moved a task to `failed/` due to a non-recoverable structural problem. Format: `<!-- terminal-failure: mato at <timestamp> — <reason> -->`. Written before the task is moved to `failed/` by reconciliation or review candidate selection. Reasons include unparseable YAML frontmatter, invalid glob syntax in `affects`, and review retry budget exhaustion. Terminal-failure markers do **not** count against the task's `max_retries` budget. To recover, fix the underlying issue (e.g. correct the YAML or glob syntax) and move the task back to `waiting/` or `backlog/`.
- `merged:` records that the merge queue successfully squashed the task branch into the target branch.
- The host parses `claimed-by` and strips scheduler-managed HTML comment lines from the task body before agent-facing interpretation. Non-managed HTML comments are preserved.

## Examples
### Full task file with all fields
```md
---
id: add-http-retries
priority: 10
depends_on:
  - setup-http-client
affects: [pkg/client/http.go, pkg/client/retry.go]
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
- Put tasks with dependencies in `waiting/` until they are satisfied.
- Put tasks without dependencies in `backlog/`.
- `waiting/` is the canonical state for tasks with unmet dependencies. Conflict-deferred tasks stay in `backlog/` but are excluded from the runnable backlog and the derived `.queue` manifest.
- Manual or automatic placement in `backlog/` does not override `depends_on`; dependency-blocked backlog tasks are moved back to `waiting/`.
- Tasks with circular dependencies (including self-dependencies) are automatically moved from `waiting/` to `failed/` with a `<!-- cycle-failure: -->` marker. To recover, fix the `depends_on` entries and move the task back to `waiting/`.
- Completed agent work moves through `ready-for-review/` (AI review gate) before reaching `ready-to-merge/`.
- mato writes `.queue` from the effective runnable backlog for operator visibility; the host uses that same runnable backlog model directly when deciding what to claim.

## Branch Naming
Each task gets a stable git branch identity managed by the host. When a task already has a standalone `<!-- branch: ... -->` marker, mato reuses that branch on the next claim unless it collides with another active task. Otherwise mato derives a branch from the filename and prefixes it with `task/`. If that derived branch is already in use by another active task, mato appends a deterministic disambiguation suffix.

**Sanitization rules (applied in order):**
1. Strip the `.md` suffix
2. Replace any character that is not alphanumeric or a hyphen (`[^a-zA-Z0-9-]`) with `-`
3. Collapse consecutive dashes into a single dash
4. Trim leading and trailing dashes
5. If the result is empty, fall back to `unnamed`

**Examples:**

| Task filename | Branch name |
| --- | --- |
| `add-http-retries.md` | `task/add-http-retries` |
| `fix  spaces & symbols!.md` | `task/fix-spaces-symbols` |
| `--leading-dashes--.md` | `task/leading-dashes` |
| `___.md` | `task/unnamed` |

Users don't need to do anything special — just pick a descriptive kebab-case filename and mato handles the rest. The `<!-- branch: ... -->` runtime comment records the authoritative branch name for future retries and review cycles. When no marker exists yet, the initial branch is still derived deterministically from the filename (with a collision suffix when needed).

## Backward Compatibility
Plain markdown task files work fine. If frontmatter is missing, mato applies these defaults:
- `id`: filename without `.md`
- `priority`: `50`
- `depends_on`: empty
- `affects`: empty
- `max_retries`: `3`

That means older task files can stay as simple markdown instructions with no metadata at all.
