---
id: optimize-build-index-skip-completed
priority: 37
affects:
  - internal/queue/index.go
  - internal/queue/index_test.go
tags: [performance]
estimated_complexity: medium
---
# Optimize BuildIndex to skip completed tasks when not needed

`BuildIndex` in `internal/queue/index.go` (lines 105-134) scans all 7 queue
directories and reads every `.md` file on each poll iteration (default: every
10 seconds). For a queue with hundreds of completed tasks, the `completed/`
directory dominates I/O cost but its contents only matter for dependency
resolution (checking if a dependency is satisfied).

The poll loop calls `BuildIndex` twice per cycle (lines 390-397 in
`runner.go`): once before reconciliation and once after. The second build
could potentially be avoided or made incremental.

## Steps to fix

1. Add a `BuildIndexOpts` struct with a `SkipCompleted bool` field.
   Callers that only need active-task information (overlap detection,
   manifest writing, claiming) can set this flag.

2. Modify `BuildIndex` to accept `*BuildIndexOpts` (nil = scan everything,
   for backward compatibility). When `SkipCompleted` is true, skip the
   `completed/` directory scan entirely and populate the completed ID set
   from a cached snapshot.

3. After `ReconcileReadyQueue`, only rebuild the index if tasks were
   actually promoted (the function already returns a bool indicating this).
   Use `SkipCompleted` for the rebuild since completed tasks haven't changed.

4. Add benchmarks:
   - `BenchmarkBuildIndex` with 10, 100, and 500 completed tasks.
   - Compare with and without `SkipCompleted`.

5. Add tests:
   - `BuildIndex` with `SkipCompleted` still correctly reports active tasks.
   - Dependency resolution still works when completed tasks are cached.

6. Run `go test -race ./internal/queue/...` to verify.
