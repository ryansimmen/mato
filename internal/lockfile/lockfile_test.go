package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// --- IsHeld tests ---

func TestIsHeld_LiveProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "live.lock")
	if err := os.WriteFile(lockPath, []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	if !IsHeld(lockPath) {
		t.Error("IsHeld should return true for a lock held by the current process")
	}
}

func TestIsHeld_DeadProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "dead.lock")
	if err := os.WriteFile(lockPath, []byte("4194300:99999999"), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	if IsHeld(lockPath) {
		t.Error("IsHeld should return false for a lock held by a dead process")
	}
}

func TestIsHeld_MissingFile(t *testing.T) {
	if IsHeld("/nonexistent/path/to/lock") {
		t.Error("IsHeld should return false for a missing file")
	}
}

func TestIsHeld_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "empty.lock")
	if err := os.WriteFile(lockPath, []byte(""), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	if IsHeld(lockPath) {
		t.Error("IsHeld should return false for an empty lock file")
	}
}

func TestIsHeld_PIDReuse(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "reuse.lock")
	// Write our PID but a bogus start time — simulates PID reuse.
	bogus := fmt.Sprintf("%d:0", os.Getpid())
	if err := os.WriteFile(lockPath, []byte(bogus), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	if IsHeld(lockPath) {
		t.Error("IsHeld should return false when PID start time doesn't match (PID reuse)")
	}
}

// --- Register tests ---

func TestRegister(t *testing.T) {
	dir := t.TempDir()

	cleanup, err := Register(dir, "test-agent")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	pidFile := filepath.Join(dir, "test-agent.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("pid file not created: %v", err)
	}
	expected := process.LockIdentity(os.Getpid())
	if string(data) != expected {
		t.Errorf("pid file content = %q, want %q", string(data), expected)
	}

	cleanup()

	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove pid file")
	}
}

func TestRegister_CreatesDir(t *testing.T) {
	base := t.TempDir()
	locksDir := filepath.Join(base, "nested", "locks")

	cleanup, err := Register(locksDir, "agent")
	if err != nil {
		t.Fatalf("Register should create missing directory: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(locksDir)
	if err != nil {
		t.Fatalf("locks directory should exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("locks path should be a directory")
	}
}

func TestRegister_MkdirAllFailure(t *testing.T) {
	base := t.TempDir()
	// Place a regular file where MkdirAll needs to create a directory.
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}
	locksDir := filepath.Join(blocker, "sub")
	_, err := Register(locksDir, "agent")
	if err == nil {
		t.Fatal("Register should fail when locks directory cannot be created")
	}
}

func TestRegister_WriteFailure(t *testing.T) {
	dir := t.TempDir()
	// Create a directory at the .pid path so WriteFile fails with EISDIR.
	pidDir := filepath.Join(dir, "agent.pid")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("creating blocker directory: %v", err)
	}
	_, err := Register(dir, "agent")
	if err == nil {
		t.Fatal("Register should fail when pid file path is a directory")
	}
}

func TestRegister_CleanupAfterExternalRemoval(t *testing.T) {
	dir := t.TempDir()
	cleanup, err := Register(dir, "agent")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	pidFile := filepath.Join(dir, "agent.pid")
	if err := os.Remove(pidFile); err != nil {
		t.Fatalf("removing pid file: %v", err)
	}
	// Cleanup must not panic when the file is already gone.
	cleanup()
}

func TestRegister_Overwrites(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "overwrite.pid")

	// Write an existing file with stale content.
	if err := os.WriteFile(pidFile, []byte("oldcontent"), 0o644); err != nil {
		t.Fatalf("writing old pid file: %v", err)
	}

	cleanup, err := Register(dir, "overwrite")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading pid file: %v", err)
	}
	expected := process.LockIdentity(os.Getpid())
	if string(data) != expected {
		t.Errorf("pid file content = %q, want %q (should overwrite old content)", string(data), expected)
	}
}

// --- Acquire I/O failure and cleanup path tests ---

func TestAcquire_MkdirAllFailure(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("creating blocker file: %v", err)
	}
	locksDir := filepath.Join(blocker, "sub")
	_, ok := Acquire(locksDir, "test")
	if ok {
		t.Fatal("Acquire should fail when locks directory cannot be created")
	}
}

func TestAcquire_NonExistCreateError(t *testing.T) {
	dir := t.TempDir()
	// A filename longer than NAME_MAX (255) triggers ENAMETOOLONG,
	// an unrecoverable error unrelated to an existing lock.
	longName := strings.Repeat("x", 300)
	_, ok := Acquire(dir, longName)
	if ok {
		t.Fatal("Acquire should return false on unrecoverable create error")
	}
}

func TestAcquire_StaleLockReadFailure(t *testing.T) {
	orig := osReadFile
	defer func() { osReadFile = orig }()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")
	if err := os.WriteFile(lockPath, []byte("4194300:99999999"), 0o644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}

	osReadFile = func(string) ([]byte, error) {
		return nil, fmt.Errorf("injected read error")
	}

	_, ok := Acquire(dir, "test")
	if ok {
		t.Fatal("Acquire should fail when stale lock file cannot be read")
	}
}

func TestAcquire_StaleLockRemoveFailure(t *testing.T) {
	orig := osRemove
	defer func() { osRemove = orig }()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")
	if err := os.WriteFile(lockPath, []byte("4194300:99999999"), 0o644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}

	osRemove = func(string) error {
		return fmt.Errorf("injected remove error")
	}

	_, ok := Acquire(dir, "test")
	if ok {
		t.Fatal("Acquire should fail when stale lock cannot be removed")
	}
}

func TestAcquire_CleanupAfterExternalRemoval(t *testing.T) {
	dir := t.TempDir()
	release, ok := Acquire(dir, "test")
	if !ok {
		t.Fatal("Acquire should succeed")
	}

	lockPath := filepath.Join(dir, "test.lock")
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("removing lock file: %v", err)
	}
	// Cleanup must not panic when the lock file is already gone.
	release()
}
