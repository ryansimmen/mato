---
id: add-task-inspect-command
priority: 24
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/status/
  - README.md
tags: [feature, cli, ux]
estimated_complexity: medium
---
# Add mato inspect command for narrow task explain/root-cause output

Mato already tracks rich task state across queue directories, status data,
dependency analysis, and task-file metadata comments. But there is no
single command that answers a narrow operator question like: "Why is this
task blocked, deferred, failed, or not running next?" Users currently have
to piece the answer together from `mato status`, `mato graph`, and the task
file itself.

## Steps to fix

1. Add an `inspect` subcommand to `cmd/mato/main.go`:
   ```
   mato inspect <task-id-or-filename> [--repo <path>] [--format text|json]
   ```

2. Scope the command narrowly around root-cause explanation for one task:
   - resolve the target by explicit `id`, filename stem, or full filename
   - report the task's current state directory
   - explain whether it is blocked by dependencies, deferred by `affects`
     overlap, waiting for review, waiting to merge, or failed/rejected
   - include the next actionable reason and, when possible, what would
     unblock it
   - avoid turning the command into a general log browser or dashboard

3. Reuse existing queue/status data instead of inventing a new data model:
   - `PollIndex` task snapshots
   - status gathering for deferred details, reverse deps, and active progress
   - dependency diagnostics for waiting-task blockage reasons
   - task metadata comments for failure/review-rejection context

4. Text output should be concise and causal, for example:
   ```
   Task: add-retry-logic
   State: backlog
   Status: deferred
   Reason: overlaps with in-progress task fix-http-client on pkg/client/http.go
   Next step: wait for fix-http-client to leave active states
   ```

5. JSON output should expose the same facts in machine-readable form, such as
   `task_id`, `filename`, `state`, `status`, `reason`, `blocking_task`,
   `blocking_dependencies`, and `next_step`.

6. Add tests:
   - inspect a waiting task blocked by dependencies
   - inspect a backlog task deferred by overlapping `affects`
   - inspect a task in `ready-for-review` / `ready-to-merge`
   - inspect a failed or review-rejected task
   - inspect unknown task ID / ambiguous match behavior

7. Update `README.md` and `docs/architecture.md` to document the new
   troubleshooting command and its narrow scope.

8. Run `go build ./... && go test -count=1 ./...` to verify.
