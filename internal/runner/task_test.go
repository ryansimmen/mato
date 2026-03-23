package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/frontmatter"
	"mato/internal/messaging"
	"mato/internal/queue"
)

func TestMoveTaskToReviewWithMarker_Success(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "marker-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Marker Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/marker-task",
		Title:    "Marker Task",
		TaskPath: inProgressPath,
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/marker-task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Task should be in ready-for-review/.
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("task should be in ready-for-review/: %v", statErr)
	}

	// Task should not be in in-progress/.
	if _, statErr := os.Stat(inProgressPath); statErr == nil {
		t.Fatal("task should not remain in in-progress/")
	}

	// Branch marker should be written.
	data, _ := os.ReadFile(readyPath)
	if !strings.Contains(string(data), "<!-- branch: task/marker-task -->") {
		t.Fatalf("branch marker not found in moved file, got:\n%s", string(data))
	}
}

func TestMoveTaskToReviewWithMarker_SourceMissing(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)

	claimed := &queue.ClaimedTask{
		Filename: "missing.md",
		TaskPath: filepath.Join(tasksDir, queue.DirInProgress, "missing.md"),
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/missing")
	if err == nil {
		t.Fatal("expected error when source file doesn't exist")
	}
}

func TestMoveTaskToReviewWithMarker_DestinationExists(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "dup.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Dup\n"), 0o644)

	// Pre-create at destination.
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(readyPath, []byte("# Existing\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		TaskPath: inProgressPath,
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/dup")
	if err == nil {
		t.Fatal("expected error when destination already exists")
	}

	// Source should still exist.
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("source should still exist: %v", statErr)
	}
}

func TestMoveTaskToReviewWithMarker_AppendFailsRollback(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "rollback.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Rollback\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		TaskPath: inProgressPath,
	}

	origAppend := appendToFileFn
	t.Cleanup(func() { appendToFileFn = origAppend })
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated write error")
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/rollback")
	if err == nil {
		t.Fatal("expected error when append fails")
	}
	if !strings.Contains(err.Error(), "rolled back to in-progress") {
		t.Fatalf("error should mention rollback, got: %v", err)
	}

	// File should be back in in-progress/.
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("file should be rolled back to in-progress/: %v", statErr)
	}
	// File should NOT be in ready-for-review/.
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatal("file should not remain in ready-for-review/ after rollback")
	}
}

func TestPostAgentPush_TaskAlreadyGone(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task file does NOT exist (already moved).
	claimed := &queue.ClaimedTask{
		Filename: "gone-task.md",
		Branch:   "task/gone-task",
		Title:    "Gone Task",
		TaskPath: filepath.Join(tasksDir, queue.DirInProgress, "gone-task.md"),
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	err := postAgentPush(env, "agent1", claimed, t.TempDir())
	if err != nil {
		t.Fatalf("expected nil when task is already gone, got: %v", err)
	}
}

func TestPostAgentPush_NoCommits(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "no-commits.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# No Commits\n"), 0o644)

	// Set up git repo with no commits above main.
	cloneDir := t.TempDir()
	gitRun := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = cloneDir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	gitRun("git", "init", "-b", "main")
	gitRun("git", "config", "user.name", "test")
	gitRun("git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	gitRun("git", "add", ".")
	gitRun("git", "commit", "-m", "init")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/no-commits",
		Title:    "No Commits",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	err := postAgentPush(env, "agent1", claimed, cloneDir)
	if err != nil {
		t.Fatalf("expected nil when no commits exist, got: %v", err)
	}

	// Task should still be in in-progress/ (not moved).
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatal("task should remain in in-progress/ when no commits made")
	}
}

func TestAgentWroteFailureRecord_MultipleAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")

	content := "# Task\n" +
		"<!-- failure: agent-a at 2026-01-01T00:00:00Z step=WORK error=test1 -->\n" +
		"<!-- failure: agent-b at 2026-01-02T00:00:00Z step=WORK error=test2 -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	if !agentWroteFailureRecord(path, "agent-a") {
		t.Fatal("should find agent-a's failure record")
	}
	if !agentWroteFailureRecord(path, "agent-b") {
		t.Fatal("should find agent-b's failure record")
	}
	if agentWroteFailureRecord(path, "agent-c") {
		t.Fatal("should not find agent-c's failure record")
	}
}

func TestExtractFailureLines_WithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := "---\npriority: 10\n---\n# Task\nBody text\n" +
		"<!-- failure: agent1 at 2026-01-01T00:00:00Z step=WORK error=tests_failed files_changed=foo.go -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	result := extractFailureLines(path)
	if !strings.Contains(result, "failure: agent1") {
		t.Fatalf("expected failure line, got %q", result)
	}
}

func TestExtractFailureLines_MultipleDistinctFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := "# Task\n" +
		"<!-- failure: agent1 at 2026-01-01T00:00:00Z step=WORK error=build_failed files_changed=a.go -->\n" +
		"<!-- failure: agent2 at 2026-01-02T00:00:00Z step=COMMIT error=no_changes files_changed=none -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	result := extractFailureLines(path)
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 failure lines, got %d: %q", len(lines), result)
	}
}

func TestWriteDependencyContextFile_InvalidFrontmatter(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirInProgress), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "bad-frontmatter.md"
	taskPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	// Invalid YAML frontmatter.
	os.WriteFile(taskPath, []byte("---\n: invalid yaml\n---\n# Bad Frontmatter\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/bad",
		Title:    "Bad",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty string for invalid frontmatter, got %q", result)
	}
}

func TestWriteDependencyContextFile_DepsButNoCompletions(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirInProgress), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "with-deps.md"
	taskPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - nonexistent-dep\n---\n# With Deps\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/with-deps",
		Title:    "With Deps",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty string when no completion files exist, got %q", result)
	}
}

func TestWriteDependencyContextFile_WithMatchingCompletion(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirInProgress), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a completion detail file for the dependency.
	detail := messaging.CompletionDetail{
		TaskID:   "dep-task",
		TaskFile: "dep-task.md",
		Branch:   "task/dep-task",
		Title:    "Dep Task",
		CommitSHA: "abc123",
	}
	if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("write completion detail: %v", err)
	}

	taskFile := "depends-on-dep.md"
	taskPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - dep-task\n---\n# Depends On Dep\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/depends-on-dep",
		Title:    "Depends On Dep",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result == "" {
		t.Fatal("expected non-empty path for matching completion")
	}

	// Verify the file exists and contains expected data.
	fileData, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("could not read dependency context file: %v", err)
	}
	if !strings.Contains(string(fileData), "dep-task.md") {
		t.Fatalf("dependency context should contain dep task info, got:\n%s", string(fileData))
	}
}

func TestRemoveDependencyContextFile_ExistingFile(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "messages"), 0o755)

	filename := "task.md"
	depPath := filepath.Join(tasksDir, "messages", "dependency-context-"+filename+".json")
	os.WriteFile(depPath, []byte("[]"), 0o644)

	removeDependencyContextFile(tasksDir, filename)

	if _, err := os.Stat(depPath); !os.IsNotExist(err) {
		t.Fatal("dependency context file should be removed")
	}
}

func TestRemoveDependencyContextFile_NonexistentFile(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "messages"), 0o755)

	// Should not panic or error.
	removeDependencyContextFile(tasksDir, "nonexistent.md")
}

func TestRecoverStuckTask_AppendsFailureWithTimestamp(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "timestamp-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Timestamp Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/timestamp",
		Title:    "Timestamp Task",
		TaskPath: inProgressPath,
	}

	captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent-x", claimed)
	})

	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task not found in backlog/: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "<!-- failure: agent-x at") {
		t.Fatal("failure record should contain agent ID and timestamp")
	}
	if !strings.Contains(content, "agent container exited without cleanup") {
		t.Fatal("failure record should contain generic message")
	}
}

func TestExtractReviewRejections_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte(""), 0o644)

	result := extractReviewRejections(path)
	if result != "" {
		t.Fatalf("expected empty string for empty file, got %q", result)
	}
}

func TestExtractReviewRejections_MissingFile(t *testing.T) {
	result := extractReviewRejections(filepath.Join(t.TempDir(), "nope.md"))
	if result != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", result)
	}
}

func TestSelectTaskForReview_ReturnsHighestPriority(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "low.md"),
		[]byte("---\npriority: 50\nmax_retries: 3\n---\n# Low\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "high.md"),
		[]byte("---\npriority: 5\nmax_retries: 3\n---\n# High\n"), 0o644)

	task := selectTaskForReview(tasksDir, nil)
	if task == nil {
		t.Fatal("expected a task to be selected")
	}
	if task.Filename != "high.md" {
		t.Fatalf("expected highest priority task (high.md), got %q", task.Filename)
	}
}

func TestSelectTaskForReview_NilIndex(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, queue.DirFailed), 0o755)

	// No tasks.
	task := selectTaskForReview(tasksDir, nil)
	if task != nil {
		t.Fatal("expected nil when no tasks available")
	}
}

func TestReviewCandidates_FilesystemFallback_SkipsParseErrors(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Valid task.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "good.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Good\n"), 0o644)
	// Invalid frontmatter task.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "bad.md"),
		[]byte("---\n: invalid\n---\n# Bad\n"), 0o644)

	_, stderr := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate (skipping bad parse), got %d", len(candidates))
		}
		if candidates[0].Filename != "good.md" {
			t.Fatalf("expected good.md, got %q", candidates[0].Filename)
		}
	})

	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected parse warning in stderr, got:\n%s", stderr)
	}
}

func TestReviewCandidates_FilesystemFallback_BranchGeneratedWhenMissing(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task without a branch marker in file.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "no-branch.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# No Branch\n"), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	expected := "task/" + frontmatter.SanitizeBranchName("no-branch.md")
	if candidates[0].Branch != expected {
		t.Fatalf("expected generated branch %q, got %q", expected, candidates[0].Branch)
	}
}
