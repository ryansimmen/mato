---
id: add-configurable-poll-interval
priority: 18
affects:
  - internal/runner/runner.go
  - internal/runner/runner_test.go
  - docs/configuration.md
tags: [feature, configuration]
estimated_complexity: medium
---
# Add configurable poll interval via environment variable

The poll interval in `internal/runner/runner.go` (lines 42-52) is hardcoded:
`basePollInterval = 10s`, `maxPollInterval = 5m`, `errBackoffThreshold = 5`.
Users on fast machines with small queues may want a shorter poll (1-2s);
users on slow CI runners or with large queues may want a longer poll (30s+).
There is no way to tune this without editing source code.

## Steps to fix

1. Add `MATO_POLL_INTERVAL` environment variable support:
   - Parse as a Go duration string (e.g. `2s`, `30s`, `1m`).
   - Validate: reject non-positive values with a clear error.
   - Use as the new `basePollInterval` if set; otherwise keep `10s`.

2. Add `MATO_MAX_POLL_INTERVAL` environment variable:
   - Parse as a Go duration string.
   - Validate: must be >= base poll interval.
   - Use as the new `maxPollInterval` if set; otherwise keep `5m`.

3. Print the effective poll interval at startup so users can confirm their
   configuration took effect.

4. Add tests:
   - Valid duration parsing.
   - Invalid/negative duration rejection.
   - Max < base rejection.
   - Default values when env vars are unset.

5. Update `docs/configuration.md` to document both new env vars in the
   environment variables table.

6. Run `go build ./... && go test ./internal/runner/...` to verify.
