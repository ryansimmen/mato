package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func setupTestRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if _, err := gitOutput(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := gitOutput(dir, "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config user.email: %v", err)
	}
	if _, err := gitOutput(dir, "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config user.name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md: %v", err)
	}
	if _, err := gitOutput(dir, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := gitOutput(dir, "commit", "-m", "initial"); err != nil {
		t.Fatalf("git commit initial: %v", err)
	}
	return dir
}

func setupTasksDir(t *testing.T) string {
	t.Helper()

	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "ready-to-merge", "completed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	return tasksDir
}

func TestAcquireMergeLockSucceedsWithoutExistingLock(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, ok := acquireMergeLock(tasksDir)
	if !ok || cleanup == nil {
		t.Fatal("expected merge lock acquisition to succeed")
	}
	cleanup()
}

func TestAcquireMergeLockFailsWhenHeldByActiveProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "merge.lock"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("os.WriteFile merge.lock: %v", err)
	}

	cleanup, ok := acquireMergeLock(tasksDir)
	if ok || cleanup != nil {
		t.Fatal("expected merge lock acquisition to fail while active holder exists")
	}
}

func TestAcquireMergeLockSucceedsWhenHeldByDeadProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "merge.lock"), []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("os.WriteFile merge.lock: %v", err)
	}

	cleanup, ok := acquireMergeLock(tasksDir)
	if !ok || cleanup == nil {
		t.Fatal("expected merge lock acquisition to succeed after removing stale lock")
	}
	data, err := os.ReadFile(filepath.Join(locksDir, "merge.lock"))
	if err != nil {
		t.Fatalf("os.ReadFile merge.lock: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("merge lock PID = %q, want %q", got, strconv.Itoa(os.Getpid()))
	}
	cleanup()
}

func TestAcquireMergeLockCleanupRemovesLockFile(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, ok := acquireMergeLock(tasksDir)
	if !ok || cleanup == nil {
		t.Fatal("expected merge lock acquisition to succeed")
	}
	cleanup()

	if _, err := os.Stat(filepath.Join(tasksDir, ".locks", "merge.lock")); !os.IsNotExist(err) {
		t.Fatalf("merge lock should be removed by cleanup, stat err = %v", err)
	}
}

func TestProcessMergeQueueEmptyReadyToMergeReturnsZero(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := gitOutput(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := gitOutput(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	if got := processMergeQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("processMergeQueue() = %d, want 0", got)
	}
}

func TestProcessMergeQueueMergesReadyTaskBranch(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := gitOutput(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := gitOutput(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := gitOutput(repoRoot, "checkout", "-b", "task/add-feature", "mato"); err != nil {
		t.Fatalf("git checkout task/add-feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile feature.txt: %v", err)
	}
	if _, err := gitOutput(repoRoot, "add", "feature.txt"); err != nil {
		t.Fatalf("git add feature.txt: %v", err)
	}
	if _, err := gitOutput(repoRoot, "commit", "-m", "feature work"); err != nil {
		t.Fatalf("git commit feature work: %v", err)
	}
	if _, err := gitOutput(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "add feature!!.md")
	taskContent := "---\npriority: 5\n---\n<!-- claimed-by: agent123  claimed-at: 2026-01-01T00:00:00Z -->\n# Add feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := processMergeQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("processMergeQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "completed", "add feature!!.md")); err != nil {
		t.Fatalf("completed task file missing: %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task file should be moved, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "feature.txt")); err != nil {
		t.Fatalf("merged feature file missing from target branch: %v", err)
	}
	msg, err := gitOutput(repoRoot, "log", "--format=%s", "-1", "mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	if got := strings.TrimSpace(msg); got != "Add feature" {
		t.Fatalf("merge commit message = %q, want %q", got, "Add feature")
	}
}

func TestProcessMergeQueueMovesConflictedTaskBackToBacklog(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := gitOutput(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := gitOutput(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	baseFile := filepath.Join(repoRoot, "README.md")
	if err := os.WriteFile(baseFile, []byte("shared\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md base: %v", err)
	}
	if _, err := gitOutput(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md base: %v", err)
	}
	if _, err := gitOutput(repoRoot, "commit", "-m", "prepare conflict base"); err != nil {
		t.Fatalf("git commit prepare conflict base: %v", err)
	}
	if _, err := gitOutput(repoRoot, "checkout", "-b", "task/conflict-task", "mato"); err != nil {
		t.Fatalf("git checkout task/conflict-task: %v", err)
	}
	if err := os.WriteFile(baseFile, []byte("task branch change\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile task branch README.md: %v", err)
	}
	if _, err := gitOutput(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md task branch: %v", err)
	}
	if _, err := gitOutput(repoRoot, "commit", "-m", "task branch change"); err != nil {
		t.Fatalf("git commit task branch change: %v", err)
	}
	if _, err := gitOutput(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}
	if err := os.WriteFile(baseFile, []byte("target branch change\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile target branch README.md: %v", err)
	}
	if _, err := gitOutput(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md target branch: %v", err)
	}
	if _, err := gitOutput(repoRoot, "commit", "-m", "target branch change"); err != nil {
		t.Fatalf("git commit target branch change: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "conflict task.md")
	taskContent := "---\npriority: 1\n---\n# Conflict task\nThis should conflict.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := processMergeQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("processMergeQueue() = %d, want 0", got)
	}
	backlogFile := filepath.Join(tasksDir, "backlog", "conflict task.md")
	data, err := os.ReadFile(backlogFile)
	if err != nil {
		t.Fatalf("backlog task file missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: merge-queue") {
		t.Fatalf("backlog task should contain merge failure record, got %q", string(data))
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task file should be moved back to backlog, stat err = %v", err)
	}
}
