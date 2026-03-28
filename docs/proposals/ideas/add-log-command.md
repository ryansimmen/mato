# Add mato log command for chronological task history

`mato status` and `mato inspect` are snapshot-oriented: they explain the current
queue and the current state of one task. There is still no command that shows a
cross-task, chronological history of what happened over time.

Users currently have to manually inspect `messages/completions/`, `failed/`, and
task-file markers to answer simple questions such as:

- what merged most recently?
- which task failed last night?
- what was the latest review rejection across the queue?

## Scope

This proposal is for **task history**, not scheduler-decision logging.

Phase 1 should focus on durable task outcomes that already exist in the codebase:

- merged/completed events from `messages/completions/`
- failed events from task-file failure markers
- review rejection events from task-file review-rejection markers

Non-goals for phase 1:

- per-poll-cycle host decision logging
- explaining why a task was skipped in a specific cycle
- retry events unless mato gains a first-class durable retry event source

## Steps to fix

1. Add a `log` subcommand to `cmd/mato/main.go`:
   ```
   mato log [--repo <path>] [--limit N] [--format text|json]
   ```

2. Gather task-history events from durable sources:
   - completion details from `messages/completions/` (merge timestamp,
     commit SHA, changed files)
   - failure markers from task files in `failed/`
   - review rejection markers from task files that record them

3. Normalize the gathered records into one event shape and sort newest first.

4. Render text output like:
   ```
   2026-03-23 10:15:00  MERGED    add-retry-logic   abc1234  (2 files changed)
   2026-03-23 10:10:00  FAILED    broken-task       agent interrupted
   2026-03-23 10:05:00  REJECTED  fix-login-bug     missing test coverage
   2026-03-23 10:00:00  MERGED    update-readme     def5678  (1 file changed)
   ```

5. Default limit: 20 events. `--limit 0` means unlimited.

6. JSON format should output an array of event objects with common fields such as
   `timestamp`, `type`, and `task_id`, plus type-specific fields like commit SHA
   or reason.

7. Add tests:
   - log with a mix of merged, failed, and rejected events
   - log with `--limit`
   - log with `--format json`
   - log with no events

8. Update `README.md` and any command reference docs.

9. Run `go build ./... && go test -count=1 ./...` to verify.

## Follow-up

If the state-machine formalization proposal later adds a transition journal,
`mato log` can optionally expand from terminal outcomes into richer task
transition history. That should be a follow-up, not a requirement for phase 1.
