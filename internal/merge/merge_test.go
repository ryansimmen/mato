package merge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/testutil"
)

func setupTasksDir(t *testing.T) string {
	t.Helper()

	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed, ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
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

func TestAcquireLockFailsWhenLockFileEmpty(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	// Simulate the race window: lock file exists but is empty (writer hasn't written identity yet)
	if err := os.WriteFile(filepath.Join(locksDir, "merge.lock"), []byte(""), 0o644); err != nil {
		t.Fatalf("os.WriteFile merge.lock: %v", err)
	}

	cleanup, ok := AcquireLock(tasksDir)
	if ok || cleanup != nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected merge lock acquisition to fail when lock file is empty (conservatively assume holder is alive)")
	}
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

func TestAtomicMove_DestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, queue.DirReadyMerge, "task.md")
	dst := filepath.Join(dir, queue.DirBacklog, "task.md")
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

	err := queue.AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail when destination exists")
	}
	if !errors.Is(err, queue.ErrDestinationExists) {
		t.Fatalf("AtomicMove error = %q, want ErrDestinationExists", err)
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
	repoRoot := testutil.SetupRepo(t)
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
	repoRoot := testutil.SetupRepo(t)
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "add feature!!.md")
	taskContent := "---\npriority: 5\n---\n<!-- claimed-by: agent123  claimed-at: 2026-01-01T00:00:00Z -->\n# Add feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "add feature!!.md")); err != nil {
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
	// Squash commit subject comes from agent's commit message on the task branch.
	if got := strings.TrimSpace(msg); got != "feature work" {
		t.Fatalf("merge commit message = %q, want %q", got, "feature work")
	}
}

func TestProcessQueue_CleansTaskBranchAfterSuccessfulMerge(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
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
	if _, err := git.Output(repoRoot, "push", "-u", "origin", "mato"); err != nil {
		t.Fatalf("git push -u origin mato: %v", err)
	}

	taskBranch := "task/cleanup-test"
	if _, err := git.Output(repoRoot, "checkout", "-b", taskBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", taskBranch, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "feature.txt"); err != nil {
		t.Fatalf("git add feature.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "add feature"); err != nil {
		t.Fatalf("git commit add feature: %v", err)
	}
	if _, err := git.Output(repoRoot, "push", "-u", "origin", taskBranch); err != nil {
		t.Fatalf("git push -u origin %s: %v", taskBranch, err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	// Verify the task branch exists on origin before merge.
	if out, err := git.Output(repoRoot, "ls-remote", "--heads", "origin", taskBranch); err != nil {
		t.Fatalf("git ls-remote before ProcessQueue: %v", err)
	} else if strings.TrimSpace(out) == "" {
		t.Fatalf("expected task branch %s to exist on origin before ProcessQueue", taskBranch)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "cleanup-test.md")
	taskContent := "---\npriority: 5\n---\n<!-- branch: " + taskBranch + " -->\n# Cleanup test\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "cleanup-test.md")); err != nil {
		t.Fatalf("completed task file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "feature.txt")); err != nil {
		t.Fatalf("merged feature file missing from target branch: %v", err)
	}

	// Verify the task branch was deleted from origin after successful merge.
	if out, err := git.Output(repoRoot, "ls-remote", "--heads", "origin", taskBranch); err != nil {
		t.Fatalf("git ls-remote after ProcessQueue: %v", err)
	} else if strings.TrimSpace(out) != "" {
		t.Fatalf("expected task branch %s to be deleted from origin after successful merge, got %q", taskBranch, out)
	}

	// Verify the local task branch was also deleted.
	if out, err := git.Output(repoRoot, "branch", "--list", taskBranch); err != nil {
		t.Fatalf("git branch --list: %v", err)
	} else if strings.TrimSpace(out) != "" {
		t.Fatalf("expected local task branch %s to be deleted after successful merge, got %q", taskBranch, out)
	}
}

func TestProcessQueue_UsesBranchFromTaskFile(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskName := "custom branch.md"
	derivedBranch := "task/" + frontmatter.SanitizeBranchName(taskName)
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, taskName)
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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskName := "fallback branch.md"
	derivedBranch := "task/" + frontmatter.SanitizeBranchName(taskName)
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, taskName)
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

func TestProcessQueue_BranchCollisionDisambiguation(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	// Two tasks whose filenames sanitize to the same branch name.
	// "collide task.md" and "collide_task.md" both sanitize to "task/collide-task".
	taskA := "collide task.md"
	taskB := "collide_task.md"
	derivedBranch := "task/" + frontmatter.SanitizeBranchName(taskA)
	disambiguatedBranch := derivedBranch + "-" + frontmatter.BranchDisambiguator(taskB)

	// Verify both filenames actually collide on the same derived branch.
	if got := "task/" + frontmatter.SanitizeBranchName(taskB); got != derivedBranch {
		t.Fatalf("test setup: expected both filenames to derive the same branch, got %q and %q", derivedBranch, got)
	}

	// Create git branches with changes for both tasks.
	if _, err := git.Output(repoRoot, "checkout", "-b", derivedBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", derivedBranch, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "a.txt"), []byte("task A\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile a.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "a.txt"); err != nil {
		t.Fatalf("git add a.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "task A work"); err != nil {
		t.Fatalf("git commit task A: %v", err)
	}

	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", disambiguatedBranch, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", disambiguatedBranch, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "b.txt"), []byte("task B\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile b.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "b.txt"); err != nil {
		t.Fatalf("git add b.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "task B work"); err != nil {
		t.Fatalf("git commit task B: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	// Task A sits in in-progress/ with a branch comment claiming the derived branch.
	inProgressDir := filepath.Join(tasksDir, queue.DirInProgress)
	if err := os.MkdirAll(inProgressDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll in-progress: %v", err)
	}
	taskAContent := "<!-- branch: " + derivedBranch + " -->\n# Collide task A\nSome work.\n"
	if err := os.WriteFile(filepath.Join(inProgressDir, taskA), []byte(taskAContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task A: %v", err)
	}

	// Task B in ready-to-merge/ has no branch comment, so it falls back to derived name.
	taskBContent := "---\npriority: 5\n---\n# Collide task B\nMore work.\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyMerge, taskB), []byte(taskBContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task B: %v", err)
	}

	// Task B should use the disambiguated branch and merge successfully.
	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "b.txt")); err != nil {
		t.Fatalf("expected disambiguated branch change (b.txt) to merge: %v", err)
	}
}

func TestProcessQueueMovesConflictedTaskBackToBacklog(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "conflict task.md")
	taskContent := "---\npriority: 1\n---\n# Conflict task\nThis should conflict.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}
	backlogFile := filepath.Join(tasksDir, queue.DirBacklog, "conflict task.md")
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
	repoRoot := testutil.SetupRepo(t)
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
	firstBranch := "task/" + frontmatter.SanitizeBranchName(firstTaskName)
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

	firstTaskFile := filepath.Join(tasksDir, queue.DirReadyMerge, firstTaskName)
	firstTaskContent := "---\npriority: 1\n---\n# First task\nThis should merge first.\n"
	if err := os.WriteFile(firstTaskFile, []byte(firstTaskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile first task file: %v", err)
	}
	conflictTaskFile := filepath.Join(tasksDir, queue.DirReadyMerge, conflictTaskName)
	conflictTaskContent := "---\npriority: 2\n---\n<!-- branch: " + conflictBranch + " -->\n# Conflict task\nThis should conflict after the first merge.\n"
	if err := os.WriteFile(conflictTaskFile, []byte(conflictTaskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile conflict task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, firstTaskName)); err != nil {
		t.Fatalf("completed first task missing: %v", err)
	}

	backlogFile := filepath.Join(tasksDir, queue.DirBacklog, conflictTaskName)
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
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "missing branch.md")
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

	failedFile := filepath.Join(tasksDir, queue.DirFailed, "missing branch.md")
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "missing branch.md")); !os.IsNotExist(err) {
		t.Fatalf("missing-branch task should not be moved to backlog, stat err = %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task should be moved to failed, stat err = %v", err)
	}
}

func TestProcessQueue_ZeroMaxRetries(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "zero-retry.md")
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
	failedFile := filepath.Join(tasksDir, queue.DirFailed, "zero-retry.md")
	data, err := os.ReadFile(failedFile)
	if err != nil {
		t.Fatalf("failed task file missing: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure:") {
		t.Fatalf("failed task should contain failure record, got %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "zero-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("zero-retry task should not be in backlog, stat err = %v", err)
	}
}

func TestProcessQueue_DefaultMaxRetries(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "missing branch.md")
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

	backlogFile := filepath.Join(tasksDir, queue.DirBacklog, "missing branch.md")
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "missing branch.md")); !os.IsNotExist(err) {
		t.Fatalf("missing-branch task should not be moved to failed yet, stat err = %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task should be moved to backlog, stat err = %v", err)
	}
}

func TestProcessQueueMovesTaskToBacklogWhenPushFails(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "add feature!!.md")
	taskContent := "---\npriority: 5\n---\n# Add feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() = %d, want 0", got)
	}

	// Task should be moved to backlog (not stuck in ready-to-merge)
	backlogFile := filepath.Join(tasksDir, queue.DirBacklog, "add feature!!.md")
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "add feature!!.md")); !os.IsNotExist(err) {
		t.Fatalf("push failure should not move task to completed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "feature.txt")); !os.IsNotExist(err) {
		t.Fatalf("target branch should not have feature after rejected push, stat err = %v", err)
	}
}

func TestProcessQueue_PushFailureRespectsMaxRetries(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
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
	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "push-retry.md")
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
	failedFile := filepath.Join(tasksDir, queue.DirFailed, "push-retry.md")
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "push-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("exhausted-retry task should not be in backlog, stat err = %v", err)
	}
}

func TestProcessQueueRetriesCompletedMoveWithoutRemerging(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
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

	completedDir := filepath.Join(tasksDir, queue.DirCompleted)
	if err := os.RemoveAll(completedDir); err != nil {
		t.Fatalf("os.RemoveAll completed: %v", err)
	}
	if err := os.WriteFile(completedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completed placeholder: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "add feature!!.md")
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "add feature!!.md")); err != nil {
		t.Fatalf("completed task file missing after retry: %v", err)
	}
	log, err := git.Output(repoRoot, "log", "--format=%s", "mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	// Squash subject comes from agent commit, not task title.
	if got := strings.Count(log, "feature work"); got != 1 {
		t.Fatalf("feature work commit count = %d, want 1 (log=%s)", got, strings.Fields(log))
	}
}

func TestProcessQueue_DuplicateInCompletedIsRemoved(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Place a task in ready-to-merge with a merged record.
	rtmFile := filepath.Join(tasksDir, queue.DirReadyMerge, "dup-task.md")
	if err := os.WriteFile(rtmFile, []byte("---\npriority: 5\n---\n# Dup\n\n<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile rtm: %v", err)
	}

	// Place a copy in completed (simulating a prior successful move).
	completedFile := filepath.Join(tasksDir, queue.DirCompleted, "dup-task.md")
	if err := os.WriteFile(completedFile, []byte("---\npriority: 5\n---\n# Dup\n\n<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completed: %v", err)
	}

	// ProcessQueue should detect the duplicate and remove the ready-to-merge copy.
	// We don't need a real repo since the task already has a merged record.
	repoRoot := t.TempDir()
	ProcessQueue(repoRoot, tasksDir, "mato")

	// The ready-to-merge copy should be gone.
	if _, err := os.Stat(rtmFile); !os.IsNotExist(err) {
		t.Fatalf("expected ready-to-merge copy to be removed, but it still exists")
	}
	// The completed copy should still be there.
	if _, err := os.Stat(completedFile); err != nil {
		t.Fatalf("completed copy should still exist: %v", err)
	}
}

func TestProcessQueue_SamePriorityDeterministicOrder(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	createReadyTaskWithBranch := func(taskName, title, changedFile string) {
		t.Helper()

		taskBranch := "task/" + frontmatter.SanitizeBranchName(taskName)
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
		if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyMerge, taskName), []byte(taskContent), 0o644); err != nil {
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
		if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, name)); err != nil {
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
	// Squash subjects come from the agent's commit messages on each task branch.
	wantOrder := []string{"A task branch work", "B task branch work", "C task branch work"}
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
	repoRoot := testutil.SetupRepo(t)
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

	malformedTask := filepath.Join(tasksDir, queue.DirReadyMerge, "bad-task.md")
	if err := os.WriteFile(malformedTask, []byte("---\npriority: not-a-number\n---\n# Bad\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile bad-task.md: %v", err)
	}
	validTask := filepath.Join(tasksDir, queue.DirReadyMerge, "good-task.md")
	if err := os.WriteFile(validTask, []byte("---\npriority: 1\n---\n# Good task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile good-task.md: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "good-task.md")); err != nil {
		t.Fatalf("completed good-task.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "good.txt")); err != nil {
		t.Fatalf("merged good.txt missing from target branch: %v", err)
	}

	backlogTask := filepath.Join(tasksDir, queue.DirBacklog, "bad-task.md")
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "clone-fail.md")
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
	backlogFile := filepath.Join(tasksDir, queue.DirBacklog, "clone-fail.md")
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

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "clone-fail-retry.md")
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
	failedFile := filepath.Join(tasksDir, queue.DirFailed, "clone-fail-retry.md")
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "clone-fail-retry.md")); !os.IsNotExist(err) {
		t.Fatalf("task should not be in backlog, stat err = %v", err)
	}
	if _, err := os.Stat(taskFile); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge task should be moved to failed, stat err = %v", err)
	}
}

// TestProcessQueue_IdempotentMergeAfterBookkeepingFailure verifies that when
// a push succeeds but the post-push bookkeeping (markTaskMerged + move) fails,
// the next merge cycle detects the already-merged branch and completes the
// bookkeeping without creating a duplicate commit.
func TestProcessQueue_IdempotentMergeAfterBookkeepingFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	// Create a task branch with a change.
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/durable-merge", "mato"); err != nil {
		t.Fatalf("git checkout task/durable-merge: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "durable.txt"), []byte("durable\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile durable.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "durable.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "durable work"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	// Sabotage: make the completed directory a file so both markTaskMerged
	// (which will succeed) and moveTaskWithRetry (which will fail) trigger
	// the "merged but could not move" path.
	completedDir := filepath.Join(tasksDir, queue.DirCompleted)
	if err := os.RemoveAll(completedDir); err != nil {
		t.Fatalf("os.RemoveAll completed: %v", err)
	}
	if err := os.WriteFile(completedDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completed placeholder: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "durable-merge.md")
	taskContent := "---\npriority: 5\n---\n# Durable merge\nMerge this.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task: %v", err)
	}

	// First run: push succeeds, move fails → task stays in ready-to-merge.
	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 0 {
		t.Fatalf("ProcessQueue() first run = %d, want 0", got)
	}
	if _, err := os.Stat(taskFile); err != nil {
		t.Fatalf("task file should still be in ready-to-merge: %v", err)
	}

	// Verify the push actually landed.
	if _, err := os.Stat(filepath.Join(repoRoot, "durable.txt")); err != nil {
		t.Fatalf("merged file missing from target branch: %v", err)
	}

	// Fix the completed directory.
	if err := os.Remove(completedDir); err != nil {
		t.Fatalf("os.Remove completed placeholder: %v", err)
	}
	if err := os.MkdirAll(completedDir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll completed: %v", err)
	}

	// Second run: should detect the already-merged branch and complete
	// bookkeeping without creating a duplicate commit.
	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() second run = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "durable-merge.md")); err != nil {
		t.Fatalf("completed task missing after retry: %v", err)
	}

	// Ensure exactly one merge commit (no duplicate).
	log, err := git.Output(repoRoot, "log", "--format=%s", "mato")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	// Squash subject comes from agent commit "durable work".
	if got := strings.Count(log, "durable work"); got != 1 {
		t.Fatalf("durable work commit count = %d, want 1; log:\n%s", got, log)
	}
}

// TestProcessQueue_MarkMergedFailsButMoveSucceeds verifies that when
// markTaskMerged fails (e.g. read-only task file), the task still moves to
// completed/ because we no longer skip moveTaskWithRetry.
func TestProcessQueue_MarkMergedFailsButMoveSucceeds(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}

	// Create a task branch.
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/readonly-task", "mato"); err != nil {
		t.Fatalf("git checkout task/readonly-task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "readonly.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "readonly.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "readonly work"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "readonly-task.md")
	taskContent := "---\npriority: 5\n---\n# Readonly task\nMerge this.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task: %v", err)
	}

	// Make task file read-only so markTaskMerged (which opens for append)
	// will fail, but os.Rename (used by moveTaskWithRetry) will still work.
	if err := os.Chmod(taskFile, 0o444); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}
	t.Cleanup(func() {
		// Ensure cleanup can remove the file.
		os.Chmod(taskFile, 0o644)
		completedFile := filepath.Join(tasksDir, queue.DirCompleted, "readonly-task.md")
		os.Chmod(completedFile, 0o644)
	})

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}

	// Task should be in completed/ despite markTaskMerged failure.
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "readonly-task.md")); err != nil {
		t.Fatalf("completed task missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "readonly.txt")); err != nil {
		t.Fatalf("merged file missing from target branch: %v", err)
	}
}

func TestFormatSquashCommitMessage_WithAllTrailers(t *testing.T) {
	task := mergeQueueTask{
		title:   "Add feature",
		id:      "add-feature",
		affects: []string{"internal/merge/merge.go", "internal/merge/merge_test.go"},
	}
	got := formatSquashCommitMessage(task, "")
	want := "Add feature\n\nTask-ID: add-feature\nAffects: internal/merge/merge.go, internal/merge/merge_test.go"
	if got != want {
		t.Fatalf("formatSquashCommitMessage() =\n%q\nwant\n%q", got, want)
	}
}

func TestFormatSquashCommitMessage_NoMetadata(t *testing.T) {
	task := mergeQueueTask{
		title: "Simple task",
	}
	got := formatSquashCommitMessage(task, "")
	if got != "Simple task" {
		t.Fatalf("formatSquashCommitMessage() = %q, want %q", got, "Simple task")
	}
}

func TestFormatSquashCommitMessage_OnlyID(t *testing.T) {
	task := mergeQueueTask{
		title: "Task with ID",
		id:    "task-id-only",
	}
	got := formatSquashCommitMessage(task, "")
	want := "Task with ID\n\nTask-ID: task-id-only"
	if got != want {
		t.Fatalf("formatSquashCommitMessage() = %q, want %q", got, want)
	}
}

func TestFormatSquashCommitMessage_OnlyAffects(t *testing.T) {
	task := mergeQueueTask{
		title:   "Task with affects",
		affects: []string{"file.go"},
	}
	got := formatSquashCommitMessage(task, "")
	want := "Task with affects\n\nAffects: file.go"
	if got != want {
		t.Fatalf("formatSquashCommitMessage() = %q, want %q", got, want)
	}
}

func TestProcessQueue_CommitIncludesTrailers(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/add-trailers-feature", "mato"); err != nil {
		t.Fatalf("git checkout task branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "trailers.txt"), []byte("trailers\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile trailers.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "trailers.txt"); err != nil {
		t.Fatalf("git add trailers.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "add trailers feature"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "add-trailers-feature.md")
	taskContent := "---\nid: add-trailers-feature\npriority: 5\naffects:\n  - internal/merge/merge.go\n  - internal/merge/merge_test.go\n---\n# Add trailers feature\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}

	msg, err := git.Output(repoRoot, "log", "--format=%B", "-1", "mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	msg = strings.TrimSpace(msg)
	if !strings.Contains(msg, "Task-ID: add-trailers-feature") {
		t.Fatalf("commit message missing Task-ID trailer:\n%s", msg)
	}
	if !strings.Contains(msg, "Affects: internal/merge/merge.go, internal/merge/merge_test.go") {
		t.Fatalf("commit message missing Affects trailer:\n%s", msg)
	}
	// Verify subject comes from agent's commit message on the task branch.
	lines := strings.SplitN(msg, "\n", 2)
	if lines[0] != "add trailers feature" {
		t.Fatalf("commit message first line = %q, want %q", lines[0], "add trailers feature")
	}
}

func TestProcessQueue_CommitOmitsTrailersWhenEmpty(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/no-metadata", "mato"); err != nil {
		t.Fatalf("git checkout task branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "plain.txt"), []byte("plain\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile plain.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "plain.txt"); err != nil {
		t.Fatalf("git add plain.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "plain work"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	// No explicit id or affects in frontmatter. The frontmatter parser
	// defaults id to the filename stem, so Task-ID will still appear.
	// Only Affects should be absent.
	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "no-metadata.md")
	taskContent := "---\npriority: 5\n---\n# No metadata task\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}

	msg, err := git.Output(repoRoot, "log", "--format=%B", "-1", "mato")
	if err != nil {
		t.Fatalf("git log mato: %v", err)
	}
	msg = strings.TrimSpace(msg)
	// Squash subject comes from agent's commit message, not the task title.
	if !strings.HasPrefix(msg, "plain work") {
		t.Fatalf("commit message should start with agent's commit subject, got:\n%s", msg)
	}
	if strings.Contains(msg, "Affects:") {
		t.Fatalf("commit message should not contain Affects trailer when affects is empty:\n%s", msg)
	}
}

func TestProcessQueue_WritesCompletionDetail(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/add-completion", "mato"); err != nil {
		t.Fatalf("git checkout task branch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "completion.txt"), []byte("completion\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completion.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "completion.txt"); err != nil {
		t.Fatalf("git add completion.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "add completion feature"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "add-completion.md")
	taskContent := "---\nid: add-completion\npriority: 5\naffects:\n  - completion.txt\n---\n# Add completion\nMerge this task.\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}

	// Verify completion detail was written
	detail, err := messaging.ReadCompletionDetail(tasksDir, "add-completion")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if detail.TaskID != "add-completion" {
		t.Fatalf("TaskID = %q, want %q", detail.TaskID, "add-completion")
	}
	if detail.TaskFile != "add-completion.md" {
		t.Fatalf("TaskFile = %q, want %q", detail.TaskFile, "add-completion.md")
	}
	if detail.Branch != "task/add-completion" {
		t.Fatalf("Branch = %q, want %q", detail.Branch, "task/add-completion")
	}
	if detail.CommitSHA == "" {
		t.Fatal("CommitSHA should not be empty")
	}
	if detail.Title != "Add completion" {
		t.Fatalf("Title = %q, want %q", detail.Title, "Add completion")
	}
	if detail.MergedAt.IsZero() {
		t.Fatal("MergedAt should not be zero")
	}
	if len(detail.FilesChanged) == 0 {
		t.Fatal("FilesChanged should not be empty")
	}
	foundCompletionTxt := false
	for _, f := range detail.FilesChanged {
		if f == "completion.txt" {
			foundCompletionTxt = true
		}
	}
	if !foundCompletionTxt {
		t.Fatalf("FilesChanged = %v, want to contain completion.txt", detail.FilesChanged)
	}

	// Verify JSON is well-formed
	path := filepath.Join(tasksDir, "messages", "completions", "add-completion.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("completion detail file should contain valid JSON")
	}
}

func TestProcessQueue_CompletionDetailFilesChanged(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := setupTasksDir(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/multi-file", "mato"); err != nil {
		t.Fatalf("git checkout task branch: %v", err)
	}
	// Create multiple files to verify files_changed captures all of them
	for _, name := range []string{"file-a.txt", "file-b.txt", "file-c.txt"} {
		if err := os.WriteFile(filepath.Join(repoRoot, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("os.WriteFile %s: %v", name, err)
		}
	}
	if _, err := git.Output(repoRoot, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "add multiple files"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	taskFile := filepath.Join(tasksDir, queue.DirReadyMerge, "multi-file.md")
	taskContent := "---\nid: multi-file\npriority: 5\n---\n# Multi file task\n"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0o644); err != nil {
		t.Fatalf("os.WriteFile task file: %v", err)
	}

	if got := ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("ProcessQueue() = %d, want 1", got)
	}

	detail, err := messaging.ReadCompletionDetail(tasksDir, "multi-file")
	if err != nil {
		t.Fatalf("ReadCompletionDetail: %v", err)
	}
	if len(detail.FilesChanged) != 3 {
		t.Fatalf("FilesChanged = %v, want 3 files", detail.FilesChanged)
	}
	want := map[string]bool{"file-a.txt": true, "file-b.txt": true, "file-c.txt": true}
	for _, f := range detail.FilesChanged {
		if !want[f] {
			t.Fatalf("unexpected file in FilesChanged: %q", f)
		}
	}
}

func TestFailMergeTask_MovesTaskEvenWhenAppendFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, queue.DirReadyMerge)
	dst := filepath.Join(dir, queue.DirBacklog)
	os.MkdirAll(src, 0o755)
	os.MkdirAll(dst, 0o755)

	taskFile := filepath.Join(src, "broken-append.md")
	os.WriteFile(taskFile, []byte("# Broken append task\n"), 0o444)
	t.Cleanup(func() {
		os.Chmod(taskFile, 0o644)
		os.Chmod(filepath.Join(dst, "broken-append.md"), 0o644)
	})

	dstPath := filepath.Join(dst, "broken-append.md")
	err := failMergeTask(taskFile, dstPath, "test failure")

	// The function should succeed (move worked) even though append failed
	if err != nil {
		t.Fatalf("failMergeTask should succeed when append fails but move works: %v", err)
	}

	// Task should be in destination
	data, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("task should be moved to destination: %v", err)
	}

	// Content should be preserved
	if !strings.Contains(string(data), "# Broken append task") {
		t.Fatal("task content should be preserved")
	}

	// Source should no longer exist
	if _, err := os.Stat(taskFile); err == nil {
		t.Fatal("source file should no longer exist after move")
	}
}

func TestFailMergeTask_NoDst_ReturnsAppendError(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "task.md")
	os.WriteFile(taskFile, []byte("# Task\n"), 0o644)

	// Make directory read-only so temp file creation fails during atomic write.
	os.Chmod(dir, 0o555)
	t.Cleanup(func() {
		os.Chmod(dir, 0o755)
	})

	// When dst is empty and append fails, should return the error
	err := failMergeTask(taskFile, "", "test failure")
	if err == nil {
		t.Fatal("failMergeTask with empty dst should return append error when directory is read-only")
	}
}

func TestFailMergeTask_BothAppendAndMoveFail(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "tasks")
	os.MkdirAll(subdir, 0o755)
	taskFile := filepath.Join(subdir, "task.md")
	os.WriteFile(taskFile, []byte("# Task\n"), 0o644)

	// Create a regular file where the destination directory should be,
	// so MkdirAll fails when trying to create subdirectories under it.
	blocker := filepath.Join(dir, "blocked-dir")
	os.WriteFile(blocker, []byte("not a directory"), 0o644)

	// Make the task directory read-only so atomic write fails.
	os.Chmod(subdir, 0o555)
	t.Cleanup(func() {
		os.Chmod(subdir, 0o755)
	})

	dstPath := filepath.Join(blocker, "sub", "task.md")

	err := failMergeTask(taskFile, dstPath, "test failure")
	if err == nil {
		t.Fatal("failMergeTask should return error when both append and move fail")
	}
	// Error should mention both failures
	if !strings.Contains(err.Error(), "move task file") {
		t.Fatalf("error should mention move failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "append failure record") {
		t.Fatalf("error should mention append failure, got: %v", err)
	}
}

func TestFormatSquashCommitMessage_WithAgentLog(t *testing.T) {
	task := mergeQueueTask{
		title:   "Add feature",
		id:      "add-feature",
		affects: []string{"internal/merge/merge.go"},
	}
	agentLog := "feat: add conflict detection for overlapping affects\n\nPrevents two tasks from modifying the same files concurrently\nby checking the affects: metadata before claiming.\n\nTask: add-feature.md\n\nChanged files:\ninternal/merge/merge.go\n"

	got := formatSquashCommitMessage(task, agentLog)

	// Subject should come from agent log.
	if !strings.HasPrefix(got, "feat: add conflict detection for overlapping affects\n") {
		t.Fatalf("subject should come from agent log, got:\n%s", got)
	}
	// Body should include the agent's description.
	if !strings.Contains(got, "Prevents two tasks from modifying the same files concurrently") {
		t.Fatalf("body should include agent description, got:\n%s", got)
	}
	// "Task:" and "Changed files:" should be stripped.
	if strings.Contains(got, "Task: add-feature.md") {
		t.Fatalf("mechanical 'Task:' line should be stripped, got:\n%s", got)
	}
	if strings.Contains(got, "Changed files:") {
		t.Fatalf("mechanical 'Changed files:' section should be stripped, got:\n%s", got)
	}
	// Trailers should be present.
	if !strings.Contains(got, "Task-ID: add-feature") {
		t.Fatalf("Task-ID trailer missing, got:\n%s", got)
	}
	if !strings.Contains(got, "Affects: internal/merge/merge.go") {
		t.Fatalf("Affects trailer missing, got:\n%s", got)
	}
}

func TestFormatSquashCommitMessage_AgentLogNoBody(t *testing.T) {
	task := mergeQueueTask{
		title: "Fix bug",
		id:    "fix-bug",
	}
	// Agent wrote a subject-only commit (no body beyond mechanical lines).
	agentLog := "fix: correct off-by-one in queue selection\n\nTask: fix-bug.md\n\nChanged files:\ninternal/queue/queue.go\n"

	got := formatSquashCommitMessage(task, agentLog)

	if !strings.HasPrefix(got, "fix: correct off-by-one in queue selection\n") {
		t.Fatalf("subject should come from agent log, got:\n%s", got)
	}
	if !strings.Contains(got, "Task-ID: fix-bug") {
		t.Fatalf("Task-ID trailer missing, got:\n%s", got)
	}
	// No body text between subject and trailers (only the mechanical lines were present).
	if strings.Contains(got, "Task: fix-bug.md") {
		t.Fatalf("mechanical 'Task:' line should be stripped, got:\n%s", got)
	}
}

func TestFormatSquashCommitMessage_EmptyAgentLogFallsBackToTitle(t *testing.T) {
	task := mergeQueueTask{
		title: "Fallback title",
		id:    "fallback-id",
	}
	got := formatSquashCommitMessage(task, "")

	if !strings.HasPrefix(got, "Fallback title\n") {
		t.Fatalf("should fall back to task title, got:\n%s", got)
	}
	if !strings.Contains(got, "Task-ID: fallback-id") {
		t.Fatalf("Task-ID trailer missing, got:\n%s", got)
	}
}

func TestFormatSquashCommitMessage_WhitespaceOnlyAgentLog(t *testing.T) {
	task := mergeQueueTask{
		title: "Whitespace test",
	}
	got := formatSquashCommitMessage(task, "  \n\n  \n")
	if got != "Whitespace test" {
		t.Fatalf("should fall back to task title for whitespace-only log, got:\n%s", got)
	}
}

func TestParseAgentCommitLog_SubjectAndBody(t *testing.T) {
	log := "fix: handle nil pointer in merge queue\n\nThe merge queue crashed when a task file had no branch comment.\nAdded a nil check before accessing the branch field.\n\nTask: fix-nil.md\n\nChanged files:\ninternal/merge/merge.go\n"

	subject, body := parseAgentCommitLog(log)
	if subject != "fix: handle nil pointer in merge queue" {
		t.Fatalf("subject = %q", subject)
	}
	wantBody := "The merge queue crashed when a task file had no branch comment.\nAdded a nil check before accessing the branch field."
	if body != wantBody {
		t.Fatalf("body = %q, want %q", body, wantBody)
	}
}

func TestParseAgentCommitLog_SubjectOnly(t *testing.T) {
	log := "fix: simple one-liner\n"

	subject, body := parseAgentCommitLog(log)
	if subject != "fix: simple one-liner" {
		t.Fatalf("subject = %q", subject)
	}
	if body != "" {
		t.Fatalf("body should be empty, got %q", body)
	}
}

func TestParseAgentCommitLog_Empty(t *testing.T) {
	subject, body := parseAgentCommitLog("")
	if subject != "" || body != "" {
		t.Fatalf("expected empty subject and body, got %q / %q", subject, body)
	}
}

func TestParseAgentCommitLog_MultiCommit(t *testing.T) {
	// git log --format=%B with multiple commits separates them with blank lines.
	log := "feat: add review gate\n\nIntroduce ready-for-review state.\n\nTask: add-review.md\n\nChanged files:\nrunner.go\n\nfix: typo in docs\n\nTask: add-review.md\n\nChanged files:\ndocs/arch.md\n"

	subject, body := parseAgentCommitLog(log)
	if subject != "feat: add review gate" {
		t.Fatalf("subject = %q, want first commit's subject", subject)
	}
	if !strings.Contains(body, "Introduce ready-for-review state") {
		t.Fatalf("body should contain first commit's description, got %q", body)
	}
	// Should NOT contain the second commit's subject.
	if strings.Contains(body, "fix: typo") {
		t.Fatalf("body should not contain second commit's content, got %q", body)
	}
}

func TestAppendTaskRecord_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	original := "# Test task\nSome content\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	if err := appendTaskRecord(path, "<!-- merged: merge-queue at %s -->", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("appendTaskRecord: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile after append: %v", err)
	}
	content := string(data)

	if !strings.HasPrefix(content, original) {
		t.Fatalf("original content was not preserved: got %q", content)
	}
	if !strings.Contains(content, "<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->") {
		t.Fatalf("appended record not found in content: %q", content)
	}

	// Verify no temp files remain after successful write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file was not cleaned up: %s", e.Name())
		}
	}
}

func TestAppendTaskRecord_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.md")

	err := appendTaskRecord(path, "<!-- failure: test -->")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "read task file") {
		t.Fatalf("error should mention reading task file, got: %v", err)
	}
}

func TestAppendTaskRecord_ReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	// Make the directory read-only so temp file creation fails.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	err := appendTaskRecord(path, "<!-- failure: test -->")
	if err == nil {
		t.Fatal("expected error when directory is read-only, got nil")
	}
	if !strings.Contains(err.Error(), "create temp") {
		t.Fatalf("error should mention temp file creation, got: %v", err)
	}
}
