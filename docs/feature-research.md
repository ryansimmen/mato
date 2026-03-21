# Feature Research: Recommended Enhancements for Mato

> **Date**: March 2026
> **Scope**: Deep research and prioritized feature recommendations for the Multi Agent Task Orchestrator

---

## Executive Summary

Mato is a production-grade distributed task orchestrator for AI coding agents, built
on a filesystem-backed queue with Docker isolation, automatic AI review gates, and
serial squash-merge integration. After analyzing the codebase, competitive landscape
(LangGraph, CrewAI, AutoGen, GitHub Agent HQ/Mission Control), and industry best
practices for multi-agent orchestration, this document provides:

1. **10 feature recommendations** grouped into three tiers by impact and effort
2. **7 alternative task management approaches** evaluated against mato's design
   philosophy, with a recommended layered strategy

---

## Current Strengths

| Area | Capability |
|------|-----------|
| Safety | Atomic file ops (link+remove), PID-based locking with start-time, retry budgets |
| Quality | Automatic AI review gate before every merge |
| Isolation | Docker containers with bind-mounted tools, per-agent temp clones |
| Coordination | Filesystem messaging protocol, conflict detection via `affects` |
| Simplicity | Zero external dependencies — no database, no daemon, no cloud service |

## Identified Gaps

| Gap | Impact |
|-----|--------|
| No web dashboard or HTTP API | Operators must SSH into the host to run `mato status` |
| No external notifications | No way to know a task completed/failed without polling |
| Exact-match `affects` only | `pkg/client/` won't conflict with `pkg/client/http.go` |
| No config file | Every option requires CLI flags or env vars; no project-level defaults |
| No agent concurrency control | No built-in worker pool; scaling requires manual multi-instance setup |
| No metrics/telemetry | No visibility into throughput, duration, failure rates |
| No task templates | Every task must be hand-authored markdown |
| Squash-merge only | No rebase or fast-forward option |
| `estimated_complexity` unused | Field is parsed but never influences scheduling |
| No GitHub integration | No PR creation, issue linking, or webhook triggers |

---

## Tier 1 — High Impact, Moderate Effort

### 1. Configuration File Support (`.mato.yaml`)

**Problem**: All settings require CLI flags or environment variables. Teams cannot
commit shared defaults to version control, and every `mato` invocation must
repeat the same flags.

**Proposal**: Load a `.mato.yaml` (or `.mato.yml`) from the repository root,
with CLI flags taking precedence.

```yaml
# .mato.yaml
branch: main
tasks_dir: .tasks
docker_image: ubuntu:24.04
agent_timeout: 45m
default_model: claude-sonnet-4.5
max_agents: 4
notifications:
  on_complete: webhook
  webhook_url: https://hooks.example.com/mato
```

**Implementation sketch**:
- Add `internal/config/config.go` with YAML unmarshaling via `gopkg.in/yaml.v3`
  (already an indirect dependency through frontmatter parsing).
- Load in `cmd/mato/main.go` before flag parsing; merge precedence:
  CLI flag > environment variable > config file > default.
- Validate unknown fields, emit warnings for deprecated keys.

**Effort**: ~200 LOC + tests | **Impact**: High — enables reproducible team workflows

---

### 2. Glob/Prefix Matching for `affects` Conflict Detection

**Problem**: The `affects` field uses exact string matching. A task declaring
`affects: [pkg/client/]` won't conflict with another declaring
`affects: [pkg/client/http.go]`, leading to silent merge conflicts.

**Proposal**: Support glob patterns via Go's `path.Match` or `filepath.Match`
in the `overlappingAffects` function, with an optional directory-prefix rule
(trailing `/` means "anything under this path").

```yaml
affects:
  - "pkg/client/**"       # glob: matches any file under pkg/client/
  - "internal/queue/*.go"  # glob: matches Go files in queue/
  - "README.md"            # exact: unchanged behavior
```

**Implementation sketch**:
- Modify `overlappingAffects()` in `internal/queue/queue.go` to attempt
  `filepath.Match(pattern, candidate)` when either string contains `*`, `?`,
  or `[`.
- Add prefix matching: if a pattern ends with `/`, treat it as `pattern + **`.
- Preserve exact-match fast path for patterns without wildcards.
- Update the file-claims index builder to expand globs against known claims.

**Effort**: ~100 LOC + tests | **Impact**: High — prevents real merge conflicts

---

### 3. Webhook / External Notifications

**Problem**: There is no way to be notified when tasks complete, fail, or need
attention without polling `mato status`.

**Proposal**: Add a pluggable notification system with initial support for
webhooks (HTTP POST) and optional Slack integration.

**Events to surface**:
| Event | Payload includes |
|-------|-----------------|
| `task.completed` | task ID, branch, commit SHA, duration |
| `task.failed` | task ID, failure reason, retry count |
| `task.review.rejected` | task ID, review feedback summary |
| `merge.conflict` | task ID, conflicting files |
| `queue.empty` | timestamp, total completed count |

**Implementation sketch**:
- Add `internal/notify/notify.go` with a `Notifier` interface
  (`Notify(event Event) error`).
- Implement `WebhookNotifier` using `net/http` with retry + timeout.
- Wire into the runner loop at existing transition points (task move,
  merge completion, review verdict).
- Configuration via `.mato.yaml` (see Feature 1) or env vars.

**Effort**: ~300 LOC + tests | **Impact**: High — enables CI/CD integration and team awareness

---

### 4. Built-in Agent Pool with Concurrency Control

**Problem**: Each `mato` process runs exactly one agent in a sequential loop.
Scaling requires users to manually launch multiple instances with no coordination
on resource limits.

**Proposal**: Add a `--max-agents N` flag (default: 1) that spawns up to N
concurrent Docker containers, each claiming independent tasks.

**Implementation sketch**:
- Add a worker pool in `internal/runner/pool.go` using a semaphore
  (`chan struct{}` of size N).
- Each worker runs the existing `runOneAgent` loop in a goroutine.
- Shared queue access is already safe (atomic file operations).
- Add pool-level graceful shutdown: drain semaphore, send SIGTERM to all
  containers, wait up to `gracefulShutdownDelay`.
- Surface pool status in `mato status` (active/idle/total workers).

**Effort**: ~250 LOC + tests | **Impact**: High — core scaling feature

---

## Tier 2 — Medium Impact, Moderate Effort

### 5. HTTP Status API and Web Dashboard

**Problem**: `mato status` is CLI-only and requires terminal access to the host
machine. Teams working remotely or across time zones cannot easily monitor queue
health.

**Proposal**: Add an optional HTTP server (`mato status --serve :8080`) that
exposes a JSON API and a minimal single-page web dashboard.

**API endpoints**:
```
GET  /api/v1/status          → queue summary (counts, agents, merge lock)
GET  /api/v1/tasks           → all tasks with state, priority, dependencies
GET  /api/v1/tasks/:id       → single task detail
GET  /api/v1/messages        → recent messages
GET  /api/v1/health          → liveness probe
```

**Dashboard**: Embedded HTML/JS via `//go:embed` (same pattern as
`task-instructions.md`). Shows task kanban board, agent activity, and
dependency graph.

**Implementation sketch**:
- Add `internal/status/server.go` using `net/http` (stdlib only).
- Reuse existing `status.Collect()` function for data.
- Embed static assets with `//go:embed dashboard/*`.
- Add `--serve` flag to `mato status` subcommand.

**Effort**: ~500 LOC + embedded assets | **Impact**: Medium-High — team visibility

---

### 6. Task Templates and Batch Creation

**Problem**: Creating tasks requires manually writing markdown files with
correct YAML frontmatter. This is error-prone and slow for large feature
decompositions.

**Proposal**: Add a `mato create` subcommand that generates task files from
templates or interactively.

```bash
# From template
mato create --template feature \
  --title "Add dark mode" \
  --affects "src/styles/**,src/components/Theme.tsx" \
  --priority 20

# Batch from YAML manifest
mato create --from tasks.yaml

# Interactive mode
mato create -i
```

**Implementation sketch**:
- Add `internal/taskgen/taskgen.go` with template rendering.
- Built-in templates: `feature`, `bugfix`, `refactor`, `test`, `docs`.
- Custom templates from `.mato/templates/` directory.
- Validate generated frontmatter against schema.
- Auto-assign IDs and wire `depends_on` for batch creation.

**Effort**: ~350 LOC + tests | **Impact**: Medium — reduces friction for task creation

---

### 7. Metrics and Observability

**Problem**: No visibility into system performance — task duration, failure
rates, agent utilization, merge throughput. Debugging requires reading
filesystem state manually.

**Proposal**: Add optional structured metrics collection, exportable as JSON
logs or Prometheus-compatible format.

**Metrics to track**:
| Metric | Type | Description |
|--------|------|-------------|
| `mato_tasks_total` | Counter | Tasks by final state (completed/failed) |
| `mato_task_duration_seconds` | Histogram | Time from claim to completion |
| `mato_agent_busy_seconds` | Gauge | Per-agent active time |
| `mato_review_verdicts_total` | Counter | Review outcomes (approve/reject/error) |
| `mato_merge_duration_seconds` | Histogram | Squash-merge time |
| `mato_merge_conflicts_total` | Counter | Merge conflicts encountered |
| `mato_retry_count` | Histogram | Retries per task before success/failure |
| `mato_queue_depth` | Gauge | Tasks per state directory |

**Implementation sketch**:
- Add `internal/metrics/metrics.go` with in-memory counters.
- Expose via `/metrics` endpoint on the status HTTP server (Feature 5).
- Optionally write JSON metrics to `.tasks/metrics/` for offline analysis.
- Use `expvar` from stdlib for zero-dependency Prometheus-compatible output,
  or add `prometheus/client_golang` for native Prometheus support.

**Effort**: ~300 LOC + tests | **Impact**: Medium — critical for production operations

---

### 8. GitHub Integration (PR Creation and Issue Linking)

**Problem**: After `mato` squash-merges a task branch, there is no automatic
pull request creation, no issue linking, and no status checks integration.
Teams using GitHub as their primary workflow have a gap.

**Proposal**: Add optional GitHub integration that creates PRs for completed
task branches and links them to issues referenced in task metadata.

```yaml
# In task frontmatter
---
id: add-dark-mode
github_issue: 42
create_pr: true
---
```

**Capabilities**:
- Auto-create PR from task branch to target branch before merge.
- Link PR to GitHub issue via `Closes #42` in PR body.
- Post task status updates as issue comments.
- Trigger `mato` task creation from GitHub issue labels
  (e.g., label `mato:ready` creates a task file).

**Implementation sketch**:
- Add `internal/github/github.go` using `go-github` library or raw
  GitHub REST API via `net/http`.
- Auth via `GITHUB_TOKEN` env var (already available in most CI).
- Wire into merge completion hook in `internal/merge/merge.go`.
- Add optional `github:` section to `.mato.yaml`.

**Effort**: ~400 LOC + tests | **Impact**: Medium — valuable for GitHub-native workflows

---

## Tier 3 — Lower Impact or Higher Effort

### 9. Complexity-Aware Scheduling

**Problem**: The `estimated_complexity` frontmatter field is parsed but unused.
All tasks are scheduled purely by priority and filename order, ignoring
complexity signals that could improve throughput.

**Proposal**: Use `estimated_complexity` to influence scheduling decisions:
- **Timeout scaling**: High-complexity tasks get longer agent timeouts
  (e.g., `high` → 1.5× default, `critical` → 2×).
- **Agent affinity**: When multiple agents are available (Feature 4),
  prefer assigning complex tasks to agents that have completed similar
  tasks successfully.
- **Estimated duration**: Track actual duration per complexity level,
  surface predictions in `mato status`.

**Implementation sketch**:
- Map complexity strings to multipliers in `internal/runner/config.go`.
- Apply timeout multiplier in `runAgent()` before `context.WithTimeout()`.
- Track completion times in `.tasks/metrics/` (pairs with Feature 7).

**Effort**: ~150 LOC + tests | **Impact**: Low-Medium — optimization, not critical path

---

### 10. Alternative Merge Strategies

**Problem**: Only squash merge is supported. Some teams prefer rebase
(linear history) or regular merge commits (preserving branch topology).

**Proposal**: Add a `merge_strategy` option (per-task or global) supporting
`squash` (default), `rebase`, and `merge`.

```yaml
# .mato.yaml (global default)
merge_strategy: squash

# Per-task override in frontmatter
---
id: large-refactor
merge_strategy: rebase
---
```

**Implementation sketch**:
- Add strategy parameter to `SquashMerge()` in `internal/merge/merge.go`.
- Implement `rebaseMerge()` using `git rebase --onto`.
- Implement `regularMerge()` using `git merge --no-ff`.
- Each strategy produces appropriate commit message formatting.

**Effort**: ~200 LOC + tests | **Impact**: Low-Medium — preference, not functionality

---

## Recommendation Priority Matrix

| # | Feature | Impact | Effort | Priority Score |
|---|---------|--------|--------|----------------|
| 1 | Configuration file (`.mato.yaml`) | High | Low | ⭐⭐⭐⭐⭐ |
| 2 | Glob matching for `affects` | High | Low | ⭐⭐⭐⭐⭐ |
| 3 | Webhook notifications | High | Medium | ⭐⭐⭐⭐ |
| 4 | Agent pool with concurrency | High | Medium | ⭐⭐⭐⭐ |
| 5 | HTTP status API + dashboard | Medium-High | Medium | ⭐⭐⭐⭐ |
| 6 | Task templates (`mato create`) | Medium | Medium | ⭐⭐⭐ |
| 7 | Metrics and observability | Medium | Medium | ⭐⭐⭐ |
| 8 | GitHub integration (PRs/issues) | Medium | High | ⭐⭐⭐ |
| 9 | Complexity-aware scheduling | Low-Medium | Low | ⭐⭐ |
| 10 | Alternative merge strategies | Low-Medium | Medium | ⭐⭐ |

## Suggested Implementation Order

**Phase 1 — Foundation** (enables other features):
1. Configuration file support — unblocks webhook config, pool config, etc.
2. Glob matching for `affects` — immediate safety improvement

**Phase 2 — Scaling & Visibility**:
3. Agent pool with concurrency control — core scaling capability
4. HTTP status API — enables remote monitoring
5. Webhook notifications — enables CI/CD integration

**Phase 3 — Developer Experience**:
6. Task templates — reduces task creation friction
7. Metrics — production observability
8. GitHub integration — workflow automation

**Phase 4 — Polish**:
9. Complexity-aware scheduling — optimization
10. Alternative merge strategies — flexibility

---

## Competitive Context

| Capability | Mato | GitHub Agent HQ | CrewAI | AutoGen |
|------------|------|-----------------|--------|---------|
| Task queue | ✅ Filesystem | ✅ Cloud | ✅ In-memory | ✅ Event-driven |
| Multi-agent | ⚠️ Manual scaling | ✅ Built-in | ✅ Built-in | ✅ Built-in |
| Review gate | ✅ AI review | ✅ Human review | ❌ | ⚠️ Human-in-loop |
| Web dashboard | ❌ | ✅ Mission Control | ❌ | ✅ Studio |
| Notifications | ❌ | ✅ GitHub native | ❌ | ⚠️ Callbacks |
| Config file | ❌ | ✅ YAML workflows | ✅ YAML | ✅ Python |
| Metrics | ❌ | ✅ Cloud metrics | ❌ | ✅ Logging |
| Zero dependencies | ✅ | ❌ Cloud required | ❌ Python | ❌ Python |
| Self-hosted | ✅ | ❌ | ✅ | ✅ |
| Git-native | ✅ | ✅ | ❌ | ❌ |

Mato's key differentiator is **zero-dependency self-hosted operation** with
**git-native coordination**. The recommended features preserve this philosophy
while closing the gaps that matter most for production adoption.

---

## Alternative Task Management Approaches

Mato currently uses a **filesystem-backed queue** where tasks are markdown files
with YAML frontmatter, organized into state directories (`waiting/`, `backlog/`,
`in-progress/`, etc.). Transitions happen via atomic `os.Link` + `os.Remove`
operations. This section evaluates alternative approaches to task management that
could replace, augment, or complement the current system.

### Current Architecture: Filesystem Queue

**How it works today:**
- Tasks are `.md` files moved between directories (`waiting/` → `backlog/` →
  `in-progress/` → `ready-for-review/` → `ready-to-merge/` → `completed/`)
- Claiming uses atomic `os.Link` + `os.Remove` to prevent race conditions
- Priority is encoded in YAML frontmatter and materialized into a `.queue` manifest
- Dependencies are resolved by checking if `depends_on` IDs exist in `completed/`
- Conflict detection uses exact-match `affects` field intersection

**Strengths of the current approach:**
- Zero external dependencies — no database, broker, or cloud service
- LLM-friendly — agents can read/write task files directly
- Git-native — task files can be committed, diffed, and code-reviewed
- Transparent — `ls .tasks/in-progress/` tells you everything
- Portable — works on any POSIX system with Docker

**Limitations:**
- No ACID transactions across multiple file operations
- Directory listings scale linearly (O(n) per scan for large queues)
- No semantic search or structured queries over task metadata
- Cross-machine coordination requires a shared filesystem (NFS, etc.)
- Polling-based discovery (10-second intervals) adds latency

---

### Alternative 1: Embedded Database Queue (SQLite / bbolt)

Replace the filesystem directories with an embedded database while keeping
the same zero-dependency philosophy.

**How it would work:**
- Single `.tasks/queue.db` file replaces the directory structure
- Tasks stored as rows/documents with state, priority, metadata columns
- State transitions become SQL `UPDATE` with row-level locking
- Dependencies resolved via JOIN queries instead of directory scans
- `.queue` manifest replaced by `SELECT ... ORDER BY priority, id WHERE state = 'backlog'`

**Go libraries available:**
| Library | Backend | Type | Key Feature |
|---------|---------|------|-------------|
| [goqite](https://github.com/maragudk/goqite) | SQLite | Message queue | SQS-like semantics, visibility timeout |
| [backlite](https://github.com/mikestefanello/backlite) | SQLite | Job queue | Type-safe, scheduled execution, Web UI |
| [bbolt](https://github.com/etcd-io/bbolt) | B+tree KV | Key-value | Pure Go, ACID, used by etcd |
| [badger](https://github.com/dgraph-io/badger) | LSM KV | Key-value | High throughput, pure Go |

**Tradeoffs vs. filesystem:**

| Dimension | Filesystem (current) | Embedded DB |
|-----------|---------------------|-------------|
| ACID guarantees | Per-file only | Full transactions |
| Query capability | Directory listing | SQL / key-range scans |
| Concurrency safety | Atomic link+remove | Row/page locking |
| Transparency | `ls`, `cat`, `grep` | Requires query tool |
| Git integration | Native (files are text) | Requires export/import |
| LLM accessibility | Direct file read/write | Needs API wrapper |
| Setup complexity | None | Compile-time dependency |
| Cross-machine | Shared filesystem | Single-machine only |
| Performance at scale | Degrades with 1000+ files | Handles millions |

**Recommendation:** **Not recommended as a full replacement.** The filesystem
approach is a core differentiator — it enables git-native workflows, LLM
direct access, and zero-dependency operation. However, an embedded DB could
be valuable as an **index/cache layer** alongside the filesystem:

```
.tasks/
  ├── backlog/task-a.md       ← source of truth (human/LLM readable)
  ├── in-progress/task-b.md
  └── .cache/index.db         ← SQLite index for fast queries
```

The index would be rebuilt on startup from filesystem state and kept in sync
during operations, enabling fast queries without sacrificing transparency.

---

### Alternative 2: GitHub Issues as Task Queue

Use GitHub Issues as the task management layer, with mato consuming issues
instead of local markdown files.

**How it would work:**
- Each GitHub issue = one task; labels encode state (`mato:backlog`, `mato:in-progress`)
- Dependencies expressed via issue references (`depends on #42`)
- Priority via labels (`priority:critical`, `priority:normal`) or milestones
- `affects` stored in issue body frontmatter or as a structured comment
- Mato polls the GitHub API (or listens via webhooks) for issue state changes
- Agent assignment = assigning the issue to a bot user

**Advantages:**
- Built-in web UI, notifications, and mobile access
- Cross-repo visibility and organization-level dashboards
- Human collaboration — team members can comment, label, and triage
- Existing integrations (Slack, email, project boards, Actions)
- Full audit trail with timestamps and actor attribution
- Search, filter, and milestone tracking out of the box

**Disadvantages:**
- API rate limits (5,000 requests/hour authenticated; polling overhead)
- Requires network connectivity (no offline operation)
- External dependency on GitHub availability
- Slower state transitions (HTTP round-trip vs. filesystem rename)
- Less control over data format and lifecycle semantics
- Issue metadata is less structured than YAML frontmatter

**Implementation sketch:**
- Add `internal/github/issues.go` with a `GitHubTaskSource` interface
- Implement adapter that maps GitHub issue state to mato queue states
- Support both directions: issue → task file, and task completion → issue close
- Auth via `GITHUB_TOKEN`; configure via `.mato.yaml`:
  ```yaml
  task_source: github
  github:
    owner: myorg
    repo: myproject
    label_prefix: "mato:"
  ```

**Recommendation:** **Implement as an optional adapter, not a replacement.**
A `task_source: github` mode would let teams that are GitHub-native use issues
as their task surface while mato handles orchestration. The filesystem queue
remains the default for zero-dependency operation.

---

### Alternative 3: Git-Native Distributed Issue Tracking

Store task data inside Git objects (not working-tree files) using a
distributed issue tracker like `git-bug`.

**How it would work:**
- Tasks stored as Git objects in a dedicated ref (e.g., `refs/mato/tasks`)
- State transitions are Git operations (create tree, write object, update ref)
- Push/pull tasks between remotes like code branches
- No working-tree pollution — tasks don't appear in `ls` or `git status`
- Full version history of every task state change

**Existing tools:**
| Tool | Approach | Status |
|------|----------|--------|
| [git-bug](https://github.com/git-bug/git-bug) | Git objects, offline-first | Active, mature |
| [git-issue](https://github.com/dspinellis/git-issue) | Text files in branch | Active |
| [Fossil](https://fossil-scm.org/) | Built-in DVCS + tracker | Mature (non-Git) |

**Tradeoffs:**

| Dimension | Filesystem files | Git objects |
|-----------|-----------------|-------------|
| Distribution | Shared filesystem needed | Push/pull via Git remotes |
| History | None (file moves only) | Full version history per task |
| Working tree | Tasks visible as files | Hidden in Git internals |
| LLM access | Direct read/write | Requires Git plumbing commands |
| Multi-machine | NFS or similar | Native via git push/pull |
| Complexity | Low | High (Git object model) |
| Query speed | Directory scan | Need custom index |

**Recommendation:** **Interesting for multi-machine scenarios but too complex
for the primary use case.** The filesystem approach's transparency is more
valuable than Git-object distribution for most mato deployments. However, the
concept of pushing/pulling tasks between remotes could be useful for
federated mato deployments.

---

### Alternative 4: Event-Driven / Message Broker Architecture

Replace polling with an event-driven architecture using a message broker
(Redis, NATS, or an embedded Go solution).

**How it would work:**
- Task creation publishes a `task.created` event to a topic
- Agents subscribe to `task.available` and receive tasks via push (no polling)
- State transitions emit events consumed by merge, review, and notification services
- Event log provides full audit trail and enables replay
- Back-pressure handled by broker (agent only receives next task when ready)

**Broker options for Go:**

| Broker | Deployment | Persistence | Go Support |
|--------|-----------|-------------|------------|
| NATS | Embedded or standalone | JetStream (built-in) | Excellent (Go-native) |
| Redis Streams | External service | AOF/RDB | Good |
| Watermill (Go) | Embedded library | Pluggable (memory, SQL, Kafka) | Native |
| RabbitMQ | External service | Disk-backed | Good |

**Tradeoffs vs. polling:**

| Dimension | Filesystem polling | Event-driven |
|-----------|-------------------|--------------|
| Latency | 10-second poll interval | Sub-millisecond |
| Resource usage | Periodic directory scans | Idle until event |
| Complexity | Low (read directories) | Medium-High (broker lifecycle) |
| Dependencies | None | Broker binary or library |
| Debugging | `ls .tasks/backlog/` | Event log viewer / CLI |
| Ordering | Priority sort on each poll | Priority queues in broker |
| Multi-machine | Shared filesystem | Network protocol (native) |
| Offline resilience | Files persist on disk | Depends on broker persistence |

**Recommendation:** **Consider NATS as an optional acceleration layer.**
NATS is written in Go, can be embedded as a library, and its JetStream
persistence model provides durable queues. This preserves the "minimal
dependency" philosophy while eliminating polling latency. The filesystem
queue could remain as the durable source of truth with NATS providing
real-time event notification on top.

---

### Alternative 5: Hybrid Filesystem + Structured Index

Keep the filesystem as the source of truth but add a structured index
for fast queries, filtering, and status aggregation.

**How it would work:**
- Task `.md` files remain in state directories (unchanged)
- An in-memory index (rebuilt on startup, updated on file operations)
  provides O(1) lookups by ID, state, priority, and affects
- Index supports:
  - Fast dependency graph traversal (vs. current O(n²) directory scans)
  - Instant queue depth counts (vs. `readdir` + parse per poll)
  - Fuzzy/prefix `affects` matching without glob expansion
  - Task search by tags, complexity, or custom metadata

**Data structure:**
```go
type TaskIndex struct {
    mu       sync.RWMutex
    byID     map[string]*IndexEntry      // O(1) lookup
    byState  map[string][]*IndexEntry    // grouped by directory
    byFile   map[string][]*IndexEntry    // affects → tasks
    depGraph map[string][]string         // dependency edges
}
```

**Advantages:**
- No new dependencies — pure Go data structures
- Filesystem remains source of truth (transparent, git-friendly)
- Eliminates repeated directory scans in the poll loop
- Enables features like task search, complex filtering, and graph queries
- Index corruption is harmless — rebuild from filesystem at any time

**Implementation sketch:**
- Add `internal/queue/index.go` with `TaskIndex` type
- Build index in `ReconcileReadyQueue` (already scans all directories)
- Use index in `SelectAndClaimTask`, `DeferredOverlappingTasks`, etc.
- Add `mato status --search <query>` for indexed task search

**Recommendation:** **Strongly recommended as a near-term improvement.**
This is the lowest-risk, highest-value change — it improves performance
and enables new features without changing the storage model.

---

### Alternative 6: Durable Workflow Engines

Use a workflow orchestration engine designed for step-by-step task execution
with checkpointing and recovery.

**How it would work:**
- Each task becomes a workflow with steps: claim → branch → work → review → merge
- Workflow engine handles retries, timeouts, and state persistence
- Steps can be paused, resumed, or rolled back
- Built-in support for DAG execution of dependent tasks

**Relevant platforms:**
| Platform | Language | Deployment | Key Feature |
|----------|----------|-----------|-------------|
| [Temporal](https://temporal.io/) | Go SDK | Server + worker | Durable execution, replay |
| [Hatchet](https://hatchet.run/) | Go SDK | PostgreSQL-backed | DAG workflows, retry, UI |
| [Inngest](https://inngest.com/) | Go SDK | Cloud or self-hosted | Step functions, event-driven |
| [Cadence](https://cadenceworkflow.io/) | Go SDK | Server cluster | Uber's workflow engine |

**Tradeoffs:**

| Dimension | Mato filesystem | Workflow engine |
|-----------|----------------|-----------------|
| Setup complexity | Zero | Server + database |
| Observability | `mato status` CLI | Built-in web dashboard |
| Retry semantics | File-based counter | Engine-managed with backoff |
| DAG execution | Manual `depends_on` | Native workflow graphs |
| Checkpoint/resume | Not supported | Native |
| Dependencies | None | Server infrastructure |
| Learning curve | Low (files + YAML) | Medium-High (SDK + concepts) |

**Recommendation:** **Not recommended as a replacement** — mato's value
proposition is zero-dependency simplicity. However, for teams already running
Temporal or Hatchet, a **Temporal adapter** that models mato's task lifecycle
as a Temporal workflow could provide best-of-both-worlds: mato's task format
and review gate with Temporal's durability and observability.

---

### Alternative 7: YAML Task Manifests (Taskfile-style)

Replace individual task files with a single YAML manifest defining all tasks,
their dependencies, and execution order — similar to how
[Taskfile](https://taskfile.dev/) or CI pipelines define workflow DAGs.

**How it would work:**
```yaml
# .tasks/manifest.yaml
tasks:
  setup-database:
    priority: 10
    affects: [src/db/**]
    body: |
      Set up PostgreSQL connection pooling...

  add-user-api:
    priority: 20
    depends_on: [setup-database]
    affects: [src/api/users.go, src/api/users_test.go]
    body: |
      Implement CRUD endpoints for user management...

  add-auth:
    priority: 20
    depends_on: [setup-database]
    affects: [src/auth/**]
    body: |
      Add JWT authentication middleware...
```

**Advantages:**
- Single file to review, edit, and version-control
- Dependency graph visible at a glance
- Batch operations (add 10 tasks) are a single file edit
- Familiar to CI/CD users (GitHub Actions, GitLab CI)
- Easier to validate (schema check one file vs. N files)

**Disadvantages:**
- Concurrent edits to single file create merge conflicts
- Harder for agents to modify atomically (can't rename a row)
- State tracking requires separate storage (can't use directories)
- Loses the "move file between directories" simplicity
- Large manifests become unwieldy (100+ tasks in one YAML)

**Recommendation:** **Support as an input format, not a runtime format.**
A `mato import --from manifest.yaml` command could read a YAML manifest and
generate individual task files in the appropriate directories. This gives
users the convenience of batch definition while preserving the filesystem
queue's operational model. The mato planning skill could also output
manifests for human review before importing.

---

### Comparison Matrix: All Approaches

| Approach | Zero-Dep | Multi-Machine | Query Speed | LLM Access | Git Native | Complexity |
|----------|----------|---------------|-------------|------------|------------|------------|
| Filesystem (current) | ✅ | ❌ (NFS) | ⚠️ O(n) | ✅ Direct | ✅ | Low |
| Embedded DB | ✅ | ❌ | ✅ O(1) | ❌ API needed | ❌ | Medium |
| GitHub Issues | ❌ | ✅ | ✅ API | ⚠️ Via API | ⚠️ | Medium |
| Git objects | ✅ | ✅ (push/pull) | ❌ | ❌ Plumbing | ✅ | High |
| Event-driven | ❌ | ✅ | ✅ Push | ❌ | ❌ | High |
| Hybrid index | ✅ | ❌ (NFS) | ✅ O(1) | ✅ Direct | ✅ | Low-Medium |
| Workflow engine | ❌ | ✅ | ✅ | ❌ | ❌ | High |
| YAML manifest | ✅ | ❌ | ✅ | ✅ Direct | ✅ | Low |

### Recommended Strategy

The research points to a **layered approach** that preserves mato's core
strengths while addressing limitations:

1. **Keep the filesystem queue as the source of truth** — it's the
   foundation of mato's zero-dependency, git-native, LLM-friendly design.

2. **Add a hybrid in-memory index** (Alternative 5) — lowest risk, highest
   immediate value. Eliminates O(n) directory scans and enables fast queries
   without new dependencies.

3. **Support GitHub Issues as an optional task source** (Alternative 2) —
   meets teams where they already work, while mato handles orchestration.

4. **Support YAML manifest import** (Alternative 7) — simplifies batch task
   creation and integrates with the existing planning skill.

5. **Consider NATS for event-driven notifications** (Alternative 4) — only
   if polling latency becomes a measurable bottleneck in production.

6. **Avoid full-replacement approaches** (embedded DB, workflow engines,
   Git objects) — they sacrifice mato's key differentiators without
   proportionate benefit for the target use case.

---

## Appendix: Research Sources

- **Codebase analysis**: Full review of all 18 packages, ~18K LOC
- **Architecture docs**: `docs/architecture.md`, `docs/task-format.md`, `docs/messaging.md`, `docs/configuration.md`
- **Competitive frameworks**: LangGraph, CrewAI, AutoGen, GitHub Agent HQ/Mission Control
- **Industry best practices**: Multi-agent orchestration patterns, observability standards, CI/CD integration patterns
- **Task queue research**: Hatchet, Temporal, Inngest, Celery, goqite, backlite, Watermill
- **Git-native tracking**: git-bug, git-issue, Fossil
- **Event-driven architectures**: NATS, Redis Streams, Confluent (Kafka), Watermill
- **Filesystem vs. DB analysis**: Arize AI, Oracle Developer Blog, AgentFS (Turso)
- **YAML task runners**: Taskfile (go-task), GitHub Actions, Airflow DAGs
