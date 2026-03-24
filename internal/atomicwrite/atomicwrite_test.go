package atomicwrite

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestWriteFile_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	data := []byte("hello, world\n")

	if err := WriteFile(path, data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}
}

func TestWriteFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	if err := WriteFile(path, []byte("first")); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	if err := WriteFile(path, []byte("second")); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want %q", got, "second")
	}
}

func TestWriteFile_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perms.txt")

	if err := WriteFile(path, []byte("test")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o644 {
		t.Errorf("permissions = %o, want 644", perm)
	}
}

func TestWriteFunc_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "func.txt")

	err := WriteFunc(path, func(f *os.File) error {
		_, err := f.WriteString("written via func")
		return err
	})
	if err != nil {
		t.Fatalf("WriteFunc: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "written via func" {
		t.Errorf("content = %q, want %q", got, "written via func")
	}
}

func TestWriteFunc_CleanupOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fail.txt")
	writeErr := errors.New("simulated write error")

	err := WriteFunc(path, func(f *os.File) error {
		return writeErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("error = %v, want wrapping %v", err, writeErr)
	}

	// Target file should not exist.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected target file not to exist after write error, got stat err: %v", err)
	}

	// No leftover temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("leftover temp file: %s", e.Name())
	}
}

func TestWriteFile_AtomicNoConcurrentPartialRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.txt")

	// Write initial content.
	if err := WriteFile(path, []byte("AAAA")); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}

	var wg sync.WaitGroup
	const iterations = 200
	errCh := make(chan error, iterations)

	// Writer goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := WriteFile(path, []byte("BBBB")); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Reader goroutine: reads must see either "AAAA" or "BBBB", never
	// partial content.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			data, err := os.ReadFile(path)
			if err != nil {
				// File may briefly not exist during rename; tolerable.
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				errCh <- err
				return
			}
			s := string(data)
			if s != "AAAA" && s != "BBBB" {
				errCh <- errors.New("read partial content: " + s)
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent error: %v", err)
	}
}

func TestAppendToFile_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "append.txt")

	if err := os.WriteFile(path, []byte("first line\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := AppendToFile(path, "second line\n"); err != nil {
		t.Fatalf("AppendToFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "first line\nsecond line\n"
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

func TestAppendToFile_FileNotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "file.txt")

	err := AppendToFile(path, "data")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got: %v", err)
	}
}

func TestAppendToFile_PermissionDenied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly.txt")

	if err := os.WriteFile(path, []byte("initial"), 0o444); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := AppendToFile(path, "more data")
	if err == nil {
		t.Fatal("expected error for read-only file, got nil")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("expected os.ErrPermission, got: %v", err)
	}
}

func TestWriteFunc_EXDEVFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exdev.txt")
	data := []byte("cross-device content")

	origRenameFn := renameFn
	renameFn = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFn = origRenameFn })

	if err := WriteFunc(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	}); err != nil {
		t.Fatalf("WriteFunc with EXDEV fallback: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("permissions = %o, want 644", perm)
	}

	// No leftover temp files.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "exdev.txt" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteFunc_EXDEVFallbackOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.txt")

	// Write initial content normally.
	if err := WriteFile(path, []byte("old")); err != nil {
		t.Fatalf("initial WriteFile: %v", err)
	}

	origRenameFn := renameFn
	renameFn = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFn = origRenameFn })

	if err := WriteFile(path, []byte("new")); err != nil {
		t.Fatalf("WriteFile with EXDEV fallback: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

func TestWriteFunc_EXDEVFallbackCleanupOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fail-exdev.txt")
	writeErr := errors.New("simulated write error")

	origRenameFn := renameFn
	renameFn = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFn = origRenameFn })

	// The write callback fails, so the fallback should never be reached.
	err := WriteFunc(path, func(f *os.File) error {
		return writeErr
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("error = %v, want wrapping %v", err, writeErr)
	}

	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("target file should not exist after callback error")
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("leftover temp file: %s", e.Name())
	}
}

func TestWriteFile_EXDEVFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "writefile-exdev.txt")

	origRenameFn := renameFn
	renameFn = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { renameFn = origRenameFn })

	data := []byte("WriteFile through fallback")
	if err := WriteFile(path, data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content = %q, want %q", got, data)
	}

	// Verify temp cleanup.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "writefile-exdev.txt" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestAppendToFile_ContentVerification(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.txt")

	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	lines := []string{"alpha\n", "beta\n", "gamma\n"}
	for _, line := range lines {
		if err := AppendToFile(path, line); err != nil {
			t.Fatalf("AppendToFile(%q): %v", line, err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "alpha\nbeta\ngamma\n"
	if string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}
