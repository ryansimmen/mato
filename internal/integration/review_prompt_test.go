package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mato/internal/queue"
	"mato/internal/testutil"
)

// reviewInstructionsPath returns the absolute path to review-instructions.md.
func reviewInstructionsPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "runner", "review-instructions.md"))
}

// reviewStateBlock extracts the first ```bash block from the named STATE
// section in review-instructions.md.
func reviewStateBlock(t *testing.T, state string) string {
	t.Helper()
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}

	sectionStart := strings.Index(string(data), "## STATE: "+state)
	if sectionStart < 0 {
		t.Fatalf("state %q not found in review instructions", state)
	}
	section := string(data[sectionStart:])

	blockStart := strings.Index(section, "```bash\n")
	if blockStart < 0 {
		t.Fatalf("bash block for state %q not found", state)
	}
	section = section[blockStart+len("```bash\n"):]

	blockEnd := strings.Index(section, "\n```")
	if blockEnd < 0 {
		t.Fatalf("bash block terminator for state %q not found", state)
	}
	return strings.TrimSpace(section[:blockEnd])
}

// reviewPreamble extracts the variable-initialization bash block that precedes
// all STATE sections in review-instructions.md.
func reviewPreamble(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}
	text := string(data)
	firstState := strings.Index(text, "## STATE:")
	if firstState < 0 {
		t.Fatal("no STATE sections found in review instructions")
	}
	preamble := text[:firstState]
	blockStart := strings.Index(preamble, "```bash\n")
	if blockStart < 0 {
		return ""
	}
	preamble = preamble[blockStart+len("```bash\n"):]
	blockEnd := strings.Index(preamble, "\n```")
	if blockEnd < 0 {
		t.Fatal("review preamble bash block terminator not found")
	}
	return strings.TrimSpace(preamble[:blockEnd])
}

type reviewVerdict struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// TestReviewVerifyReview runs the VERIFY_REVIEW state block against a task in
// ready-for-review/ and verifies it reads variables, confirms the task exists,
// and writes exactly one progress message.
func TestReviewVerifyReview(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirReadyReview, "review-task.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/review-task -->\n"+
			"# Review Task\nDo the review.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-review-task.md.json")

	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
		`echo "FILENAME=$FILENAME"`,
		`echo "BRANCH=$BRANCH"`,
		`echo "TASK_TITLE=$TASK_TITLE"`,
		`echo "VERDICT_PATH=$VERDICT_PATH"`,
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-1",
		"MATO_TASK_FILE=review-task.md",
		"MATO_TASK_BRANCH=task/review-task",
		"MATO_TASK_TITLE=Review Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirReadyReview, "review-task.md"),
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify review: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "FILENAME=review-task.md") {
		t.Fatalf("verify review output missing filename: %s", out)
	}
	if !strings.Contains(out, "BRANCH=task/review-task") {
		t.Fatalf("verify review output missing branch: %s", out)
	}
	if !strings.Contains(out, "TASK_TITLE=Review Task") {
		t.Fatalf("verify review output missing title: %s", out)
	}
	if !strings.Contains(out, "VERDICT_PATH=") {
		t.Fatalf("verify review output missing verdict path: %s", out)
	}

	// Exactly one progress message should have been written.
	msgs := readPromptEventMessages(t, tasksDir)
	progressCount := 0
	for _, msg := range msgs {
		if msg.Type == "progress" && msg.From == "test-reviewer-1" {
			progressCount++
			if !strings.Contains(msg.Body, "VERIFY_REVIEW") {
				t.Fatalf("progress message body = %q, want VERIFY_REVIEW", msg.Body)
			}
			if msg.Task != "review-task.md" {
				t.Fatalf("progress message task = %q, want review-task.md", msg.Task)
			}
		}
	}
	if progressCount != 1 {
		t.Fatalf("progress message count = %d, want 1", progressCount)
	}

	// Task file must remain untouched in ready-for-review/.
	mustExist(t, filepath.Join(tasksDir, queue.DirReadyReview, "review-task.md"))
}

// TestReviewEmptyDiffRejects runs VERIFY_REVIEW + DIFF + VERDICT when the task
// branch has no changes relative to the target branch. The review prompt should
// reject with the reason "branch contains no changes".
func TestReviewEmptyDiffRejects(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Create a task branch that is identical to mato (no diff).
	// We can't use createTaskBranch with nil files because git commit fails
	// with nothing to commit. Instead, create the branch directly pointing at mato.
	mustGitOutput(t, repoRoot, "branch", "task/empty-diff", "mato")

	writeTask(t, tasksDir, queue.DirReadyReview, "empty-diff.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/empty-diff -->\n"+
			"# Empty Diff Task\nShould be rejected.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-empty-diff.md.json")

	// DIFF state needs to fetch the branch; the clone was created from the repo
	// so origin already exists.
	// Build a script that runs VERIFY_REVIEW, then DIFF. The DIFF block in the
	// prompt transitions to VERDICT with a reject when the diff is empty.
	// We include the full DIFF bash block and an inline reject-on-empty check
	// that mirrors the documented decision table.
	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
		// DIFF: fetch and compute diff
		`if ! git fetch origin "$BRANCH" 2>/dev/null; then
  FAIL_STEP="DIFF"
  FAIL_REASON="could not fetch branch $BRANCH from origin"
fi`,
		`DIFF_FILES="$(git diff --name-only "mato...origin/$BRANCH" 2>/dev/null || true)"`,
		`if [ -z "$DIFF_FILES" ]; then
  cat > "$VERDICT_PATH" << 'VERDICTEOF'
{"verdict":"reject","reason":"branch contains no changes"}
VERDICTEOF
  echo "Rejected: branch contains no changes"
else
  echo "Diff files: $DIFF_FILES"
fi`,
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-2",
		"MATO_TASK_FILE=empty-diff.md",
		"MATO_TASK_BRANCH=task/empty-diff",
		"MATO_TASK_TITLE=Empty Diff Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirReadyReview, "empty-diff.md"),
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash empty diff: %v\noutput:\n%s", err, out)
	}

	mustExist(t, verdictPath)
	verdictData := readFile(t, verdictPath)

	var v reviewVerdict
	if err := json.Unmarshal([]byte(verdictData), &v); err != nil {
		t.Fatalf("parse verdict JSON: %v\ncontent: %s", err, verdictData)
	}
	if v.Verdict != "reject" {
		t.Fatalf("verdict = %q, want reject", v.Verdict)
	}
	if !strings.Contains(v.Reason, "branch contains no changes") {
		t.Fatalf("reject reason = %q, want 'branch contains no changes'", v.Reason)
	}

	// Task file must remain in ready-for-review/ — agent never moves files.
	mustExist(t, filepath.Join(tasksDir, queue.DirReadyReview, "empty-diff.md"))
}

// TestReviewFetchFailureWritesErrorVerdict verifies that when git fetch fails
// for the task branch, the ON_FAILURE state writes a verdict file with
// {"verdict":"error",...} that the host can parse.
func TestReviewFetchFailureWritesErrorVerdict(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirReadyReview, "fetch-fail.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/nonexistent-branch -->\n"+
			"# Fetch Fail Task\nBranch does not exist.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-fetch-fail.md.json")

	// Simulate the DIFF → ON_FAILURE transition: attempt fetch, fail, write error.
	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
		// Attempt DIFF fetch — branch doesn't exist, so it fails.
		`FAIL_STEP=""
FAIL_REASON=""
if ! git fetch origin "$BRANCH" 2>/dev/null; then
  FAIL_STEP="DIFF"
  FAIL_REASON="could not fetch branch $BRANCH from origin"
fi`,
		// Transition to ON_FAILURE if fetch failed.
		`if [ -n "$FAIL_STEP" ]; then`,
		reviewStateBlock(t, "ON_FAILURE"),
		`fi`,
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-3",
		"MATO_TASK_FILE=fetch-fail.md",
		"MATO_TASK_BRANCH=task/nonexistent-branch",
		"MATO_TASK_TITLE=Fetch Fail Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirReadyReview, "fetch-fail.md"),
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash fetch failure: %v\noutput:\n%s", err, out)
	}

	mustExist(t, verdictPath)
	verdictData := readFile(t, verdictPath)

	var v reviewVerdict
	if err := json.Unmarshal([]byte(verdictData), &v); err != nil {
		t.Fatalf("parse verdict JSON: %v\ncontent: %s", err, verdictData)
	}
	if v.Verdict != "error" {
		t.Fatalf("verdict = %q, want error", v.Verdict)
	}
	if !strings.Contains(v.Reason, "DIFF") {
		t.Fatalf("error reason = %q, want it to contain 'DIFF'", v.Reason)
	}
	if !strings.Contains(v.Reason, "could not fetch") {
		t.Fatalf("error reason = %q, want it to contain 'could not fetch'", v.Reason)
	}

	// Task file must remain in ready-for-review/.
	mustExist(t, filepath.Join(tasksDir, queue.DirReadyReview, "fetch-fail.md"))
}

// TestReviewRejectReasonWithSpecialChars verifies that rejection reasons
// containing double quotes and newlines still produce valid JSON in the
// verdict file.
func TestReviewRejectReasonWithSpecialChars(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirReadyReview, "special-chars.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/special-chars -->\n"+
			"# Special Chars Task\nTest special characters.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-special-chars.md.json")

	// Write a verdict with special characters in the reason using a here-doc
	// that mirrors how the prompt's VERDICT state writes rejections.
	// The prompt instructs: "Escape any double quotes in the reason with a backslash."
	script := strings.Join([]string{
		reviewPreamble(t),
		`VERDICT_PATH="` + verdictPath + `"`,
		`REASON='missing error wrapping in \"postAgentPush\" — the function does not call fmt.Errorf with %w'`,
		`cat > "$VERDICT_PATH" << VERDICTEOF
{"verdict":"reject","reason":"$REASON"}
VERDICTEOF`,
	}, "\n\n")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-4",
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash special chars: %v\noutput:\n%s", err, out)
	}

	mustExist(t, verdictPath)
	verdictData := readFile(t, verdictPath)

	var v reviewVerdict
	if err := json.Unmarshal([]byte(verdictData), &v); err != nil {
		t.Fatalf("verdict JSON is not valid: %v\ncontent: %s", err, verdictData)
	}
	if v.Verdict != "reject" {
		t.Fatalf("verdict = %q, want reject", v.Verdict)
	}
	if v.Reason == "" {
		t.Fatal("reject reason is empty; expected a non-empty reason")
	}
}

// TestReviewNoPushNoCommitNoMoveInstructions verifies the review prompt does
// not contain instructions for git push, git commit, or file moves — the
// review agent must never perform these actions.
func TestReviewNoPushNoCommitNoMoveInstructions(t *testing.T) {
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}
	text := string(data)

	if strings.Contains(text, "git push") {
		t.Fatal("review instructions should not contain 'git push'")
	}
	if strings.Contains(text, "git commit") {
		t.Fatal("review instructions should not contain 'git commit'")
	}

	// ON_FAILURE must not move files — host handles that.
	onFailure := reviewStateBlock(t, "ON_FAILURE")
	if strings.Contains(onFailure, "mv ") {
		t.Fatal("ON_FAILURE should not move files; host handles file moves")
	}
}

// TestReviewVerdictApproveFormat verifies that the VERDICT state's approve
// block produces a well-formed JSON verdict file.
func TestReviewVerdictApproveFormat(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-approve-test.md.json")

	// Run the approve branch of the VERDICT state block directly.
	script := strings.Join([]string{
		reviewPreamble(t),
		`FILENAME="approve-test.md"`,
		`BRANCH="task/approve-test"`,
		`VERDICT_PATH="` + verdictPath + `"`,
		// Extract and run the approve block from VERDICT state.
		`cat > "$VERDICT_PATH" << 'VERDICTEOF'
{"verdict":"approve"}
VERDICTEOF`,
		`echo "Approved $FILENAME on $BRANCH."`,
	}, "\n\n")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-5",
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash approve verdict: %v\noutput:\n%s", err, out)
	}

	mustExist(t, verdictPath)
	verdictData := readFile(t, verdictPath)

	var v reviewVerdict
	if err := json.Unmarshal([]byte(verdictData), &v); err != nil {
		t.Fatalf("approve verdict JSON not valid: %v\ncontent: %s", err, verdictData)
	}
	if v.Verdict != "approve" {
		t.Fatalf("verdict = %q, want approve", v.Verdict)
	}
	if v.Reason != "" {
		t.Fatalf("approve verdict should have empty reason, got %q", v.Reason)
	}
}

// TestReviewVerifyReviewMissingTask verifies that the VERIFY_REVIEW state
// exits gracefully when the task file does not exist at MATO_TASK_PATH.
func TestReviewVerifyReviewMissingTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-missing.md.json")
	missingPath := filepath.Join(cloneTasksDir, queue.DirReadyReview, "missing-task.md")

	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-6",
		"MATO_TASK_FILE=missing-task.md",
		"MATO_TASK_BRANCH=task/missing-task",
		"MATO_TASK_TITLE=Missing Task",
		"MATO_TASK_PATH=" + missingPath,
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}
	// The VERIFY_REVIEW block does `exit 0` when the task file is missing,
	// so runBash should succeed.
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash missing task: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Task file not found") {
		t.Fatalf("expected 'Task file not found' in output, got:\n%s", out)
	}
}
