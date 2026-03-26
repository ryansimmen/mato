# Issue-to-Task Triage

**Priority:** Medium
**Effort:** Medium
**Inspired by:** Squad's Ralph work monitor and `squad triage` command

## Problem

There's a manual gap between "issues exist on GitHub" and "task files are in the
mato queue." Someone has to read issues, write task markdown files, set frontmatter,
and place them in the right queue directory. This is tedious for repos with many
open issues.

## Idea

Add a `mato triage` command that scans GitHub issues and generates task files from
them, optionally with dependency detection and priority assignment.

## How It Would Work

### Basic Flow

```bash
# One-shot: scan issues and generate tasks
mato triage

# Continuous: poll every 10 minutes
mato triage --watch --interval 10m

# Filter by label
mato triage --label "mato" --label "ready"
```

1. `mato triage` uses `gh issue list` to find issues matching the filter criteria.
2. For each issue, it generates a task file:
   - Filename: kebab-case of the issue title (e.g. `fix-login-timeout.md`)
   - Frontmatter: `id` from issue number, `tags` from issue labels, link to issue
   - Body: issue title as heading + issue body as task description
3. The task file is placed in `backlog/` (no dependencies) or `waiting/` (if
   dependencies are detected from issue references).
4. Issues that already have corresponding task files are skipped (idempotent).

### Generated Task Example

```markdown
---
id: issue-42
priority: 30
tags: [bug, backend]
affects: []
---
# Fix login timeout on slow connections

Resolves: https://github.com/org/repo/issues/42

Users on slow connections experience a timeout during login because the
HTTP client uses a 5-second default timeout. Increase to 30 seconds and
add retry logic for transient failures.
```

### Configuration

```yaml
# .mato.yaml
triage:
  labels: ["mato", "agent-ready"]
  default_priority: 30
  auto_affects: true  # attempt to detect affected files from issue body
```

## Design Considerations

- Triage should be idempotent: running it twice doesn't create duplicate tasks.
  Match by issue number or a `<!-- issue: #42 -->` marker in the task file.
- The `affects` field is hard to auto-detect reliably from issue descriptions.
  Consider leaving it empty by default and letting users fill it in, or using
  a heuristic based on file paths mentioned in the issue body.
- Dependency detection could look for "depends on #N" or "blocked by #N" in
  issue text and translate to `depends_on` entries.
- The command should work without `--watch` for one-shot CI use.
- Consider a `--dry-run` flag that prints what tasks would be generated.
- Issue state sync: when a task completes, optionally close the linked issue
  or add a comment. This could be a post-merge hook.

## Relationship to Existing Features

- Uses `gh` CLI which is already a mato prerequisite.
- Generated task files use the standard task format from `docs/task-format.md`.
- Works with existing dependency resolution, overlap prevention, and merge queue.
- The `--watch` mode would be a separate process from the main `mato` run loop,
  or could be integrated into the polling loop as an optional step.
