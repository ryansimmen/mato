# Dependency Modeling for `mato`

## 1. Goal and current constraints

Today `mato` treats every Markdown file in `.tasks/backlog/` as immediately claimable work.

From the current implementation and embedded task prompt:

- `task-instructions.md` tells each agent to list `.md` files in `.tasks/backlog/` and try `mv backlog/<file> in-progress/<file>` until one succeeds.
- `main.go` only checks whether backlog has at least one top-level `.md` file via `hasAvailableTasks()`.
- task ownership is enforced by the atomic filesystem rename from `backlog/` to `in-progress/`.
- completion is represented by moving the task file into `.tasks/completed/`.
- failed or orphaned tasks are moved back to `backlog/` or eventually into `failed/`.

That means any dependency model has to work with these realities:

1. **The filesystem is the source of truth.**
2. **Task state is encoded by directory location.**
3. **Claiming should stay atomic.**
4. **Agents must be able to discover dependency info from files and their contents.**
5. **The system should still work when multiple `mato` processes run concurrently.**

The design question is not just how to *declare* dependencies, but also how `mato` determines which backlog tasks are actually ready.

---

## 2. Common scheduling semantics

Regardless of storage format, the dependency engine should use one consistent model.

### 2.1 Task identity

Because task files move between `backlog/`, `in-progress/`, `completed/`, and `failed/`, dependency references should point to a **logical task ID**, not a state-specific path.

Practical options:

- for flat queues: `setup-db.md`
- for nested queues: `01-phase-one/setup-db.md`
- optionally an explicit `id:` field, but filename/path should still remain the default key

For an MVP, using the task's **relative path within its logical queue layout** is enough.

### 2.2 Ready-state algorithm

For every backlog task:

1. parse its dependency metadata
2. resolve dependency references to known tasks
3. check every **hard dependency**
4. treat the task as **ready** only if all hard dependencies are in `completed/`
5. keep the task **blocked** if any hard dependency is in `backlog/`, `in-progress/`, or `failed/`
6. if a dependency is missing entirely, mark the task invalid and surface an operator error

Suggested state interpretation:

| Dependency state | Meaning for dependent task |
|---|---|
| `completed/` | satisfied |
| `backlog/` | not ready |
| `in-progress/` | not ready |
| `failed/` | blocked until dependency is retried or manually resolved |
| missing | configuration error |

### 2.3 Hard vs soft dependencies

Use two dependency classes:

- **hard dependency**: task must not be claimable until the dependency completes
- **soft dependency**: task may be claimable, but should be deprioritized until the dependency completes

Soft dependencies are useful for things like:

- docs that should follow implementation
- cleanup tasks that benefit from another task finishing first
- tests that can start early but should ideally wait for a refactor to land

Recommended scheduling rule:

1. filter to tasks whose **hard** dependencies are all satisfied
2. score ready tasks by priority
3. add a penalty if **soft** dependencies are still incomplete
4. pick the highest-scoring ready task

### 2.4 Priority with dependencies

Dependencies answer **"can this run?"**. Priority answers **"which ready task goes first?"**.

Recommended ordering:

1. highest explicit priority first
2. then fewest incomplete soft dependencies
3. then oldest filename or lexical order as deterministic tie-breaker

Example scoring model:

```text
score = priority - (10 * incomplete_soft_deps)
```

This keeps hard dependencies absolute while letting soft dependencies influence scheduling without blocking it.

### 2.5 Circular dependency handling

Every approach should validate the graph before agents start claiming work.

Recommended behavior:

1. scan all known task definitions
2. build a directed graph of hard dependencies
3. run cycle detection with DFS or Kahn's algorithm
4. if a cycle exists, mark all involved tasks as invalid and do not offer them for claiming
5. surface a clear error naming the cycle chain

Soft dependencies can be excluded from hard cycle checks, or reported separately as warnings.

### 2.6 Partial failure or rollback

A dependency can become invalid after previously being completed, for example if:

- an operator moves a completed task back to `backlog/`
- a task is superseded or reverted
- the completed artifact is removed manually

Recommended rule:

- **readiness is always computed from current filesystem state**, not historical memory

So if `task-a.md` moves out of `completed/`, then dependents should become unready on the next scheduling pass.

If a dependent task is already in `in-progress/`, the safest behavior is:

1. allow work to continue locally
2. re-check dependencies before merge or before moving to `completed/`
3. if a hard dependency is no longer satisfied, append a failure note and move the dependent back to `backlog/`

That gives `mato` a deterministic answer for rollback without inventing another state store.

---

## 3. Comparison summary

| Approach | Expressiveness | Fits current atomic claim flow | Global validation | Soft deps / priority | Main drawback |
|---|---|---:|---:|---:|---|
| Frontmatter in task files | High | Yes | Good | Excellent | Need metadata parser in Markdown files |
| Manifest file | High | Yes | Excellent | Excellent | Separate source of truth can drift |
| Directory phases | Low-Medium | Partially | Very simple | Weak | Too coarse; hurts parallelism |
| Filename ordering | Low | Yes | Simple | Weak | Only supports linear ordering |
| Sidecar `.deps` files | Medium-High | Awkward | Good | Good | Paired-file atomicity problem |

---

## 4. Approach 1: YAML/TOML frontmatter in task files

### 4.1 How dependencies are declared

Each task file gets a frontmatter block at the top, followed by the existing Markdown body.

Example with YAML frontmatter:

```markdown
---
id: implement-api
priority: 80
depends_on:
  - setup-db.md
  - bootstrap-env.md
soft_depends_on:
  - api-docs-outline.md
---
# Implement API

Build the HTTP handlers and wire them into the router.
```

Equivalent TOML frontmatter:

```markdown
+++
id = "implement-api"
priority = 80
depends_on = ["setup-db.md", "bootstrap-env.md"]
soft_depends_on = ["api-docs-outline.md"]
+++
# Implement API

Build the HTTP handlers and wire them into the router.
```

Recommended fields:

- `depends_on`: hard dependencies
- `soft_depends_on`: advisory ordering
- `priority`: integer, higher means earlier
- optional `id`: explicit stable identifier if filenames may change

### 4.2 How `mato` determines readiness

`mato` scans backlog task files, parses frontmatter, and marks a task ready when every item in `depends_on` is present in `.tasks/completed/`.

Pseudo-flow:

1. list backlog tasks
2. parse each file's frontmatter
3. resolve dependency names against tasks in all queue directories
4. discard blocked tasks
5. sort ready tasks by priority and soft-dependency penalty
6. let an agent try to claim only from that ready set

### 4.3 How agents discover dependency info

If the current agent-driven claim model stays in place, agents can read the frontmatter directly before attempting `mv`.

That means Step 1 in `task-instructions.md` would need to evolve from:

- "list all `.md` files"

into:

- list candidate `.md` files
- inspect frontmatter for each candidate
- skip tasks whose hard dependencies are not complete
- prefer higher priority tasks among the ready ones

A stronger implementation is to make **host-side Go code compute the ready set** and only launch the agent when at least one ready task exists. Even in that model, agents can still read frontmatter for context.

### 4.4 Circular dependency detection/prevention

Frontmatter makes cycle detection straightforward:

- parse all backlog, in-progress, completed, and failed task frontmatter
- build a graph from `depends_on`
- run DFS/Kahn cycle detection
- emit a clear error such as `cycle detected: task-a.md -> task-b.md -> task-a.md`

Prevention options:

- add `mato validate-tasks` as a preflight command
- validate on each polling loop and refuse to offer cyclical tasks
- optionally reject cycles in CI if `.tasks/` is committed in another future workflow

### 4.5 Impact on the current atomic `mv` flow

This approach preserves the current ownership primitive well:

- dependency checks happen **before** attempting the `mv`
- claim still happens with a single atomic rename of the `.md` file
- no secondary files need to move

That is a major strength of frontmatter: metadata travels with the task automatically as it moves across queue states.

### 4.6 Partial failures and rollback

If `setup-db.md` had completed but later moves back to backlog, `implement-api.md` becomes blocked again because its hard dependency is no longer in `completed/`.

If `implement-api.md` was already in progress, `mato` should revalidate hard dependencies before final completion. Frontmatter does not solve rollback by itself, but it makes revalidation simple because the dependency declaration remains attached to the task.

### 4.7 Example file contents

```markdown
---
priority: 100
depends_on: []
soft_depends_on: []
---
# Bootstrap environment

Create the basic project scaffolding.
```

```markdown
---
priority: 70
depends_on:
  - bootstrap-env.md
soft_depends_on:
  - write-readme.md
---
# Implement API

Add the first version of the API.
```

### 4.8 Complexity of implementation in Go

**Complexity: Medium**

Work involved:

- parse frontmatter from Markdown files
- strip metadata before presenting task body to the agent
- build dependency graph and ready-task selection
- add cycle validation
- update prompt instructions to explain metadata

Likely implementation notes:

- YAML/TOML parsing probably adds a module dependency unless parsing is deliberately minimal
- the rest is straightforward stdlib graph/state logic

Estimated effort: roughly **250-400 LOC** plus tests.

### 4.9 Assessment

This is the best balance of:

- locality of metadata
- compatibility with current file moves
- expressiveness for hard deps, soft deps, and priority
- human readability for agents and operators

---

## 5. Approach 2: Central manifest file

### 5.1 How dependencies are declared

A single file such as `.tasks/tasks.yaml` or `.tasks/tasks.json` defines the full graph.

YAML example:

```yaml
tasks:
  bootstrap-env.md:
    priority: 100
  implement-api.md:
    priority: 70
    depends_on:
      - bootstrap-env.md
    soft_depends_on:
      - write-readme.md
  write-readme.md:
    priority: 20
```

JSON example:

```json
{
  "tasks": {
    "bootstrap-env.md": { "priority": 100 },
    "implement-api.md": {
      "priority": 70,
      "depends_on": ["bootstrap-env.md"],
      "soft_depends_on": ["write-readme.md"]
    },
    "write-readme.md": { "priority": 20 }
  }
}
```

### 5.2 How `mato` determines readiness

`mato` reads the manifest, then combines it with live queue state from the filesystem.

A task is ready when:

- the manifest says it exists
- its file is currently in `backlog/`
- all manifest-defined hard dependencies are currently in `completed/`

This cleanly separates **definition** from **state**.

### 5.3 How agents discover dependency info

Agents read two things:

1. the task body from the `.md` file
2. dependency metadata from the manifest

This is workable, but less self-contained than frontmatter. A human or LLM reading only `implement-api.md` would not see its dependencies without also reading `.tasks/tasks.yaml`.

### 5.4 Circular dependency detection/prevention

This is the easiest format for validation because the whole graph lives in one file.

Detection flow:

- parse one manifest
- build one graph
- run cycle detection once
- reject any invalid graph before claiming starts

This is the strongest option for global consistency.

### 5.5 Impact on the current atomic `mv` flow

The claim primitive stays unchanged:

```bash
mv .tasks/backlog/task.md .tasks/in-progress/task.md
```

But the scheduler now has **two sources of information**:

- task file contents for instructions
- manifest for dependencies and priority

That means drift becomes a new operational problem:

- a task file may exist with no manifest entry
- a manifest entry may exist for a missing task file
- a task may be renamed without updating the manifest

### 5.6 Partial failures and rollback

Rollback handling is still fine as long as current filesystem state is authoritative for completion. If a dependency leaves `completed/`, dependents become blocked again.

However, manifest-only dependency declarations can make rollback debugging slightly harder because the dependency info is not attached to the affected task file.

### 5.7 Example file contents

Task file:

```markdown
# Implement API

Add the first version of the API.
```

Manifest:

```yaml
tasks:
  implement-api.md:
    depends_on:
      - bootstrap-env.md
    soft_depends_on:
      - write-readme.md
    priority: 70
```

### 5.8 Complexity of implementation in Go

**Complexity: Medium**

Work involved:

- parse a single manifest file
- correlate manifest entries with queue state
- validate drift between manifest and actual files
- build ready-set selection and cycle detection

If JSON is used, parsing can stay in the standard library. If YAML is used, it likely adds a parser dependency.

Estimated effort: roughly **200-300 LOC** plus drift-validation tests.

### 5.9 Assessment

The manifest is excellent for global validation, but it weakens the desirable property that a task is self-describing. It also creates a central file that multiple people or tools may need to edit, increasing merge-conflict risk.

---

## 6. Approach 3: Directory-based ordering

### 6.1 How dependencies are declared

Dependencies are implicit in directory layout.

Example:

```text
.tasks/backlog/
├── 01-phase-one/
│   ├── bootstrap-env.md
│   └── define-schema.md
├── 02-phase-two/
│   ├── implement-api.md
│   └── add-tests.md
└── 03-phase-three/
    └── write-docs.md
```

Typical rule:

- tasks in `02-phase-two/` depend on **all** tasks in `01-phase-one/`
- tasks in `03-phase-three/` depend on **all** tasks in `01-*` and `02-*`

### 6.2 How `mato` determines readiness

`mato` finds the earliest phase that still has incomplete tasks.

- tasks in the earliest incomplete phase are ready
- all later phases are blocked

This is a phase barrier model, not an arbitrary graph.

### 6.3 How agents discover dependency info

Agents discover dependencies by listing phase directories and only choosing tasks from the earliest open phase.

This is a major change from the current instructions because current agent logic only looks for `.md` files directly under `.tasks/backlog/`.

To support this approach, both host code and prompt instructions would need recursive directory scanning.

### 6.4 Circular dependency detection/prevention

Strict phases make cycles effectively impossible because ordering is one-way by directory number.

Validation is simple:

- parse numeric phase prefixes
- ensure all dependencies point forward only by phase number
- there is no need for graph traversal if the rule is strictly phase-based

### 6.5 Impact on the current atomic `mv` flow

This is where the approach becomes awkward.

Current claim logic assumes flat files. With phase directories, claiming needs to preserve the relative path:

```bash
mkdir -p .tasks/in-progress/02-phase-two
mv .tasks/backlog/02-phase-two/implement-api.md \
   .tasks/in-progress/02-phase-two/implement-api.md
```

Likewise, `hasAvailableTasks()` in `main.go` would need to switch from a top-level `ReadDir()` to a recursive walk.

### 6.6 Partial failures and rollback

Rollback is simple conceptually:

- if any task from an earlier phase leaves `completed/`, all later phases are blocked again

But this model is coarse. One reverted task in phase 1 can stall every task in phases 2 and 3, even if most of them do not truly depend on it.

### 6.7 Example file contents

Task body stays unchanged:

```markdown
# Implement API

Add the first version of the API.
```

Dependency meaning comes entirely from directory placement.

### 6.8 Complexity of implementation in Go

**Complexity: Low-Medium**

Work involved:

- recursive task discovery
- phase parsing from directory names
- relative-path-preserving moves
- phase barrier readiness logic

No metadata parser is required, but queue traversal and move logic become more complex than today.

Estimated effort: roughly **150-250 LOC** plus tests.

### 6.9 Assessment

This is attractive if you only need broad sequential waves of work, but it throws away too much parallelism and cannot model selective dependencies well.

It also conflicts with today's flat backlog assumptions.

---

## 7. Approach 4: Filename convention

### 7.1 How dependencies are declared

Dependencies are encoded in numeric filename prefixes.

Example:

```text
.tasks/backlog/
├── 001-bootstrap-env.md
├── 001-define-schema.md
├── 002-implement-api.md
├── 002-add-tests.md
└── 003-write-docs.md
```

A practical rule is:

- all `001-*` tasks are ready immediately
- all `002-*` tasks depend on all `001-*` tasks being completed
- all `003-*` tasks depend on all lower prefixes being completed

This is essentially phase ordering without subdirectories.

### 7.2 How `mato` determines readiness

`mato` groups backlog tasks by numeric prefix and only offers tasks from the lowest incomplete prefix.

Because files remain flat in `backlog/`, this fits the existing queue layout better than directory phases.

### 7.3 How agents discover dependency info

Agents sort task filenames and only consider the lowest-numbered group with pending work.

This is easier for the current prompt than recursive directories because the files remain in one folder.

### 7.4 Circular dependency detection/prevention

As with phases, strict numeric ordering makes hard cycles impossible.

Validation rules are simple:

- ensure filenames begin with a valid numeric prefix
- ensure ordering only points from lower to higher numbers
- optionally warn on mixed naming styles

### 7.5 Impact on the current atomic `mv` flow

This approach fits the current `mv` flow very well:

- no extra files
- no nested directories
- no parsing of Markdown metadata
- same claim operation as today

The main change is in candidate selection, not claiming.

### 7.6 Partial failures and rollback

If `001-bootstrap-env.md` is rolled back out of `completed/`, then every `002-*` and `003-*` task becomes blocked again.

As with directory phases, this can over-block the system because the dependency model is intentionally coarse.

### 7.7 Example file contents

Task body remains the current simple format:

```markdown
# Implement API

Add the first version of the API.
```

Dependency meaning lives entirely in the filename.

### 7.8 Complexity of implementation in Go

**Complexity: Low**

Work involved:

- parse numeric prefixes from filenames
- group/sort tasks by prefix
- update scheduling logic

Everything can stay in the standard library.

Estimated effort: roughly **100-180 LOC** plus tests.

### 7.9 Assessment

This is the simplest dependency mechanism to implement, but also the least expressive useful one. It only works well for mostly linear delivery plans.

It becomes awkward when you need:

- one task to depend on only one earlier task
- a task to skip over unrelated earlier work
- soft dependencies
- priority independent of ordering number

---

## 8. Approach 5: Sidecar dependency files

### 8.1 How dependencies are declared

Each task gets a sibling metadata file such as `implement-api.md.deps`.

Example task and sidecar:

```markdown
# Implement API

Add the first version of the API.
```

```yaml
depends_on:
  - bootstrap-env.md
soft_depends_on:
  - write-readme.md
priority: 70
```

The sidecar format could be YAML, JSON, TOML, or even a simple line format.

### 8.2 How `mato` determines readiness

When scanning backlog tasks, `mato` looks for the sidecar next to each `.md` file, parses it, and applies the same readiness rules as frontmatter.

This preserves a clean Markdown body while still allowing arbitrary graph definitions.

### 8.3 How agents discover dependency info

Agents read the task body from `task.md` and dependency metadata from `task.md.deps`.

This is clear enough, but slightly less convenient than frontmatter because each task now requires two reads.

### 8.4 Circular dependency detection/prevention

Cycle detection is essentially the same as frontmatter:

- parse all `.deps` files
- build graph from `depends_on`
- run DFS/Kahn cycle detection
- block cyclical tasks

### 8.5 Impact on the current atomic `mv` flow

This is the biggest weakness of sidecars.

Current claim flow moves exactly one file:

```bash
mv .tasks/backlog/task.md .tasks/in-progress/task.md
```

But now the task's metadata lives in a second file.

Problems:

- moving `task.md` without `task.md.deps` leaves metadata behind
- moving both files requires two renames, which is not atomic as a pair
- recovering orphaned tasks also has to move both artifacts
- agents and operators can accidentally desynchronize the task and its sidecar

There are only two robust fixes:

1. accept paired-file race risk, or
2. change the queue unit from a single file to a **task directory bundle**

Example bundle:

```text
.tasks/backlog/implement-api/
├── task.md
└── task.md.deps
```

Then claiming becomes an atomic directory rename. That works well technically, but it is no longer the current flat-file queue model.

### 8.6 Partial failures and rollback

Rollback logic is fine only if the `.md` and `.deps` files always move together. If they do not, the system can compute readiness against stale metadata.

So sidecars are safe only when the queue treats the task and metadata as one atomic bundle.

### 8.7 Example file contents

Task file:

```markdown
# Add tests

Cover the API handlers with integration tests.
```

Sidecar:

```yaml
depends_on:
  - implement-api.md
soft_depends_on:
  - write-readme.md
priority: 60
```

### 8.8 Complexity of implementation in Go

**Complexity: Medium-High**

Work involved:

- parse sidecar metadata
- keep task file and sidecar synchronized across all state transitions
- update orphan recovery and failure handling
- potentially migrate from file moves to directory-bundle moves

Estimated effort: roughly **250-450 LOC** plus careful race-condition tests.

### 8.9 Assessment

Sidecars keep Markdown files clean, but the atomicity mismatch with today's single-file queue is a serious design flaw. They are much more attractive if `mato` is willing to move task directories instead of individual files.

---

## 9. Priority and soft dependencies across approaches

### 9.1 Best fit by approach

| Approach | Where priority lives | Where soft deps live | Notes |
|---|---|---|---|
| Frontmatter | `priority:` field | `soft_depends_on:` field | Best overall fit |
| Manifest | task entry in manifest | task entry in manifest | Strongest validation |
| Directory phases | directory name or extra filename prefix | hard to express | Usually needs augmentation |
| Filename convention | numeric prefix or second prefix | hard to express | Soft deps are unnatural |
| Sidecar | `priority:` in `.deps` | `soft_depends_on:` in `.deps` | Good fit if atomicity solved |

### 9.2 Recommended semantics

For any expressive approach, use:

```yaml
priority: 70
depends_on:
  - setup-db.md
soft_depends_on:
  - write-docs.md
```

Scheduling rule:

- task is **not eligible** until all `depends_on` items are completed
- once eligible, incomplete `soft_depends_on` items reduce ranking but do not block execution

This makes the scheduler both strict and practical.

---

## 10. Recommendation

### Recommended primary design: **frontmatter in task files**

If the goal is to add file-based dependency modeling with the least disruption to `mato`'s current queue mechanics, frontmatter is the best option.

Why:

1. **Metadata travels with the task** as it moves between queue states.
2. **Atomic claiming remains unchanged** because the queue unit is still one file.
3. **Dependencies, soft dependencies, and priority all fit naturally** in one place.
4. **Agents and operators can understand a task by opening one file.**
5. **Rollback revalidation is simple** because the dependency definition is always attached to the task.

Recommended frontmatter schema:

```yaml
---
priority: 50
depends_on: []
soft_depends_on: []
---
```

### Runner-up: **manifest file**

If the top priority is centralized validation and graph management, a manifest is the strongest alternative. It is especially attractive if task creation will be automated by tooling rather than hand-authored.

### Not recommended for a general solution

- **directory-based ordering**: too coarse, requires recursive queue changes
- **filename convention**: too limiting for non-linear work
- **sidecar files**: elegant in theory, but awkward with today's atomic single-file claim flow

---

## 11. Suggested implementation path in Go

1. Add a task metadata parser for the chosen format.
2. Scan all task states and build a logical task index.
3. Validate missing references and hard-dependency cycles.
4. Compute the ready set from backlog tasks only.
5. Rank ready tasks by priority, then soft-dependency penalty, then lexical tie-breaker.
6. Keep the atomic `mv backlog -> in-progress` as the final claim step.
7. Revalidate hard dependencies before moving a task to `completed/`.
8. Update `task-instructions.md` so agents understand how to read and respect dependency metadata.

That gives `mato` a dependency model without changing its core filesystem-first architecture.
