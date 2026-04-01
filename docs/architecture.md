# Mato Architecture
This document describes the architecture implemented by `cmd/mato/main.go` and the packages in `internal/`: `runner/`, `setup/`, `queue/`, `dag/`, `git/`, `merge/`, `frontmatter/`, `messaging/`, `status/`, `history/`, `inspect/`, `doctor/`, `graph/`, `identity/`, `lockfile/`, `process/`, `atomicwrite/`, `taskfile/`, `config/`, `dirs/`, `pause/`, `sessionmeta/`, `taskstate/`, and `runtimecleanup/`.
## 1. System Overview
`mato` is a filesystem-backed task orchestrator for Copilot agents. The host process watches `.mato/`, promotes tasks whose dependencies are satisfied, defers overlapping tasks, selects and claims a task, launches one agent run at a time in an ephemeral Docker container, runs an AI review pass for completed work, and squash-merges approved task branches back into a target branch. An optional `.mato/.paused` sentinel can temporarily stop new claims and review launches without stopping the host process. The agent container handles exactly one pre-claimed task lifecycle: verify the claim, work in an isolated clone on the host-selected task branch, commit its changes, and exit. The host then pushes the task branch, moves the task to `ready-for-review/`, processes the review verdict, and later merges approved work.
```text
+-------------------------+      +-----------------------------+
| CLI: mato               |      | CLI: mato status            |
| main.go -> runner.Run() |      | main.go -> status.Show()    |
+------------+------------+      +-----------------------------+
             |
             v
+-------------------------------+
| Host loop in runner.go        |
| manages .mato/, Docker, Git  |
+---------------+---------------+
                |
                v
       +-------------------+
       | .mato/ queue     |
       | waiting/backlog/  |
       | in-progress/...   |
       +----+---------+----+
            |         |
            | claim   | merge
            v         ^
+------------------+  |
| Docker agent     |  |
| temp clone       |  |
| task/<name>      +--+
+------------------+     ready-to-merge -> completed
```
High-level flow:
1. `main.go` parses flags, loads optional repo-local `.mato.yaml`, resolves precedence across CLI flags, env vars, config file, and hardcoded defaults, then either starts `runner.Run(...)` via `mato run`, bootstraps a repository via `setup.InitRepo(...)`, or routes to read-only subcommands such as `status.Show(...)`, `history.Show(...)`, `inspect.Show(...)`, and `graph.Show(...)`.
2. `runner.Run(...)` receives fully resolved `RunOptions`, creates/maintains the queue, writes `.queue`, and starts agent runs. `RunOptions.Mode` selects between the default daemon loop, one-iteration bounded execution (`--once`), and drain-until-idle bounded execution (`--until-idle`).
3. The host selects and claims a task via `queue.SelectAndClaimTask(...)`, chooses a stable task branch (reusing any recorded branch marker when safe, otherwise deriving a sanitized/disambiguated branch), then launches the agent with pre-resolved task info as env vars (`MATO_TASK_FILE`, `MATO_TASK_BRANCH`, `MATO_TASK_TITLE`, `MATO_TASK_PATH`). The agent prompt in `task-instructions.md` verifies the claim, works on the preselected branch, and commits locally.
4. After the agent exits, the host pushes the task branch and moves the task file to `ready-for-review/`.
5. A review agent evaluates the pushed branch. Approved tasks advance to `ready-to-merge/`; rejected tasks return to `backlog/` with recorded feedback.
6. The host merge queue squashes approved task branches into the target branch and moves the task file to `completed/`.
## 2. Host Loop
### Startup
`runner.Run(repoRoot, branch, opts)` performs host initialization in this order:
1. Resolve `repoRoot` with `git rev-parse --show-toplevel`.
2. Ensure the target branch exists with `git.EnsureBranch(...)`; if the local branch exists, check it out; otherwise query the live `origin` branch first. If `origin/<branch>` exists, fetch it and create the local branch from the refreshed remote-tracking ref. If the remote is reachable and the branch is absent there, create the branch from `HEAD` and ignore any stale cached `origin/<branch>` ref. If `origin` is unavailable, fall back to a cached `origin/<branch>` ref when present; otherwise create from `HEAD`.
3. Resolve `tasksDir` as `<repoRoot>/.mato`.
4. Create queue directories: `waiting/`, `backlog/`, `in-progress/`, `ready-for-review/`, `ready-to-merge/`, `completed/`, and `failed/`.
5. Create messaging directories with `messaging.Init(...)`: `.mato/messages/events/`, `.mato/messages/completions/`, and `.mato/messages/presence/`.
6. Generate an agent ID with `identity.GenerateAgentID()`.
7. Register the process as active by writing `.mato/.locks/<agentID>.pid` via `queue.RegisterAgent(...)`.
8. Ensure `/.mato/` is in `.gitignore` via `git.EnsureGitignoreContains(...)`, then commit with `git.CommitGitignore(...)` only if the file was modified.
9. Resolve host tools and runtime dependencies: `copilot`, `git`, `git-upload-pack`, `git-receive-pack`, `gh`, `GOROOT`, optional `~/.config/gh`, optional `/etc/ssl/certs`, and Git author/committer identity. Docker image, task model, review model, task reasoning effort, review reasoning effort, agent timeout, and retry cooldown are already resolved in `cmd/mato/main.go`.
10. Build the embedded prompts by replacing placeholders in `task-instructions.md` and `review-instructions.md` with `/workspace/.mato`, the configured target branch, and `/workspace/.mato/messages`. Review launches also interpolate a host-written review-context block describing the task branch, current branch tip, prior reviewed tip (if known), and prior rejection reason (if known).
11. Prepare durable Copilot session metadata under `.mato/runtime/sessionmeta/`. Work runs always load or create a `work-<task>.json` record and pass `copilot --resume=<session-id>`. Review runs do the same for `review-<task>.json` when `review_session_resume_enabled` is true (default true, configurable via `.mato.yaml` or `MATO_REVIEW_SESSION_RESUME_ENABLED=false`). If Copilot rejects a stored resume ID, `mato` resets that phase's session record and retries once with a fresh generated session ID.
12. Install `SIGINT`/`SIGTERM` handlers.

### Explicit initialization path
`mato init` uses `setup.InitRepo(repoRoot, branch)` for lightweight repository bootstrap without Docker. This path intentionally stays separate from `runner.Run(...)` so the runtime Docker gate and existing runner ordering do not change. `setup.InitRepo(...)` composes shared helpers such as `git.EnsureBranch(...)`, `git.EnsureIdentity(...)`, `git.EnsureGitignoreContains(...)`, and `messaging.Init(...)` to:

1. Resolve the canonical queue directory as `<repoRoot>/.mato`.
2. Create or switch to the target branch.
3. Create queue directories, `.mato/.locks/`, and messaging subdirectories.
4. Ensure local git identity exists.
5. Update `.gitignore` with `/.mato/`.
6. Create a bootstrap commit on empty repositories so the target branch becomes a born branch.

Integration coverage verifies that running `mato init` followed by `runner.DryRun(...)` produces a fully valid queue layout.
### Polling loop
The default daemon loop in `Run()` polls every 10 seconds. The exact order of a poll iteration is:
```text
queue.RecoverOrphanedTasks(tasksDir)
queue.CleanStaleLocks(tasksDir)
queue.CleanStaleReviewLocks(tasksDir)
messaging.CleanStalePresence(tasksDir)
messaging.CleanOldMessages(tasksDir, 24*time.Hour)
taskstate.Sweep(tasksDir)
sessionmeta.Sweep(tasksDir)

idx := queue.BuildIndex(tasksDir)          // build poll index once
queue.ReconcileReadyQueue(tasksDir, idx)
idx = queue.BuildIndex(tasksDir)           // rebuild after reconcile

view := queue.ComputeRunnableBacklogView(tasksDir, idx)
// apply failedDirExcluded and refresh derived .queue
queue.WriteQueueManifestFromView(tasksDir, failedDirExcluded, idx, view)
if not paused:
    candidates := queue.OrderedRunnableFilenames(view, failedDirExcluded)
    claimed, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, candidates, cooldown, idx)
if FailedDirUnavailableError → add task to failedDirExcluded
if claimed != nil:
    messaging.WriteMessage(...) intent
    runOnce(...)
    recoverStuckTask(...)
if not paused and reviewTask, cleanup := selectAndLockReview(tasksDir, idx); reviewTask != nil:
    runReview(reviewTask, ...)
merge.AcquireLock(tasksDir) + merge.ProcessQueue(...)
if paused: print paused heartbeat
else if no claimed task and no ready-to-merge: print idle message
wait for signal or 10 seconds
```
Important details from the implementation:
- The poll loop builds a `PollIndex` (`queue.BuildIndex(tasksDir)`) at the start of each cycle. The index reads every task file once and provides O(1) lookups for task IDs, active branches, `affects` metadata, and state. All consumers in the cycle share this snapshot instead of independently scanning directories and parsing YAML frontmatter. The index is rebuilt after `ReconcileReadyQueue` since it may promote tasks from `waiting/` to `backlog/`. Functions that accept a `*PollIndex` parameter treat `nil` as "build a temporary index internally", preserving backward compatibility for callers outside the poll loop (e.g., `DryRun`, `status`, integration tests).
- Orphan recovery happens before new work so abandoned `in-progress/` tasks can be retried.
- Queue cleanup is fully filesystem-based; there is no database or daemon.
- `.queue` is written once per iteration, after dependency enforcement and overlap deferral, so the manifest reflects the effective runnable backlog. The manifest is a newline-separated list of task filenames (e.g. `my-task.md`), ordered by priority ascending (lower number = higher priority), then alphabetically by filename. It is written atomically by the host via `WriteQueueManifestFromView()`. Dependency-blocked backlog tasks and conflict-deferred tasks are excluded from the manifest. This refresh still happens while the repo is paused.
- `queue.SelectAndClaimTask(...)` now receives an ordered candidate slice from the caller instead of reading `.queue`. In the polling loop that candidate list is derived from `ComputeRunnableBacklogView()`, so claim order, `mato status`, `mato inspect`, and `.queue` all share the same runnable backlog model. `SelectAndClaimTask(...)` still reparses each candidate from disk before claim-time validation, re-checks `depends_on`, counts `<!-- failure: ... -->` records, applies the resolved retry cooldown, atomically moves the candidate from `backlog/` to `in-progress/`, then checks the failure record budget—moving exhausted tasks to `failed/` (or returning a `FailedDirUnavailableError` if `failed/` is unavailable). Dependency-blocked candidates are moved back to `waiting/` as a safety net if they were edited or requeued after the last reconcile pass. `HasAvailableTasks` is a separate helper but is not called in the polling loop.
- When a `FailedDirUnavailableError` is returned, the host loop adds the task filename to a persistent `failedDirExcluded` set. On each subsequent iteration, that set is applied as an extra exclusion when deriving ordered runnable candidates, preventing the same task (whose failure record budget is exhausted) from being re-selected and livelocking the host loop.
- After claiming, the host writes a best-effort `intent` message, then launches `runOnce(...)`.
- `recoverStuckTask(...)` runs immediately after `runOnce(...)` returns: if the task file is still in `in-progress/`, the agent did not complete its lifecycle, so the host appends a failure record and moves the task back to `backlog/`.
- Merge processing happens after any agent run in the same outer loop.
- Lightweight runtime metadata lives under `.mato/runtime/taskstate/<task-filename>.json`. Work pushes and review launches/completions update this file best-effort so follow-up reviews can carry explicit host-curated context even before durable Copilot session resume exists.
- Durable Copilot resume metadata lives under `.mato/runtime/sessionmeta/work-<task-filename>.json` and `.mato/runtime/sessionmeta/review-<task-filename>.json`. Work and review sessions are intentionally separate. Terminal task transitions delete both taskstate and sessionmeta; `pollCleanup()` also sweeps stale runtime files whose task is no longer active.
- Pause mode is controlled by `.mato/.paused`, which stores the UTC RFC3339 time
  when the repo was paused. While present, the host skips new claims and review
  launches but continues merge processing.
- Bounded run modes reuse the same startup, iteration order, and deferred cleanup. `RunModeOnce` exits after one full `pollIterate(...)` completes. `RunModeUntilIdle` performs an additional post-iteration filesystem check using the same reconcile and claimability rules as normal scheduling, then exits only when there is no immediately claimable backlog work, no remaining review candidates, and no tasks left in `ready-to-merge/`.
- `RunModeUntilIdle` intentionally treats retry-cooldowned backlog tasks as non-actionable, so cooldown alone does not keep the process alive. Retry-exhausted backlog tasks still count as actionable because the next claim pass will move them to `failed/`.
- Pause interacts with bounded idle narrowly: paused queues still are not idle if actionable review or merge work remains, but a paused queue with no actionable backlog, review, or merge work is allowed to exit.
### Orphan recovery and lock cleanup
The `queue` package provides the host-side recovery primitives:
- `queue.RegisterAgent(...)` (in `queue.go`) writes `.mato/.locks/<agentID>.pid` and returns a cleanup function.
- `identity.IsAgentActive(...)` reads a PID file and tests liveness with signal `0`.
- `queue.CleanStaleLocks(...)` removes dead agent lock files.
- `queue.RecoverOrphanedTasks(...)` (in `queue.go`) scans `in-progress/*.md`; if the claiming agent is no longer active, it appends `<!-- failure: mato-recovery ... -->` and renames the task back to `backlog/`.
- If `claimed-by` points at a still-live agent, recovery skips that task.
### Signal handling
`Run()` listens for `SIGINT` and `SIGTERM`. On either signal it prints `Interrupted. Exiting.`, returns `nil`, and the deferred cleanup removes the host lock file.
## 3. Agent Lifecycle
`runOnce(...)` launches one isolated task agent.
### Temporary clone and origin behavior
Before Docker starts, `runOnce(...)`:
1. Creates a temporary local clone with `git.CreateClone(repoRoot)`.
2. Defers `git.RemoveClone(cloneDir)`.
3. Sets `receive.denyCurrentBranch=updateInstead` in the origin repo so the temp clone can push into the checked-out target branch safely.
The design relies on `.mato/` being Git-ignored so queue updates do not dirty the branch being updated via `updateInstead`.
### Docker runtime
The container runs as `docker run --rm --init -it --user <uid>:<gid>` with working directory `/workspace`. The `--init` flag ensures an init process reaps zombie child processes inside the container.
Primary mounts:
| Host | Container | Why |
| --- | --- | --- |
| temp clone | `/workspace` | isolated working tree for the agent |
| `<repoRoot>/.mato` | `/workspace/.mato` | shared queue and message state |
| `repoRoot` | same absolute host path | keeps the clone's local-path `origin` reachable |
| `copilot` | `/usr/local/bin/copilot` (ro) | Copilot CLI inside container |
| `git` | `/usr/local/bin/git` (ro) | Git inside container |
| `git-upload-pack` | `/usr/local/bin/git-upload-pack` (ro) | local-path fetch support |
| `git-receive-pack` | `/usr/local/bin/git-receive-pack` (ro) | local-path push support |
| `gh` | `/usr/local/bin/gh` (ro) | GitHub CLI |
| host `GOROOT` | `/usr/local/go` (ro) | Go toolchain |
| host `~/.copilot` | `$HOME/.copilot` | Copilot auth/package state |
| host `~/.cache/copilot` | `$HOME/.cache/copilot` | Copilot cache data |
| host `~/go/pkg/mod` | `$HOME/go/pkg/mod` | Go module cache |
| host `~/.cache/go-build` | `$HOME/.cache/go-build` | Go build cache |
| host `~/.config/gh` | `$HOME/.config/gh` (ro, optional) | `gh` config |
| host git-templates dir | same absolute host path (ro, optional) | Git hooks and templates |
| host `/etc/ssl/certs` | `/etc/ssl/certs` (ro, optional) | CA trust |
Environment variables injected by the host:
- `GOROOT=/usr/local/go`
- `PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
- `MATO_AGENT_ID=<generated id>`
- `MATO_MESSAGING_ENABLED=1`
- `MATO_MESSAGES_DIR=/workspace/.mato/messages`
- `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=safe.directory`, `GIT_CONFIG_VALUE_0=*`
- `GIT_AUTHOR_NAME` / `GIT_COMMITTER_NAME` if host Git config supplies a name
- `GIT_AUTHOR_EMAIL` / `GIT_COMMITTER_EMAIL` if host Git config supplies an email
- `HOME=<host home path>`
- `GOPATH=<home>/go`, `GOMODCACHE=<home>/go/pkg/mod`, `GOCACHE=<home>/.cache/go-build`
- `MATO_DEPENDENCY_CONTEXT=<file path>` (conditionally, only when the task has `depends_on` entries with available completion data; points to `/workspace/.mato/messages/dependency-context-<filename>.json`)
The final command is:
```text
copilot [--resume=<session-id>] -p <embedded task prompt> --autopilot --allow-all --model <run.model> --reasoning-effort <run.reasoningEffort>
```
`buildDockerArgs(...)` always appends `--model <run.model>` and `--reasoning-effort <run.reasoningEffort>` using the fully resolved values from `RunOptions`, and appends `--resume=<session-id>` only when the host has a durable session record for that phase.
### Host-side task claiming
Before launching a Docker container, the host derives ordered candidates from `ComputeRunnableBacklogView()` and calls `queue.SelectAndClaimTask(tasksDir, agentID, candidates, cooldown, idx)`, which:
1. Iterates the caller-provided candidate filenames in order.
2. Reparses each candidate from disk and re-checks `depends_on` against the current completed/non-completed snapshot.
3. If a candidate is dependency-blocked, moves it from `backlog/` back to `waiting/` and continues.
4. Counts `<!-- failure: ... -->` records and applies retry cooldown.
5. Atomically renames the candidate from `backlog/` to `in-progress/`.
6. If the number of `<!-- failure: ... -->` records >= `max_retries` (default 3), moves the task from `in-progress/` to `failed/` and continues to the next candidate. If `failed/` is unavailable, the task is rolled back to `backlog/` and a `FailedDirUnavailableError` (carrying the task filename) is returned; the host loop adds this task to a persistent exclusion set to prevent livelocking on the same retry-exhausted task.
7. Prepends a `<!-- claimed-by: ... -->` header to the task file.
8. Returns the filename, derived branch name, extracted title, and host-side path.

The host then writes one best-effort `intent` message via `messaging.WriteMessage(...)` and passes the claimed task info to the agent as environment variables: `MATO_TASK_FILE`, `MATO_TASK_BRANCH`, `MATO_TASK_TITLE`, `MATO_TASK_PATH`.

### Task state machine from `task-instructions.md`
```text
VERIFY_CLAIM -> WORK -> COMMIT
     \__________________________/
          unrecoverable error -> ON_FAILURE
```
The host creates the task branch before the agent starts, and pushes it after the agent exits.
State-by-state behavior:
- `VERIFY_CLAIM`: read pre-resolved task details from `MATO_TASK_*` env vars, confirm the task file exists in `in-progress/`, read recent coordination messages.
- `WORK`: read the task body, ignoring YAML frontmatter and comment-only metadata lines, implement the task, and run the repository's existing validation commands with up to 3 attempts.
- `COMMIT`: `git add -A`, commit with the task title, and exit. The host detects commits and handles push + review transition.
- `ON_FAILURE`: append a structured `<!-- failure: ... -->` record, try to check out the target branch, then always move the task back to `backlog/`. The host checks the failure record budget on the next cycle via `SelectAndClaimTask`.

After the agent exits, the host (`postAgentPush` in `task.go`) checks for commits on the task branch. If commits exist and the task is still in `in-progress/`, the host pushes the branch with `--force-with-lease`, writes the `<!-- branch: ... -->` marker, moves the task to `ready-for-review/`, updates `.mato/runtime/taskstate/<task-filename>.json` with the pushed branch tip and outcome, updates the `work` session's `last_head_sha`, and sends `conflict-warning` and `completion` messages.

The prompt enforces several invariants: one task per run; agents never push any branches (the host handles all pushes); they send at most 4 messages per task (3 `progress` + 1 for `ON_FAILURE`). The `intent` message is sent by the host before the agent starts.
## 4. Task Queue States
The queue is encoded directly in directories under `.mato/`.
```text
waiting/ --deps met--> backlog/ --host claim--> in-progress/ --host push--> ready-for-review/ --approved--> ready-to-merge/ --merge--> completed/
   ^                      |                          |                              |
   |                      |                          +--ON_FAILURE--> backlog/       +--review rejection--> backlog/
   +--dep not met---------+                          +--host orphan recovery--> backlog/
                          |                          +--host failure record budget exhausted--> failed/
                           +--dependency-blocked tasks are demoted back to waiting/
                           +--conflict-deferred tasks stay in backlog/ but excluded from .queue
```
State meanings:
| State | Meaning | Entered by | Left by |
| --- | --- | --- | --- |
| `waiting/` | dependency-blocked task | authored there initially for dependency tracking, or demoted from `backlog/` when `depends_on` is not satisfied | `queue.ReconcileReadyQueue(...)` moves it to `backlog/` when every dependency is completed and no active overlap exists |
| `backlog/` | runnable task (or conflict-deferred) | initial ready state, dependency promotion, orphan recovery, merge requeue after squash conflicts, `ON_FAILURE` requeue, or review rejection | host `SelectAndClaimTask` moves runnable tasks to `in-progress/`; conflict-deferred tasks remain here but are excluded from `.queue`; dependency-blocked tasks are moved back to `waiting/` |
| `in-progress/` | task owned by one agent | host `SelectAndClaimTask` | host `postAgentPush` (to `ready-for-review/`), `ON_FAILURE`, or host orphan recovery |
| `ready-for-review/` | branch pushed, waiting for AI review | host `postAgentPush` | host `postReviewAction` moves to `ready-to-merge/` on approval, or to `backlog/` on rejection |
| `ready-to-merge/` | reviewed and approved, waiting for host merge | host `postReviewAction` | `merge.ProcessQueue(...)` moves it to `completed/` on success, to `backlog/` on squash conflict or missing task branch (if retries remain), to `failed/` if the task branch is missing and `max_retries` is exhausted, or leaves it in place if the squash commit cannot be pushed |
| `completed/` | merged terminal state | host merge success | no normal exit |
| `failed/` | failure record budget exhausted, operator-cancelled, circular dependency, or duplicate waiting task ID | host `SelectAndClaimTask` moves already-claimed task from `in-progress/` to `failed/` when the number of `<!-- failure: ... -->` records reaches `max_retries` (default 3), `mato cancel` moves queued tasks here with `<!-- cancelled: ... -->` markers, merge-queue failure handling (squash conflict or missing branch with retries exhausted), `ReconcileReadyQueue` moves cycle members from `waiting/` to `failed/` with `<!-- cycle-failure: ... -->` markers, or `ReconcileReadyQueue` moves duplicate waiting task ID copies to `failed/` with `<!-- terminal-failure: ... -->` markers | no normal exit |
Retry counting is comment-based, not directory-based. Task agent failures use `<!-- failure: ... -->` records, counted by `CountFailureLines()` in `SelectAndClaimTask` and by `shouldFailTaskAfterNextFailure()` in the merge queue. Review infrastructure failures use `<!-- review-failure: ... -->` records, counted separately by `CountReviewFailureLines()` in `reviewCandidates()`. This separation ensures that transient review issues (network blips, diff timeouts) do not consume a task's failure record budget, and vice versa.
## 5. Dependency Resolution
`queue.ReconcileReadyQueue(tasksDir, idx)` in `queue/reconcile.go` demotes dependency-blocked backlog tasks to `waiting/`, promotes waiting tasks whose dependencies are satisfied, moves cyclic tasks to `failed/`, and moves duplicate waiting task ID copies to `failed/`.
### DAG Analysis
Dependency resolution is built on a shared analysis pass in `internal/dag/`. `dag.Analyze(nodes, completedIDs, ambiguousIDs, knownIDs)` constructs a dependency graph from the waiting tasks and classifies each node:
- **DepsSatisfied**: all `depends_on` entries are in the completed-ID set (unambiguously).
- **Blocked**: at least one dependency is not yet completed. `BlockDetail` records the reason — `BlockedByWaiting` (dependency is itself in `waiting/`), `BlockedByUnknown` (dependency ID not found in any queue directory), `BlockedByExternal` (dependency exists in a non-completed, non-waiting state such as `failed/` or `in-progress/`), or `BlockedByAmbiguous` (dependency ID maps to multiple completed tasks via stem/explicit-ID aliasing, so it is unsafe to promote). Cycle members do not appear in `Blocked` — they are tracked separately in `Cycles`.
- **Cycles**: strongly connected components with more than one member, or self-edges, detected via Tarjan's SCC algorithm. Only actual cycle members are failed — downstream tasks that depend on a cycle member remain in `Blocked` with `BlockedByWaiting` referencing the cycle member.
The analysis produces deterministic, sorted output for all three categories.
### Diagnostics Wrapper
`queue.DiagnoseDependencies(tasksDir, idx)` in `queue/diagnostics.go` bridges the `PollIndex` and `dag.Analyze()`. It builds the `completedIDs` (safe, unambiguous completions), `ambiguousIDs` (IDs that map to multiple completed tasks via stem/explicit-ID aliasing), and `knownIDs` (IDs present in any queue directory) sets from the index, constructs the node list from waiting tasks, runs `dag.Analyze()`, and produces sorted `DependencyIssue` entries for structured warning output.
### Reconcile Flow
`ReconcileReadyQueue(tasksDir, idx)` calls `DiagnoseDependencies()` once per reconcile cycle, then:
1. Emits structured warnings from `diag.Issues` (duplicate waiting IDs, unknown dependencies, ambiguous completed IDs, cycles).
2. Moves duplicate waiting task ID copies to `failed/` with `<!-- terminal-failure: ... -->` markers. When multiple `waiting/` files share the same task ID, the first file (by filename sort) is retained and every subsequent copy is failed. These markers do **not** consume the task's normal `max_retries` budget.
3. Moves cycle members to `failed/` with `<!-- cycle-failure: mato at <timestamp> — circular dependency -->` markers. These markers do **not** consume the task's normal `max_retries` budget (only `<!-- failure: ... -->` records are counted).
4. Moves dependency-blocked `backlog/` tasks back to `waiting/` before manifest generation or claim selection.
5. Promotes deps-satisfied tasks from `waiting/` to `backlog/` if they also pass the `HasActiveOverlap` filter (no conflicting active tasks).
### One-Hop Promotion Semantics
Promotion is intentionally one-hop per reconcile cycle. If A is completed, B depends on A, and C depends on B, then one reconcile pass promotes B to `backlog/` but leaves C in `waiting/`. C becomes promotable on the next reconcile cycle after B completes. This preserves the existing behavior and avoids premature promotion of deeply chained tasks.
### Cycle-to-Failed Behavior
Previously, cyclic waiting tasks (including self-dependent tasks) remained in `waiting/` indefinitely with only a stderr warning. Now they are moved to `failed/` during reconcile. This is a deliberate improvement: cycles cannot self-resolve, and silent indefinite blocking is worse than explicit failure. Users can fix the cycle by editing the task's `depends_on` and moving it back to `waiting/`.
The cycle-to-failed sequence is idempotent: it checks for an existing `<!-- cycle-failure: -->` marker before appending, then atomically moves the task to `failed/`.
### Dependency Matching
- A dependency is satisfied only if it matches a task already in `completed/`.
- Matching can happen by file stem or explicit `id:` frontmatter.
- Dependencies on tasks that exist elsewhere in `waiting/`, `backlog/`, `in-progress/`, `ready-to-merge/`, or `failed/` but are not completed remain blocked.
- `depends_on` is authoritative regardless of directory placement; `backlog/` does not override unmet dependencies.
- Truly unknown dependency IDs (not found in any queue directory) produce structured warnings.
- Ambiguous completed IDs (where multiple completed tasks share the same ID via stem/explicit-ID aliasing) block promotion with a structured warning rather than silently satisfying the dependency.
Relevant frontmatter defaults from `frontmatter.go`:
- `ID` defaults to the filename stem.
- `Priority` defaults to `50`.
- `MaxRetries` defaults to `3`.
## 6. Merge Queue
`merge.ProcessQueue(repoRoot, tasksDir, branch)` in `merge.go` is the host-side integrator.
### Locking
Before processing the queue, `Run()` calls `merge.AcquireLock(tasksDir)`. The lock file is `.mato/.locks/merge.lock`.
Behavior:
- create with `O_CREATE|O_EXCL`
- write a `"PID:starttime"` identity (via `process.LockIdentity`) into the file to detect PID reuse; on non-Linux systems the start time is unavailable so the value is just `"PID"`
- if the file already exists, read the identity string
- if `process.IsLockHolderAlive(...)` determines the holder is still running (PID alive and start-time matches), skip merging this loop
- if the holder is stale, dead, or the identity is invalid, remove the lock and retry once
This is the main multi-process safety mechanism for host-side merges.
### Task ordering and merge flow
`merge.ProcessQueue(...)` scans `ready-to-merge/*.md`, parses each task, and sorts by ascending `priority`, then filename.
For each task:
1. Parse the task file with `frontmatter.ParseTaskFile(...)`.
2. Derive the task title with `frontmatter.ExtractTitle(...)` from the first non-empty body line; leading `#` is stripped. Build the squash commit message via `formatSquashCommitMessage(task, agentLog)` in `squash.go` (see [Squash commit message format](#squash-commit-message-format) below).
3. Read `<!-- branch: ... -->` from the task file when present; if absent, fall back to `task/<sanitizeBranchName(filename)>`.
4. Create a fresh temp clone.
5. Configure clone identity from repo Git config, then global config, with fallbacks `mato` and `mato@local.invalid`.
6. `git fetch origin`
7. `git checkout -B <target-branch> origin/<target-branch>`
8. Verify `origin/<task-branch>` exists.
9. `git merge --squash origin/<task-branch>`
10. `git commit -m <formatted message with trailers>`
11. `git push origin <target-branch>`
12. After a successful push, write a completion detail file to `.mato/messages/completions/` (using a collision-resistant encoding of the task ID; see `docs/messaging.md` for the filename encoding and full schema) with the commit SHA, changed files, branch, title, and merge timestamp. This file is used by `runner.writeDependencyContextFile(...)` to create the dependency context file referenced by `MATO_DEPENDENCY_CONTEXT` when a dependent task runs.
13. Append `<!-- merged: merge-queue at ... -->` and rename the task file `ready-to-merge/ -> completed/`
### Squash commit message format
`formatSquashCommitMessage(task, agentLog)` builds the squash-merge commit message from the agent's commit and the task metadata. The format is:

1. **Subject line**: the agent's commit subject. If the agent made no commit (or the log is empty), the task title is used as a fallback.
2. **Body** (optional): the agent's commit body, if present. Lines matching `Task: <filename>` and `Changed files:` sections are stripped since that metadata is redundant with the trailers.
3. **Trailers**: appended after a blank separator line:
   - `Task-ID: <id>` — the task's frontmatter `id` field (omitted if no id is set).
   - `Affects: <file1>, <file2>` — comma-separated list from the task's frontmatter `affects` field (omitted if empty).

Example merge commit message:
```
feat: add retry backoff to agent launcher

Implement exponential backoff with jitter when the agent container
fails to start, capped at 60 seconds between attempts.

Task-ID: add-retry-backoff
Affects: internal/runner/runner.go, internal/runner/runner_test.go
```

### Conflict and failure handling
Merge failure handling is branch-specific:
1. Missing task branch: append `<!-- failure: merge-queue ... -->` and move the task to `backlog/` if retries remain, or `failed/` if `max_retries` is exhausted.
2. Squash merge conflict: append `<!-- failure: merge-queue ... -->`, move the task to `backlog/` (or `failed/` if retries exhausted). When the task is requeued to `backlog/`, mato also deletes the stale task branch locally and on `origin` and clears the task file's branch marker so the next work run can start from a fresh branch. Terminal `failed/` tasks keep their branch marker/history.
3. Push failure after a successful squash commit: append `<!-- failure: merge-queue ... -->` but leave the task file in `ready-to-merge/` for retry on the next merge pass.
4. Parse errors are also recorded as merge-queue failures and requeued to `backlog/`.
## 7. Review Agent
After the host pushes a task branch and moves the task to `ready-for-review/`, it launches a review agent to evaluate the changes before merging.

### How it works
1. `selectTaskForReview(tasksDir)` scans `ready-for-review/*.md` for the next task to review.
2. The host verifies the task branch exists (`git rev-parse --verify`). If the branch is missing, the host writes a `<!-- review-failure: ... -->` record and skips the review.
3. `runReview(...)` launches a review agent in the same Docker container model as task agents (ephemeral container, temp clone, identical bind mounts). Before launch, the host resolves the current branch tip, loads any existing `.mato/runtime/taskstate/<task-filename>.json`, reads the most recent review-rejection reason from the task file, renders a `Review context:` block into the embedded review prompt, and, after clone setup succeeds, records a best-effort `review-launched` taskstate update. When review-session resume is enabled, the host also loads or creates a durable `review` session record and passes `copilot --resume=<session-id>` to the review run.
4. The review agent diffs the task branch against the target branch, analyzes the changes, and writes a JSON verdict file to `.mato/messages/verdict-<filename>.json` with `{"verdict":"approve"}`, `{"verdict":"reject","reason":"..."}`, or `{"verdict":"error","reason":"..."}`.
5. After the review agent exits, the host reads the verdict file via `postReviewAction(...)`:
   - **Approved**: host writes `<!-- reviewed: ... -->` to the task file, moves it to `ready-to-merge/`, records `review-approved` in taskstate, and sends a completion message.
   - **Rejected**: host writes `<!-- review-rejection: ... -->` to the task file, moves it back to `backlog/`, records `review-rejected` in taskstate, and sends a completion message.
   - **Error**: host writes a `<!-- review-failure: ... -->` record, records `review-error`, and leaves the task in `ready-for-review/`.
   - **No verdict file** (agent crashed): host falls back to checking for HTML markers in the task file, then writes a `<!-- review-failure: ... -->` record and records `review-incomplete` if none found.

### Key properties
- Review rejections do **not** count against `max_retries`. Only `<!-- failure: ... -->` records are counted for the task's failure record budget.
- Review infrastructure failures (network errors, diff timeouts) are recorded as `<!-- review-failure: ... -->` and counted separately from task failure records. This ensures transient review issues do not exhaust the task's failure record budget.
- **Malformed review candidates** (unparseable frontmatter) are quarantined: `reviewCandidates()` appends a `<!-- terminal-failure: ... -->` marker and moves the task to `failed/`. This prevents a broken task file from blocking review throughput indefinitely. Both the indexed and filesystem fallback code paths follow the same quarantine behavior, consistent with how `ReconcileReadyQueue` handles unparseable `waiting/` and `backlog/` tasks.
- On retry, the host injects previous review feedback via the `MATO_REVIEW_FEEDBACK` environment variable so the implementing agent can address the reviewer's concerns.
- The review agent uses the embedded prompt `review-instructions.md`.
- The review agent writes only a `progress` message; the host sends the `completion` message after processing the verdict.
- The review verdict is communicated via a JSON verdict file. The host writes the HTML comment markers to the task file for state tracking after reading the verdict.

## 8. Conflict Prevention
`queue.DeferredOverlappingTasks(tasksDir, idx)` (in `queue/overlap.go`) prevents multiple backlog tasks from claiming the same files simultaneously.
Algorithm:
1. Collect active tasks (`in-progress/`, `ready-for-review/`, `ready-to-merge/`) and backlog tasks from the `PollIndex` (or by scanning the filesystem when no index is provided).
2. Parse each task's `priority` and `affects`.
3. Sort backlog tasks by ascending priority, then filename.
4. Seed the "kept" set with active tasks (immovable — already being worked on or awaiting merge/review).
5. For each backlog task, compare its `affects` list with every kept task.
6. If there is overlap, add the task to the deferred exclusion set (it stays in `backlog/` but is excluded from `.queue` — agents won't see it).
7. The exclusion set is passed to `WriteQueueManifest` and `SelectAndClaimTask`.
`overlappingAffects(a, b)` (in `overlap.go`) detects conflicts between two affects lists:
- no overlap if either list is empty
- exact strings are compared literally
- an entry ending with `/` is treated as a directory prefix: it matches any entry that starts with that prefix (e.g. `pkg/client/` conflicts with `pkg/client/http.go`). Two prefix entries conflict if one contains the other.
- when no prefix or glob entries are present, a fast-path map lookup is used (O(n+m)); otherwise pairwise comparison is applied
- duplicates are removed from the overlap report
- the overlap list is sorted before logging
- entries containing glob metacharacters (`*`, `?`, `[`, `{`) are matched using `doublestar` pattern syntax. Glob vs concrete path uses `doublestar.Match`; glob vs directory prefix and glob vs glob use static-prefix comparison for conservative conflict detection (no false negatives, possible false positives).
## 9. Code Structure

The codebase follows standard Go project layout: `cmd/mato/` for the CLI entrypoint, `internal/` for library packages.

### `cmd/mato/main.go`
- CLI entrypoint.
- `main()` builds the Cobra command tree for `run`, `status`, `log`, `doctor`, `graph`, `init`, `inspect`, `cancel`, `retry`, `pause`, `resume`, and `version`.
- The root command shows help/version and registers repo-aware subcommands.
- `mato run` resolves config and starts `runner.Run(...)` with fully resolved task/review model and reasoning-effort settings.

### `internal/runner/`
- Embeds `task-instructions.md` (the task agent prompt/state machine) and `review-instructions.md` (the review agent prompt).
- `Run(...)` initializes the repo/queue and executes the polling loop (`runner.go`).
- `runOnce(...)` builds the temp clone + Docker runtime for one task agent run (`runner.go`).
- Docker configuration split into immutable `envConfig` and per-task `runContext` (`config.go`).
- Host tool discovery (`findGitHelper`) in `tools.go`.
- Task lifecycle: `postAgentPush`, `extractFailureLines` (`task.go`).
- Review lifecycle: `selectTaskForReview`, `runReview`, `postReviewAction` (`review.go`).

### `internal/queue/`
- Poll index — `BuildIndex`, `PollIndex`, `TaskSnapshot` (`index.go`).
- Task claiming and failure-record counting (`claim.go`).
- Queue directory constants — re-exports from `internal/dirs` (`dirs.go`).
- Agent registration — `RegisterAgent` (`queue.go`).
- Lock file management — `CleanStaleLocks`, `AcquireReviewLock` (`locks.go`).
- Queue manifest writing (`manifest.go`).
- Overlap deferral — `DeferredOverlappingTasks` (`overlap.go`).
- Dependency promotion — `ReconcileReadyQueue` (`reconcile.go`).
- Dependency diagnostics — `DiagnoseDependencies` (`diagnostics.go`).
- Orphan recovery — `RecoverOrphanedTasks` (`queue.go`).
- Task file enumeration — `ListTaskFiles` (`taskfiles.go`).
- Atomic file moves — `AtomicMove` (`queue.go`).
- Runnable backlog computation — `ComputeRunnableBacklogView` (`runnable_backlog.go`).
- Task reference resolution — `ResolveTask` by filename, stem, or explicit ID (`resolve.go`).
- Task cancellation — `CancelTask` (`cancel.go`).
- Task retry — `RetryTask` (`retry.go`).

### `internal/dag/`
- Shared dependency graph analysis — `Analyze` builds a DAG from waiting tasks, classifies nodes as deps-satisfied, blocked (with reason), or cyclic (`dag.go`).
- Uses Kahn's algorithm for topological ordering and Tarjan's SCC for cycle detection.
- Deterministic sorted output for all categories.

### `internal/identity/`
- Agent ID generation (`GenerateAgentID`) and liveness checks (`IsAgentActive`) via `.locks/*.pid`.

### `internal/git/`
- `Output(...)` wrapper for git commands.
- Temp clone helpers.
- Branch creation/check-out and `.gitignore` maintenance.

### `internal/merge/`
- Ready-to-merge scanning, ordering, and merge lock — `ProcessQueue`, `HasReadyTasks`, `AcquireLock` (`merge.go`).
- Temp-clone squash merges — `mergeReadyTask`, `formatSquashCommitMessage`, branch cleanup (`squash.go`).
- Task lifecycle during merging — retry-budget checks (`shouldFailTask`), failure/success record writing, requeue path (`taskops.go`).

### `internal/process/`
- Shared process identity and liveness helpers used by the lock systems in `internal/queue/` and `internal/merge/`.
- `LockIdentity(pid)` — returns `"PID:starttime"` (or just `"PID"` on non-Linux).
- `IsLockHolderAlive(identity)` — parses an identity string and checks process liveness with start-time verification to detect PID reuse.
- `processStartTime(pid)` — reads `/proc/<pid>/stat` field 22 (unexported).
- `isProcessActive(pid)` — sends signal 0 to check if a PID is alive (unexported).

### `internal/atomicwrite/`
- `WriteFile` — atomically writes `[]byte` to a path via temp-file-then-rename. Fsyncs the temp file before rename and syncs the parent directory afterward for crash durability.
- `WriteFunc` — atomically writes via a caller-supplied callback with the same fsync-before-rename and dir-sync-after-rename guarantees.

### `internal/taskfile/`
- Branch marker parsing — `ParseBranch`, `ParseBranchMarkerLine`, `ParseClaimedBy`, `ParseClaimedAt` (`taskfile.go`, `metadata.go`).
- Failure/review-failure tracking — `CountFailureMarkers`, `ExtractFailureLines`, `AppendFailureRecord`, etc. (`metadata.go`).
- Cycle-failure tracking — `AppendCycleFailureRecord`, `ContainsCycleFailure`, `CountCycleFailureMarkers`, `LastCycleFailureReason` (`metadata.go`). Cycle-failure markers use `<!-- cycle-failure: ... -->` and do not count against the task's `max_retries` budget.
- Active task collection — `CollectActiveAffects` scans in-progress/review/merge directories (`active.go`).

### `internal/frontmatter/`
- `TaskMeta` schema.
- YAML frontmatter parsing via `gopkg.in/yaml.v3` — `ParseTaskFile` (from disk) and `ParseTaskData` (from raw bytes).
- Default metadata values and task-body extraction.
- Strips comment-only HTML metadata lines from the body.
- Branch-name sanitization — `SanitizeBranchName`, `BranchDisambiguator`.

### `internal/messaging/`
- `Message` and `presence` JSON types, plus `CompletionDetail` for merge-time completion data.
- `messaging.Init(...)` creates the event, presence, and completions directories.
- Atomic JSON write helpers.
- Message reading, stale presence cleanup, and old-event garbage collection.
- `WriteCompletionDetail(...)` and `ReadCompletionDetail(...)` for storing and retrieving per-task merge results used by the dependency context flow.

### `internal/status/`
- `mato status` dashboard — compact default text view plus expanded
  `--verbose` text view, via `Show`, `ShowTo`, `ShowVerbose`, `ShowVerboseTo`,
  `Watch`, and `WatchVerbose` (`status.go`).
- Data gathering layer — `gatherStatus` collects queue counts, active agents, presence, task lists, completions, messages, merge-lock state (`status_gather.go`).
- Rendering layer — compact and verbose text renderers plus shared section
  helpers and terminal colors (`status_render.go`).
- JSON output layer — structured `StatusJSON` types and `ShowJSON`/`ShowJSONTo`
  entry points for machine-readable `--json` output (`status_json.go`).

### `internal/history/`
- `mato log` durable outcome timeline — `Show`, `ShowTo` (`history.go`).
- Reads only durable history sources in phase 1: `.mato/messages/completions/*.json`, `<!-- failure: ... -->` task markers, and `<!-- review-rejection: ... -->` task markers.
- Sorts events newest first and renders either compact text or JSON for scripting.

### `internal/inspect/`
- `mato inspect` single-task troubleshooting command — `Show`, `ShowTo` (`inspect.go`).
- Reuses `queue.BuildIndex(...)`, `queue.DiagnoseDependencies(...)`, and `queue.ComputeRunnableBacklogView(...)` so explanations match current scheduler behavior.
- Resolves one task reference across indexed snapshots and parse failures, then renders either compact text or JSON without mutating the queue.

### `internal/doctor/`
- Health check command — `Run`, `RenderText`, `RenderJSON` (`doctor.go`, `checks.go`, `render.go`).
- 8 checks: git, tools, docker, queue layout, task parsing, locks & orphans, hygiene, dependencies.
- Queue-only preflight usage via `mato doctor --only queue,tasks,deps`; queue-only runs skip unrelated Docker-image config resolution in command setup.
- Fix mode for repairable issues — `--fix` flag.

### `internal/graph/`
- `mato graph` visualization command — `Show`, `ShowTo`, `Build` (`graph.go`).
- Reuses `PollIndex` and `DiagnoseDependencies` to build a read-only `GraphData` structure from the task queue.
- Three renderers: human-readable text grouped by state (`render_text.go`), Graphviz DOT with color-coded nodes and edge styles (`render_dot.go`), and JSON serialization (`render_json.go`).
- Alias resolution via both filename stems and `meta.ID`, with cycle key mapping scoped to waiting tasks.
- `--all` flag controls whether completed and failed tasks appear in the output.

### `internal/config/`
- Repository-local `.mato.yaml` loading — `Load`, `LoadFile` (`config.go`).
- Strict unknown-key rejection via `yaml.Decoder.KnownFields(true)`.
- Multi-document YAML rejection.
- Whitespace-only string normalization (treated as unset).

### `internal/dirs/`
- Canonical queue directory name constants shared across packages — `Root`, `Waiting`, `Backlog`, `InProgress`, `ReadyReview`, `ReadyMerge`, `Completed`, `Failed`, `Locks` (`dirs.go`).
- `All` ordered slice of all queue directories (excludes `Locks`).

### `internal/lockfile/`
- Generic exclusive file-lock mechanism — `Acquire`, `Register`, `IsHeld`, `CheckHeld` (`lockfile.go`).
- Locks use `"PID:starttime"` identity strings via `process.LockIdentity` for stale-lock detection.
- `Register` is non-exclusive (overwrites); `Acquire` is exclusive (hard-link + retry).

### `internal/pause/`
- Durable `.paused` sentinel management — `Read`, `Pause`, `Resume` (`pause.go`).
- `Pause` writes the current UTC RFC3339 timestamp; repairs malformed sentinels.
- `Resume` removes the sentinel file.

### `internal/runtimecleanup/`
- Best-effort cleanup of both `taskstate` and `sessionmeta` for terminal task transitions — `DeleteAll` (`runtimecleanup.go`).

### `internal/sessionmeta/`
- Durable Copilot session metadata under `.mato/runtime/sessionmeta/` — `LoadOrCreate`, `Save`, `DeleteAll`, `Sweep` (`sessionmeta.go`).
- Separate records for work and review phases (`work-<task>.json`, `review-<task>.json`).
- `Sweep` removes stale records whose task is no longer active in any non-terminal queue directory.

### `internal/setup/`
- `mato init` bootstrap flow — `InitRepo` (`setup.go`).
- Composes `git.EnsureBranch`, `git.EnsureIdentity`, `git.EnsureGitignoreContains`, and `messaging.Init` to create the queue layout, target branch, and git identity without requiring Docker.

### `internal/taskstate/`
- Lightweight per-task runtime state tracking under `.mato/runtime/taskstate/` — `Update`, `Load`, `Delete`, `Sweep` (`taskstate.go`).
- Records pushed branch tips, review outcomes, and agent progress for follow-up review continuity.
- `Sweep` removes stale records whose task is no longer active.

### `internal/testutil/`
- Shared test helpers — `SetupRepo`, `SetupRepoWithTasks` (`testutil.go`).
- Used by integration tests and package-level tests to create temporary git repos with pre-populated task queues.

### Test files
Most packages have tests alongside their source. `internal/git/` has `git_test.go` (covering helpers like `EnsureGitignoreContains` and `CommitGitignore`) and its helpers are also exercised through the integration tests. Integration tests live in `internal/integration/` using `package integration_test`. Repository tests run with `go test ./...`.
## 10. Host-Curated Knowledge Flow
The host acts as a knowledge broker between agents, following an agent → host → agent pattern. Individual agents produce information as side effects of their work (conflict warnings with changed-file lists, failure records in task metadata), and the host aggregates this information and injects curated context into new agents before they start.

Concrete examples:
- **Failure context**: When an agent fails, it appends a `<!-- failure: ... -->` comment to the task file. On the next attempt, the host reads these lines via `extractFailureLines(...)` and injects them as `MATO_PREVIOUS_FAILURES`, so the new agent can learn from prior mistakes without parsing the task file itself.
- **File claims**: The host scans `affects:` metadata across active tasks and writes a `file-claims.json` index. Entries are stored as the literal `affects:` keys, including directory prefixes ending with `/`. Each new agent receives `MATO_FILE_CLAIMS` pointing to this index, enabling it to detect file-level conflicts with other running tasks.
- **Dependency context**: After merging a task, the host writes a `CompletionDetail` record. When a dependent task launches, the host reads these records, writes them to a file, and injects the file path as `MATO_DEPENDENCY_CONTEXT`, giving the agent full knowledge of what its prerequisites changed.
- **Review feedback**: When the review agent rejects a task, it appends `<!-- review-rejection: ... -->` to the task file. On the next attempt, the host reads these lines and injects them as `MATO_REVIEW_FEEDBACK`, so the implementing agent can address the reviewer's concerns.
- **Review continuity metadata**: The host stores lightweight runtime metadata in `.mato/runtime/taskstate/` and injects an explicit review-context block into review prompts. This keeps follow-up reviews auditable and complements durable Copilot session resume rather than relying on it as the only source of continuity.

This pattern keeps agents stateless and single-task-focused while the host provides the coordination intelligence. No agent needs to query other agents directly — the host curates and delivers the relevant context at launch time.

## 11. End-to-End Summary
Responsibility is split cleanly:
- the host owns queue maintenance, dependency promotion, overlap prevention, stale-state cleanup, review orchestration, and merging;
- task agents own isolated task execution;
- review agents own change evaluation and approval/rejection;
- Git branches carry code changes;
- task files carry scheduling metadata and retry history;
- `.locks/` and `.queue` provide coarse coordination;
- `.mato/messages/` provides advisory coordination.

The merge queue enriches each squash commit with git trailers (`Task-ID`, `Affects`) and writes completion detail files that flow back to dependent agents via `MATO_DEPENDENCY_CONTEXT`. This creates an agent → review → merge → dependent-agent knowledge chain: a task agent pushes its branch, the review agent evaluates the changes, the merge queue records what changed, and the next agent that depends on that work receives the context automatically.

In practice, `mato` is a queue scheduler built from ordinary filesystem state, Git branches, and short-lived Dockerized Copilot runs rather than a centralized service.
