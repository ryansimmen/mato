# Inter-Agent Communication for mato — Evaluation & Recommendation

## Approaches Evaluated

| # | Approach | Transport | New Dependencies | Complexity |
|---|----------|-----------|------------------|------------|
| 1 | File-based messaging | Shared `.tasks/messages/` directory | None | Low-Medium |
| 2 | SQLite shared database | `.tasks/coordination.db` | `modernc.org/sqlite` | Medium |
| 3 | Git-based refs | `refs/mato/messages/...` custom refs | None | Medium |
| 4 | HTTP/REST API | Host-embedded HTTP server | None (stdlib) | Medium-High |
| 5 | Unix domain socket | Host-brokered `.tasks/.broker/mato.sock` | None (stdlib) | Medium-High |

---

## Comparative Analysis

### Alignment with mato's Architecture

mato's design is simple and deliberate: filesystem-based task queue, Docker containers with bind mounts, temporary git clones, no long-running services beyond the mato loop itself.

| Criterion | Files | SQLite | Git Refs | HTTP | Unix Socket |
|-----------|-------|--------|----------|------|-------------|
| Uses existing shared mount | ✅ | ✅ | ❌ (uses git) | ❌ (network) | ✅ |
| No new daemon/server | ✅ | ✅ | ✅ | ❌ | ❌ |
| Survives container restart | ✅ | ✅ | ✅ | ❌ (in-memory) | ❌ (in-memory) |
| Works across mato processes | ✅ | ✅ | ✅ | ❌ (per-process) | ❌ (per-process) |
| No new Go dependency | ✅ | ❌ | ✅ | ✅ | ✅ |
| Agents can use from shell | ✅ | ❌ (needs CLI) | ❌ (needs CLI) | ✅ (curl) | ❌ (needs CLI) |
| Structured queries | ❌ | ✅ | ❌ | ✅ | ⚠️ (limited) |
| Auditable history | ✅ | ✅ | ✅✅ (git native) | ⚠️ (if persisted) | ❌ |

### Key Architectural Concern: Multi-Process Parallelism

mato's current parallelism model is **multiple independent mato processes** in separate terminals. This is a critical differentiator:

- **Files, SQLite, Git refs** all work across multiple mato host processes because the shared medium (filesystem or git repo) is independent of any single process.
- **HTTP and Unix socket** are per-process brokers. They only coordinate agents launched by the same mato instance. Multi-process support requires broker discovery/election — significant additional complexity.

This alone makes approaches 4 and 5 a poor fit for mato's current concurrency model.

### Complexity vs. Value

| Approach | MVP Effort | Lines of Code | Value for Effort |
|----------|-----------|---------------|------------------|
| 1. Files | 1-2 days | ~250-400 | ⭐⭐⭐⭐⭐ |
| 2. SQLite | 2-3 days | ~400-700 | ⭐⭐⭐⭐ |
| 3. Git refs | 1-2 days | ~300-500 | ⭐⭐⭐ |
| 4. HTTP API | 1.5-3 days | ~500-900 | ⭐⭐ |
| 5. Unix socket | 2-3 days | ~500-800 | ⭐⭐ |

---

## Individual Assessments

### 1. File-Based Messaging — ⭐⭐⭐⭐⭐ Recommended

**Strengths:** This is the most natural extension of mato's existing design. It requires zero new dependencies, zero new infrastructure, and zero Docker networking changes. Messages are plain JSON files in `.tasks/messages/events/` — agents can write them with `echo`/`cat` and read them with shell tools. The host process only handles directory creation and garbage collection. It works perfectly across multiple mato processes because the filesystem is the shared medium.

**Weaknesses:** No push delivery (polling only), approximate ordering, directory scans can grow if cleanup is neglected. Advisory only — cannot enforce file locks.

**Verdict:** The best fit for mato's philosophy of simplicity. Low risk, low complexity, and covers the most important use cases (intent broadcasting, completion announcements, conflict warnings).

### 2. SQLite Shared Database — ⭐⭐⭐⭐ Strong Alternative

**Strengths:** Structured queries are genuinely useful ("who is editing main.go?", "show me all warnings for my task"). WAL mode handles mato's concurrency level well. Works across multiple mato processes. Could eventually replace the `.locks/` PID mechanism.

**Weaknesses:** Adds a Go dependency (`modernc.org/sqlite`). Agents can't use it from shell directly — requires a helper CLI. Dual source of truth (filesystem queue + DB) adds cognitive load. SQLite on non-local filesystems is risky.

**Verdict:** The best choice if you need rich querying and structured coordination. Slightly over-engineered for a first implementation but has the best long-term ceiling.

### 3. Git-Based Refs — ⭐⭐⭐ Interesting but Complex

**Strengths:** Elegant idea — messages are immutable git commit objects under custom refs. No new infrastructure, survives restarts, fully auditable with standard git tools. Conflict-free because each message gets its own unique ref.

**Weaknesses:** Git is not a message bus. Extra fetch/push overhead on every poll. Custom refs are a power-user Git feature that's unfamiliar territory. Requires a helper CLI (`mato comm`) because raw `git commit-tree` plumbing is too complex for LLM agents. Retention management (pruning old refs) is required.

**Verdict:** Clever and durable, but the implementation complexity is higher than it appears. The indirection through git plumbing commands makes it harder to debug than files or SQLite.

### 4. HTTP/REST API — ⭐⭐ Not Recommended

**Strengths:** Real-time, structured, queryable. Agents can use curl. Clear extension path to dashboards and UIs.

**Weaknesses:** Introduces Docker networking complexity (host.docker.internal, port management, auth tokens). Single point of failure. Does NOT work across multiple mato processes without additional coordination. More moving parts in a previously simple system. Security considerations (binding, auth).

**Verdict:** Over-engineered for mato. The networking overhead contradicts mato's design principle of simplicity through filesystem sharing.

### 5. Unix Domain Socket — ⭐⭐ Not Recommended

**Strengths:** Low latency, no network stack, piggybacks on existing `.tasks/` mount.

**Weaknesses:** Same per-process broker limitation as HTTP. Socket file lifecycle is operationally fragile. Agents need a client tool (no shell-native way to use Unix sockets). Linux-first behavior. Best-effort unless persistence is added.

**Verdict:** Similar tradeoffs to HTTP but with worse ergonomics (no curl equivalent). The per-process broker limitation is a dealbreaker given mato's multi-process parallelism model.

---

## Recommendation: File-Based Messaging (Approach 1)

**File-based messaging is the clear winner for mato.** Here's why:

1. **Architectural alignment** — It extends the exact pattern mato already uses (`.tasks/` filesystem queue). No paradigm shift required.

2. **Multi-process safe** — Works correctly when users run multiple `mato` instances in separate terminals, which is the documented parallelism model.

3. **Zero dependencies** — No new Go modules, no new services, no Docker networking changes.

4. **Agent-friendly** — LLM agents already reason about files and shell commands. Writing a JSON message file is trivial compared to calling an API or using git plumbing.

5. **Graceful degradation** — If messaging fails or agents ignore it, task execution still works perfectly. Communication is purely advisory.

6. **Lowest risk** — The implementation touches the fewest critical paths and introduces the least new failure modes.

### Suggested Implementation Path

**Phase 1 (MVP, 1-2 days):**
- Add `.tasks/messages/events/` and `.tasks/messages/presence/` directories
- Agents write JSON message files with atomic rename
- Add cleanup/GC in the mato host loop
- Update `task-instructions.md` with messaging steps (announce intent, check messages before merge, announce completion)

**Phase 2 (If structured queries become needed):**
- Migrate to SQLite (Approach 2) as the storage backend while keeping the same message semantics
- The file-based MVP validates the message types and prompt instructions before investing in richer infrastructure

This phased approach lets you ship useful coordination quickly and upgrade the storage layer only if the use cases demand it.
