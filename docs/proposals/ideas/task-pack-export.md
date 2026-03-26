# Task Pack Export/Import

**Priority:** Medium
**Effort:** Medium
**Inspired by:** Squad's export/import and team portability features

## Problem

When setting up mato on a new project or a similar project, users start from
scratch. Common task patterns (e.g. "set up CI", "add linting", "create test
infrastructure") are written repeatedly across projects. There's no way to
share or reuse task sets.

## Idea

Add `mato export` and `mato import` commands that package task files (with
frontmatter and dependency graphs) into portable archives that can be applied
to other projects.

## How It Would Work

### Export

```bash
# Export all backlog and waiting tasks
mato export --out my-tasks.json

# Export specific tasks
mato export --tasks "setup-ci,add-linting,create-test-infra" --out starter-pack.json

# Export completed tasks as templates (strip runtime metadata)
mato export --completed --out patterns.json
```

The export file is a JSON archive containing:
- Task file contents (body + frontmatter)
- Dependency graph (which tasks depend on which)
- Source project metadata (for reference, not applied on import)

### Import

```bash
# Import tasks into current project
mato import starter-pack.json

# Import with prefix to avoid collisions
mato import starter-pack.json --prefix "infra-"

# Dry-run to preview
mato import starter-pack.json --dry-run
```

Import creates task files in `waiting/` (if they have dependencies) or
`backlog/` (if they don't), stripping any runtime metadata (failure records,
branch markers, claimed-by headers).

### Example Export Format

```json
{
  "version": 1,
  "exported_at": "2026-03-20T10:00:00Z",
  "source": "github.com/org/repo",
  "tasks": [
    {
      "filename": "setup-ci.md",
      "frontmatter": {
        "id": "setup-ci",
        "priority": 10,
        "depends_on": [],
        "affects": [".github/workflows/"],
        "tags": ["infra"]
      },
      "body": "# Set up CI\n\nCreate a GitHub Actions workflow..."
    },
    {
      "filename": "add-linting.md",
      "frontmatter": {
        "id": "add-linting",
        "priority": 20,
        "depends_on": ["setup-ci"],
        "affects": [".github/workflows/", ".golangci.yml"],
        "tags": ["infra", "quality"]
      },
      "body": "# Add linting\n\nAdd golangci-lint to the CI pipeline..."
    }
  ]
}
```

## Design Considerations

- Export should strip all runtime metadata (failure records, branch markers,
  claimed-by headers, merged markers) to produce clean templates.
- Import should validate the dependency graph is consistent within the imported
  set (no references to tasks not in the import or the existing queue).
- Filename collision handling: skip, overwrite, or prefix.
- Consider community sharing: a registry or GitHub repo of task packs for
  common project setups.
- Keep the format simple and human-readable (JSON, not a binary archive).

## Relationship to Existing Features

- Uses existing `frontmatter.ParseTaskFile` for export and existing task file
  writing conventions for import.
- Dependency validation can reuse `dag.Analyze` to check the imported graph.
- Works with `mato doctor` for post-import validation.
