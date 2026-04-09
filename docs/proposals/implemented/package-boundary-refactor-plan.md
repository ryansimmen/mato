# Package Boundary Refactor Plan

> **Status: Implemented** — This proposal has been fully implemented.
> **Read-only:** This file is a historical snapshot. Update the source code and living docs instead.
> The text below describes the original design; see the source code for
> the current implementation.

This proposal describes a focused architectural cleanup of mato's internal
package boundaries. The goal is not to reduce package count for its own sake.
The goal is to make dependencies flow downward more consistently, remove thin
glue packages, eliminate duplicated helpers, and create clearer seams around
the broadest packages before they become harder to change.

## Status

- Phase 1 is complete: built-in defaults now live in `internal/config/`,
  `configresolve` no longer imports `runner` or `queue`, and `cmd/mato/`
  performs the `RunConfig` to `runner.RunOptions` conversion.
- Phase 2 is complete: `internal/runtimedata/` now owns runtime sidecar state,
  `internal/taskstate/`, `internal/sessionmeta/`, and
  `internal/runtimecleanup/` have been removed, and the existing on-disk
  runtime layout was preserved.
- Phase 3 is complete: task-file listing now lives in `internal/taskfile/`,
  `internal/queue/taskfiles.go` and `internal/queue/dirs.go` have been
  removed, and fence detection intentionally remains duplicated in
  `internal/frontmatter/` and `internal/taskfile/`.
- Phase 4 is complete: the queue read model now lives in `internal/queueview/`,
  read-only consumers use it directly where appropriate, and `internal/queue/`
  retains mutation/orchestration behavior with a smaller compatibility layer.
- The remaining active work in this proposal is Phase 5 `runner`
  reassessment.

## 1. Goals

- Remove upward dependencies from low-level packages into orchestration code.
- Collapse runtime-state packages that currently split one cohesive concern.
- Eliminate duplicated helper logic and redundant package re-exports.
- Prepare `runner` and `queue` for eventual narrower boundaries without forcing
  a large all-at-once rewrite.
- Keep behavior stable while improving package ownership and testability.

## 2. Non-goals

- No feature changes.
- No queue-state or CLI behavior redesign.
- No package churn just to reduce the number of directories.
- No speculative extraction of tiny helpers into new utility packages.
- No broad rename campaign unless a specific boundary change requires it.
- No `timeutil` package changes in this proposal.

## 3. Problem Summary

### 3.1 Phase 1 resolved: `configresolve` no longer depends upward on orchestration packages

`internal/configresolve/` previously imported both `internal/runner/` and
`internal/queue/` to obtain built-in defaults, and it exposed a helper that
returned `runner.RunOptions`.

Phase 1 resolved this by moving built-in defaults into `internal/config/` and
moving the `RunConfig` to `runner.RunOptions` conversion into `cmd/mato/`.

This was the clearest dependency-direction problem in the design:

- config resolution should be reusable by commands and operators
- config resolution should not depend on the runtime orchestration layer
- default values should not be owned only by packages that sit above the
  resolver in the dependency graph

### 3.2 Phase 2 resolved: runtime sidecar state is no longer split across three packages

`internal/taskstate/` and `internal/sessionmeta/` previously managed JSON files
under `.mato/runtime/` with very similar CRUD and sweep behavior. The separate
`internal/runtimecleanup/` package existed mainly to coordinate deletion across
them.

Phase 2 resolved this by consolidating that ownership into
`internal/runtimedata/` while preserving the existing on-disk runtime layout.

### 3.3 Phase 3 resolved: helper ownership cleanup removed the remaining low-value shims

Phase 3 resolved the two main low-value boundary leaks:

- task-file listing was consolidated under `internal/taskfile/ListTaskFiles`
- queue directory constants and the ordered directory list are now owned only
  by `internal/dirs/`

Fence-line detection remains duplicated in `internal/frontmatter/` and
`internal/taskfile/` by design. The proposal explicitly preferred keeping that
small helper duplicated over forcing a shared home that might complicate
dependencies.

### 3.4 `runner` and `queue` are broad packages

`internal/runner/` currently mixes:

- top-level run lifecycle
- Docker and host-tool validation/discovery
- environment assembly
- task execution
- review execution
- dry-run rendering support

`internal/queue/` currently mixes:

- queue indexing and query views
- dependency diagnostics
- overlap/runnable analysis
- claiming and reconciliation
- orphan recovery and movement helpers
- lock registration and runtime cleanup hooks

Neither package is unworkable today, but both already have visible internal
subdomains.

## 4. Decisions

### 4.1 Built-in config defaults live in `internal/config`

Built-in configuration defaults now live in `internal/config/`, alongside the
`Config` schema they describe.

Responsibilities:

- default Docker image
- default task model
- default review model
- default reasoning effort
- default agent timeout
- default retry cooldown
- default branch name

After Phase 1:

- `configresolve` depends on `config` and validation helpers
- `runner` consumes resolved config and may still reference `config` defaults
  where local fallback behavior is needed
- `queue` uses `config` as the owner of retry-cooldown default ownership if it
  still needs that constant

The important outcome is that `configresolve` stops importing `runner` and
`queue`.

### 4.2 `configresolve` is independent of `runner`

`configresolve` no longer returns `runner.RunOptions` directly.

Instead, `configresolve` owns its own resolved config model and the conversion
seam lives in the command layer.

Recommended direction:

- keep `RunConfig` in `configresolve`
- remove direct import of `runner`
- convert resolved config into `runner.RunOptions` in `cmd/mato/`

This keeps the resolver reusable and prevents configuration code from depending
on the main runtime package just for a result type.

### 4.3 `taskstate`, `sessionmeta`, and `runtimecleanup` are merged

A single runtime-state package now owns the host-managed sidecar files under
`.mato/runtime/`.

Suggested package name:

```text
internal/runtimedata/
```

Keep separate files and types inside the package:

- `taskstate.go`
- `session.go`
- `cleanup.go`

Exported surface:

- task state load/update/delete/sweep helpers
- session metadata load/update/delete/sweep helpers
- session kind constants (`work`, `review`) so callers do not need local string
  duplication
- session-ID reset helpers used when a stale Copilot session must be rotated
- cleanup helpers that remove one or both runtime record types
- best-effort cleanup wrappers that also call into `taskfile` for verdict-file
  deletion when a terminal transition needs both operations

This merged one cohesive concern while preserving conceptual clarity inside the
package.

### 4.4 Keep `process` as a separate leaf package

Do not merge `process` into `lockfile`.

Reasoning:

- `process` already has production consumers outside `lockfile`
- `doctor` should not need to import a locking package just to perform process
  liveness checks
- `process` is a healthy leaf package with no internal dependencies

Its small size is not a problem. It is small because it is focused.

### 4.5 Do not merge `identity` into `lockfile` yet

Do not take this merge in the first refactor wave.

Reasoning:

- `identity` is the agent-naming and liveness layer used by `queue`, `runner`,
  and `doctor` independently of lock semantics
- lock-file metadata and acquisition are generic filesystem locking concerns
- agent ID generation is not a locking concern
- current `identity` size is small, but the boundary is still understandable

This can be revisited later if agent-liveness logic grows and the split stops
pulling its weight. It is not a priority change.

### 4.6 Deduplicate helper ownership without creating new micro-packages

Make these targeted changes:

- keep one task-file listing helper and reuse it from callers that need sorted
  `.md` task names
- remove queue's re-exported `Dir*` constants and ordered directory list, and
  import `internal/dirs` directly at call sites
- only deduplicate fence detection if there is a natural home that does not
  create a new dependency cycle or a new package solely for one helper

The important rule here is to prefer simpler ownership, not more utility
packages.

### 4.7 Extract the `queue` read model after the low-risk cleanup phases

`queue` is the strongest real split candidate in the codebase.

The emerging boundary is:

- read/query side
  - index building
  - dependency diagnostics
  - runnable backlog views
  - overlap analysis
  - dependency resolution helpers
- mutation side
  - claiming
  - reconciliation
  - recovery
  - atomic moves and queue transitions
  - locks and manifests

This split is attractive because the dependency direction is already mostly
one-way: mutation logic consumes the read model, while the read model does not
need the heavier queue-mutation dependencies.

If extracted, the likely direction is:

```text
internal/queue/      // mutations and transitions
internal/queueview/  // read-only index, diagnostics, runnable views
```

Before implementation, define the exported move/stay boundary explicitly rather
than inferring it from filenames alone.

Expected `queueview` ownership:

- `TaskSnapshot`
- `ParseFailure`
- `BuildWarning`
- `PollIndex`
- `BuildIndex`
- task-resolution helpers such as `ResolveTask`
- dependency and runnable-view types such as `DependencyBlock`,
  `RunnableBacklogView`, and `DeferralInfo`
- dependency diagnostics, overlap analysis, runnable backlog calculation, and
  other read-only query APIs

Expected `queue` ownership:

- claim / reconcile / cancel / retry mutations
- recovery and queue transitions
- atomic move helpers and mutation-only filesystem operations
- lock and manifest writes that are part of mutation paths

`internal/dirs` should continue owning queue directory names and the ordered
directory list.

Do not take this extraction first. First remove the simpler dependency smells
and redundant re-exports, then extract the read model once the remaining seam is
clear.

### 4.8 Do not split `runner` yet

`runner` is broad, but the current file layout already reflects its main
subdomains reasonably well:

- orchestration in `runner.go` and `runner_poll.go`
- host and Docker setup in `config.go` and `tools.go`
- task and review execution in `task.go` and `review.go`

The main pressure on `runner` today comes from `configresolve` importing upward
into it for defaults and `RunOptions`. Once that coupling is removed, the case
for an immediate package split becomes much weaker.

Do not create a new single-consumer package such as `runtimeenv` or
`runnerhost` in the first refactor wave. The Docker and env logic is currently a
private implementation detail of `runner`, not shared infrastructure.

### 4.9 Keep any future `runner` extraction as a deferred option

Treat `runner` as having three subdomains for documentation and internal
organization purposes:

- orchestration and poll loop
- host environment and Docker/tool setup
- task/review execution

If `runner` grows significantly after the config decoupling lands, revisit a
package split later. It is not a current priority.

## 5. Implementation Order

Use this sequence to minimize churn and keep behavior changes low-risk.

### Phase 1: Fix the clearest dependency smell (completed)

1. Move built-in config defaults from `runner` and `queue` into
   `internal/config`.
2. Update `configresolve` to consume `config` instead of `runner` and `queue`
   for defaults.
3. Remove `configresolve`'s direct dependency on `runner.RunOptions` by moving
   the conversion into the caller.
4. Keep command and runtime behavior unchanged while updating tests around
   config resolution.

### Phase 2: Collapse runtime-sidecar ownership (completed)

5. Add `internal/runtimedata/`.
6. Move task-state logic into `runtimedata`.
7. Move session-metadata logic into `runtimedata`.
8. Move cleanup helpers into `runtimedata` and delete
   `internal/runtimecleanup/`.
9. Preserve the current session-kind constants and session-reset behavior while
   moving APIs into `runtimedata`.
10. Update imports in `queue`, `runner`, `merge`, integration tests, and any
    other runtime-state consumers.
11. Verify sweep and cleanup behavior stays identical.

### Phase 3: Remove duplicated helper ownership and redundant re-exports (completed)

12. Consolidate task-file listing into one helper.
13. Update queue and taskfile callers to use the single helper.
14. Remove `internal/queue/dirs.go` re-exports. These compatibility shims still
    exist after Phase 2 and are one of the main remaining transitional
    artifacts.
15. Update call sites to use `internal/dirs` directly for both `Dir*`
    constants and the ordered directory list.
16. Remove `internal/queue/taskfiles.go` by consolidating callers onto the
    shared task-file listing helper.
17. Decide whether fence-line detection should remain duplicated or be moved to
    a shared existing package after checking for dependency-cycle risk.

### Phase 4: Extract the `queue` read model (completed)

18. Create `internal/queueview/` for read-only queue index and diagnostic APIs.
19. Move explicitly named read-model types and functions into `queueview`:
    `TaskSnapshot`, `ParseFailure`, `BuildWarning`, `PollIndex`, `BuildIndex`,
    `ResolveTask`, runnable backlog views, overlap helpers, dependency-view
    types, and diagnostics.
20. Keep mutation APIs in `queue`: claim, reconcile, cancel, retry, recovery,
    transitions, and mutation-only lock/manifest logic.
21. Update mutation code in `queue` to depend on `queueview`.
22. Update read-only consumers such as `status`, `doctor`, `graph`, `inspect`,
    `history`, `merge`, `runner`, and any command-layer callers to use
    `queueview` directly where appropriate.
23. Keep `configresolve` out of scope for this phase; its queue dependency
    should already be removed in Phase 1.

### Phase 5: Reassess `runner`, but defer package extraction by default

24. Keep `runner` as one package unless post-refactor growth or coupling makes a
    real split clearly worthwhile.

## 6. Detailed Plan

### 6.1 Phase 1: `configresolve` decoupling

#### Move defaults into `internal/config/`

This landed in `internal/config/`, which already owns the config schema and
loading logic.

Suggested API:

```go
package config

const (
    DefaultBranch          = "mato"
    DefaultDockerImage     = "ubuntu:24.04"
    DefaultTaskModel       = "claude-opus-4.6"
    DefaultReviewModel     = "gpt-5.4"
    DefaultReasoningEffort = "high"
)

const (
    DefaultAgentTimeout  = 30 * time.Minute
    DefaultRetryCooldown = 2 * time.Minute
)
```

The current code no longer needs `configresolve` to import `runner` or `queue`
for defaults. `runner` and `queue` may still reference `config` defaults where
they remain the local owner of fallback behavior.

#### `configresolve` migration

This landed in `internal/configresolve/`:

- imports `config` instead of `runner` and `queue` for defaults
- resolves values into its own `RunConfig`
- no longer needs `runner.RunOptions` in package scope

Recommended API shape:

```go
type RunConfig struct {
    DockerImage                Resolved[string]
    TaskModel                  Resolved[string]
    ReviewModel                Resolved[string]
    ReviewSessionResumeEnabled Resolved[bool]
    TaskReasoningEffort        Resolved[string]
    ReviewReasoningEffort      Resolved[string]
    AgentTimeout               Resolved[time.Duration]
    RetryCooldown              Resolved[time.Duration]
}
```

`cmd/mato/` now contains the converter that builds `runner.RunOptions`.

Do not keep the conversion method inside `configresolve` if it requires the
resolver to import `runner`.

#### Validation path

`internal/configresolve/validate.go` now uses the same lower-level default
package so validation does not keep the old upward imports alive.

### 6.2 Phase 2: runtime-state merge

This landed in `internal/runtimedata/`, which now owns functionality from:

- `internal/taskstate/`
- `internal/sessionmeta/`
- `internal/runtimecleanup/`

Suggested exported types:

```go
type TaskState struct { ... }
type Session struct { ... }
```

Suggested exported functions:

```go
func LoadTaskState(tasksDir, taskFilename string) (*TaskState, error)
func UpdateTaskState(tasksDir, taskFilename string, fn func(*TaskState)) error
func DeleteTaskState(tasksDir, taskFilename string) error
func SweepTaskState(tasksDir string) error

const (
    KindWork   = "work"
    KindReview = "review"
)

func LoadSession(tasksDir, kind, taskFilename string) (*Session, error)
func LoadOrCreateSession(tasksDir, kind, taskFilename, taskBranch string) (*Session, error)
func ResetSessionID(tasksDir, kind, taskFilename, taskBranch string) (*Session, error)
func UpdateSession(tasksDir, kind, taskFilename string, fn func(*Session)) error
func DeleteSession(tasksDir, kind, taskFilename string) error
func DeleteAllSessions(tasksDir, taskFilename string) error
func SweepSessions(tasksDir string) error

func DeleteRuntimeArtifacts(tasksDir, filename string)
func DeleteRuntimeArtifactsPreservingVerdict(tasksDir, filename string)
```

These names are intentionally explicit so one package can hold both runtime-data
concerns without creating ambiguous `Load` and `Delete` APIs.

The runtime behavior remained unchanged while package ownership moved:

- keep the current on-disk paths under `.mato/runtime/taskstate/` and
  `.mato/runtime/sessionmeta/`
- keep the session-kind contract stable for `runner`, tests, and integration
  flows
- preserve session reset semantics for stale-resume recovery

Boundary note:

- task state and session metadata live under `.mato/runtime/`, with their
  existing `taskstate/` and `sessionmeta/` subdirectories preserved
- preserved review verdict files live under `.mato/messages/` and still belong
  to `taskfile`
- `runtimedata` importing `taskfile` for terminal-transition verdict cleanup is
  acceptable and expected because that remains a downward coordination
  dependency

The cleanup wrappers in `runtimedata` should be understood as coordination
logic that delegates verdict deletion to `taskfile`, not as proof that verdict
files moved into the runtime-data boundary.

Migration guidance:

- move code first, behavior unchanged
- update call sites
- delete old packages only after imports are clean
- preserve current on-disk paths under `.mato/runtime/taskstate/` and
  `.mato/runtime/sessionmeta/`

### 6.3 Phase 3: helper deduplication

#### Task-file listing

Keep one exported helper for reading sorted `.md` task filenames.

Preferred owner:

```text
internal/taskfile/
```

Reasoning:

- the helper is task-file-specific, not queue-state-specific
- `queue` already depends on `taskfile`
- moving it into `dirs` would give `dirs` filesystem traversal responsibility it
  does not currently own

Suggested API:

```go
func ListTaskFiles(dir string) ([]string, error)
```

Before deleting `internal/queue/taskfiles.go`, verify that the queue and
taskfile implementations have equivalent sorting and error-handling behavior.
If there is any mismatch, preserve current queue behavior in the unified
`taskfile.ListTaskFiles` implementation before migrating callers.

Then:

- delete `internal/queue/taskfiles.go`
- update queue callers to use `taskfile.ListTaskFiles`

#### Queue directory constants

Delete `internal/queue/dirs.go` and update all call sites to use `internal/dirs`
directly.

This includes both the `queue.Dir*` constants and the ordered directory list
currently exposed as `queue.AllDirs`.

Preferred ownership:

```text
internal/dirs.All
```

This removes redundant API surface and makes ownership explicit.

#### Fence-line detection

Do not force a shared helper if it complicates dependencies.

Acceptable options:

1. keep the duplicate helper and add tests that ensure both packages interpret
   fences consistently
2. if dependency-safe, move the helper to one existing package that both users
   already import without introducing a cycle

Option 1 is preferable if the only motivation is deduping ten lines of code.

### 6.4 Phase 4: extracted the `queue` read model

#### `queue`

A read-only sibling package now owns the queue read model.

Suggested package:

```text
internal/queueview/
```

Read/query responsibilities now live there:

- query files
  - `index.go`
  - `diagnostics.go`
  - `overlap.go`
  - `runnable_backlog.go`
  - `resolve.go`
- mutation files
  - `claim.go`
  - `reconcile.go`
  - `cancel.go`
  - `retry.go`
  - `queue.go`

Landed end state:

```text
internal/queue/       // mutations and transitions
internal/queueview/   // read-only index, diagnostics, runnable views
```

Why this split was worth taking:

- `TaskSnapshot` and `PollIndex` are primarily read-model types
- many consumers across the codebase read queue state without mutating it
- the read model has a lighter dependency set than queue mutation code
- dependency direction already mostly flows from mutation code into the read
  model, not the reverse

Migration note:

The read-model seam was real but not perfectly clean. Several helper
types and functions were interleaved across queue files during the extraction:

- `ensureIndex` lives in `index.go`
- `dependencyLookup` lives in `runnable_backlog.go`
- `DeferralInfo` lives in `overlap.go`
- `immediatelyClaimableTask` lives in `claim.go`

Read-model consumers include:

- `runner`
- `status`
- `doctor`
- `graph`
- `inspect`
- `history`
- `merge`
- command-layer callers
- tests

Phase 4 therefore required moving or duplicating these small supporting
helpers so `queueview` could become a true read-only package without depending
back on queue mutation code. The extraction was not purely a file move.

Outcome note:

- Phase 1 is ready immediately.
- Phase 2 is ready once the runtime-session API surface above is preserved.
- Phase 3 is ready once `AllDirs` ownership is migrated alongside `Dir*`
  constants.
- Phase 4 landed after the explicit move/stay surface was agreed and
  documented.

After Phase 3, `queueview` imports `taskfile` for `ListTaskFiles` rather
than reintroducing a queue-specific directory-scanning helper.

#### `runner`

Do not extract a new package now. Keep the current file-level organization:

- orchestration
  - `runner.go`
  - `runner_poll.go`
  - `runner_dryrun.go`
- host env / docker / tools
  - `config.go`
  - `tools.go`
  - `runner_support.go`
- execution
  - `task.go`
  - `review.go`
  - `runner_exec.go`

If a later split ever becomes necessary, the likely direction is:

```text
internal/runner/   // poll loop and lifecycle orchestration
internal/worker/   // task and review execution logic
```

Do not take this extraction as part of this proposal.

## 7. File Changes by Phase

### Phase 1

Status: completed.

Modify:

```text
internal/config/config.go
internal/config/config_test.go
internal/configresolve/configresolve.go
internal/configresolve/validate.go
internal/configresolve/configresolve_test.go
cmd/mato/*.go (callers that build runner options)
internal/runner/*.go (only if forwarding constants remain temporarily)
internal/queue/*.go (only if forwarding constants remain temporarily)
```

### Phase 2

Status: completed.

Create:

```text
internal/runtimedata/taskstate.go
internal/runtimedata/session.go
internal/runtimedata/cleanup.go
internal/runtimedata/taskstate_test.go
internal/runtimedata/session_test.go
internal/runtimedata/cleanup_test.go
```

Modify:

```text
internal/runner/*.go
internal/merge/*.go
internal/queue/*.go
internal/integration/*.go
```

Also migrate the existing `internal/taskstate/*_test.go` and
`internal/sessionmeta/*_test.go` coverage into `internal/runtimedata/*_test.go`.

Deleted after migration:

```text
internal/taskstate/
internal/sessionmeta/
internal/runtimecleanup/
```

### Phase 3

Status: completed.

Modify:

```text
cmd/mato/*.go
internal/doctor/*.go
internal/dirs/*.go
internal/frontmatter/*.go (only if fence helper is consolidated)
internal/graph/*.go
internal/history/*.go
internal/inspect/*.go
internal/integration/*.go
internal/merge/*.go
internal/messaging/*.go
internal/taskfile/*.go
internal/queue/*.go
internal/runner/*.go
internal/status/*.go
```

Delete:

```text
internal/queue/taskfiles.go
internal/queue/dirs.go
```

### Phase 4

Status: completed.

Create:

```text
internal/queueview/*.go
internal/queueview/*_test.go
```

Modify:

```text
cmd/mato/*.go
internal/integration/*.go
internal/queue/*.go
internal/dirs/*.go (only if queueview migration needs supporting ownership cleanup)
internal/status/*.go
internal/doctor/*.go
internal/graph/*.go
internal/history/*.go
internal/inspect/*.go
internal/merge/*.go
internal/runner/*.go
```

### Phase 5

No new package extraction by default. Only revisit `runner` if later changes
show that the package boundary is genuinely blocking maintainability.

## 8. Testing Strategy

### Phase 1

- verify default values still resolve exactly as before
- verify config validation still reports the same errors and sources
- verify command-layer conversion to `runner.RunOptions` preserves behavior
- verify no resolver code still imports `runner` or `queue` for defaults

### Phase 2

- keep current task-state and session tests by porting them to the new package
- add cleanup tests that cover verdict-preserving and verdict-deleting paths
- add sweep tests to ensure runtime files are removed only for non-active tasks

### Phase 3

- verify queue and taskfile callers both use the shared task-file listing helper
- verify direct `dirs` imports do not change queue behavior
- if fence detection remains duplicated, add behavior tests that keep the two
  implementations aligned

### Phase 4

- verify `queueview` owns the read-model types and helpers cleanly
- verify queue mutation paths still use the same effective runnable and
  diagnostic logic through the new read-model package
- verify command-layer callers that build indexes or resolve tasks now use the
  new read-model package directly where appropriate
- verify read-only consumers (`status`, `doctor`, `graph`, `inspect`, `merge`,
  `history`, `runner`) continue to produce the same results

### Phase 5

- run full build, vet, and test verification after each phase
- avoid combining broad package extraction with behavior changes in the same PR

## 9. Risks and Mitigations

| Risk | Mitigation |
|------|-----------|
| Config behavior drifts while decoupling `configresolve` | Move defaults first, then migrate imports, then update conversion seams with targeted tests. |
| Runtime-state merge creates confusing APIs | Use explicit names like `LoadTaskState` and `LoadSession` inside the merged package. |
| Helper dedup creates dependency cycles | Prefer leaving tiny helpers duplicated over introducing a bad shared package. |
| Direct `dirs` imports create large diff churn | Remove queue re-exports in a focused PR with mechanical updates only. |
| `queueview` extraction causes widespread import churn | Land it only after config and runtime-state cleanup reduce unrelated moving parts. |
| Prematurely splitting `runner` increases churn without real payoff | Keep `runner` as one package unless later growth proves otherwise. |

## 10. Recommended PR Breakdown

The only remaining work in this proposal is optional later reassessment of
`runner` if future growth justifies it.

1. optional later reassessment of `runner`, with no package split by default

Phases 1, 2, 3, and 4 already landed separately.

## 11. Acceptance Criteria

This plan is complete when:

- `configresolve` no longer imports `runner` or `queue`
- runtime sidecar state is owned by one package and `runtimecleanup` is gone
- queue/taskfile helper duplication is removed where it is clearly worthwhile
- queue directory constants are owned only by `internal/dirs`
- the queue read model lives in a lighter package separate from queue mutation
  code
- preserved verdict files remain owned by `taskfile` even if cleanup wrappers in
  `runtimedata` coordinate deleting them during terminal transitions
- `runner` remains intentionally unsplit unless future growth justifies changing
  that decision

## 12. Verification

After each phase, run:

```text
go build ./...
go vet ./...
go test -count=1 ./...
```
