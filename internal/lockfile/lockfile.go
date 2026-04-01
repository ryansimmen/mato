// Package lockfile provides a generic exclusive file-lock mechanism backed
// by the filesystem. Locks are identified by a "PID:starttime" identity
// string so that stale locks left by dead processes can be reclaimed.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mato/internal/process"
)

// Test hooks for injecting I/O failures in Acquire.
var (
	osReadFile = os.ReadFile
	osRemove   = os.Remove
)

// TestHookReadFile exposes the current read hook for tests.
func TestHookReadFile() func(string) ([]byte, error) {
	return osReadFile
}

// SetTestHookReadFile replaces the read hook for tests.
func SetTestHookReadFile(fn func(string) ([]byte, error)) {
	osReadFile = fn
}

// CheckHeld checks whether a lock file at the given path exists and is held
// by a live process. Unlike IsHeld, it returns an error when the file exists
// but cannot be read, allowing callers to distinguish unreadable files from
// absent or dead locks.
func CheckHeld(lockPath string) (bool, error) {
	data, err := osReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return false, nil
	}
	return process.IsLockHolderAlive(content), nil
}

// IsHeld checks whether a lock file at the given path exists and is held by
// a live process. Returns false if the file does not exist, is empty, cannot
// be read, or the holder process is no longer running.
func IsHeld(lockPath string) bool {
	held, _ := CheckHeld(lockPath)
	return held
}

// Register writes the current process identity ("PID:starttime") to a file
// named "<name>.pid" inside locksDir. Unlike Acquire, this is non-exclusive:
// it overwrites any existing file. Returns a cleanup function that removes
// the file. Used for agent presence registration.
func Register(locksDir, name string) (func(), error) {
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create locks dir %s: %w", locksDir, err)
	}
	lockFile := filepath.Join(locksDir, name+".pid")
	identity := process.LockIdentity(os.Getpid())
	if err := os.WriteFile(lockFile, []byte(identity), 0o644); err != nil {
		return nil, fmt.Errorf("write lock %s: %w", lockFile, err)
	}
	return func() { os.Remove(lockFile) }, nil
}

// Acquire attempts to create an exclusive lock file named "<name>.lock"
// inside locksDir. It writes the current process identity into the file so
// other callers can detect stale locks from dead processes.
//
// Returns a cleanup function and true on success, or nil and false if the
// lock is already held by a live process (or an unrecoverable error occurs).
// The caller must invoke the cleanup function when done.
func Acquire(locksDir, name string) (func(), bool) {
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: create locks dir for %s: %v\n", name, err)
		return nil, false
	}

	lockFile := filepath.Join(locksDir, name+".lock")
	identity := process.LockIdentity(os.Getpid())

	for attempts := 0; attempts < 2; attempts++ {
		// Write identity to a temporary file, then hard-link it to the
		// lock path. os.Link fails atomically with EEXIST when the lock
		// file already exists, and unlike open-then-write the lock file
		// never appears empty on disk.
		tmpFile, err := os.CreateTemp(locksDir, name+".lock.tmp.*")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: create %s lock tmp: %v\n", name, err)
			return nil, false
		}
		tmpPath := tmpFile.Name()
		_, writeErr := tmpFile.WriteString(identity)
		closeErr := tmpFile.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(tmpPath)
			fmt.Fprintf(os.Stderr, "warning: write %s lock tmp: write=%v close=%v\n", name, writeErr, closeErr)
			return nil, false
		}
		linkErr := os.Link(tmpPath, lockFile)
		os.Remove(tmpPath)
		if linkErr == nil {
			return func() { os.Remove(lockFile) }, true
		}
		if !os.IsExist(linkErr) {
			fmt.Fprintf(os.Stderr, "warning: create %s lock: %v\n", name, linkErr)
			return nil, false
		}

		data, readErr := osReadFile(lockFile)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: read %s lock: %v\n", name, readErr)
			return nil, false
		}

		content := strings.TrimSpace(string(data))
		if content != "" && process.IsLockHolderAlive(content) {
			return nil, false
		}
		// Empty content (crash between create and write) or dead PID: reclaim.
		if removeErr := osRemove(lockFile); removeErr != nil && !os.IsNotExist(removeErr) {
			fmt.Fprintf(os.Stderr, "warning: remove stale %s lock: %v\n", name, removeErr)
			return nil, false
		}
	}

	return nil, false
}
