package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/messaging"
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

func TestBuildDependencyContext_WithCompletionFiles(t *testing.T) {
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

	result := buildDependencyContext(tasksDir, claimed)
	if result == "" {
		t.Fatal("expected non-empty dependency context")
	}

	var details []messaging.CompletionDetail
	if err := json.Unmarshal([]byte(result), &details); err != nil {
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

func TestBuildDependencyContext_NoDeps(t *testing.T) {
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

	result := buildDependencyContext(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty dependency context, got %q", result)
	}
}

func TestBuildDependencyContext_NoCompletionFiles(t *testing.T) {
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

	result := buildDependencyContext(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty dependency context when no completion files exist, got %q", result)
	}
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

func TestExtractFailureLines_NonexistentFile(t *testing.T) {
	got := extractFailureLines(filepath.Join(t.TempDir(), "nonexistent.md"))
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
