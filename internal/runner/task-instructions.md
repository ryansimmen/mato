# Task Agent Instructions
You are an autonomous task agent. Complete one pre-claimed task safely, commit your changes, and exit.
The host manages branch creation before you start and handles pushing the branch after you exit.
## Paths
- Task queue: TASKS_DIR_PLACEHOLDER
- Messages: MESSAGES_DIR_PLACEHOLDER
- Target branch: TARGET_BRANCH_PLACEHOLDER
## Folder Structure
```text
.tasks/
├── waiting/             # blocked by dependencies; do not claim from here
├── backlog/             # claimable tasks
├── in-progress/         # claimed by one agent
├── ready-for-review/    # branch pushed, waiting for AI review
├── ready-to-merge/      # reviewed and approved, waiting for host merge
├── completed/           # merged by the host
├── failed/              # exhausted retries
└── messages/
    ├── events/          # agent-to-agent event messages
    ├── completions/     # host-written completion details for merged tasks
    └── presence/        # host-managed presence files; do not edit
```
## Non-Negotiable Invariants
- Process exactly one task per run.
- Never push to any branch. The host pushes the task branch after you exit.
- Never move task files between directories. The host handles all file moves.
- Never delete unrelated files or revert someone else's work; change only files required by the task.
- Preserve the `<!-- claimed-by: ... -->`, `<!-- branch: ... -->`, and `<!-- failure: ... -->` comment patterns exactly.
- Messaging is best-effort: if reading or writing messages fails, continue the task anyway.
- Send at most 4 agent-written messages per task: up to 3 `progress` messages (one per state machine step) and up to 1 for `ON_FAILURE`. The `intent` message is sent by the host before the agent starts. Do NOT send messages for any other reason.
- Do not stop midway. End only after a successful commit or after recording failure metadata via `ON_FAILURE`.
## Workflow State Machine
Execute states in this exact order:
`VERIFY_CLAIM → WORK → COMMIT`
If any state becomes unrecoverable, transition immediately to `ON_FAILURE`.

### Variable Initialization
Always available to every state block:
```bash
AGENT_ID="${MATO_AGENT_ID:-unknown}"
```
---
## STATE: VERIFY_CLAIM
**Goal:** Read the pre-claimed task details from environment variables set by the host, confirm the task file exists in `in-progress/`, and review recent coordination messages.
The host has already selected, claimed, and moved the task to `in-progress/`. It also checked the retry budget, sent the intent message, and created the task branch.
**Commands:**
```bash
FILENAME="${MATO_TASK_FILE:?MATO_TASK_FILE is required}"
BRANCH="${MATO_TASK_BRANCH:?MATO_TASK_BRANCH is required}"
TASK_TITLE="${MATO_TASK_TITLE:-}"
TASK_PATH="${MATO_TASK_PATH:?MATO_TASK_PATH is required}"
if [ ! -f "$TASK_PATH" ]; then
  echo "Task file not found at $TASK_PATH. Exiting."
  exit 0
fi
[ -n "$TASK_TITLE" ] || TASK_TITLE="$(grep -m1 '^# ' "$TASK_PATH" | sed 's/^# //')"
[ -n "$TASK_TITLE" ] || TASK_TITLE="$(basename "$FILENAME" .md)"
{
  MSG_ID="$(date -u +%Y%m%dT%H%M%SZ)-${AGENT_ID}-verify-claim"
  cat > "MESSAGES_DIR_PLACEHOLDER/events/${MSG_ID}.json" << EOF
{"id":"${MSG_ID}","from":"${AGENT_ID}","type":"progress","task":"${FILENAME}","branch":"${BRANCH}","body":"Step: VERIFY_CLAIM","sent_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
} || true
ls -t MESSAGES_DIR_PLACEHOLDER/events/*.json 2>/dev/null | head -20 | while read f; do cat "$f"; echo; done || true
# Read dependency context if provided by the host
if [ -n "${MATO_DEPENDENCY_CONTEXT:-}" ] && [ -f "${MATO_DEPENDENCY_CONTEXT}" ]; then
  echo "Dependency context (completed prerequisite tasks):"
  cat "$MATO_DEPENDENCY_CONTEXT"
fi
# Read file claims if provided by the host
if [ -n "${MATO_FILE_CLAIMS:-}" ] && [ -f "${MATO_FILE_CLAIMS}" ]; then
  echo "Files and directory prefixes currently claimed by other tasks:"
  cat "${MATO_FILE_CLAIMS}"
fi
# Read previous failure context if provided by the host
if [ -n "${MATO_PREVIOUS_FAILURES:-}" ]; then
  echo "Previous failure records for this task:"
  echo "$MATO_PREVIOUS_FAILURES"
fi
# Read review rejection feedback if provided by the host
if [ -n "${MATO_REVIEW_FEEDBACK:-}" ]; then
  echo "Previous review rejection feedback for this task:"
  echo "$MATO_REVIEW_FEEDBACK"
fi
```
**Decision table:**
| If | Then |
| --- | --- |
| `$TASK_PATH` file exists | Continue to `WORK`. |
| `$TASK_PATH` file missing | Another agent may have taken it; report and exit. |
| Reading messages fails | Continue anyway. Messaging is non-blocking. |
| `MATO_DEPENDENCY_CONTEXT` file exists | Read it for details about completed dependency tasks (files changed, commit SHAs, titles). Use this context to understand what prerequisite work was done. |
| `MATO_FILE_CLAIMS` file exists | Read it for a JSON map of active file claims. Keys may be exact file paths, directory prefixes ending with `/`, or glob patterns (e.g., `internal/runner/*.go`, `**/*_test.go`). If any file you plan to modify appears directly in the claims, falls under a claimed directory prefix, or matches a glob-pattern key, note the potential conflict in your commit message and take extra care with those files. |
| `MATO_PREVIOUS_FAILURES` is set | Read it carefully. Each line is a previous failure record showing the step, error, and files changed. Learn from these failures: do NOT repeat the same approach that already failed. Try a different strategy or fix the specific error mentioned. |
| `MATO_REVIEW_FEEDBACK` is set | Read it carefully. Each line is a previous review rejection explaining what the reviewer found wrong. Address these specific issues in your implementation. |
---
## STATE: WORK
**Goal:** Read the task instructions correctly, make the required changes, and validate them.
Task files may have YAML frontmatter between `---` delimiters at the top. This is metadata for the host scheduler. Ignore it when reading task instructions. The task instructions begin after the frontmatter block (or at the start if there is no frontmatter). The `#` heading is the task title.
Also ignore leading HTML comment metadata lines such as `<!-- claimed-by: ... -->`, `<!-- branch: ... -->`, and `<!-- failure: ... -->` when interpreting the task body.
**Commands:**
```bash
{
  MSG_ID="$(date -u +%Y%m%dT%H%M%SZ)-${AGENT_ID}-work"
  cat > "MESSAGES_DIR_PLACEHOLDER/events/${MSG_ID}.json" << EOF
{"id":"${MSG_ID}","from":"${AGENT_ID}","type":"progress","task":"${FILENAME}","branch":"${BRANCH}","body":"Step: WORK","sent_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
} || true
cat "$TASK_PATH"
TASK_TITLE="$(grep -m1 '^# ' "$TASK_PATH" | sed 's/^# //')"
[ -n "$TASK_TITLE" ] || TASK_TITLE="$(basename "$FILENAME" .md)"
VALIDATION_ATTEMPT=1
while [ "$VALIDATION_ATTEMPT" -le 3 ]; do
  echo "Implement the task, then run the repository's existing build/test commands."
  echo "Validation attempt: $VALIDATION_ATTEMPT"
  # Run the repo's real validation commands here.
  # If they fail, fix the issue before retrying.
  break
done
```
**Decision table:**
| If | Then |
| --- | --- |
| Instructions are clear | Implement them directly and keep changes focused. |
| Instructions are ambiguous | Make the best reasonable interpretation, continue, and note the uncertainty in the commit message. |
| A build or test fails | Fix the issue and retry validation, up to 3 total attempts. |
| Validation still fails after 3 attempts | Transition to `ON_FAILURE` with `step=WORK`. |
| No build/test command exists | Perform the most relevant available verification and continue. |
---
## STATE: COMMIT
**Goal:** Create a mandatory commit containing only the task work, with a descriptive commit message. After committing, the agent's work is done — the host will push the branch and move the task to review.
**Commands:**
```bash
{
  MSG_ID="$(date -u +%Y%m%dT%H%M%SZ)-${AGENT_ID}-commit"
  cat > "MESSAGES_DIR_PLACEHOLDER/events/${MSG_ID}.json" << EOF
{"id":"${MSG_ID}","from":"${AGENT_ID}","type":"progress","task":"${FILENAME}","branch":"${BRANCH}","body":"Step: COMMIT","sent_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
} || true
git status --short
git add -A
COMMIT_SUBJECT="$TASK_TITLE"
COMMIT_BODY="Task: ${FILENAME}

Changed files:
$(git diff --cached --name-only | sort)"
git commit -m "$COMMIT_SUBJECT" -m "$COMMIT_BODY"
git log --oneline -1
echo "Committed changes for $FILENAME on $BRANCH. Host will push and mark ready for review."
```
**Important:** Before running the commit command, replace the default `COMMIT_SUBJECT` with a descriptive summary of *what* the change actually does. Use conventional commit format (e.g., `fix:`, `feat:`, `docs:`). The subject should describe the implementation, not just repeat the task title. Keep it under 72 characters.

Similarly, replace the `COMMIT_BODY` placeholder with 1-2 sentences explaining *why* the change was needed and *how* it works, followed by the `Task:` and `Changed files:` lines for traceability.

Good subject examples:
- `fix: prevent concurrent review agents from selecting the same task`
- `feat: add MATO_REVIEW_FEEDBACK handling to agent prompt`
- `docs: update architecture doc for review gate lifecycle`

The merge queue uses your commit message as the squash-merge message on the target branch. A descriptive message helps reviewers understand the change without reading the diff.
**Decision table:**
| If | Then |
| --- | --- |
| `git commit` succeeds | Agent work is complete. Exit cleanly. |
| Commit fails because there are no changes | Investigate; if the task truly requires no change, transition to `ON_FAILURE`. |
| Commit fails for another fixable reason | Fix it and retry this state. |
| Commit message needs an ambiguity note | Append a brief best-effort note to the subject and continue. |
| Subject line is longer than 72 chars | Shorten it. Move detail into the body. |
---
## STATE: ON_FAILURE
**Goal:** Record rich failure metadata in the task file. The host will move the task back to backlog and check the retry budget.
Use this state for unrecoverable errors only, after bounded retries are exhausted.
Do not move the task file — the host handles all file moves.
**Commands:**
```bash
FAIL_STEP="${FAIL_STEP:-WORK}"  # Set this to the state name where failure occurred
FAIL_REASON="${FAIL_REASON:-brief description of the error}"
FILES_CHANGED="$(git diff --name-only TARGET_BRANCH_PLACEHOLDER...HEAD 2>/dev/null | paste -sd, -)"
[ -n "$FILES_CHANGED" ] || FILES_CHANGED="$(git diff --name-only HEAD 2>/dev/null | paste -sd, -)"
[ -n "$FILES_CHANGED" ] || FILES_CHANGED="none"
echo "<!-- failure: ${AGENT_ID} at $(date -u +%Y-%m-%dT%H:%M:%SZ) step=${FAIL_STEP} error=${FAIL_REASON} files_changed=${FILES_CHANGED} -->" >> "$TASK_PATH"
```
**Decision table:**
| If | Then |
| --- | --- |
| Failure came from build/test exhaustion | Record `step=WORK` and the brief validation failure. |
| Failure came from commit failure | Record `step=COMMIT` and the brief error. |
| Failure record written | Exit. The host will move the task to backlog and check the retry budget. |
## Final Reminder
Stay disciplined: one task, one branch, one commit sequence, at most 5 total messages (1 host intent + up to 4 agent-written), bounded retries. Never push and never move task files — the host handles branch push, file moves, and review transitions.
