# Approach 2: SQLite shared database for inter-agent communication

## 1. Overview

Today, `mato` agents share only the filesystem queue under `.tasks/` and the PID lock files under `.tasks/.locks/`. That is enough for atomic task claiming, but it gives agents no structured way to tell each other:

- what they are currently working on
- which files they intend to edit
- whether they discovered a blocker or warning
- whether they already solved part of a problem another agent is about to touch
- when they are done and what branch/commit contains the result

A shared SQLite database in the already-shared `.tasks/` directory is a strong fit for `mato` because it preserves the current deployment model:

- the host `mato` process already creates and manages `.tasks/`
- every Docker container already bind-mounts that same directory at `/workspace/.tasks`
- all agents are on one machine, so SQLite file locking is practical
- the workload is low-write / high-read metadata, which is exactly where SQLite performs well

Recommended database path:

- `.tasks/coordination.db`
- plus SQLite sidecar files created automatically in WAL mode:
  - `.tasks/coordination.db-wal`
  - `.tasks/coordination.db-shm`

The important design choice is: **keep the existing filesystem queue for task claiming and retries, and add SQLite only for communication and coordination**. That keeps the highest-risk part of the current design (`mv`-based task claiming) unchanged while giving agents a richer shared state channel.

In practice, the database becomes the shared "control plane" for:

- agent heartbeats
- task metadata
- message passing
- file claims / locks
- warnings and handoffs
- audit trail of what happened during a task

## 2. Architecture

### High-level flow

1. `run()` creates `.tasks/` subdirectories as it does today.
2. `run()` also opens `.tasks/coordination.db` and runs schema migrations.
3. `runOnce()` launches a container exactly as it does today, still mounting `.tasks` at `/workspace/.tasks`.
4. Inside the container, the autonomous agent writes coordination data to the shared DB while it works.
5. Other agents can query the same DB to see live status, inbox messages, claimed files, and recent completions.

### Keep existing queue semantics

The current queue behavior should remain:

- `.tasks/backlog/` is still the source of work
- claim is still an atomic move to `.tasks/in-progress/`
- retries still come from failure metadata in task files
- `.tasks/completed/` and `.tasks/failed/` still remain the final destinations

That means SQLite is **not** the source of truth for whether a task exists; it is the source of truth for coordination metadata around that task.

### Recommended schema

A practical v1 schema for `mato` would look like this:

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    task_id TEXT PRIMARY KEY,
    task_file TEXT NOT NULL UNIQUE,
    title TEXT,
    status TEXT NOT NULL,                 -- backlog, in_progress, completed, failed
    target_branch TEXT NOT NULL,
    claimed_by_agent_id TEXT,
    claimed_at TEXT,
    completed_at TEXT,
    failed_at TEXT,
    retry_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agent_status (
    agent_id TEXT PRIMARY KEY,
    state TEXT NOT NULL,                  -- idle, claiming, running, merging, completed, failed, stopped
    host_pid INTEGER,
    repo_root TEXT NOT NULL,
    clone_dir TEXT,
    container_task_file TEXT,
    current_task_id TEXT,
    current_branch TEXT,
    started_at TEXT NOT NULL,
    last_heartbeat_at TEXT NOT NULL,
    last_error TEXT,
    metadata_json TEXT
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    task_id TEXT,
    from_agent_id TEXT NOT NULL,
    to_agent_id TEXT,                     -- NULL means broadcast
    kind TEXT NOT NULL,                   -- status, warning, completion, request, handoff, file_claim
    severity TEXT NOT NULL DEFAULT 'info',-- info, warning, error
    topic TEXT,
    file_path TEXT,
    body TEXT NOT NULL,
    payload_json TEXT,
    acknowledged_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_messages_task_created
    ON messages(task_id, created_at);

CREATE INDEX IF NOT EXISTS idx_messages_target_ack
    ON messages(to_agent_id, acknowledged_at, created_at);

CREATE TABLE IF NOT EXISTS file_locks (
    file_path TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    owner_agent_id TEXT NOT NULL,
    lock_kind TEXT NOT NULL DEFAULT 'write',
    reason TEXT,
    acquired_at TEXT NOT NULL,
    refreshed_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_file_locks_owner
    ON file_locks(owner_agent_id);

CREATE TABLE IF NOT EXISTS task_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    agent_id TEXT,
    event_type TEXT NOT NULL,             -- claimed, started, test_failed, merge_retry, completed, recovered
    event_at TEXT NOT NULL,
    details_json TEXT
);
```

### Why these tables fit the current codebase

#### `tasks`

`mato` already has a task lifecycle encoded in filesystem folders. This table mirrors that lifecycle so agents can query it without scraping markdown files or directories. It also gives a place to store:

- stable task identifiers
- the current owning agent
- retry count
- last error
- target branch

A good v1 approach is to add a `task-id` metadata line when a task is claimed, alongside the existing `claimed-by` metadata that the prompt already inserts.

Example metadata header:

```markdown
<!-- task-id: 7f4d1c2a  claimed-by: a1b2c3d4  claimed-at: 2026-01-01T00:00:00Z -->
```

That preserves the current recovery model while giving SQLite a durable join key.

#### `agent_status`

Today, liveness is represented indirectly by `.tasks/.locks/<agent>.pid` plus the `claimed-by` header in a task file. That works, but it is not very expressive.

`agent_status` adds structured liveness and phase tracking:

- which agent is alive
- when it last heartbeated
- which task it owns
- which branch it is on
- whether it is claiming, testing, merging, or stuck

This table can coexist with `.locks` initially. I would keep the PID lock files in phase 1 for backward-compatible orphan recovery and gradually move stale-agent detection to heartbeat timestamps in SQLite.

#### `messages`

This is the actual communication bus. Agents can write broadcast or targeted records that other agents query with normal SQL-backed helper commands.

Examples:

- broadcast warning: "I am changing `main.go`; avoid editing it unless necessary"
- targeted request: "agent `b4e2f9c1`, please review whether your change also touches `README.md`"
- task completion: "task X completed on branch `task/add-retry`, commit `abc1234`"
- dependency note: "found flaky `go test`; likely unrelated to my task"

#### `file_locks`

This is the most useful coordination primitive after messaging. Right now multiple agents can easily collide on the same Go file and only discover the problem at merge time. `file_locks` allows an agent to announce intent before editing.

A lock here is not a kernel file lock and should not try to be perfect. It is a **cooperative claim** with TTL. That is enough to reduce collisions significantly.

#### `task_events`

This gives an append-only audit log for debugging and observability. It is especially helpful when diagnosing why a task was recovered, retried, or blocked.

### How agents should connect

There are two possible ways for agents to use the database:

1. **Direct SQL from inside the container**
2. **Go helper commands backed by SQLite**

For `mato`, I recommend **Go helper commands**.

Why:

- the current Docker image is `ubuntu:24.04`, not a purpose-built image with `sqlite3` installed
- asking autonomous Copilot agents to hand-write SQL is fragile
- a helper CLI can enforce consistent writes, short transactions, TTL handling, and message formatting
- it keeps the schema private to the implementation instead of baking SQL into prompts

### Recommended connection model

Add subcommands to the `mato` binary (or a tiny sibling binary such as `mato-agentctl`) and mount that binary into the container the same way `copilot`, `git`, and `gh` are mounted today.

Examples:

```bash
mato coord heartbeat --state running --task-file .tasks/in-progress/add-retry-logic.md
mato coord inbox --task-file .tasks/in-progress/add-retry-logic.md
mato coord lock acquire --path main.go --reason "updating run loop"
mato coord message send --kind warning --file main.go --body "Will touch run() and runOnce()"
mato coord task complete --task-file .tasks/in-progress/add-retry-logic.md --branch task/add-retry-logic --commit abc1234
```

This is much more realistic for autonomous agents than raw SQL.

## 3. Implementation Details

### SQLite driver choice

Use:

- `modernc.org/sqlite`

Reasons it fits `mato`:

- pure Go; no CGO toolchain requirement
- easy to vendor/build in the existing single-binary workflow
- works well with `database/sql`
- ideal for a small CLI that may be mounted into containers

Recommended import pattern:

```go
import (
    "database/sql"
    _ "modernc.org/sqlite"
)
```

### Code changes in `mato`

The current codebase is small, so I would avoid over-engineering. A good implementation split would be:

- `main.go`
  - wire up DB initialization
  - pass DB path / helper binary into the container
  - emit top-level agent lifecycle updates
- `coorddb.go` or `internal/coorddb/...`
  - open DB
  - run migrations
  - helper methods for heartbeats, messages, locks, task state sync
- `coordcli.go` or subcommand handling in `main.go`
  - implement `mato coord ...` commands for use by agents
- `main_test.go` plus new DB-focused tests
  - migration tests
  - lock acquisition tests
  - stale heartbeat cleanup tests

### Specific touch points in current code

#### `run()`

Today `run()`:

- resolves repo root
- ensures the branch exists
- creates `.tasks/{backlog,in-progress,completed,failed}`
- generates `agentID`
- registers PID lock files in `.tasks/.locks`
- loops forever, calling `recoverOrphanedTasks`, `cleanStaleLocks`, and `runOnce`

Add to `run()`:

1. compute `dbPath := filepath.Join(tasksDir, "coordination.db")`
2. open DB and run migrations once at startup
3. insert/update an `agent_status` row for the host-side orchestrator agent ID
4. update heartbeat before each poll iteration
5. on shutdown, mark the agent as `stopped`
6. optionally clean expired file locks and stale agent rows during the existing maintenance loop

This fits naturally with the existing polling loop.

#### `runOnce()`

Today `runOnce()` already mounts:

- the temp clone at `/workspace`
- the shared `.tasks` dir at `/workspace/.tasks`
- the origin repo path
- host binaries for `copilot`, `git`, `gh`

Add:

- mount the `mato` binary itself into the container, e.g. `/usr/local/bin/mato`
- set `MATO_DB_PATH=/workspace/.tasks/coordination.db`
- optionally set `MATO_TASKS_DIR=/workspace/.tasks`
- optionally set `MATO_TARGET_BRANCH=<branch>`

That gives the agent a stable way to use the coordination helpers without altering the fundamental container model.

A practical implementation detail is to resolve the currently running executable via `os.Executable()` and mount that path read-only into the container.

#### `task-instructions.md`

This file is where the agent workflow is taught today. It is the right place to introduce coordination behavior.

I would add explicit steps such as:

- after claiming a task, register the task in SQLite
- before editing a file, acquire a cooperative file lock
- periodically send heartbeat/status updates during long work
- check inbox/recent warnings before merge
- publish completion or failure notes before moving the task file

Example instruction additions:

- "Before editing any file outside `.tasks/`, run `mato coord lock acquire --path <file>`"
- "If lock acquisition fails, read recent messages and either choose different work or continue only if clearly safe"
- "Before Step 7 merge/push, run `mato coord inbox` and `mato coord locks list` to see whether another agent touched the same area"

#### `recoverOrphanedTasks()` and `cleanStaleLocks()`

These functions currently depend on `.locks` PID files and `claimed-by` comments. They should remain in place initially.

However, the DB gives better recovery signals:

- stale `agent_status.last_heartbeat_at`
- expired `file_locks.expires_at`
- last task event before crash

Phase 1 recommendation:

- keep the existing `.locks` logic as the hard safety net
- add DB cleanup alongside it

Phase 2 recommendation:

- make SQLite heartbeat data the primary recovery signal
- keep `.locks` only as a fallback or remove it later

### Connection pooling

SQLite is not a client/server database; connection strategy matters.

Recommended settings for long-lived host-side `*sql.DB` objects:

```go
db.SetMaxOpenConns(1)
db.SetMaxIdleConns(1)
db.SetConnMaxLifetime(0)
```

Why this is appropriate here:

- `mato` has very low coordination throughput
- multiple pooled connections per process increase write contention for no real gain
- some PRAGMA settings are per-connection, so a single connection is simpler and more predictable

For short-lived `mato coord ...` helper invocations inside containers, each command can:

1. open the DB
2. set PRAGMAs
3. run one short transaction
4. close immediately

That avoids holding SQLite connections open inside autonomous agent sessions.

### WAL mode and startup PRAGMAs

At DB initialization time, execute:

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
```

Notes:

- `journal_mode=WAL` is the most important setting for `mato`
- WAL allows readers to continue while a writer commits
- `synchronous=NORMAL` is a good tradeoff for coordination metadata
- `busy_timeout` prevents immediate `database is locked` failures during brief write contention
- `foreign_keys=ON` keeps table relationships sane

### Docker considerations

The good news is that `mato` is already almost set up for this design.

#### What already works

- `.tasks` is already a bind mount shared across all containers
- containers already run as the host UID/GID, so DB file ownership should line up
- each agent already gets a stable `MATO_AGENT_ID`
- each agent already has access to `/workspace/.tasks`

#### What needs to change

- mount the `mato` binary or a helper binary into the container
- expose `MATO_DB_PATH`
- update the prompt so agents know the helper commands exist

#### Important caveat

SQLite file locking is reliable on normal local Linux filesystems (ext4, xfs, etc.). It can be problematic on some network filesystems. Since `mato` is currently designed around a local bind-mounted repository, that is acceptable, but it should be documented as a constraint.

## 4. Message Types

The database should support a small, opinionated set of message kinds rather than a free-for-all.

### 1. `status`

Used for phase changes and progress updates.

Examples:

- `claimed task`
- `running go test ./...`
- `rebasing against mato`
- `waiting on file lock for main.go`

These are useful for observability and for other agents deciding whether to avoid overlapping work.

### 2. `file_claim`

Used when an agent intends to edit a specific file or directory. In most cases the `file_locks` table is the canonical source, but emitting a message as well gives a searchable history.

Examples:

- `claiming main.go`
- `claiming README.md`
- `releasing pkg/client/http.go`

### 3. `warning`

Used when an agent discovers a risk another agent should know about.

Examples:

- `main.go has active concurrent edits`
- `go test ./... is failing on baseline`
- `merge conflict likely in task-instructions.md`
- `origin/mato moved while I was rebasing`

### 4. `request`

Used for directed asks between agents.

Examples:

- `please avoid touching main.go for the next 10 minutes`
- `I changed task file metadata format; re-read before parsing`
- `can you verify your branch still merges after my refactor`

This is optional in v1 but valuable if `mato` grows beyond simple independent tasks.

### 5. `completion`

Used when an agent finishes and wants to advertise the resulting branch / commit.

Examples:

- `completed task/add-retry-logic at abc1234`
- `merged into mato successfully`

### 6. `handoff`

Used when an agent is blocked or intentionally leaving notes for recovery/retry.

Examples:

- `tests fail because fixture setup is broken; next agent should inspect main_test.go`
- `lock expired while container was interrupted; safe to retry from commit abc1234`

### 7. `error`

Used for unrecoverable failures or repeated retry conditions.

Examples:

- `failed to push after 3 retries`
- `could not resolve merge safely`
- `database busy beyond timeout during lock acquisition`

### Suggested message shape

The `messages` table should keep a compact common shape:

- `kind`
- `severity`
- `task_id`
- `from_agent_id`
- optional `to_agent_id`
- optional `file_path`
- `body`
- `payload_json`

The `payload_json` field gives flexibility for structured extras without forcing a schema change for every new detail.

Examples:

```json
{"branch":"task/add-retry-logic","commit":"abc1234"}
```

```json
{"command":"go test ./...","result":"failed","summary":"2 tests failing"}
```

## 5. Concurrency & Reliability

### SQLite locking behavior

SQLite allows many concurrent readers, but only one writer at a time per database file. That sounds restrictive, but it is acceptable for `mato` because the writes are tiny:

- heartbeat update
- insert message
- acquire/release file lock
- update task status

These should all complete in milliseconds if transactions stay short.

### Why WAL mode matters

Without WAL, writes block readers more aggressively. With WAL:

- readers can continue using the last committed snapshot
- writers append to the WAL file
- contention is much lower for the read-heavy coordination workload

For `mato`, WAL is effectively mandatory.

### Busy timeout

Set a busy timeout of roughly 3-5 seconds on each connection. This prevents spurious failures when two agents try to write at nearly the same moment.

If a write still times out:

- helper command should return a clear error
- agent should sleep with jitter and retry a small number of times
- if still failing, write a failure note to the task file and continue cautiously or abort depending on operation importance

Suggested retry policy for helper commands:

- retry 3 times
- exponential backoff starting at 100-250ms
- random jitter to avoid synchronized retries

### Multiple writers

Do not try to hold long transactions while the agent thinks, edits files, or runs tests.

Good pattern:

- open transaction
- perform one state change
- commit immediately

Bad pattern:

- begin transaction
- run shell commands
- keep transaction open through the task

Only short write transactions will scale.

### Transaction isolation

SQLite gives snapshot isolation semantics for reads. For coordination operations that must be atomic, use explicit short transactions.

#### File lock acquisition

Use `BEGIN IMMEDIATE` for lock acquisition so contention is surfaced immediately and only one agent can claim the row safely.

Recommended algorithm:

1. `BEGIN IMMEDIATE`
2. delete expired lock row for the target path
3. attempt `INSERT INTO file_locks ...`
4. if insert succeeds, commit
5. if insert fails because row exists, rollback and report lock held by another agent

This keeps lock acquisition deterministic.

#### Task state updates

When synchronizing task metadata with queue transitions:

- task claim -> one transaction
- task completion -> one transaction
- task failure / retry -> one transaction

The filesystem move remains the real claim mechanism. The DB write should happen immediately after the move so the metadata is close to the source of truth.

### Heartbeats and stale records

Recommended heartbeat policy:

- agent updates `agent_status.last_heartbeat_at` every 10-15 seconds while active
- file locks get TTLs of 2-10 minutes depending on expected task size
- helper command `lock refresh` updates `refreshed_at` and `expires_at`
- recovery loop deletes expired file locks

Staleness rules:

- if no heartbeat for >30 seconds, mark agent suspicious
- if no heartbeat for >2 poll intervals and PID lock is also absent, treat as dead
- expired file locks can be removed automatically

### Crash safety

SQLite itself is crash-safe for committed transactions. If a container dies mid-task:

- committed messages and locks remain
- uncommitted changes disappear cleanly
- stale locks are cleaned up by TTL
- task file still exists in `.tasks/in-progress/`
- existing `recoverOrphanedTasks()` logic can move it back to backlog
- recovery can write an additional DB `task_event` explaining what happened

That is a strong operational story.

## 6. Pros

### Strongest advantages for `mato`

1. **Structured queries instead of scraping files**
   - Agents can ask precise questions like "who holds `main.go`?" or "show warnings for my task".

2. **Fits the current `.tasks` bind-mount architecture**
   - No new server process is required.
   - No extra network port or service discovery is needed.

3. **Pure Go implementation is easy to ship**
   - `modernc.org/sqlite` keeps the binary self-contained.

4. **WAL mode handles the expected concurrency level well**
   - `mato` has a small number of agents and tiny metadata writes.

5. **Much better observability**
   - `agent_status` and `task_events` provide a real audit trail.

6. **Reduces merge conflicts**
   - Cooperative `file_locks` let agents advertise edit intent early.

7. **Backward-compatible rollout path**
   - Keep queue claiming and PID locks as-is while layering DB coordination on top.

8. **Searchable history for future tooling**
   - A future `mato status` or `mato inspect task <id>` command becomes easy.

## 7. Cons

1. **Still single-writer at the database level**
   - SQLite is excellent here, but it is not infinite-write-scale infrastructure.
   - If `mato` eventually runs dozens of agents with frequent writes, this may become a bottleneck.

2. **More implementation surface area than plain files**
   - schema migrations
   - helper commands
   - testing concurrency cases
   - stale lock cleanup

3. **Agents need a disciplined interface**
   - Direct SQL from LLM-driven agents would be brittle.
   - A helper CLI is practically required.

4. **Potential confusion from dual sources of truth**
   - queue state still lives in the filesystem
   - coordination state lives in SQLite
   - code must clearly define that filesystem moves win if they disagree

5. **SQLite on network filesystems can be risky**
   - acceptable for local bind mounts
   - less safe if users point `mato` at NFS/SMB-backed repos

6. **TTL-based locks are cooperative, not absolute**
   - they reduce conflicts but cannot fully prevent reckless or buggy agents from editing the same file

7. **Prompt complexity increases**
   - `task-instructions.md` becomes longer because it must teach agents when to query, lock, refresh, and publish

## 8. Complexity Estimate

### Suggested rollout phases

### Phase 1: basic shared state (small / medium)

Scope:

- add SQLite dependency
- create/migrate DB
- add `agent_status`, `messages`, and `task_events`
- mount `mato` helper binary into container
- expose `mato coord heartbeat`, `mato coord message send`, `mato coord inbox`
- add prompt instructions to use them

Estimate:

- ~250-400 lines of production Go
- ~100-200 lines of tests
- about **1-2 focused development days**

### Phase 2: file claims / locks (medium)

Scope:

- add `file_locks`
- implement acquire / refresh / release commands
- add TTL cleanup
- teach agents to claim files before editing

Estimate:

- additional ~150-250 lines of Go
- additional ~100-150 lines of tests
- about **1 more day**

### Phase 3: deeper recovery integration (medium / large)

Scope:

- tie heartbeat staleness into orphan recovery
- reduce reliance on `.locks`
- add richer `mato status` / inspection commands

Estimate:

- additional ~150-300 lines of Go
- additional ~100-200 lines of tests
- about **1-2 more days**

### Overall estimate

For a solid first implementation that is actually usable by autonomous agents, I would budget:

- **~400-700 lines of Go changes**
- **~200-400 lines of tests**
- **~3-5 development days** including prompt iteration and concurrency testing

The technically hard part is **not SQLite itself**. The hard part is defining a small, reliable coordination contract that autonomous agents will actually follow consistently.

## Recommended conclusion

For `mato`, a shared SQLite database is a very plausible inter-agent communication mechanism and probably the best next step if the goal is richer coordination without introducing a separate service.

The safest implementation strategy is:

- keep filesystem task claiming exactly as it is
- add `.tasks/coordination.db` as a shared control plane
- use `modernc.org/sqlite`
- run in WAL mode with short transactions and busy timeouts
- expose helper subcommands through the `mato` binary rather than expecting agents to write raw SQL
- roll out heartbeats/messages first, then file locks, then deeper recovery integration

That gives `mato` real inter-agent communication while staying close to its current single-binary, bind-mounted, Docker-driven architecture.
