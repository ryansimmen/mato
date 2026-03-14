# Approach 3: Git-based inter-agent communication for mato

## 1. Overview

`mato` already has almost everything needed to use Git as an inter-agent message bus:

- `runOnce()` creates an isolated temporary clone for each agent.
- The clone’s `origin` points at the shared host repo.
- The Docker container already has `git`, `git-upload-pack`, and `git-receive-pack` mounted in.
- Agents already `fetch`, `merge`, and `push` to the shared target branch.
- All agents ultimately rendezvous through the same origin repository, even though they work in separate clones.

The proposal is to keep the existing filesystem task queue (`.tasks/backlog`, `.tasks/in-progress`, etc.) for task claiming, but add a **Git-backed coordination channel** for agent-to-agent communication.

### Recommended design

Use a **custom ref namespace** as the primary transport:

- Messages are written to refs under `refs/mato/messages/...`.
- Each message is stored as an immutable Git commit object.
- Agents publish by creating a new unique ref and pushing it to `origin`.
- Agents consume by fetching the namespace and listing/parsing refs.

This gives mato a lightweight mailbox without introducing Redis, a database, sockets, or a long-running coordinator.

### Why refs instead of notes or a dedicated branch?

- **Custom refs are the best primary mechanism** for mato because they avoid merge conflicts entirely when each message gets its own unique ref.
- **Git notes are useful as a secondary feature** for annotating final task commits, but they are awkward for inbox-style communication because notes must attach to an existing object.
- **A dedicated communication branch is possible**, but it becomes a hot spot: concurrent agents would all need to append commits to the same branch and handle frequent non-fast-forward retries.

So the recommended implementation is:

1. **Primary bus:** immutable message refs under `refs/mato/messages/...`
2. **Optional audit add-on:** `refs/notes/mato` notes attached to final task merge commits
3. **Not recommended as primary:** append-only message files on a shared branch

---

## 2. Architecture

## Storage model in Git

Each message becomes:

1. a small commit object containing structured metadata + message body
2. referenced by a unique ref in a custom namespace

### Ref layout

```text
refs/mato/messages/all/<timestamp>-<agentID>-<nonce>
refs/mato/messages/agent/<recipient>/<timestamp>-<agentID>-<nonce>
refs/mato/messages/task/<task-name>/<timestamp>-<agentID>-<nonce>
refs/mato/messages/branch/<branch-name>/<timestamp>-<agentID>-<nonce>
```

Examples:

```text
refs/mato/messages/all/20260302T101530Z-a1b2c3d4-6f09a7c1
refs/mato/messages/agent/ff91aa22/20260302T101615Z-a1b2c3d4-e2c1b8f0
refs/mato/messages/task/add-retry-logic/20260302T101700Z-a1b2c3d4-903e7c22
```

This layout makes it easy to support:

- broadcast messages (`all`)
- direct messages to a specific agent (`agent/<id>`)
- task-scoped coordination (`task/<sanitized-task>`)
- branch-scoped coordination for merge/rebase chatter (`branch/<task-branch>`)

### Message object format

The ref should point to a **commit object**, not a blob.

Reasons:

- commit objects already carry author/committer identity and timestamps
- `git show`, `git log`, and `git cat-file` work naturally
- history is auditable and human-debuggable
- pushes/fetches of commits are a normal Git workflow

The commit can use the empty tree and put the structured payload in the commit message.

Suggested commit message format:

```text
mato-msg: intent

id: 20260302T101530Z-a1b2c3d4-6f09a7c1
from: a1b2c3d4
to: all
type: intent
task: add-retry-logic.md
branch: task/add-retry-logic
target-branch: mato
base: 1a2b3c4d5e6f
files: pkg/client/http.go,pkg/client/http_test.go
sent-at: 2026-03-02T10:15:30Z

I am updating retry behavior in pkg/client/http.go and adding retry tests.
Please avoid overlapping edits in pkg/client/http_test.go for the next few minutes.
```

This is intentionally line-oriented instead of JSON so agents can read it with plain shell tools and mato can parse it with `strings.Split`.

### Optional notes usage

After a task is merged, mato can optionally write a note to the resulting commit in `refs/notes/mato` containing:

- task filename
- agent ID
- summary of communication messages associated with the task
- any handoff or blocked/unblocked events

That gives a durable post-hoc audit trail directly on the task’s commit, without making notes the primary delivery mechanism.

### Why not a communication branch?

A branch such as `mato-comms` with one file per message would work, but it creates unnecessary contention:

- every publish modifies the same branch tip
- concurrent agents will race on push
- agents must fetch/rebase/merge just to append a message
- message transport becomes coupled to conflict resolution

That is the opposite of what mato needs. Today mato already has enough merge pressure on the target branch. The comms channel should be **append-only and conflict-free**.

---

## 3. Implementation Details

## A. Go code changes

The cleanest implementation is to add a small Git communication layer in new files instead of continuing to grow `main.go`.

### New files / types

Suggested files:

- `comm.go` — types and interface
- `comm_git.go` — Git ref implementation
- `comm_test.go` — repo-level tests using temporary Git repos

Suggested types:

```go
type CommConfig struct {
    RefRoot        string
    NotesRef       string
    Retention      time.Duration
    PollWindow     time.Duration
    EnableNotes    bool
}

type Message struct {
    ID           string
    From         string
    To           string
    Type         string
    Task         string
    Branch       string
    TargetBranch string
    BaseSHA      string
    Files        []string
    SentAt       time.Time
    Body         string
    Ref          string
    Commit       string
}
```

Suggested helper functions:

```go
func defaultCommConfig() CommConfig
func sanitizeRefComponent(s string) string
func ensureCommNamespace(repoRoot string, cfg CommConfig) error
func fetchMessageRefs(repoDir string, cfg CommConfig) error
func publishMessage(repoDir string, cfg CommConfig, msg Message) (string, error)
func listMessages(repoDir string, cfg CommConfig, scopes []string, since time.Time) ([]Message, error)
func parseMessageCommit(refName, commitText string) (Message, error)
func addTaskNote(repoDir string, cfg CommConfig, commitSHA string, note string) error
func pruneOldMessages(repoRoot string, cfg CommConfig, cutoff time.Time) error
```

### Best integration point: mount the mato binary into the container

Today `runOnce()` mounts `copilot`, `git`, and `gh` into the container. Do the same with the running `mato` binary:

- resolve it with `os.Executable()`
- mount it read-only as `/usr/local/bin/mato`

Then add hidden subcommands such as:

```bash
mato comm send ...
mato comm poll ...
mato comm note ...
mato comm prune ...
```

This is a better fit than teaching the LLM raw `git commit-tree` plumbing in the prompt.

It also keeps the hard Git logic in Go, where it can be tested.

### Changes in `main.go`

#### `run()`

After `ensureBranch()` and `ensureGitignored()`:

- initialize `CommConfig`
- ensure the namespace exists conceptually (no repo mutation required, but validate configuration)
- optionally prune old refs every N loops

Potential addition:

```go
cfg := defaultCommConfig()
```

Then inside the main loop, before `hasAvailableTasks(tasksDir)`:

- optionally prune very old refs
- optionally prefetch comm refs into the origin working copy for debugging, though this is not required

#### `runOnce()`

This is the most important insertion point.

After `createClone(repoRoot)`:

1. call `fetchMessageRefs(cloneDir, cfg)` so the clone starts with a current mailbox
2. mount the `mato` binary into the container
3. pass new environment variables into the container:

```text
MATO_COMM_ENABLED=1
MATO_COMM_REF_ROOT=refs/mato/messages
MATO_COMM_NOTES_REF=refs/notes/mato
MATO_TARGET_BRANCH=<branch>
MATO_AGENT_ID=<existing agent id>
```

This is fully aligned with mato’s current execution model:

- clones are temporary
- origin is reachable by local path
- the container already runs Git commands against that origin

### Hidden CLI / subcommand flow

A small subcommand surface is enough:

#### `mato comm send`

Inputs:

- `--type`
- `--to`
- `--task`
- `--branch`
- `--files`
- `--body`

Behavior:

1. determine current base SHA (`git rev-parse origin/<target-branch>` or local HEAD)
2. build message text
3. create empty-tree commit with that message
4. create unique ref name
5. `git update-ref <ref> <commit>`
6. `git push origin <ref>:<ref>`

#### `mato comm poll`

Inputs:

- `--agent`
- `--task`
- `--branch`
- `--since`

Behavior:

1. `git fetch origin "refs/mato/messages/*:refs/mato/messages/*"`
2. list matching refs under:
   - `refs/mato/messages/all/`
   - `refs/mato/messages/agent/<agentID>/`
   - `refs/mato/messages/task/<task>/`
   - `refs/mato/messages/branch/<branch>/`
3. sort by `sent-at`
4. print a compact human-readable summary for the LLM agent

#### `mato comm note`

After successful Step 7, optionally attach a note to the resulting commit on `TARGET_BRANCH`.

### Under-the-hood Git commands

Even if agents use `mato comm`, the underlying Git behavior should be explicit in the design.

#### Publish

```bash
EMPTY_TREE=$(git hash-object -t tree /dev/null)
MSG_ID="$(date -u +%Y%m%dT%H%M%SZ)-${MATO_AGENT_ID}-$(openssl rand -hex 4)"
REF="refs/mato/messages/all/$MSG_ID"
COMMIT=$(printf '%s\n' "$MESSAGE_TEXT" | git commit-tree "$EMPTY_TREE")
git update-ref "$REF" "$COMMIT"
git push origin "$REF:$REF"
```

For task-scoped or direct messages, change the ref path accordingly.

#### Consume

```bash
git fetch origin "refs/mato/messages/*:refs/mato/messages/*"
git for-each-ref --sort=creatordate --format='%(refname) %(objectname)' refs/mato/messages
```

Then read each commit body with:

```bash
git cat-file commit <sha>
```

## B. Updates to `task-instructions.md`

The task prompt is the other major change. Right now it tells agents how to claim tasks, branch, test, merge, and mark complete, but it gives them no coordination behavior.

Add a new mandatory communication step after branching and before substantial edits.

### Proposed prompt additions

#### New Step 4.5: Check Messages

After the agent creates `task/<name>`:

- run `mato comm poll --agent "$MATO_AGENT_ID" --task "<filename>" --branch "<task-branch>" --since 24h`
- review any messages about the same task, overlapping files, or active blockers
- summarize relevant messages before coding

#### New Step 5A: Announce intent

Before editing major files, publish an intent message:

```bash
mato comm send \
  --type intent \
  --to all \
  --task "<filename>" \
  --branch "<task-branch>" \
  --files "pkg/client/http.go,pkg/client/http_test.go" \
  --body "Starting implementation; planning retry loop and tests."
```

#### New Step 5B: Use messages while blocked

If the agent discovers ambiguity, merge risk, or dependency on another task:

- send `blocked`, `question`, or `handoff` messages
- poll again before making risky assumptions

#### Before current Step 7 (merge/push)

Add one more mandatory poll:

- fetch recent messages
- check whether another agent completed related work
- adjust merge strategy before `git fetch origin` / `git merge origin/TARGET_BRANCH`

#### After successful push

Send a completion message:

```bash
mato comm send \
  --type task-complete \
  --to all \
  --task "<filename>" \
  --branch "<task-branch>" \
  --body "Merged to TARGET_BRANCH_PLACEHOLDER. Main files changed: ..."
```

Optionally add a note to the resulting merge commit.

### Important prompt guidance

The instructions should emphasize that messages are:

- **advisory coordination**, not a lock
- a way to share intent, status, and warnings
- not a replacement for Git merge/rebase correctness
- not a replacement for the `.tasks` queue

That matters because task claiming must still rely on the current atomic filesystem `mv`, which is simpler and stronger than trying to claim work through Git refs.

---

## 4. Message Types

These are the message types that fit mato’s current workflow best.

| Type | Purpose | Typical sender moment | Key fields |
| --- | --- | --- | --- |
| `intent` | Announces planned edits | Right before Step 5 work begins | task, branch, files, body |
| `task-started` | Announces active work | After task claim + branch creation | task, branch, base |
| `question` | Ask for context or assumptions | During ambiguous implementation | task, body |
| `answer` | Respond to a prior question | When another agent can help | task, body |
| `blocked` | Warn peers of unresolved issue | On dependency or merge risk | task, branch, body |
| `handoff` | Leave context for the next agent | Before failing/requeuing or after partial discovery | task, files, body |
| `conflict-warning` | Warn about overlapping edits or likely merge pain | Before Step 7 | task, files, body |
| `task-complete` | Broadcast completion | After successful push | task, branch, files, body |
| `task-failed` | Broadcast that work was requeued | In On Failure path | task, body |
| `status` | Lightweight heartbeat/progress update | Long-running tasks | task, branch, body |

For mato specifically, the highest-value messages are probably:

1. `intent`
2. `blocked`
3. `handoff`
4. `task-complete`
5. `conflict-warning`

Those directly reduce duplicated work and merge surprises.

---

## 5. Concurrency & Reliability

## Concurrent publishes

The recommended ref-per-message design handles concurrency well because agents do **not** push to the same ref.

If message refs are named with:

- UTC timestamp
- `agentID` from existing `generateAgentID()` / `MATO_AGENT_ID`
- random nonce

then pushes are effectively collision-free.

Example:

```text
refs/mato/messages/all/20260302T101530Z-a1b2c3d4-6f09a7c1
```

Two agents can publish at the same time without a merge or rebase step.

### Conflict resolution behavior

There are only a few cases to handle:

1. **Ref name collision** — extremely unlikely; regenerate nonce and retry.
2. **Push race on same ref path** — only possible if two publishers somehow chose the same ID; same fix as above.
3. **Fetch sees stale state** — acceptable; this channel is eventually consistent.

This is much better than a shared comms branch, where all agents race on one branch tip.

## Delivery semantics

Git refs give mato **durable eventual delivery**, not real-time messaging.

That is a good fit for the current system because mato agents already operate in coarse steps:

- poll task queue
- claim task
- work for minutes
- merge/push
- exit

This is not a chat system; it is asynchronous coordination.

### Recommended fetch frequency

In the container, agents should poll:

1. once immediately after creating the task branch
2. once before major edits if the task is ambiguous
3. once before Step 7 merge/push
4. optionally every 5-10 minutes for unusually long tasks

That keeps overhead small while still being useful.

Because `origin` is currently a local-path remote mounted from the host repo, fetches should be relatively cheap.

## Reliability across restarts

This is one of the biggest benefits for mato:

- container restarts do not lose messages
- temporary clone deletion does not lose messages
- host mato restarts do not lose messages
- another agent can fetch historical messages later

The data lives in the shared origin repository, which is exactly the shared durable surface mato already trusts.

## Retention and pruning

Messages should not live forever in the hot namespace.

Suggested policy:

- keep live message refs for 7-30 days
- optionally attach final summaries to `refs/notes/mato`
- prune old message refs from the active namespace periodically

A simple host-side cleanup path in `run()` or `mato comm prune` is enough.

## Important reliability boundary

Git-based communication should be treated as a **coordination aid**, not a correctness boundary.

Still rely on:

- `.tasks/backlog -> .tasks/in-progress` via atomic `mv` for claiming
- normal Git merge/fetch/push rules for source-of-truth code integration
- existing orphan recovery and retry logic in `recoverOrphanedTasks()`

If messaging fails, task execution should still proceed.

---

## 6. Pros

1. **No new infrastructure**  
   Mato already ships Git into the container and already depends on fetch/push to a shared origin.

2. **Messages survive container restarts**  
   This is a major advantage over in-memory or per-container IPC.

3. **Auditable history**  
   Every message is a Git object with timestamps and authorship. Debugging becomes `git show <ref>` instead of scraping logs.

4. **Works with mato’s temp-clone model**  
   Agents are isolated, but Git already bridges the isolation through the shared origin.

5. **No shared writable daemon required**  
   No socket server, no Redis, no extra supervisor process.

6. **Plays well with existing flow**  
   The `.tasks` queue stays as-is; communication is additive.

7. **Natural fit for post-task annotations**  
   Git notes can attach final summaries to the commits agents already create.

8. **Easy manual inspection**  
   Operators can inspect or clean up with stock Git commands.

---

## 7. Cons

1. **Git is not a real message bus**  
   Delivery is eventual, polling-based, and relatively coarse.

2. **Extra Git overhead**  
   More fetch/push activity means more process launches and object churn.

3. **Custom refs are less familiar**  
   This is a powerful Git feature, but not a common team workflow.

4. **Implementation complexity is moderate**  
   The raw plumbing is simple, but the ergonomic layer (`mato comm`) and prompt integration take care.

5. **Retention management is required**  
   Without pruning, the namespace will grow indefinitely.

6. **Not ideal for high-volume traffic**  
   Fine for coordination messages; bad for verbose logs or streaming output.

7. **LLM agents should not be expected to use low-level Git plumbing directly**  
   A wrapper command or helper is effectively required for reliability.

8. **Does not eliminate merge conflicts**  
   It only helps agents warn each other earlier.

---

## 8. Complexity Estimate

## Scope estimate

This feels like a **medium-sized change**, not a tiny patch but also not a rewrite.

### Code/effort estimate

| Area | Estimated work |
| --- | --- |
| `comm.go` / `comm_git.go` implementation | 150-250 LoC |
| CLI/subcommand parsing for `mato comm ...` | 80-150 LoC |
| Docker mount/env wiring in `runOnce()` | 20-40 LoC |
| Prompt updates in `task-instructions.md` | 30-60 lines |
| Tests with temp Git repos | 150-250 LoC |
| Optional notes/pruning support | 50-100 LoC |

### Overall estimate

- **Engineering effort:** roughly 1-2 focused days
- **Risk:** moderate
- **Test complexity:** moderate, because temp repo fetch/push tests are straightforward
- **Operational complexity:** low to moderate once the helper command exists

### Why the estimate is not higher

Mato already has several pieces in place:

- `gitOutput()` centralizes Git invocation
- `runOnce()` already prepares a clone and Docker environment
- `task-instructions.md` is already embedded and templated
- tests already use temp directories and validate orchestration behavior

The main new work is not Git itself; it is making the Git messaging ergonomic and robust for autonomous agents.

## Recommended implementation order

1. **Phase 1:** add Git ref publish/list primitives in Go
2. **Phase 2:** expose them as hidden `mato comm` subcommands
3. **Phase 3:** mount `mato` into the container and update `task-instructions.md`
4. **Phase 4:** add optional commit notes and pruning

That path delivers value early while keeping the first version simple.

---

## Bottom line

For mato, Git-based communication is a strong fit **if it is built on immutable custom refs, not a shared branch**.

That design matches the current architecture:

- shared origin repo
- isolated temp clones
- Dockerized agents
- existing Git fetch/push workflow
- filesystem queue kept for atomic task claiming

The result is a durable, auditable, restart-safe coordination layer that improves agent awareness without adding a separate service.