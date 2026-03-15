# Mato Architecture
This document describes the architecture implemented by `main.go`, `runner.go`, `queue.go`, `git.go`, `merge.go`, `frontmatter.go`, `messaging.go`, `status.go`, `util.go`, and the embedded task prompt in `task-instructions.md`.
## 1. System Overview
`mato` is a filesystem-backed task orchestrator for Copilot agents. The host process watches `.tasks/`, promotes tasks whose dependencies are satisfied, defers overlapping tasks, launches one agent run at a time in an ephemeral Docker container, and squash-merges completed task branches back into a target branch. The agent container handles exactly one task lifecycle: claim one task, create one task branch, make changes in an isolated clone, push the branch, move the task file to `ready-to-merge/`, and exit.
```text
+-------------------+      +-----------------------------+
| CLI: mato         |      | CLI: mato status            |
| main.go -> run()  |      | main.go -> showStatus()     |
+---------+---------+      +-----------------------------+
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
1. `main.go` parses flags and either starts `run(...)` or routes to `showStatus(...)`.
2. `run(...)` creates/maintains the queue, writes `.queue`, and starts agent runs.
3. The agent prompt in `task-instructions.md` claims one task and pushes `task/<sanitized-filename>`.
4. The host merge queue squashes that task branch into the target branch and moves the task file to `completed/`.
## 2. Host Loop
### Startup
`run(repoRoot, branch, tasksDirOverride, copilotArgs)` performs host initialization in this order:
1. Resolve `repoRoot` with `git rev-parse --show-toplevel`.
2. Ensure the target branch exists with `ensureBranch(...)`; if it exists, check it out, otherwise create it from `HEAD`.
3. Resolve `tasksDir`; default is `<repoRoot>/.tasks`.
4. Create queue directories: `waiting/`, `backlog/`, `in-progress/`, `ready-to-merge/`, `completed/`, and `failed/`.
5. Create messaging directories with `initMessaging(...)`: `.tasks/messages/events/` and `.tasks/messages/presence/`.
6. Generate an agent ID with `generateAgentID()`.
7. Register the process as active by writing `.tasks/.locks/<agentID>.pid`.
8. Ensure `/.tasks/` is in `.gitignore` via `ensureGitignored(...)`; if missing, `mato` appends it, stages `.gitignore`, and commits that change.
9. Resolve host tools and config: Docker image, `copilot`, `git`, `git-upload-pack`, `git-receive-pack`, `gh`, `GOROOT`, optional `~/.config/gh`, optional `/etc/ssl/certs`, and Git author/committer identity.
10. Build the embedded prompt by replacing placeholders in `task-instructions.md` with `/workspace/.tasks`, the configured target branch, and `/workspace/.tasks/messages`.
11. Install `SIGINT`/`SIGTERM` handlers.
### Polling loop
The loop in `run()` polls every 10 seconds. The exact order is:
```text
recoverOrphanedTasks(tasksDir)
cleanStaleLocks(tasksDir)
cleanStalePresence(tasksDir)
cleanOldMessages(tasksDir, 24*time.Hour)
reconcileReadyQueue(tasksDir)
writeQueueManifest(tasksDir)
removeOverlappingTasks(tasksDir)
writeQueueManifest(tasksDir)
hasAvailableTasks(tasksDir)
runOnce(...) if backlog has tasks
acquireMergeLock(tasksDir) + processMergeQueue(...)
if no backlog and no ready-to-merge: print idle message
wait for signal or 10 seconds
```
Important details from the implementation:
- Orphan recovery happens before new work so abandoned `in-progress/` tasks can be retried.
- Queue cleanup is fully filesystem-based; there is no database or daemon.
- `.queue` is written twice each iteration: once after dependency promotion and again after overlap deferral, so the manifest matches the final backlog state.
- `hasAvailableTasks(...)` only checks `backlog/`, not `waiting/`.
- Merge processing happens after any agent run in the same outer loop.
### Orphan recovery and lock cleanup
`queue.go` provides the host-side recovery primitives:
- `registerAgent(...)` writes `.tasks/.locks/<agentID>.pid` and returns a cleanup function.
- `isAgentActive(...)` reads a PID file and tests liveness with signal `0`.
- `cleanStaleLocks(...)` removes dead agent lock files.
- `recoverOrphanedTasks(...)` scans `in-progress/*.md`; if the claiming agent is no longer active, it appends `<!-- failure: mato-recovery ... -->` and renames the task back to `backlog/`.
- If `claimed-by` points at a still-live agent, recovery skips that task.
### Signal handling
`run()` listens for `SIGINT` and `SIGTERM`. On either signal it prints `Interrupted. Exiting.`, returns `nil`, and the deferred cleanup removes the host lock file.
## 3. Agent Lifecycle
`runOnce(...)` launches one isolated task agent.
### Temporary clone and origin behavior
Before Docker starts, `runOnce(...)`:
1. Creates a temporary local clone with `createClone(repoRoot)`.
2. Defers `removeClone(cloneDir)`.
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
The final command is:
```text
copilot -p <embedded task prompt> --autopilot --allow-all [copilotArgs...]
```
If forwarded arguments do not already contain `--model`, `runOnce(...)` appends `--model claude-opus-4.6`.
### Task state machine from `task-instructions.md`
```text
SELECT_TASK -> CLAIM_TASK -> CREATE_BRANCH -> WORK -> COMMIT -> PUSH_BRANCH -> MARK_READY
                     \______________________________________________________________/
                                      unrecoverable error -> ON_FAILURE
```
State-by-state behavior:
- `SELECT_TASK`: read `.tasks/.queue` if present, otherwise list `backlog/*.md` alphabetically; skip invalid/missing entries.
- `CLAIM_TASK`: move one file `backlog/ -> in-progress/`, prepend `<!-- claimed-by: ... -->`, count `<!-- failure: ... -->` lines, fail immediately if the count is already `>= 3`, read recent messages, then write one best-effort `intent` message.
- `CREATE_BRANCH`: create `task/<safe-name>` from the target branch. The safe name is derived from the task filename stem; Go reconstructs the same name later with `sanitizeBranchName(...)`.
- `WORK`: read the task body, ignoring YAML frontmatter and comment-only metadata lines, implement the task, and run the repository's existing validation commands with up to 3 attempts.
- `COMMIT`: `git add -A`, commit with the task title, and continue only if a commit is created.
- `PUSH_BRANCH`: compute changed files relative to the target branch, write one best-effort `conflict-warning` message, push `origin <task-branch>` with up to 3 attempts, and verify the remote branch exists.
- `MARK_READY`: move the task file `in-progress/ -> ready-to-merge/`, write one best-effort `completion` message, and exit.
- `ON_FAILURE`: append a structured `<!-- failure: ... -->` line, try to check out the target branch, then move the task back to `backlog/` if failures are still below 3, otherwise to `failed/`.
The prompt enforces several invariants: one task per run, no force-pushes, no direct pushes to the target branch, and at most 3 messages per task (`intent`, `conflict-warning`, `completion`).
## 4. Task Queue States
The queue is encoded directly in directories under `.tasks/`.
```text
waiting/ --all deps complete--> backlog/ --claim--> in-progress/ --mark ready--> ready-to-merge/ --merge--> completed/
   ^                               |                     |
   |                               |                     +--ON_FAILURE / recovery--> backlog/ or failed/
   +--removeOverlappingTasks()-----+                     
```
State meanings:
| State | Meaning | Entered by | Left by |
| --- | --- | --- | --- |
| `waiting/` | blocked task | authored there initially or deferred from `backlog/` because of overlap | `reconcileReadyQueue(...)` moves it to `backlog/` when every dependency is completed |
| `backlog/` | claimable task | initial ready state, dependency promotion, orphan recovery, merge requeue, or `ON_FAILURE` with retries left | agent claim moves it to `in-progress/`; overlap deferral moves it to `waiting/` |
| `in-progress/` | task owned by one agent | prompt `CLAIM_TASK` | `MARK_READY`, `ON_FAILURE`, retry exhaustion, or host orphan recovery |
| `ready-to-merge/` | branch pushed, waiting for host merge | prompt `MARK_READY` | `processMergeQueue(...)` moves it to `completed/` on success or `backlog/` on failure |
| `completed/` | merged terminal state | host merge success | no normal exit |
| `failed/` | retry budget exhausted | prompt `CLAIM_TASK` or `ON_FAILURE` when failure count reaches 3+ | no normal exit |
Retry counting is comment-based, not directory-based. The prompt checks for `<!-- failure:` lines, and host recovery/merge failures also append those lines.
## 5. Dependency Resolution
`reconcileReadyQueue(tasksDir)` in `queue.go` promotes waiting tasks whose dependencies are satisfied.
How it works:
1. `completedTaskIDs(tasksDir)` scans `completed/*.md`.
2. For each completed task, it records both the filename stem and `TaskMeta.ID` in a set.
3. `reconcileReadyQueue(...)` scans `waiting/*.md` and parses each file with `parseTaskFile(...)`.
4. Every entry in `meta.DependsOn` must exist in the completed-ID set.
5. If all dependencies are present, the task moves `waiting/ -> backlog/`; otherwise it stays in `waiting/`.
What “ready” means in the current code:
- A dependency is satisfied only if it matches a task already in `completed/`.
- Matching can happen by file stem or explicit `id:` frontmatter.
- Dependencies on tasks that exist elsewhere but are not completed are treated the same as unknown IDs: not ready.
- The function prints a warning for each unsatisfied dependency, describing it as a missing task ID.
Relevant frontmatter defaults from `frontmatter.go`:
- `ID` defaults to the filename stem.
- `Priority` defaults to `50`.
- `MaxRetries` defaults to `3`.
## 6. Merge Queue
`processMergeQueue(repoRoot, tasksDir, branch)` in `merge.go` is the host-side integrator.
### Locking
Before processing the queue, `run()` calls `acquireMergeLock(tasksDir)`. The lock file is `.tasks/.locks/merge.lock`.
Behavior:
- create with `O_CREATE|O_EXCL`
- write the holder PID into the file
- if the file already exists, read the PID
- if that PID is still alive, skip merging this loop
- if the PID is stale or invalid, remove the lock and retry once
This is the main multi-process safety mechanism for host-side merges.
### Task ordering and merge flow
`processMergeQueue(...)` scans `ready-to-merge/*.md`, parses each task, and sorts by ascending `priority`, then filename.
For each task:
1. Parse the task file with `parseTaskFile(...)`.
2. Derive the squash commit message with `taskTitle(...)` from the first non-empty body line; leading `#` is stripped.
3. Create a fresh temp clone.
4. Configure clone identity from repo Git config, then global config, with fallbacks `mato` and `mato@local.invalid`.
5. `git fetch origin`
6. `git checkout -B <target-branch> origin/<target-branch>`
7. Reconstruct the task branch as `task/<sanitizeBranchName(filename)>`
8. `git merge --squash origin/<task-branch>`
9. `git commit -m <task title>`
10. `git push origin <target-branch>`
11. Rename the task file `ready-to-merge/ -> completed/`
### Conflict and failure handling
Any parse, fetch, checkout, squash merge, commit, or push error is treated as a merge-queue failure. `failMergeTask(...)` then:
1. appends `<!-- failure: merge-queue ... -->` to the task file
2. renames it `ready-to-merge/ -> backlog/`
So merge conflicts are not resolved automatically; they are requeued for another future agent attempt.
## 7. Conflict Prevention
`removeOverlappingTasks(tasksDir)` prevents multiple backlog tasks from advertising the same `affects` entries simultaneously.
Algorithm:
1. Scan `backlog/*.md`.
2. Parse each task's `priority` and `affects`.
3. Sort tasks by ascending priority, then filename.
4. Keep the highest-priority non-overlapping tasks in `backlog/`.
5. For each later task, compare its `affects` list with every already-kept task.
6. If there is overlap, rename the later task `backlog/ -> waiting/`.
`overlappingAffects(a, b)` is an exact intersection test:
- no overlap if either list is empty
- values must match by exact string equality
- duplicates are removed from the overlap report
- the overlap list is sorted before logging
Important consequence: `affects` is metadata, not a live diff. `mato` does not interpret it as globs or path prefixes.
## 8. Code Structure
### `main.go`
- CLI entrypoint.
- `main()` routes `status` to `showStatus(...)` and otherwise starts `run(...)`.
- `parseArgs(...)` handles `--repo`, `--branch`, `--tasks-dir`, `--help`, `--`, and forwards all other args to Copilot CLI.
### `runner.go`
- Embeds `task-instructions.md`.
- `run(...)` initializes the repo/queue and executes the polling loop.
- `runOnce(...)` builds the temp clone + Docker runtime for one agent run.
### `queue.go`
- Agent liveness via `.locks/*.pid`.
- Orphan recovery, dependency promotion, backlog overlap deferral, and `.queue` manifest writing.
### `git.go`
- `gitOutput(...)` wrapper.
- Temp clone helpers.
- Branch creation/check-out and `.gitignore` maintenance.
### `merge.go`
- Ready-to-merge scanning and ordering.
- Temp-clone squash merges into the target branch.
- Merge lock and merge-failure requeue path.
### `frontmatter.go`
- `TaskMeta` schema.
- YAML-like frontmatter parsing.
- Default metadata values and task-body extraction.
- Strips comment-only HTML metadata lines from the body returned to consumers.
### `messaging.go`
- `Message` and `presence` JSON types.
- Event/presence directory setup.
- Atomic JSON write helpers.
- Message reading, stale presence cleanup, and old-event garbage collection.
### `status.go`
- `mato status` implementation.
- Counts task files by queue directory.
- Lists active agents from `.locks/*.pid`.
- Shows waiting-task dependency summaries and the five most recent messages.
### `util.go`
- Random agent ID generation.
- Branch-name sanitization.
- `hasModelArg(...)` helper for Docker command construction.
### `task-instructions.md`
- The embedded agent prompt/state machine.
- Describes folder semantics, per-state commands, retry rules, and failure handling.
## 9. End-to-End Summary
Responsibility is split cleanly:
- the host owns queue maintenance, dependency promotion, overlap prevention, stale-state cleanup, and merging;
- the agent owns one isolated task execution;
- Git branches carry code changes;
- task files carry scheduling metadata and retry history;
- `.locks/` and `.queue` provide coarse coordination;
- `.tasks/messages/` provides advisory coordination.
In practice, `mato` is a queue scheduler built from ordinary filesystem state, Git branches, and short-lived Dockerized Copilot runs rather than a centralized service.
