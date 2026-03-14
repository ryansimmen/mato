# Approach 5: Unix Domain Socket Broker for Inter-Agent Communication

## Overview

This approach adds a **host-managed Unix domain socket broker** to `mato` so otherwise-isolated Copilot agents can exchange messages while they work. Today, `mato` only shares state through the filesystem queue in `.tasks/`, and each container runs independently after `runOnce()` launches `docker run` from `main.go`. With a Unix socket broker, the host process becomes a lightweight relay:

1. `mato` starts a Unix domain socket listener before entering the task-processing loop in `run()`.
2. The socket lives on the host inside the shared `.tasks/` tree so containers can see it through the existing bind mount.
3. Each agent container connects to the socket when it wants to announce itself, send a message, or poll for new messages.
4. The host broker receives the request, routes it to the target inbox, and returns an acknowledgement or queued messages.

This keeps the communication path local to the machine, avoids opening TCP ports, and fits mato’s current Linux + Docker + shared-bind-mount model.

**Important codebase caveat:** the current implementation in `main.go` generates one `agentID` per `mato` process and launches **one container at a time per process**. The README recommends running multiple `mato` processes in separate terminals for parallelism. A Unix socket owned by one host process only brokers messages for containers launched by that process. So this design works best if either:

- `mato` evolves toward a **single host process with multiple concurrent workers**, or
- one process is elected as the shared broker owner and other `mato` processes discover and reuse that socket.

For the first implementation, I would document the single-host-process assumption explicitly.

## Architecture

### Recommended socket location

Use a broker directory inside the already-shared task tree:

```text
<repo>/.tasks/
├── backlog/
├── in-progress/
├── completed/
├── failed/
├── .locks/
└── .broker/
    ├── mato.sock
    ├── state.json          # optional broker metadata
    └── spool/              # optional persisted envelopes
```

Host path:

```go
socketDir := filepath.Join(tasksDir, ".broker")
socketPath := filepath.Join(socketDir, "mato.sock")
```

Container path:

```text
/workspace/.tasks/.broker/mato.sock
```

Because `runOnce()` already mounts `tasksDir` at `/workspace/.tasks`:

```go
"-v", fmt.Sprintf("%s:%s/.tasks", tasksDir, workdir),
```

**no extra Docker `-v` is strictly required** if the socket is created inside `.tasks`. That is the most codebase-friendly option. If you want a cleaner runtime path such as `/run/mato/mato.sock`, then add an explicit mount for the socket directory, but the existing `.tasks` mount makes that unnecessary.

### Broker model

The host process owns the listener and message state:

- Accepts connections with `net.Listen("unix", socketPath)`
- Handles each client connection in its own goroutine
- Maintains in-memory inboxes keyed by agent session ID
- Supports simple request/response operations: register, send, poll, ack, heartbeat
- Optionally persists envelopes to `.tasks/.broker/spool/` for restart recovery

### Agent identity

The current code uses one `agentID` for both:

- task claim metadata (`<!-- claimed-by: ... -->`)
- host liveness tracking via `.tasks/.locks/<agentID>.pid`
- environment variable injection into the container (`MATO_AGENT_ID`)

That is fine for orphan-task recovery, but it is not enough for socket routing. For messaging, split identity into two concepts:

- **Host/process ID**: keep current `MATO_AGENT_ID` semantics for `.locks/` and task-file metadata
- **Session/worker ID**: add `MATO_SESSION_ID` (or `MATO_RUN_ID`) per container launch for broker addressing

That avoids breaking `recoverOrphanedTasks()` and `isAgentActive()`, which currently rely on the `.locks` files written by `registerAgent()`.

### Protocol design

Use **line-delimited JSON (NDJSON)**. It is easy to implement with only Go’s standard library, easy to debug with logs, and flexible enough for richer message envelopes later.

Recommended framing:

- One JSON object per line
- UTF-8 text
- Client opens a connection, sends one request or a short request sequence, reads one response, then exits
- Keep protocol pull-oriented so agents do not need a long-lived listener process inside the container

Example request:

```json
{"op":"send","session_id":"sess-a1b2c3d4","from":"sess-a1b2c3d4","to":"broadcast","kind":"help-request","task":"refactor-cache.md","body":{"question":"Anyone already editing pkg/cache/store.go?"},"sent_at":"2026-03-14T07:00:00Z"}
```

Example poll response:

```json
{"ok":true,"messages":[{"id":"msg-42","from":"sess-deadbeef","kind":"status","task":"api-cleanup.md","body":{"status":"editing main.go"}}]}
```

### Routing strategy

Keep routing intentionally small for v1:

- `to: "broadcast"` → every connected/known session except sender
- `to: "session:<id>"` → one specific agent
- `to: "task:<filename>"` → agents currently working on the same task or task family
- `to: "host"` → host-only control/diagnostic messages

The broker can also track lightweight metadata from `register` requests:

- session ID
- host agent ID
- claimed task file
- branch name
- container PID if available
- last heartbeat time

That enables later features like targeted conflict warnings or “who is working on file X?” requests without changing transport.

## Implementation Details

### 1. Add a broker type in Go

Create a new file such as `broker.go` instead of expanding `main.go` further. Suggested shapes:

```go
type Broker struct {
    socketPath string
    listener   net.Listener

    mu        sync.Mutex
    clients   map[string]*ClientState
    inboxes   map[string][]Envelope
    stopped   bool
}

type ClientState struct {
    SessionID    string
    AgentID      string
    TaskFile     string
    Branch       string
    LastSeen     time.Time
}

type Envelope struct {
    ID        string          `json:"id"`
    From      string          `json:"from"`
    To        string          `json:"to"`
    Kind      string          `json:"kind"`
    Task      string          `json:"task,omitempty"`
    Body      json.RawMessage `json:"body,omitempty"`
    SentAt    time.Time       `json:"sent_at"`
}
```

Core methods:

```go
func StartBroker(tasksDir string) (*Broker, error)
func (b *Broker) Serve() error
func (b *Broker) Close() error
func (b *Broker) handleConn(conn net.Conn)
func (b *Broker) route(msg Envelope) error
func (b *Broker) poll(sessionID string, max int) []Envelope
```

### 2. Start the broker from `run()`

`run()` in `main.go` is the right place to start and stop the broker because it already owns lifecycle concerns such as:

- directory setup for `.tasks/`
- signal handling
- the long-running orchestration loop

Recommended flow:

```go
broker, err := StartBroker(tasksDir)
if err != nil {
    return fmt.Errorf("start broker: %w", err)
}
defer broker.Close()

go func() {
    if err := broker.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
        fmt.Fprintf(os.Stderr, "warning: broker exited: %v\n", err)
    }
}()
```

### 3. Clean up stale socket files before listening

Unix sockets are filesystem entries. If `mato` crashes, the socket path may remain and prevent rebinding.

Startup sequence should:

1. `os.MkdirAll(filepath.Join(tasksDir, ".broker"), 0o700)`
2. `os.Remove(socketPath)` if it exists
3. `net.Listen("unix", socketPath)`
4. `os.Chmod(socketPath, 0o660)`
5. on shutdown: close listener and remove the socket file

That cleanup is mandatory.

### 4. Add env vars for containers

In `runOnce()`, add new environment variables alongside the existing `MATO_AGENT_ID` injection:

```go
args = append(args,
    "-e", "MATO_AGENT_ID="+agentID,
    "-e", "MATO_SESSION_ID="+sessionID,
    "-e", "MATO_BROKER_SOCKET="+filepath.Join(workdir, ".tasks", ".broker", "mato.sock"),
)
```

Notes:

- `agentID` should stay tied to the host/process lock-file model.
- `sessionID` should be generated per `runOnce()` call.
- If mato later supports multiple simultaneous containers per process, `sessionID` becomes essential.

### 5. Consider mounting the `mato` binary into containers

Agents need a practical client. The cleanest way, given `runOnce()` already mounts host binaries like `copilot`, `git`, and `gh`, is to also mount the running `mato` binary as a helper CLI:

```go
selfPath, err := os.Executable()
// ...
args = append(args, "-v", fmt.Sprintf("%s:/usr/local/bin/mato:ro", selfPath))
```

Then add lightweight subcommands such as:

```text
mato msg register
mato msg send
mato msg poll
mato msg ack
```

This is much better than depending on `socat`, `nc -U`, Python, or ad hoc shell parsing inside `ubuntu:24.04`, because those tools are not guaranteed to exist in the container today.

### 6. Add a tiny client mode to the CLI

Extend `parseArgs()` or add a separate subcommand parser for message operations. Example CLI shape:

```bash
mato msg register \
  --socket "$MATO_BROKER_SOCKET" \
  --session "$MATO_SESSION_ID" \
  --agent "$MATO_AGENT_ID" \
  --task "$TASK_FILE" \
  --branch "$(git branch --show-current)"

mato msg send \
  --socket "$MATO_BROKER_SOCKET" \
  --session "$MATO_SESSION_ID" \
  --to broadcast \
  --kind help-request \
  --task "$TASK_FILE" \
  --body '{"question":"Anyone already editing main.go?"}'

mato msg poll \
  --socket "$MATO_BROKER_SOCKET" \
  --session "$MATO_SESSION_ID" \
  --wait 5s \
  --max 10
```

The client implementation can be tiny:

```go
conn, err := net.Dial("unix", socketPath)
enc := json.NewEncoder(conn)
dec := json.NewDecoder(conn)
if err := enc.Encode(req); err != nil { ... }
if err := dec.Decode(&resp); err != nil { ... }
```

### 7. Update `task-instructions.md`

Because the agent behavior is driven by the embedded prompt in `taskInstructions`, you will need to add a small “Inter-agent communication” section. It should be optional and very constrained so agents do not spam the broker.

Suggested guidance:

- register after claiming a task
- poll before large refactors or before merging
- send broadcast conflict warnings when editing shared files
- ask for help only when blocked
- include task filename and short reason in every message
- continue normally if the broker is unavailable

### 8. Optional persistence for restart recovery

A pure in-memory broker is easiest, but messages disappear if the host restarts. Because `.tasks/` already persists across containers, a pragmatic middle ground is a spool directory:

```text
.tasks/.broker/spool/<session-id>.jsonl
```

On `send`:

- append the envelope to the recipient spool file(s)
- keep it in memory for fast delivery

On `ack`:

- mark as delivered in memory
- optionally compact the spool file later

On broker restart:

- replay spool files into inboxes
- expire messages older than a TTL (for example 30 minutes)

That gives you useful reliability without adding an external service.

## Message Types

There are really two categories: **transport/control** messages and **agent-semantic** messages.

### Transport/control

- `register` - session announces itself to the broker
- `heartbeat` - keep last-seen fresh
- `poll` - fetch queued messages
- `ack` - confirm delivery of message IDs
- `unregister` - best-effort cleanup on exit
- `list` - optional debugging endpoint for host visibility

### Agent-semantic

These are the messages agents would actually exchange:

1. **status**
   - “I claimed `<task>`”
   - “I am editing `pkg/cache/store.go`”
   - “I am about to merge into `mato`”

2. **help-request**
   - ask whether another agent already understands or owns an area
   - ask for quick guidance before duplicating work

3. **help-response**
   - direct answer to a help request
   - can include file paths, branch names, or warnings

4. **conflict-warning**
   - “I modified `main.go` and `README.md`; expect merge overlap”
   - useful just before Step 7 in the current task workflow

5. **dependency-update**
   - “I renamed function X to Y”
   - “I changed CLI flags”
   - “Branch `task/add-cache` introduced new helper package”

6. **artifact-ready**
   - “Shared helper committed on branch `task/refactor-http-client`”
   - tells other agents they can fetch/rebase or inspect a concrete change

7. **review-request**
   - optional, for asking another agent to sanity-check an approach before merge

8. **host-event**
   - broker-generated notices such as “broker restarting”, “message queue full”, or “target unavailable”

I would keep the first release intentionally small: `register`, `poll`, `send`, `ack`, plus semantic kinds `status`, `help-request`, `help-response`, and `conflict-warning`.

## Concurrency & Reliability

### Goroutine-per-connection is fine

Traffic volume in mato should be low: a few agents, short messages, infrequent polling. A standard Go model is enough:

- one accept loop
- one goroutine per accepted connection
- a mutex around broker state, or a single broker-state goroutine with channels

Either works. For simplicity, a `sync.Mutex` is likely enough.

### Bounded inboxes

Do not allow unbounded queues. Add a per-session limit such as 100 messages.

When full, either:

- reject new messages with a structured error, or
- drop oldest non-acked messages and emit a `host-event`

Rejecting is safer for v1 because silent drops would be hard for agents to reason about.

### Broker restart behavior

If the broker is unavailable because the host process restarts:

- agent `mato msg ...` commands will get `ENOENT` or connection errors
- clients should treat this as **non-fatal** and continue their task without communication
- once the broker returns, agents can re-register on next poll/send

If you implement spool persistence, queued but unacked messages can survive restart. Without it, messages are best-effort only.

### Session liveness

Track `LastSeen` from `register`, `poll`, and `heartbeat`.

A cleanup goroutine can periodically mark sessions stale after, for example, 60 seconds of silence. Stale sessions can still keep spooled messages, but the broker should not treat them as actively connected.

### Socket file cleanup

This is one of the trickiest operational details with Unix sockets:

- remove stale socket before `Listen`
- remove it again on shutdown
- create `.tasks/.broker/` with restrictive permissions
- avoid leaving a stale entry after crashes

This is straightforward, but it must be implemented carefully.

### Interaction with current orphan recovery

`recoverOrphanedTasks()` and `cleanStaleLocks()` should remain authoritative for task recovery. The socket broker should not replace the `.locks/` model immediately.

That means:

- task ownership still comes from moving files into `.tasks/in-progress/`
- crash recovery still comes from `.locks/*.pid` + task metadata
- broker state is advisory, not the source of truth, in v1

That is the safest incremental path.

## Pros

1. **Low latency and simple local transport**
   - Unix sockets are faster and simpler than local TCP for same-host communication.

2. **No exposed network port**
   - no port allocation, firewall rules, or accidental remote access.

3. **Fits existing Docker design**
   - mato already shares `.tasks/` into `/workspace/.tasks`; placing the socket there means the broker piggybacks on current mounts.

4. **Pure Go standard library**
   - broker and client can use `net`, `bufio`, and `encoding/json`; no new service dependency required.

5. **Good ergonomics for a host-side broker**
   - the host already owns lifecycle, task polling, and Docker execution, so adding a broker to `run()` is conceptually aligned.

6. **Keeps the filesystem queue intact**
   - message passing is additive; it does not force a rewrite of the task claim/recovery mechanism.

7. **Easier debugging than a more distributed solution**
   - one socket path, one broker, one log stream.

## Cons

1. **Current mato concurrency model limits the value**
   - today, one `mato` process runs one container at a time. The README’s parallelism story is “start multiple mato processes.” A per-process broker does not automatically span those processes.

2. **Linux-first behavior**
   - Unix sockets bind-mounted into containers are most straightforward on Linux hosts. Docker Desktop/macOS behavior is less predictable and should be treated as non-primary.

3. **Socket file lifecycle is operationally fragile**
   - stale socket paths, permissions, and cleanup bugs can break startup.

4. **Agents need a usable client tool**
   - the container currently has `copilot`, `git`, `gh`, and `go`, but no guaranteed `socat`/`nc -U`. Without mounting `mato` itself or shipping another helper, the socket is awkward to use.

5. **Best-effort unless you add persistence**
   - an in-memory broker loses queued messages on restart.

6. **Broker becomes a local single point of failure**
   - if the host process crashes, communication disappears immediately.

7. **More identity/state complexity**
   - you need to distinguish process identity (`MATO_AGENT_ID`) from per-container routing identity (`MATO_SESSION_ID`).

8. **Prompt complexity risk**
   - if `task-instructions.md` over-emphasizes messaging, agents may waste time chatting instead of executing tasks. The instructions need tight constraints.

## Complexity Estimate

### MVP (best-effort, in-memory only)

Estimated effort: **2-3 engineering days**

Rough code size:

- `broker.go`: 200-300 LoC
- client subcommands / CLI parsing: 100-180 LoC
- `main.go` integration and env injection: 40-80 LoC
- prompt updates in `task-instructions.md`: 20-40 LoC
- tests: 150-250 LoC

Main tasks:

- add broker startup/shutdown
- add socket path management
- add request/response protocol structs
- add message send/poll CLI
- mount `mato` binary into container
- add unit tests for routing and socket cleanup

### More production-ready version (spool persistence + stale-session cleanup)

Estimated effort: **4-6 engineering days**

Additional work:

- spool persistence and replay
- inbox compaction / TTL handling
- richer test coverage for restart behavior
- optional broker-owner discovery if multiple `mato` host processes are supported

### Suggested testing plan

Add tests alongside existing `main_test.go` patterns, likely in a new `broker_test.go`:

- `StartBroker` removes stale socket and listens successfully
- `send` to `broadcast` fans out correctly
- `poll` returns only targeted messages
- stale session cleanup works
- spool replay restores queued messages after broker restart
- `runOnce()` injects `MATO_BROKER_SOCKET` and mounts the helper binary

## Recommendation

Unix domain sockets are a good fit for mato **if** you want a lightweight, host-local broker and are willing to keep communication best-effort. For this codebase, the cleanest implementation is:

1. create the socket at `.tasks/.broker/mato.sock`
2. start a broker goroutine from `run()`
3. keep `.tasks` + `.locks` as the source of truth for task ownership and crash recovery
4. add a per-container `MATO_SESSION_ID`
5. mount the `mato` binary into the container and expose `mato msg send/poll`
6. keep protocol and message types intentionally small for v1

The biggest architectural issue is not the socket itself; it is that current parallelism is based on **multiple independent mato host processes**. If inter-agent communication is meant to span all workers, I would pair this design with either a single-host multi-worker mode or a broker-owner election mechanism in `.tasks/.broker/`.