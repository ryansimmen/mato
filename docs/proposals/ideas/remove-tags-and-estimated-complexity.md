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

- `internal/frontmatter/frontmatter.go:33` — `Tags []string` is parsed and filtered
  via `filterEmpty` (line 119) but never referenced downstream.
- `internal/frontmatter/frontmatter.go:34-35` — `EstimatedComplexity` comment says
  explicitly: "parsed for external consumers; not used internally."
- No code path in `queue/`, `runner/`, `status/`, `dag/`, `doctor/`, `inspect/`, or
  `graph/` reads `meta.Tags` or `meta.EstimatedComplexity`.
- The "external consumers" referenced in the comment have no evidence of existing in
  this repository or its integrations.

## Idea

Remove both fields from the supported task format:

1. Remove `Tags` and `EstimatedComplexity` from the `TaskMeta` struct in
   `internal/frontmatter/frontmatter.go`.
2. Remove the `filterEmpty(meta.Tags)` call.
3. Remove both fields from documentation: `README.md` (Quick Start example),
   `docs/task-format.md` (informational fields section, all examples),
   `.github/skills/mato/SKILL.md`.
4. Update tests in `frontmatter_test.go` and `taskfile_test.go` that reference
   these fields.

Existing task files containing these fields will continue to parse without errors —
Go's YAML unmarshaler silently ignores unknown keys.

## Design Considerations

- **Backward compatibility:** Existing task files with `tags` or
  `estimated_complexity` will parse without warnings or errors. No migration is
  needed.
- **External consumers:** The struct comment mentions "external consumers" but no
  evidence of such consumers exists. If discovered, the fields could be deprecated
  with a changelog note rather than removed immediately.
- **Scope creep guard:** If either field gains a concrete consumer in the future
  (e.g., tag-based skill matching, complexity-aware scheduling), it can be re-added
  at that point with a clear design and actual behavior.
- **Coordinated update:** The removal touches user-facing docs in three places
  (README, task-format.md, SKILL.md) and should be done as a single change to
  avoid stale references.

## Relationship to Existing Features

- If the context-layers proposal (`context-layers.md`) adds tag-based skill
  matching, `tags` could be reintroduced with a concrete consumer at that point.
  Removing dead fields now does not block that future work.
