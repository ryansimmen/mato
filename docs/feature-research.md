# Feature Research: Recommended Enhancements for Mato

> **Date**: March 2026
> **Scope**: Deep research and prioritized feature recommendations for the Multi Agent Task Orchestrator

---

## Executive Summary

Mato is a production-grade distributed task orchestrator for AI coding agents, built
on a filesystem-backed queue with Docker isolation, automatic AI review gates, and
serial squash-merge integration. After analyzing the codebase, competitive landscape
(LangGraph, CrewAI, AutoGen, GitHub Agent HQ/Mission Control), and industry best
practices for multi-agent orchestration, this document recommends **10 features**
grouped into three tiers by impact and implementation effort.

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

## Appendix: Research Sources

- **Codebase analysis**: Full review of all 18 packages, ~18K LOC
- **Architecture docs**: `docs/architecture.md`, `docs/task-format.md`, `docs/messaging.md`, `docs/configuration.md`
- **Competitive frameworks**: LangGraph, CrewAI, AutoGen, GitHub Agent HQ/Mission Control
- **Industry best practices**: Multi-agent orchestration patterns, observability standards, CI/CD integration patterns
