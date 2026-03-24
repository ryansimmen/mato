---
id: implement-tag-based-filtering
priority: 46
affects:
  - internal/frontmatter/frontmatter.go
  - internal/queue/manifest.go
  - internal/queue/manifest_test.go
  - internal/status/status_gather.go
  - internal/status/status_render.go
  - cmd/mato/main.go
tags: [feature]
estimated_complexity: medium
---
# Implement tag-based filtering for tasks

The `tags` field in `TaskMeta` (`internal/frontmatter/frontmatter.go`,
line 26) is parsed from YAML frontmatter but never used by any internal
code. Users may set tags expecting filtering or grouping behavior, but
nothing happens. The field is documented in `docs/task-format.md` with no
mention that it is unused internally.

## Steps to fix

1. Add `--tags` flag to the root `mato` command:
   ```
   mato --tags backend,urgent
   ```
   When set, only tasks whose `tags` field contains at least one of the
   specified tags are eligible for claiming. Tasks without matching tags
   are treated as deferred (stay in backlog, excluded from `.queue`).

2. Pass the tag filter through to `SelectAndClaimTask` and
   `WriteQueueManifest`.

3. Add `--tags` filter to `mato status`:
   ```
   mato status --tags backend
   ```
   Show only tasks matching the filter in the queue overview.

4. Display tags in status output when present:
   ```
   backlog:          3
     add-retry-logic    [backend, reliability]
     fix-login-bug      [bug, urgent]
   ```

5. Add tests:
   - Claiming with tag filter (only matching tasks claimed).
   - Manifest with tag filter (non-matching tasks excluded).
   - Status with tag filter.
   - No filter (all tasks eligible, backward compatible).

6. Update `docs/task-format.md` to document the tag filtering behavior.

7. Run `go build ./... && go test -count=1 ./...` to verify.
