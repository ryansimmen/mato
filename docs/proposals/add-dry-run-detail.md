---
id: add-dry-run-detail
priority: 25
affects:
  - internal/runner/runner.go
  - internal/runner/runner_test.go
tags: [ux, feature]
estimated_complexity: medium
---
# Enhance dry-run output with execution plan detail

The `DryRun` function in `internal/runner/runner.go` (line 74) prints queue
counts and deferred tasks, but does not show the dependency graph, task
execution order, or the full overlap analysis. Users run `--dry-run` to
understand what will happen, but the current output lacks the detail needed
for confident decision-making.

## Steps to fix

1. Extend `DryRun` to print additional sections:

   - **Execution order**: List tasks in the order they would be claimed
     (priority-sorted, after overlap deferral), numbered.
   - **Dependency graph summary**: For each waiting task, show its
     dependencies and their current state (completed, waiting, failed,
     unknown).
   - **Overlap conflicts**: For each deferred task, explain which active
     or higher-priority task it conflicts with and on which `affects`
     entries.
   - **Task frontmatter summary**: For each backlog task, show id,
     priority, affects, and depends_on in a compact format.

2. Reuse existing functions:
   - `DeferredOverlappingTasks` already returns the deferred set.
   - `DiagnoseDependencies` provides the dependency analysis.
   - `PollIndex` has all the task metadata.

3. Add tests for the enhanced output: verify that each new section
   appears when there are relevant tasks.

4. Run `go test ./internal/runner/...` to verify.
