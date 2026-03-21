package atomicwrite

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
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
