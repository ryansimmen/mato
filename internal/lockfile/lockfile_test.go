package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mato/internal/process"
)

func TestAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()

	release, ok := Acquire(dir, "test")
	if !ok {
		t.Fatal("expected Acquire to succeed")
	}

	lockPath := filepath.Join(dir, "test.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file should exist after Acquire: %v", err)
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("lock file should contain identity string")
	}

	release()

	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatal("lock file should be removed after release")
	}
}

func TestDoubleAcquireFails(t *testing.T) {
	dir := t.TempDir()

	release, ok := Acquire(dir, "test")
	if !ok {
		t.Fatal("first Acquire should succeed")
	}
	defer release()

	_, ok2 := Acquire(dir, "test")
	if ok2 {
		t.Fatal("second Acquire should fail while lock is held")
	}
}

func TestStaleLockReclamation(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	// Write a lock file with a PID that almost certainly doesn't exist.
	// Use a very high PID that's virtually guaranteed to be unused.
	stalePID := 4194300 // near Linux PID_MAX_LIMIT
	staleIdentity := fmt.Sprintf("%d:99999999", stalePID)
	if err := os.WriteFile(lockPath, []byte(staleIdentity), 0o644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}

	release, ok := Acquire(dir, "test")
	if !ok {
		t.Fatal("Acquire should reclaim stale lock from dead process")
	}
	defer release()

	// Verify the lock file now contains our identity.
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading reclaimed lock: %v", err)
	}
	expected := process.LockIdentity(os.Getpid())
	if string(data) != expected {
		t.Errorf("lock identity = %q, want %q", string(data), expected)
	}
}

func TestPIDReuseDetection(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	// Write a lock file with our own PID but a bogus start time.
	// The process package should detect that the start time doesn't match
	// and treat the lock as stale.
	myPID := os.Getpid()
	bogusIdentity := fmt.Sprintf("%d:0", myPID)
	if err := os.WriteFile(lockPath, []byte(bogusIdentity), 0o644); err != nil {
		t.Fatalf("writing PID-reuse lock: %v", err)
	}

	release, ok := Acquire(dir, "test")
	if !ok {
		t.Fatal("Acquire should reclaim lock when PID start time doesn't match (PID reuse)")
	}
	defer release()
}

func TestConcurrentAcquireMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	const goroutines = 10

	var mu sync.Mutex
	winners := 0
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			release, ok := Acquire(dir, "race")
			if ok {
				mu.Lock()
				winners++
				mu.Unlock()
				// Hold the lock briefly to let other goroutines attempt.
				time.Sleep(5 * time.Millisecond)
				release()
			}
		}()
	}
	wg.Wait()

	// Because all goroutines share the same PID, only one should succeed
	// at any given moment. With the current non-blocking implementation
	// and no retry delay, exactly one goroutine wins.
	if winners != 1 {
		t.Errorf("expected exactly 1 winner among concurrent acquires, got %d", winners)
	}
}

func TestRetryAfterRelease(t *testing.T) {
	dir := t.TempDir()

	// First goroutine holds the lock, then releases after a short delay.
	release, ok := Acquire(dir, "retry")
	if !ok {
		t.Fatal("initial Acquire should succeed")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	// Second caller retries in a loop until it succeeds, simulating a
	// caller-level retry pattern on top of the non-blocking Acquire.
	var release2 func()
	deadline := time.After(2 * time.Second)
	for {
		var ok2 bool
		release2, ok2 = Acquire(dir, "retry")
		if ok2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting to acquire lock after release")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	release2()
}

func TestInternalRetryOnVanishedLockFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "vanish.lock")

	// Create a lock file that will be removed between OpenFile (EEXIST)
	// and ReadFile, exercising the continue path in the retry loop.
	// We create the file, then remove it in a very tight race window.
	// Since we can't perfectly time this, we instead simulate the
	// scenario by writing a stale lock (dead PID), letting Acquire
	// remove it and re-create it on the second iteration.
	stalePID := 4194301
	staleIdentity := fmt.Sprintf("%d:88888888", stalePID)
	if err := os.WriteFile(lockPath, []byte(staleIdentity), 0o644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}

	release, ok := Acquire(dir, "vanish")
	if !ok {
		t.Fatal("Acquire should succeed after reclaiming stale lock on retry")
	}
	defer release()
}

func TestAcquireCreatesLocksDir(t *testing.T) {
	base := t.TempDir()
	locksDir := filepath.Join(base, "nested", "locks")

	release, ok := Acquire(locksDir, "test")
	if !ok {
		t.Fatal("Acquire should create missing locks directory")
	}
	defer release()

	info, err := os.Stat(locksDir)
	if err != nil {
		t.Fatalf("locks directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("locks path should be a directory")
	}
}

func TestDifferentNamesAreIndependent(t *testing.T) {
	dir := t.TempDir()

	release1, ok1 := Acquire(dir, "alpha")
	if !ok1 {
		t.Fatal("Acquire alpha should succeed")
	}
	defer release1()

	release2, ok2 := Acquire(dir, "beta")
	if !ok2 {
		t.Fatal("Acquire beta should succeed while alpha is held")
	}
	defer release2()
}

func TestEmptyLockFileBlocksAcquire(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "empty.lock")

	// An empty lock file (no identity) is treated as held (not stale)
	// because the code checks content == "" and returns false.
	if err := os.WriteFile(lockPath, []byte(""), 0o644); err != nil {
		t.Fatalf("writing empty lock: %v", err)
	}

	_, ok := Acquire(dir, "empty")
	if ok {
		t.Fatal("Acquire should fail when lock file is empty (treated as held)")
	}
}
