# Interactive Task Authoring

**Priority:** Low
**Effort:** Medium
**Inspired by:** Squad's interactive shell and natural language decomposition

## Problem

Writing task files with correct frontmatter, dependency chains, and `affects`
metadata is manual and error-prone. Users need to know the task format spec,
understand the dependency graph, and predict which files will be affected.
For larger features that decompose into many tasks, this is tedious.

## Idea

Add a `mato plan` command that uses an LLM to interactively decompose a feature
description into a set of task files with proper dependencies, priorities, and
`affects` metadata.

## How It Would Work

### Basic Usage

```bash
# Interactive: describe what you want to build
mato plan "Add user authentication with OAuth2 support"

# From a file (e.g. a PRD or feature spec)
mato plan --from feature-spec.md

# Dry-run: show tasks without writing them
mato plan --dry-run "Add rate limiting to the API"
```

### Flow

1. User provides a feature description (CLI argument or file).
2. `mato plan` launches a Copilot agent with a planning prompt that:
   - Reads the codebase structure
   - Analyzes the feature requirements
   - Proposes a set of task files with frontmatter
3. The agent writes task files to a staging area (e.g. `.mato/planned/`).
4. The user reviews the proposed tasks.
5. On confirmation, tasks are moved to `waiting/` or `backlog/` as appropriate.

### Example Output

```
Planning: "Add user authentication with OAuth2 support"

Proposed tasks:
  1. setup-auth-models.md        (priority: 10, no deps)
     affects: internal/models/user.go, internal/models/session.go

  2. add-oauth-provider.md       (priority: 20, depends: setup-auth-models)
     affects: internal/auth/oauth.go, internal/auth/provider.go

  3. add-auth-middleware.md       (priority: 20, depends: setup-auth-models)
     affects: internal/middleware/auth.go

  4. add-login-endpoints.md       (priority: 30, depends: add-oauth-provider, add-auth-middleware)
     affects: internal/api/auth.go, internal/api/routes.go

  5. add-auth-tests.md            (priority: 40, depends: add-login-endpoints)
     affects: internal/api/auth_test.go, internal/auth/oauth_test.go

Write these tasks? [y/n/edit]
```

## Design Considerations

- The planning agent uses the same Docker/Copilot infrastructure as task agents,
  but with a different embedded prompt focused on task decomposition.
- The planning prompt should reference `docs/task-format.md` for correct
  frontmatter generation.
- Consider a `--template` flag for common patterns (e.g. `--template crud`
  generates standard CRUD task sets).
- The `affects` field is the hardest to get right automatically. The planning
  agent should scan the codebase structure to make informed guesses, but users
  should review and adjust.
- This is essentially what the existing mato SKILL.md does for coding agents.
  `mato plan` would formalize it as a CLI command.

## Relationship to Existing Features

- Uses the existing Docker agent infrastructure from `internal/runner/`.
- Generated task files use the standard format from `docs/task-format.md`.
- The dependency graph can be validated with `dag.Analyze` before writing.
- `mato doctor --only tasks,deps` can validate the output post-generation.
- Builds on the mato skill (`.github/skills/mato/SKILL.md`) which already
  defines task planning conventions.
