# New Features Debate: Multi-Agent Discussion (Round 2)

**Date:** 2026-03-21
**Participants:**
- 🟣 **Claude Opus 4.6** — "Correctness + Pragmatism" advocate
- 🟢 **GPT-5.4** — "Correct, Operable, Scalable" advocate
- 🔵 **Gemini 3.1 Pro** — "Developer Experience & Observability" advocate

> **Correction from Round 1:** The first round of this debate incorrectly proposed "Parallel Review Pipeline" and "Task Cancellation" as new features. Both already exist:
> - **Concurrency** is achieved by launching additional `mato` processes in separate terminals. Atomic task claiming, per-agent lock files, per-task review locks, and merge locking already handle coordination safely.
> - **Cancellation** works via Ctrl-C (SIGINT) or SIGTERM/SIGKILL. The runner catches signals, gracefully stops Docker containers (SIGTERM → 10s grace → SIGKILL), cleans up lock files, and orphaned tasks are auto-recovered on next startup.

---

## Summary of Positions

| Rank | 🟣 Claude Opus 4.6 | 🟢 GPT-5.4 | 🔵 Gemini 3.1 Pro |
|------|---------------------|-------------|---------------------|
| 1 | Glob Affects Matching | DAG-Aware Dependency Scheduler | Interactive TUI Dashboard |
| 2 | `mato doctor` Command | Per-Task Execution Profiles | Glob Pattern Affects |
| 3 | DAG Dependency Resolution | Event Journal + Live Status | Task Scaffolding & Templates |
| 4 | Persistent JSONL Audit Log | `mato doctor` Diagnostics | Dependency Cycle Detection |
| 5 | Task Templates + Batch Gen | Task Templates + Batch Gen | Per-Task Docker Images |

### Consensus Features (appeared in 2+ proposals)

| Feature | Advocates | Combined Priority |
|---------|-----------|-------------------|
| **Glob Affects Matching** | Opus (#1), Gemini (#2) | 🔴 Critical |
| **DAG Dependency Resolution** | Opus (#3), GPT (#1), Gemini (#4) | 🔴 Critical |
| **`mato doctor` Command** | Opus (#2), GPT (#4) | 🟠 High |
| **Structured Audit Trail** | Opus (#4), GPT (#3) | 🟠 High |
| **Task Templates & Batch Gen** | Opus (#5), GPT (#5), Gemini (#3) | 🟡 Medium |

### Unique Proposals

| Feature | Advocate | Priority |
|---------|----------|----------|
| **Interactive TUI Dashboard** | Gemini (#1) | 🟡 Medium |
| **Per-Task Execution Profiles** | GPT (#2) | 🟡 Medium |
| **Per-Task Docker Images** | Gemini (#5) | 🟡 Medium |

---

## Detailed Positions

---

## 🟣 Claude Opus 4.6 — "Correctness + Pragmatism"

### Philosophy

> I've studied every package in this codebase — every struct field, every state transition, every `AtomicMove` call. My five proposals are the features that would transform mato from a capable prototype into a production-grade orchestrator. I've ordered them by the **ratio of impact to risk**, because in infrastructure software, the fastest way to destroy trust is to ship something that breaks the queue.

Opus prioritizes features with highest impact-to-effort ratio, leading with quick wins that prevent real failures, then building toward deeper correctness and productivity improvements.

### Feature 1: Glob Pattern Matching in `affects`

**Problem:** `overlappingAffects()` in `internal/queue/overlap.go` performs exact string equality (map lookup). If Task A affects `internal/runner/runner.go` and Task B affects `internal/runner/review.go`, mato sees zero conflict — even though both are editing the same package. Users must enumerate every file path manually. Worse, it's unsound by default: users who forget a file get silent merge conflicts.

**Proposed Design:**
- Add `matchAffects(pattern, path string) bool` to `internal/queue/overlap.go`
- Use `filepath.Match` for simple globs, `doublestar.Match` for `**` recursive patterns
- When either affects list contains glob characters (`*`, `?`, `[`), fall back to O(n×m) pairwise matching; otherwise keep O(n+m) set-intersection fast path
- Globs expanded at comparison time, never rewrite the task file (preserves filesystem-as-state)
- Update `docs/task-format.md` with glob examples

**Example:**
```yaml
affects:
  - "internal/runner/*.go"
  - "docs/architecture.md"
  - "cmd/**"
```

**Impact:** Highest-ROI change. Merge conflicts are the most expensive failure mode — they waste an entire agent run, burn a retry, and cascade. Existing exact-path entries work identically (zero behavioral change for current users).

**Effort:** Small (1–2 days)

### Feature 2: `mato doctor` Diagnostic Command

**Problem:** When mato fails or behaves unexpectedly, debugging is painful. Is Docker running? Is Copilot CLI installed? Is `gh auth status` happy? Are there orphaned locks? Malformed task files? The `discoverHostTools()` function fails fast on the first missing tool with no summary.

**Proposed Design:**
- New package: `internal/doctor/`
- New CLI: `mato doctor [--repo PATH] [--tasks-dir PATH]`
- 14 checks: Docker reachable, Copilot CLI version, Git version, GitHub CLI auth, valid git repo, target branch exists, `.tasks/` structure valid, orphaned locks, stuck tasks, circular dependencies, missing dependency IDs, malformed YAML, disk space, Docker image pullable
- Output with ✓/✗/⚠ indicators and `Fix:` suggestions for each failure
- `mato doctor --json` for scripted use

**Impact:** Lowest-effort, highest-UX-value feature. Every CLI tool with external dependencies needs a doctor command. Turns "mato doesn't work" (bug report) into "mato told me what's wrong" (30-second fix). Also serves as living documentation of prerequisites.

**Effort:** Small (1–2 days)

### Feature 3: Full DAG Dependency Resolution with Cycle Reporting

**Problem:** Three blind spots in `internal/queue/reconcile.go`:
1. **No topological ordering** — `resolvePromotableTasks()` iterates in directory order, promoting one task per poll cycle. A 5-task chain takes 5 × 10s = 50s just for promotion cascading.
2. **Cycle detection is advisory-only** — `logCircularDependency()` prints to stderr but deadlocked tasks sit in `waiting/` forever. `mato status` doesn't surface this.
3. **No transitive dependency context** — task C depending on B depending on A only gets B's completion detail, not A's.

**Proposed Design:**
- New package: `internal/dag/` with `BuildGraph()`, `TopologicalSort()`, `TransitiveDeps()`, `PromotableSet()`
- Replace `resolvePromotableTasks()` with DAG-based promotion (full set in one pass)
- Surface cycles in `mato status` with full cycle path (A → B → C → A)
- Include transitive dependency context in `MATO_DEPENDENCY_CONTEXT`
- Add `mato status --validate-deps` pre-flight check

**Impact:** Deep dependency chains are mato's sweet spot. This turns the dependency system from "works for simple cases" to "production-grade."

**Effort:** Medium (3–5 days)

### Feature 4: Persistent Structured Audit Log

**Problem:** Events are garbage-collected after 24 hours via `CleanOldMessages()`. Runtime metadata is scattered across HTML comments in task files. No cross-task timeline exists — answering "what happened between 2pm and 3pm?" requires scanning every file.

**Proposed Design:**
- New package: `internal/auditlog/`
- Append-only JSONL at `.tasks/audit.log` (~200 bytes/entry, atomic on Linux for lines < PIPE_BUF)
- ~12 event types: `task.claimed`, `task.promoted`, `task.completed`, `task.failed`, `task.review.approved`, `task.review.rejected`, `task.merged`, `task.deferred`, `task.recovered`, `agent.registered`, `agent.exited`, `cycle.detected`
- New CLI: `mato log`, `mato log --task <id>`, `mato log --since 2h`, `mato log --type task.failed`, `mato log --json`
- Manual rotation via `mato log --rotate` (no auto-rotation — disk is cheap, lost debug context is expensive)
- Does NOT replace existing messaging system (different lifecycles, different consumers)

**Impact:** Table stakes for production use. When a task fails at 2am on retry 3, you need the full history. JSONL is greppable, `jq`-friendly, and needs zero new dependencies.

**Effort:** Medium (3–5 days)

### Feature 5: Task Templates with Batch Generation

**Problem:** Repetitive task patterns ("add error handling to every HTTP handler" = 15 tasks) require manual copy-paste of frontmatter. Error-prone and tedious. Also blocks CI/CD integration — no programmatic task creation.

**Proposed Design:**
- New package: `internal/template/`
- New CLI: `mato generate --template <file> [--var key=value ...] [--batch batch.yaml] [--dry-run]`
- Templates use Go `text/template` with custom FuncMap (`default`, `join`, `kebab`, `quote`)
- Batch YAML defines multiple tasks from one template with per-task variable overrides
- Validation: parse rendered output, check duplicate IDs, validate depends_on references, warn if affects patterns match no files
- Generated files land in `.tasks/waiting/` or `.tasks/backlog/`

**Impact:** Productivity multiplier. The bottleneck isn't mato's execution — it's task authoring. Templates enable CI/CD integration (pipeline generates batch → mato executes).

**Effort:** Medium (3–5 days)

### Rebuttals

> "Glob patterns add complexity — exact paths are safer."

Exact paths are safer only if users list them all. In practice, they don't. Any task with no `affects` field is never deferred, even if it modifies conflicting files. The system already operates with incomplete information. Globs make the *expressed* information more accurate.

> "A DAG package is over-engineered."

Consider batch generation (Feature 5): 15 tasks with a dependency chain. Under the current system, promotion takes 15 × 10s = 2.5 minutes of idle waiting. With topological sort, all promotable tasks are identified in one pass. Also, current cycle detection only prints to stderr — invisible in CI/CD.

> "Why not a web UI instead of `mato doctor` + `mato log`?"

A web UI requires persistent HTTP server, frontend assets, WebSocket — 2+ weeks minimum. `mato doctor` and `mato log` ship in 2–4 days total and provide 80% of the observability value through the terminal users already live in. Build CLI tools first; they become the API a future web UI calls.

---

## 🟢 GPT-5.4 — "Correct, Operable, Scalable"

### Philosophy

> Mato's core loop is already strong: queueing, claiming, isolation, review, and merge all exist. The next features should **improve correctness, operability, and scale of use** — not just add surface area.

GPT-5.4 focuses on making the existing system more robust and production-ready, emphasizing execution policy and observability.

### Feature 1: DAG-Aware Dependency Scheduler

**Problem:** Only direct `depends_on` relationships are checked. No full DAG reasoning, no robust cycle prevention, weak explanations for why a task is blocked.

**Proposed Design:**
- New file: `internal/queue/dag.go` with `BuildDependencyGraph()`, `DetectCycles()`, `BlockedReasons()`, `ReadyTasks()`
- Extend `internal/queue/reconcile.go` to compute full graph, reject/quarantine cyclic tasks, move only truly-ready tasks, emit precise blocking reasons
- Extend `internal/status/` with dependency chains, cycle reports, "blocked by X → Y → Z"
- Optional CLI: `mato status --deps`, reusable by `mato doctor`

**Impact:** Upgrades mato from "queue with dependency hints" to a real orchestrator. Prevents deadlocks, improves scheduling quality, gives users immediate clarity.

**Effort:** Medium (3–5 days)

### Feature 2: Per-Task Execution Profiles

**Problem:** Docker image is fixed per run, limits are global. A docs task, a Go refactor, and a Node migration shouldn't all run in the same environment with the same timeout.

**Proposed Design:**
- New frontmatter fields: `profile`, or `runtime.image`, `runtime.timeout`, `runtime.cpu`, `runtime.memory`
- New file: `internal/runner/profile.go` to resolve profiles
- Repo-level profile definitions: `.mato/profiles/*.yaml`
- Update runner to choose image per task, apply Docker resource flags, fall back to global defaults
- CLI: `mato run --profile-dir .mato/profiles`, `mato status` shows active profile

**Example:**
```yaml
profile: go-default
# or
runtime:
  image: ghcr.io/org/mato-go:latest
  timeout: 20m
  cpu: "2"
  memory: "4g"
```

**Impact:** Unlocks mato for heterogeneous repos and production use. Improves reliability, reduces wasted compute, avoids over-provisioning.

**Effort:** Medium (3–5 days)

### Feature 3: Event Journal + Live Status Stream

**Problem:** Mato has scattered state but no durable, queryable run history and no true live observability. `mato status` is a point-in-time snapshot.

**Proposed Design:**
- New package: `internal/history/`
- Append-only JSONL event records to `.tasks/history/YYYY/MM/DD/events.jsonl`
- Record all state transitions: claimed, started, progress, review, merge, retries, failures, recoveries
- Extend `internal/status/` with `mato status --json`, `mato status --follow`, `mato status --since 1h`
- Optional: `mato dashboard` serving tiny local web UI via `internal/status/server.go` (SSE or polling backed by journal)

**Impact:** Solves two gaps at once: observability and auditability. Makes debugging, demos, adoption, and postmortems dramatically easier. Creates foundation for future web UI without inventing a second source of truth.

**Effort:** Large (1–2 weeks)

### Feature 4: `mato doctor` Diagnostics and Safe Repair

**Problem:** When mato misbehaves, users infer problems from filesystem state, Docker availability, locks, and metadata. Too much operational knowledge required for a CLI tool.

**Proposed Design:**
- New package: `internal/doctor/`
- New command: `mato doctor`
- Checks: Docker reachable, CLI tools available, repo cleanliness, malformed frontmatter, unknown/circular dependencies, stale locks, orphaned tasks, duplicate IDs, impossible queue states
- Modes: `mato doctor`, `mato doctor --json`, `mato doctor --fix` (safe repairs: remove stale locks, recover orphans, rebuild derived state)

**Impact:** Force multiplier. Cuts setup friction, reduces support burden, makes mato feel trustworthy. Every serious CLI orchestrator needs a diagnostic story.

**Effort:** Medium (3–5 days)

### Feature 5: Task Templates and Batch Generation

**Problem:** Creating high-quality task files is manual. Hurts consistency, slows adoption, makes large backlogs tedious.

**Proposed Design:**
- New package: `internal/template/`, optional `internal/batch/`
- Repo structure: `.mato/templates/*.md.tmpl`, `.mato/batches/*.yaml`
- Commands: `mato template list`, `mato template render feature --var name=auth`, `mato batch create sprint-12.yaml`
- Go `text/template` with schema validation for required variables
- Generated files land in `.tasks/backlog/` or `.tasks/waiting/` with normalized frontmatter

**Impact:** Makes mato scalable for real planning workflows. Better task inputs lead directly to better autonomous execution quality.

**Effort:** Medium (3–5 days)

### Rebuttals

> "A web UI should be top priority."

Not yet. A UI without a durable event model becomes a thin veneer over fragile filesystem scraping. Build the event journal first; then a dashboard becomes cheap and correct.

> "Templates are just convenience."

In agentic systems, task quality is throughput quality. Templates reduce ambiguity, enforce metadata hygiene, and make batch planning practical.

> "Per-task profiles add too much complexity."

Only if they allow arbitrary Docker flags. Keep it safe: prefer named profiles in `.mato/profiles/`, with controlled overrides. Flexibility without turning mato into a container orchestration free-for-all.

> "Doctor can wait."

Operational trust cannot. When a tool coordinates agents, git, Docker, locks, and queue state, diagnosis is not optional — it is product quality.

---

## 🔵 Gemini 3.1 Pro — "Developer Experience & Observability"

### Philosophy

> While my colleagues make excellent points regarding stability, I believe we must aim higher than just "fixing" the current system. To evolve from a "task runner" to a true "orchestration platform," we must address the friction points that slow down agent development and observability. Mato's concurrency model is already sufficient — focus on DX.

Gemini focuses on operator experience and runtime flexibility, arguing that the system's biggest gaps are in visibility and usability, not scheduling algorithms.

### Feature 1: Interactive TUI Dashboard (`mato monitor`)

**Problem:** `mato status` provides a static snapshot. Engineers running long-lived agent swarms have no visibility into real-time progress without manually refreshing. Hard to spot stuck agents or visualize queue flow.

**Proposed Design:**
- New command: `mato monitor` using Bubble Tea framework (`charmbracelet/bubbletea`)
- Left pane: task queue stats (Waiting: 5, In-Progress: 2, Done: 10)
- Center pane: live list of active agents showing Task ID, Agent ID, Duration
- Footer: last 5 system events (log tail)
- Implementation: `cmd/mato/monitor.go` with `fsnotify` watching `.tasks/` for event-driven updates

**Impact:** Transforms mato from a "black box" into a transparent system. Crucial for debugging multi-agent contentions.

**Effort:** Medium (3–5 days)

### Feature 2: Glob Pattern Support for `affects`

**Problem:** Exact string matching in resource locking is brittle. Modifying `internal/queue/queue.go` often requires locking the entire package. Listing every file is impossible; locking just the folder string is ambiguous without explicit support.

**Proposed Design:**
- Allow `affects: ["internal/queue/**/*.go"]` syntax
- Replace exact-match map lookup with `path/filepath.Match` or `bmatcuk/doublestar`
- Performance: O(N) instead of O(1), but with N < 1000 active tasks, overhead is negligible compared to Docker startup time

**Impact:** Drastically reduces "lock missing" bugs where agents overwrite each other's work.

**Effort:** Small (1–2 days)

### Feature 3: Task Scaffolding & Templates

**Problem:** Creating tasks manually is error-prone. Users forget `affects`, misformat YAML, or forget initial state. No standardized way to inject context (like coding guidelines) into every task.

**Proposed Design:**
- Template directory: `.tasks/.templates/*.md`
- New command: `mato create <template-name> --title "Fix Bug" --param priority=high`
- Use Go `text/template` with `cmd/mato/create.go`
- Default templates: `bugfix`, `feature`, `refactor`

**Impact:** Standardizes task quality and context injection, leading to higher agent success rates.

**Effort:** Small (1–2 days)

### Feature 4: Dependency Cycle Detection & Visualization

**Problem:** `depends_on` has no validation. Users can create deadlocks (A waits for B, B waits for A) causing agents to spin forever. No way to visualize the critical path.

**Proposed Design:**
- New command: `mato graph [--check-cycles]`
- Build DAG from all task files, run DFS to detect back-edges
- If cycle found during `mato run` startup, error out with cycle path
- `mato graph` outputs DOT format (Graphviz) for visualization

**Impact:** Prevents "silent failures" where the system hangs indefinitely due to bad configuration.

**Effort:** Medium (3–5 days)

### Feature 5: Polyglot Runtime Support (Per-Task Docker Images)

**Problem:** Single runtime environment for all tasks. Monorepos with Go backend, TypeScript frontend, and Python scripts can't use mato effectively without a bloated "mega-image."

**Proposed Design:**
- New frontmatter field: `runtime.image` and `runtime.env`
- Update `internal/frontmatter` struct and `internal/runner` to read and use per-task images
- Fall back to global default if not specified

**Example:**
```yaml
runtime:
  image: "node:20-alpine"
  env:
    NODE_ENV: "production"
```

**Impact:** Unlocks mato for full-stack repository orchestration.

**Effort:** Medium (3–5 days)

### Rebuttals

> "Why a TUI instead of a Web UI?"

A Web UI introduces HTTP server, WebSocket, frontend assets, security concerns (CORS, auth). Mato is a CLI tool for terminals and CI. A TUI via Bubble Tea uses the existing TTY, requires no ports, and fits Unix philosophy.

> "Glob matching is slow."

Premature optimization. Even with 500 tasks, glob comparison in Go takes microseconds. The bottleneck is Docker startup (seconds) and LLM inference (seconds).

> "Just use one big Docker image."

Mega-images are slow to pull, hard to cache, and riddled with version conflicts (Python 3.9 vs 3.11). Per-task images let agents bring their own tools.

---

## Cross-Debate Analysis

### Where All Three Agree

1. **Glob Affects Matching** — All three agents identified exact-string matching as fundamentally brittle. Strongest consensus feature. Two ranked it #1-2, the third included it. Low effort, high impact.

2. **DAG Dependency Resolution** — All three include some form of dependency graph improvement. GPT and Opus propose comprehensive DAG packages; Gemini focuses on cycle detection and visualization.

3. **Task Templates** — All three include template/scaffolding features. Universal agreement that manual task authoring is the human bottleneck.

### Where Two Agree (Strong Signal)

4. **`mato doctor` Command** — Both Opus (#2) and GPT (#4) propose a comprehensive diagnostic command. Strong signal for operability improvement.

5. **Structured Audit Trail** — Both Opus (#4) and GPT (#3) propose JSONL event logging. Foundation for all future observability.

6. **Per-Task Execution Environments** — Both GPT (#2) and Gemini (#5) propose per-task Docker images and resource profiles. Enables polyglot orchestration.

### Key Disagreements

| Topic | Correctness Camp (Opus, GPT) | DX Camp (Gemini) |
|-------|------------------------------|------------------|
| **Top priority** | Glob matching + `mato doctor` | TUI dashboard |
| **Observability approach** | JSONL audit log first, UI later | TUI first (real-time ops) |
| **DAG depth** | Full topological sort + transitive context | Cycle detection + DOT output |
| **Template scope** | Batch generation + CI/CD integration | Interactive scaffolding |

### Recommended Implementation Order

Based on the debate, ordered by consensus strength and effort:

| Phase | Feature | Effort | Rationale |
|-------|---------|--------|-----------|
| **Phase 1** | Glob Affects Matching | Small (1-2 days) | Unanimous consensus, highest ROI, prevents real failures |
| **Phase 1** | `mato doctor` | Small (1-2 days) | Quick win, strong consensus, immediate UX improvement |
| **Phase 2** | DAG Dependency Resolution | Medium (3-5 days) | All three agree deps need improvement; prevents deadlocks |
| **Phase 2** | Structured Audit Trail | Medium (3-5 days) | Foundation for all future observability features |
| **Phase 3** | Per-Task Execution Profiles | Medium (3-5 days) | Enables polyglot repos, production hardening |
| **Phase 3** | Task Templates + Batch Gen | Medium (3-5 days) | Removes human authoring bottleneck, enables CI/CD |
| **Phase 4** | Interactive TUI Dashboard | Medium (3-5 days) | Best built on top of audit trail data |

**Total estimated effort:** ~4-5 weeks for all features across phases.

---

## Conclusion

With concurrency and cancellation already handled, the debate reveals clear priorities for what mato actually needs:

1. **Glob Affects Matching** — Unanimous consensus. The simplest, highest-impact change. Prevents the most expensive failure mode (merge conflicts from missed overlaps).

2. **`mato doctor`** — Strong consensus. Lowest-effort diagnostic command that makes mato immediately more approachable and debuggable.

3. **DAG Dependency Resolution** — All three agents agree dependencies need improvement. Prevents silent deadlocks, enables deep task chains, surfaces cycles prominently.

4. **Structured Audit Trail** — Strong consensus. Production-grade observability without infrastructure. Foundation for any future UI.

5. **Per-Task Execution Profiles** — Two agents agree. Unlocks polyglot orchestration and proper resource management.

6. **Task Templates** — Universal agreement this is valuable, but lower priority than correctness and observability features.

7. **TUI Dashboard** — Gemini's #1 pick, but best built after the audit trail provides reliable data to display.
