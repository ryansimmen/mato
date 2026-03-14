# Approach 4: Error recovery and edge-case prompt patterns

## Recommendation

Treat failure handling as a **first-class control flow** in the agent prompt, not as a catch-all footnote.

For mato, the most reliable pattern is:

1. **Classify failures early** so agents do not retry terminal problems.
2. **Bound every retry loop** with small, explicit budgets.
3. **Fail closed on safety-critical uncertainty** (unresolved conflicts, ambiguous product behavior, broken verification).
4. **Preserve useful partial work safely** on the task branch, but never merge or push broken code to the shared target branch.
5. **Write structured failure metadata** that is still backward-compatible with mato's existing `<!-- failure:` counting logic.

That matches mato's actual execution model in `main.go`:

- each agent works in a temporary clone inside Docker,
- the agent itself performs the merge and push,
- failures are retried by moving the task back to `backlog/`,
- orphan recovery already appends `<!-- failure: ... -->` records.

The prompt should therefore optimize for **safe autonomous recovery**, not for pretending every task is clean and deterministic.

---

## Current mato constraints that matter for prompting

From `main.go` and `task-instructions.md`, the agent is operating with these realities:

- The queue is filesystem-based: `backlog/`, `in-progress/`, `completed/`, `failed/`.
- Task claiming is atomic via `mv`, so claim failures are normal concurrency, not exceptional bugs.
- The agent runs in a temp clone but pushes back to the checked-out target branch in the origin repo.
- Multiple agents may merge into the same target branch concurrently.
- The host only understands failure count by counting `<!-- failure:` lines today.
- Orphan recovery already injects `<!-- failure: mato-recovery ... -->` comments.

That means prompt guidance must handle two classes of problems especially well:

1. **Integration problems** caused by concurrent agents.
2. **Task execution problems** caused by ambiguous instructions, failing tests, or environment issues.

---

## Design principles for reliable failure prompts

### 1. Separate transient failures from semantic failures
Agents do better when the prompt says **which failures deserve a retry** and which deserve an immediate abort.

### 2. Use budgets, not open-ended encouragement
"Try again" often becomes infinite loops. Prompts should say:

- retry this command at most 3 times,
- attempt repair at most 3 times,
- attempt conflict resolution once,
- if the same signature repeats twice, stop.

### 3. Fail closed on uncertainty
If the agent cannot explain a merge conflict, cannot determine intended behavior, or cannot get verification green, the safest action is to **stop and report**, not to guess.

### 4. Preserve work without contaminating the shared branch
When repair is exhausted, the agent can keep a WIP commit on the task branch for forensic value, but it should **not merge** that branch into `TARGET_BRANCH_PLACEHOLDER`.

### 5. Prefer decision tables over nested conditionals
A short taxonomy plus a few phase-specific budgets is easier for agents to follow than a long tree of `if X then Y else Z` exceptions.

---

## 1. Taxonomy of failures

The prompt should explicitly classify failures into a few operational buckets.

| Category | Typical examples | Recoverable? | Retry? | Default action |
| --- | --- | --- | --- | --- |
| **Transient infrastructure** | `git fetch` timeout, temporary push rejection due to race, file lock contention, intermittent network/auth hiccup | Usually yes | Yes, same command only, max 2-3 times with backoff | Retry the same step, then abort if unchanged |
| **Task-file / queue format** | empty task, unreadable file, missing `#` heading, malformed metadata, binary file in backlog | No until a human edits the task | No | Fail immediately; mark task failed or backlog with `retryable=no` note |
| **Ambiguous / impossible instructions** | contradictory requirements, missing acceptance criteria, references to nonexistent systems, impossible constraints | Sometimes | Only if ambiguity can be resolved from repo context | Proceed only on a conservative, well-supported interpretation; otherwise abort |
| **Work-product failures** | build error, test failure, lint failure caused by the agent's changes | Often yes | Yes, bounded repair loop | Attempt targeted fix up to N times, then stop |
| **Baseline failures** | tests or build already failing before changes | Sometimes | Not as a blind full-suite loop | Record baseline, validate changed scope, avoid owning unrelated failures |
| **Integration conflicts** | merge conflicts, non-fast-forward push, upstream branch changed during task | Sometimes | Yes, bounded integration loop | Refresh, merge, resolve once if safe, then retry push |
| **Environment / permission failures** | repo not writable, missing tool, no disk space, Docker mount issue, auth missing | Rarely from inside the task | At most 1 diagnostic retry | Abort quickly with detailed report |
| **Safety / repo integrity failures** | unresolved conflict markers remain, detached/unexpected branch state, corrupted `.git`, dirty target branch after failed merge | No | No | Abort immediately; do not continue |

### Recoverable vs terminal guidance

#### Recoverable by retrying the same command
Use this only for **idempotent, transient operations**:

- `git fetch origin`
- `git push origin TARGET_BRANCH_PLACEHOLDER` after a race
- reading a task file that was momentarily busy

Pattern:

- retry the same command,
- do not change strategy yet,
- max 3 attempts,
- exponential backoff: 1s, 2s, 4s.

#### Recoverable by repair work
Use this only when the failure is about code or tests:

- compile errors,
- failing tests,
- a small local conflict that the agent can explain.

Pattern:

- inspect the first failure cluster,
- make one targeted change,
- rerun the same verification,
- max 3 repair attempts.

#### Terminal for this run
Abort immediately when retries will not help:

- empty or malformed task file,
- missing critical tools,
- read-only workspace,
- unresolved ambiguity with multiple plausible product outcomes,
- merge conflict the agent cannot explain safely,
- repository corruption,
- unresolved conflict markers.

### Recommended retry/abort policy

A compact policy agents can follow reliably:

- **Transient infra:** retry up to 3 times.
- **Build/test failures:** repair up to 3 iterations.
- **Merge/push race:** integration loop up to 3 times total.
- **Ambiguous task / malformed task / unsafe merge / permission issue:** abort immediately.

---

## 2. Merge conflict handling

The current prompt is directionally right but too vague. "Preserve the intent of both changes" is correct, but not operational enough.

Agents do better when conflict resolution is framed as a **disciplined mini-workflow**.

## Recommended conflict-resolution pattern

### A. Stop and inspect before editing
The agent should not start editing conflict markers blindly.

Prompt pattern:

> If `git merge origin/TARGET_BRANCH_PLACEHOLDER --no-edit` reports conflicts, stop and inspect before making changes. List conflicted files with `git diff --name-only --diff-filter=U`. For each conflicted file, inspect the base, your version, and the target-branch version so you understand what each side changed.

Concrete commands worth naming in the prompt:

```bash
git diff --name-only --diff-filter=U
git show :1:path/to/file   # merge base
git show :2:path/to/file   # ours (task branch)
git show :3:path/to/file   # theirs (target branch)
```

This is much better than telling the agent to rely on the combined conflict block alone.

### B. Require a semantic explanation for each file
A very effective safeguard is to require the agent to answer two questions before resolving a file:

1. What changed on the task branch?
2. What changed on `origin/TARGET_BRANCH_PLACEHOLDER`?

Prompt pattern:

> Do not resolve a conflicted file until you can explain, in one or two sentences, what each side changed. If you cannot explain both sides, treat the conflict as unsafe and abort.

That prevents low-confidence edits that accidentally drop one side.

### C. Use file-type-specific heuristics
Agents are more reliable when the prompt tells them that not all conflicts are the same.

#### Generated files and lockfiles
Prompt pattern:

> If the conflicted file is generated or lock-like (for example a lockfile or generated artifact), prefer regenerating it from the authoritative source rather than hand-editing conflict markers, **if** the repo already has a standard command for that.

#### Config / manifest / JSON / YAML files
Prompt pattern:

> For structured config files, prefer an additive merge that preserves keys or entries from both sides, then validate the file parses cleanly.

#### Source code
Prompt pattern:

> For source files, keep both behavioral changes when possible. Do not resolve a code conflict by blindly choosing `ours` or `theirs` unless one side is clearly obsolete or generated.

#### Markdown / docs
Prompt pattern:

> For docs conflicts, prefer keeping both substantive edits unless they are duplicates or directly contradictory.

### D. Add explicit safety limits
Not every conflict should be auto-resolved.

Recommended stop conditions:

- more than **5 conflicted files**,
- conflict touches schema/migration/public API plus tests and the correct combination is unclear,
- the agent cannot explain both sides,
- focused verification still fails after resolution,
- any conflict marker remains.

Prompt pattern:

> If there are more than 5 conflicted files, or if you cannot determine a safe semantic resolution, abort the merge and follow On Failure. Do not guess.

### E. Verify after resolution
A common failure mode is resolving textually but not semantically.

Prompt pattern:

> After resolving all conflicts, verify there are no unmerged paths, no conflict markers remain, and run the smallest relevant build or test command for the affected files before completing the merge commit.

Concrete checks:

```bash
git diff --check
git diff --name-only --diff-filter=U
grep -R -n '^<<<<<<<\|^=======\|^>>>>>>>' .
```

### Stronger Step 7 merge wording

A better prompt for the merge section is:

> If a merge conflict occurs, resolve it file by file using a 3-way comparison (`base`, `ours`, `theirs`). Preserve both sides' intent when safe. Never leave conflict markers in the tree. Never blindly choose one side for source code. If you cannot explain a safe resolution, abort the merge and fail the task rather than risking data loss.

That is short enough to survive prompt bloat, but concrete enough to improve agent behavior.

---

## 3. Test failure loops

The current prompt says to run tests and fix issues, but it does not define:

- what command to use,
- how many times to iterate,
- what to do if the repo was already failing,
- when to stop.

## Recommended verification loop

### A. Establish a baseline when practical
Before large edits, the agent should run the most relevant verification command once.

Why this matters:

- if the repo was already red, the agent should not spend all of its budget fixing unrelated failures,
- the agent can distinguish **new regressions** from **pre-existing failures**.

Prompt pattern:

> Before major changes, run the most relevant existing verification command once to establish a baseline. If it already fails, record that baseline failure and focus on proving your change does not make it worse.

### B. Prefer a verification ladder
Agents should use the repo's existing verification hierarchy, not invent one.

Recommended order:

1. repo-standard command if clearly present (`make test`, documented test script, existing CI entrypoint),
2. language-standard command (`go test ./...`, `npm test`, `pytest`),
3. focused package/module tests for the files you changed,
4. if no tests exist, at least run a build or syntax check.

### C. Bound the repair loop
This is the most important change.

Prompt pattern:

> If verification fails because of your changes, enter a repair loop with a maximum of 3 iterations. In each iteration: inspect the first failure cluster, make one targeted change, rerun the same verification command, and compare the result. If the same failure signature repeats twice or the situation gets broader, stop.

This keeps the agent out of flailing behavior.

### D. Define failure signatures
Agents often keep retrying the same failure because the prompt does not tell them to detect repetition.

A practical failure signature can be:

- first failing test name,
- first compiler error line,
- first build target that failed.

Prompt pattern:

> Treat the first failing test name or first compiler/build error as the failure signature. If the same signature appears twice after separate repair attempts, stop the loop and report failure.

### E. Distinguish three outcomes
The prompt should explicitly separate:

1. **Green:** tests/build pass.
2. **Known baseline red:** baseline was already failing, but the changed scope now passes or is unchanged.
3. **Still broken after repair budget:** stop and do not merge.

### F. Preserve partial work safely after repair budget is exhausted
This directly addresses the user's concern.

Prompt pattern:

> If the repair budget is exhausted, do not keep iterating. If your branch contains useful partial progress, create a WIP commit on the task branch, record the commit SHA and remaining failure in the failure note, and do not merge that branch into `TARGET_BRANCH_PLACEHOLDER`.

Important nuance for mato:

- a WIP commit is valuable only if it is **preserved**,
- so if mato wants this to survive container teardown, the prompt should explicitly allow `git push origin <task-branch>` for failed tasks **without** merging into the target branch.

If branch preservation is not desired operationally, the prompt should instead say not to create WIP commits and to rely on the failure record alone. But if the goal is "commit what you have," then preserving the task branch is the practical version of that policy.

### Suggested bounded repair loop language

> Use a repair budget of 3 attempts. After each failed verification, fix one failure cluster and rerun the same command. Do not broaden the command during the loop. If the same failure signature repeats twice, or if you cannot produce a green or non-worsened result within 3 attempts, stop and follow On Failure.

---

## 4. Ambiguous task instructions

The prompt should not force an all-or-nothing choice between reckless guessing and immediate failure.

The reliable pattern is **best effort when ambiguity is local, abort when ambiguity is product-defining**.

## Recommended ambiguity policy

### Safe to proceed with best effort
Proceed when the ambiguity can be resolved from local evidence such as:

- adjacent code,
- existing tests,
- README/docs in the repo,
- established naming or API conventions,
- a clearly dominant interpretation that does not change external behavior.

Prompt pattern:

> If the task is slightly underspecified but the existing codebase strongly implies one conservative implementation, proceed with that interpretation and record the assumption in your final report.

### Unsafe to guess
Abort when the ambiguity changes behavior in material ways, for example:

- two plausible API shapes,
- unknown database or schema intent,
- user-visible behavior with no precedent,
- contradictory requirements,
- references to missing systems or files that cannot be inferred.

Prompt pattern:

> If multiple materially different implementations are plausible and the repo does not clearly favor one, do not guess. Treat the task as ambiguous, record exactly what information is missing, and fail the task for human clarification.

### Empty or malformed task files
These should be treated as **terminal task-definition failures**, not as retryable execution problems.

Prompt pattern:

> If the task file is empty, unreadable, lacks a usable Markdown title/body, or contains only metadata/comments, fail immediately. Retries will not fix a malformed task.

### Impossible tasks
Examples:

- requested file or dependency does not exist and cannot be added safely,
- required credentials or services are unavailable,
- task contradicts repository policy.

Prompt pattern:

> If the requested outcome is impossible in the current repo or environment, do not fabricate a solution. Record why it is impossible, what prerequisite is missing, and stop.

---

## 5. Cascading failure prevention

The prompt should include a small set of **hard safety invariants**. This is the main defense against agents making things worse.

## Recommended invariants

1. **Never push broken code to `TARGET_BRANCH_PLACEHOLDER`.**
   - Do not merge into the shared branch if required verification failed.

2. **Never force-push or rewrite shared branch history.**
   - No `git push --force`.
   - No destructive reset of the shared target branch.
   - Do not rebase the shared target branch.

3. **Never leave unresolved conflict state in a committed tree.**
   - No conflict markers.
   - No unmerged paths.

4. **Never resolve semantic conflicts by dropping one side silently.**
   - `ours`/`theirs` is allowed only when clearly justified, such as generated artifacts or deleted-on-one-side cases that are obviously correct.

5. **Never keep retrying without new information.**
   - repeated identical failure signature means stop.

6. **Do not delete unrelated files or clean the repo aggressively.**
   - no broad `rm -rf`,
   - no `git clean -fdx` unless the task explicitly requires it and the prompt author intends it,
   - no deleting `.tasks` metadata except normal queue movement.

7. **Do not widen scope to "fix the repo" unless the task requires it.**
   - if the repo has unrelated failing tests, record that rather than going on a side quest.

### High-value safety language

> When safety is uncertain, fail closed. It is better to return a task for retry or clarification than to merge broken or incomplete work into the shared branch.

That single sentence is worth adding explicitly.

---

## 6. Graceful degradation patterns

The goal is to make the prompt resilient **without** turning it into unreadable spaghetti.

The best way to do that is to define a few reusable policy blocks.

## Pattern A: phase-specific budgets

Instead of writing conditionals everywhere, define budgets once:

- **Transient retry budget:** 3 attempts
- **Repair budget:** 3 attempts
- **Integration budget:** 3 full fetch/merge/push cycles
- **Conflict-resolution budget:** 1 resolution pass; abort if still unsafe

Prompt pattern:

> Use these fixed budgets throughout the task unless the task file says otherwise: transient retries 3, repair attempts 3, integration attempts 3, conflict-resolution passes 1.

## Pattern B: fallback ladders

A ladder is easier for agents than free-form improvisation.

### Verification ladder

> Prefer the repository's standard test/build command. If none is obvious, use the language-standard command. If that is too broad for debugging, use the smallest relevant package or module command. If no tests exist, run the narrowest build or syntax check available.

### Task-understanding ladder

> Resolve unclear instructions in this order: task file, nearby code/tests, repo docs, existing conventions. If the answer is still unclear and affects behavior materially, stop.

### Conflict-resolution ladder

> Prefer semantic merge. If the file is generated, regenerate it. If the conflict remains unclear after inspection, abort rather than guessing.

## Pattern C: retry only on idempotent steps

A small but important rule:

> Only retry the same command when retrying is safe and idempotent. Do not repeatedly retry a step that changes repository state unless you first restore a clean state.

This is especially useful around merges and pushes.

## Pattern D: explicit stop triggers

Prompts become much more reliable when they say what ends the loop.

Recommended stop triggers:

- same failure signature twice,
- repair budget exhausted,
- more than 5 conflicted files,
- unresolved conflict markers,
- verification worsened from baseline,
- unsafe ambiguity remains.

---

## 7. Failure reporting

The current `<!-- failure: ... -->` format is enough for retry counting but not for diagnosis.

Because `main.go` already counts failure lines by matching `<!-- failure:`, the improved format should stay backward-compatible with that prefix.

## Recommended failure metadata

### Required fields

- `agent`: agent ID
- `at`: UTC timestamp
- `phase`: where it failed (`step-2`, `step-5`, `step-7`, etc.)
- `category`: taxonomy bucket
- `retryable`: `yes` or `no`
- `summary`: one-line human-readable reason

### High-value optional fields

- `attempt`: current attempt / max attempts
- `branch`: task branch name
- `head`: current commit SHA on task branch
- `target`: target branch name
- `target_head`: fetched target branch SHA if relevant
- `command`: command that failed
- `exit`: exit code or signal
- `files`: comma-separated list of touched/conflicted files
- `verify`: verification command and result summary
- `baseline`: whether failure existed before changes
- `next`: recommended next action for a human or next retry

## Backward-compatible failure line format

Recommended single-line comment:

```html
<!-- failure: agent=4f2a1c9d at=2026-03-14T19:17:28Z phase=step-7 category=integration retryable=yes attempt=2/3 branch=task/fix-auth head=abc1234 target=mato target_head=def5678 command="git push origin mato" exit=1 files="pkg/auth.go,pkg/router.go" summary="non-fast-forward after concurrent merge" next="retry integration from fresh fetch" -->
```

That is verbose, but it preserves the current host behavior and gives humans much better debugging context.

## Reporting policy recommendations

### On retryable failure
Append a structured failure line and move the task back to `backlog/`.

### On terminal failure
Append a structured failure line and move the task to `failed/` immediately if the failure is clearly not fixed by retry (malformed task, impossible requirement, missing tool, unsafe ambiguity). If mato wants to preserve its current retry-only flow, still use `retryable=no` so humans can see that automated retries are wasteful.

### If partial work is preserved
Also record:

- `branch=<task-branch>`
- `head=<commit-sha>`
- `next="inspect preserved task branch"`

---

## 8. Concrete prompt rewrites

Below are ready-to-drop rewrites for the most error-prone parts of `task-instructions.md`.

## 8.1 Add a short failure policy near the top

Place this near the workflow overview so the rest of the prompt can reference it:

```markdown
### Failure policy

Classify failures before reacting:

- **Transient** (fetch/push timeout, race, temporary lock): retry the same command up to 3 times with short backoff.
- **Repairable** (tests/build broken because of your changes): use a repair loop of up to 3 attempts.
- **Integration** (merge conflict, non-fast-forward): use the Step 7 integration loop, up to 3 attempts total.
- **Terminal** (malformed task, missing tool, unreadable workspace, unsafe ambiguity, unresolved conflict): stop immediately and follow On Failure.

When safety is uncertain, fail closed. Do not guess, do not force-push, and do not merge broken code into TARGET_BRANCH_PLACEHOLDER.
```

## 8.2 Rewrite Step 5 with validation and bounded verification

```markdown
### Step 5: Work on the Task Safely

1. Read `.tasks/in-progress/<filename>`.
2. Ignore leading `<!-- ... -->` runtime metadata lines.
3. Validate the task before editing:
   - If the file is empty, unreadable, missing a usable `# ` title, or contains only metadata/comments, treat it as a terminal task-file failure and follow On Failure.
   - If the instructions are ambiguous, use nearby code, tests, and repo docs to infer the most conservative interpretation.
   - If multiple materially different implementations are plausible and the repo does not clearly favor one, stop and follow On Failure rather than guessing.
4. Before major edits, run the most relevant existing verification command once to establish a baseline when practical.
5. Implement the task with focused changes.
6. Run verification using the repository's existing command if available (for example `make test`), otherwise the language-standard command.
7. If verification fails because of your changes, use a repair loop with a maximum of 3 attempts:
   - inspect the first failure cluster,
   - make one targeted fix,
   - rerun the same verification command,
   - stop if the same failure signature appears twice.
8. If the repo was already failing before your changes, record that baseline and prove your work does not make the relevant scope worse.
9. If you cannot reach a green or non-worsened verified state within 3 repair attempts, stop. Do not continue to commit-and-merge as if the task succeeded.
```

## 8.3 Replace Step 7 with an explicit integration loop

```markdown
### Step 7: Integrate into TARGET_BRANCH_PLACEHOLDER and Push

You are responsible for integrating your task branch into `TARGET_BRANCH_PLACEHOLDER` and pushing it to `origin`. Use an integration loop with a maximum of 3 attempts.

For each integration attempt:

1. Check out your task branch.
2. Fetch the latest target branch:
   ```bash
   git fetch origin
   ```
   If fetch fails, retry the same command up to 3 times with short backoff. If it still fails, follow On Failure.
3. Merge the latest target branch into your task branch:
   ```bash
   git merge origin/TARGET_BRANCH_PLACEHOLDER --no-edit
   ```
4. If this merge reports conflicts:
   - list conflicted files with `git diff --name-only --diff-filter=U`,
   - inspect base / ours / theirs for each file,
   - resolve conflicts file by file while preserving both sides' intent when safe,
   - never blindly choose one side for source code,
   - abort if more than 5 files conflict or if you cannot explain a safe resolution.
5. After resolving conflicts, verify that:
   - there are no unmerged paths,
   - no conflict markers remain,
   - the smallest relevant build/test command still passes.
6. Check out `TARGET_BRANCH_PLACEHOLDER` and merge your task branch:
   ```bash
   git checkout TARGET_BRANCH_PLACEHOLDER
   git merge <task-branch> --no-edit
   ```
7. Push the target branch:
   ```bash
   git push origin TARGET_BRANCH_PLACEHOLDER
   ```
8. If the push is rejected as non-fast-forward, another agent merged first. Do **not** force-push. Start the integration loop again from a fresh `git fetch origin`. Stop after 3 total integration attempts.
9. Only proceed to Step 8 after the push succeeds.
```

## 8.4 Stronger conflict-resolution wording for Step 7

```markdown
#### Conflict resolution rules

- Resolve conflicts using a 3-way comparison of base, task-branch, and target-branch versions.
- Do not resolve a file until you can explain what both sides changed.
- For generated files, prefer regeneration if the repo already has a standard command.
- For structured config files, preserve entries from both sides when possible and validate the result parses.
- For source code, preserve both behavioral changes when safe; do not blindly choose `ours` or `theirs`.
- After resolving all files, verify that no conflict markers remain anywhere in the tree.
- If you cannot determine a safe semantic resolution, abort and follow On Failure.
```

## 8.5 Replace the On Failure section entirely

```markdown
### On Failure

If any step reaches a terminal or exhausted-fallback state, stop and fail safely.

1. Classify the failure:
   - **Transient:** retryable infrastructure problem such as fetch/push timeout or race.
   - **Repairable:** build/test failure caused by your changes.
   - **Integration:** merge conflict or non-fast-forward during Step 7.
   - **Terminal:** malformed task, unreadable workspace, missing tool, unsafe ambiguity, unresolved conflict, or any state where continuing may corrupt work.
2. If the failure is transient or repairable and you still have budget remaining, use the allowed retry loop. Do not invent new loops.
3. If the failure is terminal, or if the allowed retry budget is exhausted:
   - return the repository to a clean, non-merged state if needed,
   - check out a safe branch (`TARGET_BRANCH_PLACEHOLDER` if possible),
   - append a structured failure record beginning with `<!-- failure:` to `.tasks/in-progress/<filename>`,
   - include at minimum: agent ID, timestamp, phase, category, retryable yes/no, and a short summary,
   - if you created useful partial progress, optionally keep a WIP commit on the task branch and record the branch name and commit SHA in the failure record,
   - move the task back to `backlog/` if retryable, or to `failed/` if the task is malformed, impossible, or clearly unsafe to retry.
4. Report briefly what failed, what you tried, and what the next human or retrying agent should know.

Never mark a task complete when:
- verification is still failing,
- conflict markers remain,
- the push to `origin` did not succeed,
- or you are uncertain that the result is correct.
```

## 8.6 Optional Step 0: environment validation

Because mato runs inside Docker with mounted host tools, a short environment check is worth adding before task claim:

```markdown
### Step 0: Validate the Environment

Before claiming a task, confirm that the workspace is writable and that required tools are available.

- Verify `git` and `copilot` are on `PATH`.
- Verify the current workspace and `.tasks/` directory are writable.
- If the environment is missing required tools or is read-only, stop immediately. This is a terminal environment failure, not a task failure.
```

This keeps the agent from claiming work it cannot possibly complete.

---

## Final recommendation

For mato, the best prompt strategy is:

- a **small explicit failure taxonomy**,
- **bounded loops** for retry, repair, and integration,
- **strong merge-conflict instructions based on 3-way understanding**, not vague conflict-marker editing,
- **fail-closed rules** that prevent force-pushes, broken merges, and endless loops,
- **structured failure comments** that preserve the existing `<!-- failure:` host contract while making debugging much easier.

If only three changes are made, they should be:

1. rewrite Step 7 as a bounded integration loop,
2. rewrite Step 5 to include baseline + repair budgets,
3. replace On Failure with structured classification and reporting.

Those three changes will do the most to improve real-world reliability when multiple autonomous agents are editing the same repo concurrently.
