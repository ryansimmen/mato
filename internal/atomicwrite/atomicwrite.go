// Package atomicwrite provides atomic file write utilities.
//
// Each function writes to a temporary file in the same directory as the target
// and renames it into place, ensuring readers never see partial content.
package atomicwrite

import (
	"fmt"
	"os"
	"path/filepath"
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

// WriteFunc atomically writes to path using a caller-supplied write callback.
// The callback receives the open temp file and may write to it in any way
// (e.g., JSON encoding, fmt.Fprintf). If fn returns an error the temp file is
// cleaned up and no rename occurs.
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
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("atomic write %s: rename temp: %w", path, err)
	}
	return nil
}
