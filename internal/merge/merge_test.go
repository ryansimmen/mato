package merge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mato/internal/git"
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

func setupTasksDir(t *testing.T) string {
	t.Helper()

	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "ready-to-merge", "completed", "failed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	return tasksDir
}

func TestAcquireLockSucceedsWithoutExistingLock(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, ok := AcquireLock(tasksDir)
	if !ok || cleanup == nil {
		t.Fatal("expected merge lock acquisition to succeed")
	}
	cleanup()
}

func TestAcquireLockFailsWhenHeldByActiveProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "merge.lock"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("os.WriteFile merge.lock: %v", err)
	}

	cleanup, ok := AcquireLock(tasksDir)
	if ok || cleanup != nil {
		t.Fatal("expected merge lock acquisition to fail while active holder exists")
	}
}

func TestAcquireLockSucceedsWhenHeldByDeadProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "merge.lock"), []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("os.WriteFile merge.lock: %v", err)
	}

	cleanup, ok := AcquireLock(tasksDir)
	if !ok || cleanup == nil {
		t.Fatal("expected merge lock acquisition to succeed after removing stale lock")
	}
	data, err := os.ReadFile(filepath.Join(locksDir, "merge.lock"))
	if err != nil {
		t.Fatalf("os.ReadFile merge.lock: %v", err)
	}
	if got := strings.TrimSpace(string(data)); !strings.HasPrefix(got, strconv.Itoa(os.Getpid())) {
		t.Fatalf("merge lock identity = %q, want prefix %q", got, strconv.Itoa(os.Getpid()))
	}
	cleanup()
}

func TestAcquireLockCleanupRemovesLockFile(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, ok := AcquireLock(tasksDir)
	if !ok || cleanup == nil {
		t.Fatal("expected merge lock acquisition to succeed")
	}
	cleanup()

	if _, err := os.Stat(filepath.Join(tasksDir, ".locks", "merge.lock")); !os.IsNotExist(err) {
		t.Fatalf("merge lock should be removed by cleanup, stat err = %v", err)
	}
}

func TestMoveTaskFile_DestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "ready-to-merge", "task.md")
	dst := filepath.Join(dir, "backlog", "task.md")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("os.MkdirAll src dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("os.MkdirAll dst dir: %v", err)
	}
	if err := os.WriteFile(src, []byte("# Ready task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("# Existing backlog task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile dst: %v", err)
	}

	err := moveTaskFile(src, dst)
	if err == nil {
		t.Fatal("moveTaskFile should fail when destination exists")
	}
	if !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("moveTaskFile error = %q, want destination already exists", err)
	}
	if data, readErr := os.ReadFile(dst); readErr != nil {
		t.Fatalf("os.ReadFile dst: %v", readErr)
	} else if string(data) != "# Existing backlog task\n" {
		t.Fatalf("destination contents changed: got %q", string(data))
	}
	if data, readErr := os.ReadFile(src); readErr != nil {
		t.Fatalf("os.ReadFile src: %v", readErr)
	} else if string(data) != "# Ready task\n" {
		t.Fatalf("source contents changed: got %q", string(data))
	}
}

func TestProcessQueueEmptyReadyToMergeReturnsZero(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}
}

func TestProcessQueueMergesReadyTaskBranch(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/add-feature", "mato"); err != nil {
		t.Fatalf("git checkout task/add-feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "feature.txt"); err != nil {
		t.Fatalf("git add feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "feature work"); err != nil {
		t.Fatalf("git commit feature work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "add feature!!.md")
	taskContent := "---\npriority: 5\n---\n<!-- claimed-by: agent123  claimed-at: 2026-01-01T00:00:00Z -->\n# Add feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
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
	msg, err := git.Output(repoRoot, "log", "--format=%s", "-1", "mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	if got := strings.TrimSpace(msg); got != "Add feature" {
		t.Fatalf("merge commit message = %q, want %q", got, "Add feature")
	}
}

func TestProcessQueue_UsesBranchFromTaskFile(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskName := "custom branch.md"
	derivedBranch := "task/" + sanitizeBranchName(taskName)
	actualBranch := "task/custom-branch-actual"

	if _, err := git.Output(repoRoot, "checkout", "-b", derivedBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", derivedBranch, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "wrong.txt"), []byte("wrong branch\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile wrong.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "wrong.txt"); err != nil {
		t.Fatalf("git add wrong.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "wrong branch work"); err != nil {
		t.Fatalf("git commit wrong branch work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato after wrong branch: %v", err)
	}

	if _, err := git.Output(repoRoot, "checkout", "-b", actualBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", actualBranch, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "right.txt"), []byte("actual branch\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile right.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "right.txt"); err != nil {
		t.Fatalf("git add right.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "actual branch work"); err != nil {
		t.Fatalf("git commit actual branch work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato after actual branch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", taskName)
	taskContent := "---\npriority: 5\n---\n<!-- branch: " + actualBranch + " -->\n# Use actual branch\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "right.txt")); err != nil {
		t.Fatalf("expected actual branch change to merge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "wrong.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected filename-derived branch to be ignored, stat err = %v", err)
	}
}

func TestProcessQueue_FallbackToDerivedBranch(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskName := "fallback branch.md"
	derivedBranch := "task/" + sanitizeBranchName(taskName)
	if _, err := git.Output(repoRoot, "checkout", "-b", derivedBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", derivedBranch, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "fallback.txt"), []byte("fallback branch\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile fallback.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "fallback.txt"); err != nil {
		t.Fatalf("git add fallback.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "fallback branch work"); err != nil {
		t.Fatalf("git commit fallback branch work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", taskName)
	taskContent := "---\npriority: 5\n---\n# Fallback branch\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "fallback.txt")); err != nil {
		t.Fatalf("expected derived branch change to merge: %v", err)
	}
}

func TestProcessQueueMovesConflictedTaskBackToBacklog(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	baseFile := filepath.Join(repoRoot, "README.md")
	if err := os.WriteFile(baseFile, []byte("shared\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md base: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md base: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "prepare conflict base"); err != nil {
		t.Fatalf("git commit prepare conflict base: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/conflict-task", "mato"); err != nil {
		t.Fatalf("git checkout task/conflict-task: %v", err)
	}
	if err := os.WriteFile(baseFile, []byte("task branch change\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile task branch README.md: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md task branch: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "task branch change"); err != nil {
		t.Fatalf("git commit task branch change: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}
	if err := os.WriteFile(baseFile, []byte("target branch change\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile target branch README.md: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md target branch: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "target branch change"); err != nil {
		t.Fatalf("git commit target branch change: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "conflict task.md")
	taskContent := "---\npriority: 1\n---\n# Conflict task\nThis should conflict.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
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

func TestProcessQueue_CleansRemoteBranchOnConflictRequeue(t *testing.T) {
	repoRoot := setupTestRepo(t)
	originDir := filepath.Join(t.TempDir(), "origin.git")
	if _, err := git.Output("", "init", "--bare", originDir); err != nil {
		t.Fatalf("git init --bare origin: %v", err)
	}
	if _, err := git.Output(repoRoot, "remote", "add", "origin", originDir); err != nil {
		t.Fatalf("git remote add origin: %v", err)
	}
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	baseFile := filepath.Join(repoRoot, "README.md")
	if err := os.WriteFile(baseFile, []byte("shared\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md base: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md base: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "prepare shared base"); err != nil {
		t.Fatalf("git commit prepare shared base: %v", err)
	}
	if _, err := git.Output(repoRoot, "push", "-u", "origin", "mato"); err != nil {
		t.Fatalf("git push -u origin mato: %v", err)
	}

	firstTaskName := "first task.md"
	firstBranch := "task/" + sanitizeBranchName(firstTaskName)
	if _, err := git.Output(repoRoot, "checkout", "-b", firstBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", firstBranch, err)
	}
	if err := os.WriteFile(baseFile, []byte("first branch change\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md first branch: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md first branch: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "first branch change"); err != nil {
		t.Fatalf("git commit first branch change: %v", err)
	}
	if _, err := git.Output(repoRoot, "push", "-u", "origin", firstBranch); err != nil {
		t.Fatalf("git push -u origin %s: %v", firstBranch, err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato after first branch: %v", err)
	}

	conflictTaskName := "conflict task.md"
	conflictBranch := "task/conflict-task-actual"
	if _, err := git.Output(repoRoot, "checkout", "-b", conflictBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", conflictBranch, err)
	}
	if err := os.WriteFile(baseFile, []byte("conflict branch change\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md conflict branch: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md conflict branch: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "conflict branch change"); err != nil {
		t.Fatalf("git commit conflict branch change: %v", err)
	}
	if _, err := git.Output(repoRoot, "push", "-u", "origin", conflictBranch); err != nil {
		t.Fatalf("git push -u origin %s: %v", conflictBranch, err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato after conflict branch: %v", err)
	}

	if out, err := git.Output(repoRoot, "ls-remote", "--heads", "origin", conflictBranch); err != nil {
		t.Fatalf("git ls-remote before ProcessQueue: %v", err)
	} else if strings.TrimSpace(out) == "" {
		t.Fatalf("expected conflicted task branch %s to exist on origin before ProcessQueue", conflictBranch)
	}

	firstTaskFile := filepath.Join(tasksDir, "ready-to-merge", firstTaskName)
	firstTaskContent := "---\npriority: 1\n---\n# First task\nThis should merge first.\n"
	if err := os.WriteFile(firstTaskFile, []byte(firstTaskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile first task file: %v", err)
	}
	conflictTaskFile := filepath.Join(tasksDir, "ready-to-merge", conflictTaskName)
	conflictTaskContent := "---\npriority: 2\n---\n<!-- branch: " + conflictBranch + " -->\n# Conflict task\nThis should conflict after the first merge.\n"
	if err := os.WriteFile(conflictTaskFile, []byte(conflictTaskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile conflict task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "completed", firstTaskName)); err != nil {
		t.Fatalf("completed first task missing: %v", err)
	}

	backlogFile := filepath.Join(tasksDir, "backlog", conflictTaskName)
	data, err := os.ReadFile(backlogFile)
	if err != nil {
		t.Fatalf("backlog conflicted task file missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: merge-queue") {
		t.Fatalf("backlog conflicted task should contain merge failure record, got %q", string(data))
	}
	if out, err := git.Output(repoRoot, "ls-remote", "--heads", "origin", conflictBranch); err != nil {
		t.Fatalf("git ls-remote after ProcessQueue: %v", err)
	} else if strings.TrimSpace(out) != "" {
		t.Fatalf("expected conflicted task branch %s to be deleted from origin, got %q", conflictBranch, out)
	}
}

func TestProcessQueue_RespectsMaxRetries(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "missing branch.md")
	taskContent := strings.Join([]string{
		"---",
		"max_retries: 1",
		"---",
		"<!-- failure: prior -->",
		"# Missing branch",
		"",
	}, "\n")
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	failedFile := filepath.Join(tasksDir, "failed", "missing branch.md")
	data, err := os.ReadFile(failedFile)
	if err != nil {
		t.Fatalf("failed task file missing: %v", err)
	}
	if !strings.Contains(string(data), "task branch not pushed by agent") {
		t.Fatalf("failed task should mention missing branch push, got %q", string(data))
	}
	if got := strings.Count(string(data), "<!-- failure:"); got != 2 {
		t.Fatalf("failed task failure count = %d, want 2\ncontents:\n%s", got, string(data))
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "missing branch.md")); !os.IsNotExist(err) {
		t.Fatalf("missing-branch task should not be moved to backlog, stat err = %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task should be moved to failed, stat err = %v", err)
	}
}

func TestProcessQueue_ZeroMaxRetries(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "zero-retry.md")
	taskContent := strings.Join([]string{
		"---",
		"max_retries: 0",
		"---",
		"# Zero retries",
		"",
	}, "\n")
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	// With max_retries: 0 and 0 prior failures, the task should go to failed/
	// because 0 >= 0 (no retries allowed).
	failedFile := filepath.Join(tasksDir, "failed", "zero-retry.md")
	data, err := os.ReadFile(failedFile)
	if err != nil {
		t.Fatalf("failed task file missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure:") {
		t.Fatalf("failed task should contain failure record, got %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "zero-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("zero-retry task should not be in backlog, stat err = %v", err)
	}
}

func TestProcessQueue_DefaultMaxRetries(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "missing branch.md")
	taskContent := strings.Join([]string{
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"# Missing branch",
		"",
	}, "\n")
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	backlogFile := filepath.Join(tasksDir, "backlog", "missing branch.md")
	data, err := os.ReadFile(backlogFile)
	if err != nil {
		t.Fatalf("backlog task file missing: %v", err)
	}
	if !strings.Contains(string(data), "task branch not pushed by agent") {
		t.Fatalf("backlog task should mention missing branch push, got %q", string(data))
	}
	if got := strings.Count(string(data), "<!-- failure:"); got != 3 {
		t.Fatalf("backlog task failure count = %d, want 3\ncontents:\n%s", got, string(data))
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "failed", "missing branch.md")); !os.IsNotExist(err) {
		t.Fatalf("missing-branch task should not be moved to failed yet, stat err = %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task should be moved to backlog, stat err = %v", err)
	}
}

func TestProcessQueueMovesTaskToBacklogWhenPushFails(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/add-feature", "mato"); err != nil {
		t.Fatalf("git checkout task/add-feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "feature.txt"); err != nil {
		t.Fatalf("git add feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "feature work"); err != nil {
		t.Fatalf("git commit feature work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "refuse"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "add feature!!.md")
	taskContent := "---\npriority: 5\n---\n# Add feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	// Task should be moved to backlog (not stuck in ready-to-merge)
	backlogFile := filepath.Join(tasksDir, "backlog", "add feature!!.md")
	data, err := os.ReadFile(backlogFile)
	if err != nil {
		t.Fatalf("task should be moved to backlog after push failure: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: merge-queue") || !strings.Contains(string(data), "push failed after squash merge") {
		t.Fatalf("backlog task should contain push failure record, got %q", string(data))
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("task should not remain in ready-to-merge after push failure, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "completed", "add feature!!.md")); !os.IsNotExist(err) {
		t.Fatalf("push failure should not move task to completed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("target branch should not have feature after rejected push, stat err = %v", err)
	}
}

func TestProcessQueue_PushFailureRespectsMaxRetries(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/push-retry", "mato"); err != nil {
		t.Fatalf("git checkout task/push-retry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "retry.txt"), []byte("retry\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile retry.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "retry.txt"); err != nil {
		t.Fatalf("git add retry.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "retry work"); err != nil {
		t.Fatalf("git commit retry work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}
	// Reject pushes to trigger errPushAfterSquashFailed
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "refuse"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	// Task already has one prior failure and max_retries: 1, so the next
	// failure should route it to failed/ instead of backlog/.
	taskFile := filepath.Join(tasksDir, "ready-to-merge", "push-retry.md")
	taskContent := strings.Join([]string{
		"---",
		"max_retries: 1",
		"---",
		"<!-- failure: prior -->",
		"# Push retry",
		"Merge this task.",
		"",
	}, "\n")
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	// With one prior failure and max_retries:1, the task should go to failed/
	failedFile := filepath.Join(tasksDir, "failed", "push-retry.md")
	data, err := os.ReadFile(failedFile)
	if err != nil {
		t.Fatalf("failed task file missing after push failure with exhausted retries: %v", err)
	}
	if !strings.Contains(string(data), "push failed after squash merge") {
		t.Fatalf("failed task should mention push failure, got %q", string(data))
	}
	if got := strings.Count(string(data), "<!-- failure:"); got != 2 {
		t.Fatalf("failed task failure count = %d, want 2\ncontents:\n%s", got, string(data))
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("task should not remain in ready-to-merge, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "push-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("exhausted-retry task should not be in backlog, stat err = %v", err)
	}
}

func TestProcessQueueRetriesCompletedMoveWithoutRemerging(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/add-feature", "mato"); err != nil {
		t.Fatalf("git checkout task/add-feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "feature.txt"); err != nil {
		t.Fatalf("git add feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "feature work"); err != nil {
		t.Fatalf("git commit feature work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	completedDir := filepath.Join(tasksDir, "completed")
	if err := os.RemoveAll(completedDir); err != nil {
		t.Fatalf("os.RemoveAll completed: %v", err)
	}
	if err := os.WriteFile(completedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completed placeholder: %v", err)
	}

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "add feature!!.md")
	taskContent := "---\npriority: 5\n---\n# Add feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() first run = %d, want 0", got)
	}
	data, err := os.ReadFile(taskFile)
	if err != nil {
		t.Fatalf("ready task file missing after completed move failure: %v", err)
	}
	if !strings.Contains(string(data), mergedTaskRecordPrefix) {
		t.Fatalf("ready task should be marked as merged after push succeeds, got %q", string(data))
	}

	if err := os.Remove(completedDir); err != nil {
		t.Fatalf("os.Remove completed placeholder: %v", err)
	}
	if err := os.MkdirAll(completedDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll completed: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() second run = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "completed", "add feature!!.md")); err != nil {
		t.Fatalf("completed task file missing after retry: %v", err)
	}
	log, err := git.Output(repoRoot, "log", "--format=%s", "mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	if got := strings.Count(log, "Add feature"); got != 1 {
		t.Fatalf("Add feature commit count = %d, want 1", got)
	}
}

func TestProcessQueue_SamePriorityDeterministicOrder(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	createReadyTaskWithBranch := func(taskName, title, changedFile string) {
		t.Helper()

		taskBranch := "task/" + sanitizeBranchName(taskName)
		if _, err := git.Output(repoRoot, "checkout", "-b", taskBranch, "mato"); err != nil {
			t.Fatalf("git checkout -b %s: %v", taskBranch, err)
		}
		if err := os.WriteFile(filepath.Join(repoRoot, changedFile), []byte(title+"\n"), 0o644); err != nil {
			t.Fatalf("os.WriteFile %s: %v", changedFile, err)
		}
		if _, err := git.Output(repoRoot, "add", changedFile); err != nil {
			t.Fatalf("git add %s: %v", changedFile, err)
		}
		if _, err := git.Output(repoRoot, "commit", "-m", title+" branch work"); err != nil {
			t.Fatalf("git commit %s branch work: %v", title, err)
		}
		if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
			t.Fatalf("git checkout mato: %v", err)
		}

		taskContent := "---\npriority: 10\n---\n# " + title + "\n"
		if err := os.WriteFile(filepath.Join(tasksDir, "ready-to-merge", taskName), []byte(taskContent), 0o644); err != nil {
			t.Fatalf("os.WriteFile %s: %v", taskName, err)
		}
	}

	createReadyTaskWithBranch("c-task.md", "C task", "c.txt")
	createReadyTaskWithBranch("a-task.md", "A task", "a.txt")
	createReadyTaskWithBranch("b-task.md", "B task", "b.txt")

	baseRev, err := git.Output(repoRoot, "rev-parse", "mato")
	if err != nil {
		t.Fatalf("git rev-parse mato: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 3 {
		t.Fatalf("ProcessQueue() = %d, want 3", got)
	}

	for _, name := range []string{"a-task.md", "b-task.md", "c-task.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, "completed", name)); err != nil {
			t.Fatalf("completed task %s missing: %v", name, err)
		}
	}

	log, err := git.Output(repoRoot, "log", "--reverse", "--format=%s", strings.TrimSpace(baseRev)+"..mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	var gotOrder []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			gotOrder = append(gotOrder, line)
		}
	}
	wantOrder := []string{"A task", "B task", "C task"}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("merged commit count = %d, want %d (%v)", len(gotOrder), len(wantOrder), gotOrder)
	}
	for i, want := range wantOrder {
		if gotOrder[i] != want {
			t.Fatalf("merge order[%d] = %q, want %q (full order: %v)", i, gotOrder[i], want, gotOrder)
		}
	}
}

func TestProcessQueue_MalformedTaskInReadyToMerge(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/good-task", "mato"); err != nil {
		t.Fatalf("git checkout task/good-task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "good.txt"), []byte("good\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile good.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "good.txt"); err != nil {
		t.Fatalf("git add good.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "good branch work"); err != nil {
		t.Fatalf("git commit good branch work: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	malformedTask := filepath.Join(tasksDir, "ready-to-merge", "bad-task.md")
	if err := os.WriteFile(malformedTask, []byte("---\npriority: not-a-number\n---\n# Bad\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile bad-task.md: %v", err)
	}
	validTask := filepath.Join(tasksDir, "ready-to-merge", "good-task.md")
	if err := os.WriteFile(validTask, []byte("---\npriority: 1\n---\n# Good task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile good-task.md: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "completed", "good-task.md")); err != nil {
		t.Fatalf("completed good-task.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "good.txt")); err != nil {
		t.Fatalf("merged good.txt missing from target branch: %v", err)
	}

	backlogTask := filepath.Join(tasksDir, "backlog", "bad-task.md")
	data, err := os.ReadFile(backlogTask)
	if err != nil {
		t.Fatalf("backlog bad-task.md missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: merge-queue") || !strings.Contains(string(data), "parse task file") {
		t.Fatalf("malformed task should be moved to backlog with parse failure record, got %q", string(data))
	}
	if _, err := os.Stat(malformedTask); !os.IsNotExist(err) {
		t.Fatalf("malformed task should not remain in ready-to-merge, stat err = %v", err)
	}
}

func TestProcessQueue_NonSentinelErrorMovesTaskToBacklog(t *testing.T) {
	// Use a non-git directory as repoRoot so CreateClone fails with a
	// non-sentinel error (not one of the three known error sentinels).
	repoRoot := t.TempDir()
	tasksDir := setupTasksDir(t)

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "clone-fail.md")
	taskContent := strings.Join([]string{
		"---",
		"priority: 1",
		"---",
		"# Clone fail task",
		"",
	}, "\n")
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	// Task should be moved to backlog/ (not left in ready-to-merge/).
	backlogFile := filepath.Join(tasksDir, "backlog", "clone-fail.md")
	data, err := os.ReadFile(backlogFile)
	if err != nil {
		t.Fatalf("backlog task file missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: merge-queue") {
		t.Fatalf("backlog task should contain merge failure record, got %q", string(data))
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task file should be moved to backlog, stat err = %v", err)
	}
}

func TestProcessQueue_NonSentinelErrorRespectsMaxRetries(t *testing.T) {
	// A non-sentinel error with exhausted retries should move to failed/.
	repoRoot := t.TempDir()
	tasksDir := setupTasksDir(t)

	taskFile := filepath.Join(tasksDir, "ready-to-merge", "clone-fail-retry.md")
	taskContent := strings.Join([]string{
		"---",
		"max_retries: 1",
		"---",
		"<!-- failure: prior -->",
		"# Clone fail retry",
		"",
	}, "\n")
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	// With max_retries: 1 and 1 prior failure, the new failure makes 2 >= 1,
	// so the task should go to failed/.
	failedFile := filepath.Join(tasksDir, "failed", "clone-fail-retry.md")
	data, err := os.ReadFile(failedFile)
	if err != nil {
		t.Fatalf("failed task file missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: merge-queue") {
		t.Fatalf("failed task should contain merge failure record, got %q", string(data))
	}
	if got := strings.Count(string(data), "<!-- failure:"); got != 2 {
		t.Fatalf("failed task failure count = %d, want 2\ncontents:\n%s", got, string(data))
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "clone-fail-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("task should not be in backlog, stat err = %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task should be moved to failed, stat err = %v", err)
	}
}

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "simple name", input: "add-feature.md", want: "add-feature"},
		{name: "spaces and special chars", input: "fix the bug (urgent).md", want: "fix-the-bug-urgent"},
		{name: "already clean no extension", input: "my-task", want: "my-task"},
		{name: "consecutive special chars", input: "foo---bar___baz.md", want: "foo-bar-baz"},
		{name: "leading and trailing specials", input: "---hello---.md", want: "hello"},
		{name: "empty after strip", input: ".md", want: "unnamed"},
		{name: "unicode characters", input: "tâche-résumé.md", want: "t-che-r-sum"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBranchName(tt.input); got != tt.want {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsProcessActive_CurrentPID(t *testing.T) {
if !isProcessActive(os.Getpid()) {
t.Fatal("current process should be active")
}
}

func TestIsProcessActive_DeadPID(t *testing.T) {
if isProcessActive(2147483647) {
t.Fatal("non-existent PID should not be active")
}
}

func TestIsProcessActive_InvalidPID(t *testing.T) {
if isProcessActive(0) {
t.Fatal("PID 0 should not be active")
}
if isProcessActive(-1) {
t.Fatal("negative PID should not be active")
}
}

func TestIsProcessActive_EPERMTreatedAsAlive(t *testing.T) {
if os.Getuid() == 0 {
t.Skip("test requires non-root user (PID 1 returns EPERM only for non-root)")
}
// PID 1 (init/systemd) belongs to root; Signal(0) returns EPERM for non-root callers.
if !isProcessActive(1) {
t.Fatal("PID 1 should be considered active (EPERM means process exists)")
}
}
