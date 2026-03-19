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

	// Task with 3 failures (default max_retries=3) — should be moved to failed/
	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"---",
		"priority: 5",
		"---",
		"# Exhausted Task",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"<!-- branch: task/exhausted -->",
		"",
	}, "\n")), 0o644)

	// Task with 1 failure — should still be a candidate
	os.WriteFile(filepath.Join(reviewDir, "healthy.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Healthy Task",
		"<!-- failure: one -->",
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

	// Task with max_retries=1 and 1 failure — should be exhausted
	os.WriteFile(filepath.Join(reviewDir, "custom.md"), []byte(strings.Join([]string{
		"---",
		"max_retries: 1",
		"priority: 5",
		"---",
		"# Custom Retries",
		"<!-- failure: one -->",
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

	// Only task has exhausted retries — selectTaskForReview should return nil
	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"# Exhausted",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
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

	// Task with 2 failures but default max_retries=3 — should remain a candidate
	os.WriteFile(filepath.Join(reviewDir, "still-ok.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Still OK",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
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
