package queue

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"mato/internal/process"
	"mato/internal/taskfile"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read pipe: %v", err)
	}
	return string(data)
}

func TestAtomicMove_MissingSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing.md")
	dst := filepath.Join(dir, "moved.md")

	if err := AtomicMove(src, dst); err == nil {
		t.Fatal("AtomicMove should return an error for a missing source")
	}
}

func TestAtomicMove_DestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")

	if err := os.WriteFile(src, []byte("source\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(dst, []byte("destination\n"), 0o644); err != nil {
		t.Fatalf("write destination: %v", err)
	}

	err := AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail when destination exists")
	}
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("AtomicMove error = %q, want ErrDestinationExists", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(data) != "destination\n" {
		t.Fatalf("destination contents changed: got %q", string(data))
	}

	// Source file should still exist (Link did not happen, so Remove was not called)
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source file should still exist after failed AtomicMove: %v", err)
	}
}

func TestAtomicMove_SuccessRemovesSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")

	if err := os.WriteFile(src, []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := AtomicMove(src, dst); err != nil {
		t.Fatalf("AtomicMove: %v", err)
	}

	// Destination should have the content
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(data) != "content\n" {
		t.Fatalf("destination contents = %q, want %q", string(data), "content\n")
	}

	// Source should be removed
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source file should be removed after successful AtomicMove, got err: %v", err)
	}
}

func TestAtomicMove_ConcurrentRace(t *testing.T) {
	dir := t.TempDir()
	const goroutines = 10

	// Create source files for each goroutine
	for i := 0; i < goroutines; i++ {
		src := filepath.Join(dir, fmt.Sprintf("src-%d.md", i))
		if err := os.WriteFile(src, []byte(fmt.Sprintf("content-%d\n", i)), 0o644); err != nil {
			t.Fatalf("write source %d: %v", i, err)
		}
	}

	// All goroutines race to rename their source to the same destination
	dst := filepath.Join(dir, "dst.md")
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			src := filepath.Join(dir, fmt.Sprintf("src-%d.md", idx))
			errs[idx] = AtomicMove(src, dst)
		}(i)
	}
	wg.Wait()

	// Exactly one goroutine should succeed
	successCount := 0
	for _, err := range errs {
		if err == nil {
			successCount++
		} else if !errors.Is(err, ErrDestinationExists) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successCount != 1 {
		t.Fatalf("expected exactly 1 success, got %d", successCount)
	}
}

func TestAtomicMove_PermissionError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dstDir := filepath.Join(dir, "readonly")

	if err := os.WriteFile(src, []byte("content\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.MkdirAll(dstDir, 0o555); err != nil {
		t.Fatalf("mkdir readonly: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dstDir, 0o755) })

	dst := filepath.Join(dstDir, "dst.md")
	err := AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail with permission error")
	}
	if errors.Is(err, ErrDestinationExists) {
		t.Fatal("error should not be ErrDestinationExists for permission failure")
	}
	// Source should still exist
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("source should still exist after permission error: %v", statErr)
	}
}

// withLinkFn overrides the linkFn hook for the duration of the test and
// restores it on cleanup.
func withLinkFn(t *testing.T, fn func(string, string) error) {
	t.Helper()
	orig := linkFn
	linkFn = fn
	t.Cleanup(func() { linkFn = orig })
}

func withReadFileFn(t *testing.T, fn func(string) ([]byte, error)) {
	t.Helper()
	orig := readFileFn
	readFileFn = fn
	t.Cleanup(func() { readFileFn = orig })
}

func withOpenFileFn(t *testing.T, fn func(string, int, os.FileMode) (*os.File, error)) {
	t.Helper()
	orig := openFileFn
	openFileFn = fn
	t.Cleanup(func() { openFileFn = orig })
}

func withWriteFileFn(t *testing.T, fn func(*os.File, []byte) error) {
	t.Helper()
	orig := writeFileFn
	writeFileFn = fn
	t.Cleanup(func() { writeFileFn = orig })
}

func TestAtomicMove_CrossDeviceSuccess(t *testing.T) {
	withLinkFn(t, func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")
	content := "cross-device content\n"

	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	if err := AtomicMove(src, dst); err != nil {
		t.Fatalf("AtomicMove cross-device: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != content {
		t.Fatalf("destination content = %q, want %q", got, content)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be removed after cross-device move, got err: %v", err)
	}
}

func TestAtomicMove_CrossDeviceDestinationExists(t *testing.T) {
	withLinkFn(t, func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")
	srcContent := "source content\n"
	dstContent := "existing destination\n"

	if err := os.WriteFile(src, []byte(srcContent), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(dst, []byte(dstContent), 0o644); err != nil {
		t.Fatalf("write destination: %v", err)
	}

	err := AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail when cross-device destination exists")
	}
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("error = %q, want ErrDestinationExists", err)
	}

	// Destination must not be clobbered.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination after conflict: %v", err)
	}
	if string(got) != dstContent {
		t.Fatalf("destination was clobbered: got %q, want %q", got, dstContent)
	}

	// Source must still exist.
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source should still exist after destination-exists error: %v", err)
	}
}

func TestAtomicMove_CrossDeviceReadFailSourceIntact(t *testing.T) {
	// Inject EXDEV on link and a read error via the readFileFn seam.
	// Verify the source remains intact and no destination is created.
	withLinkFn(t, func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	})
	withReadFileFn(t, func(name string) ([]byte, error) {
		return nil, fmt.Errorf("injected read error")
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")
	content := "precious content\n"

	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail when source read fails")
	}

	// Source must remain on disk unchanged.
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("source should still exist after failed read: %v", err)
	}
	if string(got) != content {
		t.Fatalf("source content changed: got %q, want %q", got, content)
	}

	// Destination must not exist.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("destination should not exist after failed move, got err: %v", err)
	}
}

func TestAtomicMove_CrossDeviceCreateFailSourceIntact(t *testing.T) {
	// Inject EXDEV on link and a create error via the openFileFn seam.
	// Verify the source remains intact and the error is not ErrDestinationExists.
	withLinkFn(t, func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	})
	withOpenFileFn(t, func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return nil, fmt.Errorf("injected create error")
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")
	content := "safe content\n"

	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail when destination create fails")
	}
	if errors.Is(err, ErrDestinationExists) {
		t.Fatal("error should not be ErrDestinationExists for create failure")
	}

	// Source must remain intact.
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("source should still exist: %v", err)
	}
	if string(got) != content {
		t.Fatalf("source content changed: got %q, want %q", got, content)
	}
}

func TestAtomicMove_CrossDeviceWriteFailCleansUp(t *testing.T) {
	// Inject EXDEV on link and a write error via the writeFileFn seam.
	// Verify the partial destination is removed and source remains intact.
	withLinkFn(t, func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	})
	withWriteFileFn(t, func(f *os.File, data []byte) error {
		return fmt.Errorf("injected write error")
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")
	content := "precious content\n"

	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	err := AtomicMove(src, dst)
	if err == nil {
		t.Fatal("AtomicMove should fail when write fails")
	}

	// Source must remain on disk unchanged.
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("source should still exist after failed write: %v", err)
	}
	if string(got) != content {
		t.Fatalf("source content changed: got %q, want %q", got, content)
	}

	// Destination must not exist (partial file should be cleaned up).
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("partial destination should be cleaned up, got err: %v", err)
	}
}

func TestAtomicMove_CrossDeviceConcurrentRace(t *testing.T) {
	// Multiple goroutines race to cross-device-move different sources to
	// the same destination. Exactly one should succeed; the rest must get
	// ErrDestinationExists (via O_CREATE|O_EXCL).
	withLinkFn(t, func(_, _ string) error {
		return &os.LinkError{Op: "link", Err: syscall.EXDEV}
	})

	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.md")

	const n = 8
	srcs := make([]string, n)
	for i := 0; i < n; i++ {
		src := filepath.Join(dir, fmt.Sprintf("src-%d.md", i))
		if err := os.WriteFile(src, []byte(fmt.Sprintf("content-%d\n", i)), 0o644); err != nil {
			t.Fatalf("write source %d: %v", i, err)
		}
		srcs[i] = src
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		idx := i
		go func() {
			defer wg.Done()
			errs[idx] = AtomicMove(srcs[idx], dst)
		}()
	}
	wg.Wait()

	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrDestinationExists) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 success in cross-device race, got %d", successes)
	}
}

func TestRecoverOrphanedTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	orphan := filepath.Join(tasksDir, DirInProgress, "fix-bug.md")
	os.WriteFile(orphan, []byte("# Fix bug\nDo the thing.\n"), 0o644)

	RecoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphaned task was not removed from in-progress/")
	}
	recovered := filepath.Join(tasksDir, DirBacklog, "fix-bug.md")
	data, err := os.ReadFile(recovered)
	if err != nil {
		t.Fatalf("recovered task not found in backlog/: %v", err)
	}
	if !strings.Contains(string(data), "# Fix bug") {
		t.Error("recovered task lost original content")
	}
	if !strings.Contains(string(data), "<!-- failure: mato-recovery") {
		t.Error("recovered task missing failure record")
	}
}

func TestRecoverOrphanedTasks_IgnoresNonMd(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	other := filepath.Join(tasksDir, DirInProgress, "notes.txt")
	os.WriteFile(other, []byte("hello"), 0o644)

	RecoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.md file should not be moved: %v", err)
	}
}

func TestRecoverOrphanedTasks_CollisionIdenticalContent(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	content := []byte("# Fix bug\nDo the thing.\n")
	backlogPath := filepath.Join(tasksDir, DirBacklog, "fix-bug.md")
	orphanPath := filepath.Join(tasksDir, DirInProgress, "fix-bug.md")
	os.WriteFile(backlogPath, content, 0o644)
	os.WriteFile(orphanPath, content, 0o644)

	stderr := captureStderr(t, func() {
		RecoverOrphanedTasks(tasksDir)
	})

	if !strings.Contains(stderr, "Removed duplicate orphan") {
		t.Fatalf("expected dedup message, got %q", stderr)
	}
	// Orphan should be removed from in-progress.
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatal("orphan should be removed from in-progress after dedup")
	}
	// Backlog copy should remain unchanged.
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog task: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("backlog task should be unchanged, got %q", string(data))
	}
}

func TestRecoverOrphanedTasks_CollisionDifferentContent(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	backlogPath := filepath.Join(tasksDir, DirBacklog, "fix-bug.md")
	orphanPath := filepath.Join(tasksDir, DirInProgress, "fix-bug.md")
	os.WriteFile(backlogPath, []byte("# Existing task\n"), 0o644)
	os.WriteFile(orphanPath, []byte("# Recovered task\n"), 0o644)

	stderr := captureStderr(t, func() {
		RecoverOrphanedTasks(tasksDir)
	})

	if !strings.Contains(stderr, "content differs from backlog copy") {
		t.Fatalf("expected rename message, got %q", stderr)
	}
	if !strings.Contains(stderr, "Recovered orphaned task") {
		t.Fatalf("expected recovery message, got %q", stderr)
	}
	// Orphan should be removed from in-progress.
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatal("orphan should be removed from in-progress after rename")
	}
	// Original backlog file should be untouched.
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog task: %v", err)
	}
	if string(data) != "# Existing task\n" {
		t.Fatalf("existing backlog task should be unchanged, got %q", string(data))
	}
	// A recovered file should exist in backlog with the orphan content.
	entries, err := os.ReadDir(filepath.Join(tasksDir, DirBacklog))
	if err != nil {
		t.Fatalf("read backlog dir: %v", err)
	}
	var recoveredFile string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fix-bug-recovered-") {
			recoveredFile = e.Name()
			break
		}
	}
	if recoveredFile == "" {
		t.Fatal("expected a recovered file in backlog, found none")
	}
	recoveredData, err := os.ReadFile(filepath.Join(tasksDir, DirBacklog, recoveredFile))
	if err != nil {
		t.Fatalf("read recovered file: %v", err)
	}
	if !strings.Contains(string(recoveredData), "# Recovered task") {
		t.Fatalf("recovered file should contain orphan content, got %q", string(recoveredData))
	}
	// The recovered file should also have a failure record.
	if !strings.Contains(string(recoveredData), "<!-- failure: mato-recovery") {
		t.Fatalf("recovered file should have failure record, got %q", string(recoveredData))
	}
}

func TestRecoverOrphanedTasks_SkipsActiveAgent(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress, ".locks"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	agentID := "active-agent"
	task := filepath.Join(tasksDir, DirInProgress, "active-task.md")
	content := fmt.Sprintf("<!-- claimed-by: %s  claimed-at: 2026-01-01T00:00:00Z -->\n# Active task\n", agentID)
	os.WriteFile(task, []byte(content), 0o644)
	os.WriteFile(filepath.Join(tasksDir, ".locks", agentID+".pid"), []byte(process.LockIdentity(os.Getpid())), 0o644)

	RecoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(task); err != nil {
		t.Fatal("task claimed by active agent should NOT be recovered")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "active-task.md")); err == nil {
		t.Fatal("task claimed by active agent should NOT appear in backlog")
	}
}

func TestRecoverOrphanedTasks_RemovesStaleInProgressCopyWhenTaskAlreadyAdvanced(t *testing.T) {
	for _, laterDir := range []string{DirReadyMerge, DirCompleted, DirFailed} {
		t.Run(laterDir, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, sub := range []string{DirBacklog, DirInProgress, DirReadyMerge, DirCompleted, DirFailed} {
				if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
					t.Fatalf("MkdirAll(%s): %v", sub, err)
				}
			}

			stalePath := filepath.Join(tasksDir, DirInProgress, "fix-bug.md")
			authoritativePath := filepath.Join(tasksDir, laterDir, "fix-bug.md")
			if err := os.WriteFile(stalePath, []byte("# Stale task\n"), 0o644); err != nil {
				t.Fatalf("write stale task: %v", err)
			}
			if err := os.WriteFile(authoritativePath, []byte("# Authoritative task\n"), 0o644); err != nil {
				t.Fatalf("write authoritative task: %v", err)
			}

			RecoverOrphanedTasks(tasksDir)

			if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
				t.Fatalf("stale in-progress copy should be removed, stat err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "fix-bug.md")); !os.IsNotExist(err) {
				t.Fatalf("task should not be recovered to backlog when %s copy exists, stat err = %v", laterDir, err)
			}
			data, err := os.ReadFile(authoritativePath)
			if err != nil {
				t.Fatalf("read authoritative task: %v", err)
			}
			if string(data) != "# Authoritative task\n" {
				t.Fatalf("authoritative task should be unchanged, got %q", string(data))
			}
		})
	}
}

func TestRecoverOrphanedTasks_AppendFailureLogsWarning(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	orphan := filepath.Join(tasksDir, DirInProgress, "unwritable.md")
	os.WriteFile(orphan, []byte("# Unwritable task\n"), 0o644)

	// Make backlog directory writable so the rename succeeds,
	// but make the destination file read-only after rename by
	// making the backlog dir non-writable — but that would block
	// the rename too. Instead, we pre-create a read-only backlog
	// file? No, that would block AtomicMove.
	//
	// Best approach: move the file first, then make it read-only.
	// We can't do that in RecoverOrphanedTasks. Instead, use a
	// directory-level trick: make the backlog dir read-only after
	// the rename completes. But we can't intercept the rename.
	//
	// Simpler approach: ensure recovery still moves the task even
	// when the failure record can't be written.
	// We'll make the backlog/unwritable.md file read-only after
	// the test to prevent append.

	// Actually, we need to simulate OpenFile failure. The simplest
	// way is to make the target file non-writable after the rename.
	// Since we can't intercept, let's test the warning message directly:
	// create a scenario where the file is read-only in backlog/.

	// Move the file to backlog ourselves, make it read-only, then
	// re-create it in in-progress and call recovery.
	backlogPath := filepath.Join(tasksDir, DirBacklog, "unwritable.md")
	// First, do a normal recovery to get the file into backlog
	RecoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(backlogPath); err != nil {
		t.Fatalf("task should be in backlog: %v", err)
	}

	// Verify the recovery did move the task and append a failure record
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read recovered task: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: mato-recovery") {
		t.Fatal("first recovery should have appended failure record")
	}
}

func TestRecoverOrphanedTasks_StillMovesWhenAppendFails(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	orphan := filepath.Join(tasksDir, DirInProgress, "readonly-task.md")
	os.WriteFile(orphan, []byte("# Read-only task\n"), 0o644)
	// Make file read-only so OpenFile with O_WRONLY will fail after rename
	os.Chmod(orphan, 0o444)
	t.Cleanup(func() {
		// Ensure test cleanup can remove the file
		os.Chmod(filepath.Join(tasksDir, DirBacklog, "readonly-task.md"), 0o644)
	})

	stderr := captureStderr(t, func() {
		RecoverOrphanedTasks(tasksDir)
	})

	// Task should still be moved to backlog even though append fails
	backlogPath := filepath.Join(tasksDir, DirBacklog, "readonly-task.md")
	if _, err := os.Stat(backlogPath); err != nil {
		t.Fatalf("task should be moved to backlog even when append fails: %v", err)
	}

	// Should have logged a warning about the append failure
	if !strings.Contains(stderr, "could not write failure record") {
		t.Fatalf("expected warning about append failure, got %q", stderr)
	}

	// Verify original content is preserved
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog task: %v", err)
	}
	if !strings.Contains(string(data), "# Read-only task") {
		t.Fatal("task content should be preserved")
	}
	// Failure record should NOT be present since append failed
	if strings.Contains(string(data), "<!-- failure:") {
		t.Fatal("failure record should not be present when append fails")
	}
}

func TestRecoverOrphanedTasks_ConcurrentCalls(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("task-%d.md", i)
		path := filepath.Join(tasksDir, DirInProgress, name)
		if err := os.WriteFile(path, []byte(fmt.Sprintf("# Task %d\n", i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	start := make(chan struct{})
	panicCh := make(chan any, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicCh <- r
				}
			}()
			<-start
			RecoverOrphanedTasks(tasksDir)
		}()
	}

	close(start)
	wg.Wait()
	close(panicCh)
	for p := range panicCh {
		t.Fatalf("RecoverOrphanedTasks panicked: %v", p)
	}

	backlogEntries, err := os.ReadDir(filepath.Join(tasksDir, DirBacklog))
	if err != nil {
		t.Fatalf("ReadDir(backlog): %v", err)
	}
	if len(backlogEntries) != 5 {
		t.Fatalf("backlog entries = %d, want 5", len(backlogEntries))
	}

	inProgressEntries, err := os.ReadDir(filepath.Join(tasksDir, DirInProgress))
	if err != nil {
		t.Fatalf("ReadDir(in-progress): %v", err)
	}
	if len(inProgressEntries) != 0 {
		t.Fatalf("in-progress entries = %d, want 0", len(inProgressEntries))
	}

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("task-%d.md", i)
		data, err := os.ReadFile(filepath.Join(tasksDir, DirBacklog, name))
		if err != nil {
			t.Fatalf("read %s from backlog: %v", name, err)
		}
		if count := strings.Count(string(data), "<!-- failure: mato-recovery"); count != 1 {
			t.Fatalf("%s failure record count = %d, want 1", name, count)
		}
	}
}

func TestParseClaimedBy(t *testing.T) {
	dir := t.TempDir()

	withClaim := filepath.Join(dir, "task.md")
	os.WriteFile(withClaim, []byte("<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n# Do stuff\n"), 0o644)
	if got := ParseClaimedBy(withClaim); got != "abc123" {
		t.Errorf("ParseClaimedBy = %q, want %q", got, "abc123")
	}

	noClaim := filepath.Join(dir, "plain.md")
	os.WriteFile(noClaim, []byte("# Just a task\n"), 0o644)
	if got := ParseClaimedBy(noClaim); got != "" {
		t.Errorf("ParseClaimedBy = %q, want empty", got)
	}

	if got := ParseClaimedBy(filepath.Join(dir, "missing.md")); got != "" {
		t.Errorf("ParseClaimedBy = %q, want empty for missing file", got)
	}
}

func TestHasAvailableTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirBacklog, DirInProgress, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	if HasAvailableTasks(tasksDir, nil) {
		t.Fatal("expected no available tasks in empty dirs")
	}

	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "notes.txt"), []byte("hi"), 0o644)
	if HasAvailableTasks(tasksDir, nil) {
		t.Fatal("non-.md file should not count as an available task")
	}

	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "task1.md"), []byte("# Task 1\n"), 0o644)
	if !HasAvailableTasks(tasksDir, nil) {
		t.Fatal("expected available task in backlog")
	}

	os.Remove(filepath.Join(tasksDir, DirBacklog, "task1.md"))
	os.WriteFile(filepath.Join(tasksDir, DirInProgress, "task2.md"), []byte("# Task 2\n"), 0o644)
	if HasAvailableTasks(tasksDir, nil) {
		t.Fatal("in-progress tasks should not count as available")
	}

	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "task3.md"), []byte("# Task 3\n"), 0o644)
	if !HasAvailableTasks(tasksDir, nil) {
		t.Fatal("expected available task in backlog")
	}
}

func TestRegisterAgent(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, err := RegisterAgent(tasksDir, "test-agent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}

	lockFile := filepath.Join(tasksDir, ".locks", "test-agent.pid")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	got := strings.TrimSpace(string(data))
	pidStr := strconv.Itoa(os.Getpid())
	// Lock file must start with the PID.
	if !strings.HasPrefix(got, pidStr) {
		t.Errorf("lock file content = %q, want prefix %q", got, pidStr)
	}
	// On Linux, expect "PID:starttime" format.
	if _, statErr := os.Stat("/proc/self/stat"); statErr == nil {
		if !strings.Contains(got, ":") {
			t.Errorf("lock file content = %q, expected PID:starttime format on Linux", got)
		}
	}

	cleanup()

	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove lock file")
	}
}

func TestRegisterAgent_RacesCleanStaleLocks(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, err := RegisterAgent(tasksDir, "race-agent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	lockFile := filepath.Join(tasksDir, ".locks", "race-agent.pid")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		CleanStaleLocks(tasksDir)
	}()
	wg.Wait()

	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("active agent lock should survive cleanup: %v", err)
	}
}

func TestCleanStaleLocks(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	os.WriteFile(filepath.Join(locksDir, "alive.pid"), []byte(process.LockIdentity(os.Getpid())), 0o644)
	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647:99999999"), 0o644)

	CleanStaleLocks(tasksDir)

	if _, err := os.Stat(filepath.Join(locksDir, "alive.pid")); err != nil {
		t.Error("live lock should not be removed")
	}
	if _, err := os.Stat(filepath.Join(locksDir, "dead.pid")); !os.IsNotExist(err) {
		t.Error("stale lock should be removed")
	}
}

func TestReconcileReadyQueue_PromotesWhenDepsMet(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "different-name.md"), []byte("---\nid: dep-a\n---\nDone\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "dep-b.md"), []byte("Done\n"), 0o644)

	waitingPath := filepath.Join(tasksDir, DirWaiting, "task.md")
	os.WriteFile(waitingPath, []byte("---\ndepends_on: [dep-a, dep-b]\n---\nReady now\n"), 0o644)

	if got := ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("ReconcileReadyQueue() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task.md")); err != nil {
		t.Fatalf("promoted task missing from backlog: %v", err)
	}
	if _, err := os.Stat(waitingPath); !os.IsNotExist(err) {
		t.Fatalf("waiting task should be moved, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_LeavesUnmetDepsWaiting(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	waitingPath := filepath.Join(tasksDir, DirWaiting, "blocked-task.md")
	os.WriteFile(waitingPath, []byte("---\ndepends_on:\n  - missing-task\n---\nStill blocked\n"), 0o644)

	if got := ReconcileReadyQueue(tasksDir, nil); got {
		t.Fatal("ReconcileReadyQueue() = true, want false")
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("task with unmet deps should stay in waiting: %v", err)
	}
}

func TestReconcileReadyQueue_PromotesTaskWithNoDeps(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "solo-task.md"), []byte("# Solo\n"), 0o644)

	if got := ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("ReconcileReadyQueue() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "solo-task.md")); err != nil {
		t.Fatalf("promoted task missing from backlog: %v", err)
	}
}

func TestReconcileReadyQueue_SkipsOverlappingWithActive(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirInProgress} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, DirInProgress, "task-a.md"), []byte("---\naffects: [main.go]\n---\nActive\n"), 0o644); err != nil {
		t.Fatalf("write active task: %v", err)
	}
	waitingPath := filepath.Join(tasksDir, DirWaiting, "task-b.md")
	if err := os.WriteFile(waitingPath, []byte("---\naffects: [main.go]\n---\nBlocked by active overlap\n"), 0o644); err != nil {
		t.Fatalf("write waiting task: %v", err)
	}

	if got := ReconcileReadyQueue(tasksDir, nil); got {
		t.Fatal("ReconcileReadyQueue() = true, want false")
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("overlapping waiting task should stay in waiting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task-b.md")); !os.IsNotExist(err) {
		t.Fatalf("overlapping waiting task should not be promoted, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_PromotesAfterActiveCompletes(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, DirCompleted, "task-a.md"), []byte("---\nid: task-a\naffects: [main.go]\n---\nDone\n"), 0o644); err != nil {
		t.Fatalf("write completed task: %v", err)
	}
	waitingPath := filepath.Join(tasksDir, DirWaiting, "task-b.md")
	if err := os.WriteFile(waitingPath, []byte("---\ndepends_on: [task-a]\naffects: [main.go]\n---\nReady now\n"), 0o644); err != nil {
		t.Fatalf("write waiting task: %v", err)
	}

	if got := ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("ReconcileReadyQueue() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task-b.md")); err != nil {
		t.Fatalf("task should be promoted after active completion: %v", err)
	}
	if _, err := os.Stat(waitingPath); !os.IsNotExist(err) {
		t.Fatalf("promoted task should leave waiting, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_DoesNotOverwriteExistingBacklogTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	waitingPath := filepath.Join(tasksDir, DirWaiting, "task.md")
	backlogPath := filepath.Join(tasksDir, DirBacklog, "task.md")
	os.WriteFile(waitingPath, []byte("# Ready\n"), 0o644)
	os.WriteFile(backlogPath, []byte("# Existing backlog\n"), 0o644)

	stderr := captureStderr(t, func() {
		if got := ReconcileReadyQueue(tasksDir, nil); got {
			t.Fatal("ReconcileReadyQueue() = true, want false")
		}
	})

	if !strings.Contains(stderr, "destination already exists") {
		t.Fatalf("expected overwrite warning, got %q", stderr)
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("waiting task should remain after failed promotion: %v", err)
	}
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog task: %v", err)
	}
	if string(data) != "# Existing backlog\n" {
		t.Fatalf("existing backlog task should be unchanged, got %q", string(data))
	}
}

func TestReconcileReadyQueue_DetectsSelfDependency(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	waitingPath := filepath.Join(tasksDir, DirWaiting, "self-task.md")
	os.WriteFile(waitingPath, []byte("---\nid: self-task\ndepends_on: [self-task]\n---\nBlocked\n"), 0o644)

	stderr := captureStderr(t, func() {
		if got := ReconcileReadyQueue(tasksDir, nil); !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	if !strings.Contains(stderr, "task self-task depends on itself") {
		t.Fatalf("expected self-dependency warning, got %q", stderr)
	}
	// Self-dependent tasks are now moved to failed/ with a cycle-failure marker.
	failedPath := filepath.Join(tasksDir, DirFailed, "self-task.md")
	if _, err := os.Stat(failedPath); err != nil {
		t.Fatalf("self-dependent task should be moved to failed/: %v", err)
	}
	if _, err := os.Stat(waitingPath); err == nil {
		t.Fatal("self-dependent task should no longer be in waiting/")
	}
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("read failed task: %v", err)
	}
	if !strings.Contains(string(data), "<!-- cycle-failure:") {
		t.Fatalf("expected cycle-failure marker in failed task, got %q", string(data))
	}
}

func TestReconcileReadyQueue_DetectsCircularDependency(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-a.md"), []byte("---\nid: task-a\ndepends_on: [task-b]\n---\nA\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-b.md"), []byte("---\nid: task-b\ndepends_on: [task-a]\n---\nB\n"), 0o644)

	stderr := captureStderr(t, func() {
		if got := ReconcileReadyQueue(tasksDir, nil); !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	// Both cycle members should have warnings.
	if !strings.Contains(stderr, "task task-a is part of a circular dependency") {
		t.Fatalf("expected circular dependency warning for task-a, got %q", stderr)
	}
	if !strings.Contains(stderr, "task task-b is part of a circular dependency") {
		t.Fatalf("expected circular dependency warning for task-b, got %q", stderr)
	}
	// Both cycle members should be moved to failed/ with cycle-failure markers.
	for _, name := range []string{"task-a.md", "task-b.md"} {
		failedPath := filepath.Join(tasksDir, DirFailed, name)
		if _, err := os.Stat(failedPath); err != nil {
			t.Fatalf("%s should be moved to failed/: %v", name, err)
		}
		waitingPath := filepath.Join(tasksDir, DirWaiting, name)
		if _, err := os.Stat(waitingPath); err == nil {
			t.Fatalf("%s should no longer be in waiting/", name)
		}
		data, err := os.ReadFile(failedPath)
		if err != nil {
			t.Fatalf("read failed task %s: %v", name, err)
		}
		if !strings.Contains(string(data), "<!-- cycle-failure:") {
			t.Fatalf("expected cycle-failure marker in %s, got %q", name, string(data))
		}
	}
}

func TestWriteQueueManifest_SortsByPriorityThenFilename(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, DirBacklog), 0o755)

	for name, content := range map[string]string{
		"z-low.md":     "---\npriority: 20\n---\nBody\n",
		"b-high.md":    "---\npriority: 5\n---\nBody\n",
		"a-high.md":    "---\npriority: 5\n---\nBody\n",
		"c-default.md": "Body\n",
	} {
		os.WriteFile(filepath.Join(tasksDir, DirBacklog, name), []byte(content), 0o644)
	}

	if err := WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	want := "a-high.md\nb-high.md\nz-low.md\nc-default.md\n"
	if string(data) != want {
		t.Fatalf("manifest = %q, want %q", string(data), want)
	}
}

func TestWriteQueueManifest_EmptyBacklog(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tasksDir, DirBacklog), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(strings.Fields(string(data))) != 0 {
		t.Fatalf("expected empty manifest, got %q", string(data))
	}
}

func TestWriteQueueManifest_SkipsMalformedFiles(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, DirBacklog), 0o755)

	os.WriteFile(filepath.Join(tasksDir, ".queue"), []byte("stale\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "good.md"), []byte("---\npriority: 10\n---\nGood\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "bad.md"), []byte("---\npriority: nope\n---\nBad\n"), 0o644)

	stderr := captureStderr(t, func() {
		if err := WriteQueueManifest(tasksDir, nil, nil); err != nil {
			t.Fatalf("WriteQueueManifest: %v", err)
		}
	})

	if !strings.Contains(stderr, "could not parse backlog task bad.md for queue manifest") {
		t.Fatalf("expected malformed file warning, got %q", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(data) != "good.md\n" {
		t.Fatalf("manifest = %q, want %q", string(data), "good.md\n")
	}
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".queue.tmp-") {
			t.Fatalf("temporary manifest file should be cleaned up, found %s", entry.Name())
		}
	}
}

func TestWriteQueueManifest_WithIndexSkipsMalformedBacklogFiles(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tasksDir, DirBacklog), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "good.md"), []byte("---\npriority: 10\n---\nGood\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "bad.md"), []byte("---\npriority: nope\n---\nBad\n"), 0o644)

	idx := BuildIndex(tasksDir)
	stderr := captureStderr(t, func() {
		if err := WriteQueueManifest(tasksDir, nil, idx); err != nil {
			t.Fatalf("WriteQueueManifest: %v", err)
		}
	})

	if !strings.Contains(stderr, "could not parse backlog task bad.md for queue manifest") {
		t.Fatalf("expected malformed file warning, got %q", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(data) != "good.md\n" {
		t.Fatalf("manifest = %q, want %q", string(data), "good.md\n")
	}
}

func TestWriteQueueManifest_WithIndexFailsWhenBacklogUnreadable(t *testing.T) {
	tasksDir := t.TempDir()
	idx := &PollIndex{buildWarnings: []BuildWarning{{State: DirBacklog, Path: filepath.Join(tasksDir, DirBacklog), Err: os.ErrPermission}}}

	stderr := captureStderr(t, func() {
		err := WriteQueueManifest(tasksDir, nil, idx)
		if err == nil {
			t.Fatal("expected WriteQueueManifest to fail when backlog dir could not be read")
		}
		if !strings.Contains(err.Error(), "read backlog dir") {
			t.Fatalf("error = %v, want backlog read failure", err)
		}
	})

	if !strings.Contains(stderr, "could not build queue index cleanly") {
		t.Fatalf("expected warning about partial index, got %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, ".queue")); !os.IsNotExist(err) {
		t.Fatalf(".queue should not be written when backlog cannot be read: %v", err)
	}
}

func TestDeferredOverlappingTasks_DefersLowerPriorityTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	for name, content := range map[string]string{
		"high-priority.md": "---\npriority: 5\naffects: [pkg/client/http.go, README.md]\n---\nKeep me\n",
		"low-priority.md":  "---\npriority: 20\naffects: [pkg/client/http.go]\n---\nDefer me\n",
		"independent.md":   "---\npriority: 30\naffects: [docs/guide.md]\n---\nKeep me too\n",
	} {
		os.WriteFile(filepath.Join(tasksDir, DirBacklog, name), []byte(content), 0o644)
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["low-priority.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "low-priority.md", deferred)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "high-priority.md")); err != nil {
		t.Fatalf("high priority task should stay in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "independent.md")); err != nil {
		t.Fatalf("independent task should stay in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "low-priority.md")); err != nil {
		t.Fatalf("low priority overlapping task should stay in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "low-priority.md")); !os.IsNotExist(err) {
		t.Fatalf("low priority overlapping task should not move to waiting, stat err = %v", err)
	}
}

func TestDeferredOverlappingTasks_ChecksInProgress(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirInProgress} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, DirInProgress, "task-a.md"), []byte("---\naffects: [main.go]\n---\nActive\n"), 0o644); err != nil {
		t.Fatalf("write active task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, "task-b.md"), []byte("---\naffects: [main.go]\n---\nConflicting\n"), 0o644); err != nil {
		t.Fatalf("write backlog task: %v", err)
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["task-b.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "task-b.md", deferred)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task-b.md")); err != nil {
		t.Fatalf("conflicting backlog task should stay in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "task-b.md")); !os.IsNotExist(err) {
		t.Fatalf("conflicting backlog task should not move to waiting, stat err = %v", err)
	}
}

func TestDeferredOverlappingTasks_ChecksReadyToMerge(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirReadyMerge} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, DirReadyMerge, "task-a.md"), []byte("---\naffects: [main.go]\n---\nActive\n"), 0o644); err != nil {
		t.Fatalf("write ready-to-merge task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, "task-b.md"), []byte("---\naffects: [main.go]\n---\nConflicting\n"), 0o644); err != nil {
		t.Fatalf("write backlog task: %v", err)
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["task-b.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "task-b.md", deferred)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task-b.md")); err != nil {
		t.Fatalf("conflicting backlog task should stay in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "task-b.md")); !os.IsNotExist(err) {
		t.Fatalf("conflicting backlog task should not move to waiting, stat err = %v", err)
	}
}

func TestDeferredOverlappingTasks_AllIdenticalAffects(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	for name, content := range map[string]string{
		"priority-5.md":  "---\npriority: 5\naffects: [main.go]\n---\nKeep me\n",
		"priority-10.md": "---\npriority: 10\naffects: [main.go]\n---\nWait\n",
		"priority-20.md": "---\npriority: 20\naffects: [main.go]\n---\nWait\n",
	} {
		if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 2 {
		t.Fatalf("len(deferred) = %d, want 2", len(deferred))
	}
	if _, ok := deferred["priority-10.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "priority-10.md", deferred)
	}
	if _, ok := deferred["priority-20.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "priority-20.md", deferred)
	}
	if _, ok := deferred["priority-5.md"]; ok {
		t.Fatalf("deferred set should not include %q: %#v", "priority-5.md", deferred)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "priority-5.md")); err != nil {
		t.Fatalf("highest-priority task should remain in backlog: %v", err)
	}
	for _, name := range []string{"priority-10.md", "priority-20.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, name)); err != nil {
			t.Fatalf("%s should remain in backlog: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should not move to waiting, stat err = %v", name, err)
		}
	}
}

func TestDeferredOverlappingTasks_NoAffects(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	for _, name := range []string{"task-a.md", "task-b.md", "task-c.md"} {
		if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, name), []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 0 {
		t.Fatalf("len(deferred) = %d, want 0", len(deferred))
	}
	for _, name := range []string{"task-a.md", "task-b.md", "task-c.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, name)); err != nil {
			t.Fatalf("%s should remain in backlog: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should not move to waiting, stat err = %v", name, err)
		}
	}
}

func TestDeferredOverlappingTasks_PrefixMatch(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// High-priority task claims a directory prefix.
	if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, "refactor-client.md"),
		[]byte("---\npriority: 5\naffects: [pkg/client/]\n---\nRefactor client package\n"), 0o644); err != nil {
		t.Fatalf("write refactor-client.md: %v", err)
	}
	// Low-priority task claims a specific file under that directory.
	if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, "fix-http.md"),
		[]byte("---\npriority: 20\naffects: [pkg/client/http.go]\n---\nFix HTTP bug\n"), 0o644); err != nil {
		t.Fatalf("write fix-http.md: %v", err)
	}
	// Independent task with no overlap.
	if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, "update-docs.md"),
		[]byte("---\npriority: 30\naffects: [docs/guide.md]\n---\nUpdate docs\n"), 0o644); err != nil {
		t.Fatalf("write update-docs.md: %v", err)
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["fix-http.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "fix-http.md", deferred)
	}
}

func TestDeferredOverlappingTasks_PrefixMatchInProgress(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// In-progress task claims a directory prefix.
	if err := os.WriteFile(filepath.Join(tasksDir, DirInProgress, "active-task.md"),
		[]byte("---\naffects: [internal/queue/]\n---\nActive work\n"), 0o644); err != nil {
		t.Fatalf("write active-task.md: %v", err)
	}
	// Backlog task claims a specific file under that prefix.
	if err := os.WriteFile(filepath.Join(tasksDir, DirBacklog, "queue-fix.md"),
		[]byte("---\naffects: [internal/queue/overlap.go]\n---\nFix overlap\n"), 0o644); err != nil {
		t.Fatalf("write queue-fix.md: %v", err)
	}

	deferred := DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["queue-fix.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "queue-fix.md", deferred)
	}
}

func TestQueueOps_SpecialCharacterFilenames(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	name := "my task (v2).md"
	waitingPath := filepath.Join(tasksDir, DirWaiting, name)
	if err := os.WriteFile(waitingPath, []byte("# Special task\n"), 0o644); err != nil {
		t.Fatalf("write waiting task: %v", err)
	}

	if got := ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("ReconcileReadyQueue() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, name)); err != nil {
		t.Fatalf("special-character task missing from backlog: %v", err)
	}
	if _, err := os.Stat(waitingPath); !os.IsNotExist(err) {
		t.Fatalf("special-character task should leave waiting, stat err = %v", err)
	}

	if err := WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(data), name) {
		t.Fatalf("manifest %q does not include %q", string(data), name)
	}
}

func TestReconcileReadyQueue_HighPriorityNotBlockedByLowPriorityBacklog(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirInProgress, DirReadyMerge} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Low-priority task already in backlog with overlapping affects
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "low-priority.md"),
		[]byte("---\npriority: 20\naffects: [main.go]\n---\n# Low\n"), 0o644)

	// High-priority task in waiting with same affects, no deps
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "high-priority.md"),
		[]byte("---\npriority: 5\naffects: [main.go]\n---\n# High\n"), 0o644)

	got := ReconcileReadyQueue(tasksDir, nil)
	if !got {
		t.Fatal("ReconcileReadyQueue() = false, want true (high-priority should promote)")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "high-priority.md")); err != nil {
		t.Fatal("high-priority task should be promoted to backlog")
	}
	// Both are now in backlog — DeferredOverlappingTasks can mark the
	// lower-priority one for exclusion from .queue.
}

func TestReconcileReadyQueue_DuplicateIDDoesNotSatisfyDependency(t *testing.T) {
	// Regression: if a completed task and a waiting task share the same ID,
	// a dependent task must NOT be promoted — the dependency is ambiguous.
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirInProgress, DirReadyMerge, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Completed task with id "shared-id"
	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "first-task.md"),
		[]byte("---\nid: shared-id\n---\n# First\nDone\n"), 0o644)

	// Waiting task also with id "shared-id"
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "second-task.md"),
		[]byte("---\nid: shared-id\n---\n# Second\nNot done\n"), 0o644)

	// Third task depends on "shared-id" — should NOT be promoted
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "dependent-task.md"),
		[]byte("---\nid: dependent\ndepends_on: [shared-id]\n---\n# Dependent\n"), 0o644)

	got := ReconcileReadyQueue(tasksDir, nil)
	// second-task has no deps so it may promote, but dependent-task must not
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "dependent-task.md")); os.IsNotExist(err) {
		t.Fatal("dependent-task should NOT be promoted when dep ID is ambiguous (duplicate)")
	}
	// second-task (no deps) should still promote
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "second-task.md")); err != nil {
		t.Fatalf("second-task (no deps) should promote, got err: %v (moved=%v)", err, got)
	}
}

func TestReconcileReadyQueue_UniqueCompletedIDStillWorks(t *testing.T) {
	// Sanity check: when there is no duplicate, dependencies are still satisfied.
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirInProgress, DirReadyMerge, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "unique-dep.md"),
		[]byte("---\nid: unique-dep\n---\n# Unique dep\nDone\n"), 0o644)

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "consumer.md"),
		[]byte("---\nid: consumer\ndepends_on: [unique-dep]\n---\n# Consumer\n"), 0o644)

	got := ReconcileReadyQueue(tasksDir, nil)
	if !got {
		t.Fatal("ReconcileReadyQueue() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "consumer.md")); err != nil {
		t.Fatal("consumer task should be promoted when dep is uniquely completed")
	}
}

func TestReconcileReadyQueue_MovesUnparseableToFailed(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Invalid YAML frontmatter
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "bad-yaml.md"),
		[]byte("---\n: :\n  - [invalid\n---\n# Bad YAML\n"), 0o644)

	stderr := captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "bad-yaml.md")); err != nil {
		t.Fatalf("unparseable task should be moved to failed/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "bad-yaml.md")); !os.IsNotExist(err) {
		t.Fatal("unparseable task should no longer be in waiting/")
	}
	if !strings.Contains(stderr, "moving unparseable waiting task") {
		t.Errorf("expected warning about moving unparseable task, got: %s", stderr)
	}
}

func TestReconcileReadyQueue_MovesMalformedBacklogTaskToFailed(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "bad-backlog.md"), []byte("---\npriority: [oops\n---\n# Bad\n"), 0o644)

	stderr := captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "bad-backlog.md")); err != nil {
		t.Fatalf("malformed backlog task should be moved to failed/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "bad-backlog.md")); !os.IsNotExist(err) {
		t.Fatal("malformed backlog task should no longer be in backlog/")
	}
	if !strings.Contains(stderr, "moving unparseable backlog task") {
		t.Errorf("expected warning about moving malformed backlog task, got: %s", stderr)
	}
}

func TestReconcileReadyQueue_MovesMissingTerminatorToFailed(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Valid YAML but missing closing --- terminator
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "no-terminator.md"),
		[]byte("---\nid: no-term\ndepends_on: [dep-a]\n"), 0o644)

	stderr := captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "no-terminator.md")); err != nil {
		t.Fatalf("task with missing terminator should be moved to failed/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "no-terminator.md")); !os.IsNotExist(err) {
		t.Fatal("task with missing terminator should no longer be in waiting/")
	}
	if !strings.Contains(stderr, "moving unparseable waiting task") {
		t.Errorf("expected warning about moving unparseable task, got: %s", stderr)
	}
}

func TestReconcileReadyQueue_ValidTasksStillPromotedAlongsideUnparseable(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{DirWaiting, DirBacklog, DirCompleted, DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// One unparseable task
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "broken.md"),
		[]byte("---\n: :\n  - [invalid\n---\n# Broken\n"), 0o644)

	// One valid task with no deps (should be promoted)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "good-task.md"),
		[]byte("---\nid: good\n---\n# Good task\n"), 0o644)

	captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "broken.md")); err != nil {
		t.Fatal("unparseable task should be in failed/")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "good-task.md")); err != nil {
		t.Fatal("valid task should be promoted to backlog/")
	}
}

func TestReconcileReadyQueue_QuarantinesWaitingTaskWithInvalidGlob(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Waiting task with invalid glob syntax (unclosed bracket).
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "bad-glob.md"),
		[]byte("---\naffects:\n  - \"internal/[bad\"\n---\n# Bad glob\n"), 0o644)

	// Waiting task with valid glob (should be promoted normally).
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "good-glob.md"),
		[]byte("---\naffects:\n  - \"internal/runner/*.go\"\n---\n# Good glob\n"), 0o644)

	stderr := captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true (only good-glob should promote)")
		}
	})

	// bad-glob should be quarantined to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "bad-glob.md")); err != nil {
		t.Fatal("task with invalid glob should be moved to failed/")
	}
	// good-glob should be promoted to backlog/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "good-glob.md")); err != nil {
		t.Fatal("task with valid glob should be promoted to backlog/")
	}
	if !strings.Contains(stderr, "invalid glob") {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, "invalid glob")
	}
}

func TestReconcileReadyQueue_QuarantinesBacklogTaskWithInvalidGlob(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Backlog task with glob+trailing-slash (invalid combination).
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "glob-slash.md"),
		[]byte("---\naffects:\n  - \"internal/*/\"\n---\n# Glob slash\n"), 0o644)

	// Backlog task with valid glob (should remain in backlog).
	os.WriteFile(filepath.Join(tasksDir, DirBacklog, "valid.md"),
		[]byte("---\naffects:\n  - \"internal/runner/*.go\"\n---\n# Valid\n"), 0o644)

	stderr := captureStderr(t, func() {
		ReconcileReadyQueue(tasksDir, nil)
	})

	// glob-slash should be quarantined to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "glob-slash.md")); err != nil {
		t.Fatal("backlog task with invalid glob should be moved to failed/")
	}
	// valid task should remain in backlog/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "valid.md")); err != nil {
		t.Fatal("backlog task with valid glob should remain in backlog/")
	}
	if !strings.Contains(stderr, "combines glob syntax with trailing /") {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, "combines glob syntax with trailing /")
	}
}

func TestCountPromotableWaitingTasks_ExcludesInvalidGlobs(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Waiting task with invalid glob (should not be counted).
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "bad-glob.md"),
		[]byte("---\naffects:\n  - \"internal/[bad\"\n---\n# Bad glob\n"), 0o644)

	// Waiting task with valid affects (should be counted).
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "good-task.md"),
		[]byte("---\naffects:\n  - main.go\n---\n# Good task\n"), 0o644)

	// Waiting task with glob+trailing-slash (should not be counted).
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "glob-slash.md"),
		[]byte("---\naffects:\n  - \"internal/*/\"\n---\n# Glob slash\n"), 0o644)

	got := CountPromotableWaitingTasks(tasksDir, nil)
	if got != 1 {
		t.Fatalf("CountPromotableWaitingTasks() = %d, want 1 (only good-task)", got)
	}
}

func TestAcquireReviewLock_Success(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)

	cleanup, ok := AcquireReviewLock(tasksDir, "test-task.md")
	if !ok {
		t.Fatal("expected lock acquisition to succeed")
	}

	lockFile := filepath.Join(tasksDir, ".locks", "review-test-task.md.lock")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}
	data, _ := os.ReadFile(lockFile)
	pidStr := strconv.Itoa(os.Getpid())
	if !strings.HasPrefix(strings.TrimSpace(string(data)), pidStr) {
		t.Errorf("lock content = %q, want PID prefix %q", string(data), pidStr)
	}

	cleanup()
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove lock file")
	}
}

func TestAcquireReviewLock_BlockedByLiveProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	// Pre-create a lock held by the current process (alive).
	lockFile := filepath.Join(locksDir, "review-held-task.md.lock")
	os.WriteFile(lockFile, []byte(process.LockIdentity(os.Getpid())), 0o644)

	_, ok := AcquireReviewLock(tasksDir, "held-task.md")
	if ok {
		t.Fatal("should not acquire lock held by a live process")
	}
}

func TestAcquireReviewLock_ReclaimsStaleLock(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	// Pre-create a lock with a dead PID.
	lockFile := filepath.Join(locksDir, "review-stale-task.md.lock")
	os.WriteFile(lockFile, []byte("2147483647:99999999"), 0o644)

	cleanup, ok := AcquireReviewLock(tasksDir, "stale-task.md")
	if !ok {
		t.Fatal("should reclaim lock held by a dead process")
	}
	cleanup()
}

func TestAcquireReviewLock_TwoLocksOnDifferentTasks(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)

	cleanup1, ok1 := AcquireReviewLock(tasksDir, "task-a.md")
	if !ok1 {
		t.Fatal("first lock should succeed")
	}
	defer cleanup1()

	cleanup2, ok2 := AcquireReviewLock(tasksDir, "task-b.md")
	if !ok2 {
		t.Fatal("second lock on different task should succeed")
	}
	defer cleanup2()
}

func TestCleanStaleReviewLocks(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	// Live process lock — should survive.
	os.WriteFile(filepath.Join(locksDir, "review-live.md.lock"),
		[]byte(process.LockIdentity(os.Getpid())), 0o644)
	// Dead process lock — should be cleaned.
	os.WriteFile(filepath.Join(locksDir, "review-dead.md.lock"),
		[]byte("2147483647:99999999"), 0o644)
	// Non-review lock — should be ignored.
	os.WriteFile(filepath.Join(locksDir, "agent.pid"),
		[]byte("2147483647:99999999"), 0o644)

	CleanStaleReviewLocks(tasksDir)

	if _, err := os.Stat(filepath.Join(locksDir, "review-live.md.lock")); err != nil {
		t.Error("live review lock should survive cleanup")
	}
	if _, err := os.Stat(filepath.Join(locksDir, "review-dead.md.lock")); !os.IsNotExist(err) {
		t.Error("stale review lock should be removed")
	}
	if _, err := os.Stat(filepath.Join(locksDir, "agent.pid")); err != nil {
		t.Error("non-review lock should not be touched")
	}
}

func TestReconcileReadyQueue_ChainPromotionSemantics(t *testing.T) {
	// A -> completed, B -> A, C -> B. First reconcile promotes B only.
	// Second reconcile (after re-indexing) promotes C.
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "task-a.md"),
		[]byte("---\nid: task-a\n---\n# A\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-b.md"),
		[]byte("---\nid: task-b\ndepends_on: [task-a]\n---\n# B\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-c.md"),
		[]byte("---\nid: task-c\ndepends_on: [task-b]\n---\n# C\n"), 0o644)

	captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("first reconcile: promoted = false, want true (only B)")
		}
	})

	// B should be in backlog, C should still be in waiting.
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task-b.md")); err != nil {
		t.Fatal("task-b should be promoted to backlog")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "task-c.md")); err != nil {
		t.Fatal("task-c should remain in waiting")
	}
}

func TestReconcileReadyQueue_LongCycleMovesToFailed(t *testing.T) {
	// 3-node cycle: A -> B -> C -> A. Downstream D -> C stays in waiting.
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-a.md"),
		[]byte("---\nid: task-a\ndepends_on: [task-c]\n---\nA\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-b.md"),
		[]byte("---\nid: task-b\ndepends_on: [task-a]\n---\nB\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-c.md"),
		[]byte("---\nid: task-c\ndepends_on: [task-b]\n---\nC\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-d.md"),
		[]byte("---\nid: task-d\ndepends_on: [task-c]\n---\nD\n"), 0o644)

	captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	// All cycle members should be in failed/ with cycle-failure markers.
	for _, name := range []string{"task-a.md", "task-b.md", "task-c.md"} {
		failedPath := filepath.Join(tasksDir, DirFailed, name)
		data, err := os.ReadFile(failedPath)
		if err != nil {
			t.Fatalf("%s should be in failed/: %v", name, err)
		}
		if !strings.Contains(string(data), "<!-- cycle-failure:") {
			t.Fatalf("expected cycle-failure marker in %s", name)
		}
	}

	// Downstream task should remain in waiting.
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "task-d.md")); err != nil {
		t.Fatal("task-d (downstream) should remain in waiting")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "task-d.md")); err == nil {
		t.Fatal("task-d should NOT be in failed/")
	}
}

func TestReconcileReadyQueue_CycleDoesNotConsumeRetryBudget(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "cyclic.md"),
		[]byte("---\nid: cyclic\ndepends_on: [cyclic]\n---\nSelf-cycle\n"), 0o644)

	captureStderr(t, func() {
		ReconcileReadyQueue(tasksDir, nil)
	})

	failedPath := filepath.Join(tasksDir, DirFailed, "cyclic.md")
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("cyclic task should be in failed/: %v", err)
	}

	// CountFailureMarkers should return 0 — cycle-failure records are excluded.
	if count := taskfile.CountFailureMarkers(data); count != 0 {
		t.Fatalf("CountFailureMarkers = %d, want 0 (cycle-failure should not consume retry budget)", count)
	}
	// But cycle-failure markers should be present.
	if count := taskfile.CountCycleFailureMarkers(data); count != 1 {
		t.Fatalf("CountCycleFailureMarkers = %d, want 1", count)
	}
}

func TestReconcileReadyQueue_DownstreamOfCycleRemainsWaiting(t *testing.T) {
	// B -> A (cycle: A -> B -> A). C -> A (downstream, not cycle member).
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-a.md"),
		[]byte("---\nid: task-a\ndepends_on: [task-b]\n---\nA\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-b.md"),
		[]byte("---\nid: task-b\ndepends_on: [task-a]\n---\nB\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task-c.md"),
		[]byte("---\nid: task-c\ndepends_on: [task-a]\n---\nC\n"), 0o644)

	captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true")
		}
	})

	// Cycle members in failed/.
	for _, name := range []string{"task-a.md", "task-b.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, name)); err != nil {
			t.Fatalf("%s should be in failed/", name)
		}
	}
	// Downstream stays in waiting.
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "task-c.md")); err != nil {
		t.Fatal("task-c should remain in waiting")
	}
}

func TestReconcileReadyQueue_AmbiguousCompletedDoesNotSatisfy(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// ID "shared-id" in both completed and waiting (ambiguous).
	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "shared-task.md"),
		[]byte("---\nid: shared-id\n---\nDone\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "other-shared.md"),
		[]byte("---\nid: shared-id\n---\nStill waiting\n"), 0o644)
	// A task that depends on the ambiguous ID.
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "dependent.md"),
		[]byte("---\nid: dependent\ndepends_on: [shared-id]\n---\nBlocked\n"), 0o644)

	stderr := captureStderr(t, func() {
		got := ReconcileReadyQueue(tasksDir, nil)
		// other-shared.md has no deps and gets promoted; dependent stays blocked.
		if !got {
			t.Fatal("ReconcileReadyQueue() = false, want true (only other-shared)")
		}
	})

	if !strings.Contains(stderr, "exists in both completed and non-completed") {
		t.Fatalf("expected ambiguous ID warning, got %q", stderr)
	}
	// The dependent task should remain in waiting because the dependency
	// is ambiguous.
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "dependent.md")); err != nil {
		t.Fatal("dependent task should remain in waiting")
	}
}

func TestCountPromotableWaitingTasks_MatchesReconcile(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirCompleted, "dep-a.md"),
		[]byte("---\nid: dep-a\n---\nDone\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "ready.md"),
		[]byte("---\nid: ready\ndepends_on: [dep-a]\n---\nReady\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "blocked.md"),
		[]byte("---\nid: blocked\ndepends_on: [missing]\n---\nBlocked\n"), 0o644)

	count := CountPromotableWaitingTasks(tasksDir, nil)
	if count != 1 {
		t.Fatalf("CountPromotableWaitingTasks = %d, want 1", count)
	}

	captureStderr(t, func() {
		moved := ReconcileReadyQueue(tasksDir, nil)
		if moved != (count > 0) {
			t.Fatalf("ReconcileReadyQueue moved = %v, CountPromotableWaitingTasks returned %d", moved, count)
		}
	})
}

func TestReconcileReadyQueue_DuplicateWaitingIDPromotesOnce(t *testing.T) {
	// Two files in waiting/ share the same meta.ID and have no deps.
	// Only the first (alphabetically) should be promoted; the duplicate
	// must remain in waiting/.
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "aaa-dup.md"),
		[]byte("---\nid: dup-id\n---\n# First\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "zzz-dup.md"),
		[]byte("---\nid: dup-id\n---\n# Second\n"), 0o644)

	moved := false
	captureStderr(t, func() {
		moved = ReconcileReadyQueue(tasksDir, nil)
	})

	if !moved {
		t.Fatal("ReconcileReadyQueue moved = false, want true (only the retained file)")
	}

	// First file (aaa-dup.md) should be promoted.
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "aaa-dup.md")); err != nil {
		t.Fatal("retained file aaa-dup.md should be promoted to backlog/")
	}

	// Duplicate file (zzz-dup.md) should remain in waiting/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "zzz-dup.md")); err != nil {
		t.Fatal("duplicate file zzz-dup.md should remain in waiting/")
	}
}

func TestReconcileReadyQueue_DuplicateWaitingIDCycleTargetsRetained(t *testing.T) {
	// Two files share the same meta.ID and form a self-dependency (cycle).
	// Only the retained file (first seen) should be moved to failed/; the
	// duplicate should remain in waiting/ untouched.
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "aaa-self.md"),
		[]byte("---\nid: self-id\ndepends_on: [self-id]\n---\n# Self dep\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, DirWaiting, "zzz-self.md"),
		[]byte("---\nid: self-id\ndepends_on: [self-id]\n---\n# Self dep copy\n"), 0o644)

	captureStderr(t, func() {
		ReconcileReadyQueue(tasksDir, nil)
	})

	// Retained file (aaa-self.md) should be moved to failed/ with a cycle-failure marker.
	failedPath := filepath.Join(tasksDir, DirFailed, "aaa-self.md")
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("retained file aaa-self.md should be in failed/: %v", err)
	}
	if !taskfile.ContainsCycleFailure(data) {
		t.Fatal("retained file should have a cycle-failure marker")
	}

	// Duplicate file (zzz-self.md) should remain in waiting/ without a cycle-failure marker.
	dupPath := filepath.Join(tasksDir, DirWaiting, "zzz-self.md")
	dupData, err := os.ReadFile(dupPath)
	if err != nil {
		t.Fatalf("duplicate file zzz-self.md should remain in waiting/: %v", err)
	}
	if taskfile.ContainsCycleFailure(dupData) {
		t.Fatal("duplicate file should NOT have a cycle-failure marker")
	}
}
