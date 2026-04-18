package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryansimmen/mato/internal/process"
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
	activeHolders := 0
	maxConcurrent := 0
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			release, ok := Acquire(dir, "race")
			if ok {
				mu.Lock()
				winners++
				activeHolders++
				if activeHolders > maxConcurrent {
					maxConcurrent = activeHolders
				}
				mu.Unlock()
				// Hold the lock briefly to let other goroutines attempt.
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				activeHolders--
				mu.Unlock()
				release()
			}
		}()
	}
	wg.Wait()

	// The real invariant is mutual exclusion: even if multiple goroutines win
	// sequentially as earlier holders release the lock, there must never be
	// more than one active holder at a time.
	if winners == 0 {
		t.Fatal("expected at least one winner among concurrent acquires")
	}
	if maxConcurrent != 1 {
		t.Errorf("expected max 1 concurrent lock holder, got %d", maxConcurrent)
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

func TestEmptyLockFileIsTreatedAsStale(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "empty.lock")

	// An empty lock file (e.g. from a crash between create and write)
	// should be treated as stale — removed and reacquired on the first call.
	if err := os.WriteFile(lockPath, []byte(""), 0o644); err != nil {
		t.Fatalf("writing empty lock: %v", err)
	}

	release, ok := Acquire(dir, "empty")
	if !ok {
		t.Fatal("Acquire should succeed by reclaiming an empty (stale) lock file")
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

func TestWhitespaceOnlyLockFileIsTreatedAsStale(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "ws.lock")

	// A lock file containing only whitespace should also be treated as stale.
	if err := os.WriteFile(lockPath, []byte("  \n\t"), 0o644); err != nil {
		t.Fatalf("writing whitespace lock: %v", err)
	}

	release, ok := Acquire(dir, "ws")
	if !ok {
		t.Fatal("Acquire should succeed by reclaiming a whitespace-only (stale) lock file")
	}
	defer release()
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

// --- CheckHeld tests ---

func TestCheckHeld_LiveProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "live.lock")
	if err := os.WriteFile(lockPath, []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}
	held, err := CheckHeld(lockPath)
	if err != nil {
		t.Fatalf("CheckHeld returned unexpected error: %v", err)
	}
	if !held {
		t.Error("CheckHeld should return true for a lock held by the current process")
	}
}

func TestCheckHeld_DeadProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "dead.lock")
	if err := os.WriteFile(lockPath, []byte("4194300:99999999"), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}
	held, err := CheckHeld(lockPath)
	if err != nil {
		t.Fatalf("CheckHeld returned unexpected error: %v", err)
	}
	if held {
		t.Error("CheckHeld should return false for a lock held by a dead process")
	}
}

func TestCheckHeld_MissingFile(t *testing.T) {
	held, err := CheckHeld("/nonexistent/path/to/lock")
	if err != nil {
		t.Fatalf("CheckHeld should return nil error for missing file, got: %v", err)
	}
	if held {
		t.Error("CheckHeld should return false for a missing file")
	}
}

func TestCheckHeld_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "empty.lock")
	if err := os.WriteFile(lockPath, []byte(""), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}
	held, err := CheckHeld(lockPath)
	if err != nil {
		t.Fatalf("CheckHeld returned unexpected error: %v", err)
	}
	if held {
		t.Error("CheckHeld should return false for an empty lock file")
	}
}

func TestCheckHeld_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "unreadable.lock")
	if err := os.WriteFile(lockPath, []byte("12345:99999"), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}
	orig := osReadFile
	osReadFile = func(path string) ([]byte, error) {
		if path == lockPath {
			return nil, fmt.Errorf("permission denied")
		}
		return orig(path)
	}
	t.Cleanup(func() { osReadFile = orig })

	held, err := CheckHeld(lockPath)
	if err == nil {
		t.Fatal("CheckHeld should return an error for an unreadable lock file")
	}
	if held {
		t.Error("CheckHeld should return false when the file is unreadable")
	}
}

func TestReadMetadata_LiveProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "live.lock")
	identity := process.LockIdentity(os.Getpid())
	if err := os.WriteFile(lockPath, []byte(identity), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	meta, err := ReadMetadata(lockPath)
	if err != nil {
		t.Fatalf("ReadMetadata returned unexpected error: %v", err)
	}
	if meta.Status != StatusActive {
		t.Fatalf("ReadMetadata status = %v, want %v", meta.Status, StatusActive)
	}
	if meta.PID != os.Getpid() {
		t.Fatalf("ReadMetadata PID = %d, want %d", meta.PID, os.Getpid())
	}
	if meta.Identity != identity {
		t.Fatalf("ReadMetadata identity = %q, want %q", meta.Identity, identity)
	}
}

func TestReadMetadata_LegacyPIDOnly(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "legacy.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	meta, err := ReadMetadata(lockPath)
	if err != nil {
		t.Fatalf("ReadMetadata returned unexpected error: %v", err)
	}
	if meta.Status != StatusActive {
		t.Fatalf("ReadMetadata status = %v, want %v", meta.Status, StatusActive)
	}
	if meta.PID != os.Getpid() {
		t.Fatalf("ReadMetadata PID = %d, want %d", meta.PID, os.Getpid())
	}
}

func TestReadMetadata_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "unreadable.lock")
	if err := os.WriteFile(lockPath, []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatalf("writing lock: %v", err)
	}

	orig := osReadFile
	osReadFile = func(path string) ([]byte, error) {
		if path == lockPath {
			return nil, fmt.Errorf("permission denied")
		}
		return orig(path)
	}
	t.Cleanup(func() { osReadFile = orig })

	meta, err := ReadMetadata(lockPath)
	if err == nil {
		t.Fatal("ReadMetadata should return an error for an unreadable file")
	}
	if meta.Status != StatusUnknown {
		t.Fatalf("ReadMetadata status = %v, want %v", meta.Status, StatusUnknown)
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

// --- Acquire retry race condition tests ---

func TestAcquire_RetryRace_StaleVanishes(t *testing.T) {
	// Simulates the race where a stale lock file is removed by another
	// process between OpenFile returning EEXIST and ReadFile. The first
	// ReadFile returns ENOENT, triggering the continue path (line 91-93)
	// in the retry loop. The second iteration's OpenFile succeeds.
	orig := osReadFile
	defer func() { osReadFile = orig }()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "race.lock")

	// Write a stale lock so the first OpenFile returns EEXIST.
	if err := os.WriteFile(lockPath, []byte("4194300:99999999"), 0o644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}

	calls := 0
	osReadFile = func(name string) ([]byte, error) {
		calls++
		if calls == 1 {
			// Simulate lock vanishing before we can read it.
			os.Remove(lockPath)
			return nil, os.ErrNotExist
		}
		return orig(name)
	}

	release, ok := Acquire(dir, "race")
	if !ok {
		t.Fatal("Acquire should succeed after retry when stale lock vanishes")
	}
	defer release()

	if calls < 1 {
		t.Error("osReadFile should have been called at least once")
	}

	// Verify the lock file contains the current process identity.
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}
	expected := process.LockIdentity(os.Getpid())
	if string(data) != expected {
		t.Errorf("lock identity = %q, want %q", string(data), expected)
	}
}

func TestAcquire_RetriesWithJitterBeforeEventuallySucceeding(t *testing.T) {
	origLink := osLink
	origReadFile := osReadFile
	origSleep := acquireRetrySleepFn
	origJitter := acquireRetryJitterFn
	defer func() {
		osLink = origLink
		osReadFile = origReadFile
		acquireRetrySleepFn = origSleep
		acquireRetryJitterFn = origJitter
	}()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "retry.lock")

	linkCalls := 0
	osLink = func(oldname, newname string) error {
		linkCalls++
		if linkCalls < 3 {
			return os.ErrExist
		}
		return origLink(oldname, newname)
	}
	osReadFile = func(string) ([]byte, error) {
		return nil, os.ErrNotExist
	}

	var slept []time.Duration
	acquireRetrySleepFn = func(delay time.Duration) {
		slept = append(slept, delay)
	}

	const jitter = 3 * time.Millisecond
	jitterCalls := 0
	acquireRetryJitterFn = func(limit time.Duration) time.Duration {
		jitterCalls++
		if limit != acquireRetryJitterMax {
			t.Fatalf("jitter limit = %v, want %v", limit, acquireRetryJitterMax)
		}
		return jitter
	}

	release, ok := Acquire(dir, "retry")
	if !ok {
		t.Fatal("Acquire should keep retrying after transient contention races")
	}
	defer release()

	if linkCalls != 3 {
		t.Fatalf("osLink calls = %d, want 3", linkCalls)
	}
	if jitterCalls != 2 {
		t.Fatalf("acquireRetryJitterFn calls = %d, want 2", jitterCalls)
	}
	if len(slept) != 2 {
		t.Fatalf("sleep calls = %d, want 2", len(slept))
	}
	for i, delay := range slept {
		want := acquireRetryBaseDelay + jitter
		if delay != want {
			t.Fatalf("sleep %d = %v, want %v", i, delay, want)
		}
	}

	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}
	expected := process.LockIdentity(os.Getpid())
	if string(data) != expected {
		t.Fatalf("lock identity = %q, want %q", string(data), expected)
	}
}

func TestAcquire_LiveLockNotReclaimed(t *testing.T) {
	// Create a lock file with the current process's real identity.
	// Acquire must detect the live holder and return (nil, false)
	// without modifying the lock file.
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "live.lock")

	liveIdentity := process.LockIdentity(os.Getpid())
	if err := os.WriteFile(lockPath, []byte(liveIdentity), 0o644); err != nil {
		t.Fatalf("writing live lock: %v", err)
	}

	_, ok := Acquire(dir, "live")
	if ok {
		t.Fatal("Acquire should fail when lock is held by a live process")
	}

	// Verify the existing lock file was not modified.
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("reading lock file: %v", err)
	}
	if string(data) != liveIdentity {
		t.Errorf("lock file was modified: got %q, want %q", string(data), liveIdentity)
	}
}

func TestAcquire_ContenderSucceedsAfterPeerRemovesStaleLock(t *testing.T) {
	origSleep := acquireRetrySleepFn
	origJitter := acquireRetryJitterFn
	defer func() {
		acquireRetrySleepFn = origSleep
		acquireRetryJitterFn = origJitter
	}()

	dir := t.TempDir()
	lockPath := filepath.Join(dir, "dual.lock")
	if err := os.WriteFile(lockPath, []byte("4194300:99999999"), 0o644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}

	pausedRetry := make(chan struct{})
	releaseRetry := make(chan struct{})
	var sleepCalls atomic.Int32
	acquireRetryJitterFn = func(time.Duration) time.Duration { return 0 }
	acquireRetrySleepFn = func(time.Duration) {
		if sleepCalls.Add(1) != 1 {
			return
		}
		close(pausedRetry)
		<-releaseRetry
	}

	type result struct {
		release func()
		ok      bool
	}
	firstResult := make(chan result, 1)
	go func() {
		release, ok := Acquire(dir, "dual")
		firstResult <- result{release: release, ok: ok}
	}()

	<-pausedRetry

	secondRelease, secondOK := Acquire(dir, "dual")
	if !secondOK {
		t.Fatal("second Acquire should succeed after another caller removes the stale lock")
	}
	defer secondRelease()

	close(releaseRetry)

	first := <-firstResult
	if first.ok {
		first.release()
		t.Fatal("first Acquire should not take over once another caller holds the live lock")
	}
}

func TestAcquire_ConcurrentTwoGoroutines(t *testing.T) {
	// Repeat several times to reduce the chance of passing by accident
	// when the scheduler serializes the two goroutines.
	for round := 0; round < 5; round++ {
		dir := t.TempDir()

		var mu sync.Mutex
		winners := 0
		var winnerRelease func()

		var wg sync.WaitGroup
		wg.Add(2)

		// Barrier: both goroutines signal readiness, then block until
		// the gate is closed so they race into Acquire together.
		ready := make(chan struct{}, 2)
		gate := make(chan struct{})

		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				ready <- struct{}{}
				<-gate
				release, ok := Acquire(dir, "dual")
				if ok {
					mu.Lock()
					winners++
					winnerRelease = release
					mu.Unlock()
				}
			}()
		}

		// Wait for both goroutines to be ready, then release them.
		<-ready
		<-ready
		close(gate)

		wg.Wait()

		if winners != 1 {
			t.Fatalf("round %d: expected exactly 1 winner among 2 concurrent acquires, got %d", round, winners)
		}

		if winnerRelease != nil {
			winnerRelease()
		}

		// After release, the lock file should be gone.
		lockPath := filepath.Join(dir, "dual.lock")
		if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
			t.Errorf("round %d: lock file should be removed after winner releases", round)
		}
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
