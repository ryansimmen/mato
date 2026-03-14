# Task Storage & Organization for mato — Evaluation & Recommendation

## Research Areas

| # | Topic | Document |
|---|-------|----------|
| 1 | Storage locations | `approach-1-storage-locations.md` |
| 2 | Dependency modeling | `approach-2-dependency-modeling.md` |
| 3 | Prioritization & ordering | `approach-3-prioritization.md` |
| 4 | Prior art analysis | `approach-4-prior-art.md` |
| 5 | Task file format | `approach-5-task-format.md` |

---

## Summary of Findings

### Where to Store Tasks

| Location | Persistence | Git Safety | Multi-Process | Multi-Repo | Portability |
|----------|------------|------------|---------------|------------|-------------|
| In-repo `.tasks/` (current) | ✅ | ⚠️ gitignore needed | ✅ | ✅ (per-repo) | ✅ |
| `~/.mato/<repo-hash>/` | ✅ | ✅ | ✅ | ✅ | ✅ |
| `$XDG_DATA_HOME/mato/<hash>/` | ✅ | ✅ | ✅ | ✅ | ⚠️ (XDG is Linux-centric) |
| Sibling `../.mato-<repo>/` | ✅ | ✅ | ✅ | ✅ | ⚠️ (fragile) |
| `$TMPDIR/mato/<hash>/` | ❌ lost on reboot | ✅ | ✅ | ✅ | ✅ |
| `--tasks-dir <path>` | depends | depends | ✅ | user-managed | ✅ |

**Recommendation: Keep tasks in-repo (`.tasks/`) as the default.** The current approach is the simplest, most discoverable option. Tasks live where the code lives. The XDG approach is cleaner in theory but adds indirection — users can't just `ls .tasks/backlog/` to see what's pending. The in-repo location also simplifies Docker mounts (already working). Add `--tasks-dir` as an escape hatch for users who want tasks elsewhere. Revisit if git safety becomes a real problem in practice.

### How to Express Dependencies

| Approach | Self-Contained | Atomic Claim | Complexity | Flexibility |
|----------|---------------|--------------|------------|-------------|
| YAML frontmatter | ✅ | ✅ | Low | High |
| Manifest file | ❌ (separate file) | ✅ | Medium | Highest |
| Directory-based ordering | ✅ | ⚠️ (recursive) | Low | Low |
| Filename convention | ✅ | ✅ | Lowest | Lowest |
| Sidecar `.deps` files | ❌ (two files) | ❌ (must move pair) | Medium | Medium |

**Recommendation: YAML frontmatter in task files.** Metadata travels with the task through every state transition. The atomic `mv` claim flow is unchanged — one file, one move. It's human-readable, well-supported in Go without heavy dependencies, and familiar from Hugo/Jekyll. Agents can ignore it; the host parses it.

### How to Prioritize and Order Tasks

| Strategy | Host Complexity | Prompt Complexity | Reliability | Best For |
|----------|----------------|-------------------|-------------|----------|
| Host-side ready queue | Medium-High | Low | High | Strong dep enforcement |
| Agent-side dep checking | Low | High | Low | Quick prototype |
| Priority field only | Low | Medium | Medium | Ranking without deps |
| **Hybrid** (host readiness + agent priority) | **Medium** | **Medium-Low** | **High** | **Recommended** |

**Recommendation: Hybrid approach.** The host manages a `waiting/` → `backlog/` promotion loop. Tasks with unmet dependencies stay in `waiting/`. When all deps are satisfied, the host moves them to `backlog/`. Agents only see ready tasks and pick the highest-priority one. This keeps the agent prompt simple while the host handles the hard graph logic.

### Task File Format

| Format | Parsing | Human Readability | LLM Friendliness | GitHub Rendering | Backward Compat |
|--------|---------|-------------------|-------------------|------------------|-----------------|
| **YAML frontmatter** | Easy (stdlib) | ✅ | ✅ (strip before prompt) | ⚠️ (renders ok) | ✅ |
| TOML frontmatter | Easy | ✅ | ✅ | ❌ (not standard) | ✅ |
| HTML comments | Regex | ⚠️ (hidden) | ✅ | ✅ (invisible) | ✅ |
| Metadata section | Regex | ✅ | ❌ (agents may read it) | ✅ | ✅ |
| Sidecar file | Trivial | ✅ | ✅ | ✅ | ❌ (breaks claim) |

**Recommendation: YAML frontmatter for author metadata, HTML comments for runtime metadata.** This gives a clean separation:
- **Authors** write structured metadata (deps, priority) in frontmatter
- **mato** appends runtime state (claimed-by, failure records) as HTML comments
- **Agents** receive only the stripped markdown body as their prompt

### Key Prior Art Lessons

All surveyed systems (Make, GitHub Actions, Airflow, Bazel, etc.) agree on these principles:
1. **The scheduler owns the graph, not the workers.** Agents should never compute dependencies.
2. **`backlog/` should mean "ready now."** Separate waiting/blocked tasks from claimable ones.
3. **Declare direct dependencies explicitly.** Don't infer from filenames or directories.
4. **Validate early, fail fast.** Reject cycles and missing deps before launching any agent.
5. **Keep state transitions atomic.** The Maildir/spool model (atomic `mv`) is proven and correct.

---

## Unified Recommendation

### Task File Format

```markdown
---
id: add-retry-logic
priority: 10
depends_on:
  - setup-http-client
tags: [backend, networking]
estimated_complexity: medium
---
# Add retry logic to HTTP client

The fetchData function in pkg/client/http.go does not retry on transient
failures. Wrap calls in a retry loop with exponential backoff (3 attempts,
starting at 500ms). Add tests covering retry on 503 and success on second
attempt.
```

- `id` defaults to filename (without `.md`) if omitted
- `priority` is numeric, lower = higher priority (default: 50)
- `depends_on` lists task IDs that must be in `completed/` before this task is claimable
- Tasks without frontmatter continue to work (backward compatible)

### Directory Layout

```
.tasks/
├── waiting/        # tasks with unmet dependencies (new)
├── backlog/        # ready to claim (all deps satisfied)
├── in-progress/    # claimed by an active agent
├── completed/      # finished successfully
├── failed/         # exceeded retry limit
└── .locks/         # PID locks for concurrent agents
```

### Host Loop (revised `run()`)

```
for each iteration:
  1. recoverOrphanedTasks()
  2. cleanStaleLocks()
  3. reconcileReadyQueue()    ← NEW: scan waiting/, promote ready tasks to backlog/
  4. if no tasks in backlog/, wait and poll
  5. runOnce(...)
```

`reconcileReadyQueue()`:
- Parse frontmatter of all tasks in `waiting/`
- For each task, check if all `depends_on` IDs exist in `completed/`
- If yes, atomically `mv` from `waiting/` to `backlog/`
- Detect cycles and missing deps at validation time

### Agent Behavior (no change to core flow)

Agents continue to:
1. List `.md` files in `backlog/`
2. Sort by priority (from frontmatter) — lowest number first
3. Claim by `mv` to `in-progress/`
4. Work, commit, merge, move to `completed/`

The agent prompt is updated to: "Pick the task with the lowest `priority` value from the frontmatter. If no frontmatter, treat priority as 50."

### Task Storage

Keep tasks in `.tasks/` inside the repo (current approach). Add `--tasks-dir` flag as an override for users who want tasks stored elsewhere.

### Implementation Phases

**Phase 1: Frontmatter parsing + waiting/ directory**
- Add YAML frontmatter parser (Go stdlib `encoding/yaml` or lightweight regex for the small schema)
- Add `waiting/` directory
- Add `reconcileReadyQueue()` to the host loop
- Update `task-instructions.md` to teach agents about priority sorting
- Backward compatible: tasks without frontmatter go straight to `backlog/`

**Phase 2: Validation and observability**
- Cycle detection in dependency graph
- `mato status` command to show task DAG and current state
- Missing dependency warnings

**Phase 3: Advanced features (if needed)**
- Soft dependencies (`soft_depends_on` — prefer ordering but don't block)
- Retry cooldown (tasks stay in `waiting/` for N seconds after failure)
- `--tasks-dir` flag for external storage
