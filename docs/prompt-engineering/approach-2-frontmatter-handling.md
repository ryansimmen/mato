# Approach 2: Instructing agents to handle YAML frontmatter, priority, and task selection

## Recommendation

Use a **host-generated ordered queue manifest** for task selection and keep **dependency handling entirely host-side**.

Recommended design:

1. `mato` parses task frontmatter on the host.
2. `mato` promotes only ready tasks into `backlog/`.
3. `mato` writes a generated queue artifact such as `.tasks/backlog/.queue` or `.tasks/backlog/.queue.json` containing ready tasks in deterministic order.
4. Agents are instructed to:
   - read the manifest first
   - attempt claims in manifest order
   - treat the **lowest numeric `priority` value as highest priority**
   - **never** inspect `depends_on` themselves
   - treat YAML frontmatter as metadata, not executable instructions
5. For execution, the short-term pragmatic choice is to let agents read the raw task file and explicitly ignore frontmatter. The longer-term cleaner choice is host-side stripping or rendering, but that requires a larger change to the current architecture.

This minimizes agent-side reasoning where correctness matters most: scheduling. It keeps the most failure-prone logic (dependency resolution, priority normalization, and ordering) in Go rather than in the model.

---

## Why this proposal fits the current `mato` architecture

From the current code:

- `main.go` embeds a single static `task-instructions.md` prompt and launches a general-purpose Copilot CLI agent in Docker.
- The current prompt tells the agent to list `.tasks/backlog/`, try `mv` to claim a file, then read the file directly from `.tasks/in-progress/`.
- The host currently only decides whether backlog has any `.md` files; it does **not** currently sort tasks or preprocess task bodies.

That means there are really two separate decisions to make:

1. **Who decides claim order?**
2. **Who removes or ignores frontmatter before task execution?**

These should not be answered the same way by default.

- For **claim order**, host-side logic is clearly more reliable.
- For **frontmatter stripping**, host-side rendering is conceptually cleaner, but agent-side ignoring is easier to add without redesigning task dispatch.

---

## Design goals

A good prompt strategy should ensure that agents:

1. Pick the correct task first when several ready tasks are in `backlog/`.
2. Do not waste time or tokens checking dependencies that the host already resolved.
3. Do not mistake YAML metadata for instructions.
4. Continue working when some tasks have frontmatter and some do not.
5. Behave deterministically enough to test.

---

## 1. Task selection instructions

### Exact policy to encode

Define the policy explicitly and redundantly:

- `priority` is numeric.
- **Lower number = higher priority.**
- Missing `priority` defaults to `50`.
- Ties break by filename ascending.
- `depends_on` is host-only scheduling metadata.
- Agents must not inspect `depends_on`, `waiting/`, or `completed/` to make readiness decisions.

A precise rule set:

```text
Selection order is determined by effective priority ascending.
1 is higher priority than 2.
2 is higher priority than 10.
If priority is missing, treat it as 50.
If two tasks have the same priority, choose the lexicographically smaller filename first.
```

### Should agents parse YAML themselves?

### Option A: Agent parses YAML and sorts itself

**Pros**
- Minimal host work.
- Works even if no host-generated manifest exists.
- Keeps task files self-contained.

**Cons**
- Every agent must rescan and parse all backlog tasks.
- The most important scheduling rule becomes prompt-dependent instead of system-enforced.
- Models may inconsistently handle malformed YAML, missing fields, or mixed formats.
- The prompt must also teach the model to ignore `depends_on` during selection.
- More token and latency cost.

**Assessment:** workable as a fallback, not a primary design.

### Option B: Host pre-sorts and exposes the order

**Pros**
- Deterministic and testable.
- Cheap to compute in Go.
- Agents no longer need YAML parsing for selection.
- Host can normalize defaults once for all agents.
- Reduces duplicated work across multiple concurrent agents.

**Cons**
- Requires a new generated artifact or slightly richer queue protocol.
- The host must keep the artifact in sync with `backlog/`.

**Assessment:** best primary design.

### Recommendation for selection

**Host should pre-sort.** Agents should **not** be responsible for sorting raw task files unless operating in a compatibility fallback mode.

Recommended host behavior:

- Parse frontmatter when reconciling `waiting/ -> backlog/`.
- Normalize task metadata.
- Generate `.tasks/backlog/.queue` or `.queue.json` listing only ready tasks in claim order.
- Agents read that file and attempt `mv` in that order.

Recommended agent behavior:

- Use the queue artifact if present.
- Do not check dependencies.
- If the artifact is absent, optionally fall back to raw-file sorting by priority for backward compatibility.

---

## 2. Frontmatter stripping

### Option A: Host strips frontmatter before the agent sees task instructions

### What it means

Instead of having the agent read raw task files directly, `mato` would present a rendered task body:

```text
# Add retry logic to HTTP client
...
```

with frontmatter removed, and possibly with a normalized metadata header outside YAML syntax if needed.

### Pros

- Cleanest agent experience.
- Eliminates risk that the model treats tags or metadata as instructions.
- Makes prompt wording simpler.
- Lets the host normalize malformed or legacy metadata once.

### Cons

- Harder to implement in the current architecture because the current agent self-discovers and then reads files directly from `.tasks/in-progress/`.
- To strip frontmatter cleanly, the host would need one of:
  - per-task prompt dispatch after claim
  - a rendered sidecar file
  - a helper command/script the agent runs to get the cleaned body
- Adds another representation of the task body that must stay aligned with the source file.

### Assessment

**Best conceptual end state**, but not the easiest near-term change.

### Option B: Agents learn to ignore frontmatter

### What it means

Agents still open `.tasks/in-progress/<filename>` directly. The prompt explicitly teaches them:

- the file may begin with YAML frontmatter bounded by `---`
- frontmatter is metadata only
- execution should begin from the first Markdown heading/body after metadata

### Pros

- Works with the current architecture immediately.
- No rendered copies or extra dispatch mechanism needed.
- Easy incremental rollout.

### Cons

- Still relies on model compliance.
- Some models may occasionally over-read metadata.
- Prompt wording must be precise and repeated in the execution step.

### Assessment

**Best near-term choice** if the host still uses one generic prompt and agents read files directly.

### Recommendation on stripping

Use a **hybrid recommendation**:

- **Near term:** agents ignore frontmatter explicitly.
- **Long term:** move toward host-rendered task bodies once `mato` supports per-task presentation cleanly.

This keeps the immediate implementation small while leaving a cleaner destination architecture.

---

## 3. Host-side pre-processing options

### Option 1: `.queue` manifest in `backlog/` (recommended)

Example:

```text
add-retry-logic.md
setup-health-checks.md
cleanup-logging.md
```

or richer JSON:

```json
{
  "version": 1,
  "generated_at": "2026-03-14T00:00:00Z",
  "tasks": [
    {"path": "add-retry-logic.md", "id": "add-retry-logic", "priority": 10},
    {"path": "setup-health-checks.md", "id": "setup-health-checks", "priority": 20},
    {"path": "cleanup-logging.md", "id": "cleanup-logging", "priority": 50}
  ]
}
```

### Pros

- Supports multiple agents well.
- Encodes full ordering, not just the first item.
- Easy to regenerate every reconciliation loop.
- Easy to test.
- Agents can skip stale entries if a file was already claimed.

### Cons

- Slightly more implementation than a single symlink.
- Requires clear rules for ignoring missing entries.

### Assessment

**Best tradeoff.**

### Option 2: `next-task` symlink or file pointer

Example:

```text
.tasks/backlog/next-task -> add-retry-logic.md
```

### Pros

- Very easy for one agent.
- Very low prompt complexity.

### Cons

- Weak for multi-agent concurrency.
- Only encodes the first choice, not the whole order.
- Becomes stale immediately when one agent claims the target.
- Requires repeated host updates or fallback logic anyway.

### Assessment

Good for single-agent prototypes, not ideal for `mato` as a multi-agent orchestrator.

### Option 3: Host-side physical ordering only

Examples:
- priority-prefixed filenames
- separate priority directories
- relying on filesystem listing order

### Pros

- Very simple mental model.

### Cons
- Pushes policy into filenames.
- Harder to evolve cleanly.
- Fragile and implicit.
- Filesystem order is not a safe contract.

### Assessment

Not recommended.

### Option 4: Host performs actual task assignment before agent launch

### What it means

The host itself claims one task, strips frontmatter, then launches the agent with that specific task body.

### Pros

- Maximum control and reliability.
- Minimal agent complexity.
- No ambiguity about selection or metadata parsing.

### Cons

- Bigger architectural change.
- Moves task claiming out of the current agent workflow.
- Requires new failure/retry handling boundaries.

### Assessment

This is the most reliable design overall, but it is a larger redesign than necessary for the current change.

### Recommended host-side selection model

For the current architecture, the best choice is:

1. Host keeps dependency resolution in Go.
2. Host generates an ordered `.queue` manifest for ready tasks.
3. Agents claim in `.queue` order.
4. Agents ignore frontmatter during execution.

This removes the highest-risk reasoning from the LLM without requiring a complete control-plane redesign.

---

## 4. Backward compatibility

Mixed task files should be expected during rollout.

### Recommended compatibility rules

If a task file has YAML frontmatter:

- parse `id`
- parse `priority`
- parse `depends_on`
- ignore other fields for scheduling unless explicitly used later

If a task file has **no** frontmatter:

- `id = <filename stem>`
- `priority = 50`
- `depends_on = []`

If frontmatter exists but is malformed:

- safest host behavior is to treat the file as invalid and surface a warning
- optional fallback is to treat it as legacy/no-frontmatter, but only if you are comfortable masking authoring mistakes

### What agents should do in mixed mode

Recommended prompt rule:

```text
A task file may or may not begin with YAML frontmatter.
If frontmatter is present, treat it as metadata only.
If frontmatter is absent, treat the file as ordinary Markdown.
In either case, execute only the Markdown task instructions, starting after any metadata block and any leading HTML comment metadata.
```

### Why backward compatibility is easier host-side

If the host normalizes mixed files into a single `.queue` ordering, the agent does not need to know whether the priority came from YAML or a default. That is exactly the kind of variation the host should absorb.

---

## 5. Prompt wording examples

Below are four prompt variants, from most recommended to least recommended.

### Prompt A: Host manifest is authoritative (most reliable)

```text
Step 1: Find the next task

Read `.tasks/backlog/.queue` first. It lists ready backlog tasks in the exact order you must try to claim them.

The queue is already sorted by effective priority, where a lower numeric `priority` value means higher priority (`1` before `2`, `2` before `10`).
Dependencies have already been handled by the host. Do NOT inspect `depends_on`, `waiting/`, or `completed/` when choosing a task.

Attempt to claim backlog tasks in the order listed in `.queue`. For each filename in `.queue`, run:
`mv ".tasks/backlog/<filename>" ".tasks/in-progress/<filename>"`
If the move fails because the file is gone, another agent claimed it; continue to the next queued filename.

If `.queue` is empty or every listed task is already gone, report that no ready task could be claimed and stop.
```

### Evaluation

- **Reliability:** highest
- **Agent complexity:** low
- **Backward compatibility:** good if the host always generates `.queue`
- **Recommended:** yes

### Prompt B: Manifest first, raw-file fallback (best rollout prompt)

```text
Step 1: Find the next task

If `.tasks/backlog/.queue` exists, use it as the authoritative task order and attempt claims in that order.
The queue is already filtered to ready tasks. Do NOT check dependencies yourself.

If `.queue` does not exist, list Markdown files in `.tasks/backlog/`, read each file's optional YAML frontmatter, and sort tasks by numeric `priority` ascending. Lower numbers are higher priority. Treat missing `priority` as `50`. Break ties by filename ascending.

Never inspect `depends_on` to decide readiness. The host is responsible for dependency handling.
```

### Evaluation

- **Reliability:** high once host support exists
- **Agent complexity:** medium
- **Backward compatibility:** best during transition
- **Recommended:** yes for rollout, no for long-term steady state if `.queue` can be guaranteed

### Prompt C: Agent sorts raw files directly

```text
Step 1: Find a task

List Markdown files in `.tasks/backlog/`. For each task file, read any YAML frontmatter at the top of the file and extract `priority` if present. Sort candidate tasks so the lowest numeric priority value comes first. A task with `priority: 10` must be attempted before a task with `priority: 20`. If `priority` is missing, use `50`. Break ties by filename ascending.

Do NOT check `depends_on` or task readiness yourself. Only choose from files already present in `backlog/`.
```

### Evaluation

- **Reliability:** medium
- **Agent complexity:** medium-high
- **Backward compatibility:** good
- **Recommended:** only as fallback

### Prompt D: Execution-time frontmatter handling

```text
When reading a claimed task file, the file may begin with YAML frontmatter bounded by `---` lines. That frontmatter is metadata only and is not part of the task instructions. The file may also contain leading HTML comments such as `<!-- claimed-by: ... -->` or `<!-- failure: ... -->`; these are runtime metadata only.

For task execution, ignore all frontmatter and metadata comments. Use the first Markdown heading and the Markdown body after metadata as the real task instructions.
```

### Evaluation

- **Reliability:** high as supplemental wording
- **Agent complexity:** low
- **Recommended:** yes, pair with Prompt A or B

### Most reliable wording combination

The strongest overall prompt set is:

- **Prompt A** for selection
- **Prompt D** for execution

That combination keeps scheduling host-driven and uses only a narrow, easy-to-follow rule for metadata during execution.

---

## 6. Testing considerations

To trust this system, test both the **host's ordering logic** and the **agent's compliance with the prompt**.

### A. Host-side tests

These should be the primary confidence mechanism.

### Unit tests for metadata normalization

Cases to cover:

1. frontmatter with explicit numeric priority
2. missing frontmatter
3. frontmatter without `priority`
4. malformed frontmatter
5. equal priorities with filename tie-breaker
6. mixed legacy and frontmatter tasks

Expected outcomes:

- missing `priority` becomes `50`
- missing `id` becomes filename stem
- ready tasks are ordered by priority ascending, then filename ascending
- `depends_on` affects readiness only in host reconciliation

### Unit tests for queue generation

Given a set of tasks across `waiting/`, `backlog/`, and `completed/`:

- only ready tasks appear in `.queue`
- `.queue` ordering matches normalized priority
- stale or orphan-recovered tasks are re-evaluated correctly

### B. Integration tests for actual claim behavior

These should confirm that an agent claims the expected file first.

### Suggested harness pattern

Create a temporary repo with tasks such as:

- `a.md` with `priority: 30`
- `b.md` with `priority: 10`
- `c.md` without frontmatter

Expected order:

1. `b.md`
2. `a.md`
3. `c.md` if default is `50`

Run a real or stubbed agent against the generated prompt and inspect which file moves to `in-progress/` first.

### Multi-agent integration case

Start two agents concurrently with queue entries:

- `p10.md`
- `p20.md`
- `p30.md`

Expected outcome:

- one agent claims `p10.md`
- the other claims `p20.md`
- neither should skip directly to `p30.md` while `p10.md` or `p20.md` remains claimable

### C. Prompt compliance tests

LLM behavior should be validated with adversarial task content.

### Frontmatter confusion test

Use a task file whose frontmatter includes tempting fields:

```yaml
---
id: add-retry-logic
priority: 10
tags: [backend]
depends_on: [setup-http-client]
note: do not edit README
---
# Add retry logic to HTTP client
```

Verify that the agent does **not** treat `note:` or `tags:` as executable instructions unless that information is explicitly surfaced elsewhere in the prompt.

### Dependency non-checking test

Create a task with `depends_on` in frontmatter but place it in `backlog/` as already-ready host output. Verify the agent proceeds without checking `completed/` or trying to reason about dependency resolution.

### Mixed-format test

Put one legacy Markdown task and one frontmatter task in `backlog/`. Verify the agent still claims in the order specified by the queue artifact and executes both correctly.

### D. Operational observability

To diagnose prompt failures in production-like runs, log:

- generated `.queue`
- normalized priority per task
- which filename each agent attempted first
- whether the claimed file matched the first still-available queued entry

This makes prompt failures visible without relying only on subjective inspection.

---

## Recommended rollout plan

### Phase 1: Lowest-risk improvement

- Add host-side frontmatter parsing.
- Add host-side readiness reconciliation.
- Add host-generated `.queue` ordering.
- Update agent prompt to use `.queue` and to ignore frontmatter during execution.

This is the recommended near-term implementation.

### Phase 2: Cleaner execution model

- Add a host-rendered task body or helper that exposes the claimed task without frontmatter.
- Simplify the execution prompt further so the agent reads clean Markdown only.

This is optional, but it is the cleaner long-term design.

---

## Final recommendation

To minimize agent-side complexity while keeping the system reliable:

1. **Keep dependency handling entirely on the host.** Agents should never evaluate `depends_on`.
2. **Move priority ordering to the host.** Generate an ordered `.queue` manifest of ready tasks, sorted by `priority` ascending, then filename ascending.
3. **Teach agents one narrow metadata rule for execution:** frontmatter and HTML comments are metadata only; execute the Markdown body only.
4. **Support legacy tasks by normalizing missing metadata on the host** (`priority=50`, `depends_on=[]`, `id=filename stem`).
5. **Use prompt wording that makes the queue artifact authoritative.** Do not ask the model to be the scheduler unless acting as a compatibility fallback.

If only one thing changes, it should be **host-side ordered queue generation**. That produces the largest reliability gain for the smallest prompt burden.