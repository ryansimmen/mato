package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
