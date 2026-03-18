package status

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/process"
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
		filepath.Join(tasksDir, "waiting", "refactor-api.md"):    "---\nid: refactor-api\ndepends_on: [setup-models, add-auth]\n---\n# Refactor the API layer\n",
		filepath.Join(tasksDir, "backlog", "add-auth.md"):        "---\nid: add-auth\npriority: 10\n---\n# Add authentication\n",
		filepath.Join(tasksDir, "in-progress", "agent-task.md"):  "<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: agent-task\n---\n# In progress task\n",
		filepath.Join(tasksDir, "ready-to-merge", "merge-me.md"): "---\npriority: 5\n---\n# Ready to merge\n",
		filepath.Join(tasksDir, "completed", "setup-models.md"):  "---\nid: setup-models\n---\n# Setup models\n",
		filepath.Join(tasksDir, "failed", "failed-task.md"):      "<!-- failure: agent-1 at 2026-01-01T00:01:00Z — tests failed -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z — merge conflict in config.yaml -->\n---\nid: failed-task\nmax_retries: 2\n---\n# A failed task\n",
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
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("io.ReadAll: %v", readErr)
	}
	if callErr != nil {
		t.Fatalf("Show: %v", callErr)
	}

	output := string(out)

	// Verify task titles appear.
	if !contains(output, "In progress task") {
		t.Errorf("output should contain in-progress task title, got:\n%s", output)
	}
	if !contains(output, "Ready to merge") {
		t.Errorf("output should contain ready-to-merge task title, got:\n%s", output)
	}
	if !contains(output, "A failed task") {
		t.Errorf("output should contain failed task title, got:\n%s", output)
	}
	if !contains(output, "Refactor the API layer") {
		t.Errorf("output should contain waiting task title, got:\n%s", output)
	}

	// Verify failure reason is shown.
	if !contains(output, "merge conflict in config.yaml") {
		t.Errorf("output should contain last failure reason, got:\n%s", output)
	}
	if !contains(output, "2/2 retries exhausted") {
		t.Errorf("output should contain retry budget info, got:\n%s", output)
	}

	// Verify merge queue shows idle.
	if !contains(output, "merge queue:    idle") {
		t.Errorf("output should contain 'merge queue:    idle', got:\n%s", output)
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

func TestShowIncludesProgressSection(t *testing.T) {
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

	// Write a progress message.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID:     "prog1",
		From:   "7da2c4fa",
		Type:   "progress",
		Task:   "fix-race.md",
		Branch: "task/fix-race",
		Body:   "Step: WORK",
		SentAt: time.Now().UTC().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	// Write a second progress message from a different agent.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID:     "prog2",
		From:   "a1b2c3d4",
		Type:   "progress",
		Task:   "add-retries.md",
		Branch: "task/add-retries",
		Body:   "Step: PUSH_BRANCH",
		SentAt: time.Now().UTC().Add(-30 * time.Second),
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
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

	// Check section header appears.
	if !contains(output, "Current Agent Progress") {
		t.Errorf("output should contain 'Current Agent Progress' section header, got:\n%s", output)
	}

	// Check first agent progress line.
	if !contains(output, "agent-7da2c4fa") {
		t.Errorf("output should contain 'agent-7da2c4fa', got:\n%s", output)
	}
	if !contains(output, "Step: WORK") {
		t.Errorf("output should contain 'Step: WORK', got:\n%s", output)
	}
	if !contains(output, "fix-race.md") {
		t.Errorf("output should contain 'fix-race.md', got:\n%s", output)
	}

	// Check second agent progress line.
	if !contains(output, "agent-a1b2c3d4") {
		t.Errorf("output should contain 'agent-a1b2c3d4', got:\n%s", output)
	}
	if !contains(output, "Step: PUSH_BRANCH") {
		t.Errorf("output should contain 'Step: PUSH_BRANCH', got:\n%s", output)
	}
}

func TestTimeInState(t *testing.T) {
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

	// Create an in-progress task claimed 2 hours ago.
	claimedAt := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	content := "<!-- claimed-by: test-agent  claimed-at: " + claimedAt + " -->\n---\nid: time-test\n---\n# Time test task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, "in-progress", "time-test.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	output := captureShow(t, repoRoot)

	// Should show time in state (~120 min).
	if !contains(output, "min") {
		t.Errorf("output should contain time-in-state with 'min', got:\n%s", output)
	}
	if !contains(output, "agent test-agent") {
		t.Errorf("output should contain 'agent test-agent', got:\n%s", output)
	}
}

func TestRetryBudgetInProgress(t *testing.T) {
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

	// Create an in-progress task with 1 prior failure.
	content := "<!-- claimed-by: retry-agent  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- failure: agent-old at 2026-01-01T00:01:00Z — tests failed -->\n---\nid: retry-test\nmax_retries: 3\n---\n# Retry test\n"
	if err := os.WriteFile(filepath.Join(tasksDir, "in-progress", "retry-test.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "1/3 retries used") {
		t.Errorf("output should contain '1/3 retries used', got:\n%s", output)
	}
}

func TestReverseDependencies(t *testing.T) {
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

	// Create an in-progress task.
	inProgressContent := "<!-- claimed-by: dep-agent  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: base-task\n---\n# Base task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, "in-progress", "base-task.md"), []byte(inProgressContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create two waiting tasks that depend on the in-progress task.
	for _, name := range []string{"waiter-a.md", "waiter-b.md"} {
		content := "---\nid: " + strings.TrimSuffix(name, ".md") + "\ndepends_on: [base-task]\n---\n# " + name + "\n"
		if err := os.WriteFile(filepath.Join(tasksDir, "waiting", name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "2 tasks waiting") {
		t.Errorf("output should contain '2 tasks waiting', got:\n%s", output)
	}
}

func TestRecentCompletions(t *testing.T) {
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

	// Write a completion detail.
	detail := messaging.CompletionDetail{
		TaskID:       "done-task",
		TaskFile:     "done-task.md",
		Branch:       "task/done-task",
		CommitSHA:    "abc123def456789",
		FilesChanged: []string{"src/main.go", "src/main_test.go", "README.md"},
		Title:        "Completed task title",
		MergedAt:     time.Now().UTC().Add(-30 * time.Minute),
	}
	if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "Recent Completions") {
		t.Errorf("output should contain 'Recent Completions' section, got:\n%s", output)
	}
	if !contains(output, "Completed task title") {
		t.Errorf("output should contain completion title, got:\n%s", output)
	}
	if !contains(output, "abc123d") {
		t.Errorf("output should contain short commit SHA 'abc123d', got:\n%s", output)
	}
	if !contains(output, "3 files") {
		t.Errorf("output should contain '3 files', got:\n%s", output)
	}
}

func TestMergeLockActive(t *testing.T) {
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

	// Write a merge lock held by the current process.
	identity := process.LockIdentity(os.Getpid())
	if err := os.WriteFile(filepath.Join(tasksDir, ".locks", "merge.lock"), []byte(identity), 0o644); err != nil {
		t.Fatalf("WriteFile merge.lock: %v", err)
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "merge queue:    active") {
		t.Errorf("output should contain 'merge queue:    active', got:\n%s", output)
	}
}

func TestFailureReasonExtraction(t *testing.T) {
	dir := t.TempDir()

	// Single failure.
	single := filepath.Join(dir, "single.md")
	os.WriteFile(single, []byte("<!-- failure: agent-1 at 2026-01-01T00:01:00Z — tests failed -->\n# Task\n"), 0o644)
	if got := lastFailureReason(single); got != "tests failed" {
		t.Errorf("lastFailureReason(single) = %q, want %q", got, "tests failed")
	}

	// Multiple failures — should return last.
	multi := filepath.Join(dir, "multi.md")
	os.WriteFile(multi, []byte("<!-- failure: agent-1 at 2026-01-01T00:01:00Z — first error -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z — second error -->\n# Task\n"), 0o644)
	if got := lastFailureReason(multi); got != "second error" {
		t.Errorf("lastFailureReason(multi) = %q, want %q", got, "second error")
	}

	// No failures.
	none := filepath.Join(dir, "none.md")
	os.WriteFile(none, []byte("# Task\n"), 0o644)
	if got := lastFailureReason(none); got != "" {
		t.Errorf("lastFailureReason(none) = %q, want empty", got)
	}
}

func TestParseClaimedAt(t *testing.T) {
	dir := t.TempDir()

	// Valid claimed-at.
	valid := filepath.Join(dir, "valid.md")
	os.WriteFile(valid, []byte("<!-- claimed-by: agent-1  claimed-at: 2026-03-15T10:30:00Z -->\n# Task\n"), 0o644)
	got := parseClaimedAt(valid)
	want := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseClaimedAt(valid) = %v, want %v", got, want)
	}

	// No claimed-at.
	none := filepath.Join(dir, "none.md")
	os.WriteFile(none, []byte("# Task\n"), 0o644)
	if got := parseClaimedAt(none); !got.IsZero() {
		t.Errorf("parseClaimedAt(none) = %v, want zero", got)
	}
}

func TestReverseDependenciesHelper(t *testing.T) {
	tasksDir := t.TempDir()
	waitingDir := filepath.Join(tasksDir, "waiting")
	os.MkdirAll(waitingDir, 0o755)

	// Task A depends on X and Y.
	os.WriteFile(filepath.Join(waitingDir, "task-a.md"), []byte("---\nid: task-a\ndepends_on: [dep-x, dep-y]\n---\n# A\n"), 0o644)
	// Task B also depends on X.
	os.WriteFile(filepath.Join(waitingDir, "task-b.md"), []byte("---\nid: task-b\ndepends_on: [dep-x]\n---\n# B\n"), 0o644)

	result := reverseDependencies(tasksDir)

	if len(result["dep-x"]) != 2 {
		t.Errorf("dep-x should have 2 dependents, got %d", len(result["dep-x"]))
	}
	if len(result["dep-y"]) != 1 {
		t.Errorf("dep-y should have 1 dependent, got %d", len(result["dep-y"]))
	}
}

func TestMergeLockIdle(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)

	// No merge lock file — should be idle.
	if isMergeLockActive(tasksDir) {
		t.Error("isMergeLockActive should be false when no lock file exists")
	}

	// Dead process lock — should be idle.
	os.WriteFile(filepath.Join(tasksDir, ".locks", "merge.lock"), []byte("2147483647"), 0o644)
	if isMergeLockActive(tasksDir) {
		t.Error("isMergeLockActive should be false for dead process")
	}
}

// captureShow runs Show and returns the captured stdout output.
func captureShow(t *testing.T, repoRoot string) string {
	t.Helper()
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = originalStdout }()

	callErr := Show(repoRoot, "")
	w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("io.ReadAll: %v", readErr)
	}
	if callErr != nil {
		t.Fatalf("Show: %v", callErr)
	}
	return string(out)
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
