package integration_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/dirs"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/queueview"
	"mato/internal/runner"
	"mato/internal/testutil"
)

// writeVerdict writes a JSON verdict file at the standard path.
func writeVerdict(t *testing.T, tasksDir, taskFile string, verdict any) {
	t.Helper()
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatalf("json.Marshal verdict: %v", err)
	}
	path := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	testutil.WriteFile(t, path, string(data))
}

// verdictPath returns the path to the verdict file for a task.
func verdictPath(tasksDir, taskFile string) string {
	return filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
}

// findCompletionMessages returns all "completion" messages from the events directory.
func findCompletionMessages(t *testing.T, tasksDir string) []messaging.Message {
	t.Helper()
	msgs, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("messaging.ReadMessages: %v", err)
	}
	var completions []messaging.Message
	for _, m := range msgs {
		if m.Type == "completion" {
			completions = append(completions, m)
		}
	}
	return completions
}

func TestReviewLifecycle_Approved(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "approve-task.md"
	reviewPath := writeTask(t, tasksDir, dirs.ReadyReview, taskFile,
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Approve Task\nDo something.\n")

	writeVerdict(t, tasksDir, taskFile, map[string]string{"verdict": "approve"})

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/approve-task",
		Title:    "Approve Task",
		TaskPath: reviewPath,
	}

	runner.PostReviewAction(tasksDir, "review-host", task)

	// Task should be moved to ready-to-merge/.
	mustExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, taskFile))
	mustNotExist(t, reviewPath)

	// Task file should contain the approval marker.
	data := readFile(t, filepath.Join(tasksDir, dirs.ReadyMerge, taskFile))
	if !strings.Contains(data, "<!-- reviewed:") {
		t.Fatal("approval marker not written to task file")
	}
	if !strings.Contains(data, "approved") {
		t.Fatal("approval marker missing 'approved' text")
	}

	// A completion message should have been emitted.
	completions := findCompletionMessages(t, tasksDir)
	if len(completions) != 1 {
		t.Fatalf("expected 1 completion message, got %d", len(completions))
	}
	if completions[0].Task != taskFile {
		t.Fatalf("completion message task = %q, want %q", completions[0].Task, taskFile)
	}
	if !strings.Contains(completions[0].Body, "approved") {
		t.Fatalf("completion message body = %q, want approval text", completions[0].Body)
	}

	// Verdict file should be cleaned up.
	mustNotExist(t, verdictPath(tasksDir, taskFile))
}

func TestReviewLifecycle_Rejected(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "reject-task.md"
	reviewPath := writeTask(t, tasksDir, dirs.ReadyReview, taskFile,
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Reject Task\nDo something.\n")

	writeVerdict(t, tasksDir, taskFile, map[string]string{
		"verdict": "reject",
		"reason":  "missing error wrapping in handler",
	})

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/reject-task",
		Title:    "Reject Task",
		TaskPath: reviewPath,
	}

	runner.PostReviewAction(tasksDir, "review-host", task)

	// Task should be moved to backlog/.
	mustExist(t, filepath.Join(tasksDir, dirs.Backlog, taskFile))
	mustNotExist(t, reviewPath)

	// Task file should contain the rejection marker with reason.
	data := readFile(t, filepath.Join(tasksDir, dirs.Backlog, taskFile))
	if !strings.Contains(data, "<!-- review-rejection:") {
		t.Fatal("rejection marker not written to task file")
	}
	if !strings.Contains(data, "missing error wrapping") {
		t.Fatal("rejection reason not included in marker")
	}

	// A completion message should have been emitted.
	completions := findCompletionMessages(t, tasksDir)
	if len(completions) != 1 {
		t.Fatalf("expected 1 completion message, got %d", len(completions))
	}
	if !strings.Contains(completions[0].Body, "rejected") {
		t.Fatalf("completion message body = %q, want rejection text", completions[0].Body)
	}

	// Verdict file should be cleaned up.
	mustNotExist(t, verdictPath(tasksDir, taskFile))
}

func TestReviewLifecycle_ErrorVerdict(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "error-task.md"
	reviewPath := writeTask(t, tasksDir, dirs.ReadyReview, taskFile,
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Error Task\n")

	writeVerdict(t, tasksDir, taskFile, map[string]string{
		"verdict": "error",
		"reason":  "could not checkout branch",
	})

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/error-task",
		Title:    "Error Task",
		TaskPath: reviewPath,
	}

	runner.PostReviewAction(tasksDir, "review-host", task)

	// Task should stay in ready-for-review/.
	mustExist(t, reviewPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, taskFile))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, taskFile))

	// Task file should contain a review-failure marker.
	data := readFile(t, reviewPath)
	if !strings.Contains(data, "<!-- review-failure:") {
		t.Fatal("review-failure marker not written for error verdict")
	}
	if !strings.Contains(data, "could not checkout branch") {
		t.Fatal("error reason not included in review-failure marker")
	}

	// Verdict file should still be cleaned up.
	mustNotExist(t, verdictPath(tasksDir, taskFile))
}

func TestReviewLifecycle_MalformedJSON(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "malformed-task.md"
	reviewPath := writeTask(t, tasksDir, dirs.ReadyReview, taskFile,
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Malformed Task\n")

	// Write invalid JSON to the verdict file.
	vPath := verdictPath(tasksDir, taskFile)
	testutil.WriteFile(t, vPath, `{not valid json!!!`)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/malformed-task",
		Title:    "Malformed Task",
		TaskPath: reviewPath,
	}

	runner.PostReviewAction(tasksDir, "review-host", task)

	// Task should stay in ready-for-review/.
	mustExist(t, reviewPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, taskFile))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, taskFile))

	// Task file should contain a review-failure marker mentioning the parse error.
	data := readFile(t, reviewPath)
	if !strings.Contains(data, "<!-- review-failure:") {
		t.Fatal("review-failure marker not written for malformed verdict")
	}
	if !strings.Contains(data, "could not parse verdict") {
		t.Fatal("review-failure should mention parse error")
	}

	// Verdict file should be cleaned up.
	mustNotExist(t, vPath)
}

func TestReviewLifecycle_MissingVerdictFile(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "no-verdict-task.md"
	reviewPath := writeTask(t, tasksDir, dirs.ReadyReview, taskFile,
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# No Verdict Task\n")

	// No verdict file is written — simulates a review agent crash.

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/no-verdict-task",
		Title:    "No Verdict Task",
		TaskPath: reviewPath,
	}

	runner.PostReviewAction(tasksDir, "review-host", task)

	// Task should stay in ready-for-review/.
	mustExist(t, reviewPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, taskFile))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, taskFile))

	// Task file should contain a review-failure marker.
	data := readFile(t, reviewPath)
	if !strings.Contains(data, "<!-- review-failure:") {
		t.Fatal("review-failure marker not written when verdict file is missing")
	}
	if !strings.Contains(data, "exited without rendering a verdict") {
		t.Fatal("review-failure should note missing verdict")
	}

	// No completion messages should be emitted.
	completions := findCompletionMessages(t, tasksDir)
	if len(completions) != 0 {
		t.Fatalf("expected 0 completion messages for missing verdict, got %d", len(completions))
	}
}

func TestReviewLifecycle_MissingTaskBranch(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "no-branch-task.md"
	reviewPath := writeTask(t, tasksDir, dirs.ReadyReview, taskFile,
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/nonexistent-branch -->\n"+
			"# No Branch Task\n")

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/nonexistent-branch",
		Title:    "No Branch Task",
		TaskPath: reviewPath,
	}

	// Exercise the real host pre-check that runs inside pollLoop before
	// any review agent is launched.  The branch does not exist in the
	// test repo, so VerifyReviewBranch must return false.
	ok := runner.VerifyReviewBranch(repoRoot, tasksDir, task, "review-host")
	if ok {
		t.Fatal("VerifyReviewBranch returned true for a branch that does not exist")
	}

	// Task should stay in ready-for-review/ — no transition occurs.
	mustExist(t, reviewPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, taskFile))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, taskFile))

	// A review-failure record should be appended with the host-side reason.
	data := readFile(t, reviewPath)
	if !strings.Contains(data, "<!-- review-failure:") {
		t.Fatal("review-failure marker not written for missing task branch scenario")
	}
	if !strings.Contains(data, "not found in host repo") {
		t.Fatal("review-failure should mention that the branch was not found in host repo")
	}

	// No completion messages — the task was not transitioned.
	completions := findCompletionMessages(t, tasksDir)
	if len(completions) != 0 {
		t.Fatalf("expected 0 completion messages for missing branch, got %d", len(completions))
	}
}

func TestReviewLifecycle_MalformedTaskQuarantined(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// Write a malformed task file with unterminated frontmatter.
	malformedFile := "malformed-review.md"
	malformedPath := writeTask(t, tasksDir, dirs.ReadyReview, malformedFile,
		"---\npriority: [oops\n# Malformed\n")

	// Write a valid task file.
	goodFile := "good-review.md"
	goodPath := writeTask(t, tasksDir, dirs.ReadyReview, goodFile,
		"<!-- branch: task/good-review -->\n---\npriority: 10\nmax_retries: 3\n---\n# Good Review Task\n")

	// Use ReviewCandidates via SelectTaskForReview (exported).
	task := runner.SelectTaskForReview(tasksDir, nil)

	// The valid task should be selected.
	if task == nil {
		t.Fatal("expected a review candidate to be selected")
	}
	if task.Filename != goodFile {
		t.Fatalf("expected %q, got %q", goodFile, task.Filename)
	}

	// The malformed task should be quarantined to failed/.
	mustExist(t, filepath.Join(tasksDir, dirs.Failed, malformedFile))
	mustNotExist(t, malformedPath)

	// The valid task should still be in ready-for-review/.
	mustExist(t, goodPath)

	// The failed task should have a terminal-failure marker.
	data := readFile(t, filepath.Join(tasksDir, dirs.Failed, malformedFile))
	if !strings.Contains(data, "<!-- terminal-failure:") {
		t.Fatal("terminal-failure marker not written to malformed task")
	}
	if !strings.Contains(data, "unparseable frontmatter") {
		t.Fatal("terminal-failure should mention unparseable frontmatter")
	}
}

func TestReviewLifecycle_MalformedTaskQuarantined_Indexed(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// Write a malformed task file.
	malformedFile := "malformed-indexed.md"
	malformedPath := writeTask(t, tasksDir, dirs.ReadyReview, malformedFile,
		"---\npriority: [oops\n# Malformed\n")

	// Write a valid task file.
	goodFile := "good-indexed.md"
	goodPath := writeTask(t, tasksDir, dirs.ReadyReview, goodFile,
		"<!-- branch: task/good-indexed -->\n---\npriority: 10\nmax_retries: 3\n---\n# Good Indexed Task\n")

	idx := queueview.BuildIndex(tasksDir)

	task := runner.SelectTaskForReview(tasksDir, idx)

	if task == nil {
		t.Fatal("expected a review candidate to be selected")
	}
	if task.Filename != goodFile {
		t.Fatalf("expected %q, got %q", goodFile, task.Filename)
	}

	// Malformed task should be in failed/.
	mustExist(t, filepath.Join(tasksDir, dirs.Failed, malformedFile))
	mustNotExist(t, malformedPath)

	// Valid task should still be in ready-for-review/.
	mustExist(t, goodPath)

	// Terminal-failure marker should be present.
	data := readFile(t, filepath.Join(tasksDir, dirs.Failed, malformedFile))
	if !strings.Contains(data, "<!-- terminal-failure:") {
		t.Fatal("terminal-failure marker not written")
	}
}
