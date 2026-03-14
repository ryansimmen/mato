# Agent Prompt Engineering — Evaluation & Recommendation

## Research Areas

| # | Topic | Document |
|---|-------|----------|
| 1 | Prompt structure & formatting | `approach-1-prompt-structure.md` |
| 2 | Frontmatter & priority handling | `approach-2-frontmatter-handling.md` |
| 3 | Messaging system prompts | `approach-3-messaging-prompts.md` |
| 4 | Error recovery patterns | `approach-4-error-recovery.md` |

---

## Key Findings

### The Current Prompt Works — But Is at Its Limit

The existing `task-instructions.md` is effective for its current linear happy path. But adding frontmatter parsing, dependency awareness, messaging, and better error handling will push it past the point where a linear step list works reliably. The research unanimously recommends restructuring.

### The Biggest Insight: Move Logic to the Host, Not the Prompt

Every research document converges on the same principle: **the host should do the hard work, agents should follow simple rules.** This applies across all new features:

- **Dependencies**: Host manages `waiting/` → `backlog/`. Agents never evaluate `depends_on`.
- **Priority**: Host generates an ordered `.queue` manifest. Agents read line 1.
- **Messaging**: Agents write/read at fixed checkpoints only. No free-form coordination.
- **Merging**: Host handles merge queue (see merge-strategy recommendation). Agents just push task branches.
- **Error recovery**: Bounded retry loops with explicit abort conditions. No open-ended "try to fix it."

---

## Recommendations

### 1. Restructure the Prompt as a State Machine

Replace the current "8 steps in exact order" with a **workflow spine plus local decision tables**:

```
States: SELECT → CLAIM → BRANCH → WORK → COMMIT → PUSH → FINALIZE
                                                          ↓
                                                       ON_FAILURE
```

Each state gets:
- A 1-2 line description of its goal
- Exact commands to run
- A small decision table for branches (e.g., "if tests fail → retry up to 3 times → if still failing → ON_FAILURE")

This is more modular than the current linear list and handles the new conditional logic (messaging checkpoints, priority sorting) without becoming a tangled mess.

### 2. Host-Generated `.queue` File for Task Selection

**Don't ask agents to parse frontmatter and sort.** Instead:

- The host writes `.tasks/.queue` — a plain text file listing ready tasks in priority order, one filename per line
- The agent prompt says: "Read `.tasks/.queue`. Claim the first unclaimed task."
- If `.queue` doesn't exist (backward compat), fall back to alphabetical listing of `backlog/`

This completely eliminates agent-side YAML parsing and sorting logic. The host already runs `reconcileReadyQueue()` — writing a `.queue` file is trivial.

### 3. Three Fixed Messaging Checkpoints

Agents message at exactly three points — no more:

| Checkpoint | When | Message Type |
|------------|------|-------------|
| After claiming task | Before starting work | `intent` — announces task and planned files |
| Before pushing branch | After committing | `conflict-warning` — lists files changed |
| After marking complete | Task is done | `completion` — announces what was merged |

Plus one mandatory **read** checkpoint:
- **Before starting work**: scan messages for active intent/conflict-warnings from other agents on overlapping files

**Guardrails:**
- Maximum 3 messages per task (hard rule in prompt)
- Messages are JSON files written with `cat > file.json << 'EOF'` (atomic via temp + mv)
- If messaging fails, continue with task (never block on communication)

### 4. Bounded Error Recovery

Replace the current minimal "On Failure" with structured recovery:

| Failure Type | Action | Max Attempts |
|-------------|--------|-------------|
| Test failure | Fix and retry | 3 |
| Build error | Fix and retry | 3 |
| Merge conflict | Resolve and retry | 3 |
| Push rejection | Re-fetch, re-merge, retry | 3 |
| Ambiguous task | Best effort, note uncertainty | 1 |
| Impossible task | Fail immediately with explanation | 0 |

**Key rules:**
- Never `git push --force`
- Never delete files you didn't create
- Never modify files unrelated to the task
- After max retries, go to ON_FAILURE: append `<!-- failure: ... -->`, move to `backlog/`

**Richer failure metadata:**
```html
<!-- failure: agent-id at 2026-03-14T12:00:00Z
  step: WORK
  error: test failure after 3 attempts
  files_changed: pkg/client/http.go, pkg/client/http_test.go
  last_error: TestRetryLogic/503_retry FAIL
-->
```

### 5. Prompt Size Target

| Section | Lines |
|---------|-------|
| Identity + invariants | 15-20 |
| Workflow spine (state overview) | 20-30 |
| State details (SELECT through FINALIZE) | 80-100 |
| Messaging protocol | 30-40 |
| Error recovery | 30-40 |
| Command templates appendix | 20-30 |
| **Total** | **~200-260** |

This is comparable to the current ~200 lines. The prompt doesn't grow much — it gets reorganized to be more modular and more robust.

### 6. Implementation Order

**Phase 1: Restructure existing prompt**
- Convert to state-machine format
- Improve error recovery with bounded loops
- No new features yet — just better reliability

**Phase 2: Add host-side queue + priority**
- Implement `.queue` manifest in Go
- Add queue-reading instructions to prompt
- Host strips frontmatter before prompt injection (agent never sees YAML)

**Phase 3: Add messaging checkpoints**
- Add the 3 fixed messaging points
- Add message-reading checkpoint
- Test with multiple concurrent agents

---

## Core Principle

> **The prompt is a simple script for a capable but literal executor. Every decision that can be made by the host should be made by the host. The agent's job is to execute, not to schedule.**
