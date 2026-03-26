# Work Type Routing

**Priority:** High
**Effort:** Medium
**Inspired by:** Squad's `routing.md` and per-agent model selection

## Problem

All mato tasks run with the same agent configuration: same Docker image, same model,
same prompt. But tasks vary widely in complexity and domain. Documentation updates
don't need `claude-opus-4.6`. Security-sensitive changes might warrant a more capable
model. Frontend tasks might need a different Docker image with Node.js tooling.

There is also no clean place for a task author to say "this task should use a
different model than the usual rule." That makes a single global default too blunt.

## Idea

Add deterministic routing rules that map task characteristics (tags, affects
patterns, complexity) to agent configurations (model, Docker image, extra prompt
context, timeout), while still allowing explicit per-task overrides for exceptional
cases.

## How It Would Work

### Routing Configuration

Add a `routing:` section to `.mato.yaml`:

```yaml
routing:
  - match:
      tags: [docs, documentation]
    config:
      model: claude-sonnet-4
      timeout: 15m

  - match:
      affects: ["internal/security/**"]
    config:
      model: claude-opus-4.6
      timeout: 45m

  - match:
      tags: [frontend]
    config:
      docker_image: node:20
      model: claude-sonnet-4

  - match:
      estimated_complexity: simple
    config:
      model: claude-haiku-3.5
      timeout: 10m
```

### Per-Task Override

For exceptional cases, a task can override the routed default model directly in
frontmatter:

```yaml
---
id: update-readme
priority: 40
tags: [docs]
model: claude-sonnet-4
---
# Update README

Add installation instructions for the new CLI flags.
```

### Resolution

When the host claims a task, it evaluates routing rules in order against the task's
frontmatter. First match wins. Unmatched tasks use the global defaults.

The resolved config is used for that specific `runOnce(...)` invocation: model
selection, Docker image, timeout, and any extra prompt injection.

Suggested precedence:

```text
explicit CLI/copilot args
  > task frontmatter overrides
    > routing rules
      > environment/config defaults
        > hardcoded defaults
```

## Design Considerations

- Keep it simple: matching on `tags`, `affects` (glob), and `estimated_complexity`
  covers most use cases.
- The routing config is in `.mato.yaml`, not a separate file, to keep configuration
  centralized.
- Per-task frontmatter overrides should stay intentionally narrow. Starting with a
  `model` field is likely enough for the first version.
- This doesn't require a coordinator LLM like Squad's -- it's deterministic
  pattern matching in the host Go code.
- Cost savings potential is significant: routing simple tasks to cheaper models
  could reduce API costs substantially.
- The host should avoid strict validation of model names at parse time because model
  identifiers change frequently.
- This proposal subsumes standalone per-task model selection. Explicit task-level
  `model` is best treated as one override within the routing system, not as a
  separate feature.

## Relationship to Existing Features

- Extends the existing `MATO_DEFAULT_MODEL` / `MATO_DOCKER_IMAGE` / `MATO_AGENT_TIMEOUT`
  surfaces to be per-task instead of global.
- Compatible with the existing precedence chain (CLI > env > config > default);
  routing would slot between config and default.
- The `affects` matching can reuse the existing `overlappingAffects` glob logic
  in `internal/queue/overlap.go`.
- `TaskMeta` in `internal/frontmatter/frontmatter.go` would gain a `model` field for
  the first explicit per-task override.
