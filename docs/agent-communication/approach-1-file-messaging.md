# Approach 1: File-Based Messaging for `mato`

## 1. Overview

`mato` already has the key primitive needed for inter-agent communication: every Dockerized agent sees the same shared `.tasks/` directory at `/workspace/.tasks` because `runOnce()` bind-mounts the host task directory into the container.

That makes a **file-based message bus** the most natural first communication mechanism.

The core idea is:

- agents continue to claim work exactly as they do now from `.tasks/backlog/`
- agents publish small immutable message files into a shared `.tasks/messages/` area
- other agents discover new messages by scanning that directory and tracking a per-agent cursor
- agents also publish lightweight presence/state files so peers can see who is active and what they appear to be touching
- `mato` itself performs directory initialization and garbage collection, but the actual message exchange remains filesystem-only

This keeps the existing architecture intact:

- no network service
- no database
- no container-to-container sockets
- no Docker networking changes
- no dependency on the agents being able to call back into the host

In practice, this would give agents a way to say things like:

- “I claimed work that will touch `pkg/client/http.go` and `pkg/client/retry.go`.”
- “I found a bug in `parseArgs()` while working on an unrelated task.”
- “I merged task `add-retry-logic.md`; downstream work should rebase and re-read `pkg/client/http.go`.”

The mechanism should be treated as **advisory coordination**, not as the source of truth for task ownership. The source of truth for task ownership remains the existing atomic file move from `backlog/` to `in-progress/`.

---

## 2. Architecture

### 2.1 Proposed directory structure

Today `run()` creates:

```text
.tasks/
├── backlog/
├── in-progress/
├── completed/
├── failed/
└── .locks/
```

Add a new messaging subtree:

```text
.tasks/
├── backlog/
├── in-progress/
├── completed/
├── failed/
├── .locks/
└── messages/
    ├── events/          # immutable message files, append-only
    ├── presence/        # one JSON file per active agent
    ├── cursors/         # last-read marker per agent
    ├── archive/         # optional soft-deleted/expired messages
    └── .gc.lock         # best-effort cleanup lock
```

### 2.2 Directory responsibilities

#### `messages/events/`
Contains the actual messages. Each message is written once and never modified.

Example filenames:

```text
20260314T120102.123456789Z-a1b2c3d4-work-claim-7f91.json
20260314T120118.887654321Z-e5f6a7b8-finding-31cc.json
20260314T120305.000000100Z-a1b2c3d4-task-complete-5521.json
```

Properties:

- lexicographically sortable by creation time
- includes sender agent ID
- includes message type in filename for easier debugging
- includes random suffix to avoid collisions

#### `messages/presence/`
Contains one file per agent, for example:

```text
.tasks/messages/presence/a1b2c3d4.json
```

This is a mutable status file describing the agent’s latest known state, such as:

- agent ID
- current status (`idle`, `claiming-task`, `working`, `merging`, `exiting`)
- current task filename
- current branch
- declared files/modules being touched
- last updated timestamp

This gives agents a cheap “who is alive and what are they doing?” view without forcing them to replay the whole event stream.

#### `messages/cursors/`
Contains one file per agent tracking the newest event file it has fully processed.

Example:

```text
.tasks/messages/cursors/a1b2c3d4.cursor
```

Contents can be as simple as the last processed filename:

```text
20260314T120305.000000100Z-a1b2c3d4-task-complete-5521.json
```

This lets an agent restart, rescan, and continue from where it left off.

#### `messages/archive/`
Used by cleanup code to move expired messages out of `events/` before hard deletion. This is optional, but useful during rollout because it preserves observability.

### 2.3 Message format

Use JSON for the on-disk wire format. Even though agents are LLM-driven, JSON is:

- easy for Go to validate and clean up
- easy for humans to inspect
- easy for agents to emit with a heredoc
- explicit enough to support future tooling

Recommended schema:

```json
{
  "id": "20260314T120102.123456789Z-a1b2c3d4-7f91",
  "type": "work-claim",
  "from": "a1b2c3d4",
  "audience": "broadcast",
  "created_at": "2026-03-14T12:01:02.123456789Z",
  "expires_at": "2026-03-15T12:01:02.123456789Z",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "target_branch": "mato",
  "files": [
    "pkg/client/http.go",
    "pkg/client/retry_test.go"
  ],
  "modules": [
    "pkg/client"
  ],
  "summary": "Starting retry work in HTTP client",
  "body": "I expect to change fetchData retry behavior and related tests.",
  "related_message_id": "",
  "metadata": {
    "severity": "info"
  }
}
```

Minimum required fields for MVP:

- `id`
- `type`
- `from`
- `created_at`
- `task_file`
- `summary`

Strongly recommended fields:

- `expires_at`
- `files`
- `modules`
- `task_branch`
- `target_branch`
- `related_message_id`

### 2.4 How agents discover and read messages

A containerized agent should follow this pattern:

1. Read `/workspace/.tasks/messages/presence/` to see active peers.
2. Read its cursor from `/workspace/.tasks/messages/cursors/$MATO_AGENT_ID.cursor` if it exists.
3. List `/workspace/.tasks/messages/events/*.json` sorted lexicographically.
4. Skip files at or before the cursor.
5. Read newer messages.
6. Filter to relevant ones:
   - same task
   - overlapping files
   - overlapping module paths
   - broadcast findings
   - recent completion notices that may invalidate local assumptions
7. Update cursor after processing.

This is simple, debuggable, and works with the current “agents are just autonomous Copilot sessions in containers” model.

---

## 3. Implementation Details

## 3.1 `parseArgs()` changes

Current `parseArgs()` only returns:

- repo root
- target branch
- passthrough Copilot args

For messaging, I would refactor it to return a config struct instead of a raw tuple. This is the right place to add messaging knobs without making `main()` and `run()` signatures balloon further.

Suggested config additions:

```go
type config struct {
    repoRoot            string
    branch              string
    copilotArgs         []string
    messagingEnabled    bool
    messageTTL          time.Duration
    messageArchiveTTL   time.Duration
    messageReadWindow   time.Duration
}
```

Suggested CLI flags:

- `--messages` / `--messages=true|false`  
  Enable file messaging. Default can be `true` once stable, but I would initially roll it out default-on only after tests and prompt tuning.
- `--message-ttl <duration>`  
  How long messages stay live in `events/` before archival. Example default: `24h`.
- `--message-archive-ttl <duration>`  
  How long archived messages remain before hard deletion. Example default: `168h` (7 days).
- `--message-read-window <duration>`  
  How far back an agent should read on cold start if it has no cursor. Example default: `6h`.

If keeping the current tuple return shape is preferred for now, then add the new values directly to the return list. That will work, but a config struct is cleaner for this codebase because `runOnce()` already has a long parameter list.

## 3.2 `run()` changes

`run()` is the main orchestration point and should own most of the host-side messaging setup.

### Add directory initialization

Today `run()` creates `backlog`, `in-progress`, `completed`, and `failed`. Extend that to also create:

- `.tasks/messages/`
- `.tasks/messages/events/`
- `.tasks/messages/presence/`
- `.tasks/messages/cursors/`
- `.tasks/messages/archive/`

Suggested helper:

```go
func ensureTasksLayout(tasksDir string, messagingEnabled bool) error
```

### Add prompt construction for messaging instructions

Right now `run()` builds a prompt by replacing placeholders in embedded `task-instructions.md`.

That file should gain a new section explaining:

- where messages live
- when the agent must read them
- when the agent should publish them
- how to update its presence file
- that messaging is advisory and does not replace task claiming

I would not bury this as optional prose. I would integrate it into the existing workflow, likely between current Step 4 (create branch) and Step 5 (work on task), and again before Step 7 (merge/push).

Suggested prompt-building helper:

```go
func buildPrompt(baseInstructions, tasksDir, branch string, cfg config) string
```

This could append a messaging section only when messaging is enabled.

### Add message cleanup and stale presence cleanup

Inside the main polling loop, `run()` already calls:

- `recoverOrphanedTasks(tasksDir)`
- `cleanStaleLocks(tasksDir)`

Add:

- `cleanStalePresence(tasksDir)`
- `archiveExpiredMessages(tasksDir, now, cfg.messageTTL)`
- `deleteArchivedMessages(tasksDir, now, cfg.messageArchiveTTL)`

Important detail: multiple `mato` processes may attempt cleanup concurrently. Cleanup should therefore use a best-effort GC lock file such as `.tasks/messages/.gc.lock`, acquired by atomic create or rename. If a process cannot acquire it, it simply skips cleanup for that loop.

Suggested helpers:

```go
func cleanStalePresence(tasksDir string)
func archiveExpiredMessages(tasksDir string, now time.Time, ttl time.Duration)
func deleteArchivedMessages(tasksDir string, now time.Time, ttl time.Duration)
func tryWithGCLock(tasksDir string, fn func())
```

## 3.3 `runOnce()` changes

`runOnce()` already mounts the critical shared path:

- `cloneDir -> /workspace`
- `tasksDir -> /workspace/.tasks`

That means **no new shared data mount is strictly required** for file messaging. This is a major advantage of this approach.

Recommended changes:

### Add environment variables

Pass explicit env vars to the container so the prompt and any helper scripts can rely on stable names:

- `MATO_MESSAGES_DIR=/workspace/.tasks/messages`
- `MATO_MESSAGE_TTL=<duration>`
- `MATO_MESSAGE_READ_WINDOW=<duration>`
- `MATO_TARGET_BRANCH=<branch>`

These are not strictly necessary because the paths are derivable, but they reduce prompt ambiguity.

### Optional helper script mount

For higher reliability, ship a small helper script or tiny Go helper binary that lives either:

1. under `.tasks/bin/` so it rides the existing bind mount, or
2. as an additional read-only bind mount into `/usr/local/bin/mato-msg`

Example commands:

```bash
mato-msg publish --type work-claim --task add-retry-logic.md --files pkg/client/http.go,pkg/client/retry_test.go --summary "Starting retry work"
mato-msg read
mato-msg presence --status working --task add-retry-logic.md --branch task/add-retry-logic --files pkg/client/http.go
```

I would treat this helper as optional but strongly recommended. Without it, agents can still write JSON files directly, but the consistency of message formatting will depend more on prompt-following quality.

## 3.4 New Go types and functions

Recommended additions in `main.go` (or a new `messaging.go` if the project is ready to split files):

```go
type message struct {
    ID               string            `json:"id"`
    Type             string            `json:"type"`
    From             string            `json:"from"`
    Audience         string            `json:"audience,omitempty"`
    CreatedAt        time.Time         `json:"created_at"`
    ExpiresAt        time.Time         `json:"expires_at,omitempty"`
    TaskFile         string            `json:"task_file,omitempty"`
    TaskBranch       string            `json:"task_branch,omitempty"`
    TargetBranch     string            `json:"target_branch,omitempty"`
    Files            []string          `json:"files,omitempty"`
    Modules          []string          `json:"modules,omitempty"`
    Summary          string            `json:"summary"`
    Body             string            `json:"body,omitempty"`
    RelatedMessageID string            `json:"related_message_id,omitempty"`
    Metadata         map[string]string `json:"metadata,omitempty"`
}

type presence struct {
    AgentID     string    `json:"agent_id"`
    Status      string    `json:"status"`
    TaskFile    string    `json:"task_file,omitempty"`
    TaskBranch  string    `json:"task_branch,omitempty"`
    Files       []string  `json:"files,omitempty"`
    Modules     []string  `json:"modules,omitempty"`
    UpdatedAt   time.Time `json:"updated_at"`
}
```

Suggested functions:

```go
func ensureMessagingLayout(tasksDir string) error
func buildPrompt(baseInstructions, workdir, branch string, cfg config) string
func writePresenceFile(tasksDir string, p presence) error
func cleanStalePresence(tasksDir string)
func archiveExpiredMessages(tasksDir string, now time.Time, ttl time.Duration)
func deleteArchivedMessages(tasksDir string, now time.Time, ttl time.Duration)
func nextMessageFilename(now time.Time, agentID, typ string) string
func writeJSONAtomically(path string, v any) error
func tryWithGCLock(tasksDir string, fn func())
```

Notably, `mato` itself does **not** need to become the broker. It only needs to initialize the directories, pass configuration, and clean up old state.

## 3.5 Changes to `task-instructions.md`

This is the most important non-Go change.

The current prompt tells the agent how to:

- find tasks
- claim them
- branch
- work
- commit
- merge
- mark complete

To make messaging real, the prompt should explicitly require these new actions:

### After claiming and creating a branch

- read recent messages from `/workspace/.tasks/messages/events/`
- inspect active peers in `/workspace/.tasks/messages/presence/`
- publish a `work-claim` message describing likely files/modules to be touched
- write/update its presence file

### During implementation

- if the affected file list changes materially, publish a `work-update`
- if a bug or design hazard is discovered that other agents should know about, publish a `finding`
- before touching a file already claimed by another active agent, read that agent’s recent messages first

### Before merge/push

- re-read recent `task-complete` and `finding` messages to detect stale assumptions

### On success

- publish `task-complete`
- update presence to `exiting` or remove the presence file

### On failure

- publish `handoff` or `blocked`
- remove or mark stale the presence file

Because agents are autonomous, this prompt text must be concrete. It should include exact shell snippets or a helper command, not just conceptual guidance.

## 3.6 Docker mount changes

Strictly speaking, none are required for the shared message data itself because this line already does the job:

```go
"-v", fmt.Sprintf("%s:%s/.tasks", tasksDir, workdir),
```

That mount exposes the entire `.tasks/` subtree, including any new `messages/` directory, to every container.

So the Docker changes are minimal:

- **required:** none for data sharing
- **recommended:** add message-related env vars
- **optional:** mount a helper CLI/script if one is introduced

That is a strong argument in favor of this approach.

## 3.7 Message lifecycle

### Create

1. Agent decides to publish a message.
2. Agent generates a filename-safe ID.
3. Agent writes JSON to a temp file in the same filesystem.
4. Agent renames temp file into `messages/events/`.
5. Agent updates its presence file if applicable.

Creation must use temp-file-then-rename so readers never see partial JSON.

### Read

1. Agent loads cursor.
2. Agent lists event files sorted by filename.
3. Agent reads files newer than cursor.
4. Agent filters by relevance.
5. Agent stores the latest processed filename as its cursor.

### Expire / Cleanup

Recommended lifecycle:

- active/live in `events/` for 24h
- then moved to `archive/`
- then hard-deleted after 7 days

During early rollout, I would prefer archiving over immediate delete because it helps debug agent behavior.

---

## 4. Message Types

The following message types are useful for `mato` specifically.

### 4.1 `work-claim`

Purpose: announce likely file/module ownership before major edits begin.

Use when:

- an agent has claimed a task and inspected the repo
- it has a decent guess about which files it will touch

Example:

- “Working on `add-retry-logic.md`; expect changes in `pkg/client/http.go` and `pkg/client/retry_test.go`.”

### 4.2 `work-update`

Purpose: refine or expand a previous claim.

Use when:

- initial assumptions changed
- the agent discovered additional files/modules it now needs
- the agent is backing off a previously claimed area

Example:

- “No longer touching `pkg/client/http.go`; work moved to `pkg/retry/policy.go`.”

### 4.3 `finding`

Purpose: broadcast a discovery relevant to other tasks.

Use when:

- the agent finds a bug outside its direct task
- the agent discovers an invariant or architecture constraint
- the agent discovers a known-dangerous file or API boundary

Example:

- “`parseArgs()` forwards unknown flags directly to Copilot. Any new mato flags must be intercepted before passthrough.”

This is particularly valuable in this codebase because `main.go` currently centralizes most orchestration logic, so unrelated tasks may still overlap in the same file.

### 4.4 `blocked`

Purpose: signal that progress is blocked by another task, missing prerequisite, or merge hazard.

Use when:

- another active agent is editing the same high-risk file
- a required refactor is underway elsewhere
- proceeding would likely cause unnecessary conflict

Example:

- “Blocked on `main.go` argument parsing changes from agent `e5f6a7b8`; waiting or rerouting work.”

### 4.5 `handoff`

Purpose: leave actionable notes for a future retry or follow-up agent.

Use when:

- the current task is failing
- the agent is about to move the task back to backlog
- partial investigation would help the next run

Example:

- “Tests fail because `docker run` argument construction needs refactor around env var injection; see attempted changes in `runOnce()`.”

### 4.6 `task-complete`

Purpose: tell other active agents that the repo state has changed in a meaningful way.

Use when:

- the task has been merged to target branch
- the agent knows which files/modules changed

Example:

- “Completed `add-retry-logic.md`; merged to `mato`; downstream agents touching `pkg/client/http.go` should fetch/rebase.”

### 4.7 `merge-risk`

Purpose: warn peers about files likely to conflict.

Use when:

- the agent is making broad edits in `main.go`
- it is changing shared build/test/config files
- it expects a merge conflict if others proceed concurrently

This is especially relevant for `mato`, where many features converge in `main.go`.

### 4.8 `agent-online` / `agent-offline` (optional system messages)

These can be emitted by the host process rather than by the Copilot agent. They are not strictly required if presence files exist, but they can make the event log easier to audit.

---

## 5. Concurrency & Reliability

## 5.1 Atomic writes

All message and presence writes should use:

1. write to temp file in same directory
2. `fsync` if desired
3. `os.Rename()` to final filename

Why this matters:

- readers never see partial JSON
- rename is atomic on the same filesystem
- this matches the existing mental model already used by task claiming (`mv` from backlog to in-progress)

Readers should ignore:

- dotfiles
- `*.tmp`
- zero-byte files
- invalid JSON files (log and skip)

## 5.2 Race conditions

### Two agents publishing at the same time

Safe, as long as filenames are unique. Use:

- RFC3339Nano timestamp
- sender agent ID
- random suffix

### One agent reading while another writes

Safe, if writes are rename-based and readers ignore temp files.

### Multiple hosts cleaning up expired messages

Use a GC lock file in `.tasks/messages/.gc.lock`. Cleanup is best-effort, so if lock acquisition fails the process should skip cleanup for that polling cycle.

## 5.3 Message ordering

There is no perfect global order unless a central broker assigns sequence numbers. For a file-based MVP, ordering should be **best effort**:

- primary order: lexicographic filename sort
- secondary order: `created_at`
- tie-break: full message ID

That is good enough for coordination messages. Agents should not assume strict total ordering across all peers.

The protocol should be documented as:

- messages are immutable
- ordering is approximate
- agents must tolerate seeing related messages slightly out of order

## 5.4 Cursor handling and idempotence

An agent may crash after reading a message but before updating its cursor. That means it may re-read some messages on restart.

That is acceptable if message handling is idempotent.

Examples:

- reading the same `finding` twice is harmless
- reading the same `task-complete` twice is harmless
- writing the same `work-claim` twice is noisy but not dangerous if IDs are unique

The design should therefore prefer “at least once observation” over trying to guarantee exactly-once delivery.

## 5.5 Stale presence files

Presence files can go stale if a container or host process dies unexpectedly.

Cleanup strategy:

- if `.tasks/.locks/<agentID>.pid` is gone or the PID is dead, remove that agent’s presence file
- if `updated_at` is older than a conservative threshold (for example 30 minutes), treat the presence file as stale even if cleanup has not run yet

Agents reading presence should treat it as advisory, not authoritative.

## 5.6 Cleanup of stale messages

Recommended cleanup rules:

- expire messages based on `expires_at` if present
- otherwise expire based on `created_at + messageTTL`
- move to `archive/` first
- hard-delete archived messages after `archiveTTL`

A slightly safer rule during MVP rollout is:

- do not archive messages newer than the oldest active agent cursor window plus safety margin

But that requires more bookkeeping. For a first implementation, TTL-based archival is enough.

## 5.7 Malformed messages

Because agents are LLM-driven, some malformed files will inevitably appear.

The system should be resilient:

- malformed message in `events/` should not break scanning
- cleanup code should log invalid files and either quarantine or archive them
- agents should be instructed to keep messages small and schema-compliant

A helper CLI would reduce this risk substantially.

---

## 6. Pros

1. **Fits the current architecture perfectly.**  
   `mato` already shares `.tasks/` into every container. Messaging rides the same path.

2. **No extra infrastructure.**  
   No Redis, no HTTP broker, no sockets, no service discovery.

3. **Easy to debug.**  
   Operators can inspect `.tasks/messages/events/` directly with ordinary shell tools.

4. **Resilient to restarts.**  
   Messages are just files; an agent can restart and catch up from cursor state.

5. **Low operational risk.**  
   The host process does not become a message router. It only initializes directories and performs cleanup.

6. **Auditable history.**  
   Archived messages create a paper trail explaining why agents made certain decisions.

7. **Advisory coordination without removing existing safety.**  
   Task claiming remains atomic via file move; messaging only improves awareness.

8. **Good match for autonomous Copilot agents.**  
   Agents already reason over files and shell commands, so file messages are a natural interface.

---

## 7. Cons

1. **No push delivery.**  
   Agents must poll the directory; they do not receive instant notifications.

2. **Ordering is approximate.**  
   Without a broker, there is no strict global sequence.

3. **Directory scans can grow expensive.**  
   If many messages accumulate, naive scans become slower. Cleanup matters.

4. **LLM protocol compliance is imperfect.**  
   Agents may forget to publish, publish malformed JSON, or over-communicate.

5. **Advisory, not enforceable.**  
   A `work-claim` cannot prevent another agent from editing the same file.

6. **Single shared filesystem assumption.**  
   This works because all containers share the same bind mount. It would not naturally extend to distributed hosts without a shared volume.

7. **Cleanup and stale-state handling add complexity.**  
   Presence, cursors, archival, and malformed-file handling all need discipline.

8. **Potential noise.**  
   If prompt instructions are too loose, agents may flood the bus with low-value messages.

---

## 8. Complexity Estimate

I would classify this as **low-to-medium complexity** for `mato`.

### MVP scope

Includes:

- new `.tasks/messages/` layout
- config/flag plumbing
- prompt updates
- presence files
- append-only event files
- cursor files
- TTL cleanup
- unit tests

Estimated code impact:

- **Go code:** ~250-400 lines
- **tests:** ~150-250 lines
- **prompt/docs changes:** ~50-120 lines

Estimated effort:

- **1-2 days** for a rough but working MVP
- **3-5 days** for a polished implementation with helper CLI/script, stronger tests, and prompt tuning

### Suggested implementation phases

#### Phase 1: Core plumbing

- add config fields / flags
- create messaging directories
- add env vars
- add cleanup helpers
- update prompt with explicit message workflow

#### Phase 2: Agent-facing usability

- add `mato-msg` helper script or binary
- standardize message creation and cursor updates
- reduce malformed message risk

#### Phase 3: Hardening

- archive/quarantine malformed messages
- add integration tests around concurrent writes and GC lock behavior
- tune prompt text based on real runs

### Testing impact

`main_test.go` already covers argument parsing, lock handling, and orphan recovery. Messaging would need analogous tests for:

- directory initialization
- atomic JSON writes
- presence cleanup when `.locks` go stale
- message archival and deletion
- filename ordering
- cursor resume behavior
- malformed message tolerance

Because `main.go` is still a single-file program, this feature is also a good forcing function to split the code into smaller files such as:

- `main.go`
- `queue.go`
- `messaging.go`
- `docker.go`

That refactor is not required for the feature, but it would make ongoing changes much safer.

---

## Recommendation

File-based messaging is the best first inter-agent communication mechanism for `mato` because it leverages infrastructure the program already has:

- shared `.tasks/`
- agent IDs
- lock files
- polling loop
- Docker bind mounts

I would implement it as an **append-only event log plus per-agent presence and cursor files**, keep it advisory, and make the host responsible only for setup and cleanup.

That gives `mato` meaningful coordination with minimal architectural disruption.