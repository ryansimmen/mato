# Mato Architecture
This document describes the architecture implemented by `cmd/mato/main.go` and the packages in `internal/`: `runner/`, `queue/`, `git/`, `merge/`, `frontmatter/`, `messaging/`, `status/`, `identity/`, `lockfile/`, `process/`, `atomicwrite/`, and `taskfile/`.
## 1. System Overview
`mato` is a filesystem-backed task orchestrator for Copilot agents. The host process watches `.tasks/`, promotes tasks whose dependencies are satisfied, defers overlapping tasks, selects and claims a task, launches one agent run at a time in an ephemeral Docker container, and squash-merges completed task branches back into a target branch. The agent container handles exactly one pre-claimed task lifecycle: verify the claim, create one task branch, make changes in an isolated clone, push the branch, move the task file to `ready-to-merge/`, and exit.
```text
+-------------------------+      +-----------------------------+
| CLI: mato               |      | CLI: mato status            |
| main.go -> runner.Run() |      | main.go -> status.Show()    |
+------------+------------+      +-----------------------------+
             |
             v
+-------------------------------+
| Host loop in runner.go        |
| manages .tasks/, Docker, Git  |
+---------------+---------------+
                |
                v
       +-------------------+
       | .tasks/ queue     |
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
1. `main.go` parses flags and either starts `runner.Run(...)` or routes to `status.Show(...)`.
2. `runner.Run(...)` creates/maintains the queue, writes `.queue`, and starts agent runs.
3. The host selects and claims a task via `queue.SelectAndClaimTask(...)`, then launches the agent with pre-resolved task info as env vars (`MATO_TASK_FILE`, `MATO_TASK_BRANCH`, `MATO_TASK_TITLE`, `MATO_TASK_PATH`). The agent prompt in `task-instructions.md` verifies the claim and pushes `task/<sanitized-filename>`.
4. The host merge queue squashes that task branch into the target branch and moves the task file to `completed/`.
## 2. Host Loop
### Startup
`runner.Run(repoRoot, branch, tasksDirOverride, copilotArgs)` performs host initialization in this order:
1. Resolve `repoRoot` with `git rev-parse --show-toplevel`.
2. Ensure the target branch exists with `git.EnsureBranch(...)`; if the local branch exists, check it out; otherwise fetch the branch from `origin` (non-fatal on failure), then if `origin/<branch>` exists create the local branch from the remote-tracking ref; otherwise create it from `HEAD`.
3. Resolve `tasksDir`; default is `<repoRoot>/.tasks`.
4. Create queue directories: `waiting/`, `backlog/`, `in-progress/`, `ready-for-review/`, `ready-to-merge/`, `completed/`, and `failed/`.
5. Create messaging directories with `messaging.Init(...)`: `.tasks/messages/events/`, `.tasks/messages/completions/`, and `.tasks/messages/presence/`.
6. Generate an agent ID with `identity.GenerateAgentID()`.
7. Register the process as active by writing `.tasks/.locks/<agentID>.pid` via `queue.RegisterAgent(...)`.
8. Ensure `/.tasks/` is in `.gitignore` via `git.EnsureGitignoreContains(...)`, then commit with `git.CommitGitignore(...)` only if the file was modified.
9. Resolve host tools and config: Docker image, `copilot`, `git`, `git-upload-pack`, `git-receive-pack`, `gh`, `GOROOT`, optional `~/.config/gh`, optional `/etc/ssl/certs`, and Git author/committer identity.
10. Build the embedded prompts by replacing placeholders in `task-instructions.md` and `review-instructions.md` with `/workspace/.tasks`, the configured target branch, and `/workspace/.tasks/messages`.
11. Install `SIGINT`/`SIGTERM` handlers.
### Polling loop
The loop in `Run()` polls every 10 seconds. The exact order is:
```text
queue.RecoverOrphanedTasks(tasksDir)
queue.CleanStaleLocks(tasksDir)
messaging.CleanStalePresence(tasksDir)
messaging.CleanOldMessages(tasksDir, 24*time.Hour)
queue.ReconcileReadyQueue(tasksDir)
deferred := queue.DeferredOverlappingTasks(tasksDir)
// merge failedDirExcluded into deferred
queue.WriteQueueManifest(tasksDir, deferred)
claimed, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, deferred)
if FailedDirUnavailableError → add task to failedDirExcluded
if claimed != nil:
    messaging.WriteMessage(...) intent
    runOnce(...)
    recoverStuckTask(...)
reviewTask := selectTaskForReview(tasksDir)
if reviewTask != "":
    runReview(reviewTask, ...)
merge.AcquireLock(tasksDir) + merge.ProcessQueue(...)
if no claimed task and no ready-to-merge: print idle message
wait for signal or 10 seconds
```
Important details from the implementation:
- Orphan recovery happens before new work so abandoned `in-progress/` tasks can be retried.
- Queue cleanup is fully filesystem-based; there is no database or daemon.
- `.queue` is written once per iteration, after overlap deferral, so the manifest reflects the final backlog state. The manifest is a newline-separated list of task filenames (e.g. `my-task.md`), ordered by priority ascending (lower number = higher priority), then alphabetically by filename. It is written atomically by the host via `WriteQueueManifest()`. Conflict-deferred tasks are excluded from the manifest so agents will not select them.
- `queue.SelectAndClaimTask(...)` reads `.queue` if present, parses each candidate's frontmatter and counts `<!-- failure: ... -->` records, atomically moves the candidate from `backlog/` to `in-progress/`, then checks the failure record budget—moving exhausted tasks to `failed/` (or returning a `FailedDirUnavailableError` if `failed/` is unavailable). `HasAvailableTasks` is a separate helper but is not called in the polling loop.
- When a `FailedDirUnavailableError` is returned, the host loop adds the task filename to a persistent `failedDirExcluded` set. On each subsequent iteration, excluded tasks are merged into the `deferred` map before calling `SelectAndClaimTask`, preventing the same task (whose failure record budget is exhausted) from being re-selected and livelocking the host loop.
- After claiming, the host writes a best-effort `intent` message, then launches `runOnce(...)`.
- `recoverStuckTask(...)` runs immediately after `runOnce(...)` returns: if the task file is still in `in-progress/`, the agent did not complete its lifecycle, so the host appends a failure record and moves the task back to `backlog/`.
- Merge processing happens after any agent run in the same outer loop.
### Orphan recovery and lock cleanup
The `queue` package (spread across `locks.go` and `reconcile.go`) provides the host-side recovery primitives:
- `queue.RegisterAgent(...)` writes `.tasks/.locks/<agentID>.pid` and returns a cleanup function.
- `identity.IsAgentActive(...)` reads a PID file and tests liveness with signal `0`.
- `queue.CleanStaleLocks(...)` removes dead agent lock files.
- `queue.RecoverOrphanedTasks(...)` scans `in-progress/*.md`; if the claiming agent is no longer active, it appends `<!-- failure: mato-recovery ... -->` and renames the task back to `backlog/`.
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
The design relies on `.tasks/` being Git-ignored so queue updates do not dirty the branch being updated via `updateInstead`.
### Docker runtime
The container runs as `docker run --rm -it --user <uid>:<gid>` with working directory `/workspace`.
Primary mounts:
| Host | Container | Why |
| --- | --- | --- |
| temp clone | `/workspace` | isolated working tree for the agent |
| `tasksDir` | `/workspace/.tasks` | shared queue and message state |
| `repoRoot` | same absolute host path | keeps the clone's local-path `origin` reachable |
| `copilot` | `/usr/local/bin/copilot` (ro) | Copilot CLI inside container |
| `git` | `/usr/local/bin/git` (ro) | Git inside container |
| `git-upload-pack` | `/usr/local/bin/git-upload-pack` (ro) | local-path fetch support |
| `git-receive-pack` | `/usr/local/bin/git-receive-pack` (ro) | local-path push support |
| `gh` | `/usr/local/bin/gh` (ro) | GitHub CLI |
| host `GOROOT` | `/usr/local/go` (ro) | Go toolchain |
| host `~/.copilot` | `$HOME/.copilot` | Copilot auth/package state |
| host `~/go/pkg/mod` | `$HOME/go/pkg/mod` | Go module cache |
| host `~/.cache/go-build` | `$HOME/.cache/go-build` | Go build cache |
| host `~/.config/gh` | `$HOME/.config/gh` (ro, optional) | `gh` config |
| host `/etc/ssl/certs` | `/etc/ssl/certs` (ro, optional) | CA trust |
Environment variables injected by the host:
- `GOROOT=/usr/local/go`
- `PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`
- `MATO_AGENT_ID=<generated id>`
- `MATO_MESSAGING_ENABLED=1`
- `MATO_MESSAGES_DIR=/workspace/.tasks/messages`
- `GIT_CONFIG_COUNT=1`, `GIT_CONFIG_KEY_0=safe.directory`, `GIT_CONFIG_VALUE_0=*`
- `GIT_AUTHOR_NAME` / `GIT_COMMITTER_NAME` if host Git config supplies a name
- `GIT_AUTHOR_EMAIL` / `GIT_COMMITTER_EMAIL` if host Git config supplies an email
- `HOME=<host home path>`
- `GOPATH=<home>/go`, `GOMODCACHE=<home>/go/pkg/mod`, `GOCACHE=<home>/.cache/go-build`
- `MATO_DEPENDENCY_CONTEXT=<file path>` (conditionally, only when the task has `depends_on` entries with available completion data; points to `.tasks/messages/dependency-context-<filename>.json`)
The final command is:
```text
copilot -p <embedded task prompt> --autopilot --allow-all [copilotArgs...]
```
If forwarded arguments do not already contain `--model`, `runOnce(...)` appends `--model` with the value from `MATO_DEFAULT_MODEL` (or `claude-opus-4.6` if unset).
### Host-side task claiming
Before launching a Docker container, the host calls `queue.SelectAndClaimTask(tasksDir, agentID, deferred)` which:
1. Reads `.tasks/.queue` if present, otherwise lists `backlog/*.md` alphabetically.
2. Parses each candidate's frontmatter and counts `<!-- failure: ... -->` records.
3. Atomically renames the candidate from `backlog/` to `in-progress/`.
4. If the number of `<!-- failure: ... -->` records >= `max_retries` (default 3), moves the task from `in-progress/` to `failed/` and continues to the next candidate. If `failed/` is unavailable, the task is rolled back to `backlog/` and a `FailedDirUnavailableError` (carrying the task filename) is returned; the host loop adds this task to a persistent exclusion set to prevent livelocking on the same retry-exhausted task.
5. Prepends a `<!-- claimed-by: ... -->` header to the task file.
6. Returns the filename, derived branch name, extracted title, and host-side path.

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

After the agent exits, the host (`postAgentPush` in `runner.go`) checks for commits on the task branch. If commits exist and the task is still in `in-progress/`, the host pushes the branch with `--force-with-lease`, writes the `<!-- branch: ... -->` marker, moves the task to `ready-for-review/`, and sends `conflict-warning` and `completion` messages.

The prompt enforces several invariants: one task per run; agents never push any branches (the host handles all pushes); they send at most 4 messages per task (3 `progress` + 1 for `ON_FAILURE`). The `intent` message is sent by the host before the agent starts.
## 4. Task Queue States
The queue is encoded directly in directories under `.tasks/`.
```text
waiting/ --deps met--> backlog/ --host claim--> in-progress/ --host push--> ready-for-review/ --approved--> ready-to-merge/ --merge--> completed/
   ^                      |                          |                              |
   |                      |                          +--ON_FAILURE--> backlog/       +--review rejection--> backlog/
   +--dep not met---------+                          +--host orphan recovery--> backlog/
                          |                          +--host failure record budget exhausted--> failed/
                          +--conflict-deferred tasks stay in backlog/ but excluded from .queue
```
State meanings:
| State | Meaning | Entered by | Left by |
| --- | --- | --- | --- |
| `waiting/` | blocked task | authored there initially for dependency tracking | `queue.ReconcileReadyQueue(...)` moves it to `backlog/` when every dependency is completed and no active overlap exists |
| `backlog/` | claimable task (or conflict-deferred) | initial ready state, dependency promotion, orphan recovery, merge requeue after squash conflicts, `ON_FAILURE` requeue, or review rejection | host `SelectAndClaimTask` moves it to `in-progress/`; conflict-deferred tasks remain here but are excluded from `.queue` |
| `in-progress/` | task owned by one agent | host `SelectAndClaimTask` | host `postAgentPush` (to `ready-for-review/`), `ON_FAILURE`, or host orphan recovery |
| `ready-for-review/` | branch pushed, waiting for AI review | host `postAgentPush` | host `postReviewAction` moves to `ready-to-merge/` on approval, or to `backlog/` on rejection |
| `ready-to-merge/` | reviewed and approved, waiting for host merge | host `postReviewAction` | `merge.ProcessQueue(...)` moves it to `completed/` on success, to `backlog/` on squash conflict, to `failed/` if the task branch is missing, or leaves it in place if the squash commit cannot be pushed |
| `completed/` | merged terminal state | host merge success | no normal exit |
| `failed/` | failure record budget exhausted or merge blocked by a missing task branch | host `SelectAndClaimTask` moves already-claimed task from `in-progress/` to `failed/` when the number of `<!-- failure: ... -->` records reaches `max_retries` (default 3), or merge-queue missing-branch handling | no normal exit |
Retry counting is comment-based, not directory-based. Task agent failures use `<!-- failure: ... -->` records, counted by `CountFailureLines()` in `SelectAndClaimTask` and in the merge queue. Review infrastructure failures use `<!-- review-failure: ... -->` records, counted separately by `CountReviewFailureLines()` in `reviewCandidates()`. This separation ensures that transient review issues (network blips, diff timeouts) do not consume a task's failure record budget, and vice versa.
## 5. Dependency Resolution
`queue.ReconcileReadyQueue(tasksDir)` in `queue/reconcile.go` promotes waiting tasks whose dependencies are satisfied.
How it works:
1. `completedTaskIDs(tasksDir)` (in `reconcile.go`) scans `completed/*.md`.
2. For each completed task, it records both the filename stem and `TaskMeta.ID` in a set.
3. `queue.ReconcileReadyQueue(...)` scans `waiting/*.md` and parses each file with `frontmatter.ParseTaskFile(...)`.
4. Every entry in `meta.DependsOn` must exist in the completed-ID set.
5. If all dependencies are present, the task moves `waiting/ -> backlog/`; otherwise it stays in `waiting/`.
What “ready” means in the current code:
- A dependency is satisfied only if it matches a task already in `completed/`.
- Matching can happen by file stem or explicit `id:` frontmatter.
- Dependencies on tasks that exist elsewhere in `waiting/`, `backlog/`, `in-progress/`, `ready-to-merge/`, or `failed/` but are not completed remain blocked silently.
- The function warns only for truly unknown dependency IDs, meaning IDs not found in any queue directory.
Relevant frontmatter defaults from `frontmatter.go`:
- `ID` defaults to the filename stem.
- `Priority` defaults to `50`.
- `MaxRetries` defaults to `3`.
## 6. Merge Queue
`merge.ProcessQueue(repoRoot, tasksDir, branch)` in `merge.go` is the host-side integrator.
### Locking
Before processing the queue, `Run()` calls `merge.AcquireLock(tasksDir)`. The lock file is `.tasks/.locks/merge.lock`.
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
12. After a successful push, write a completion detail file to `.tasks/messages/completions/<task-id>.json` with the commit SHA, changed files, branch, title, and merge timestamp (see `docs/messaging.md` for the full schema). This file is used by `runner.writeDependencyContextFile(...)` to create the dependency context file referenced by `MATO_DEPENDENCY_CONTEXT` when a dependent task runs.
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
1. Missing task branch: append `<!-- failure: merge-queue ... -->` and move the task `ready-to-merge/ -> failed/`.
2. Squash merge conflict: append `<!-- failure: merge-queue ... -->`, move the task `ready-to-merge/ -> backlog/`, and delete the stale task branch locally and on `origin` so a future agent run can push a fresh branch.
3. Push failure after a successful squash commit: append `<!-- failure: merge-queue ... -->` but leave the task file in `ready-to-merge/` for retry on the next merge pass.
4. Parse errors are also recorded as merge-queue failures and requeued to `backlog/`.
## 7. Review Agent
After the host pushes a task branch and moves the task to `ready-for-review/`, it launches a review agent to evaluate the changes before merging.

### How it works
1. `selectTaskForReview(tasksDir)` scans `ready-for-review/*.md` for the next task to review.
2. The host verifies the task branch exists (`git rev-parse --verify`). If the branch is missing, the host writes a `<!-- review-failure: ... -->` record and skips the review.
3. `runReview(...)` launches a review agent in the same Docker container model as task agents (ephemeral container, temp clone, identical bind mounts).
4. The review agent diffs the task branch against the target branch, analyzes the changes, and writes a JSON verdict file to `.tasks/messages/verdict-<filename>.json` with `{"verdict":"approve"}`, `{"verdict":"reject","reason":"..."}`, or `{"verdict":"error","reason":"..."}`.
5. After the review agent exits, the host reads the verdict file via `postReviewAction(...)`:
   - **Approved**: host writes `<!-- reviewed: ... -->` to the task file, moves it to `ready-to-merge/`, and sends a completion message.
   - **Rejected**: host writes `<!-- review-rejection: ... -->` to the task file, moves it back to `backlog/`, and sends a completion message.
   - **Error**: host writes a `<!-- review-failure: ... -->` record; the task stays in `ready-for-review/`.
   - **No verdict file** (agent crashed): host falls back to checking for HTML markers in the task file, then writes a `<!-- review-failure: ... -->` record if none found.

### Key properties
- Review rejections do **not** count against `max_retries`. Only `<!-- failure: ... -->` records are counted for the task's failure record budget.
- Review infrastructure failures (network errors, diff timeouts) are recorded as `<!-- review-failure: ... -->` and counted separately from task failure records. This ensures transient review issues do not exhaust the task's failure record budget.
- On retry, the host injects previous review feedback via the `MATO_REVIEW_FEEDBACK` environment variable so the implementing agent can address the reviewer's concerns.
- The review agent uses the embedded prompt `review-instructions.md`.
- The review agent writes only a `progress` message; the host sends the `completion` message after processing the verdict.
- The review verdict is communicated via a JSON verdict file. The host writes the HTML comment markers to the task file for state tracking after reading the verdict.

## 8. Conflict Prevention
`queue.DeferredOverlappingTasks(tasksDir)` (in `queue/overlap.go`) prevents multiple backlog tasks from claiming the same files simultaneously.
Algorithm:
1. Scan `backlog/*.md` and `in-progress/*.md` + `ready-to-merge/*.md` (active tasks).
2. Parse each task's `priority` and `affects`.
3. Sort backlog tasks by ascending priority, then filename.
4. Seed the "kept" set with active tasks (immovable — already being worked on or awaiting merge).
5. For each backlog task, compare its `affects` list with every kept task.
6. If there is overlap, add the task to the deferred exclusion set (it stays in `backlog/` but is excluded from `.queue` — agents won't see it).
7. The exclusion set is passed to `WriteQueueManifest` and `SelectAndClaimTask`.
`overlappingAffects(a, b)` (in `overlap.go`) is an exact intersection test:
- no overlap if either list is empty
- values must match by exact string equality
- duplicates are removed from the overlap report
- the overlap list is sorted before logging
Important consequence: `affects` is metadata, not a live diff. `mato` does not interpret it as globs or path prefixes.
## 9. Code Structure

The codebase follows standard Go project layout: `cmd/mato/` for the CLI entrypoint, `internal/` for library packages.

### `cmd/mato/main.go`
- CLI entrypoint.
- `main()` routes `status` to `status.Show(...)` and otherwise starts `runner.Run(...)`.
- `parseArgs(...)` handles `--repo`, `--branch`, `--tasks-dir`, `--help`, `--`, and forwards all other args to Copilot CLI.

### `internal/runner/`
- Embeds `task-instructions.md` (the task agent prompt/state machine) and `review-instructions.md` (the review agent prompt).
- `Run(...)` initializes the repo/queue and executes the polling loop (`runner.go`).
- `runOnce(...)` builds the temp clone + Docker runtime for one task agent run (`runner.go`).
- Docker configuration split into immutable `envConfig` and per-task `runContext` (`config.go`).
- Host tool discovery (`findGitHelper`) in `tools.go`.
- Task lifecycle: `postAgentPush`, `extractFailureLines` (`task.go`).
- Review lifecycle: `selectTaskForReview`, `runReview`, `postReviewAction` (`review.go`).

### `internal/queue/`
- Task claiming and failure-record counting (`claim.go`).
- Queue directory constants (`dirs.go`).
- Lock file management — `RegisterAgent`, `CleanStaleLocks` (`locks.go`).
- Queue manifest writing (`manifest.go`).
- Overlap deferral — `DeferredOverlappingTasks` (`overlap.go`).
- Orphan recovery, dependency promotion — `ReconcileReadyQueue`, `RecoverOrphanedTasks` (`reconcile.go`).
- Task file enumeration — `ListTaskFiles` (`taskfiles.go`).
- Atomic file moves — `AtomicMove` (`queue.go`).

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
- `WriteFile` — atomically writes `[]byte` to a path via temp-file-then-rename.
- `WriteFunc` — atomically writes via a caller-supplied callback.

### `internal/taskfile/`
- Branch comment parsing — `ParseBranch`, `ParseBranchComment`, `ParseClaimedBy`, `ParseClaimedAt` (`taskfile.go`, `metadata.go`).
- Failure/review-failure tracking — `CountFailureMarkers`, `ExtractFailureLines`, `AppendFailureRecord`, etc. (`metadata.go`).
- Active task collection — `CollectActiveAffects` scans in-progress/review/merge directories (`active.go`).

### `internal/frontmatter/`
- `TaskMeta` schema.
- YAML-like frontmatter parsing (stdlib only).
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
- `mato status` dashboard — `Show`, `ShowTo`, `Watch` (`status.go`).
- Data gathering layer — `gatherStatus` collects queue counts, active agents, presence, task lists, completions, messages, merge-lock state (`status_gather.go`).
- Rendering layer — individual `render*` functions for each dashboard section, terminal colors (`status_render.go`).

### Test files
Most packages have tests alongside their source. `internal/git/` has `git_test.go` (covering helpers like `EnsureGitignoreContains` and `CommitGitignore`) and its helpers are also exercised through the integration tests. Repository tests run with `go test ./...`.
## 10. Host-Curated Knowledge Flow
The host acts as a knowledge broker between agents, following an agent → host → agent pattern. Individual agents produce information as side effects of their work (conflict warnings with changed-file lists, failure records in task metadata), and the host aggregates this information and injects curated context into new agents before they start.

Concrete examples:
- **Failure context**: When an agent fails, it appends a `<!-- failure: ... -->` comment to the task file. On the next attempt, the host reads these lines via `extractFailureLines(...)` and injects them as `MATO_PREVIOUS_FAILURES`, so the new agent can learn from prior mistakes without parsing the task file itself.
- **File claims**: The host scans `affects:` metadata across active tasks and writes a `file-claims.json` index. Each new agent receives `MATO_FILE_CLAIMS` pointing to this index, enabling it to detect file-level conflicts with other running tasks.
- **Dependency context**: After merging a task, the host writes a `CompletionDetail` record. When a dependent task launches, the host reads these records, writes them to a file, and injects the file path as `MATO_DEPENDENCY_CONTEXT`, giving the agent full knowledge of what its prerequisites changed.
- **Review feedback**: When the review agent rejects a task, it appends `<!-- review-rejection: ... -->` to the task file. On the next attempt, the host reads these lines and injects them as `MATO_REVIEW_FEEDBACK`, so the implementing agent can address the reviewer's concerns.

This pattern keeps agents stateless and single-task-focused while the host provides the coordination intelligence. No agent needs to query other agents directly — the host curates and delivers the relevant context at launch time.

## 11. End-to-End Summary
Responsibility is split cleanly:
- the host owns queue maintenance, dependency promotion, overlap prevention, stale-state cleanup, review orchestration, and merging;
- task agents own isolated task execution;
- review agents own change evaluation and approval/rejection;
- Git branches carry code changes;
- task files carry scheduling metadata and retry history;
- `.locks/` and `.queue` provide coarse coordination;
- `.tasks/messages/` provides advisory coordination.

The merge queue enriches each squash commit with git trailers (`Task-ID`, `Affects`) and writes completion detail files that flow back to dependent agents via `MATO_DEPENDENCY_CONTEXT`. This creates an agent → review → merge → dependent-agent knowledge chain: a task agent pushes its branch, the review agent evaluates the changes, the merge queue records what changed, and the next agent that depends on that work receives the context automatically.

In practice, `mato` is a queue scheduler built from ordinary filesystem state, Git branches, and short-lived Dockerized Copilot runs rather than a centralized service.
