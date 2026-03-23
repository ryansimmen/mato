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
	return reviewStateBlockN(t, state, 0)
}

// reviewStateBlockN extracts the nth (0-indexed) ```bash block from the named
// STATE section in review-instructions.md.
func reviewStateBlockN(t *testing.T, state string, n int) string {
	t.Helper()
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}

	header := "## STATE: " + state
	sectionStart := strings.Index(string(data), header)
	if sectionStart < 0 {
		t.Fatalf("state %q not found in review instructions", state)
	}
	section := string(data[sectionStart+len(header):])

	// Limit to this state section.
	if next := strings.Index(section, "\n## STATE:"); next >= 0 {
		section = section[:next]
	}

	for i := 0; i <= n; i++ {
		blockStart := strings.Index(section, "```bash\n")
		if blockStart < 0 {
			t.Fatalf("bash block %d for state %q not found", i, state)
		}
		section = section[blockStart+len("```bash\n"):]

		blockEnd := strings.Index(section, "\n```")
		if blockEnd < 0 {
			t.Fatalf("bash block %d terminator for state %q not found", i, state)
		}
		if i == n {
			return strings.TrimSpace(section[:blockEnd])
		}
		section = section[blockEnd+len("\n```"):]
	}

	t.Fatalf("unreachable: bash block %d for state %q", n, state)
	return ""
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

// TestReviewEmptyDiffRejects runs the real VERIFY_REVIEW, DIFF, and VERDICT
// blocks when the task branch has no changes relative to the target branch.
// The DIFF decision table says to reject with "branch contains no changes".
func TestReviewEmptyDiffRejects(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Create a task branch that is identical to mato (no diff).
	mustGitOutput(t, repoRoot, "branch", "task/empty-diff", "mato")

	writeTask(t, tasksDir, queue.DirReadyReview, "empty-diff.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/empty-diff -->\n"+
			"# Empty Diff Task\nShould be rejected.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-empty-diff.md.json")

	// Build the real VERDICT reject block with the empty-diff reason substituted
	// into the placeholder.
	rejectBlock := strings.Replace(
		reviewStateBlockN(t, "VERDICT", 1),
		`<one-paragraph summary of the specific issue(s) found>`,
		`branch contains no changes`, 1)

	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
		// Execute the real DIFF block (fetch + list changed files).
		reviewStateBlock(t, "DIFF"),
		// Check for empty diff per the DIFF state decision table.
		`DIFF_FILES="$(git diff --name-only "mato...origin/$BRANCH" 2>/dev/null || true)"`,
		`if [ -z "$DIFF_FILES" ]; then`,
		rejectBlock,
		`fi`,
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
// for the task branch, the real DIFF block sets FAIL_STEP/FAIL_REASON and the
// ON_FAILURE block writes a verdict file with {"verdict":"error",...}.
func TestReviewFetchFailureWritesErrorVerdict(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirReadyReview, "fetch-fail.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/nonexistent-branch -->\n"+
			"# Fetch Fail Task\nBranch does not exist.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-fetch-fail.md.json")

	// Execute the real DIFF block. After the fetch fails, the block sets
	// FAIL_STEP and FAIL_REASON. The subsequent git diff command will also
	// fail (branch doesn't exist), so we wrap to allow the script to
	// continue to the ON_FAILURE transition.
	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
		`FAIL_STEP=""`,
		`FAIL_REASON=""`,
		`{ ` + reviewStateBlock(t, "DIFF") + `; } 2>/dev/null || true`,
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
// verdict file when written through the real VERDICT reject block.
func TestReviewRejectReasonWithSpecialChars(t *testing.T) {
	tests := []struct {
		name   string
		reason string // text substituted into the VERDICT reject block placeholder
		check  func(t *testing.T, v reviewVerdict)
	}{
		{
			name:   "double quotes",
			reason: `missing error wrapping in \"postAgentPush\" — the function does not call fmt.Errorf with %w`,
			check: func(t *testing.T, v reviewVerdict) {
				t.Helper()
				if !strings.Contains(v.Reason, "postAgentPush") {
					t.Fatalf("reason = %q, want it to contain 'postAgentPush'", v.Reason)
				}
			},
		},
		{
			name:   "embedded newlines",
			reason: `line one\\nline two`,
			check: func(t *testing.T, v reviewVerdict) {
				t.Helper()
				if !strings.Contains(v.Reason, "\n") {
					t.Fatalf("reason = %q, want it to contain a literal newline", v.Reason)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

			writeTask(t, tasksDir, queue.DirReadyReview, "special-chars.md",
				"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
					"<!-- branch: task/special-chars -->\n"+
					"# Special Chars Task\nTest special characters.\n")

			cloneDir := createPromptClone(t, repoRoot, tasksDir)
			cloneTasksDir := filepath.Join(cloneDir, ".tasks")

			verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-special-chars.md.json")

			// Use the real VERDICT reject block with the placeholder replaced.
			rejectBlock := strings.Replace(
				reviewStateBlockN(t, "VERDICT", 1),
				`<one-paragraph summary of the specific issue(s) found>`,
				tt.reason, 1)

			script := strings.Join([]string{
				reviewPreamble(t),
				`FILENAME="special-chars.md"`,
				`BRANCH="task/special-chars"`,
				`VERDICT_PATH="` + verdictPath + `"`,
				rejectBlock,
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
			tt.check(t, v)
		})
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

// TestReviewVerdictApproveFormat verifies that the real VERDICT state approve
// block produces a well-formed JSON verdict file.
func TestReviewVerdictApproveFormat(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-approve-test.md.json")

	// Execute the real VERDICT approve block (first bash block in VERDICT state).
	script := strings.Join([]string{
		reviewPreamble(t),
		`FILENAME="approve-test.md"`,
		`BRANCH="task/approve-test"`,
		`VERDICT_PATH="` + verdictPath + `"`,
		reviewStateBlock(t, "VERDICT"),
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
