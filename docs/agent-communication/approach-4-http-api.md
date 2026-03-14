# HTTP/REST API Inter-Agent Communication Proposal for `mato`

## 1. Overview

Today, `mato` coordinates agents indirectly through the shared `.tasks/` directory and git operations. The host process polls `.tasks/backlog/`, launches a Docker container, and the agent inside that container claims work by moving task files between `backlog/`, `in-progress/`, `completed/`, and `failed` (`main.go:68-73`, `main.go:178-210`, `task-instructions.md:25-39`, `README.md:42-47`). Each agent works in its own temporary clone created by `createClone()` (`main.go:407-417`) and has no direct channel to other agents beyond the shared filesystem.

An HTTP API adds a central, structured coordination layer without changing the basic `mato` execution model. The host `mato` process would run a lightweight HTTP server in-process, alongside the existing polling loop in `run()` (`main.go:56-211`). Agents in Docker containers would call that server to:

- publish messages to other agents or to all agents,
- read new messages since a cursor or timestamp,
- register heartbeats and status,
- discover what other agents are active,
- advertise coordination facts such as “I’m touching file X”, “I am blocked”, or “I just merged task Y”.

This keeps the current filesystem task queue as the source of truth for task claiming and completion while introducing a real-time coordination plane for everything that does **not** fit naturally into task file moves.

In other words: `.tasks/` remains the durable work queue, and the new HTTP API becomes the control bus.

---

## 2. Architecture

### High-level design

The proposal is to embed a small REST server into the `mato` host process. A single host process already owns the orchestration loop, so it is the natural place to host shared coordination state.

Proposed runtime model:

1. `run()` resolves repo state, ensures `.tasks/` subdirectories exist, and generates the host agent ID as it does today (`main.go:57-90`).
2. Before entering the polling loop, `run()` starts an HTTP server in a goroutine.
3. `runOnce()` passes API connection information into each container via environment variables.
4. Agents use `curl` (or any HTTP client) inside the container to talk to the API.
5. The API tracks messages, heartbeats, and agent metadata.
6. On shutdown, `mato` gracefully stops the HTTP server together with the main loop.

### Recommended endpoint set

A practical minimum API for `mato` is:

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/agents/register` | Register a running agent container instance and its metadata |
| `POST` | `/agents/heartbeat` | Refresh liveness / update last-seen timestamp |
| `POST` | `/status` | Publish current agent status, task, branch, progress, or blockers |
| `GET` | `/agents` | List active or recently seen agents and their latest status |
| `POST` | `/messages` | Send a message to another agent, a group, or broadcast |
| `GET` | `/messages` | Read messages for an agent, optionally filtered by `since`, `after_id`, `topic`, or `from` |
| `POST` | `/messages/ack` | Mark delivery/read progress so agents can avoid rereading old messages |
| `GET` | `/healthz` | Simple health probe for debugging and startup checks |
| `GET` | `/config` | Return API capabilities, TTLs, polling interval guidance, and server time |

A slightly richer second phase could add:

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/leases` | Claim a soft coordination lease (for example on a file path or topic) |
| `DELETE` | `/leases/{id}` | Release a lease |
| `GET` | `/events` | Return a unified stream of recent messages + status events |

For the first implementation, `messages`, `status`, `agents`, `register`, and `heartbeat` are enough.

### Example resource shapes

#### `POST /agents/register`

Request:

```json
{
  "agent_id": "8f21c4ab",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "container_hostname": "mato-8f21c4ab",
  "repo_root": "/workspace",
  "capabilities": ["git", "go", "gh"],
  "pid_hint": null
}
```

Response:

```json
{
  "ok": true,
  "server_time": "2026-03-14T12:00:00Z",
  "poll_interval_seconds": 5,
  "message_ttl_seconds": 3600
}
```

#### `POST /messages`

Request:

```json
{
  "from": "8f21c4ab",
  "to": "broadcast",
  "topic": "merge-warning",
  "type": "coordination.notice",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "body": {
    "paths": ["pkg/client/http.go"],
    "note": "I am about to refactor retry behavior in the HTTP client"
  },
  "expires_in_seconds": 1800
}
```

Response:

```json
{
  "id": 42,
  "created_at": "2026-03-14T12:01:10Z"
}
```

#### `GET /messages?agent_id=15aa9d01&after_id=40`

Response:

```json
{
  "messages": [
    {
      "id": 41,
      "from": "8f21c4ab",
      "to": "15aa9d01",
      "topic": "dependency",
      "type": "question",
      "body": {
        "question": "Are you also editing pkg/client/http.go?"
      },
      "created_at": "2026-03-14T12:01:02Z",
      "expires_at": "2026-03-14T13:01:02Z"
    }
  ],
  "next_after_id": 41
}
```

#### `POST /status`

Request:

```json
{
  "agent_id": "8f21c4ab",
  "state": "working",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "summary": "Implementing retry loop and writing tests",
  "progress": 60,
  "blocked_on": null,
  "touching_paths": [
    "pkg/client/http.go",
    "pkg/client/http_test.go"
  ]
}
```

### Where the server runs

The HTTP server should run inside the existing `mato` process, not as a second binary. This best matches the current single-binary design (`main.go` only) and avoids adding another process lifecycle to manage.

A good structure is:

- `main.go` keeps orchestration and Docker launching.
- New `api.go` holds HTTP server types, routes, handlers, and persistence.
- `run()` creates the API store and starts `http.Server` in a goroutine.
- `run()` shuts the server down when `SIGINT` / `SIGTERM` is received (`main.go:173-176`, `main.go:204-209`).

### How containers reach the host

This is the key deployment detail.

Current `docker run` arguments do **not** set networking explicitly (`main.go:238-294`). That means containers run on Docker’s default bridge network.

Recommended options:

#### Preferred cross-platform option

Pass:

```text
--add-host host.docker.internal:host-gateway
```

and expose the API URL to the container as:

```text
MATO_API_URL=http://host.docker.internal:7777
```

Why this fits `mato`:

- It works with the existing `docker run` pattern.
- It avoids `--network host`, which is Linux-specific and changes isolation characteristics.
- It keeps the server bound on the host and leaves the container networking otherwise unchanged.

#### Alternative option: `--network host`

This allows agents to call `http://127.0.0.1:<port>` directly, but it is less portable and gives containers broader host network access. It is acceptable for Linux-only environments, but it should not be the default recommendation for `mato`.

#### Docker Desktop behavior

On macOS/Windows, `host.docker.internal` typically already resolves. On Linux, `--add-host host.docker.internal:host-gateway` should be added explicitly in `runOnce()`.

---

## 3. Implementation Details

### 3.1 Go code changes

### New types and files

Recommended new files:

- `api.go`: REST server, route registration, handler implementations
- `api_store.go` or keep store in `api.go`: in-memory state + persistence
- `api_test.go`: unit tests for handlers, filtering, TTL cleanup, persistence reload

Key structs:

```go
type APIServer struct {
    srv        *http.Server
    store      *APIStore
    token      string
    baseURL    string
    persistDir string
}

type APIStore struct {
    mu           sync.RWMutex
    nextMessageID int64
    messages     []Message
    agents       map[string]AgentState
    statuses     map[string]AgentStatus
    cursors      map[string]int64
}

type Message struct {
    ID        int64           `json:"id"`
    From      string          `json:"from"`
    To        string          `json:"to"`
    Topic     string          `json:"topic"`
    Type      string          `json:"type"`
    TaskFile  string          `json:"task_file,omitempty"`
    TaskBranch string         `json:"task_branch,omitempty"`
    Body      json.RawMessage `json:"body"`
    CreatedAt time.Time       `json:"created_at"`
    ExpiresAt time.Time       `json:"expires_at,omitempty"`
}
```

### Changes in `run()`

`run()` in `main.go` is the correct startup hook because it already performs all host initialization before entering the loop (`main.go:56-177`).

Add roughly this flow after `.tasks` setup and before the prompt is built or before the loop begins:

```go
apiCfg := loadAPIConfig(repoRoot, tasksDir)
apiStore, err := OpenAPIStore(filepath.Join(tasksDir, "api"))
if err != nil {
    return fmt.Errorf("open API store: %w", err)
}
apiServer, err := StartAPIServer(apiCfg, apiStore)
if err != nil {
    return fmt.Errorf("start API server: %w", err)
}
defer apiServer.Shutdown(context.Background())
```

Suggested defaults:

- listen address: `127.0.0.1:7777`
- persist directory: `<repo>/.tasks/api/`
- token auth: enabled by default
- message TTL: 1 hour
- heartbeat expiry: 30 seconds

Why `<repo>/.tasks/api/` is a good location:

- `.tasks/` is already created by `run()` and already gitignored by `ensureGitignored()` (`main.go:89-92`, `main.go:439-467`).
- It keeps coordination state next to the existing queue state.
- It fits the project’s filesystem-oriented design.

### Changes in `runOnce()`

`runOnce()` currently injects environment variables like `MATO_AGENT_ID` and mounts shared paths (`main.go:238-294`). This is the right place to pass API connectivity details.

Add to the Docker arguments:

```go
args = append(args,
    "--add-host", "host.docker.internal:host-gateway",
    "-e", "MATO_API_URL="+apiBaseURL,
    "-e", "MATO_API_TOKEN="+apiToken,
)
```

Also consider:

```go
-e MATO_API_POLL_INTERVAL=5
```

so agents know how often to poll without hardcoding it in prompt instructions.

### Graceful shutdown

The signal handling already exists in `run()` (`main.go:173-176`). Reuse it so shutdown order becomes:

1. stop polling,
2. shut down the HTTP server with timeout,
3. flush/store final API state,
4. remove the PID lock cleanup as today.

That preserves the current control flow and avoids orphaning the server goroutine.

### 3.2 Persistence design

A pure in-memory store is simplest, but `mato` already recovers from crashes by inspecting `.tasks/` and should do something similar for the API. The best fit is a hybrid store:

- **in-memory maps/slices** for fast handler access,
- **append-only JSONL journal** in `.tasks/api/events.jsonl` for durability,
- optional **periodic snapshot** in `.tasks/api/snapshot.json` to speed restart.

Recommended persisted records:

- agent registration / heartbeat updates,
- latest status per agent,
- message creation,
- message acknowledgements.

On startup, the API server should replay the snapshot and journal to rebuild memory.

This is more consistent with `mato` than adding SQLite immediately because the project currently has no data layer and relies on filesystem primitives.

### 3.3 Handler behavior

#### `POST /agents/register`

- Upserts `agents[agent_id]`
- sets `registered_at` and `last_seen_at`
- records task metadata if provided
- returns poll interval and TTL settings

#### `POST /agents/heartbeat`

- updates `last_seen_at`
- optionally refreshes current task and branch
- may auto-create a minimal agent entry if register was skipped

#### `POST /status`

- upserts `statuses[agent_id]`
- status should be latest-write-wins
- also emits an internal event record for audit/history

#### `GET /agents`

Returns recent agent view such as:

```json
{
  "agents": [
    {
      "agent_id": "8f21c4ab",
      "state": "working",
      "task_file": "add-retry-logic.md",
      "task_branch": "task/add-retry-logic",
      "last_seen_at": "2026-03-14T12:03:00Z",
      "touching_paths": ["pkg/client/http.go"]
    }
  ]
}
```

Support query parameters like:

- `active_only=true`
- `task_file=...`
- `path=...`

#### `POST /messages`

- validate sender and destination
- assign monotonic `id`
- default TTL if caller omits one
- allow destinations:
  - specific agent ID,
  - `broadcast`,
  - optionally `task:<task-file>`,
  - optionally `topic:<topic>`

#### `GET /messages`

Support:

- `agent_id=<id>`
- `after_id=<n>`
- `limit=<n>`
- `topic=<topic>`
- `include_broadcast=true`

Delivery semantics should be **at-least-once read**. Agents are expected to track `after_id` or use `/messages/ack`.

### 3.4 Docker networking setup

Current container launch code mounts files and binaries but does not expose host networking configuration (`main.go:238-294`). To enable host API access with minimal change:

1. Keep the HTTP server listening on host `127.0.0.1:<port>`.
2. Add `--add-host host.docker.internal:host-gateway` to `docker run`.
3. Pass `MATO_API_URL=http://host.docker.internal:<port>`.
4. Optionally generate a random token at host startup and pass it as `MATO_API_TOKEN`.

This avoids opening the API on all interfaces and still makes it reachable to containers.

### 3.5 Agent-side usage

The simplest agent client is `curl`, because the base image already runs shell commands and task instructions are shell-oriented.

Add a new section to `task-instructions.md` describing optional coordination calls, for example:

```bash
API_URL="${MATO_API_URL:-}"
API_TOKEN="${MATO_API_TOKEN:-}"
AGENT_ID="${MATO_AGENT_ID:-unknown}"

if [ -n "$API_URL" ]; then
  curl -sS -X POST "$API_URL/agents/register" \
    -H "Authorization: Bearer $API_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"agent_id\":\"$AGENT_ID\"}"
fi
```

Other examples to add to the prompt:

- after claiming a task: register + status update,
- before editing risky files: broadcast touched paths,
- when blocked: `POST /status` with `state=blocked`,
- before merge/push: send a merge-warning broadcast,
- after completion: send `task.completed` message.

A useful instruction addition would be:

> If `MATO_API_URL` is set, use the API to announce task claim, progress, blockers, and high-conflict file edits. The API does not replace `.tasks/`; it supplements it.

This preserves compatibility with older behavior if the API is disabled.

---

## 4. Message Types

The API should standardize a small set of coordination message types so agents can make consistent decisions.

### Recommended message categories

#### Coordination notices

- `coordination.notice`
- `coordination.warning`
- `coordination.request`
- `coordination.response`

Use for general cross-agent communication.

#### Work claims beyond task files

- `work.touch-paths`
- `work.refactor-starting`
- `work.schema-change`
- `work.test-suite-running`

Use when an agent wants others to know it is editing a hot file or subsystem.

#### Merge / branch coordination

- `merge.intent`
- `merge.completed`
- `merge.conflict-risk`

Useful because current agents merge independently into the target branch in Step 7 of `task-instructions.md:107-158`.

#### Dependency / question flow

- `question`
- `answer`
- `dependency.available`
- `dependency.blocked`

Example: one agent can announce that a helper function or refactor has landed so another agent can rebase or wait.

#### Status / lifecycle events

- `task.claimed`
- `task.started`
- `task.progress`
- `task.blocked`
- `task.completed`
- `task.failed`

These overlap with `/status`, but having them as explicit event messages is useful for audit/history and agent polling.

### Recommended destinations

- direct: `to = <agent_id>`
- broadcast: `to = broadcast`
- topic fanout: `to = topic:merge`
- task fanout: `to = task:add-retry-logic.md`

### Suggested message body fields

Depending on message type:

- `task_file`
- `task_branch`
- `paths`
- `summary`
- `question`
- `answer`
- `blocking_reason`
- `commit`
- `merged_branch`
- `priority`

The API should keep `body` flexible JSON rather than trying to hardcode every shape.

---

## 5. Concurrency & Reliability

### Thread safety inside the host server

Because the HTTP server will run concurrently with the polling loop and may receive requests from several containers at once, the store must be synchronized.

Recommended approach:

- `sync.RWMutex` around the shared store,
- write lock for register/status/message mutation,
- read lock for `GET /agents` and `GET /messages`,
- monotonic IDs generated under lock,
- cleanup goroutine for expired messages and stale agents.

The main loop itself does not need to wait on API requests; the server should be independent except for shared shutdown.

### Restart behavior

If `mato` restarts, current in-memory-only messages would be lost. That is a poor fit because the rest of `mato` already tries to recover from interruption (`recoverOrphanedTasks()` in `main.go:553-592`).

Recommended restart behavior:

- reload message/status journal from `.tasks/api/` on startup,
- expire very old messages during reload,
- mark agents without recent heartbeats as offline,
- keep the filesystem task queue authoritative for task recovery.

This means after restart:

- task claiming still works because `.tasks/` never changed,
- status history remains visible,
- recent messages survive,
- agents can re-register on next heartbeat.

### Delivery model

Keep the semantics simple:

- **message send**: durable once appended to the journal,
- **message read**: pull-based polling,
- **ack**: optional but recommended,
- **delivery guarantee**: at-least-once,
- **ordering**: global ordering by increasing message ID.

This is enough for coordination and avoids pretending to be a full message broker.

### TTLs and cleanup

Recommended defaults:

- heartbeat expiry: 30 seconds
- agent shown as stale/offline after 60 seconds
- status retention: 24 hours for audit, latest status always materialized
- normal message TTL: 1 hour
- merge / conflict-risk message TTL: 10 minutes
- task lifecycle message TTL: 24 hours

A background cleanup ticker every 30-60 seconds can:

- remove expired messages from memory,
- compact snapshots,
- mark agents stale based on `last_seen_at`.

### Failure cases

#### API unavailable while agent is running

The task agent must continue. The HTTP API is coordination-only, not required for correctness.

That means instructions should explicitly say:

- if API calls fail, continue performing the assigned task,
- do not abandon Step 6/7/8 because coordination failed,
- log a warning in output if practical.

#### Multiple `mato` hosts pointed at the same repo

Today, multiple `mato` instances can run concurrently using `.locks/` and task moves (`README.md:72-88`, `main.go:487-551`). With an HTTP API, multiple host processes become trickier because each host would have its own API state.

Recommendation:

- Version 1 assumes one active API host per repository / branch.
- If multiple `mato` processes are expected, either:
  - elect one API host, or
  - move API persistence to a truly shared backend later.

This is an important limitation to document.

---

## 6. Pros

### Real-time coordination without replacing the queue

The biggest advantage is that `mato` can keep its simple filesystem queue while gaining a live control channel. No redesign of Step 1/2 task claiming is required.

### Structured and queryable

Messages and statuses are explicit JSON objects instead of ad hoc comments in task files. That makes it easy to answer questions like:

- Which agents are active?
- Which files are currently hot?
- Which agent is blocked?
- Has anyone announced a merge on this subsystem?

### Good fit for current host-orchestrated design

`mato` already has a long-lived host loop in `run()`. Adding `net/http` there is straightforward and does not require a separate daemon.

### Easy agent-side integration

Agents can call the API with `curl` from shell steps. No Go client library is required in the container.

### Clear extension path

Once the server exists, future features become easier:

- live dashboard,
- event log,
- smarter merge coordination,
- soft leases on file paths,
- status UI for human operators.

### Better observability

Compared with the current design, the host can inspect a coherent view of all running agents instead of inferring state only from `.tasks/in-progress/` and PID locks.

---

## 7. Cons

### Networking complexity

This is the main downside. Today, the container only needs mounted files and local git paths. An HTTP API introduces host/container networking rules, host name resolution, port management, and auth.

### Single point of failure

If the host API goes down, coordination stops. Agents should still finish tasks, but inter-agent messaging and live status become unavailable.

### More moving parts in a previously simple system

The current design is elegant partly because it uses only filesystem moves and git. HTTP adds:

- request validation,
- handler code,
- persistence,
- cleanup jobs,
- auth token handling,
- tests for API behavior.

### Multi-host coordination remains unsolved

If several `mato` host processes are orchestrating the same repo, API state can fragment across hosts unless one is designated as the coordination server.

### Agents need HTTP client instructions

The prompt in `task-instructions.md` gets longer and slightly more complex. Agents must learn when coordination is optional and when to ignore failures.

### Potential security footguns

If the server listens on `0.0.0.0` without auth, anything on the network could post fake messages. The design should default to loopback binding plus a bearer token.

---

## 8. Complexity Estimate

### Overall estimate

This is a **moderate** implementation, not a tiny patch but also not a major architectural rewrite.

### Rough code size

Expected change size:

- `main.go` updates: ~40-80 lines
- `api.go` server + handlers: ~200-300 lines
- persistence / store helpers: ~120-200 lines
- tests: ~150-250 lines
- `task-instructions.md` updates: ~20-50 lines
- `README.md` updates: ~20-40 lines

Total: roughly **500-900 lines** depending on how much persistence and testing is included.

### Engineering effort

Approximate effort for a careful implementation:

1. **Minimal in-memory API only**: 0.5-1 day
   - start server
   - register/status/messages endpoints
   - pass URL/token into containers
   - update prompt instructions

2. **Recommended production-ish version**: 1.5-3 days
   - file-backed journal in `.tasks/api/`
   - TTL cleanup
   - startup replay
   - handler/unit tests
   - README updates

3. **Extended version with leases/events/dashboard hooks**: 3-5 days
   - soft locks on paths
   - richer querying
   - better observability/UI integration

### Suggested implementation phases

#### Phase 1: Basic API

- `POST /agents/register`
- `POST /agents/heartbeat`
- `POST /status`
- `GET /agents`
- `POST /messages`
- `GET /messages`
- loopback bind + bearer token + Docker host mapping

#### Phase 2: Reliability

- JSONL persistence under `.tasks/api/`
- startup reload
- TTL cleanup
- `/messages/ack`
- API tests

#### Phase 3: Smarter coordination

- file-path soft leases
- conflict-risk warnings
- event stream/dashboard support

### Recommendation

For `mato`, HTTP/REST is a strong proposal if the goal is **better real-time coordination with minimal disruption to the existing filesystem queue model**. The design fits the current `run()` / `runOnce()` host-orchestrated architecture well, especially because the host already launches containers and injects environment variables. The main engineering risks are Docker host reachability and ensuring the API remains optional so agent task execution still succeeds if coordination fails.

If implemented, the best first version is a host-local `net/http` server with bearer-token auth, `host.docker.internal` access from containers, and a file-backed journal under `.tasks/api/` for restart recovery.
