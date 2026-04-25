package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/merge"
	"github.com/ryansimmen/mato/internal/testutil"
)

// TestMergeMissingBranch_ExplicitMarker verifies that a ready-to-merge task
// whose explicit <!-- branch: ... --> marker points at a nonexistent branch
// is moved to failed/ (when retries are exhausted) with a merge-queue failure
// record.
func TestMergeMissingBranch_ExplicitMarker(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Record the HEAD of the target branch before the merge attempt.
	headBefore := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))

	// Create a task in ready-to-merge with an explicit branch marker pointing
	// at a branch that was never pushed. Use max_retries: 1 with one prior
	// failure so the next failure exhausts the budget → failed/.
	taskContent := strings.Join([]string{
		"<!-- branch: task/nonexistent-explicit -->",
		"---",
		"id: missing-explicit",
		"priority: 1",
		"max_retries: 1",
		"---",
		"<!-- failure: prior-agent at 2026-01-01T00:00:00Z step=WORK error=first-attempt -->",
		"# Missing explicit branch",
		"This task's branch was never pushed.",
	}, "\n")
	writeTask(t, tasksDir, dirs.ReadyMerge, "missing-explicit.md", taskContent)

	merged := merge.ProcessQueue(repoRoot, tasksDir, "mato")
	if merged != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0 (no tasks should merge)", merged)
	}

	// Task should be in failed/, not in ready-to-merge/ or backlog/.
	failedPath := filepath.Join(tasksDir, dirs.Failed, "missing-explicit.md")
	mustExist(t, failedPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, "missing-explicit.md"))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, "missing-explicit.md"))

	// The failure record should mention "merge-queue" and the missing branch.
	data := readFile(t, failedPath)
	if !strings.Contains(data, "<!-- failure: merge-queue") {
		t.Fatalf("failed task missing merge-queue failure record:\n%s", data)
	}
	if !strings.Contains(data, "task branch not pushed by agent") {
		t.Fatalf("failed task missing 'task branch not pushed by agent' text:\n%s", data)
	}

	// Target branch should be unchanged.
	headAfter := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))
	if headAfter != headBefore {
		t.Fatalf("target branch changed: %s → %s", headBefore, headAfter)
	}

	// No completion-detail file should have been written.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	entries, err := os.ReadDir(completionsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", completionsDir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "missing-explicit") {
			t.Fatalf("unexpected completion detail for missing-explicit: %s", e.Name())
		}
	}
}

// TestMergeMissingBranch_MissingMarker verifies that a ready-to-merge task with
// no explicit <!-- branch: ... --> marker is treated as a corrupted handoff and
// moved to failed/ when its retry budget is exhausted.
func TestMergeMissingBranch_MissingMarker(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	headBefore := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))

	// No explicit branch marker. Use max_retries: 1 with one prior failure.
	taskContent := strings.Join([]string{
		"---",
		"id: missing-derived",
		"priority: 1",
		"max_retries: 1",
		"---",
		"<!-- failure: prior-agent at 2026-01-01T00:00:00Z step=WORK error=first-attempt -->",
		"# Missing derived branch",
		"This task's branch was never pushed.",
	}, "\n")
	writeTask(t, tasksDir, dirs.ReadyMerge, "missing-derived.md", taskContent)

	merged := merge.ProcessQueue(repoRoot, tasksDir, "mato")
	if merged != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", merged)
	}

	failedPath := filepath.Join(tasksDir, dirs.Failed, "missing-derived.md")
	mustExist(t, failedPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, "missing-derived.md"))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, "missing-derived.md"))

	data := readFile(t, failedPath)
	if !strings.Contains(data, "<!-- failure: merge-queue") {
		t.Fatalf("failed task missing merge-queue failure record:\n%s", data)
	}
	if !strings.Contains(data, "missing required") || !strings.Contains(data, "after work handoff") {
		t.Fatalf("failed task missing required-marker text:\n%s", data)
	}

	headAfter := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))
	if headAfter != headBefore {
		t.Fatalf("target branch changed: %s → %s", headBefore, headAfter)
	}

	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	entries, err := os.ReadDir(completionsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", completionsDir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "missing-derived") {
			t.Fatalf("unexpected completion detail for missing-derived: %s", e.Name())
		}
	}
}

// TestMergeMissingBranch_InvalidMarker verifies that a ready-to-merge task with
// an invalid explicit <!-- branch: ... --> marker is treated as a corrupted
// handoff and moved to failed/ when its retry budget is exhausted.
func TestMergeMissingBranch_InvalidMarker(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	headBefore := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))

	taskContent := strings.Join([]string{
		"<!-- branch: --bad -->",
		"---",
		"id: invalid-marker",
		"priority: 1",
		"max_retries: 1",
		"---",
		"<!-- failure: prior-agent at 2026-01-01T00:00:00Z step=WORK error=first-attempt -->",
		"# Invalid branch marker",
		"This task's branch marker is invalid.",
	}, "\n")
	writeTask(t, tasksDir, dirs.ReadyMerge, "invalid-marker.md", taskContent)

	merged := merge.ProcessQueue(repoRoot, tasksDir, "mato")
	if merged != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", merged)
	}

	failedPath := filepath.Join(tasksDir, dirs.Failed, "invalid-marker.md")
	mustExist(t, failedPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, "invalid-marker.md"))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Backlog, "invalid-marker.md"))

	data := readFile(t, failedPath)
	if !strings.Contains(data, "<!-- failure: merge-queue") {
		t.Fatalf("failed task missing merge-queue failure record:\n%s", data)
	}
	if !strings.Contains(data, "invalid required") || !strings.Contains(data, "after work handoff") {
		t.Fatalf("failed task missing invalid-marker text:\n%s", data)
	}
	if !strings.Contains(data, "invalid branch name") || !strings.Contains(data, "branch names must not begin with '-'") {
		t.Fatalf("failed task missing invalid branch detail:\n%s", data)
	}

	headAfter := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))
	if headAfter != headBefore {
		t.Fatalf("target branch changed: %s → %s", headBefore, headAfter)
	}
}

// TestMergeMissingBranch_RetriesRemaining verifies that when a missing-branch
// failure occurs but retries remain, the task moves to backlog/ (not failed/).
func TestMergeMissingBranch_RetriesRemaining(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	headBefore := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))

	// Default max_retries is 3, no prior failures → goes to backlog/.
	taskContent := strings.Join([]string{
		"<!-- branch: task/retryable-missing -->",
		"---",
		"id: retryable-missing",
		"priority: 1",
		"---",
		"# Retryable missing branch",
	}, "\n")
	writeTask(t, tasksDir, dirs.ReadyMerge, "retryable-missing.md", taskContent)

	merged := merge.ProcessQueue(repoRoot, tasksDir, "mato")
	if merged != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", merged)
	}

	// With retries remaining, the task should go to backlog/ for retry.
	backlogPath := filepath.Join(tasksDir, dirs.Backlog, "retryable-missing.md")
	mustExist(t, backlogPath)
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, "retryable-missing.md"))
	mustNotExist(t, filepath.Join(tasksDir, dirs.Failed, "retryable-missing.md"))

	data := readFile(t, backlogPath)
	if !strings.Contains(data, "<!-- failure: merge-queue") {
		t.Fatalf("backlog task missing merge-queue failure record:\n%s", data)
	}
	if !strings.Contains(data, "task branch not pushed by agent") {
		t.Fatalf("backlog task missing 'task branch not pushed by agent' text:\n%s", data)
	}

	headAfter := strings.TrimSpace(mustGitOutput(t, repoRoot, "rev-parse", "mato"))
	if headAfter != headBefore {
		t.Fatalf("target branch changed: %s → %s", headBefore, headAfter)
	}

	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	entries, err := os.ReadDir(completionsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", completionsDir, err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "retryable-missing") {
			t.Fatalf("unexpected completion detail for retryable-missing: %s", e.Name())
		}
	}
}

// TestMergeMissingBranch_SuccessfulTaskUnaffected verifies that a successful
// task still merges correctly even when another task in the same queue has a
// missing branch.
func TestMergeMissingBranch_SuccessfulTaskUnaffected(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Create a real task branch for the good task.
	createTaskBranch(t, repoRoot, "task/good-task",
		map[string]string{"good.txt": "good content\n"},
		"add good file")

	// Good task — branch exists.
	writeTask(t, tasksDir, dirs.ReadyMerge, "good-task.md",
		"<!-- branch: task/good-task -->\n---\npriority: 1\n---\n# Good task\n")

	// Bad task — branch missing, retries exhausted.
	badContent := strings.Join([]string{
		"<!-- branch: task/nonexistent -->",
		"---",
		"priority: 10",
		"max_retries: 1",
		"---",
		"<!-- failure: prior at 2026-01-01T00:00:00Z step=WORK error=prior -->",
		"# Bad task",
	}, "\n")
	writeTask(t, tasksDir, dirs.ReadyMerge, "bad-task.md", badContent)

	merged := merge.ProcessQueue(repoRoot, tasksDir, "mato")
	if merged != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1 (only good task)", merged)
	}

	// Good task completed, bad task failed.
	mustExist(t, filepath.Join(tasksDir, dirs.Completed, "good-task.md"))
	mustExist(t, filepath.Join(tasksDir, dirs.Failed, "bad-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, "good-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, dirs.ReadyMerge, "bad-task.md"))

	// The good task's content should be on the target branch.
	good, err := git.Output(repoRoot, "show", "mato:good.txt")
	if err != nil {
		t.Fatalf("git show mato:good.txt: %v", err)
	}
	if strings.TrimSpace(good) != "good content" {
		t.Fatalf("good.txt = %q, want %q", strings.TrimSpace(good), "good content")
	}
}
