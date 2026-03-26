# Lifecycle Policy Hooks

**Priority:** High
**Effort:** Medium
**Inspired by:** Squad's file guards, reviewer lockout, and hook pipeline

## Problem

Today mato relies mostly on prompt instructions, Docker isolation, and the review
agent to keep work safe. That is a good baseline, but it does not provide a
deterministic policy layer for rules such as:

- never modify `vendor/` or generated code
- never commit secrets or credentials
- require extra scrutiny for production config changes
- run project-specific validation before merge

These policies are often organizational, repetitive, and better enforced by the host
than by the model.

## Idea

Add host-enforced lifecycle policy hooks that run at well-defined boundaries in the
task pipeline: before task execution, after the agent exits, and before merge.

Unlike Squad's pre-tool hooks, mato should keep this deterministic and host-driven.
The goal is not to intercept every shell command inside the container. The goal is to
validate inputs, outputs, and merge eligibility at the places mato already controls.

## How It Would Work

### Configuration

```yaml
# .mato.yaml
policies:
  blocked_paths:
    - vendor/**
    - "**/*.pem"
    - "**/*.key"
  protected_paths:
    - config/production/**
  secret_scan: true
  max_changed_files: 30

hooks:
  post_task:
    - "scripts/check-generated-files.sh"
  pre_merge:
    - "go test -count=1 ./..."
```

### Lifecycle Boundaries

#### Pre-Task

Run deterministic checks before launching an agent, for example:

- reject tasks whose `affects` match blocked paths
- require `requires_human` for protected areas
- validate that required tools or environment are available

#### Post-Task

After the agent exits, inspect the task branch diff and commit contents:

- changed-file allowlist/blocklist checks
- secret or credential scanning
- max-files-changed threshold
- generated-file drift detection

A failed post-task policy returns the task to `backlog/` with structured feedback,
similar to a review rejection.

#### Pre-Merge

Before squash-merging, run host-side checks such as:

- command-based validation hooks
- protected-path approval requirements
- branch metadata validation
- optional review lockout rules for certain categories of rework

## Example Outcomes

- A task that edits `config/production/` is moved to `waiting-for-human/` instead of
  merging automatically.
- A task that adds `credentials.json` is rejected before review or merge.
- A repo-specific `pre_merge` hook prevents merge until a required verification step
  succeeds.

## Design Considerations

- Start with built-in deterministic guards before adding arbitrary custom hooks.
- Hook boundaries should align to mato's existing state machine so failures are easy
  to explain and recover from.
- Feedback should be written into task metadata so retries have concrete policy
  guidance instead of a generic failure.
- Hooks run on the host, not inside the task container, so their behavior is auditable
  and independent of model compliance.
- Custom hooks should be explicitly opt-in because they increase portability and
  security complexity.
- This should complement the review agent, not replace it. Policy checks are for
  hard rules; review is for judgment.

## Relationship to Existing Features

- Extends `human-team-members.md`: protected-path policies could route tasks into a
  manual gate.
- Complements `context-layers.md`: context layers tell agents what to do and what the
  repo has already learned, while policies let the host enforce what must not pass.
- Fits naturally with `observability-telemetry.md` and `orchestration-log.md`, which
  could record hook timings and policy failures.
