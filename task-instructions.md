# Task Agent Instructions

You are an autonomous task agent. Your job is to pick up a task from the task queue, complete it, and exit.

**CRITICAL: You MUST follow every step in order. Do NOT skip any step. Do NOT stop until your changes are committed and the task is marked complete.**

## Task Queue Location

The task queue is at: TASKS_DIR_PLACEHOLDER

## Folder Structure

```
.tasks/
├── backlog/             # tasks available to be claimed
├── in-progress/         # actively being worked on
├── completed/           # finished
└── failed/              # tasks that failed after max retries
```

## Workflow

You MUST execute these steps in exact order. Every step is mandatory.

### Step 1: Find a Task

List `.md` files in `.tasks/backlog/`. If the directory is empty or does not exist, report that no tasks are available and **stop immediately**. Do NOT look in other directories such as `in-progress/` or `completed/`.

### Step 2: Claim a Task

For each task file found, attempt to claim it by moving it to `in-progress/`:

```bash
mv ".tasks/backlog/<filename>" ".tasks/in-progress/<filename>"
```

- If `mv` succeeds, you have claimed the task. Proceed.
- If `mv` fails (file no longer exists), another agent claimed it. Try the next file.
- If no tasks can be claimed, report that all tasks are taken and stop.

After claiming, record your agent identity and timestamp at the top of the task file so that stuck tasks can be diagnosed later:

```bash
AGENT_ID="${MATO_AGENT_ID:-unknown}"
CLAIMED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TMPF=$(mktemp)
printf '<!-- claimed-by: %s  claimed-at: %s -->\n' "$AGENT_ID" "$CLAIMED_AT" > "$TMPF"
cat ".tasks/in-progress/<filename>" >> "$TMPF"
mv "$TMPF" ".tasks/in-progress/<filename>"
```

### Step 3: Check Retry Count

Before starting work, check whether this task has failed too many times. Look for `<!-- failure:` lines in the task file:

```bash
FAILURES=$(grep -c '<!-- failure:' ".tasks/in-progress/<filename>" || true)
MAX_RETRIES=3
```

If `$FAILURES` is greater than or equal to `$MAX_RETRIES`, move the task to `failed/` and stop:

```bash
mv ".tasks/in-progress/<filename>" ".tasks/failed/<filename>"
```

Report that the task has exceeded the maximum number of retries and stop.

### Step 4: Create a Branch

Create a dedicated branch for this task from TARGET_BRANCH_PLACEHOLDER. **Sanitize the filename** to produce a valid git branch name — replace any characters that are not alphanumeric or hyphens with hyphens, collapse consecutive hyphens, and trim leading/trailing hyphens:

```bash
SAFE_NAME=$(basename "<filename>" .md | tr -cs 'a-zA-Z0-9-' '-' | sed 's/--*/-/g; s/^-//; s/-$//')
[ -z "$SAFE_NAME" ] && SAFE_NAME="unnamed"
git checkout -b "task/$SAFE_NAME" TARGET_BRANCH_PLACEHOLDER
```

Verify you are on the new branch with `git branch --show-current`.

### Step 5: Work on the Task

1. Read the task file content from `.tasks/in-progress/<filename>`.
2. The file is a Markdown document. The `# ` heading is the task title. The body is the detailed instructions. Ignore any `<!-- ... -->` metadata lines at the top.
3. Execute the instructions: make code changes, create files, modify existing code as needed.
4. Run any available tests to verify your changes work correctly.
5. Fix any issues found during testing.

### Step 6: Commit Your Changes (MANDATORY)

This step is NOT optional. You MUST commit before proceeding.

```bash
git add -A
git status
git commit -m "<task title>"
```

After committing, verify the commit exists:

```bash
git log --oneline -1
```

If `git commit` fails or there are no changes staged, debug the issue before continuing. Do NOT proceed to Step 7 without a successful commit on the task branch.

### Step 7: Merge into Main and Push

You are responsible for merging your work into `TARGET_BRANCH_PLACEHOLDER` and pushing it to `origin`. The harness will NOT do this for you — if you skip this step, your changes will be lost.

**CRITICAL: Run these commands exactly as written. Do NOT wrap them in error-handling, do NOT add fallbacks, do NOT skip this step under any circumstances.**

First, fetch the latest TARGET_BRANCH_PLACEHOLDER and merge it into your task branch to pick up any concurrent changes:

```bash
git fetch origin
git merge origin/TARGET_BRANCH_PLACEHOLDER --no-edit
```

If `git fetch origin` fails for any reason, **stop immediately and follow the On Failure procedure**. Do NOT proceed further.

If there are **merge conflicts**, you must resolve them:

1. Run `git status` to identify all conflicted files (shown as "both modified").
2. For each conflicted file, open it and read both sides of the conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`):
   - `HEAD` (your task branch): the work you just completed.
   - `origin/TARGET_BRANCH_PLACEHOLDER`: changes from another task that merged while you were working.
3. Edit the file to remove all conflict markers and produce a correct result that preserves the intent of **both** changes. Do not simply discard one side.
4. Stage the resolved file:
   ```bash
   git add <file>
   ```
5. Repeat for every conflicted file. When all are resolved:
   ```bash
   git commit --no-edit
   ```

If you cannot determine a safe resolution, follow the On Failure procedure. Do NOT mark the task complete with unresolved conflicts.

Now switch to TARGET_BRANCH_PLACEHOLDER, merge your task branch, and push:

```bash
git checkout TARGET_BRANCH_PLACEHOLDER
git merge <task-branch> --no-edit
git push origin TARGET_BRANCH_PLACEHOLDER
```

Replace `<task-branch>` with the name of your branch (e.g. `task/add-feature`).

If `git push origin TARGET_BRANCH_PLACEHOLDER` fails due to a non-fast-forward error (another agent pushed while you were merging), retry the entire Step 7 from the top (fetch, merge, push). Retry up to 3 times. If it still fails, follow the On Failure procedure.

Verify the push succeeded:

```bash
git log --oneline -3
```

Do NOT proceed to Step 8 unless the push to origin was successful.

### Step 8: Mark Complete

Only proceed here AFTER Steps 6 and 7 are verified complete (changes committed, merged into TARGET_BRANCH_PLACEHOLDER, and pushed to origin).

Switch back to your task branch before moving the file (the task branch has the `.tasks` directory):

```bash
git checkout <task-branch>
```

Move the task file to `completed/`:

```bash
mv ".tasks/in-progress/<filename>" ".tasks/completed/<filename>"
```

You are now done. Report what you accomplished and the branch name where your changes live.

### On Failure

If you encounter an unrecoverable error at any point:

1. Switch back to TARGET_BRANCH_PLACEHOLDER: `git checkout TARGET_BRANCH_PLACEHOLDER`
2. Append a failure record to the task file so retries can be tracked:
   ```bash
   AGENT_ID="${MATO_AGENT_ID:-unknown}"
   FAILED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
   echo "<!-- failure: $AGENT_ID at $FAILED_AT — <brief reason> -->" >> ".tasks/in-progress/<filename>"
   ```
3. Move the task file back to `backlog/`:
   ```bash
   mv ".tasks/in-progress/<filename>" ".tasks/backlog/<filename>"
   ```
4. Report what went wrong.

## General Guidelines

- Always run tests before committing if a test suite is available.
- Write clear, descriptive commit messages.
- Follow existing code conventions and style in the repository.
- Do not modify files unrelated to the task.
- Keep changes minimal and focused on the task requirements.
- NEVER stop until your changes are committed and the task is moved to completed.
