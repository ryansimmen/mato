# Approach 5: Task file format and metadata design

## Recommendation

Use **YAML frontmatter for author-defined metadata** and keep **runtime metadata as HTML comments**.

That gives mato a single human-editable Markdown file, keeps metadata close to the task instructions, preserves backward compatibility with today's plain `# Title` + body format, and only requires a small amount of Go parsing code. The host can strip frontmatter before presenting task instructions to an agent, while continuing to use lightweight HTML comments for runtime state such as `claimed-by` and `failure` records.

If the project strongly prefers a stricter syntax over YAML's flexibility, **TOML frontmatter is the best runner-up**. The other options are workable, but each gives up too much in readability, parser clarity, prompt cleanliness, or operational simplicity.

---

## Current mato behavior

From the current implementation:

- Tasks are discovered as `.md` files in `.tasks/backlog/`.
- Task content is treated as plain Markdown.
- The agent instruction template says the `# ` heading is the task title and the rest is the body.
- The agent is told to ignore leading HTML comment metadata lines.
- Runtime metadata is already written into task files as HTML comments:
  - claim records: `<!-- claimed-by: ... claimed-at: ... -->`
  - failure records: `<!-- failure: ... -->`
- The host itself currently only parses runtime metadata with lightweight logic, for example `parseClaimedBy()` uses a regex against `<!-- claimed-by: ... -->`.

That means any new format has to satisfy two audiences:

1. **The mato host**, which wants structured fields it can parse cheaply and reliably.
2. **The autonomous agent**, which wants a clean Markdown instruction body.

---

## Design goals

A good task format for mato should:

1. Stay readable in raw files in a normal editor.
2. Keep the main task instructions in Markdown.
3. Be easy for Go code to parse without a full Markdown AST.
4. Allow optional metadata with sensible defaults.
5. Preserve existing runtime annotations and retry tracking.
6. Be backward compatible with current plain Markdown tasks.
7. Avoid forcing authors to maintain two separate sources of truth.

---

## Metadata schema to support

These fields are sufficient for a strong v1:

| Field | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `id` | string | no | filename stem | Canonical task identifier. Should be stable even if the filename later changes. |
| `priority` | string or int | no | `medium` | Accept `high`/`medium`/`low` and optionally `1..N`. Normalize internally. |
| `depends_on` | list of strings | no | empty | References task IDs, not filenames. Filename fallback can be supported for legacy tasks. |
| `tags` | list of strings | no | empty | Freeform labels such as `frontend`, `testing`, `refactor`. |
| `estimated_complexity` | enum | no | `medium` | Suggested values: `simple`, `medium`, `complex`. Useful for agent/model selection. |
| `max_retries` | int | no | host default | Per-task override of the global retry limit. |
| `created_at` | timestamp string | no | empty | Prefer RFC3339 / ISO 8601 UTC. |
| `author` | string | no | empty | Human or tool that authored the task. |

### Recommended normalization rules

- `id`: default to filename stem without `.md`.
- `priority`:
  - accept `high`, `medium`, `low`
  - optionally accept `1`, `2`, `3`
  - normalize internally to a sortable numeric rank, for example `high=1`, `medium=2`, `low=3`
- `depends_on`: always normalize to task IDs.
- `estimated_complexity`: restrict to `simple`, `medium`, `complex`.
- `created_at`: parse as RFC3339, but store the original string if parsing fails so mato can warn without discarding the task.

### Suggested Go shape

```go
type TaskFile struct {
    Path            string
    Metadata        TaskMetadata
    Runtime         TaskRuntime
    Title           string
    BodyMarkdown    string
    RawMarkdownBody string
}

type TaskMetadata struct {
    ID                  string    `yaml:"id" toml:"id" json:"id"`
    Priority            Priority  `yaml:"priority" toml:"priority" json:"priority"`
    DependsOn           []string  `yaml:"depends_on" toml:"depends_on" json:"depends_on"`
    Tags                []string  `yaml:"tags" toml:"tags" json:"tags"`
    EstimatedComplexity string    `yaml:"estimated_complexity" toml:"estimated_complexity" json:"estimated_complexity"`
    MaxRetries          *int      `yaml:"max_retries" toml:"max_retries" json:"max_retries"`
    CreatedAt           string    `yaml:"created_at" toml:"created_at" json:"created_at"`
    Author              string    `yaml:"author" toml:"author" json:"author"`
}

type TaskRuntime struct {
    ClaimedBy   string
    ClaimedAt   string
    Failures    []FailureRecord
}
```

---

## Parsing strategy in Go

### Efficient parsing without heavy dependencies

The host does **not** need a Markdown parser. A simple line-oriented parser is enough:

1. Read the file once.
2. Check the first line:
   - `---` => try YAML frontmatter
   - `+++` => try TOML frontmatter
   - `<!--` => possible leading runtime comments or HTML-comment metadata
   - anything else => legacy plain Markdown
3. Split the file into three logical regions:
   - **author metadata** (optional)
   - **runtime annotations** (optional)
   - **Markdown body**
4. Parse the Markdown body title from the first `# ` heading if needed.
5. Apply defaults if author metadata is absent.

### Suggested implementation shape

- Use `bufio.Scanner` or a manual line splitter.
- Detect frontmatter only when the delimiter is on the **first line of the file**.
- Find the matching closing delimiter on its own line.
- Parse only that slice of bytes with a structured decoder.
- Parse runtime comments separately with very small regexes or prefix checks.

### Dependency guidance

- **YAML frontmatter**: `gopkg.in/yaml.v3` is sufficient and small enough for this job.
- **TOML frontmatter**: `github.com/BurntSushi/toml` is a good fit.
- **JSON sidecar**: standard library `encoding/json` needs no extra dependency.

The important point is that mato should own the **frontmatter detection and prompt stripping logic** itself, rather than depending on a full static-site frontmatter framework.

---

## How metadata should be stripped before agent execution

Today, the agent reads the task file directly and is told to ignore top HTML comments. With richer metadata, mato should treat the task file as structured input and expose only the Markdown instructions to the agent.

Recommended behavior:

1. Parse author metadata first.
2. Parse runtime annotations next.
3. Build the agent-visible task body from the remaining Markdown only.
4. Optionally prepend a **normalized metadata summary** outside the raw file format if the agent should know about priority or dependencies.

For example, the host could construct an internal prompt like:

```text
Task metadata:
- id: refactor-auth-flow
- priority: high
- depends_on: setup-integration-tests
- estimated_complexity: complex

Task instructions:
# Refactor auth flow

...
```

That is cleaner than exposing the raw frontmatter syntax to every agent, while still keeping the task file human-friendly on disk.

If mato continues to let the agent read the task file directly, `task-instructions.md` should be updated to say:

- ignore an optional YAML or TOML frontmatter block at the top of the file
- ignore runtime HTML comment metadata lines
- use the first `# ` heading after metadata as the task title

---

## Format evaluation

### 1. YAML frontmatter

Example shape:

```markdown
---
id: refactor-auth-flow
priority: high
depends_on:
  - setup-integration-tests
  - add-auth-fixtures
tags:
  - backend
  - auth
  - refactor
estimated_complexity: complex
max_retries: 5
created_at: 2026-02-10T14:30:00Z
author: ryan.simmen
---
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages
```

### Parsing complexity in Go

- **Delimiter detection** is trivial: look for `---` on line 1 and the matching closing `---`.
- **Structured parse** is straightforward with `yaml.v3`.
- No Markdown parser is needed.
- Avoid regex for the actual metadata payload; use a structured decoder once the block is isolated.

Complexity: **low to medium**.

### Readability for humans writing tasks

- Very good for most developers.
- Lists are easy to read and edit.
- The metadata stays close to the task body in a single file.
- Slight downside: YAML is whitespace-sensitive and allows a lot of syntax, so style should be constrained.

Human readability: **high**.

### Readability for LLM agents executing tasks

- Generally acceptable.
- Many models can ignore frontmatter if instructed.
- Still better if mato strips it before prompt construction.
- Raw YAML at the top is less confusing than a trailing metadata section, because it is clearly separate from the instructions.

Agent readability: **good if stripped, acceptable if left in place**.

### Compatibility with standard Markdown renderers

- Frontmatter is **not standard CommonMark**.
- Some markdown ecosystems understand it; plain renderers may show it as visible text or render it awkwardly.
- GitHub and VS Code source editing are still fine, but you should not assume every preview hides it.

Renderer compatibility: **medium**.

### How mato strips/ignores metadata

- If the file begins with `---`, split out the frontmatter block.
- Parse it into `TaskMetadata`.
- Remove it from the prompt body.
- Then parse any following runtime HTML comments and remove those too.

### Full example with runtime metadata coexistence

```markdown
---
id: refactor-auth-flow
priority: high
depends_on:
  - setup-integration-tests
  - add-auth-fixtures
tags:
  - backend
  - auth
  - refactor
estimated_complexity: complex
max_retries: 5
created_at: 2026-02-10T14:30:00Z
author: ryan.simmen
---
<!-- claimed-by: 4f1c8e2a  claimed-at: 2026-02-10T15:00:03Z -->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages

<!-- failure: 4f1c8e2a at 2026-02-10T15:42:18Z — refresh tests still failing -->
```

### Verdict

**Best overall choice.** It gives a strong single-file authoring experience with manageable parsing cost.

---

### 2. TOML frontmatter

Example shape:

```markdown
+++
id = "refactor-auth-flow"
priority = "high"
depends_on = ["setup-integration-tests", "add-auth-fixtures"]
tags = ["backend", "auth", "refactor"]
estimated_complexity = "complex"
max_retries = 5
created_at = "2026-02-10T14:30:00Z"
author = "ryan.simmen"
+++
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages
```

### Parsing complexity in Go

- Same delimiter-splitting model as YAML frontmatter.
- TOML decoding is predictable and strongly typed.
- Libraries are mature and easy to use.

Complexity: **low to medium**.

### Readability for humans writing tasks

- Good, especially for developers already used to config files.
- Inline arrays are concise.
- Slightly noisier than YAML for lists and string-heavy content.
- Less natural when authors want to add many tags or dependencies over time.

Human readability: **good**.

### Readability for LLM agents executing tasks

- Similar to YAML.
- The syntax is explicit enough that models usually recognize it as metadata.
- Still better if stripped before prompt construction.

Agent readability: **good if stripped, acceptable otherwise**.

### Compatibility with standard Markdown renderers

- Same issue as YAML frontmatter: not part of standard Markdown.
- Some tools tolerate it; others will display it as literal content.

Renderer compatibility: **medium**.

### How mato strips/ignores metadata

- If the file begins with `+++`, split and decode the block.
- Strip it before sending instructions to the agent.
- Parse following runtime HTML comments exactly as with YAML.

### Full example with runtime metadata coexistence

```markdown
+++
id = "refactor-auth-flow"
priority = "high"
depends_on = ["setup-integration-tests", "add-auth-fixtures"]
tags = ["backend", "auth", "refactor"]
estimated_complexity = "complex"
max_retries = 5
created_at = "2026-02-10T14:30:00Z"
author = "ryan.simmen"
+++
<!-- claimed-by: 4f1c8e2a  claimed-at: 2026-02-10T15:00:03Z -->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages

<!-- failure: 4f1c8e2a at 2026-02-10T15:42:18Z — refresh tests still failing -->
```

### Verdict

A strong technical option. If the team dislikes YAML's whitespace rules, TOML is a very reasonable alternative. It loses slightly on human ergonomics for hand-authored Markdown tasks.

---

### 3. HTML comments as metadata

Example shape:

```markdown
<!-- mato:
id=refactor-auth-flow
priority=high
depends_on=setup-integration-tests,add-auth-fixtures
tags=backend,auth,refactor
estimated_complexity=complex
max_retries=5
created_at=2026-02-10T14:30:00Z
author=ryan.simmen
-->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages
```

A single-line form like `<!-- mato: priority=1 depends_on=setup.md -->` is possible, but it becomes hard to read once lists and timestamps appear.

### Parsing complexity in Go

- Easy to detect with string prefix checks.
- Harder to make robust once values need quoting, escaping, arrays, or multiline fields.
- A regex-only solution becomes fragile quickly.
- You would likely invent a mini format inside HTML comments.

Complexity: **low for trivial metadata, medium to high for rich metadata**.

### Readability for humans writing tasks

- Good for one or two tiny fields.
- Poor once metadata gets larger.
- Comment payload syntax is ad hoc, so authors must learn mato-specific rules.
- It is easy to accidentally create parsing ambiguities.

Human readability: **medium at small scale, poor at full schema size**.

### Readability for LLM agents executing tasks

- Very good if comments are already treated as ignorable.
- Agents are unlikely to confuse HTML comments with work instructions.
- This matches today's mental model.

Agent readability: **high**.

### Compatibility with standard Markdown renderers

- Excellent. HTML comments are standard and typically hidden in rendered output.

Renderer compatibility: **high**.

### How mato strips/ignores metadata

- Strip leading `<!-- mato: ... -->` comment blocks when building the prompt.
- Continue parsing runtime comments such as `claimed-by` and `failure`.
- But now author metadata and runtime metadata share the same syntactic channel, so the host must distinguish them by prefixes.

### Full example with runtime metadata coexistence

```markdown
<!-- mato:
id=refactor-auth-flow
priority=high
depends_on=setup-integration-tests,add-auth-fixtures
tags=backend,auth,refactor
estimated_complexity=complex
max_retries=5
created_at=2026-02-10T14:30:00Z
author=ryan.simmen
-->
<!-- claimed-by: 4f1c8e2a  claimed-at: 2026-02-10T15:00:03Z -->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages

<!-- failure: 4f1c8e2a at 2026-02-10T15:42:18Z — refresh tests still failing -->
```

### Verdict

Attractive because it extends today's pattern, but it does not scale cleanly to lists, enums, validation, or future schema growth. Good for runtime annotations; weaker for author-defined metadata.

---

### 4. Separate metadata section inside the Markdown file

Example shape:

```markdown
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages

## Metadata

- id: refactor-auth-flow
- priority: high
- depends_on:
  - setup-integration-tests
  - add-auth-fixtures
- tags:
  - backend
  - auth
  - refactor
- estimated_complexity: complex
- max_retries: 5
- created_at: 2026-02-10T14:30:00Z
- author: ryan.simmen
```

### Parsing complexity in Go

- Easy to detect `## Metadata` with a line scan.
- Hard to make fully robust without defining a strict sub-format.
- Ambiguous if the task body itself legitimately contains a `## Metadata` heading.
- Stripping the section from the agent prompt requires more care than frontmatter.

Complexity: **medium**.

### Readability for humans writing tasks

- High in raw Markdown because everything looks like Markdown.
- But the metadata is mixed with the instructional content.
- Authors may accidentally edit the wrong section or move it around.

Human readability: **good**.

### Readability for LLM agents executing tasks

- Weakest option for agent clarity.
- Unless mato strips the section, many agents will read the metadata as part of the task instructions.
- Metadata at the end can also be missed by the agent if it focuses on the top of the document.

Agent readability: **poor unless stripped**.

### Compatibility with standard Markdown renderers

- Excellent, because it is ordinary Markdown.

Renderer compatibility: **high**.

### How mato strips/ignores metadata

- Scan for the reserved heading `## Metadata` at the end of the file.
- Treat everything after it as metadata, not instructions.
- This requires a stricter convention than frontmatter and is easy to break accidentally.

### Full example with runtime metadata coexistence

```markdown
<!-- claimed-by: 4f1c8e2a  claimed-at: 2026-02-10T15:00:03Z -->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages

## Metadata

- id: refactor-auth-flow
- priority: high
- depends_on:
  - setup-integration-tests
  - add-auth-fixtures
- tags:
  - backend
  - auth
  - refactor
- estimated_complexity: complex
- max_retries: 5
- created_at: 2026-02-10T14:30:00Z
- author: ryan.simmen

<!-- failure: 4f1c8e2a at 2026-02-10T15:42:18Z — refresh tests still failing -->
```

### Verdict

This looks the most like plain Markdown, but it is the most likely to leak metadata into the agent-visible instructions and the easiest to make ambiguous over time.

---

### 5. Companion sidecar file

Example shape:

`refactor-auth-flow.md`

```markdown
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages
```

`refactor-auth-flow.md.meta`

```yaml
id: refactor-auth-flow
priority: high
depends_on:
  - setup-integration-tests
  - add-auth-fixtures
tags:
  - backend
  - auth
  - refactor
estimated_complexity: complex
max_retries: 5
created_at: 2026-02-10T14:30:00Z
author: ryan.simmen
```

JSON would also work here, but YAML keeps authoring a little nicer.

### Parsing complexity in Go

- Very straightforward.
- Parse `.md` as plain Markdown and `.md.meta` as JSON/YAML/TOML.
- No prompt stripping needed because the task instructions remain pure Markdown.
- Requires extra file existence checks, synchronization rules, and error handling for drift.

Complexity: **low parser complexity, medium operational complexity**.

### Readability for humans writing tasks

- The Markdown task itself stays perfectly clean.
- But authors now have to manage two files.
- Renames, copies, and review diffs become more annoying.
- Metadata can drift from the content or be forgotten entirely.

Human readability: **mixed**.

### Readability for LLM agents executing tasks

- Best possible prompt cleanliness if the agent only reads the `.md` file.
- But if the agent needs to know metadata, the host must join information from two files anyway.

Agent readability: **high**.

### Compatibility with standard Markdown renderers

- Excellent for the `.md` file itself.
- The metadata file is outside normal Markdown tooling.

Renderer compatibility: **high**.

### How mato strips/ignores metadata

- No stripping is needed for the Markdown body.
- The host loads the sidecar separately and can optionally summarize metadata into the prompt.

### Full example with runtime metadata coexistence

`refactor-auth-flow.md`

```markdown
<!-- claimed-by: 4f1c8e2a  claimed-at: 2026-02-10T15:00:03Z -->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

Requirements:
- preserve existing API behavior
- add tests for invalid refresh tokens
- keep the diff focused on auth packages

<!-- failure: 4f1c8e2a at 2026-02-10T15:42:18Z — refresh tests still failing -->
```

`refactor-auth-flow.md.meta`

```yaml
id: refactor-auth-flow
priority: high
depends_on:
  - setup-integration-tests
  - add-auth-fixtures
tags:
  - backend
  - auth
  - refactor
estimated_complexity: complex
max_retries: 5
created_at: 2026-02-10T14:30:00Z
author: ryan.simmen
```

### Verdict

Technically clean, but it weakens the "task as one self-contained artifact" property. Good only if mato decides prompt purity matters more than single-file authoring.

---

## Comparison summary

| Option | Go parsing | Human readability | Agent readability | Markdown compatibility | Overall |
| --- | --- | --- | --- | --- | --- |
| YAML frontmatter | low-medium | high | good if stripped | medium | **best overall** |
| TOML frontmatter | low-medium | good | good if stripped | medium | strong runner-up |
| HTML comments metadata | medium at scale | medium | high | high | good for runtime, weaker for author metadata |
| Separate metadata section | medium | good | poor unless stripped | high | not recommended |
| Sidecar file | low parser / medium ops | mixed | high | high | viable, but operationally clumsy |

---

## Runtime metadata coexistence

### Keep author metadata and runtime metadata separate

These two kinds of metadata have different jobs:

- **Author metadata** is stable and intentional.
- **Runtime metadata** is transient, append-only, and written by automated agents.

They should **not** use the same storage/update mechanics.

### Recommended rule

- Use **frontmatter** for author-defined metadata.
- Use **HTML comments** for runtime annotations.

That gives each class of metadata the format that best matches its write pattern.

### Why runtime metadata should stay as comments

Current runtime updates are intentionally shell-friendly:

- prepend or inject a `claimed-by` line
- append `failure` lines
- count failures with `grep -c '<!-- failure:'`

If runtime state moved into YAML or TOML frontmatter, every claim/failure update would become a read/parse/modify/write cycle instead of a simple append or insert. That would make the agent instructions more complicated and more error-prone.

### Placement rules when frontmatter exists

If mato adopts frontmatter, runtime annotations should be placed like this:

1. **Author frontmatter first**
2. then **runtime HTML comments**
3. then the actual Markdown body
4. failure records may also be appended at the end of the file

That means the current claim step must change slightly. Today it prepends `<!-- claimed-by: ... -->` to the very top of the file. With frontmatter, prepending above line 1 would break standard frontmatter detection. So the claim logic should instead insert the comment **after the closing frontmatter delimiter**.

### Example of the recommended final layout

```markdown
---
id: refactor-auth-flow
priority: high
depends_on:
  - setup-integration-tests
tags:
  - backend
  - auth
estimated_complexity: complex
max_retries: 5
created_at: 2026-02-10T14:30:00Z
author: ryan.simmen
---
<!-- claimed-by: 4f1c8e2a  claimed-at: 2026-02-10T15:00:03Z -->
# Refactor auth flow

Update the authentication flow so token refresh and session invalidation share a single code path.

<!-- failure: 4f1c8e2a at 2026-02-10T15:42:18Z — refresh tests still failing -->
```

---

### Should runtime metadata use the same format as author metadata?

**No.**

The main reasons:

1. **Write pattern mismatch**: author metadata is edited by humans; runtime metadata is frequently mutated by automation.
2. **Operational simplicity**: shell commands can add HTML comments without needing a parser/serializer.
3. **Backward compatibility**: mato already recognizes `claimed-by` and `failure` comments.
4. **Failure resilience**: append-only comments are less fragile than rewriting a structured frontmatter block after every retry.

If mato wants more structure later, it can standardize runtime comments without moving them into frontmatter, for example:

```html
<!-- mato-claim: agent=4f1c8e2a claimed_at=2026-02-10T15:00:03Z -->
<!-- mato-failure: agent=4f1c8e2a failed_at=2026-02-10T15:42:18Z reason="refresh tests still failing" -->
```

But even that is optional; the existing comment forms are serviceable.

---

## Backward compatibility

Tasks with no metadata should continue to work exactly as they do today.

### Behavior for legacy tasks

If a task file has no frontmatter and no author metadata:

- `id` = filename stem
- `priority` = `medium`
- `depends_on` = empty list
- `tags` = empty list
- `estimated_complexity` = `medium`
- `max_retries` = host default
- `created_at` = empty
- `author` = empty
- the full file body remains valid task instructions

### Migration story

This allows a gradual rollout:

1. Existing `.md` tasks keep working unchanged.
2. New tasks can start using frontmatter immediately.
3. The host parser can support both legacy plain Markdown and frontmatter files.
4. Runtime comment parsing remains unchanged for both formats.

That is important for mato because the current codebase and agent instructions already assume `.md` tasks, simple HTML comment annotations, and filename-based task handling.

---

## Recommended v1 contract

### File format

- `.md` remains the task file extension.
- Optional YAML frontmatter may appear at the top of the file.
- Runtime HTML comments may appear immediately after frontmatter and/or at end of file.
- The task body is regular Markdown.

### Parser contract

The host should expose a single parser that returns:

- normalized author metadata
- parsed runtime metadata
- markdown body with metadata removed
- derived title from the first heading

### Prompt contract

The host should pass the agent:

- either the stripped Markdown body only
- or the stripped Markdown body plus a normalized metadata summary generated by the host

It should **not** rely on every agent to correctly interpret raw frontmatter.

### Validation contract

Mato should validate author metadata lightly:

- reject malformed frontmatter with a clear error
- warn on unknown fields in strict mode or ignore them in permissive mode
- reject invalid `estimated_complexity`
- normalize `priority`
- treat missing `id` as filename-derived

---

## Final recommendation

Choose **YAML frontmatter for author metadata** and **keep runtime metadata as HTML comments**.

Why this is the best fit for mato:

- preserves a **single file per task**
- keeps the task itself readable as Markdown
- gives the host a **structured schema** for dependency and scheduling logic
- lets agents keep using lightweight runtime annotations without rewriting structured config blocks
- supports clean prompt building by stripping the frontmatter before execution
- remains fully backward compatible with today's plain Markdown tasks

In short:

- **Author intent** belongs in frontmatter.
- **Runtime state** belongs in comments.
- **Task instructions** stay in Markdown.
