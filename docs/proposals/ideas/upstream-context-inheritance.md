# Upstream Context Inheritance

**Priority:** Medium
**Effort:** Medium
**Inspired by:** Squad's upstream inheritance and team portability

## Problem

Teams using mato across multiple repositories often want to share the same operating
context:

- organization-wide directives
- reusable skills
- common decision records
- starter task templates

Today the only option is copying files between repositories. That creates drift and
makes it hard to roll out improvements consistently.

## Idea

Allow a repository to inherit read-only mato context from one or more upstream
sources. Upstreams provide shared guidance, while the local repository keeps its own
queue, task state, and overrides.

This is different from task-pack export/import. Task packs share work items; upstream
inheritance shares operating context.

## How It Would Work

### Configuration

```yaml
# .mato.yaml
upstreams:
  - name: org-defaults
    type: git
    source: git@github.com:acme/mato-defaults.git
    ref: main
    include: [directives, skills, decisions]

  - name: platform-team
    type: local
    path: ../platform-mato
    include: [skills]
```

### Imported Content

An upstream can contribute selected context layers such as:

- `directives.md`
- `.mato/skills/*.md`
- `decisions.md`
- task templates for `mato new` or future planning commands

### Merge Rules

At runtime, mato resolves upstreams in order and builds the effective context:

1. upstream defaults
2. later upstreams override earlier ones where relevant
3. local repository files override all upstream content

The task queue itself is never inherited. Only guidance and templates flow across
repositories.

### Sync Model

To keep host execution predictable, upstreams should be synchronized explicitly:

```bash
mato upstream sync
```

That command updates cached upstream content under `.mato/upstreams/` and records the
resolved revision so agent runs do not depend on live network access.

## Design Considerations

- Keep inheritance read-only. Local agents should not write back to upstream sources.
- Pin git upstreams to a commit or ref recorded in local state for reproducibility.
- Avoid inheriting queue state, lockfiles, or operational logs.
- Prefer explicit sync over implicit background fetches.
- The merged prompt context should clearly label which content came from upstreams vs.
  the local repo so debugging stays understandable.
- Conflict handling should be simple: local wins, and duplicate upstream entries are
  resolved by configured order.

## Relationship to Existing Features

- Extends `context-layers.md` from a single-repo system into reusable
  organization-wide context.
- Complements `task-pack-export.md`, which is about portable task sets rather than
  shared operational guidance.
- Pairs well with `add-new-task-command.md` and `interactive-task-authoring.md` if
  upstreams later provide shared templates or planning defaults.
