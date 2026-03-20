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
		f, err := os.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if _, writeErr := f.WriteString(identity); writeErr != nil {
				f.Close()
				os.Remove(lockFile)
				fmt.Fprintf(os.Stderr, "warning: write %s lock: %v\n", name, writeErr)
				return nil, false
			}
			if closeErr := f.Close(); closeErr != nil {
				os.Remove(lockFile)
				fmt.Fprintf(os.Stderr, "warning: close %s lock: %v\n", name, closeErr)
				return nil, false
			}
			return func() { os.Remove(lockFile) }, true
		}
		if !os.IsExist(err) {
			fmt.Fprintf(os.Stderr, "warning: create %s lock: %v\n", name, err)
			return nil, false
		}

		data, readErr := os.ReadFile(lockFile)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: read %s lock: %v\n", name, readErr)
			return nil, false
		}

		content := strings.TrimSpace(string(data))
		if content == "" || process.IsLockHolderAlive(content) {
			return nil, false
		}
		if removeErr := os.Remove(lockFile); removeErr != nil && !os.IsNotExist(removeErr) {
			fmt.Fprintf(os.Stderr, "warning: remove stale %s lock: %v\n", name, removeErr)
			return nil, false
		}
	}

	return nil, false
}
