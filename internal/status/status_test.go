package status

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mato/internal/messaging"
	"mato/internal/process"
	"mato/internal/queue"
	"mato/internal/testutil"
)

func TestShowWithPopulatedTasksDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	files := map[string]string{
		filepath.Join(tasksDir, queue.DirWaiting, "refactor-api.md"):       "---\nid: refactor-api\ndepends_on: [setup-models, add-auth]\n---\n# Refactor the API layer\n",
		filepath.Join(tasksDir, queue.DirBacklog, "add-auth.md"):           "---\nid: add-auth\npriority: 10\n---\n# Add authentication\n",
		filepath.Join(tasksDir, queue.DirInProgress, "agent-task.md"):      "<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: agent-task\n---\n# In progress task\n",
		filepath.Join(tasksDir, queue.DirReadyReview, "review-feature.md"): "<!-- branch: task/review-feature -->\n---\npriority: 10\n---\n# Review this feature\n",
		filepath.Join(tasksDir, queue.DirReadyMerge, "merge-me.md"):        "---\npriority: 5\n---\n# Ready to merge\n",
		filepath.Join(tasksDir, queue.DirCompleted, "setup-models.md"):     "---\nid: setup-models\n---\n# Setup models\n",
		filepath.Join(tasksDir, queue.DirFailed, "failed-task.md"):         "<!-- failure: agent-1 at 2026-01-01T00:01:00Z — tests failed -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z — merge conflict in config.yaml -->\n---\nid: failed-task\nmax_retries: 2\n---\n# A failed task\n",
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
	if !contains(output, "Review this feature") {
		t.Errorf("output should contain ready-for-review task title, got:\n%s", output)
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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register both agents as active so their progress messages are shown.
	cleanup1, err := queue.RegisterAgent(tasksDir, "7da2c4fa")
	if err != nil {
		t.Fatalf("RegisterAgent 7da2c4fa: %v", err)
	}
	defer cleanup1()
	cleanup2, err := queue.RegisterAgent(tasksDir, "a1b2c3d4")
	if err != nil {
		t.Fatalf("RegisterAgent a1b2c3d4: %v", err)
	}
	defer cleanup2()

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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
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
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "time-test.md"), []byte(content), 0o644); err != nil {
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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create an in-progress task with 1 prior failure.
	content := "<!-- claimed-by: retry-agent  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- failure: agent-old at 2026-01-01T00:01:00Z — tests failed -->\n---\nid: retry-test\nmax_retries: 3\n---\n# Retry test\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "retry-test.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "1/3 retries used") {
		t.Errorf("output should contain '1/3 retries used', got:\n%s", output)
	}
}

func TestReverseDependencies(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create an in-progress task.
	inProgressContent := "<!-- claimed-by: dep-agent  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: base-task\n---\n# Base task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "base-task.md"), []byte(inProgressContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create two waiting tasks that depend on the in-progress task.
	for _, name := range []string{"waiter-a.md", "waiter-b.md"} {
		content := "---\nid: " + strings.TrimSuffix(name, ".md") + "\ndepends_on: [base-task]\n---\n# " + name + "\n"
		if err := os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "2 tasks waiting") {
		t.Errorf("output should contain '2 tasks waiting', got:\n%s", output)
	}
}

func TestRecentCompletions(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
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
	waitingDir := filepath.Join(tasksDir, queue.DirWaiting)
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

func TestProgressFilteredToActiveAgents(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register only agent-1 as active (agent-2 is dead — no lock file).
	cleanup, err := queue.RegisterAgent(tasksDir, "active01")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	// Write progress for both the active and a dead agent.
	for _, msg := range []messaging.Message{
		{ID: "p1", From: "active01", Type: "progress", Task: "live.md", Body: "Step: WORK", SentAt: time.Now().UTC().Add(-1 * time.Minute)},
		{ID: "p2", From: "dead0000", Type: "progress", Task: "ghost.md", Body: "Step: MARK_READY", SentAt: time.Now().UTC().Add(-500 * time.Minute)},
	} {
		if err := messaging.WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
	}

	output := captureShow(t, repoRoot)

	// Active agent's progress should appear in Current Agent Progress.
	if !contains(output, "agent-active01") {
		t.Errorf("output should contain active agent progress, got:\n%s", output)
	}
	// Dead agent should NOT appear in Current Agent Progress section.
	// Extract just the Current Agent Progress section for verification.
	progressStart := strings.Index(output, "Current Agent Progress")
	progressEnd := strings.Index(output[progressStart:], "\n\n")
	if progressStart < 0 || progressEnd < 0 {
		t.Fatalf("could not find Current Agent Progress section in output:\n%s", output)
	}
	progressSection := output[progressStart : progressStart+progressEnd]
	if contains(progressSection, "dead0000") {
		t.Errorf("Current Agent Progress should NOT contain dead agent, got:\n%s", progressSection)
	}
	if contains(progressSection, "ghost.md") {
		t.Errorf("Current Agent Progress should NOT contain dead agent's task, got:\n%s", progressSection)
	}
}

func TestRecentMessagesAgentPrefix(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Message with bare ID (no "agent-" prefix).
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		ID:     "m1",
		From:   "abc12345",
		Type:   "intent",
		Task:   "some-task.md",
		Body:   "Starting",
		SentAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	output := captureShow(t, repoRoot)

	// Recent Messages should show "agent-abc12345", not bare "abc12345".
	if !contains(output, "agent-abc12345") {
		t.Errorf("Recent Messages should show 'agent-abc12345', got:\n%s", output)
	}
}

func TestReadyForReviewSection(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Write a task with a branch comment.
	taskContent := "<!-- branch: task/add-login -->\n---\npriority: 20\n---\n# Add login page\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add-login.md"), []byte(taskContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Write a task without a branch comment.
	taskContent2 := "---\npriority: 30\n---\n# Fix typo in docs\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "fix-typo.md"), []byte(taskContent2), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Write a task with unparseable frontmatter (produces empty title).
	taskContent3 := "<!-- branch: task/empty-title -->\n---\n: invalid yaml [\n---\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "no-title.md"), []byte(taskContent3), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
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

	if !contains(output, "Ready for Review") {
		t.Errorf("output should contain 'Ready for Review' section header, got:\n%s", output)
	}
	if !contains(output, "add-login.md") {
		t.Errorf("output should contain task filename 'add-login.md', got:\n%s", output)
	}
	if !contains(output, "Add login page") {
		t.Errorf("output should contain task title 'Add login page', got:\n%s", output)
	}
	if !contains(output, "on task/add-login") {
		t.Errorf("output should contain branch info 'on task/add-login', got:\n%s", output)
	}
	if !contains(output, "fix-typo.md") {
		t.Errorf("output should contain task filename 'fix-typo.md', got:\n%s", output)
	}
	if !contains(output, "Fix typo in docs") {
		t.Errorf("output should contain task title 'Fix typo in docs', got:\n%s", output)
	}
	// The queue overview should show the count.
	if !contains(output, "ready-review:   3") {
		t.Errorf("output should contain 'ready-review:   3', got:\n%s", output)
	}

	// Empty-title task should not produce a double space or trailing dash.
	if !contains(output, "no-title.md — on task/empty-title") {
		t.Errorf("output should show 'no-title.md — on task/empty-title' (no double space), got:\n%s", output)
	}
	// Ensure the empty title doesn't produce "—  on" (double space after dash).
	if contains(output, "no-title.md —  on") {
		t.Errorf("output should NOT have double space after dash for empty title, got:\n%s", output)
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

func TestShowToBuffer(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Add a backlog task so output is meaningful.
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "demo.md"), []byte("---\nid: demo\npriority: 10\n---\n# Demo task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, ""); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	output := buf.String()
	if !contains(output, "Queue Overview") {
		t.Errorf("ShowTo output should contain 'Queue Overview', got:\n%s", output)
	}
	if !contains(output, "runnable:") {
		t.Errorf("ShowTo output should contain 'runnable:', got:\n%s", output)
	}
	if !contains(output, "Recent Messages") {
		t.Errorf("ShowTo output should contain 'Recent Messages', got:\n%s", output)
	}
}

func TestWatch(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Add a backlog task so Show() produces meaningful output.
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "sample.md"), []byte("---\ntitle: Sample Task\n---\nDo something.\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Capture stdout.
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = originalStdout }()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Watch(ctx, repoRoot, tasksDir, 100*time.Millisecond)
	}()

	// Let Watch run for at least one refresh cycle.
	time.Sleep(250 * time.Millisecond)
	cancel()

	watchErr := <-errCh
	w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("io.ReadAll: %v", readErr)
	}
	if watchErr != nil {
		t.Fatalf("Watch returned error: %v", watchErr)
	}

	output := string(out)
	if !contains(output, "backlog") {
		t.Errorf("Watch output should contain queue overview with 'backlog', got:\n%s", output)
	}
	if !contains(output, "Ctrl+C") {
		t.Errorf("Watch output should contain 'Ctrl+C' hint, got:\n%s", output)
	}
	// Verify atomic redraw: cursor-home (\033[H) and clear-to-end (\033[J)
	// should be present, but full-screen clear (\033[2J) should NOT.
	if contains(output, "\033[2J") {
		t.Errorf("Watch output should NOT contain full-screen clear (\\033[2J)")
	}
	if !contains(output, "\033[H") {
		t.Errorf("Watch output should contain cursor-home (\\033[H)")
	}
	if !contains(output, "\033[J") {
		t.Errorf("Watch output should contain clear-to-end-of-screen (\\033[J)")
	}
}

func TestFailedTaskRendering_CycleFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a cycle-failed task — has cycle-failure marker but no regular failure markers.
	content := "---\nid: cyclic-task\nmax_retries: 3\n---\n# Cyclic task\n\n<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "cyclic-task.md"), []byte(content), 0o644)

	output := captureShow(t, repoRoot)

	// Should show "circular dependency" instead of "0/3 retries exhausted".
	if !contains(output, "circular dependency") {
		t.Errorf("output should contain 'circular dependency' for cycle-failed task, got:\n%s", output)
	}
	if contains(output, "0/3 retries exhausted") {
		t.Errorf("output should NOT show '0/3 retries exhausted' for cycle-failed task, got:\n%s", output)
	}
}

func TestFailedTaskRendering_MixedFailures(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a task with both regular failure and cycle-failure markers.
	// In practice this is unusual, but the rendering should handle it.
	content := "<!-- failure: agent-1 at 2026-01-01T00:01:00Z — tests failed -->\n<!-- cycle-failure: mato at 2026-01-02T00:00:00Z — circular dependency -->\n---\nid: mixed-task\nmax_retries: 3\n---\n# Mixed task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "mixed-task.md"), []byte(content), 0o644)

	output := captureShow(t, repoRoot)

	// With both markers present, it should display the cycle reason.
	if !contains(output, "circular dependency") {
		t.Errorf("output should contain 'circular dependency' for mixed-failure task, got:\n%s", output)
	}
	// It should also show retry-budget info alongside the cycle reason.
	if !contains(output, "1/3 retries used") {
		t.Errorf("output should contain '1/3 retries used' for mixed-failure task, got:\n%s", output)
	}
	// It should show the last regular failure reason.
	if !contains(output, "tests failed") {
		t.Errorf("output should contain last regular failure reason 'tests failed', got:\n%s", output)
	}
}
