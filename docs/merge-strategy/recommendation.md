# Merge Strategy & Conflict Resolution — Evaluation & Recommendation

## Research Areas

| # | Topic | Document |
|---|-------|----------|
| 1 | Merge strategy comparison | `approach-1-merge-patterns.md` |
| 2 | Conflict prevention | `approach-2-conflict-prevention.md` |
| 3 | Host-managed merge queue | `approach-3-host-merge-queue.md` |

---

## Key Findings

### The Current Approach Is the Weakest Part of mato

Today, each agent independently merges to the target branch, retrying up to 3 times on push failure. This is:
- **The most complex part of the agent prompt** (Step 7 is the longest, most error-prone step)
- **A race condition by design** (multiple agents pushing to the same branch tip)
- **The #1 cause of agent failures** (merge conflicts, push rejections, retry exhaustion)

All three research documents independently converge on the same conclusion: **move merge responsibility from agents to the host.**

### Strategy Comparison

| Strategy | Conflict Handling | Prompt Complexity | History | Reliability |
|----------|------------------|-------------------|---------|-------------|
| Agent merge + retry (current) | Agent resolves | High | Messy merge commits | Low under concurrency |
| Agent rebase + push | Agent resolves | Higher | Linear but fragile | Lower (rebase is harder) |
| Agent squash + push | Agent resolves | Medium | Clean | Medium |
| **Host serial merge queue** | **Host handles** | **Low** | **Clean** | **High** |
| GitHub-style merge queue | Host + CI | Lowest | Cleanest | Highest (but heavy) |

---

## Recommendation: Host-Managed Serial Merge Queue

### The Big Change

**Agents stop merging.** Their new workflow ends at "push task branch to origin." The host takes over from there.

### Revised Workflow

**Agent responsibilities (simplified):**
1. Claim task from backlog
2. Create `task/<name>` branch from target
3. Do work, run tests, commit
4. Push `task/<name>` to origin
5. Move task to `ready-to-merge/`
6. Done — exit

**Host responsibilities (new):**
1. Detect tasks in `ready-to-merge/`
2. Pick highest-priority branch
3. In a temp clone: merge task branch into target (squash by default)
4. Run validation (build/test) if configured
5. Push target branch to origin
6. Move task to `completed/`
7. If merge conflicts: move task back to `backlog/` for agent to refresh

### New Directory Layout

```
.tasks/
├── waiting/          # unmet dependencies
├── backlog/          # ready to claim
├── in-progress/      # claimed by agent
├── ready-to-merge/   # agent finished, awaiting host merge (new)
├── completed/        # merged to target branch
├── failed/           # exceeded retry limit
└── .locks/           # PID locks + merge lock
```

`ready-to-merge/` is the key addition. It separates "agent finished work" from "code is on the target branch," which is a critical distinction the current system conflates.

### Merge Queue Ordering

When multiple tasks are ready to merge:
1. **Dependency order** — tasks whose dependents are waiting go first
2. **Priority** — lower priority number wins
3. **Completion time** — first finished, first merged

### Conflict Handling

When the host encounters a merge conflict:
1. Don't try to auto-resolve (the host lacks task context)
2. Append a `<!-- failure: merge-conflict ... -->` record to the task file
3. Move task back to `backlog/`
4. The next agent to pick it up will work from a fresher base and likely avoid the conflict

This is safer than the current approach where agents attempt conflict resolution with limited context and often make it worse.

### Why Squash Merge

- One commit per task on the target branch — clean, auditable history
- Each commit message = task title + summary
- Reduces conflict surface (fewer intermediate commits to conflict with)
- Easy rollback: `git revert <one-commit>`
- Agent's internal branch history is preserved on `task/<name>` refs if needed for debugging

### Conflict Prevention (Complementary)

In addition to the merge queue, add **host-side overlap prevention**:

1. **`affects:` frontmatter** — tasks declare which files/directories they'll touch
2. **Host overlap detection** — if two ready tasks have overlapping `affects:`, run them sequentially (one stays in `waiting/`)
3. **Post-merge validation** — optionally run `make test` after each merge to catch semantic conflicts

This is the optimal combination:
- **Prevention** (scheduling) eliminates predictable conflicts
- **Detection** (merge queue) handles unpredictable conflicts
- **Validation** (post-merge tests) catches semantic breakage

### Impact on Agent Prompt

Step 7 (currently 50+ lines of merge/push/retry/conflict-resolution instructions) becomes:

```markdown
### Step 7: Push Your Branch

Push your task branch to origin:

    git push origin task/<branch-name>

If push fails, retry up to 3 times. If it still fails, follow the On Failure procedure.

Verify the push:

    git log --oneline -1

Move the task to ready-to-merge:

    mv ".tasks/in-progress/<filename>" ".tasks/ready-to-merge/<filename>"
```

That's ~10 lines instead of ~50. The entire merge conflict resolution section is removed from the agent prompt.

### Implementation Phases

**Phase 1: Basic host merge queue**
- Add `ready-to-merge/` directory
- Simplify agent prompt (push branch only)
- Add merge loop to host after `runOnce()` returns
- Squash merge in a temp clone
- Move to `completed/` on success, back to `backlog/` on conflict

**Phase 2: Validation and prevention**
- Add optional post-merge test execution
- Add `affects:` frontmatter
- Add overlap detection to prevent concurrent conflicting tasks

**Phase 3: Observability**
- `mato status` command showing queue state
- Merge audit log
- Branch cleanup (delete merged task branches)

### Multi-Process Considerations

Multiple mato host processes need to coordinate merges. Solution: a `.tasks/.locks/merge.lock` file. Only the process that holds the lock can merge. Others skip the merge step and let the lock holder handle it. This is consistent with the existing `.locks/` pattern.

---

## Summary

| What Changes | From | To |
|-------------|------|-----|
| Who merges | Agent (in prompt) | Host (in Go) |
| Merge type | Regular merge | Squash merge |
| Conflict resolution | Agent guesses | Host requeues for fresh attempt |
| Push target | Agent pushes target branch | Agent pushes task branch only |
| Prompt complexity | ~50 lines for Step 7 | ~10 lines for Step 7 |
| Task states | 4 (backlog → in-progress → completed/failed) | 5 (+ ready-to-merge) |
| Conflict prevention | None | Host scheduling + affects: metadata |

> **Bottom line: The single highest-impact change mato can make is moving merge responsibility from agents to the host. It fixes the concurrency race, dramatically simplifies the prompt, and gives the host full control over integration quality.**
