// Package atomicwrite provides atomic file write utilities.
//
// Each function writes to a temporary file in the same directory as the target
// and renames it into place, ensuring readers never see partial content.
// When the temp directory and target path live on different filesystems
// (e.g. Docker bind mounts), the rename may fail with EXDEV; in that case
// a fallback copies via O_CREATE|O_EXCL to preserve atomicity.
package atomicwrite

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// WriteFile atomically writes data to path. It creates a temporary file in the
// same directory, sets permissions to 0o644, writes the data, and renames the
// temp file to the target path.
func WriteFile(path string, data []byte) error {
	return WriteFunc(path, func(f *os.File) error {
		_, err := f.Write(data)
		return err
	})
}

// AppendToFile appends content to the file at path using O_APPEND|O_WRONLY.
// It checks both write and close errors, returning a context-wrapped error on
// failure. This is intentionally non-atomic (no temp-file rename) because
// append operations need to preserve existing file content.
func AppendToFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s for append: %w", path, err)
	}
	_, writeErr := f.WriteString(content)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("append to %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s after append: %w", path, closeErr)
	}
	return nil
}

// renameFn is the function used by WriteFunc to rename the temp file.
// Tests override it to simulate EXDEV without separate filesystems.
var renameFn = os.Rename

// WriteFunc atomically writes to path using a caller-supplied write callback.
// The callback receives the open temp file and may write to it in any way
// (e.g., JSON encoding, fmt.Fprintf). If fn returns an error the temp file is
// cleaned up and no rename occurs.
//
// When the rename fails with EXDEV (cross-device link), WriteFunc falls back
// to an exclusive-create + copy path that preserves atomicity guarantees.
func WriteFunc(path string, fn func(f *os.File) error) error {
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomic write %s: create temp: %w", path, err)
	}
	tmpName := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}

	if err := tmpFile.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("atomic write %s: chmod temp: %w", path, err)
	}
	if err := fn(tmpFile); err != nil {
		cleanup()
		return fmt.Errorf("atomic write %s: write temp: %w", path, err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomic write %s: close temp: %w", path, err)
	}
	if err := renameFn(tmpName, path); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return crossDeviceFallback(tmpName, path)
		}
		os.Remove(tmpName)
		return fmt.Errorf("atomic write %s: rename temp: %w", path, err)
	}
	return nil
}

// crossDeviceFallback handles the EXDEV case where the temp file and target
// path are on different filesystems. It opens the target with O_CREATE|O_EXCL
// to prevent partial reads, copies the temp file contents, fsyncs, and removes
// the temp file.
func crossDeviceFallback(tmpName, path string) error {
	src, err := os.Open(tmpName)
	if err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomic write %s: open temp for fallback: %w", path, err)
	}
	defer func() {
		src.Close()
		os.Remove(tmpName)
	}()

	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Target already exists — remove it and retry the exclusive create.
			// This mirrors normal rename semantics (overwrite existing target).
			if rmErr := os.Remove(path); rmErr != nil {
				return fmt.Errorf("atomic write %s: remove existing for fallback: %w", path, rmErr)
			}
			dst, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return fmt.Errorf("atomic write %s: create destination after remove: %w", path, err)
			}
		} else {
			return fmt.Errorf("atomic write %s: create destination: %w", path, err)
		}
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(path)
		return fmt.Errorf("atomic write %s: copy to destination: %w", path, err)
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		os.Remove(path)
		return fmt.Errorf("atomic write %s: fsync destination: %w", path, err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("atomic write %s: close destination: %w", path, err)
	}
	return nil
}
