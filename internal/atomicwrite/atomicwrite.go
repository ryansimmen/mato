// Package atomicwrite provides atomic file write utilities.
//
// Each function writes to a temporary file in the same directory as the target
// and renames it into place, ensuring readers never see partial content.
// When the temp directory and target path live on different filesystems
// (e.g. Docker bind mounts), the rename may fail with EXDEV; in that case
// a fallback copies into a second temp file in the destination directory and
// renames that file into place.
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
// to copying the data into a second temp file in the destination directory and
// renaming that file into place, preserving normal overwrite semantics.
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
// path are on different filesystems. It copies the original temp file into a
// second temp file in the destination directory, fsyncs it, then renames that
// temp file into place so existing targets are replaced only at the final step.
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

	dir := filepath.Dir(path)
	tmpDst, err := os.CreateTemp(dir, "."+filepath.Base(path)+".xdev-*")
	if err != nil {
		return fmt.Errorf("atomic write %s: create fallback temp: %w", path, err)
	}
	tmpDstName := tmpDst.Name()
	cleanupDst := func() {
		tmpDst.Close()
		os.Remove(tmpDstName)
	}

	if err := tmpDst.Chmod(0o644); err != nil {
		cleanupDst()
		return fmt.Errorf("atomic write %s: chmod fallback temp: %w", path, err)
	}
	if _, err := io.Copy(tmpDst, src); err != nil {
		cleanupDst()
		return fmt.Errorf("atomic write %s: copy to fallback temp: %w", path, err)
	}
	if err := tmpDst.Sync(); err != nil {
		cleanupDst()
		return fmt.Errorf("atomic write %s: fsync fallback temp: %w", path, err)
	}
	if err := tmpDst.Close(); err != nil {
		os.Remove(tmpDstName)
		return fmt.Errorf("atomic write %s: close fallback temp: %w", path, err)
	}
	if err := os.Rename(tmpDstName, path); err != nil {
		os.Remove(tmpDstName)
		return fmt.Errorf("atomic write %s: rename fallback temp: %w", path, err)
	}
	return nil
}
