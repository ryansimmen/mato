---
id: add-log-command
priority: 27
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
tags: [feature, cli, ux]
estimated_complexity: medium
---
# Add mato log command to view task history

There is no way to view a chronological history of task completions,
failures, and retries. Users must manually inspect `completed/` and
`failed/` directories and read HTML comment metadata from markdown files.

## Steps to fix

1. Add a `log` subcommand to `cmd/mato/main.go`:
   ```
   mato log [--repo <path>] [--limit N] [--format text|json]
   ```

2. Gather events from multiple sources:
   - Completion details from `messages/completions/` (merge timestamp,
     commit SHA, changed files).
   - Failure records from task files in `failed/` (failure timestamps
     and reasons).
   - Review rejection records from task files (rejection timestamps
     and reasons).

3. Sort all events by timestamp (newest first) and display:
   ```
   2026-03-23 10:15:00  MERGED   add-retry-logic     abc1234  (2 files changed)
   2026-03-23 10:10:00  FAILED   broken-task          attempt 3/3: agent was interrupted
   2026-03-23 10:05:00  REJECTED fix-login-bug        review: missing test coverage
   2026-03-23 10:00:00  MERGED   update-readme        def5678  (1 file changed)
   ```

4. Default limit: 20 events. `--limit 0` for unlimited.

5. JSON format should output an array of event objects with `timestamp`,
   `type`, `task_id`, and type-specific fields.

6. Add tests:
   - Log with mix of completed and failed tasks.
   - Log with --limit.
   - Log with --format json.
   - Log with empty queue (no events).

7. Update `README.md` and `docs/configuration.md`.

8. Run `go build ./... && go test -count=1 ./...` to verify.
