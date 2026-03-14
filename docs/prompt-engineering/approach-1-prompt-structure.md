# Approach 1: Prompt structure for reliable `mato` agents

## Recommendation

Restructure `task-instructions.md` around a **short linear workflow spine** plus **small local decision tables**.

For `mato`, the most reliable pattern is:

1. a brief identity/mission section
2. a compact list of non-negotiable invariants
3. a state-machine style workflow overview
4. detailed instructions for each state
5. explicit branch handlers for waiting, dependency checks, retries, merge conflicts, and inter-agent messaging
6. a final completion checklist
7. a small appendix of concrete examples for tricky file formats and command templates

Do **not** keep growing the current prompt by appending more prose to the end. The new features—YAML frontmatter, dependency gating, waiting/ready queues, and file-based messaging—introduce enough branching behavior that `mato` now needs a more modular prompt shape.

---

## Current prompt analysis

The current prompt in `task-instructions.md` already has several strong patterns, but it is optimized for a mostly linear happy path.

### What works well today

1. **Clear mission and exit condition**  
   The opening makes the agent's job concrete: pick up a task, complete it, and exit. That is good framing for an autonomous agent.

2. **Strong step ordering**  
   The numbered workflow reduces ambiguity. For the current claim → branch → work → commit → merge → mark-complete flow, the prompt gives the model a very obvious path.

3. **Concrete shell commands**  
   The prompt does not only describe intent; it often shows exact commands. This is especially helpful for atomic file moves, branch creation, and retry tracking.

4. **Explicit success barriers**  
   Steps 6, 7, and 8 use hard gates such as “do not proceed until the commit exists” and “do not proceed unless the push succeeded.” That kind of checkpoint language is valuable.

5. **Failure handling exists**  
   The prompt already includes an On Failure path and retry-count tracking. That is much better than leaving failure behavior implicit.

6. **It reinforces the most dangerous omission**  
   The prompt repeatedly emphasizes that commit/merge/push/mark-complete are mandatory. This is exactly the kind of behavior that should be reinforced.

### What is fragile

1. **The prompt is a single long instruction chain**  
   It reads like one long procedure with exceptions embedded inside it. That works for a simple lifecycle, but it becomes fragile once the agent must decide between:
   - backlog vs ready vs waiting
   - dependency satisfied vs unsatisfied
   - work available vs no work available
   - message publish vs message read vs no-op
   - retry now vs wait vs fail permanently

2. **Happy-path linearity is overemphasized**  
   “You MUST execute these steps in exact order” is useful today, but new coordination features require controlled branching. If the agent has to check dependencies or wait for another task, a purely linear script becomes the wrong mental model.

3. **Some instructions will become mutually confusing once queues expand**  
   Today the prompt says not to look in directories other than `backlog/`, `in-progress/`, and `completed/`. That will conflict with a future `ready/` or `waiting/` model and a `messages/` area unless the prompt is restructured around queue semantics instead of a single directory scan rule.

4. **The prompt mixes different instruction types at the same level**  
   It interleaves:
   - identity
   - policy
   - workflow
   - exception handling
   - shell templates
   - examples
   - reminders

   LLMs are more reliable when these are separated cleanly.

5. **There are already soft contradictions**  
   The current prompt says:
   - stop immediately if no tasks are available
   - never stop until changes are committed and task is moved to completed

   A human resolves that easily because the first rule is clearly an exception. A model can still be pulled in both directions if the exception structure is not explicit.

6. **Placeholder-heavy command snippets are easy to misapply**  
   Prompts that use `<filename>`, `<task-branch>`, and `TARGET_BRANCH_PLACEHOLDER` are useful, but models sometimes copy placeholders too literally or forget which values must be substituted.

7. **The model must remember important invariants for too long**  
   Important constraints appear early, but the agent may need them much later. The longer the distance between an invariant and the action that depends on it, the more likely it is to drift.

8. **Raw task-file parsing burden will increase**  
   Today the agent only has to ignore leading HTML comments. With YAML frontmatter, dependency metadata, and message references, asking the agent to parse raw file structure directly becomes more error-prone.

### Where LLMs commonly deviate in prompts like this

For code-executing agents, the most common failure modes are:

1. **Skipping verification after a seemingly successful command**  
   Example: creating a branch or running `git push`, then moving on without checking the actual state.

2. **Following the happy path and under-handling exceptions**  
   Example: merge conflict logic exists, but the agent only handles the first conflicted file or forgets to commit after conflict resolution.

3. **Treating examples as copy-paste templates**  
   Example: using `<task-branch>` literally or not replacing placeholders consistently.

4. **Resolving ambiguity with guesswork instead of explicit checks**  
   Example: assuming a dependency is satisfied from task title similarity instead of checking metadata or queue state.

5. **Obeying the most local instruction and forgetting earlier global rules**  
   Example: focusing on a late merge step and forgetting that task status files or runtime metadata must also be updated.

6. **Leaving state half-updated**  
   Example: code committed, but task file not moved; or task moved, but failure metadata not appended; or message published, but presence not updated.

For `mato`, the most dangerous failure is not a bad code edit; it is an **inconsistent orchestration state**.

---

## Numbered steps vs. decision trees

### When linear numbered steps work best

Use strict numbered steps for the **core lifecycle that should almost always happen in the same order**.

For `mato`, this spine should stay linear:

1. discover candidate work
2. claim work atomically
3. parse task metadata
4. decide whether the task is ready to execute
5. create/switch branch
6. perform work
7. verify with tests
8. commit
9. sync and merge
10. mark the task complete or otherwise transition it to the correct queue

This kind of sequence matches how models tend to follow instructions well: small ordered units with visible progress.

### When branching logic is necessary

Use branching rules when the agent must classify the situation before acting.

For `mato`, the new features create several branch points:

- **task availability**: no task vs ready task vs task blocked by dependency
- **dependency resolution**: all dependencies satisfied vs some missing
- **coordination**: no relevant messages vs relevant peer messages vs conflict warning
- **integration**: clean merge vs merge conflict vs non-fast-forward push rejection
- **retry policy**: retryable failure vs permanent failure
- **queue routing**: ready vs waiting vs failed vs completed

### How LLMs handle conditional logic

LLMs are usually worse at following one giant prose decision tree than they are at following:

1. a short linear phase
2. a local “if X, do Y; otherwise do Z” branch
3. a return to the main workflow

That means `mato` should prefer **linear spine + local branch tables**, not one monolithic branching script.

### Recommended pattern for `mato`

Use a structure like this:

```text
State 1: Select work
- Check the ready queue.
- If a task is available, claim it and continue to State 2.
- If no ready task is available, check the waiting queue and recent messages.
- If no actionable work exists, report idle and stop.

State 2: Evaluate readiness
- Read normalized task metadata.
- If any dependency is incomplete, move the task to waiting, publish a wait message, and stop.
- Otherwise continue to State 3.
```

This is much easier for a model to execute than a deeply nested paragraph.

### Practical rule

- Use **numbered steps** for the default lifecycle.
- Use **first-match decision tables** for branch points.
- Keep each branch local to the step where it matters.
- After a branch completes, tell the model exactly which state it returns to—or that it must stop.

---

## Imperative vs. declarative style

### Declarative instructions

Declarative wording states the desired outcome:

- “Ensure you are on the main branch.”
- “Make sure dependencies are satisfied before working.”
- “Preserve both sides of the merge conflict.”

This is good for **invariants** and **safety properties**, but it leaves too much room for the model to choose how to satisfy them.

### Imperative instructions

Imperative wording states the action to take:

- “Run `git checkout "$TARGET_BRANCH"`.”
- “Read the dependency list from the task metadata.”
- “Append a failure record, then move the task file back to the backlog.”

This is usually more reliable for code-executing agents because it reduces interpretation.

### Best pattern: imperative action + declarative verification

For `mato`, the most reliable style is not purely imperative or purely declarative. It is:

1. **state the required outcome**
2. **give the action to take**
3. **require a verification check**

Example:

```text
Goal: be on the target branch before merging.
Action: run `git checkout "$TARGET_BRANCH"`.
Verify: run `git branch --show-current` and confirm it equals `$TARGET_BRANCH`.
```

That pattern is stronger than either of these alone:

- “Ensure you are on the main branch.”
- “Run `git checkout main`.”

The first is too vague. The second may succeed syntactically but still leave the model without a verification habit.

### Recommendation for `mato`

- Use **declarative wording** for mission, invariants, and queue semantics.
- Use **imperative wording** for shell operations and file mutations.
- Pair every important imperative with a **verification clause**.

Examples:

- Good: “Claim the task by atomically moving it into `in-progress/`. If the move fails because the file no longer exists, another agent claimed it; try the next candidate.”
- Better: “Run `mv ...`. If it succeeds, you own the task. If it fails because the source path is missing, try the next file. Do not edit the task before the move succeeds.”
- Best: “Run `mv ...`; if it succeeds, confirm the file now exists in `in-progress/` and no longer exists in `backlog/`.”

---

## Section organization

For a prompt this operational, ordering matters.

### Recommended top-level order

1. **Identity and mission**  
   One short paragraph: who the agent is, what success looks like, and what “done” means.

2. **Inputs, variables, and filesystem contract**  
   Define task directories, queue meanings, placeholders, branch names, message directories, and whether metadata is passed in normalized form.

3. **Non-negotiable invariants**  
   Short, high-importance rules that must hold across all paths.

4. **Workflow overview**  
   A compact state list or numbered summary of the full lifecycle.

5. **Detailed instructions by state**  
   One subsection per state with small numbered actions.

6. **Decision tables / branch handlers**  
   Local rules for dependency blocking, waiting, retries, merge conflicts, non-fast-forward pushes, and inter-agent messages.

7. **Communication protocol**  
   When to publish presence, when to read messages, what messages mean, and when queue state should change.

8. **Completion and failure checklists**  
   Checklist format is useful for important terminal states.

9. **Examples / templates appendix**  
   Keep this small and focused on syntax that is easy to get wrong.

### Why this order is good

This order puts the most durable reasoning aids first:

- mission
- definitions
- invariants
- overall workflow

Then it gives execution details.

That is better than leading with many raw shell snippets because the model first learns the control structure and only then the mechanics.

### Recommended organization for new `mato` features

The future prompt should likely have these sections:

```text
1. Mission
2. Runtime variables and directory meanings
3. Queue semantics
4. Non-negotiable rules
5. Workflow state machine
   5.1 Select a ready task
   5.2 Claim it
   5.3 Read metadata and decide readiness
   5.4 Publish presence / read messages
   5.5 Create branch
   5.6 Work / test / commit
   5.7 Merge / push / retry
   5.8 Move task to completed, waiting, backlog, or failed
6. Failure handling
7. Message formats and examples
8. Completion checklist
```

### Important structural advice

Do not bury queue semantics deep inside the workflow. For the new ready/waiting model, the prompt should define queue meaning early and explicitly, for example:

- `backlog/`: not yet evaluated for readiness or not currently selected
- `ready/`: eligible to claim immediately
- `waiting/`: blocked on dependencies or an external condition
- `in-progress/`: actively owned by one agent
- `completed/`: successfully integrated
- `failed/`: exceeded retry policy or requires manual intervention

That makes later branching easier.

---

## Verbosity vs. precision

### When more detail helps

More detail helps when:

1. the action is high-risk
2. the agent must mutate shared state
3. the correct action is surprising
4. there is a specific failure mode you have already observed
5. the syntax is easy to get wrong

For `mato`, more detail is justified for:

- atomic claims
- queue transitions
- retry accounting
- merge conflict policy
- message file format
- waiting behavior and dependency checks

### When more detail hurts

More detail hurts when it:

1. repeats the same rule in multiple slightly different ways
2. mixes examples with instructions without labeling them
3. turns every obvious shell action into a long paragraph
4. introduces exceptions long before or long after they matter
5. leaves stale text in place after the workflow changes

A long prompt is not automatically bad. A long prompt with **duplicated, overlapping, or weakly separated rules** is bad.

### Recommended balance for `mato`

Keep the prompt:

- **short on generic advice**
- **detailed on orchestration mechanics**
- **brief on coding best practices that the agent already knows**
- **explicit on terminal states and state transitions**

That means some current prose can shrink.

For example, generic statements like “Follow existing code conventions and style” are useful, but they do not need much expansion. By contrast, a new waiting-system rule absolutely should be explicit because it affects coordination correctness.

### Good compression strategy

Use this rule of thumb:

- **One sentence** for a principle.
- **One numbered list** for a procedure.
- **One decision table** for conditional behavior.
- **One small example** for tricky syntax.

That keeps precision high without making the prompt feel like a wall of prose.

---

## Anchoring and repetition

### What repetition is good for

Repetition is useful for a small number of critical behaviors that are costly to miss.

For `mato`, the prompt should reinforce these rules more than once:

1. **Never leave orchestration state half-updated.**
2. **Do not mark a task complete unless commit, merge, and push all succeeded.**
3. **Only claim work through the atomic queue move.**
4. **Do not work a task whose dependencies are not satisfied.**
5. **Use the correct queue transition on exit: completed, waiting, backlog, or failed.**
6. **Read and write coordination files at the defined checkpoints.**

These are orchestration invariants, not style preferences.

### What repetition hurts

Do not repeat:

- low-risk coding advice
- directory listings in multiple places
- long copies of shell commands
- whole paragraphs of warning text

Repeated prose tends to drift, and once two versions diverge, the model has to choose which one is authoritative.

### Best way to reinforce key instructions

Use **anchored repetition**, not paragraph duplication.

That means:

1. put the rule once in a **Non-negotiable rules** section
2. restate it in a **local checklist** where the action occurs
3. avoid repeating full explanations

Example:

- Top-level invariant: “Never mark a task complete until commit, merge, and push are verified.”
- Local completion checklist: “Before moving the task file to `completed/`, confirm: last commit exists, target branch contains the merge, push to origin succeeded.”

This is better than repeating “Do NOT skip this step” in large blocks throughout the prompt.

### Specific `mato` instructions that deserve reinforcement

I would explicitly reinforce these in two places each:

- claim atomically before editing the task
- re-check dependency readiness before starting work
- publish wait/failure metadata before moving queues
- do not infer dependency completion from memory
- do not use placeholders literally
- do not stop with the task in `in-progress/` unless you still own it and are actively continuing

---

## Examples in prompts

### When examples help most

Examples are especially useful when the model must produce or parse a structure.

For `mato`, examples are worth including for:

1. YAML frontmatter
2. runtime HTML comment metadata
3. message-file JSON
4. presence-file JSON
5. queue transition outcomes
6. retry-record format

These are all places where syntax matters.

### When abstract instructions are better

For general development behavior, abstract instructions are often enough:

- make the requested code change
- run relevant tests
- keep changes focused
- follow repository conventions

Examples are less necessary there because they can bloat the prompt without adding much control.

### Risk of too many examples

Too many examples create two problems:

1. **copying behavior**  
   The agent may mimic literal values instead of generalizing.

2. **attention dilution**  
   The actual normative instruction becomes less salient.

### Best practice for `mato`

Use examples only for:

- file formats
- branch naming templates
- message publishing
- waiting/dependency transitions
- merge retry flow

Keep them clearly labeled.

For example:

```text
Example runtime metadata block (example only; substitute real values):
<!-- claimed-by: agent-123 claimed-at: 2026-03-14T12:01:02Z -->
```

That “example only; substitute real values” phrase matters. It reduces placeholder-copy mistakes.

### Recommended example set for the new prompt

A good prompt appendix would include only 4–6 examples:

1. task file with YAML frontmatter
2. dependency-blocked task routed to `waiting/`
3. ready task claim flow
4. message publish example
5. merge conflict resolution checklist
6. failure record append + queue move

That is enough to anchor behavior without taking over the prompt.

---

## Prompt length considerations

### Is ~200 lines too long?

Not necessarily. For an autonomous agent prompt, ~200 lines is still reasonable if the structure is clean and each section has a distinct role.

The bigger issue is not raw length. It is **instruction density and conflict density**.

### Tradeoffs of expanding significantly

Expanding the prompt for new features has benefits:

- more explicit coordination rules
- fewer unstated assumptions
- better coverage of edge cases
- less guesswork around waiting and dependencies

But it also has real costs:

1. **more chances for contradictory wording**
2. **more stale command snippets over time**
3. **more attention spent on reading policy instead of acting**
4. **higher risk that important rules are buried in the middle**
5. **more maintenance burden every time queue semantics change**

### The right answer is refactoring, not just expansion

`mato` should probably allow the prompt to grow somewhat, but only after refactoring it into layers.

I would rather have:

- a **260-line well-structured prompt**

than:

- a **200-line prompt with tangled control flow**
- or a **400-line prompt formed by appending more exceptions at the end**

### Recommended size target

A good target would be:

- **core always-on instructions**: 100–150 lines
- **feature-specific protocol sections**: 60–120 lines
- **appendix examples**: 30–60 lines

That keeps the whole thing in a manageable range while giving new features the detail they need.

### Strong recommendation for `mato`

Do not keep a single undifferentiated monolith forever. Even if the final runtime prompt is still emitted as one file, treat it conceptually as:

1. core identity + invariants
2. workflow
3. coordination extensions
4. appendices

If possible, build it from internal templates so future changes do not require editing one giant block of prose.

---

## Specific recommendations for restructuring `task-instructions.md`

### 1. Replace the current “exact order” framing with a workflow spine plus explicit branches

Keep an ordered lifecycle, but make it stateful rather than purely linear.

Suggested top-level states:

1. `SELECT_TASK`
2. `CLAIM_TASK`
3. `READ_METADATA`
4. `CHECK_READINESS`
5. `SYNC_COORDINATION`
6. `CREATE_BRANCH`
7. `WORK_AND_TEST`
8. `COMMIT`
9. `MERGE_AND_PUSH`
10. `FINALIZE_TASK`
11. `FAIL_TASK`
12. `IDLE_EXIT`

This will handle new queues much better than “Step 1 through Step 8, in exact order, no matter what.”

### 2. Define queue semantics before the workflow starts

Add a dedicated section that says what each queue means and when a task belongs there.

Without this, the agent has to infer semantics from scattered movement rules.

### 3. Move critical invariants into a short dedicated section near the top

For example:

```text
Non-negotiable rules:
1. Only claim a task by moving it atomically into `in-progress/`.
2. Do not start implementation until dependencies are confirmed satisfied.
3. Do not mark a task complete until commit, merge, and push are verified.
4. Before stopping, place the task in exactly one terminal queue: completed, waiting, backlog, or failed.
5. At required checkpoints, read and write coordination files.
```

That gives the model a compact memory anchor.

### 4. Prefer host-provided normalized metadata over asking the agent to parse raw frontmatter

For reliability, `mato` should ideally present the agent with something like:

```text
Task metadata:
- id: add-retry-logic
- priority: high
- depends_on: [setup-http-fixtures]
- ready_status: blocked

Task instructions:
# Add retry logic
...
```

That is more reliable than expecting the agent to interpret arbitrary frontmatter correctly every time.

If the raw file must still be read directly, then add a very short parsing rule:

- ignore an optional YAML frontmatter block at the top
- ignore runtime HTML comments after frontmatter
- use the first Markdown heading as the title

Do not make frontmatter parsing a long prose section.

### 5. Separate decision rules from procedures

For each place where the agent must decide what kind of situation it is in, add a small decision table.

Example:

| Condition | Action | Next state |
| --- | --- | --- |
| ready task exists | claim it | `CLAIM_TASK` |
| no ready task, actionable wait resolved | move task to ready or claim it | `CLAIM_TASK` |
| only blocked tasks remain | publish idle/wait status and stop | `IDLE_EXIT` |
| no tasks at all | report idle and stop | `IDLE_EXIT` |

This is easier for an LLM to follow than nested prose.

### 6. Use imperative command blocks only where syntax really matters

Keep concrete commands for:

- atomic `mv`
- metadata append templates
- branch creation
- push / merge retry flow
- message file write templates

For higher-level logic, prefer short action descriptions plus verification.

### 7. Add explicit coordination checkpoints

The prompt should not merely describe the messaging system. It should specify exactly when to use it.

For example:

- after claiming a task: publish presence and work-claim message
- before starting work: read new messages relevant to this task or overlapping files
- before merge: re-read recent completion/conflict messages
- when blocked by dependencies: publish wait message and move task to waiting
- on completion: publish completion message before exiting

LLMs follow timing rules better when the checkpoints are tied to workflow states.

### 8. Introduce terminal-state checklists

At the end of successful and unsuccessful runs, use short checklists.

Example completion checklist:

- task branch contains the implementation commit
- target branch contains the merged result
- push to origin succeeded
- task file moved to `completed/`
- completion metadata/message written

Example failure checklist:

- failure reason recorded
- retry count updated
- task moved to correct queue
- presence/message updated
- agent reported the stop reason

Checklists are an excellent compliance tool for terminal actions.

### 9. Reduce “warning paragraph” style repetition

Replace repeated warning prose with:

- one invariant list
- one local checklist at the point of execution

This will make the prompt easier to maintain and less self-contradictory.

### 10. Keep examples in an appendix, not inline with the main control flow

Inline examples interrupt the workflow and are easier for models to copy literally.

A better structure is:

- main instructions first
- appendix at the end with clearly marked examples

---

## Suggested outline for the rewritten prompt

```markdown
# Task Agent Instructions

## Mission
Short description of the agent's job and what counts as done.

## Runtime variables and paths
- target branch
- tasks root
- queue directories
- messages directory
- agent id

## Queue semantics
Define backlog / ready / waiting / in-progress / completed / failed.

## Non-negotiable rules
5-8 short invariant rules.

## Workflow overview
1. Select task
2. Claim task
3. Read metadata
4. Check readiness
5. Sync coordination
6. Create branch
7. Work and test
8. Commit
9. Merge and push
10. Finalize

## State instructions
### SELECT_TASK
### CLAIM_TASK
### READ_METADATA
### CHECK_READINESS
### SYNC_COORDINATION
### CREATE_BRANCH
### WORK_AND_TEST
### COMMIT
### MERGE_AND_PUSH
### FINALIZE_TASK
### FAIL_TASK
### IDLE_EXIT

## Decision tables
- dependency blocked
- message indicates overlap/conflict
- non-fast-forward push
- merge conflict
- retry exhausted

## Completion checklist
## Failure checklist

## Examples appendix
- YAML frontmatter example
- runtime metadata example
- message JSON example
- waiting transition example
```

---

## Final assessment

The current `task-instructions.md` is a good first-generation autonomous-agent prompt because it is concrete, procedural, and explicit about the current happy path.

But the next `mato` features push it beyond a simple checklist. Once tasks can be blocked by dependencies, routed between ready and waiting queues, and coordinated through file-based messages, the prompt needs a more formal control structure.

The key design change is this:

- keep the **main lifecycle linear**
- make **branch points explicit and local**
- separate **invariants, procedures, and examples**
- reinforce a few orchestration-critical rules
- make **state transitions** the center of the prompt

That structure should materially improve compliance and reduce the kinds of half-finished or state-inconsistent behaviors that matter most in `mato`.

---

## Research basis

These recommendations align with common guidance from major model providers and prompt-engineering references:

- Anthropic: clear/direct instructions, sequential steps, XML/tagged structure, and carefully chosen examples improve compliance.
- OpenAI Codex guidance: start from a strong autonomy/tool-use prompt, avoid unnecessary verbosity, and optimize prompt structure for action rather than narration.
- Google Vertex AI guidance: structure complex prompts with explicit sections/delimiters and break complex tasks into smaller, controllable sub-tasks.

Useful references:

- https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices
- https://developers.openai.com/cookbook/examples/gpt-5/codex_prompting_guide
- https://docs.cloud.google.com/vertex-ai/generative-ai/docs/learn/prompts/structure-prompts
- https://docs.cloud.google.com/vertex-ai/generative-ai/docs/learn/prompts/break-down-prompts
