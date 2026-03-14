# Approach 3: Prompt instructions for file-based agent messaging

## Goal

Teach `mato` task agents to use `.tasks/messages/events/` as a **small, reliable, advisory coordination channel**.

The prompt should make four things very clear:

1. messaging is **mandatory at a few exact checkpoints**
2. messaging is **best-effort and non-blocking**
3. agents must **change behavior when messages indicate overlap or risk**
4. agents must **not chatter**

This proposal assumes the file transport from [Approach 1](../agent-communication/approach-1-file-messaging.md): agents write immutable JSON files into `.tasks/messages/events/` and other agents discover them by scanning the directory.

---

## Reliability principles

The prompt should optimize for compliance, not elegance.

- Keep the protocol to **three message types only**: `intent`, `conflict-warning`, `completion`
- Make messaging occur at **specific existing workflow steps**, not "whenever helpful"
- Require agents to **act on relevant messages**, not merely acknowledge them
- Use **simple JSON with a tiny schema** so agents can write it correctly with a heredoc
- Keep messaging **advisory only**: failure to read or write messages must never block task execution

The task queue remains the source of truth for task ownership. Messages are only for coordination.

---

## 1. When to message

The prompt should tie messaging to the existing numbered workflow in `task-instructions.md`.

| Existing workflow step | Required messaging action | Why this timing is reliable |
|---|---|---|
| Step 5: Work on the Task, after reading the task and identifying likely files, but **before the first edit** | **Check recent messages** and then send **one `intent` message** | This is the first moment the agent has enough context to declare useful file plans |
| Step 7: Merge into Main and Push, **immediately before** `git fetch origin` / merge | **Check recent messages again** | Catches peers' recent completions or warnings before the highest-risk moment |
| Step 7: Merge into Main and Push, **only if overlap/risk exists** | Send **one `conflict-warning` message** | Warns peers that merge conflict or semantic overlap is likely |
| Step 8: Mark Complete, after Step 7 push succeeds and after switching back to the task branch, but **before** moving the task file to `completed/` | Send **one `completion` message** | Announces that merged work is now live and peers should re-read affected files |

### Exact trigger rules

The agent MUST follow these rules:

1. **Before starting real work**
   - Read the task file
   - Decide the likely files to edit
   - Check recent messages
   - Adjust the plan if needed
   - Send exactly one `intent` message
   - Only then start editing

2. **Before merging**
   - Re-scan recent messages
   - If another agent recently reported overlapping work, completion, or a conflict on the same files, treat that as actionable input
   - If the overlap creates real merge or semantic risk, send one `conflict-warning` before proceeding

3. **After successful push**
   - Send exactly one `completion` message describing the final affected files

### What should NOT trigger a message

The agent should **not** message for:

- every file edit
- every test run
- every thought or intermediate plan
- acknowledging that it read another message
- minor progress updates
- failure handling already covered by the task file retry metadata

That keeps the system sparse and predictable.

---

## 2. Message format instructions

The prompt should tell agents to use a **minimal JSON schema** and a **single safe write pattern**.

### Recommended on-disk schema

Use only these fields for MVP:

```json
{
  "type": "intent",
  "from": "agent-7",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "created_at": "2026-03-14T12:01:02Z",
  "files": ["pkg/client/http.go", "pkg/client/retry_test.go"],
  "summary": "Planning retry logic changes in HTTP client"
}
```

### Why this schema

It is intentionally small:

- `type` tells the reader how to react
- `from` lets other agents ignore their own messages
- `task_file` and `task_branch` identify the source work
- `created_at` helps reason about recency
- `files` gives the coordination target
- `summary` gives a short human-readable explanation

No nested metadata, no optional objects, no free-form mini-protocol. Simpler is more reliable for LLMs.

### Required file-writing pattern

Agents should always write a new message file with a temp file and atomic rename:

```bash
MSG_DIR=".tasks/messages/events"
AGENT_ID="${MATO_AGENT_ID:-unknown}"
MSG_TS="$(date -u +%Y%m%dT%H%M%SZ)"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RAND="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
mkdir -p "$MSG_DIR"
TMP_MSG="$(mktemp "$MSG_DIR/.tmp.${AGENT_ID}.XXXXXX")"
MSG_FILE="$MSG_DIR/${MSG_TS}-${AGENT_ID}-<type>-${RAND}.json"
```

Then write JSON with `cat`:

```bash
cat > "$TMP_MSG" <<EOF
{
  "type": "<type>",
  "from": "$AGENT_ID",
  "task_file": "<filename>",
  "task_branch": "<task-branch>",
  "created_at": "$CREATED_AT",
  "files": ["<file-1>", "<file-2>"],
  "summary": "<one-sentence summary>"
}
EOF
mv "$TMP_MSG" "$MSG_FILE"
```

### Important formatting rules for agents

The prompt should explicitly say:

- Use **double quotes** for all JSON strings
- Do **not** include comments inside JSON
- Do **not** leave trailing commas
- `files` must always be a JSON array, even for one file
- If only one file is known, use `"files": ["path/to/file"]`
- If exact files are not yet certain, list the best 1-3 likely files rather than inventing a large speculative list
- Write **one new file per message**; never edit an existing message file

---

## 3. Reading messages

The prompt should make message reading concrete and operational.

### Exact scan command

Before work starts and before merge starts, agents should run:

```bash
find ".tasks/messages/events" -maxdepth 1 -type f -name '*.json' | sort | tail -n 50 | while read -r f; do
  echo "--- $f"
  cat "$f"
  echo
done
```

If the directory does not exist yet, agents should continue without error.

### How to interpret messages

A message is **relevant** if `from` is not your own agent ID and at least one of these is true:

- `task_file` matches your task file
- any path in its `files` list overlaps with files you plan to edit, files you already changed, or files you are about to merge
- it is a recent `completion` for code you depend on
- it is a `conflict-warning` involving files you touched

### What agents must do with relevant messages

The prompt should say that agents must **adjust behavior**, not just note the message.

#### If the relevant message is `intent`

- Avoid touching the overlapping file if the task can be solved another way
- Narrow your plan to non-overlapping files when possible
- If you still must touch the same file, continue carefully and expect extra merge work later

#### If the relevant message is `completion`

- Re-read the affected files before continuing or merging
- Refresh your understanding of the changed area
- Re-run any tests that cover the affected files if your work intersects them

#### If the relevant message is `conflict-warning`

- Treat it as a required risk review
- Compare your changed files against the warned files
- Re-open overlapping files and verify your result still preserves both intents
- If you cannot confidently merge safely, follow the existing On Failure procedure instead of pushing blindly

### What agents should NOT do after reading messages

- Do not send an acknowledgement message
- Do not wait for a reply
- Do not enter a polling loop
- Do not abandon the task solely because another agent is touching similar files

The system is advisory, so the agent should adapt, not stall.

---

## 4. Guardrails against over-messaging

This is the most important anti-chattiness section.

### Hard message budget

Use a strict budget:

- **1 `intent` message per task** — mandatory
- **0 or 1 `conflict-warning` message per task** — only if real overlap/risk exists
- **1 `completion` message per task** — mandatory

That means **at most 3 messages per task**.

### Allowed reasons to send a message

An agent may only write a message for one of these reasons:

1. announcing planned edits before starting work
2. warning about likely overlap before merge
3. announcing merged completion after push succeeds

Everything else is noise and should be forbidden by the prompt.

### Additional anti-noise rules

- Keep `summary` to **one sentence**
- Keep `files` to the **most relevant files only**, ideally 1-5
- Do not send duplicate `intent` messages because the plan changed slightly
- Do not send periodic status updates
- Do not send "FYI", "noted", or "thanks" messages

A sparse bus is more likely to be used correctly.

---

## 5. Guardrails against ignoring messages

The prompt should treat message checks as mandatory workflow gates.

### Mandatory check points

Agents MUST perform a message scan:

1. before the first edit in Step 5
2. again immediately before Step 7 merge/push

The wording should be as strong as the rest of `task-instructions.md`: **"Do NOT skip this check."**

### Behavioral requirements

The prompt should also require observable follow-through:

- If a relevant `completion` exists, re-read those files before merging
- If a relevant `conflict-warning` exists, do a deliberate overlap review before merging
- If the warning suggests an unsafe merge and the agent cannot resolve it confidently, use the existing On Failure path
- Do not claim compliance by merely saying "I checked messages"

### Good enforcement wording

A strong line for the prompt is:

> If a relevant message indicates overlapping files or recent merged changes, you MUST adapt your work or merge plan to account for it. Do not merely acknowledge the message and continue unchanged.

That gives the model a direct instruction to change behavior.

---

## 6. Failure tolerance

Messaging should improve coordination without becoming a new point of failure.

### Required failure behavior

If any of these fail:

- `.tasks/messages/events/` does not exist
- scanning messages returns no files
- reading a message file fails
- writing a message file fails
- a message appears malformed

then the agent should:

1. make one reasonable attempt to continue
2. treat messaging as unavailable
3. continue executing the task normally
4. never fail the task solely because messaging failed

### Prompt wording to use

> Messaging is best-effort advisory coordination. If messaging commands fail, continue the task without blocking, waiting, or retry loops.

This is important because it prevents the communication channel from becoming more critical than the work itself.

---

## 7. Concrete prompt text

Below is the prompt text I would add to `task-instructions.md`. It matches the existing imperative, step-by-step style.

### Add this new section under `## General Guidelines` or immediately before `## Workflow`

````md
## Coordination Messages

Agents use `.tasks/messages/events/` for advisory coordination.

**CRITICAL: Messaging is mandatory at the checkpoints below, but it is advisory only. Do NOT block the task if messaging fails. Do NOT send extra status chatter.**

You may only send these message types:

- `intent`
- `conflict-warning`
- `completion`

You may send at most:

- 1 `intent` message per task
- 1 `conflict-warning` message per task
- 1 `completion` message per task

Use this setup before writing any message:

```bash
MSG_DIR=".tasks/messages/events"
AGENT_ID="${MATO_AGENT_ID:-unknown}"
MSG_TS="$(date -u +%Y%m%dT%H%M%SZ)"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RAND="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
mkdir -p "$MSG_DIR"
TMP_MSG="$(mktemp "$MSG_DIR/.tmp.${AGENT_ID}.XXXXXX")"
MSG_FILE="$MSG_DIR/${MSG_TS}-${AGENT_ID}-<type>-${RAND}.json"
```

Write JSON messages with `cat` and atomically rename them into place with `mv`. Never edit an existing message file.
````

### Add these Step 5 instructions

Insert these substeps at the start of Step 5, after the agent reads the task and identifies likely files, but before making edits:

````md
6. Before making your first code change, check recent coordination messages. Do NOT skip this check.
   ```bash
   if [ -d ".tasks/messages/events" ]; then
     find ".tasks/messages/events" -maxdepth 1 -type f -name '*.json' | sort | tail -n 50 | while read -r f; do
       echo "--- $f"
       cat "$f"
       echo
     done
   fi
   ```
7. If another agent recently announced overlapping files, adjust your plan if possible to reduce overlap. Do not merely acknowledge the message.
8. Before your first edit, send exactly one `intent` message describing the files you expect to modify.
   ```bash
   MSG_DIR=".tasks/messages/events"
   AGENT_ID="${MATO_AGENT_ID:-unknown}"
   MSG_TS="$(date -u +%Y%m%dT%H%M%SZ)"
   CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
   RAND="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
   mkdir -p "$MSG_DIR"
   TMP_MSG="$(mktemp "$MSG_DIR/.tmp.${AGENT_ID}.XXXXXX")"
   MSG_FILE="$MSG_DIR/${MSG_TS}-${AGENT_ID}-intent-${RAND}.json"
   cat > "$TMP_MSG" <<EOF
   {
     "type": "intent",
     "from": "$AGENT_ID",
     "task_file": "<filename>",
     "task_branch": "<task-branch>",
     "created_at": "$CREATED_AT",
     "files": ["<file-1>", "<file-2>"],
     "summary": "<one-sentence summary>"
   }
   EOF
   mv "$TMP_MSG" "$MSG_FILE"
   ```
9. If messaging fails, continue the task. Do not block, wait, or retry in a loop.
````

### Add these Step 7 instructions

Insert these substeps at the start of Step 7, before `git fetch origin`:

````md
Before fetching and merging, check recent coordination messages again. Do NOT skip this check.

```bash
if [ -d ".tasks/messages/events" ]; then
  find ".tasks/messages/events" -maxdepth 1 -type f -name '*.json' | sort | tail -n 50 | while read -r f; do
    echo "--- $f"
    cat "$f"
    echo
  done
fi
```

If a relevant `completion` or `conflict-warning` message mentions files you changed or are about to merge, re-read those files and adjust your merge plan. Do not merely acknowledge the message.

If you detect real overlap or merge risk, send one `conflict-warning` message before proceeding:

```bash
MSG_DIR=".tasks/messages/events"
AGENT_ID="${MATO_AGENT_ID:-unknown}"
MSG_TS="$(date -u +%Y%m%dT%H%M%SZ)"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RAND="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
mkdir -p "$MSG_DIR"
TMP_MSG="$(mktemp "$MSG_DIR/.tmp.${AGENT_ID}.XXXXXX")"
MSG_FILE="$MSG_DIR/${MSG_TS}-${AGENT_ID}-conflict-warning-${RAND}.json"
cat > "$TMP_MSG" <<EOF
{
  "type": "conflict-warning",
  "from": "$AGENT_ID",
  "task_file": "<filename>",
  "task_branch": "<task-branch>",
  "created_at": "$CREATED_AT",
  "files": ["<file-1>", "<file-2>"],
  "summary": "<one-sentence overlap or merge risk summary>"
}
EOF
mv "$TMP_MSG" "$MSG_FILE"
```

If messaging fails, continue the task. Do not block, wait, or retry in a loop.
````

### Add these Step 8 instructions

Insert these substeps in Step 8 after switching back to the task branch and before moving the task file to `completed/`:

````md
Before moving the task file to `completed/`, send exactly one `completion` message describing the final files that were merged:

```bash
MSG_DIR=".tasks/messages/events"
AGENT_ID="${MATO_AGENT_ID:-unknown}"
MSG_TS="$(date -u +%Y%m%dT%H%M%SZ)"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RAND="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
mkdir -p "$MSG_DIR"
TMP_MSG="$(mktemp "$MSG_DIR/.tmp.${AGENT_ID}.XXXXXX")"
MSG_FILE="$MSG_DIR/${MSG_TS}-${AGENT_ID}-completion-${RAND}.json"
cat > "$TMP_MSG" <<EOF
{
  "type": "completion",
  "from": "$AGENT_ID",
  "task_file": "<filename>",
  "task_branch": "<task-branch>",
  "created_at": "$CREATED_AT",
  "files": ["<file-1>", "<file-2>"],
  "summary": "Merged and pushed task changes"
}
EOF
mv "$TMP_MSG" "$MSG_FILE"
```

If messaging fails, continue completing the task. Do not block task completion on messaging.
````

### Why this wording fits the current prompt

It matches the rest of `task-instructions.md` because it is:

- imperative
- tied to exact step numbers
- command-oriented
- explicit about failure behavior
- strict about required actions

That style gives the model less room to reinterpret the protocol.

---

## 8. Message template examples

These are the exact JSON bodies agents should use after replacing placeholders.

### `intent`

```json
{
  "type": "intent",
  "from": "agent-7",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "created_at": "2026-03-14T12:01:02Z",
  "files": ["pkg/client/http.go", "pkg/client/retry_test.go"],
  "summary": "Planning retry logic changes in HTTP client"
}
```

### `conflict-warning`

```json
{
  "type": "conflict-warning",
  "from": "agent-7",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "created_at": "2026-03-14T12:08:55Z",
  "files": ["pkg/client/http.go"],
  "summary": "Possible overlap before merge on pkg/client/http.go"
}
```

### `completion`

```json
{
  "type": "completion",
  "from": "agent-7",
  "task_file": "add-retry-logic.md",
  "task_branch": "task/add-retry-logic",
  "created_at": "2026-03-14T12:15:31Z",
  "files": ["pkg/client/http.go", "pkg/client/retry.go", "pkg/client/retry_test.go"],
  "summary": "Merged and pushed retry logic changes"
}
```

---

## Recommendation

The prompt should be opinionated and repetitive:

- **message only at fixed checkpoints**
- **use only three message types**
- **write only tiny JSON files**
- **check messages before editing and before merging**
- **adapt behavior when messages are relevant**
- **never block on messaging failure**

That combination is the best chance of getting consistent LLM behavior: enough structure to force compliance, but not so much protocol complexity that agents either spam the bus or stop using it.