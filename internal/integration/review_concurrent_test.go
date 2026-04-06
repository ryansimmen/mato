package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"mato/internal/dirs"
	"mato/internal/queue"
	"mato/internal/queueview"
	"mato/internal/runner"
	"mato/internal/testutil"
)

// TestConcurrentReviewLockRace verifies that when two goroutines race to
// acquire the review lock for the same task, exactly one succeeds and the
// other is rejected. This tests the mutual-exclusion guarantee of the
// per-task review lock.
func TestConcurrentReviewLockRace(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "review-race.md"
	writeTask(t, tasksDir, dirs.ReadyReview, taskFile, "# Review race\nTest task.\n")

	const numGoroutines = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	var wins atomic.Int32
	var losses atomic.Int32
	cleanups := make([]func(), numGoroutines)
	var panics atomic.Int32

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()

			<-start
			cleanup, ok := queue.AcquireReviewLock(tasksDir, taskFile)
			if ok {
				wins.Add(1)
				cleanups[id] = cleanup
			} else {
				losses.Add(1)
			}
		}(g)
	}

	close(start)
	wg.Wait()

	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}
	if got := wins.Load(); got != 1 {
		t.Fatalf("expected exactly 1 lock winner, got %d", got)
	}
	if got := losses.Load(); got != int32(numGoroutines-1) {
		t.Fatalf("expected %d lock losers, got %d", numGoroutines-1, got)
	}

	// Verify the lock file exists.
	lockPath := filepath.Join(tasksDir, ".locks", "review-"+taskFile+".lock")
	mustExist(t, lockPath)

	// Release the lock.
	for _, cleanup := range cleanups {
		if cleanup != nil {
			cleanup()
		}
	}

	// Lock file should be removed after cleanup.
	mustNotExist(t, lockPath)
}

// TestStaleReviewLockReclaim verifies that a stale review lock left by a
// dead process can be reclaimed by a later reviewer. The stale lock is
// simulated by writing a lock file with a PID that does not exist.
func TestStaleReviewLockReclaim(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "stale-review.md"
	writeTask(t, tasksDir, dirs.ReadyReview, taskFile, "# Stale review\nTest task.\n")

	// Simulate a stale lock from a dead process (PID 2147483647 unlikely alive).
	locksDir := filepath.Join(tasksDir, ".locks")
	staleLockPath := filepath.Join(locksDir, "review-"+taskFile+".lock")
	if err := os.WriteFile(staleLockPath, []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	mustExist(t, staleLockPath)

	// A new reviewer should be able to reclaim the stale lock.
	cleanup, ok := queue.AcquireReviewLock(tasksDir, taskFile)
	if !ok || cleanup == nil {
		t.Fatal("expected to reclaim stale review lock, but acquisition failed")
	}

	// Verify the lock file now contains the current process identity (not the stale one).
	data, err := os.ReadFile(staleLockPath)
	if err != nil {
		t.Fatalf("read reclaimed lock: %v", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "2147483647" {
		t.Fatal("lock file still contains stale PID after reclaim")
	}

	cleanup()
	mustNotExist(t, staleLockPath)
}

// TestCleanStaleReviewLocks verifies that CleanStaleReviewLocks removes
// review lock files belonging to dead processes while leaving live locks
// and non-review locks untouched.
func TestCleanStaleReviewLocks(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	locksDir := filepath.Join(tasksDir, ".locks")

	// Create a stale review lock (dead PID).
	staleLock := filepath.Join(locksDir, "review-stale-task.md.lock")
	if err := os.WriteFile(staleLock, []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}

	// Create a non-review lock file (should not be touched).
	nonReviewLock := filepath.Join(locksDir, "merge.lock")
	if err := os.WriteFile(nonReviewLock, []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("write non-review lock: %v", err)
	}

	// Create a .pid file (should not be touched by CleanStaleReviewLocks).
	pidFile := filepath.Join(locksDir, "some-agent.pid")
	if err := os.WriteFile(pidFile, []byte("2147483647"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	queue.CleanStaleReviewLocks(tasksDir)

	// Stale review lock should be removed.
	mustNotExist(t, staleLock)

	// Non-review lock and pid file should be untouched.
	mustExist(t, nonReviewLock)
	mustExist(t, pidFile)
}

// TestReviewLocksIndependent verifies that review locks for different tasks
// do not block each other. Two tasks should be independently lockable.
func TestReviewLocksIndependent(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskA := "review-task-a.md"
	taskB := "review-task-b.md"
	writeTask(t, tasksDir, dirs.ReadyReview, taskA, "# Review A\nTest task A.\n")
	writeTask(t, tasksDir, dirs.ReadyReview, taskB, "# Review B\nTest task B.\n")

	// Acquire lock for task A.
	cleanupA, okA := queue.AcquireReviewLock(tasksDir, taskA)
	if !okA || cleanupA == nil {
		t.Fatal("expected to acquire review lock for task A")
	}

	// Acquire lock for task B — should succeed independently.
	cleanupB, okB := queue.AcquireReviewLock(tasksDir, taskB)
	if !okB || cleanupB == nil {
		t.Fatal("expected to acquire review lock for task B while A is held")
	}

	// Verify both lock files exist.
	lockA := filepath.Join(tasksDir, ".locks", "review-"+taskA+".lock")
	lockB := filepath.Join(tasksDir, ".locks", "review-"+taskB+".lock")
	mustExist(t, lockA)
	mustExist(t, lockB)

	// A second attempt on task A should fail while it's held.
	_, okA2 := queue.AcquireReviewLock(tasksDir, taskA)
	if okA2 {
		t.Fatal("second lock on task A should fail while first is held")
	}

	// Release A; acquiring A again should now succeed.
	cleanupA()
	mustNotExist(t, lockA)

	cleanupA3, okA3 := queue.AcquireReviewLock(tasksDir, taskA)
	if !okA3 || cleanupA3 == nil {
		t.Fatal("expected to re-acquire review lock for task A after release")
	}
	cleanupA3()

	// B should still be held.
	mustExist(t, lockB)
	cleanupB()
	mustNotExist(t, lockB)
}

// TestConcurrentReviewLockMultipleTasks verifies that multiple goroutines
// racing to lock different tasks each succeed, and no task is double-locked.
func TestConcurrentReviewLockMultipleTasks(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	const numTasks = 5
	const goroutinesPerTask = 4

	// Create tasks in ready-for-review/.
	taskFiles := make([]string, numTasks)
	for i := 0; i < numTasks; i++ {
		taskFiles[i] = "concurrent-review-" + string(rune('a'+i)) + ".md"
		writeTask(t, tasksDir, dirs.ReadyReview, taskFiles[i],
			"# Concurrent review "+string(rune('A'+i))+"\nTest.\n")
	}

	type result struct {
		taskIdx int
		won     bool
		cleanup func()
	}

	total := numTasks * goroutinesPerTask
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]result, total)
	var panics atomic.Int32

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()

			taskIdx := idx % numTasks
			<-start
			cleanup, ok := queue.AcquireReviewLock(tasksDir, taskFiles[taskIdx])
			results[idx] = result{taskIdx: taskIdx, won: ok, cleanup: cleanup}
		}(i)
	}

	close(start)
	wg.Wait()

	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}

	// Count wins per task.
	winsPerTask := make(map[int]int)
	for _, r := range results {
		if r.won {
			winsPerTask[r.taskIdx]++
		}
	}

	// Each task should have exactly 1 winner.
	for i := 0; i < numTasks; i++ {
		wins := winsPerTask[i]
		if wins != 1 {
			t.Errorf("task %d (%s): expected 1 winner, got %d", i, taskFiles[i], wins)
		}
	}

	// Verify all lock files exist.
	for _, tf := range taskFiles {
		lockPath := filepath.Join(tasksDir, ".locks", "review-"+tf+".lock")
		mustExist(t, lockPath)
	}

	// Clean up all locks.
	for _, r := range results {
		if r.cleanup != nil {
			r.cleanup()
		}
	}

	// Verify all lock files are removed.
	for _, tf := range taskFiles {
		lockPath := filepath.Join(tasksDir, ".locks", "review-"+tf+".lock")
		mustNotExist(t, lockPath)
	}
}

// TestReviewLockAcquireReleaseCycle verifies that a review lock can be
// repeatedly acquired and released without leaving stale state.
func TestReviewLockAcquireReleaseCycle(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	taskFile := "cycle-review.md"
	writeTask(t, tasksDir, dirs.ReadyReview, taskFile, "# Cycle review\nTest.\n")

	lockPath := filepath.Join(tasksDir, ".locks", "review-"+taskFile+".lock")

	for i := 0; i < 10; i++ {
		cleanup, ok := queue.AcquireReviewLock(tasksDir, taskFile)
		if !ok || cleanup == nil {
			t.Fatalf("iteration %d: expected lock acquisition to succeed", i)
		}
		mustExist(t, lockPath)

		// While held, a second acquire should fail.
		_, ok2 := queue.AcquireReviewLock(tasksDir, taskFile)
		if ok2 {
			t.Fatalf("iteration %d: second acquire should fail while held", i)
		}

		cleanup()
		mustNotExist(t, lockPath)
	}
}

// TestConcurrentReviewSelection_RequiresBranchMarkers verifies that the
// exported review selection path returns explicitly marked tasks and skips
// marker-less handoff files with a recorded review failure.
func TestConcurrentReviewSelection_RequiresBranchMarkers(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// One valid review task with an explicit branch marker, plus one corrupted
	// handoff missing the required marker.
	writeTask(t, tasksDir, dirs.ReadyReview, "add_feature.md",
		"---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Underscore\n")
	writeTask(t, tasksDir, dirs.ReadyReview, "add-feature.md",
		"<!-- branch: task/add-feature -->\n---\npriority: 20\nmax_retries: 3\n---\n# Add Feature Dash\n")

	idx := queueview.BuildIndex(tasksDir)
	first := runner.SelectTaskForReview(tasksDir, idx)
	if first == nil {
		t.Fatal("expected a review candidate, got nil")
	}
	if first.Branch != "task/add-feature" {
		t.Fatalf("candidate branch = %q, want %q", first.Branch, "task/add-feature")
	}

	// Remove the valid candidate so the next selection only sees the corrupted task.
	src := filepath.Join(tasksDir, dirs.ReadyReview, first.Filename)
	dst := filepath.Join(tasksDir, dirs.InProgress, first.Filename)
	if err := os.Rename(src, dst); err != nil {
		t.Fatalf("move first task: %v", err)
	}

	idx2 := queueview.BuildIndex(tasksDir)
	second := runner.SelectTaskForReview(tasksDir, idx2)
	if second != nil {
		t.Fatalf("expected nil candidate for marker-less handoff, got %+v", second)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, "add_feature.md"))
	if err != nil {
		t.Fatalf("ReadFile add_feature.md: %v", err)
	}
	if !strings.Contains(string(data), "missing required") || !strings.Contains(string(data), "ready-for-review") {
		t.Fatalf("expected review-failure marker, got:\n%s", string(data))
	}
}
