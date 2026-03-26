# Context Layers

**Priority:** High
**Effort:** Medium
**Inspired by:** Squad's directives, `decisions.md`, skills, and per-agent memory

## Problem

Several proposal ideas point at the same underlying gap: mato does not yet have a
layered, durable, repo-native memory model.

Today the system has strong task-local execution state, but limited project memory.
That leads to repeated instructions in task bodies, lost rationale across runs, and
weak retry context when a task fails.

## Idea

Add a layered context system under `.mato/` with four distinct responsibilities:

1. `directives` -- permanent repo rules every task should follow
2. `decisions` -- project-wide architectural and process decisions
3. `skills` -- human-authored domain guidance selected by task characteristics
4. `task learnings` -- per-task retry context accumulated across failed attempts

These layers should be host-managed, injected deterministically, and kept separate so
each type of context stays small, legible, and purposeful.

## Proposed Layout

```text
.mato/
├── directives.md
├── decisions.md
├── decisions/
│   └── inbox/
├── skills/
│   ├── testing-conventions.md
│   ├── api-design.md
│   └── security.md
└── learnings/
    └── <task-id>.md
```

## Layer 1: Directives

`directives.md` contains unconditional rules that should be visible to every agent
run.

Example:

```markdown
- Always use TypeScript strict mode
- Never edit generated files by hand
- Run `go test -count=1 ./...` before finishing Go changes
- Do not modify files under `vendor/`
```

Characteristics:

- human-authored
- repo-wide
- injected into every task prompt
- prescriptive rather than descriptive

## Layer 2: Decisions

`decisions.md` records architectural or process decisions that later tasks should
understand.

Example:

```markdown
### 2026-03-15: Use Zod for API validation
**Task:** add-api-validation
**What:** All API input validation uses Zod schemas.
**Why:** Type-safe, composable, generates TypeScript types.
```

Characteristics:

- durable and append-only
- project-wide
- descriptive rather than imperative
- injected into every task prompt, likely with a size budget or summary policy

To avoid direct write conflicts, agents should write candidate decision entries to
`.mato/decisions/inbox/`, and the host should merge them.

## Layer 3: Skills

`.mato/skills/*.md` contains contextual domain knowledge that only applies to certain
tasks.

Example:

```markdown
---
name: testing-conventions
triggers:
  affects: ["**/*_test.go"]
  tags: [testing, test]
---
# Testing Conventions

- Use table-driven tests with `t.Run`.
- Use `t.TempDir()` for temp directories.
- No third-party Go test frameworks.
```

Characteristics:

- human-authored
- selectively injected based on `affects`, `tags`, or similar task metadata
- small and domain-specific
- reusable across many tasks

## Layer 4: Task Learnings

`.mato/learnings/<task-id>.md` captures richer retry context for one task across
failed attempts.

Example:

```markdown
## Attempt 1 (2026-03-15T10:30:00Z)

### Approach
Added retry logic using fixed one-second delays.

### What Went Wrong
Tests expected exponential backoff and failed.

### Suggestion for Next Attempt
Use the existing backoff utility in `pkg/client/backoff.go`.
```

Characteristics:

- per-task rather than repo-wide
- accumulates only on failure or rejection
- injected only on retries for the same task
- removed or archived when the task reaches a terminal state

## Injection Model

At agent launch, the host builds the effective context in layers:

1. embed task instructions as today
2. add `directives.md`
3. add relevant excerpts from `decisions.md`
4. add matching skill files
5. add task-specific learnings if this is a retry

This keeps the system deterministic and avoids needing a separate long-lived memory
agent.

## Design Considerations

- Keep each layer small and legible so context quality stays high.
- Prefer host-managed injection over agent-managed mutation for predictability.
- Apply size budgets per layer; decisions and learnings may need summarization or
  archival once they grow.
- Make the distinction between directives and decisions explicit: rules versus
  rationale.
- Keep skills human-authored and read-only in the first version.
- Clean up per-task learnings on terminal task states so ephemeral context does not
  leak forever.

## Why One Proposal

These ideas work best as one coherent system rather than as separate features. If
implemented independently, they risk duplicating files, overlapping injection rules,
and confusing operators about where context belongs.

Treating them as layers gives mato a simple mental model:

- directives tell agents what must always be true
- decisions tell agents what the repo has already chosen
- skills tell agents how this repo handles a particular domain
- task learnings tell the next retry what already failed

## Relationship to Existing Features

- Complements `MATO_PREVIOUS_FAILURES` and `MATO_REVIEW_FEEDBACK` with richer,
  better-structured context.
- Pairs naturally with `upstream-context-inheritance.md`, which could share
  directives, decisions, and skills across repositories.
- Pairs naturally with `policy-hooks.md`: context layers guide the agent, while
  policy hooks let the host enforce hard boundaries.
