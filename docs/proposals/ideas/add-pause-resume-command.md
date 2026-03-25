---
id: add-pause-resume-command
priority: 45
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/runner/runner.go
  - internal/runner/runner_test.go
tags: [feature, cli]
estimated_complexity: medium
---
# Add mato pause and resume commands

There is no way to pause the orchestrator without killing the process. A
paused state would stop claiming new tasks while letting in-progress work
complete normally. This is useful for maintenance, debugging, or manual
intervention.

## Steps to fix

1. Add `pause` and `resume` subcommands to `cmd/mato/main.go`:
   - `mato pause [--repo <path>]` — creates a `.mato/.paused` sentinel file
     containing a timestamp and optional reason.
   - `mato resume [--repo <path>]` — removes the `.mato/.paused` sentinel
     file.

2. In the poll loop (`runner.go`), check for the `.paused` file before
   calling `SelectAndClaimTask`. If paused:
   - Skip task claiming.
   - Skip review selection.
   - Continue running merge processing (let approved work complete).
   - Print a periodic message: `"[mato] paused — run 'mato resume' to continue"`.

3. Update `mato status` to show paused state:
   ```
   Status: PAUSED (since 2026-03-23T10:00:00Z)
   ```

4. Add the `mato doctor` check: warn if `.paused` file exists and is
   older than 24 hours (likely forgotten).

5. Add tests:
   - Pause creates sentinel file.
   - Resume removes sentinel file.
   - Resume when not paused is a no-op (idempotent).
   - Poll loop skips claiming when paused.
   - Status shows paused state.

6. Update `README.md` and `docs/configuration.md`.

7. Run `go build ./... && go test -count=1 ./...` to verify.
