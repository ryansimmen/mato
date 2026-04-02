package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mato/internal/lockfile"
	"mato/internal/process"
)

func TestCleanStaleLocks_RemovesDeadProcessLock(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Write a .pid lock file with a PID that is definitely not running.
	staleLock := filepath.Join(locksDir, "dead-agent.pid")
	if err := os.WriteFile(staleLock, []byte("999999:0"), 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	CleanStaleLocks(tasksDir)

	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatalf("stale lock should have been removed, stat returned: %v", err)
	}
}

func TestCleanStaleLocks_PreservesLiveProcessLock(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Write a .pid lock file for our own process (definitely alive).
	identity := process.LockIdentity(os.Getpid())
	liveLock := filepath.Join(locksDir, "live-agent.pid")
	if err := os.WriteFile(liveLock, []byte(identity), 0o644); err != nil {
		t.Fatalf("write live lock: %v", err)
	}

	CleanStaleLocks(tasksDir)

	if _, err := os.Stat(liveLock); err != nil {
		t.Fatalf("live lock should be preserved: %v", err)
	}
}

func TestCleanStaleLocks_SkipsDirectories(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Create a subdirectory with .pid suffix — should be skipped.
	subDir := filepath.Join(locksDir, "subdir.pid")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subdir.pid: %v", err)
	}

	CleanStaleLocks(tasksDir) // Should not panic or remove directories.

	if _, err := os.Stat(subDir); err != nil {
		t.Fatalf("directory should be preserved: %v", err)
	}
}

func TestCleanStaleLocks_SkipsNonPidFiles(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// A .lock file (not .pid) should be ignored.
	lockFile := filepath.Join(locksDir, "some.lock")
	if err := os.WriteFile(lockFile, []byte("999999:0"), 0o644); err != nil {
		t.Fatalf("write .lock file: %v", err)
	}

	CleanStaleLocks(tasksDir)

	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf(".lock file should be preserved: %v", err)
	}
}

func TestCleanStaleLocks_EmptyLocksDir(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Should not panic on empty directory.
	CleanStaleLocks(tasksDir)
}

func TestCleanStaleLocks_MissingLocksDir(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Remove .locks if it exists.
	os.RemoveAll(filepath.Join(tasksDir, DirLocks))

	// Should not panic when .locks doesn't exist.
	CleanStaleLocks(tasksDir)
}

func TestCleanStaleLocks_PreservesUnreadableLock(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}
	lockPath := filepath.Join(locksDir, "unknown-agent.pid")
	if err := os.WriteFile(lockPath, []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	orig := lockfile.TestHookReadFile()
	lockfile.SetTestHookReadFile(func(path string) ([]byte, error) {
		if path == lockPath {
			return nil, fmt.Errorf("permission denied")
		}
		return orig(path)
	})
	t.Cleanup(func() { lockfile.SetTestHookReadFile(orig) })

	CleanStaleLocks(tasksDir)

	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("unreadable lock should be preserved: %v", err)
	}
}

func TestAcquireReviewLock_SuccessWhenNoLock(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	cleanup, ok := AcquireReviewLock(tasksDir, "test-task.md")
	if !ok {
		t.Fatal("AcquireReviewLock should succeed when no lock exists")
	}
	defer cleanup()

	// Lock file should exist.
	lockPath := filepath.Join(locksDir, "review-test-task.md.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}
}

func TestAcquireReviewLock_FailsWhenHeldByLiveProcess(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Acquire the lock first.
	cleanup1, ok1 := AcquireReviewLock(tasksDir, "test-task.md")
	if !ok1 {
		t.Fatal("first AcquireReviewLock should succeed")
	}
	defer cleanup1()

	// Second acquisition should fail.
	cleanup2, ok2 := AcquireReviewLock(tasksDir, "test-task.md")
	if ok2 {
		cleanup2()
		t.Fatal("second AcquireReviewLock should fail when lock is held")
	}
}

func TestAcquireReviewLock_ReclaimsStaleReviewLock(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Write a stale lock file with a dead process PID.
	lockPath := filepath.Join(locksDir, "review-test-task.md.lock")
	if err := os.WriteFile(lockPath, []byte("999999:0"), 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	// Should be able to acquire since the lock holder is dead.
	cleanup, ok := AcquireReviewLock(tasksDir, "test-task.md")
	if !ok {
		t.Fatal("AcquireReviewLock should reclaim stale lock from dead process")
	}
	cleanup()
}

func TestAcquireReviewLock_CleanupRemovesLock(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	cleanup, ok := AcquireReviewLock(tasksDir, "test-task.md")
	if !ok {
		t.Fatal("AcquireReviewLock should succeed")
	}

	lockPath := filepath.Join(locksDir, "review-test-task.md.lock")

	// Call cleanup — lock should be removed.
	cleanup()

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file should be removed after cleanup, stat: %v", err)
	}

	// Should be acquirable again.
	cleanup2, ok2 := AcquireReviewLock(tasksDir, "test-task.md")
	if !ok2 {
		t.Fatal("AcquireReviewLock should succeed after cleanup")
	}
	cleanup2()
}

func TestCleanStaleReviewLocks_RemovesDeadProcess(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Write a stale review lock.
	staleLock := filepath.Join(locksDir, "review-dead-task.md.lock")
	if err := os.WriteFile(staleLock, []byte("999999:0"), 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	CleanStaleReviewLocks(tasksDir)

	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatalf("stale review lock should have been removed: %v", err)
	}
}

func TestCleanStaleReviewLocks_PreservesLiveProcess(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// Acquire a real review lock (our own PID).
	cleanup, ok := AcquireReviewLock(tasksDir, "live-task.md")
	if !ok {
		t.Fatal("AcquireReviewLock should succeed")
	}
	defer cleanup()

	CleanStaleReviewLocks(tasksDir)

	lockPath := filepath.Join(locksDir, "review-live-task.md.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("live review lock should be preserved: %v", err)
	}
}

func TestCleanStaleReviewLocks_SkipsNonReviewLocks(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	locksDir := filepath.Join(tasksDir, DirLocks)
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}

	// A non-review .lock file should be ignored.
	nonReview := filepath.Join(locksDir, "agent.lock")
	if err := os.WriteFile(nonReview, []byte("999999:0"), 0o644); err != nil {
		t.Fatalf("write non-review lock: %v", err)
	}

	CleanStaleReviewLocks(tasksDir)

	if _, err := os.Stat(nonReview); err != nil {
		t.Fatalf("non-review lock should be preserved: %v", err)
	}
}

func TestCleanStaleReviewLocks_MissingLocksDir(t *testing.T) {
	tasksDir := setupTasksDirs(t)
	os.RemoveAll(filepath.Join(tasksDir, DirLocks))

	// Should not panic.
	CleanStaleReviewLocks(tasksDir)
}
