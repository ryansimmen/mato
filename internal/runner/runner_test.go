package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mato/internal/messaging"
	"mato/internal/process"
	"mato/internal/queue"
)

func TestRecoverStuckTask_MovesToBacklog(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	inProgressPath := filepath.Join(tasksDir, "in-progress", taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Example Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}

	recoverStuckTask(tasksDir, "agent1", claimed)

	// Task should no longer be in in-progress/
	if _, err := os.Stat(inProgressPath); err == nil {
		t.Fatal("task file still in in-progress/ after recovery")
	}

	// Task should be in backlog/ with a failure record
	backlogPath := filepath.Join(tasksDir, "backlog", taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task file not found in backlog/: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: agent1") {
		t.Fatal("failure record not appended to recovered task")
	}
	if !strings.Contains(string(data), "agent container exited without cleanup") {
		t.Fatal("failure record missing expected message")
	}
}

func TestRecoverStuckTask_NoopWhenTaskAlreadyMoved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", "ready-to-merge"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	// Task already moved to ready-to-merge by the agent (success case)
	readyPath := filepath.Join(tasksDir, "ready-to-merge", taskFile)
	os.WriteFile(readyPath, []byte("# Example Task\n"), 0o644)

	inProgressPath := filepath.Join(tasksDir, "in-progress", taskFile)
	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}

	// Should be a no-op since the task is not in in-progress/
	recoverStuckTask(tasksDir, "agent1", claimed)

	// backlog/ should remain empty
	entries, _ := os.ReadDir(filepath.Join(tasksDir, "backlog"))
	if len(entries) != 0 {
		t.Fatalf("expected backlog/ to be empty, got %d entries", len(entries))
	}

	// ready-to-merge/ should still have the file
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatal("task file disappeared from ready-to-merge/")
	}
}

func TestRecoverStuckTask_BacklogCollision(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	inProgressPath := filepath.Join(tasksDir, "in-progress", taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Example Task\n"), 0o644)

	// A file with the same name already exists in backlog/
	backlogPath := filepath.Join(tasksDir, "backlog", taskFile)
	os.WriteFile(backlogPath, []byte("# Existing Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}

	// Recovery should refuse to overwrite the existing backlog file
	recoverStuckTask(tasksDir, "agent1", claimed)

	// The existing backlog file must be untouched
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("backlog file should still exist: %v", err)
	}
	if string(data) != "# Existing Task\n" {
		t.Fatalf("backlog file was overwritten, got: %s", string(data))
	}

	// The in-progress file should remain since recovery was skipped
	if _, err := os.Stat(inProgressPath); err != nil {
		t.Fatal("in-progress file should remain when backlog destination exists")
	}
}

func TestHasModelArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no model flag", args: []string{"--autopilot"}, want: false},
		{name: "model with value", args: []string{"--model", "gpt-5"}, want: true},
		{name: "model equals syntax", args: []string{"--model=gpt-5"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasModelArg(tt.args); got != tt.want {
				t.Fatalf("hasModelArg(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestWriteDependencyContextFile_WithCompletionFiles(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a task that depends on "dep-a" and "dep-b"
	taskFile := "my-task.md"
	taskPath := filepath.Join(tasksDir, "in-progress", taskFile)
	taskContent := "---\ndepends_on:\n  - dep-a\n  - dep-b\n---\n# My Task\n"
	os.WriteFile(taskPath, []byte(taskContent), 0o644)

	// Write completion detail for dep-a only
	if err := messaging.WriteCompletionDetail(tasksDir, messaging.CompletionDetail{
		TaskID:       "dep-a",
		TaskFile:     "dep-a.md",
		Branch:       "task/dep-a",
		CommitSHA:    "sha-a",
		FilesChanged: []string{"a.go"},
		Title:        "Dep A",
	}); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/my-task",
		Title:    "My Task",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result == "" {
		t.Fatal("expected non-empty file path")
	}

	expectedPath := filepath.Join(tasksDir, "messages", "dependency-context-"+taskFile+".json")
	if result != expectedPath {
		t.Fatalf("path = %q, want %q", result, expectedPath)
	}

	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var details []messaging.CompletionDetail
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("expected 1 dependency detail, got %d", len(details))
	}
	if details[0].TaskID != "dep-a" {
		t.Fatalf("TaskID = %q, want %q", details[0].TaskID, "dep-a")
	}
	if details[0].CommitSHA != "sha-a" {
		t.Fatalf("CommitSHA = %q, want %q", details[0].CommitSHA, "sha-a")
	}
}

func TestWriteDependencyContextFile_NoDeps(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "no-deps.md"
	taskPath := filepath.Join(tasksDir, "in-progress", taskFile)
	taskContent := "---\npriority: 5\n---\n# No deps\n"
	os.WriteFile(taskPath, []byte(taskContent), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/no-deps",
		Title:    "No deps",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty path, got %q", result)
	}
}

func TestWriteDependencyContextFile_NoCompletionFiles(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "has-deps.md"
	taskPath := filepath.Join(tasksDir, "in-progress", taskFile)
	taskContent := "---\ndepends_on:\n  - missing-dep\n---\n# Has deps\n"
	os.WriteFile(taskPath, []byte(taskContent), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/has-deps",
		Title:    "Has deps",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty path when no completion files exist, got %q", result)
	}
}

func TestRemoveDependencyContextFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	filename := "my-task.md"
	depCtxPath := filepath.Join(tasksDir, "messages", "dependency-context-"+filename+".json")
	os.WriteFile(depCtxPath, []byte(`[{"task_id":"dep-a"}]`), 0o644)

	if _, err := os.Stat(depCtxPath); err != nil {
		t.Fatalf("file should exist before removal: %v", err)
	}

	removeDependencyContextFile(tasksDir, filename)

	if _, err := os.Stat(depCtxPath); !os.IsNotExist(err) {
		t.Fatalf("file should have been removed, got err: %v", err)
	}
}

func TestRemoveDependencyContextFile_MissingFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Should not panic or error when file doesn't exist
	removeDependencyContextFile(tasksDir, "nonexistent.md")
}

func TestConfigureReceiveDeny_Success(t *testing.T) {
	// Create a real git repo to test the config call
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	if err := configureReceiveDeny(repoDir); err != nil {
		t.Fatalf("configureReceiveDeny should succeed on a valid repo: %v", err)
	}

	// Verify the config was set
	cmd = exec.Command("git", "-C", repoDir, "config", "receive.denyCurrentBranch")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reading config: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "updateInstead" {
		t.Fatalf("receive.denyCurrentBranch = %q, want %q", got, "updateInstead")
	}
}

func TestConfigureReceiveDeny_Failure(t *testing.T) {
	// Point at a nonexistent directory so git config fails
	err := configureReceiveDeny(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Fatal("configureReceiveDeny should fail for a nonexistent repo")
	}
}

func TestCheckIdleTransition(t *testing.T) {
	tests := []struct {
		name        string
		sequence    []bool // sequence of isIdle values per iteration
		wantPrints  []bool // expected shouldPrint results per iteration
	}{
		{
			name:       "prints once on entering idle",
			sequence:   []bool{true, true, true},
			wantPrints: []bool{true, false, false},
		},
		{
			name:       "prints again after leaving and re-entering idle",
			sequence:   []bool{true, true, false, true, true},
			wantPrints: []bool{true, false, false, true, false},
		},
		{
			name:       "never prints when always active",
			sequence:   []bool{false, false, false},
			wantPrints: []bool{false, false, false},
		},
		{
			name:       "alternating prints each time",
			sequence:   []bool{true, false, true, false},
			wantPrints: []bool{true, false, true, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wasIdle := false
			for i, isIdle := range tt.sequence {
				got := checkIdleTransition(isIdle, &wasIdle)
				if got != tt.wantPrints[i] {
					t.Errorf("iteration %d: checkIdleTransition(%v) = %v, want %v",
						i, isIdle, got, tt.wantPrints[i])
				}
			}
		})
	}
}

func TestExtractFailureLines_NoFailures(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# My Task\nDo something.\n"), 0o644)

	got := extractFailureLines(f)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExtractFailureLines_SingleFailure(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\nDo something.\n<!-- failure: agent-7 at 2026-01-01T00:03:00Z step=WORK error=tests_failed files_changed=queue.go -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractFailureLines(f)
	if !strings.Contains(got, "step=WORK") {
		t.Fatalf("expected failure line with step=WORK, got %q", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("expected single line (no newlines), got %q", got)
	}
}

func TestExtractFailureLines_MultipleFailures(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed files_changed=main.go -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z step=COMMIT error=no_changes files_changed=none -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractFailureLines(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 failure lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "agent-1") {
		t.Fatalf("first line should contain agent-1, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "agent-2") {
		t.Fatalf("second line should contain agent-2, got %q", lines[1])
	}
}

func TestExtractFailureLines_IgnoresBodyText(t *testing.T) {
	content := "---\npriority: 5\n---\n# Retry budget\n`CountFailureLines()` counts `<!-- failure: ... -->` records.\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractFailureLines(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 failure line (ignoring body text), got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "agent-1") {
		t.Fatalf("expected agent-1 failure line, got %q", lines[0])
	}
}

func TestExtractFailureLines_NonexistentFile(t *testing.T) {
	got := extractFailureLines(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", got)
	}
}

func TestExtractReviewRejections_NoRejections(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# My Task\nDo something.\n"), 0o644)

	got := extractReviewRejections(f)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExtractReviewRejections_SingleRejection(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\nDo something.\n<!-- review-rejection: reviewer-1 at 2026-01-01T00:03:00Z reason=missing_tests -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractReviewRejections(f)
	if !strings.Contains(got, "reason=missing_tests") {
		t.Fatalf("expected rejection line with reason=missing_tests, got %q", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("expected single line (no newlines), got %q", got)
	}
}

func TestExtractReviewRejections_MultipleRejections(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\n<!-- review-rejection: reviewer-1 at 2026-01-01T00:01:00Z reason=missing_tests -->\n<!-- review-rejection: reviewer-2 at 2026-01-01T00:02:00Z reason=style_issues -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractReviewRejections(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 rejection lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "reviewer-1") {
		t.Fatalf("first line should contain reviewer-1, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "reviewer-2") {
		t.Fatalf("second line should contain reviewer-2, got %q", lines[1])
	}
}

func TestExtractReviewRejections_MixedRecords(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed files_changed=main.go -->\n<!-- review-rejection: reviewer-1 at 2026-01-01T00:02:00Z reason=missing_tests -->\n<!-- failure: agent-2 at 2026-01-01T00:03:00Z step=COMMIT error=no_changes files_changed=none -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractReviewRejections(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 rejection line, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "reviewer-1") {
		t.Fatalf("line should contain reviewer-1, got %q", lines[0])
	}
	if strings.Contains(got, "failure") {
		t.Fatalf("should not contain failure lines, got %q", got)
	}
}

func TestExtractReviewRejections_NonexistentFile(t *testing.T) {
	got := extractReviewRejections(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", got)
	}
}

func TestRecoverStuckTask_StillMovesWhenAppendFails(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "readonly-task.md"
	inProgressPath := filepath.Join(tasksDir, "in-progress", taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Read-only task\n"), 0o444)
	t.Cleanup(func() {
		os.Chmod(filepath.Join(tasksDir, "backlog", taskFile), 0o644)
	})

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/readonly-task",
		Title:    "Read-only task",
		TaskPath: inProgressPath,
	}

	recoverStuckTask(tasksDir, "agent1", claimed)

	// Task should be moved to backlog even though append will fail
	backlogPath := filepath.Join(tasksDir, "backlog", taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task should be moved to backlog even when append fails: %v", err)
	}

	// Original content preserved
	if !strings.Contains(string(data), "# Read-only task") {
		t.Fatal("task content should be preserved")
	}

	// Failure record should NOT be present since file was read-only
	if strings.Contains(string(data), "<!-- failure:") {
		t.Fatal("failure record should not be present when append fails on read-only file")
	}

	// Task should no longer be in in-progress
	if _, err := os.Stat(inProgressPath); err == nil {
		t.Fatal("task file still in in-progress/ after recovery")
	}
}

func TestSelectTaskForReview_EmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "ready-for-review"), 0o755)

	got := selectTaskForReview(tasksDir)
	if got != nil {
		t.Fatalf("expected nil for empty ready-for-review/, got %+v", got)
	}
}

func TestSelectTaskForReview_NonexistentDir(t *testing.T) {
	got := selectTaskForReview(filepath.Join(t.TempDir(), "nonexistent"))
	if got != nil {
		t.Fatalf("expected nil for nonexistent tasks dir, got %+v", got)
	}
}

func TestSelectTaskForReview_SingleTask(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)

	content := "<!-- claimed-by: abc12345  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: test-task\npriority: 10\n---\n# Test Task\nDo something.\n\n<!-- branch: task/test-task -->\n"
	os.WriteFile(filepath.Join(reviewDir, "test-task.md"), []byte(content), 0o644)

	got := selectTaskForReview(tasksDir)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Filename != "test-task.md" {
		t.Fatalf("Filename = %q, want %q", got.Filename, "test-task.md")
	}
	if got.Branch != "task/test-task" {
		t.Fatalf("Branch = %q, want %q", got.Branch, "task/test-task")
	}
	if got.Title != "Test Task" {
		t.Fatalf("Title = %q, want %q", got.Title, "Test Task")
	}
}

func TestSelectTaskForReview_HighestPriority(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)

	// Priority 20 — lower priority (higher number)
	os.WriteFile(filepath.Join(reviewDir, "low-pri.md"), []byte(
		"---\npriority: 20\n---\n# Low Priority\n<!-- branch: task/low-pri -->\n"), 0o644)

	// Priority 5 — highest priority (lowest number)
	os.WriteFile(filepath.Join(reviewDir, "high-pri.md"), []byte(
		"---\npriority: 5\n---\n# High Priority\n<!-- branch: task/high-pri -->\n"), 0o644)

	// Priority 10 — middle
	os.WriteFile(filepath.Join(reviewDir, "mid-pri.md"), []byte(
		"---\npriority: 10\n---\n# Mid Priority\n<!-- branch: task/mid-pri -->\n"), 0o644)

	got := selectTaskForReview(tasksDir)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Filename != "high-pri.md" {
		t.Fatalf("expected highest priority task, got %q", got.Filename)
	}
}

func TestSelectTaskForReview_SamePriorityAlphabetical(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "beta-task.md"), []byte(
		"---\npriority: 10\n---\n# Beta\n<!-- branch: task/beta -->\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "alpha-task.md"), []byte(
		"---\npriority: 10\n---\n# Alpha\n<!-- branch: task/alpha -->\n"), 0o644)

	got := selectTaskForReview(tasksDir)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Filename != "alpha-task.md" {
		t.Fatalf("expected alphabetically first task, got %q", got.Filename)
	}
}

func TestSelectTaskForReview_IgnoresNonMdFiles(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)

	// Non-.md files should be ignored
	os.WriteFile(filepath.Join(reviewDir, "notes.txt"), []byte("not a task"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, ".hidden"), []byte("hidden"), 0o644)
	os.MkdirAll(filepath.Join(reviewDir, "subdir"), 0o755)

	got := selectTaskForReview(tasksDir)
	if got != nil {
		t.Fatalf("expected nil when only non-.md files present, got %+v", got)
	}
}

func TestSelectTaskForReview_BranchFallback(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)

	// No branch comment — should fall back to task/<sanitized-name>
	os.WriteFile(filepath.Join(reviewDir, "my-task.md"), []byte(
		"---\npriority: 5\n---\n# My Task\nNo branch comment here.\n"), 0o644)

	got := selectTaskForReview(tasksDir)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Branch != "task/my-task" {
		t.Fatalf("Branch = %q, want %q (fallback)", got.Branch, "task/my-task")
	}
}

func TestParseBranchFromTaskFile_WithBranch(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# Task\n\n<!-- branch: task/foo-bar -->\n"), 0o644)

	got := parseBranchFromTaskFile(f)
	if got != "task/foo-bar" {
		t.Fatalf("got %q, want %q", got, "task/foo-bar")
	}
}

func TestParseBranchFromTaskFile_WithoutBranch(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# Task\nNo branch.\n"), 0o644)

	got := parseBranchFromTaskFile(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranchFromTaskFile_NonexistentFile(t *testing.T) {
	got := parseBranchFromTaskFile(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestFailedDirUnavailable_ExclusionLogic(t *testing.T) {
	// Simulate the runner loop's exclusion behavior: when SelectAndClaimTask
	// returns a FailedDirUnavailableError, the runner should add the task to
	// the deferred set so it is not re-selected on the next poll.

	excluded := make(map[string]struct{})

	// Simulate first error for task "exhausted.md"
	err1 := fmt.Errorf("wrap: %w", &queue.FailedDirUnavailableError{
		TaskFilename: "exhausted.md",
		MoveErr:      fmt.Errorf("permission denied"),
	})
	var fdErr *queue.FailedDirUnavailableError
	if !errors.As(err1, &fdErr) {
		t.Fatal("errors.As should match FailedDirUnavailableError")
	}
	excluded[fdErr.TaskFilename] = struct{}{}

	if _, ok := excluded["exhausted.md"]; !ok {
		t.Fatal("exhausted.md should be in exclusion set")
	}

	// Simulate second error for a different task
	err2 := &queue.FailedDirUnavailableError{
		TaskFilename: "another-exhausted.md",
		MoveErr:      fmt.Errorf("no space"),
	}
	if !errors.As(err2, &fdErr) {
		t.Fatal("errors.As should match direct FailedDirUnavailableError")
	}
	excluded[fdErr.TaskFilename] = struct{}{}

	// Both should be excluded
	if len(excluded) != 2 {
		t.Fatalf("expected 2 excluded tasks, got %d", len(excluded))
	}

	// Merging into deferred should work
	deferred := map[string]struct{}{"overlap-task.md": {}}
	for name := range excluded {
		deferred[name] = struct{}{}
	}
	if len(deferred) != 3 {
		t.Fatalf("expected 3 deferred tasks after merge, got %d", len(deferred))
	}
	for _, want := range []string{"exhausted.md", "another-exhausted.md", "overlap-task.md"} {
		if _, ok := deferred[want]; !ok {
			t.Fatalf("%s should be in merged deferred set", want)
		}
	}

	// A non-FailedDirUnavailableError should NOT match
	plainErr := fmt.Errorf("unrelated claim error")
	if errors.As(plainErr, &fdErr) {
		t.Fatal("plain error should not match FailedDirUnavailableError")
	}
}

func TestFailedDirUnavailable_IsPredicateFromRunner(t *testing.T) {
	err := &queue.FailedDirUnavailableError{
		TaskFilename: "test.md",
		MoveErr:      fmt.Errorf("disk full"),
	}
	if !queue.IsFailedDirUnavailable(err) {
		t.Fatal("IsFailedDirUnavailable should return true")
	}
	if queue.IsFailedDirUnavailable(fmt.Errorf("other")) {
		t.Fatal("IsFailedDirUnavailable should return false for unrelated errors")
	}
}

func TestSelectAndLockReview_AcquiresLock(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)

	os.WriteFile(filepath.Join(reviewDir, "task-a.md"), []byte(
		"---\npriority: 10\n---\n# Task A\n<!-- branch: task/task-a -->\n"), 0o644)

	task, cleanup := selectAndLockReview(tasksDir)
	if task == nil {
		t.Fatal("expected non-nil task")
	}
	if task.Filename != "task-a.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "task-a.md")
	}

	// Lock file should exist.
	lockFile := filepath.Join(tasksDir, ".locks", "review-task-a.md.lock")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}

	cleanup()
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove lock file")
	}
}

func TestSelectAndLockReview_SkipsLockedTask(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(locksDir, 0o755)

	// Two tasks: task-a (priority 5) and task-b (priority 10).
	os.WriteFile(filepath.Join(reviewDir, "task-a.md"), []byte(
		"---\npriority: 5\n---\n# Task A\n<!-- branch: task/task-a -->\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "task-b.md"), []byte(
		"---\npriority: 10\n---\n# Task B\n<!-- branch: task/task-b -->\n"), 0o644)

	// Lock task-a (highest priority) — simulates another agent holding it.
	// Use real lock identity so IsLockHolderAlive recognizes it as alive.
	os.WriteFile(filepath.Join(locksDir, "review-task-a.md.lock"),
		[]byte(process.LockIdentity(os.Getpid())), 0o644)

	task, cleanup := selectAndLockReview(tasksDir)
	if task == nil {
		t.Fatal("expected non-nil task (should fall back to task-b)")
	}
	if task.Filename != "task-b.md" {
		t.Fatalf("Filename = %q, want %q (should skip locked task-a)", task.Filename, "task-b.md")
	}
	cleanup()
}

func TestSelectAndLockReview_AllLocked(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(locksDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "task-a.md"), []byte(
		"---\npriority: 5\n---\n# Task A\n<!-- branch: task/task-a -->\n"), 0o644)

	// Lock it — simulates another agent holding it.
	// Use real lock identity so IsLockHolderAlive recognizes it as alive.
	os.WriteFile(filepath.Join(locksDir, "review-task-a.md.lock"),
		[]byte(process.LockIdentity(os.Getpid())), 0o644)

	task, cleanup := selectAndLockReview(tasksDir)
	if task != nil {
		cleanup()
		t.Fatal("expected nil when all tasks are locked")
	}
}

func TestSelectAndLockReview_EmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "ready-for-review"), 0o755)

	task, cleanup := selectAndLockReview(tasksDir)
	if task != nil {
		cleanup()
		t.Fatal("expected nil for empty ready-for-review/")
	}
}

func TestReviewCandidates_SortedByPriority(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	os.MkdirAll(reviewDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "low.md"), []byte(
		"---\npriority: 20\n---\n# Low\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "high.md"), []byte(
		"---\npriority: 5\n---\n# High\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "mid.md"), []byte(
		"---\npriority: 10\n---\n# Mid\n"), 0o644)

	candidates := reviewCandidates(tasksDir)
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	if candidates[0].Filename != "high.md" {
		t.Errorf("first candidate = %q, want %q", candidates[0].Filename, "high.md")
	}
	if candidates[1].Filename != "mid.md" {
		t.Errorf("second candidate = %q, want %q", candidates[1].Filename, "mid.md")
	}
	if candidates[2].Filename != "low.md" {
		t.Errorf("third candidate = %q, want %q", candidates[2].Filename, "low.md")
	}
}

func TestReviewCandidates_SkipsRetryExhausted(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	failedDir := filepath.Join(tasksDir, "failed")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 3 review-failures (default max_retries=3) — should be moved to failed/
	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"---",
		"priority: 5",
		"---",
		"# Exhausted Task",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- review-failure: three -->",
		"<!-- branch: task/exhausted -->",
		"",
	}, "\n")), 0o644)

	// Task with 1 review-failure — should still be a candidate
	os.WriteFile(filepath.Join(reviewDir, "healthy.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Healthy Task",
		"<!-- review-failure: one -->",
		"<!-- branch: task/healthy -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Filename != "healthy.md" {
		t.Fatalf("expected healthy.md, got %q", candidates[0].Filename)
	}

	// Exhausted task should be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "exhausted.md")); err != nil {
		t.Fatalf("exhausted task not moved to failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, "exhausted.md")); !os.IsNotExist(err) {
		t.Fatal("exhausted task still in ready-for-review")
	}
}

func TestReviewCandidates_CustomMaxRetries(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	failedDir := filepath.Join(tasksDir, "failed")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with max_retries=1 and 1 review-failure — should be exhausted
	os.WriteFile(filepath.Join(reviewDir, "custom.md"), []byte(strings.Join([]string{
		"---",
		"max_retries: 1",
		"priority: 5",
		"---",
		"# Custom Retries",
		"<!-- review-failure: one -->",
		"<!-- branch: task/custom -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir)
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates (custom max_retries=1 exhausted), got %d", len(candidates))
	}

	if _, err := os.Stat(filepath.Join(failedDir, "custom.md")); err != nil {
		t.Fatalf("custom.md not moved to failed: %v", err)
	}
}

func TestSelectTaskForReview_SkipsRetryExhausted(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	failedDir := filepath.Join(tasksDir, "failed")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Only task has exhausted review retries — selectTaskForReview should return nil
	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"# Exhausted",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- review-failure: three -->",
		"<!-- branch: task/exhausted -->",
		"",
	}, "\n")), 0o644)

	got := selectTaskForReview(tasksDir)
	if got != nil {
		t.Fatalf("expected nil (all tasks retry-exhausted), got %+v", got)
	}

	if _, err := os.Stat(filepath.Join(failedDir, "exhausted.md")); err != nil {
		t.Fatalf("exhausted task not in failed: %v", err)
	}
}

func TestReviewCandidates_BelowBudgetNotMoved(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	failedDir := filepath.Join(tasksDir, "failed")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 2 review-failures but default max_retries=3 — should remain a candidate
	os.WriteFile(filepath.Join(reviewDir, "still-ok.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Still OK",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- branch: task/still-ok -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Filename != "still-ok.md" {
		t.Fatalf("expected still-ok.md, got %q", candidates[0].Filename)
	}

	// Should NOT be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "still-ok.md")); !os.IsNotExist(err) {
		t.Fatal("task with retries remaining should not be in failed")
	}
}

func TestReviewCandidates_TaskFailuresIgnored(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	failedDir := filepath.Join(tasksDir, "failed")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 3 task-agent failures but 0 review-failures.
	// Task failures should NOT count toward review retry budget.
	os.WriteFile(filepath.Join(reviewDir, "task-fails-only.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Task Failures Only",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- failure: agent-2 at 2025-01-02T00:00:00Z step=WORK error=tests -->",
		"<!-- failure: agent-3 at 2025-01-03T00:00:00Z step=PUSH error=push -->",
		"<!-- branch: task/task-fails-only -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (task failures should not exhaust review budget), got %d", len(candidates))
	}
	if candidates[0].Filename != "task-fails-only.md" {
		t.Fatalf("expected task-fails-only.md, got %q", candidates[0].Filename)
	}

	// Should NOT be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "task-fails-only.md")); !os.IsNotExist(err) {
		t.Fatal("task with only task-agent failures should not be moved to failed by review budget check")
	}
}

func TestReviewCandidates_ReviewFailuresSeparateFromTaskFailures(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	failedDir := filepath.Join(tasksDir, "failed")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 2 task-agent failures and 2 review-failures (max_retries=3).
	// Only the 2 review-failures count toward review retry budget, so it should
	// remain a candidate (2 < 3).
	os.WriteFile(filepath.Join(reviewDir, "mixed.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Mixed Failures",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- failure: agent-2 at 2025-01-02T00:00:00Z step=WORK error=tests -->",
		"<!-- review-failure: rev-1 at 2025-01-03T00:00:00Z step=DIFF error=fetch -->",
		"<!-- review-failure: rev-2 at 2025-01-04T00:00:00Z step=DIFF error=timeout -->",
		"<!-- branch: task/mixed -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (only 2 review-failures < max_retries 3), got %d", len(candidates))
	}

	// Should NOT be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "mixed.md")); !os.IsNotExist(err) {
		t.Fatal("mixed-failure task should not be in failed (only 2 review-failures)")
	}
}

func TestRunOnce_TimeoutKillsCommand(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	cfg := dockerConfig{
		timeout: 100 * time.Millisecond,
	}

	// runOnceWithTimeout is not directly callable with a subprocess
	// instead we test the context timeout behavior directly
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "10")
	err := cmd.Run()

	if err == nil {
		t.Fatal("expected error from timed-out command, got nil")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
	}
}

func TestParseAgentTimeout(t *testing.T) {
	tests := []struct {
		name    string
		envVal  string
		want    time.Duration
		wantErr bool
	}{
		{
			name:   "default when unset",
			envVal: "",
			want:   defaultAgentTimeout,
		},
		{
			name:   "valid duration 1h",
			envVal: "1h",
			want:   time.Hour,
		},
		{
			name:   "valid duration 45m",
			envVal: "45m",
			want:   45 * time.Minute,
		},
		{
			name:   "valid duration 2h30m",
			envVal: "2h30m",
			want:   2*time.Hour + 30*time.Minute,
		},
		{
			name:    "invalid duration",
			envVal:  "notaduration",
			wantErr: true,
		},
		{
			name:    "negative duration",
			envVal:  "-5m",
			wantErr: true,
		},
		{
			name:    "zero duration",
			envVal:  "0s",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAgentTimeout(tt.envVal)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.envVal)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.envVal, err)
			}
			if got != tt.want {
				t.Fatalf("parseAgentTimeout(%q) = %v, want %v", tt.envVal, got, tt.want)
			}
		})
	}
}

func TestDefaultAgentTimeout(t *testing.T) {
	if defaultAgentTimeout != 30*time.Minute {
		t.Fatalf("expected default timeout of 30m, got %v", defaultAgentTimeout)
	}
}

func TestAgentWroteFailureRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")

	// No failure record: should return false.
	os.WriteFile(path, []byte("# Task\nSome content\n"), 0o644)
	if agentWroteFailureRecord(path, "abc12345") {
		t.Fatal("expected false when no failure record exists")
	}

	// Failure from a different agent: should return false.
	os.WriteFile(path, []byte("# Task\n<!-- failure: other-agent at 2026-01-01T00:00:00Z step=WORK error=test -->\n"), 0o644)
	if agentWroteFailureRecord(path, "abc12345") {
		t.Fatal("expected false for a different agent's failure record")
	}

	// Failure from the matching agent: should return true.
	os.WriteFile(path, []byte("# Task\n<!-- failure: abc12345 at 2026-01-01T00:00:00Z step=WORK error=test -->\n"), 0o644)
	if !agentWroteFailureRecord(path, "abc12345") {
		t.Fatal("expected true when agent's failure record exists")
	}

	// Nonexistent file: should return false.
	if agentWroteFailureRecord(filepath.Join(dir, "nonexistent.md"), "abc12345") {
		t.Fatal("expected false for nonexistent file")
	}
}

func TestRecoverStuckTask_SkipsDuplicateFailureRecord(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "my-task.md"
	inProgressPath := filepath.Join(tasksDir, "in-progress", taskFile)
	// Agent already wrote a failure record via ON_FAILURE.
	os.WriteFile(inProgressPath, []byte(strings.Join([]string{
		"<!-- claimed-by: agent-x -->\n# My Task",
		"<!-- failure: agent-x at 2026-01-01T00:00:00Z step=WORK error=tests_failed files_changed=foo.go -->",
		"",
	}, "\n")), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/my-task",
		Title:    "My Task",
		TaskPath: inProgressPath,
	}

	recoverStuckTask(tasksDir, "agent-x", claimed)

	backlogPath := filepath.Join(tasksDir, "backlog", taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task file not found in backlog/: %v", err)
	}

	// Should have exactly 1 failure record (the agent's), not 2.
	count := strings.Count(string(data), "<!-- failure:")
	if count != 1 {
		t.Fatalf("failure record count = %d, want 1 (no duplicate)\ncontents:\n%s", count, string(data))
	}
	if !strings.Contains(string(data), "step=WORK") {
		t.Fatal("original failure record should be preserved")
	}
}

func TestPostAgentPush_SkipsWhenReadyForReviewExists(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"in-progress", "ready-for-review", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	inProgressPath := filepath.Join(tasksDir, "in-progress", taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Example Task\n"), 0o644)

	// Place a stale file at the ready-for-review/ destination.
	staleContent := []byte("<!-- claimed-by: old-agent -->\n# Stale Task\n")
	readyPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(readyPath, staleContent, 0o644)

	// Set up a minimal git repo so postAgentPush can check for commits.
	cloneDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = cloneDir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.name", "test")
	run("git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	run("git", "add", ".")
	run("git", "commit", "-m", "init")
	// Create a second commit on a task branch so there are commits above main.
	run("git", "checkout", "-b", "task/example-task")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644)
	run("git", "add", ".")
	run("git", "commit", "-m", "task work")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}
	cfg := dockerConfig{
		tasksDir:     tasksDir,
		agentID:      "agent1",
		targetBranch: "main",
	}

	err := postAgentPush(cfg, claimed, cloneDir)

	// Should return an error indicating the destination exists.
	if err == nil {
		t.Fatal("expected error when ready-for-review/ destination already exists, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error should mention destination already exists, got: %v", err)
	}

	// The stale file should not have been overwritten.
	data, readErr := os.ReadFile(readyPath)
	if readErr != nil {
		t.Fatalf("stale file in ready-for-review/ was removed: %v", readErr)
	}
	if string(data) != string(staleContent) {
		t.Fatalf("stale file was overwritten; got %q, want %q", string(data), string(staleContent))
	}

	// The in-progress/ file should still be there (no move occurred).
	if _, err := os.Stat(inProgressPath); err != nil {
		t.Fatalf("in-progress/ task file should still exist: %v", err)
	}
}

func TestPostReviewAction_Approved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	// Write a verdict file (the new approach).
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"approve"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Should be moved to ready-to-merge/.
	if _, err := os.Stat(filepath.Join(tasksDir, "ready-to-merge", taskFile)); err != nil {
		t.Fatal("approved task not moved to ready-to-merge/")
	}
	if _, err := os.Stat(reviewPath); err == nil {
		t.Fatal("approved task still in ready-for-review/")
	}
	// Task file should have the approval marker written by the host.
	data, _ := os.ReadFile(filepath.Join(tasksDir, "ready-to-merge", taskFile))
	if !strings.Contains(string(data), "<!-- reviewed:") {
		t.Fatal("approval marker not written to task file")
	}
	// Verdict file should be cleaned up.
	if _, err := os.Stat(verdictPath); err == nil {
		t.Fatal("verdict file should be cleaned up after processing")
	}
}

func TestPostReviewAction_Rejected(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	// Write a rejection verdict file.
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":"missing error wrapping in postAgentPush"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Should be moved to backlog/.
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", taskFile)); err != nil {
		t.Fatal("rejected task not moved to backlog/")
	}
	if _, err := os.Stat(reviewPath); err == nil {
		t.Fatal("rejected task still in ready-for-review/")
	}
	// Task file should have the rejection marker with the reason.
	data, _ := os.ReadFile(filepath.Join(tasksDir, "backlog", taskFile))
	if !strings.Contains(string(data), "<!-- review-rejection:") {
		t.Fatal("rejection marker not written to task file")
	}
	if !strings.Contains(string(data), "missing error wrapping") {
		t.Fatal("rejection reason not included in marker")
	}
}

func TestPostReviewAction_NoVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Task should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task with no verdict should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure record not written for task with no verdict")
	}
	if !strings.Contains(string(data), "exited without rendering a verdict") {
		t.Fatal("review-failure record missing expected reason")
	}
}

func TestPostReviewAction_ErrorVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	// Write an error verdict file (review agent's ON_FAILURE).
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"error","reason":"DIFF: could not fetch branch"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Task should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("error verdict task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure record not written for error verdict")
	}
	if !strings.Contains(string(data), "could not fetch branch") {
		t.Fatal("review-failure reason should include the error details")
	}
	// Verdict file should be cleaned up.
	if _, err := os.Stat(verdictPath); err == nil {
		t.Fatal("verdict file should be cleaned up after processing")
	}
}

func TestPostReviewAction_UnknownVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"maybe","reason":"unsure"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("unknown verdict task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure not written for unknown verdict")
	}
	if !strings.Contains(string(data), `unknown verdict: "maybe"`) {
		t.Fatalf("review-failure should mention the unknown verdict, got:\n%s", string(data))
	}
}

func TestPostReviewAction_MalformedJSON(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{bad json`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("malformed JSON task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure not written for malformed JSON")
	}
	if !strings.Contains(string(data), "could not parse verdict file") {
		t.Fatal("review-failure should mention parse error")
	}
}

func TestPostReviewAction_EmptyVerdictFile(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(""), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("empty verdict file task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure not written for empty verdict file")
	}
}

func TestReviewedReRegex(t *testing.T) {
	tests := []struct {
		name  string
		input string
		match bool
	}{
		{"complete marker", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->", true},
		{"extra whitespace before close", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved  -->", true},
		{"missing closing tag", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved", false},
		{"partial write no em-dash", "<!-- reviewed: agent1 at 2026-01", false},
		{"missing approved word", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — -->", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewedRe.MatchString(tt.input); got != tt.match {
				t.Errorf("reviewedRe.MatchString(%q) = %v, want %v", tt.input, got, tt.match)
			}
		})
	}
}

func TestReviewRejectionReRegex(t *testing.T) {
	tests := []struct {
		name  string
		input string
		match bool
	}{
		{"complete marker", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing error wrapping -->", true},
		{"reason with spaces", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — needs better test coverage for edge cases -->", true},
		{"extra whitespace before close", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — bad code  -->", true},
		{"missing closing tag", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing error wrapping", false},
		{"partial write no em-dash", "<!-- review-rejection: agent1 at 2026-01", false},
		{"missing reason after em-dash", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — -->", false},
		{"no em-dash or reason", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewRejectionRe.MatchString(tt.input); got != tt.match {
				t.Errorf("reviewRejectionRe.MatchString(%q) = %v, want %v", tt.input, got, tt.match)
			}
		})
	}
}

func TestPostReviewAction_PartialRejectionMarkerTreatedAsNoVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"ready-for-review", "ready-to-merge", "backlog", "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, "ready-for-review", taskFile)
	// Partial rejection marker (no closing -->, simulating agent crash during write)
	os.WriteFile(reviewPath, []byte(strings.Join([]string{
		"<!-- claimed-by: task-agent -->",
		"<!-- branch: task/review-task -->",
		"# Review Task",
		"",
		"<!-- review-rejection: review-agent at 2026-01-01T00:00:00Z",
	}, "\n")), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Partial marker should NOT be treated as a valid rejection.
	// Task should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task with partial rejection marker should stay in ready-for-review/")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", taskFile)); err == nil {
		t.Fatal("task with partial rejection marker should not be moved to backlog/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure record not written for partial rejection marker")
	}
}

func TestAppendReviewFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	appendReviewFailure(path, "review-abc", "could not fetch branch")

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- review-failure: review-abc") {
		t.Fatal("review-failure record not written")
	}
	if !strings.Contains(string(data), "error=could not fetch branch") {
		t.Fatal("review-failure reason not included")
	}
}

func TestBuildDockerArgs_TTYFlag(t *testing.T) {
	base := dockerConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}

	t.Run("with TTY passes -it", func(t *testing.T) {
		cfg := base
		cfg.isTTY = true
		args := buildDockerArgs(cfg, nil, nil)
		if args[2] != "-it" {
			t.Fatalf("expected -it flag when isTTY=true, got %q", args[2])
		}
	})

	t.Run("without TTY passes -i", func(t *testing.T) {
		cfg := base
		cfg.isTTY = false
		args := buildDockerArgs(cfg, nil, nil)
		if args[2] != "-i" {
			t.Fatalf("expected -i flag when isTTY=false, got %q", args[2])
		}
	})
}

func TestIsTerminal(t *testing.T) {
	// os.Stdin in a test runner (go test) is typically not a terminal.
	// A pipe or /dev/null is definitely not a terminal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	if isTerminal(r) {
		t.Fatal("pipe should not be detected as a terminal")
	}

	// /dev/null is also not a terminal.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	if isTerminal(devNull) {
		t.Fatal("/dev/null should not be detected as a terminal")
	}
}

func TestMoveReviewedTask_Success(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, "ready-for-review")
	dstDir := filepath.Join(tasksDir, "ready-to-merge")
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "my-task.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# My Task\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/my-task",
		Title:    "My Task",
		TaskPath: srcPath,
	}

	moveReviewedTask(tasksDir, "agent1", task, "ready-to-merge", "approved", "review")

	// Source should be removed
	if _, err := os.Stat(srcPath); err == nil {
		t.Fatal("source file should have been removed after move")
	}
	// Destination should exist
	dstPath := filepath.Join(dstDir, taskFile)
	if _, err := os.Stat(dstPath); err != nil {
		t.Fatalf("destination file should exist: %v", err)
	}
}

func TestMoveReviewedTask_DestinationExists(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, "ready-for-review")
	dstDir := filepath.Join(tasksDir, "backlog")
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "dup-task.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# Dup Task\n"), 0o644)

	// Pre-create a file at the destination to simulate a race
	dstPath := filepath.Join(dstDir, taskFile)
	os.WriteFile(dstPath, []byte("# Existing\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/dup-task",
		Title:    "Dup Task",
		TaskPath: srcPath,
	}

	moveReviewedTask(tasksDir, "agent1", task, "backlog", "rejected", "review")

	// Source should still exist (move was skipped)
	if _, err := os.Stat(srcPath); err != nil {
		t.Fatalf("source file should still exist when destination already exists: %v", err)
	}
	// Destination should still have original content
	data, _ := os.ReadFile(dstPath)
	if string(data) != "# Existing\n" {
		t.Fatalf("destination file should not have been overwritten, got: %s", string(data))
	}
}

func TestCheckDocker_NotInPath(t *testing.T) {
	// Override PATH so "docker" cannot be found.
	t.Setenv("PATH", t.TempDir())

	err := checkDocker()
	if err == nil {
		t.Fatal("checkDocker should fail when docker is not in PATH")
	}
	if !strings.Contains(err.Error(), "docker is required but not available") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCheckDocker_Available(t *testing.T) {
	// Only run when Docker is actually available in the test environment.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available in test environment")
	}
	// Also skip if daemon is not running (e.g., CI without Docker).
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not running: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	if err := checkDocker(); err != nil {
		t.Fatalf("checkDocker should succeed when Docker is available: %v", err)
	}
}

func TestGracefulShutdownDelay(t *testing.T) {
	if gracefulShutdownDelay != 10*time.Second {
		t.Fatalf("expected gracefulShutdownDelay to be 10s, got %v", gracefulShutdownDelay)
	}
}

func TestSignalForwarding_ContextCancelSendsSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	// Create a cancellable context to simulate SIGINT/SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start command: %v", err)
	}

	// Cancel the parent context (simulates receiving a signal).
	cancel()

	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected error from cancelled command, got nil")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", ctx.Err())
	}
}

func TestSignalForwarding_TimeoutStillWorks(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	// Verify that timeout still kills the command when using the
	// two-phase signal forwarding setup.
	ctx := context.Background()
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error from timed-out command, got nil")
	}
	if timeoutCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", timeoutCtx.Err())
	}
}

func TestSignalForwarding_ProcessExitsOnSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	// Verify that the process exits promptly after SIGTERM via Cancel,
	// well before the WaitDelay would escalate to SIGKILL.
	ctx, cancel := context.WithCancel(context.Background())
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Cancel after a short delay to ensure the process is running.
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_ = cmd.Wait()
	elapsed := time.Since(start)

	// sleep responds to SIGTERM immediately, so it should exit well
	// before the 10s WaitDelay. Allow 5 seconds to be safe in CI.
	if elapsed > 5*time.Second {
		t.Fatalf("process took %v to exit after SIGTERM; expected prompt exit", elapsed)
	}
}

func TestPollBackoff_BelowThreshold(t *testing.T) {
	for i := 0; i < errBackoffThreshold; i++ {
		got := pollBackoff(i)
		if got != basePollInterval {
			t.Errorf("pollBackoff(%d) = %v, want %v", i, got, basePollInterval)
		}
	}
}

func TestPollBackoff_AtAndAboveThreshold(t *testing.T) {
	// At threshold: 10s * 2^1 = 20s
	got := pollBackoff(errBackoffThreshold)
	want := basePollInterval * 2
	if got != want {
		t.Errorf("pollBackoff(%d) = %v, want %v", errBackoffThreshold, got, want)
	}

	// One above: 10s * 2^2 = 40s
	got = pollBackoff(errBackoffThreshold + 1)
	want = basePollInterval * 4
	if got != want {
		t.Errorf("pollBackoff(%d) = %v, want %v", errBackoffThreshold+1, got, want)
	}

	// Two above: 10s * 2^3 = 80s
	got = pollBackoff(errBackoffThreshold + 2)
	want = basePollInterval * 8
	if got != want {
		t.Errorf("pollBackoff(%d) = %v, want %v", errBackoffThreshold+2, got, want)
	}
}

func TestPollBackoff_CapsAtMax(t *testing.T) {
	got := pollBackoff(100)
	if got != maxPollInterval {
		t.Errorf("pollBackoff(100) = %v, want %v (max)", got, maxPollInterval)
	}

	// Just past the cap boundary: basePollInterval * 2^N >= maxPollInterval
	// with base=10s and max=5m=300s, 10*2^5=320s > 300s, so threshold+4
	got = pollBackoff(errBackoffThreshold + 4)
	if got != maxPollInterval {
		t.Errorf("pollBackoff(%d) = %v, want %v (max)", errBackoffThreshold+4, got, maxPollInterval)
	}
}

func TestPollBackoff_ExponentialProgression(t *testing.T) {
	prev := pollBackoff(errBackoffThreshold - 1) // base
	for i := errBackoffThreshold; i < errBackoffThreshold+10; i++ {
		cur := pollBackoff(i)
		if cur < prev {
			t.Errorf("pollBackoff(%d) = %v < pollBackoff(%d) = %v; expected non-decreasing", i, cur, i-1, prev)
		}
		if cur > maxPollInterval {
			t.Errorf("pollBackoff(%d) = %v exceeds maxPollInterval %v", i, cur, maxPollInterval)
		}
		prev = cur
	}
}

func TestValidateTasksDir(t *testing.T) {
	t.Run("relative path resolved to absolute", func(t *testing.T) {
		dir := t.TempDir()
		rel := filepath.Join(dir, "subdir", ".tasks")
		// Create the parent so validation passes.
		os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

		got, err := validateTasksDir(rel)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !filepath.IsAbs(got) {
			t.Errorf("expected absolute path, got %q", got)
		}
	})

	t.Run("nonexistent parent returns error", func(t *testing.T) {
		dir := t.TempDir()
		bad := filepath.Join(dir, "no-such-parent", "deep", ".tasks")

		_, err := validateTasksDir(bad)
		if err == nil {
			t.Fatal("expected error for nonexistent parent, got nil")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("error should mention 'does not exist', got: %v", err)
		}
	})

	t.Run("parent is a file returns error", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "not-a-dir")
		os.WriteFile(filePath, []byte("x"), 0o644)
		bad := filepath.Join(filePath, ".tasks")

		_, err := validateTasksDir(bad)
		if err == nil {
			t.Fatal("expected error when parent is a file, got nil")
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("error should mention 'not a directory', got: %v", err)
		}
	})

	t.Run("valid absolute path passes", func(t *testing.T) {
		dir := t.TempDir()
		tasksDir := filepath.Join(dir, ".tasks")

		got, err := validateTasksDir(tasksDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != tasksDir {
			t.Errorf("expected %q, got %q", tasksDir, got)
		}
	})
}
