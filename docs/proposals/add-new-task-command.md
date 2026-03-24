---
id: add-new-task-command
priority: 41
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
tags: [feature, cli, ux]
estimated_complexity: medium
---
# Add mato new command to scaffold task files

Creating a new task requires manually writing a markdown file with correct
YAML frontmatter. This is error-prone: typos in field names (e.g.
`depend_on` instead of `depends_on`) fail silently, and users must remember
the full frontmatter schema.

## Steps to fix

1. Add a `new` subcommand to `cmd/mato/main.go`:
   ```
   mato new <task-name> [--priority N] [--depends-on a,b] [--affects x,y] [--tags t1,t2] [--waiting]
   ```

2. Generate a correctly-formatted task file:
   ```markdown
   ---
   id: <task-name>
   priority: <N, default 50>
   depends_on: [a, b]       # only if --depends-on provided
   affects: [x, y]          # only if --affects provided
   tags: [t1, t2]           # only if --tags provided
   estimated_complexity: medium
   ---
   # <Task Name (kebab-case converted to title case)>

   <!-- Describe what needs to change and why -->

   ## Steps to fix

   1. <!-- Step 1 -->
   2. <!-- Step 2 -->
   ```

3. Place the file in `backlog/` by default, or `waiting/` if `--waiting`
   is passed or if `--depends-on` is provided.

4. Validate:
   - Task name must be valid kebab-case (lowercase, hyphens, no special chars).
   - Priority must be a positive integer.
   - `depends_on` task IDs should be non-empty strings.
   - `affects` paths should not contain `..` (path traversal).
   - File must not already exist in any queue directory.

5. Print the path of the created file and open instruction.

6. Add tests:
   - Basic task creation with defaults.
   - Task creation with all flags.
   - Duplicate task name rejection.
   - Invalid task name rejection.
   - Auto-placement in waiting when depends_on is set.

7. Update `README.md` and `docs/configuration.md`.

8. Run `go build ./... && go test -count=1 ./...` to verify.
