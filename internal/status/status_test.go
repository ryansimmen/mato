package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mato/internal/lockfile"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/process"
	"mato/internal/queue"
	"mato/internal/taskfile"
	"mato/internal/testutil"
)

func TestShowWithPopulatedTasksDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	callErr := ShowVerbose(repoRoot)
	w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("io.ReadAll: %v", readErr)
	}
	if callErr != nil {
		t.Fatalf("ShowVerbose: %v", callErr)
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
	if !contains(output, "pause state:    not paused") {
		t.Errorf("output should contain pause state line, got:\n%s", output)
	}

	// Verify backlog count appears in queue overview (1 task in backlog/).
	if !contains(output, "backlog:") {
		t.Errorf("output should contain 'backlog:' line in queue overview, got:\n%s", output)
	}
	if !contains(output, "blocked:") {
		t.Errorf("output should contain 'blocked:' line in queue overview, got:\n%s", output)
	}

	// Verify runnable backlog section appears with the backlog task.
	if !contains(output, "Runnable Backlog (execution order)") {
		t.Errorf("output should contain runnable backlog section header, got:\n%s", output)
	}
	if !contains(output, "add-auth.md") {
		t.Errorf("output should contain runnable backlog task 'add-auth.md', got:\n%s", output)
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

	agents, _, agentsErr := activeAgents(tasksDir)
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

	agents, _, err := activeAgents(tasksDir)
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
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	callErr := Show(repoRoot)
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
	if contains(output, "(PID") {
		t.Errorf("compact output should not contain PID details, got:\n%s", output)
	}
}

func TestShowIncludesProgressSection(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	callErr := Show(repoRoot)
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if callErr != nil {
		t.Fatalf("Show: %v", callErr)
	}

	output := string(out)

	// Check first agent progress line.
	if !contains(output, "agent-7da2c4fa") {
		t.Errorf("output should contain 'agent-7da2c4fa', got:\n%s", output)
	}
	if !contains(output, "WORK") {
		t.Errorf("output should contain normalized stage 'WORK', got:\n%s", output)
	}
	if !contains(output, "fix-race.md") {
		t.Errorf("output should contain 'fix-race.md', got:\n%s", output)
	}

	// Check second agent progress line.
	if !contains(output, "agent-a1b2c3d4") {
		t.Errorf("output should contain 'agent-a1b2c3d4', got:\n%s", output)
	}
	if !contains(output, "PUSH_BRANCH") {
		t.Errorf("output should contain normalized stage 'PUSH_BRANCH', got:\n%s", output)
	}
}

func TestTimeInState(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

	// Should show time in state (~2 hr).
	if !contains(output, "hr") {
		t.Errorf("output should contain time-in-state with 'hr', got:\n%s", output)
	}
	if !contains(output, "agent test-agent") {
		t.Errorf("output should contain 'agent test-agent', got:\n%s", output)
	}
}

func TestRetryBudgetInProgress(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

	if !contains(output, "1/3 retries used") {
		t.Errorf("output should contain '1/3 retries used', got:\n%s", output)
	}
}

func TestReverseDependencies(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

	if !contains(output, "2 tasks waiting") {
		t.Errorf("output should contain '2 tasks waiting', got:\n%s", output)
	}
}

func TestRecentCompletions(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

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
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

	if !contains(output, "merge queue:    active") {
		t.Errorf("output should contain 'merge queue:    active', got:\n%s", output)
	}
}

func TestFailureReasonExtraction(t *testing.T) {
	// Single failure.
	single := []byte("<!-- failure: agent-1 at 2026-01-01T00:01:00Z — tests failed -->\n# Task\n")
	if got := taskfile.LastFailureReason(single); got != "tests failed" {
		t.Errorf("LastFailureReason(single) = %q, want %q", got, "tests failed")
	}

	// Multiple failures — should return last.
	multi := []byte("<!-- failure: agent-1 at 2026-01-01T00:01:00Z — first error -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z — second error -->\n# Task\n")
	if got := taskfile.LastFailureReason(multi); got != "second error" {
		t.Errorf("LastFailureReason(multi) = %q, want %q", got, "second error")
	}

	// No failures.
	none := []byte("# Task\n")
	if got := taskfile.LastFailureReason(none); got != "" {
		t.Errorf("LastFailureReason(none) = %q, want empty", got)
	}
}

func TestParseClaimedAt(t *testing.T) {
	// Valid claimed-at.
	valid := []byte("<!-- claimed-by: agent-1  claimed-at: 2026-03-15T10:30:00Z -->\n# Task\n")
	got, ok := taskfile.ParseClaimedAt(valid)
	want := time.Date(2026, 3, 15, 10, 30, 0, 0, time.UTC)
	if !ok || !got.Equal(want) {
		t.Errorf("ParseClaimedAt(valid) = %v, %v, want %v, true", got, ok, want)
	}

	// No claimed-at.
	none := []byte("# Task\n")
	if got, ok := taskfile.ParseClaimedAt(none); ok || !got.IsZero() {
		t.Errorf("ParseClaimedAt(none) = %v, %v, want zero, false", got, ok)
	}
}

func TestReverseDependenciesHelper(t *testing.T) {
	tasksDir := t.TempDir()
	for _, dir := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, dir), 0o755)
	}
	waitingDir := filepath.Join(tasksDir, queue.DirWaiting)

	// Task A depends on X and Y.
	os.WriteFile(filepath.Join(waitingDir, "task-a.md"), []byte("---\nid: task-a\ndepends_on: [dep-x, dep-y]\n---\n# A\n"), 0o644)
	// Task B also depends on X.
	os.WriteFile(filepath.Join(waitingDir, "task-b.md"), []byte("---\nid: task-b\ndepends_on: [dep-x]\n---\n# B\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	result := reverseDepsFromIndex(idx)

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

	callErr := Show(repoRoot)
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

func captureShowVerbose(t *testing.T, repoRoot string) string {
	t.Helper()
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = originalStdout }()

	callErr := ShowVerbose(repoRoot)
	w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("io.ReadAll: %v", readErr)
	}
	if callErr != nil {
		t.Fatalf("ShowVerbose: %v", callErr)
	}
	return string(out)
}

func TestProgressFilteredToActiveAgents(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

	// Active agent should appear in the verbose Active Agents section.
	if !contains(output, "agent-active01") {
		t.Errorf("output should contain active agent progress, got:\n%s", output)
	}
	// Dead agent should NOT appear in the verbose Active Agents section.
	progressStart := strings.Index(output, "Active Agents")
	progressEnd := strings.Index(output[progressStart:], "\n\n")
	if progressStart < 0 || progressEnd < 0 {
		t.Fatalf("could not find Active Agents section in output:\n%s", output)
	}
	progressSection := output[progressStart : progressStart+progressEnd]
	if contains(progressSection, "dead0000") {
		t.Errorf("Active Agents section should NOT contain dead agent, got:\n%s", progressSection)
	}
	if contains(progressSection, "ghost.md") {
		t.Errorf("Active Agents section should NOT contain dead agent's task, got:\n%s", progressSection)
	}
}

func TestRecentMessagesAgentPrefix(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

	// Recent Messages should show "agent-abc12345", not bare "abc12345".
	if !contains(output, "agent-abc12345") {
		t.Errorf("Recent Messages should show 'agent-abc12345', got:\n%s", output)
	}
}

func TestReadyForReviewSection(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	callErr := ShowVerbose(repoRoot)
	w.Close()
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("io.ReadAll: %v", readErr)
	}
	if callErr != nil {
		t.Fatalf("ShowVerbose: %v", callErr)
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
	tasksDir := filepath.Join(repoRoot, ".mato")
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
	if err := ShowTo(&buf, repoRoot); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	output := buf.String()
	if !contains(output, "Queue:") {
		t.Errorf("ShowTo output should contain compact queue summary, got:\n%s", output)
	}
	if !contains(output, "Next Up") {
		t.Errorf("ShowTo output should contain compact next-up section, got:\n%s", output)
	}
	if contains(output, "Recent Messages") {
		t.Errorf("compact ShowTo output should not contain 'Recent Messages', got:\n%s", output)
	}
}

func TestShowTo_TextWriterError(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "demo.md"), []byte("---\nid: demo\npriority: 10\n---\n# Demo task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	tests := []struct {
		name string
		show func(io.Writer, string) error
	}{
		{name: "compact", show: ShowTo},
		{name: "verbose", show: ShowVerboseTo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeErr := errors.New("broken pipe")
			fw := &failAfterNWriter{n: 1, err: writeErr}

			err := tt.show(fw, repoRoot)
			if err == nil {
				t.Fatal("expected writer error, got nil")
			}
			if !errors.Is(err, writeErr) {
				t.Fatalf("error = %v, want wrapped %v", err, writeErr)
			}
		})
	}
}

func TestShowTo_UnreadableLockFileWarning(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Register an agent so it appears active.
	cleanup, err := queue.RegisterAgent(tasksDir, "warn0001")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	// Inject a hook that fails to read the lock file (simulating TOCTOU race).
	origFn := readLockFileFn
	readLockFileFn = func(name string) ([]byte, error) {
		if filepath.Base(name) == "warn0001.pid" {
			return nil, errors.New("permission denied")
		}
		return origFn(name)
	}
	defer func() { readLockFileFn = origFn }()

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot); err != nil {
		t.Fatalf("ShowTo should not error on unreadable lock file: %v", err)
	}

	output := buf.String()
	if !contains(output, "skipped unreadable lock file") {
		t.Errorf("ShowTo output should contain lock file warning, got:\n%s", output)
	}
}

func TestActiveAgents_GenuinelyUnreadableLockFile(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Write a lock file then make it unreadable via permissions.
	// Before the fix, identity.IsAgentActive would silently return false
	// (lockfile.IsHeld returns false on read error), causing activeAgents
	// to skip the agent without generating a warning.
	lockPath := filepath.Join(locksDir, "unread01.pid")
	if err := os.WriteFile(lockPath, []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	origRead := lockfile.TestHookReadFile()
	lockfile.SetTestHookReadFile(func(path string) ([]byte, error) {
		if path == lockPath {
			return nil, errors.New("permission denied")
		}
		return origRead(path)
	})
	t.Cleanup(func() { lockfile.SetTestHookReadFile(origRead) })

	agents, warnings, err := activeAgents(tasksDir)
	if err != nil {
		t.Fatalf("activeAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(agents))
	}
	if len(warnings) == 0 {
		t.Fatal("expected at least one warning for unreadable lock file")
	}
	found := false
	for _, w := range warnings {
		if contains(w, "unread01.pid") && contains(w, "unreadable lock file") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about unread01.pid, got: %v", warnings)
	}
}

func TestWatch(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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
		errCh <- Watch(ctx, repoRoot, 100*time.Millisecond)
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
	if !contains(output, "\033[K") {
		t.Errorf("Watch output should contain clear-to-end-of-line (\\033[K) to prevent artifacts when lines shrink")
	}
}

func TestFailedTaskRendering_CycleFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

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
	tasksDir := filepath.Join(repoRoot, ".mato")
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

	output := captureShowVerbose(t, repoRoot)

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

func TestFailedTaskRendering_TerminalFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a terminal-failed task — has terminal-failure marker but no regular failure markers.
	content := "---\nid: terminal-task\nmax_retries: 3\n---\n# Terminal task\n\n<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "terminal-task.md"), []byte(content), 0o644)

	output := captureShowVerbose(t, repoRoot)

	// Should show "structural failure: unparseable frontmatter".
	if !contains(output, "structural failure") {
		t.Errorf("output should contain 'structural failure' for terminal-failed task, got:\n%s", output)
	}
	if !contains(output, "unparseable frontmatter") {
		t.Errorf("output should contain 'unparseable frontmatter' for terminal-failed task, got:\n%s", output)
	}
	// Should NOT show retry budget info.
	if contains(output, "0/3 retries exhausted") {
		t.Errorf("output should NOT show '0/3 retries exhausted' for terminal-failed task, got:\n%s", output)
	}
}

func TestFailedTaskRendering_TerminalWithRetries(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a task with both regular failure and terminal-failure markers.
	content := "<!-- failure: agent-1 at 2026-01-01T00:01:00Z — build failed -->\n<!-- terminal-failure: mato at 2026-01-02T00:00:00Z — invalid glob syntax -->\n---\nid: mixed-terminal\nmax_retries: 3\n---\n# Mixed terminal task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "mixed-terminal.md"), []byte(content), 0o644)

	output := captureShowVerbose(t, repoRoot)

	// Should show terminal failure as structural.
	if !contains(output, "structural failure") {
		t.Errorf("output should contain 'structural failure' for mixed terminal task, got:\n%s", output)
	}
	if !contains(output, "invalid glob syntax") {
		t.Errorf("output should contain 'invalid glob syntax' for mixed terminal task, got:\n%s", output)
	}
	// Should also show retry budget info alongside.
	if !contains(output, "1/3 retries used") {
		t.Errorf("output should contain '1/3 retries used' for mixed terminal task, got:\n%s", output)
	}
	// Should show the last regular failure reason.
	if !contains(output, "build failed") {
		t.Errorf("output should contain last regular failure reason 'build failed', got:\n%s", output)
	}
}

// failWriter fails every Write call with the given error.
type failWriter struct {
	err error
}

func (w *failWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

func TestWatchTo_RedrawWriteError(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	writeErr := errors.New("stdout closed")
	fw := &failWriter{err: writeErr}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- WatchTo(ctx, fw, repoRoot, 100*time.Millisecond)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("WatchTo should return an error when writes fail")
		}
		if !errors.Is(err, writeErr) {
			t.Errorf("WatchTo error should wrap the write error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "redraw") {
			t.Errorf("WatchTo error should contain 'redraw' context, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchTo did not exit after write failure")
	}
}

// failAfterNWriter succeeds for the first n writes, then fails.
type failAfterNWriter struct {
	n     int
	count int
	err   error
	buf   bytes.Buffer
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	w.count++
	if w.count > w.n {
		return 0, w.err
	}
	return w.buf.Write(p)
}

func TestWatchTo_PartialRedrawFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Succeed on the cursor-home write (first), fail on content write (second).
	writeErr := fmt.Errorf("broken pipe")
	fw := &failAfterNWriter{n: 1, err: writeErr}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- WatchTo(ctx, fw, repoRoot, 100*time.Millisecond)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("WatchTo should return an error on partial write failure")
		}
		if !strings.Contains(err.Error(), "redraw content") {
			t.Errorf("error should mention 'redraw content', got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchTo did not exit after partial write failure")
	}
}

func TestWatchTo_ReturnsNilOnCancel(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Add a backlog task so ShowTo produces meaningful output.
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "ticker-test.md"), []byte("---\nid: ticker-test\n---\n# Ticker test\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- WatchTo(ctx, &buf, repoRoot, 50*time.Millisecond)
	}()

	// Let WatchTo run for a few iterations.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("WatchTo should return nil on context cancellation, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchTo did not exit after context cancellation")
	}

	output := buf.String()
	if !strings.Contains(output, "backlog") {
		t.Errorf("WatchTo output should contain 'backlog' from queue overview, got:\n%s", output)
	}
	if !strings.Contains(output, "Ctrl+C") {
		t.Errorf("WatchTo output should contain 'Ctrl+C' hint, got:\n%s", output)
	}
}

func TestRunnableBacklogSection(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create backlog tasks with different priorities.
	files := map[string]string{
		filepath.Join(tasksDir, queue.DirBacklog, "alpha.md"): "---\nid: alpha\npriority: 30\n---\n# Alpha task\n",
		filepath.Join(tasksDir, queue.DirBacklog, "beta.md"):  "---\nid: beta\npriority: 10\n---\n# Beta task\n",
		filepath.Join(tasksDir, queue.DirBacklog, "gamma.md"): "---\nid: gamma\npriority: 20\n---\n# Gamma task\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "Next Up") {
		t.Errorf("output should contain compact next-up header, got:\n%s", output)
	}

	// Tasks should appear in priority order: beta (10), gamma (20), alpha (30).
	betaIdx := strings.Index(output, "beta.md")
	gammaIdx := strings.Index(output, "gamma.md")
	alphaIdx := strings.Index(output, "alpha.md")
	if betaIdx < 0 || gammaIdx < 0 || alphaIdx < 0 {
		t.Fatalf("output should contain all backlog tasks, got:\n%s", output)
	}
	if betaIdx >= gammaIdx {
		t.Errorf("beta (priority 10) should appear before gamma (priority 20), got:\n%s", output)
	}
	if gammaIdx >= alphaIdx {
		t.Errorf("gamma (priority 20) should appear before alpha (priority 30), got:\n%s", output)
	}

	// Numbered entries.
	if !contains(output, "1. beta.md") {
		t.Errorf("output should show '1. beta.md', got:\n%s", output)
	}
}

func TestRunnableBacklogExcludesDeferred(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create an in-progress task that claims "src/main.go".
	inProgressContent := "<!-- claimed-by: test-agent  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: active-task\naffects:\n  - src/main.go\n---\n# Active task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "active-task.md"), []byte(inProgressContent), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Create backlog tasks: one overlaps with in-progress affects, one does not.
	conflicting := "---\nid: conflict-task\npriority: 5\naffects:\n  - src/main.go\n---\n# Conflicting task\n"
	runnable := "---\nid: runnable-task\npriority: 10\naffects:\n  - src/other.go\n---\n# Runnable task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "conflict-task.md"), []byte(conflicting), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "runnable-task.md"), []byte(runnable), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	output := captureShow(t, repoRoot)

	// Next Up should NOT contain the deferred (conflicting) task.
	runnableSection := extractSection(output, "Next Up", "")
	if strings.Contains(runnableSection, "conflict-task.md") {
		t.Errorf("runnable backlog should NOT contain deferred task, got:\n%s", runnableSection)
	}
	if !strings.Contains(runnableSection, "runnable-task.md") {
		t.Errorf("runnable backlog should contain runnable task, got:\n%s", runnableSection)
	}

	if !contains(output, "1 conflict-deferred") {
		t.Errorf("compact output should summarize deferred work in Attention, got:\n%s", output)
	}
}

func TestRunnableBacklogEmpty(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	output := captureShow(t, repoRoot)

	if !contains(output, "Next Up") {
		t.Errorf("output should contain compact next-up header even when empty, got:\n%s", output)
	}
	runnableSection := extractSection(output, "Next Up", "")
	if !strings.Contains(runnableSection, "(none)") {
		t.Errorf("empty runnable backlog should show (none), got:\n%s", runnableSection)
	}
}

func TestRunnableBacklogJSON_Ordering(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create backlog tasks with different priorities.
	files := map[string]string{
		filepath.Join(tasksDir, queue.DirBacklog, "z-task.md"): "---\nid: z-task\npriority: 5\n---\n# Z task\n",
		filepath.Join(tasksDir, queue.DirBacklog, "a-task.md"): "---\nid: a-task\npriority: 5\n---\n# A task\n",
		filepath.Join(tasksDir, queue.DirBacklog, "m-task.md"): "---\nid: m-task\npriority: 1\n---\n# M task\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	// Should have 3 runnable backlog entries.
	if len(result.RunnableBacklog) != 3 {
		t.Fatalf("expected 3 runnable backlog tasks, got %d", len(result.RunnableBacklog))
	}

	// Order: m-task (priority 1), a-task (priority 5, alpha first), z-task (priority 5).
	if result.RunnableBacklog[0].Name != "m-task.md" {
		t.Errorf("expected first task 'm-task.md', got %q", result.RunnableBacklog[0].Name)
	}
	if result.RunnableBacklog[1].Name != "a-task.md" {
		t.Errorf("expected second task 'a-task.md', got %q", result.RunnableBacklog[1].Name)
	}
	if result.RunnableBacklog[2].Name != "z-task.md" {
		t.Errorf("expected third task 'z-task.md', got %q", result.RunnableBacklog[2].Name)
	}
}

// extractSection returns the text between two section headers in the output.
func extractSection(output, startHeader, endHeader string) string {
	startIdx := strings.Index(output, startHeader)
	if startIdx < 0 {
		return ""
	}
	rest := output[startIdx:]
	if endHeader == "" {
		return rest
	}
	endIdx := strings.Index(rest, endHeader)
	if endIdx < 0 {
		return rest
	}
	return rest[:endIdx]
}

func TestShowJSON_ValidOutput(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	files := map[string]string{
		filepath.Join(tasksDir, queue.DirWaiting, "refactor-api.md"):       "---\nid: refactor-api\ndepends_on: [setup-models]\n---\n# Refactor the API layer\n",
		filepath.Join(tasksDir, queue.DirBacklog, "add-auth.md"):           "---\nid: add-auth\npriority: 10\n---\n# Add authentication\n",
		filepath.Join(tasksDir, queue.DirInProgress, "agent-task.md"):      "<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: agent-task\n---\n# In progress task\n",
		filepath.Join(tasksDir, queue.DirReadyReview, "review-feature.md"): "<!-- branch: task/review-feature -->\n---\npriority: 10\n---\n# Review this feature\n",
		filepath.Join(tasksDir, queue.DirReadyMerge, "merge-me.md"):        "---\npriority: 5\n---\n# Ready to merge\n",
		filepath.Join(tasksDir, queue.DirCompleted, "setup-models.md"):     "---\nid: setup-models\n---\n# Setup models\n",
		filepath.Join(tasksDir, queue.DirFailed, "failed-task.md"):         "<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=tests failed files_changed=none -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z step=WORK error=merge conflict files_changed=none -->\n---\nid: failed-task\nmax_retries: 2\n---\n# A failed task\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", path, err)
		}
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	// Verify it is valid JSON.
	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	// Verify expected top-level keys.
	if result.Counts == nil {
		t.Fatal("counts should not be nil")
	}
	if result.Counts["in_progress"] != 1 {
		t.Errorf("expected 1 in-progress, got %d", result.Counts["in_progress"])
	}
	if result.Counts["backlog"] != 1 {
		t.Errorf("expected 1 backlog, got %d", result.Counts["backlog"])
	}
	if result.Counts["failed"] != 1 {
		t.Errorf("expected 1 failed, got %d", result.Counts["failed"])
	}
	if result.Counts["waiting"] != 1 {
		t.Errorf("expected 1 waiting, got %d", result.Counts["waiting"])
	}
	if result.Counts["blocked"] != 1 {
		t.Errorf("expected 1 blocked, got %d", result.Counts["blocked"])
	}
	if result.MergeQueue != "idle" {
		t.Errorf("expected merge_queue idle, got %s", result.MergeQueue)
	}

	// Verify in-progress tasks.
	if len(result.InProgress) != 1 {
		t.Fatalf("expected 1 in-progress task, got %d", len(result.InProgress))
	}
	if result.InProgress[0].Title != "In progress task" {
		t.Errorf("expected title 'In progress task', got %q", result.InProgress[0].Title)
	}
	if result.InProgress[0].ClaimedBy != "abc123" {
		t.Errorf("expected claimed_by 'abc123', got %q", result.InProgress[0].ClaimedBy)
	}

	// Verify ready-for-review tasks.
	if len(result.ReadyReview) != 1 {
		t.Fatalf("expected 1 ready-for-review task, got %d", len(result.ReadyReview))
	}
	if result.ReadyReview[0].Branch != "task/review-feature" {
		t.Errorf("expected branch 'task/review-feature', got %q", result.ReadyReview[0].Branch)
	}

	// Verify ready-to-merge tasks.
	if len(result.ReadyMerge) != 1 {
		t.Fatalf("expected 1 ready-to-merge task, got %d", len(result.ReadyMerge))
	}
	if result.ReadyMerge[0].Title != "Ready to merge" {
		t.Errorf("expected title 'Ready to merge', got %q", result.ReadyMerge[0].Title)
	}

	// Verify waiting tasks with dependency info.
	if len(result.Waiting) != 1 {
		t.Fatalf("expected 1 waiting task, got %d", len(result.Waiting))
	}
	if result.Waiting[0].State != queue.DirWaiting {
		t.Errorf("expected waiting task state %q, got %q", queue.DirWaiting, result.Waiting[0].State)
	}
	if result.Waiting[0].Title != "Refactor the API layer" {
		t.Errorf("expected title 'Refactor the API layer', got %q", result.Waiting[0].Title)
	}
	if len(result.Waiting[0].Dependencies) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(result.Waiting[0].Dependencies))
	}
	if result.Waiting[0].Dependencies[0].ID != "setup-models" {
		t.Errorf("expected dependency ID 'setup-models', got %q", result.Waiting[0].Dependencies[0].ID)
	}
	if result.Waiting[0].Dependencies[0].Status != queue.DirCompleted {
		t.Errorf("expected dependency status %q, got %q", queue.DirCompleted, result.Waiting[0].Dependencies[0].Status)
	}

	// Verify failed tasks.
	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	if result.Failed[0].FailCount != 2 {
		t.Errorf("expected fail_count 2, got %d", result.Failed[0].FailCount)
	}
	if result.Failed[0].MaxRetries != 2 {
		t.Errorf("expected max_retries 2, got %d", result.Failed[0].MaxRetries)
	}
	if result.Failed[0].FailureKind != "retry" {
		t.Errorf("expected failure_kind 'retry', got %q", result.Failed[0].FailureKind)
	}

	// Verify runnable backlog.
	if len(result.RunnableBacklog) != 1 {
		t.Fatalf("expected 1 runnable backlog task, got %d", len(result.RunnableBacklog))
	}
	if result.RunnableBacklog[0].Name != "add-auth.md" {
		t.Errorf("expected runnable backlog task 'add-auth.md', got %q", result.RunnableBacklog[0].Name)
	}
	if result.RunnableBacklog[0].Title != "Add authentication" {
		t.Errorf("expected title 'Add authentication', got %q", result.RunnableBacklog[0].Title)
	}
	if result.RunnableBacklog[0].Priority != 10 {
		t.Errorf("expected priority 10, got %d", result.RunnableBacklog[0].Priority)
	}
}

func TestShowJSON_BacklogDependencyBlockedAppearsInWaiting(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "blocked.md"), []byte("---\nid: blocked\ndepends_on: [missing]\npriority: 10\n---\n# Blocked\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocked): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "runnable.md"), []byte("---\nid: runnable\npriority: 20\n---\n# Runnable\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(runnable): %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	if len(result.RunnableBacklog) != 1 || result.RunnableBacklog[0].Name != "runnable.md" {
		t.Fatalf("RunnableBacklog = %#v, want only runnable.md", result.RunnableBacklog)
	}
	if len(result.Waiting) != 1 || result.Waiting[0].Name != "blocked.md" {
		t.Fatalf("Waiting = %#v, want blocked.md", result.Waiting)
	}
	if result.Counts["waiting"] != 0 {
		t.Fatalf("Counts[waiting] = %d, want 0 physical waiting/ tasks", result.Counts["waiting"])
	}
	if result.Counts["blocked"] != 1 {
		t.Fatalf("Counts[blocked] = %d, want 1 dependency-blocked task", result.Counts["blocked"])
	}
	if result.Waiting[0].State != queue.DirBacklog {
		t.Fatalf("Waiting[0].State = %q, want %q", result.Waiting[0].State, queue.DirBacklog)
	}
}

func TestShowJSON_EmptyTasksDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	// All lists should be empty but non-nil.
	if result.ActiveAgents == nil {
		t.Error("active_agents should not be nil")
	}
	if len(result.ActiveAgents) != 0 {
		t.Errorf("expected 0 active agents, got %d", len(result.ActiveAgents))
	}
	if len(result.InProgress) != 0 {
		t.Errorf("expected 0 in-progress tasks, got %d", len(result.InProgress))
	}
	if result.Counts["in_progress"] != 0 {
		t.Errorf("expected in_progress count 0, got %d", result.Counts["in_progress"])
	}
	if len(result.RunnableBacklog) != 0 {
		t.Errorf("expected 0 runnable backlog tasks, got %d", len(result.RunnableBacklog))
	}
}

func TestShowJSON_WriterError(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	writeErr := errors.New("disk full")
	fw := &failWriter{err: writeErr}

	err := ShowJSON(fw, repoRoot)
	if err == nil {
		t.Fatal("ShowJSON should return an error when the writer fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("ShowJSON error should wrap the write error, got: %v", err)
	}
}

func TestShowJSON_FailedLastReason(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Two failure records — last_reason should be from the most recent one.
	content := "<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=tests failed files_changed=none -->\n" +
		"<!-- failure: agent-2 at 2026-01-01T00:02:00Z step=COMMIT error=merge conflict files_changed=api.go -->\n" +
		"---\nid: broken-task\nmax_retries: 3\n---\n# A broken task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "broken-task.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	ft := result.Failed[0]
	if ft.Name != "broken-task.md" {
		t.Errorf("expected name 'broken-task.md', got %q", ft.Name)
	}
	if ft.Title != "A broken task" {
		t.Errorf("expected title 'A broken task', got %q", ft.Title)
	}
	if ft.FailCount != 2 {
		t.Errorf("expected fail_count 2, got %d", ft.FailCount)
	}
	if ft.MaxRetries != 3 {
		t.Errorf("expected max_retries 3, got %d", ft.MaxRetries)
	}
	if ft.LastReason != "merge conflict" {
		t.Errorf("expected last_reason 'merge conflict', got %q", ft.LastReason)
	}
	if ft.FailureKind != "retry" {
		t.Errorf("expected failure_kind 'retry', got %q", ft.FailureKind)
	}
}

func TestShowJSON_TerminalFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	content := "<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\n---\nid: terminal-task\nmax_retries: 3\n---\n# Terminal task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "terminal-task.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	ft := result.Failed[0]
	if ft.FailureKind != "terminal" {
		t.Errorf("expected failure_kind 'terminal', got %q", ft.FailureKind)
	}
	if ft.TerminalReason != "unparseable frontmatter" {
		t.Errorf("expected terminal_reason 'unparseable frontmatter', got %q", ft.TerminalReason)
	}
	if ft.FailCount != 0 {
		t.Errorf("expected fail_count 0, got %d", ft.FailCount)
	}
}

func TestShowJSON_ParseFailedTerminalFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	content := "<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\n---\npriority: nope\n---\n# Broken terminal task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "broken-terminal.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	ft := result.Failed[0]
	if ft.Name != "broken-terminal.md" {
		t.Errorf("expected name 'broken-terminal.md', got %q", ft.Name)
	}
	if ft.FailureKind != "terminal" {
		t.Errorf("expected failure_kind 'terminal', got %q", ft.FailureKind)
	}
	if ft.TerminalReason != "unparseable frontmatter" {
		t.Errorf("expected terminal_reason 'unparseable frontmatter', got %q", ft.TerminalReason)
	}
}

func TestShow_ParseFailedInProgressTaskPreservesClaimMetadata(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	content := "<!-- claimed-by: test-agent  claimed-at: 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n# Broken active task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "broken-active.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	output := captureShowVerbose(t, repoRoot)
	if !contains(output, "broken-active.md") {
		t.Errorf("output should contain parse-failed task name, got:\n%s", output)
	}
	if !contains(output, "agent test-agent") {
		t.Errorf("output should preserve claimed-by metadata, got:\n%s", output)
	}
	if !contains(output, "In-Progress Tasks") {
		t.Errorf("output should contain in-progress section, got:\n%s", output)
	}
}

func TestShowJSON_WarningsIncluded(t *testing.T) {
	result := statusDataToJSON(statusData{warnings: []string{"could not read agent presence: boom"}})
	if len(result.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(result.Warnings))
	}
	if result.Warnings[0] != "could not read agent presence: boom" {
		t.Errorf("warning = %q, want %q", result.Warnings[0], "could not read agent presence: boom")
	}
}

func TestShowJSON_PauseState(t *testing.T) {
	result := statusDataToJSON(statusData{pauseState: pause.State{Active: true, Since: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)}})
	if !result.Paused.Active {
		t.Fatal("Paused.Active = false, want true")
	}
	if result.Paused.Since != "2026-03-23T10:00:00Z" {
		t.Fatalf("Paused.Since = %q", result.Paused.Since)
	}

	problem := statusDataToJSON(statusData{pauseState: pause.State{Active: true, ProblemKind: pause.ProblemMalformed, Problem: `invalid timestamp: "bad"`}})
	if !problem.Paused.Active {
		t.Fatal("problem pause should stay active")
	}
	if problem.Paused.Since != "" {
		t.Fatalf("Paused.Since = %q, want empty for problem state", problem.Paused.Since)
	}

	unpaused := statusDataToJSON(statusData{})
	if unpaused.Paused.Active {
		t.Fatal("Paused.Active = true, want false")
	}
}

func TestShowJSON_CycleFailureKind(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	content := "<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n---\nid: cycle-task\nmax_retries: 3\n---\n# Cycle task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "cycle-task.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	ft := result.Failed[0]
	if ft.FailureKind != "cycle" {
		t.Errorf("expected failure_kind 'cycle', got %q", ft.FailureKind)
	}
	if ft.CycleReason != "circular dependency" {
		t.Errorf("expected cycle_reason 'circular dependency', got %q", ft.CycleReason)
	}
}

func TestShowJSON_CancelledFailureKind(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	content := "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: cancelled-task\nmax_retries: 3\n---\n# Cancelled task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "cancelled-task.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var buf bytes.Buffer
	if err := ShowJSON(&buf, repoRoot); err != nil {
		t.Fatalf("ShowJSON: %v", err)
	}

	var result StatusJSON
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("JSON unmarshal failed: %v\nraw output:\n%s", err, buf.String())
	}

	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	if result.Failed[0].FailureKind != "cancelled" {
		t.Fatalf("expected failure_kind 'cancelled', got %q", result.Failed[0].FailureKind)
	}
}

func TestShowJSON_CancelledPrecedence(t *testing.T) {
	result := statusDataToJSON(statusData{
		failedTasks: []taskEntry{{
			name:                      "cancelled.md",
			cancelled:                 true,
			failureCount:              2,
			maxRetries:                3,
			lastFailureReason:         "tests failed",
			lastCycleFailureReason:    "circular dependency",
			lastTerminalFailureReason: "invalid glob",
		}},
	})
	if len(result.Failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(result.Failed))
	}
	if result.Failed[0].FailureKind != "cancelled" {
		t.Fatalf("expected failure_kind 'cancelled', got %q", result.Failed[0].FailureKind)
	}
}

func TestShow_CancelledParseFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")
	for _, sub := range []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	content := "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n# Broken cancelled task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "broken-cancelled.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	output := captureShowVerbose(t, repoRoot)
	if !contains(output, "broken-cancelled.md") || !contains(output, "(cancelled)") {
		t.Fatalf("expected cancelled parse failure in output, got:\n%s", output)
	}
}

func TestShow_UninitializedRepo(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	var buf bytes.Buffer
	err := ShowTo(&buf, repoRoot)
	if err == nil {
		t.Fatal("expected error for uninitialized repo, got nil")
	}
	if !strings.Contains(err.Error(), "mato init") {
		t.Fatalf("expected error to mention 'mato init', got: %v", err)
	}
}

func TestShowJSON_UninitializedRepo(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	var buf bytes.Buffer
	err := ShowJSON(&buf, repoRoot)
	if err == nil {
		t.Fatal("expected error for uninitialized repo, got nil")
	}
	if !strings.Contains(err.Error(), "mato init") {
		t.Fatalf("expected error to mention 'mato init', got: %v", err)
	}
}
