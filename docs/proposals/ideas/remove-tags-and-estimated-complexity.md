# Remove `tags` and `estimated_complexity` from the Task Format

**Priority:** High
**Effort:** Low
**Source:** Multi-model feature research debate (GPT-5.4, Claude Opus 4.6, Gemini 3.1 Pro)

## Problem

The `tags` and `estimated_complexity` frontmatter fields are parsed into `TaskMeta`
but never consumed by any internal logic. No scheduling, queue, diagnostic, display,
or agent code reads these values after parsing.

Despite being non-functional, both fields are actively taught to new users:

- The README Quick Start example includes `tags: [backend]`.
- The task-format docs, skill reference, and full-task examples all include
  `estimated_complexity: medium`.
- `docs/task-format.md` documents them under "Informational fields" with the caveat
  "not used by queue reconciliation" and "safe to omit."

This creates adoption friction: users writing task files see these fields in examples,
assume they affect behavior, invest time choosing values, then discover they do
nothing.

## Evidence

- Before removal, `internal/frontmatter/frontmatter.go` parsed both fields into
  `TaskMeta`, and `tags` also passed through `filterEmpty(...)`, but neither field
  had any downstream consumer.
- No code path in `queue/`, `runner/`, `status/`, `dag/`, `doctor/`, `inspect/`, or
  `graph/` read those fields.
- The "external consumers" mentioned in the old struct comment had no evidence of
  existing in this repository or its integrations.

## Idea

Remove both fields from the supported task format and scrub them from the repo:

1. Remove `Tags` and `EstimatedComplexity` from the `TaskMeta` struct in
   `internal/frontmatter/frontmatter.go`.
2. Remove the `filterEmpty(meta.Tags)` call.
3. Remove both fields from user-facing docs and examples: `README.md`,
   `docs/task-format.md`, `.github/skills/mato/SKILL.md`, and any stale examples
   in historical proposal docs that still teach these fields.
4. Update tests in `frontmatter_test.go`, `taskfile_test.go`, and any other
   fixtures that still mention the removed keys.
5. Leave stale `tags` / `estimated_complexity` keys ignored if they still appear
   in old task files; no special parsing or warnings are needed.

## Design Considerations

- **No special compatibility machinery:** Unknown frontmatter keys are already
  ignored, so old files can remain harmlessly stale without adding deprecation
  shims, warnings, or migration logic.
- **External consumers:** The struct comment mentions "external consumers" but no
  evidence of such consumers exists in this repo. The burden of proof should be on
  a real consumer before carrying dead fields indefinitely.
- **Scope creep guard:** If either field gains a concrete consumer in the future
  (e.g., tag-based skill matching, complexity-aware scheduling), it can be added
  back with an explicit design and actual behavior.
- **Repo-wide cleanup:** This should be treated as a full consistency pass, not
  just a parser change, so examples and tests stop reintroducing the dead fields.

## Relationship to Existing Features

- If the context-layers proposal (`context-layers.md`) adds tag-based skill
  matching, `tags` could be reintroduced with a concrete consumer at that point.
  Removing dead fields now does not block that future work.
