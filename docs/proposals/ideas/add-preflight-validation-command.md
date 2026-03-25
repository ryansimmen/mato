---
id: add-preflight-validation-command
priority: 26
affects:
  - cmd/mato/main.go
  - cmd/mato/main_test.go
  - internal/runner/runner.go
  - internal/doctor/
  - README.md
tags: [feature, cli, validation]
estimated_complexity: medium
---
# Add pre-flight validation for tasks and queue state

Mato has validation logic spread across `--dry-run`, `mato doctor`,
frontmatter parsing, and dependency diagnostics, but there is no dedicated
pre-flight validation command focused on catching queue/task problems before
an operator starts a mutating run. That makes common authoring and queue
integrity issues harder to check intentionally.

## Steps to fix

1. Add a `validate` subcommand to `cmd/mato/main.go`:
   ```
   mato validate [--repo <path>] [--format text|json]
   ```

2. Make the command read-only and focused on pre-flight checks for the queue:
   - task frontmatter parse errors
   - invalid `affects` glob syntax and unsafe entries
   - duplicate IDs and ambiguous completed-ID matches
   - unknown dependencies and cycles
   - backlog / waiting state issues relevant before a real run starts

3. Reuse existing internals instead of creating a parallel validation stack:
   - `BuildIndex` and its recorded warnings/failures
   - dependency diagnostics used by `mato doctor`
   - queue manifest / reconcile-related validation already surfaced in
     `runner.DryRun`

4. Clarify how `validate` differs from existing commands:
   - unlike `mato doctor`, it is centered on task/queue correctness rather
     than host environment checks like Docker, git, or tool availability
   - unlike `--dry-run`, it does not pretend to start the orchestrator or
     print runtime queue previews unrelated to validation results

5. Text output should summarize findings by severity, for example:
   ```
   mato validate: 2 errors, 1 warning
   - ERROR backlog/broken-task.md: invalid affects glob "internal/**/"
   - ERROR waiting/cycle-a.md: circular dependency involving cycle-b
   - WARN  completed/legacy-copy.md: ambiguous dependency alias for id=setup
   ```

6. JSON output should expose structured findings with fields like `severity`,
   `code`, `state`, `filename`, and `message`.

7. Add tests:
   - clean queue returns success
   - parse error / invalid glob / unknown dependency / cycle cases
   - JSON output shape
   - exit status behavior for warnings vs. errors

8. Update `README.md`, `docs/architecture.md`, and `docs/configuration.md`
   with guidance on when to use `validate` versus `doctor` and `--dry-run`.

9. Run `go build ./... && go test -count=1 ./...` to verify.
