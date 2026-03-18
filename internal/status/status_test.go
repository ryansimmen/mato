package status

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
)

func setupTestRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if _, err := git.Output(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := git.Output(dir, "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config user.email: %v", err)
	}
	if _, err := git.Output(dir, "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config user.name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md: %v", err)
	}
	if _, err := git.Output(dir, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(dir, "commit", "-m", "initial"); err != nil {
		t.Fatalf("git commit initial: %v", err)
	}
	return dir
}

func TestShowWithPopulatedTasksDir(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	files := map[string]string{
		filepath.Join(tasksDir, "waiting", "refactor-api.md"):    "---\nid: refactor-api\ndepends_on: [setup-models, add-auth]\n---\nRefactor API\n",
		filepath.Join(tasksDir, "backlog", "add-auth.md"):        "---\nid: add-auth\npriority: 10\n---\nAdd auth\n",
		filepath.Join(tasksDir, "in-progress", "agent-task.md"):  "In progress\n",
		filepath.Join(tasksDir, "ready-to-merge", "merge-me.md"): "Ready\n",
		filepath.Join(tasksDir, "completed", "setup-models.md"):  "---\nid: setup-models\n---\nDone\n",
		filepath.Join(tasksDir, "failed", "failed-task.md"):      "Failed\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", path, err)
		}
	}
	// Use RegisterAgent to write the lock file with a valid "PID:starttime" identity.
	cleanup, err := queue.RegisterAgent(tasksDir, "abcd1234")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID:     "msg1",
		From:   "agent-abcd1234",
		Type:   "intent",
		Task:   "refactor-api.md",
		Body:   "Starting work on refactor-api.md",
		SentAt: time.Date(2024, time.May, 1, 14, 30, 2, 0, time.UTC),
	}); err != nil {
		t.Fatalf("messaging.WriteMessage: %v", err)
	}

	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = originalStdout }()

	callErr := Show(repoRoot, "")
	w.Close()
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if callErr != nil {
		t.Fatalf("Show: %v", callErr)
	}
}

func TestActiveAgentsPIDColonStarttime(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Use RegisterAgent to write a lock file with the real "PID:starttime"
	// identity so that IsAgentActive returns true.
	cleanup, err := queue.RegisterAgent(tasksDir, "abc12345")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	agents, agentsErr := activeAgents(tasksDir)
	if agentsErr != nil {
		t.Fatalf("activeAgents: %v", agentsErr)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	pid := os.Getpid()
	if agents[0].PID != pid {
		t.Errorf("expected PID %d, got %d", pid, agents[0].PID)
	}
	if agents[0].ID != "abc12345" {
		t.Errorf("expected ID abc12345, got %s", agents[0].ID)
	}
}

func TestActiveAgentsLegacyPIDOnly(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Write lock file with legacy PID-only format.
	pid := os.Getpid()
	if err := os.WriteFile(filepath.Join(locksDir, "legacy01.pid"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	agents, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].PID != pid {
		t.Errorf("expected PID %d, got %d", pid, agents[0].PID)
	}
	if agents[0].ID != "legacy01" {
		t.Errorf("expected ID legacy01, got %s", agents[0].ID)
	}
}

func TestShowIncludesPresenceInfo(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register an agent so it shows up as active.
	cleanup, err := queue.RegisterAgent(tasksDir, "abc12345")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	// Write presence for this agent.
	if err := messaging.WritePresence(tasksDir, "abc12345", "fix-race.md", "task/fix-race"); err != nil {
		t.Fatalf("WritePresence: %v", err)
	}

	// Capture stdout.
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = originalStdout }()

	callErr := Show(repoRoot, "")
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if callErr != nil {
		t.Fatalf("Show: %v", callErr)
	}

	output := string(out)
	wantAgent := "agent-abc12345"
	wantTask := "fix-race.md"
	wantBranch := "task/fix-race"

	if !contains(output, wantAgent) {
		t.Errorf("output should contain %q", wantAgent)
	}
	if !contains(output, wantTask) {
		t.Errorf("output should contain task %q", wantTask)
	}
	if !contains(output, wantBranch) {
		t.Errorf("output should contain branch %q", wantBranch)
	}

	// Verify the line has the expected format.
	expectedLine := "agent-abc12345 (PID"
	if !contains(output, expectedLine) {
		t.Errorf("output should contain %q, got:\n%s", expectedLine, output)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && len(substr) > 0 && containsSubstring(s, substr)
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
