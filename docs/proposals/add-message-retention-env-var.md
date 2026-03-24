---
id: add-message-retention-env-var
priority: 21
affects:
  - internal/runner/runner.go
  - internal/runner/runner_test.go
  - docs/configuration.md
tags: [feature, configuration]
estimated_complexity: simple
---
# Make message retention period configurable

`messaging.CleanOldMessages(tasksDir, 24*time.Hour)` in
`internal/runner/runner.go` (line 380) uses a hardcoded 24-hour retention
window. Users debugging issues from the previous day may find messages
already deleted, while long-running queues may accumulate unnecessary
messages.

## Steps to fix

1. Add `MATO_MESSAGE_RETENTION` environment variable:
   - Parse as a Go duration string (e.g. `48h`, `168h` for 7 days).
   - Validate: reject non-positive values with a clear error.
   - Default to `24h` when unset.

2. Read the env var during host initialization (near other env var reads
   in `runner.go`) and pass the resolved duration to `CleanOldMessages`.

3. Add tests:
   - Valid duration parsing (e.g. `48h`, `7h30m`).
   - Invalid duration rejection (e.g. `-1h`, `abc`).
   - Default value when env var is unset.

4. Update `docs/configuration.md` to add `MATO_MESSAGE_RETENTION` to the
   environment variables table.

5. Run `go build ./... && go test ./internal/runner/...` to verify.
