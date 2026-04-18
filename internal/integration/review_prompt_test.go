package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/testutil"
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

	writeTask(t, tasksDir, dirs.ReadyReview, "review-task.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/review-task -->\n"+
			"# Review Task\nDo the review.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, dirs.ReadyReview, "review-task.md"),
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

	paths, err := filepath.Glob(filepath.Join(tasksDir, "messages", "events", "*.json"))
	if err != nil {
		t.Fatalf("filepath.Glob(events): %v", err)
	}
	sort.Strings(paths)
	if len(paths) != 1 {
		t.Fatalf("event file count = %d, want 1", len(paths))
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", paths[0], err)
	}
	content := string(raw)
	for _, want := range []string{
		`"from":"test-reviewer-1"`,
		`"type":"progress"`,
		`"task":"review-task.md"`,
		`"branch":"task/review-task"`,
		`"body":"Step: VERIFY_REVIEW"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("raw event file missing %q:\n%s", want, content)
		}
	}

	// Task file must remain untouched in ready-for-review/.
	mustExist(t, filepath.Join(tasksDir, dirs.ReadyReview, "review-task.md"))
}

// TestReviewEmptyDiffRejects runs the real VERIFY_REVIEW, DIFF, and VERDICT
// blocks when the task branch has no changes relative to the target branch.
// The DIFF decision table says to reject with "branch contains no changes".
func TestReviewEmptyDiffRejects(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Create a task branch that is identical to mato (no diff).
	mustGitOutput(t, repoRoot, "branch", "task/empty-diff", "mato")

	writeTask(t, tasksDir, dirs.ReadyReview, "empty-diff.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/empty-diff -->\n"+
			"# Empty Diff Task\nShould be rejected.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, dirs.ReadyReview, "empty-diff.md"),
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
	mustExist(t, filepath.Join(tasksDir, dirs.ReadyReview, "empty-diff.md"))
}

func TestReviewDiff_FetchesFromRewrittenOriginPath(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	targetBranch, err := git.Output(repoRoot, "branch", "--show-current")
	if err != nil {
		t.Fatalf("git branch --show-current: %v", err)
	}
	targetBranch = strings.TrimSpace(targetBranch)

	taskFile := "review-fetch.md"
	writeTask(t, tasksDir, dirs.ReadyReview, taskFile, strings.Join([]string{
		"<!-- branch: task/review-fetch -->",
		"# Review Fetch",
		"",
	}, "\n"))

	if _, err := git.Output(repoRoot, "checkout", "-b", "task/review-fetch"); err != nil {
		t.Fatalf("git checkout -b task/review-fetch: %v", err)
	}
	testutil.WriteFile(t, filepath.Join(repoRoot, "fetch.txt"), "fetched through rewritten origin\n")
	if _, err := git.Output(repoRoot, "add", "--", "fetch.txt"); err != nil {
		t.Fatalf("git add fetch.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "add fetch file"); err != nil {
		t.Fatalf("git commit fetch.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", targetBranch); err != nil {
		t.Fatalf("git checkout %s: %v", targetBranch, err)
	}

	mountedRepo := filepath.Join(t.TempDir(), "mounted-origin.git")
	if _, err := git.Output("", "clone", "--quiet", "--bare", repoRoot, mountedRepo); err != nil {
		t.Fatalf("git clone --bare: %v", err)
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)
	if _, err := git.Output(cloneDir, "remote", "set-url", "origin", mountedRepo); err != nil {
		t.Fatalf("git remote set-url origin: %v", err)
	}

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-review-fetch.md.json")
	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
		reviewStateBlock(t, "DIFF"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, targetBranch)

	env := []string{
		"MATO_AGENT_ID=test-reviewer-fetch",
		"MATO_TASK_FILE=" + taskFile,
		"MATO_TASK_BRANCH=task/review-fetch",
		"MATO_TASK_TITLE=Review Fetch",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, dirs.ReadyReview, taskFile),
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash rewritten origin diff: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "fetch.txt") {
		t.Fatalf("expected rewritten-origin diff output to include fetch.txt, got:\n%s", out)
	}
}

// TestReviewFetchFailureWritesErrorVerdict verifies that when git fetch fails
// for the task branch, the real DIFF block sets FAIL_STEP/FAIL_REASON and the
// ON_FAILURE block writes a verdict file with {"verdict":"error",...}.
func TestReviewFetchFailureWritesErrorVerdict(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.ReadyReview, "fetch-fail.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/nonexistent-branch -->\n"+
			"# Fetch Fail Task\nBranch does not exist.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, dirs.ReadyReview, "fetch-fail.md"),
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
	mustExist(t, filepath.Join(tasksDir, dirs.ReadyReview, "fetch-fail.md"))
}

// TestReviewRejectReasonWithSpecialChars verifies that rejection reasons
// containing pre-escaped double quotes produce valid JSON in the verdict
// file when written through the VERDICT reject block's simple printf approach.
// For reasons containing raw special characters (unescaped quotes, newlines),
// the prompt instructs agents to use their file-writing tools instead of bash.
func TestReviewRejectReasonWithSpecialChars(t *testing.T) {
	tests := []struct {
		name   string
		reason string // text substituted into the VERDICT reject block placeholder
		check  func(t *testing.T, v reviewVerdict)
	}{
		{
			name:   "pre-escaped double quotes",
			reason: `missing error wrapping in \"postAgentPush\" — the function does not call fmt.Errorf with %w`,
			check: func(t *testing.T, v reviewVerdict) {
				t.Helper()
				if !strings.Contains(v.Reason, "postAgentPush") {
					t.Fatalf("reason = %q, want it to contain 'postAgentPush'", v.Reason)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

			writeTask(t, tasksDir, dirs.ReadyReview, "special-chars.md",
				"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
					"<!-- branch: task/special-chars -->\n"+
					"# Special Chars Task\nTest special characters.\n")

			cloneDir := createPromptClone(t, repoRoot, tasksDir)
			cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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

// TestReviewSpecialCharGuidanceInPrompt verifies that the review instructions
// tell agents to use their file-writing tools for rejection reasons that
// contain raw special characters (double quotes, backslashes, newlines)
// rather than relying on shell-based JSON escaping.
func TestReviewSpecialCharGuidanceInPrompt(t *testing.T) {
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}
	text := string(data)

	if !strings.Contains(text, "file-writing tool") {
		t.Fatal("review instructions should tell agents to use file-writing tools for JSON verdict creation")
	}
	if !strings.Contains(text, "special characters") {
		t.Fatal("review instructions should mention handling special characters in rejection reasons")
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

// TestReviewMessageBudgetMatchesRuntime verifies that the review prompt
// accurately describes the message budget: no intent message, one agent
// progress, one host completion. This prevents the prompt from drifting out
// of sync with the runtime behavior in review.go.
func TestReviewMessageBudgetMatchesRuntime(t *testing.T) {
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}
	text := string(data)

	// The prompt must not tell the review agent that the host sends an intent message.
	// Runtime (review.go) does not send intent for reviews.
	forbidden := []struct {
		pattern string
		reason  string
	}{
		{"host sends the `intent`", "review prompt must not claim host sends intent — runtime does not send intent for reviews"},
		{"host intent", "review prompt must not reference 'host intent' — no intent message is sent for reviews"},
	}
	for _, f := range forbidden {
		if strings.Contains(text, f.pattern) {
			t.Fatalf("review instructions contain %q: %s", f.pattern, f.reason)
		}
	}

	// The prompt must mention that the host sends completion.
	if !strings.Contains(text, "completion") {
		t.Fatal("review instructions must mention 'completion' — host sends completion after processing verdict")
	}

	// The prompt must describe the budget as 2 total messages.
	if !strings.Contains(text, "2 total messages") {
		t.Fatal("review instructions must describe '2 total messages' budget (1 agent progress + 1 host completion)")
	}
}

// TestReviewVerdictApproveFormat verifies that the real VERDICT state approve
// block produces a well-formed JSON verdict file.
func TestReviewVerdictApproveFormat(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-missing.md.json")
	missingPath := filepath.Join(cloneTasksDir, dirs.ReadyReview, "missing-task.md")

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

// TestReviewProgressMessageFilenameIsDistinct verifies that the review agent's
// VERIFY_REVIEW progress message uses a state-specific filename suffix
// (verify-review) rather than a generic one, ensuring it won't collide with
// task-agent progress messages written by the same agent ID.
func TestReviewProgressMessageFilenameIsDistinct(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.ReadyReview, "review-progress.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/review-progress -->\n"+
			"# Review Progress Test\nTest review progress message.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-review-progress.md.json")

	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-reviewer-progress",
		"MATO_TASK_FILE=review-progress.md",
		"MATO_TASK_BRANCH=task/review-progress",
		"MATO_TASK_TITLE=Review Progress Test",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, dirs.ReadyReview, "review-progress.md"),
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash review progress: %v\noutput:\n%s", err, out)
	}

	msgs := readPromptEventMessages(t, tasksDir)
	found := false
	for _, msg := range msgs {
		if msg.Type == "progress" && msg.From == "test-reviewer-progress" {
			found = true
			if !strings.Contains(msg.Body, "VERIFY_REVIEW") {
				t.Fatalf("progress body = %q, want VERIFY_REVIEW", msg.Body)
			}
			// The MSG_ID should contain "verify-review", not generic "progress".
			if !strings.Contains(msg.ID, "verify-review") {
				t.Fatalf("progress message ID = %q, want it to contain 'verify-review'", msg.ID)
			}
		}
	}
	if !found {
		t.Fatal("no progress message found from test-reviewer-progress")
	}
}

// TestReviewSandboxSafeShellPatterns verifies that bash blocks in the review
// instructions do not use shell constructs likely to be blocked by execution
// sandboxes: command substitution, heredocs with variable interpolation,
// ${VAR:?} parameter expansions, shell escaping pipelines, and
// process-management commands.
func TestReviewSandboxSafeShellPatterns(t *testing.T) {
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}

	blocks := extractReviewBashBlocks(t, string(data))
	if len(blocks) == 0 {
		t.Fatal("no bash blocks found in review instructions")
	}

	for i, block := range blocks {
		// No command substitution — sandbox blocks $(...)
		if strings.Contains(block, "$(") {
			t.Fatalf("bash block %d contains command substitution $(...):\n%s", i, block)
		}

		// No ${VAR:?message} parameter expansions.
		if strings.Contains(block, ":?") {
			t.Fatalf("bash block %d contains ${VAR:?...} parameter expansion:\n%s", i, block)
		}

		// No heredocs with variable interpolation.
		if strings.Contains(block, "<< EOF") || strings.Contains(block, "<<EOF") ||
			strings.Contains(block, "<<VERDICTEOF") {
			t.Fatalf("bash block %d contains heredoc with interpolation:\n%s", i, block)
		}

		// No shell-based JSON escaping pipelines — agents should use their
		// file-writing tools for JSON encoding instead.
		if strings.Contains(block, "ESCAPED_REASON") || strings.Contains(block, "ESCAPED_ERROR") {
			t.Fatalf("bash block %d contains shell-based JSON escaping pipeline:\n%s", i, block)
		}

		// No process-management commands.
		for _, cmd := range []string{"kill ", "pkill ", "killall "} {
			if strings.Contains(block, cmd) {
				t.Fatalf("bash block %d contains process-management command %q:\n%s", i, cmd, block)
			}
		}
	}
}

// TestReviewSandboxSafeInvariants verifies the review instructions include
// guidance telling agents to avoid sandbox-risky patterns.
func TestReviewSandboxSafeInvariants(t *testing.T) {
	data, err := os.ReadFile(reviewInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(review instructions): %v", err)
	}
	text := string(data)

	required := []struct {
		substring string
		reason    string
	}{
		{"process-management cleanup commands", "should warn agents not to invent kill/pkill commands"},
		{"Do not collapse multiple state blocks", "should warn agents not to flatten steps into one shell command"},
		{"Avoid command substitution", "should explicitly ban $(...)"},
		{"file-writing tool", "should recommend agent file-writing tools for verdict JSON"},
	}
	for _, r := range required {
		if !strings.Contains(text, r.substring) {
			t.Fatalf("review instructions missing %q: %s", r.substring, r.reason)
		}
	}
}

// extractReviewBashBlocks returns all ```bash ... ``` blocks from a markdown document.
func extractReviewBashBlocks(t *testing.T, text string) []string {
	t.Helper()
	var blocks []string
	remaining := text
	for {
		start := strings.Index(remaining, "```bash\n")
		if start < 0 {
			break
		}
		remaining = remaining[start+len("```bash\n"):]
		end := strings.Index(remaining, "\n```")
		if end < 0 {
			t.Fatal("unterminated bash block in prompt")
		}
		blocks = append(blocks, remaining[:end])
		remaining = remaining[end+len("\n```"):]
	}
	return blocks
}

// TestReviewProgressMessagesAccumulateAcrossRuns verifies that two separate
// review runs from the same agent ID produce distinct progress message files
// that do not overwrite each other, even when both runs happen in the exact
// same second. This is a regression test for the append-only messaging invariant.
func TestReviewProgressMessagesAccumulateAcrossRuns(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.ReadyReview, "review-accum.md",
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/review-accum -->\n"+
			"# Review Accumulate Test\nTest that review progress messages accumulate.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	verdictPath := filepath.Join(cloneTasksDir, "messages", "verdict-review-accum.md.json")
	taskPath := filepath.Join(cloneTasksDir, dirs.ReadyReview, "review-accum.md")

	script := strings.Join([]string{
		reviewPreamble(t),
		reviewStateBlock(t, "VERIFY_REVIEW"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	// Create a fake date command that always returns a pinned timestamp for
	// the +%Y%m%dT%H%M%SZ format so both runs produce the same MATO_TS.
	fakeDateDir := t.TempDir()
	fakeDate := filepath.Join(fakeDateDir, "date")
	if err := os.WriteFile(fakeDate, []byte(
		"#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in +%Y%m%dT%H%M%SZ) echo 20260101T000000Z; exit 0 ;; esac; done\n/usr/bin/date \"$@\"\n",
	), 0o755); err != nil {
		t.Fatalf("write fake date: %v", err)
	}

	env := []string{
		"PATH=" + fakeDateDir + ":" + os.Getenv("PATH"),
		"MATO_AGENT_ID=same-reviewer",
		"MATO_TASK_FILE=review-accum.md",
		"MATO_TASK_BRANCH=task/review-accum",
		"MATO_TASK_TITLE=Review Accumulate Test",
		"MATO_TASK_PATH=" + taskPath,
		"MATO_REVIEW_VERDICT_PATH=" + verdictPath,
	}

	// Run #1.
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash run1: %v\noutput:\n%s", err, out)
	}

	msgs1 := readPromptEventMessages(t, tasksDir)
	var count1 int
	for _, msg := range msgs1 {
		if msg.Type == "progress" && msg.From == "same-reviewer" &&
			strings.Contains(msg.Body, "VERIFY_REVIEW") {
			count1++
		}
	}
	if count1 != 1 {
		t.Fatalf("after run1: expected 1 VERIFY_REVIEW message, got %d", count1)
	}

	// Run #2 immediately with the same pinned timestamp. The PID-based nonce
	// in the filename guarantees a distinct file even in the same second.
	out2, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash run2: %v\noutput:\n%s", err, out2)
	}

	// After two runs, there should be 2 distinct VERIFY_REVIEW messages.
	msgs2 := readPromptEventMessages(t, tasksDir)
	var count2 int
	var collectedIDs []string
	filenames := make(map[string]bool)
	for _, msg := range msgs2 {
		if msg.Type == "progress" && msg.From == "same-reviewer" &&
			strings.Contains(msg.Body, "VERIFY_REVIEW") {
			count2++
			collectedIDs = append(collectedIDs, msg.ID)
		}
	}
	if count2 != 2 {
		t.Fatalf("after run2: expected 2 VERIFY_REVIEW messages (one per run), got %d", count2)
	}

	// Same-second runs from the same agent should produce unique IDs
	// (PID suffix ensures per-event uniqueness) while sharing the same
	// tie-break prefix (<ts>-<agent>-<state>).
	if collectedIDs[0] == collectedIDs[1] {
		t.Fatalf("same-second runs produced identical IDs %q; "+
			"IDs should be unique (PID suffix ensures per-event uniqueness)", collectedIDs[0])
	}

	// Verify the ID follows the <ts>-<agent>-<step>-<pid> format.
	for _, id := range collectedIDs {
		if !strings.HasPrefix(id, "20260101T000000Z-same-reviewer-verify-review-") {
			t.Fatalf("ID %q does not match expected format <ts>-<agent>-<step>-<pid>", id)
		}
	}

	// Both runs used the pinned timestamp; verify filenames are still distinct.
	paths, _ := filepath.Glob(filepath.Join(tasksDir, "messages", "events", "*same-reviewer*verify-review*.json"))
	for _, p := range paths {
		base := filepath.Base(p)
		if filenames[base] {
			t.Fatalf("duplicate filename across runs: %s", base)
		}
		filenames[base] = true
	}
	if len(filenames) != 2 {
		t.Fatalf("expected 2 distinct filenames, got %d: %v", len(filenames), filenames)
	}
}
