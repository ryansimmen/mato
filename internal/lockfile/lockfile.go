// Package lockfile provides a generic exclusive file-lock mechanism backed
// by the filesystem. Locks are identified by a "PID:starttime" identity
// string so that stale locks left by dead processes can be reclaimed.
package lockfile

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mato/internal/process"
)

// Test hooks for injecting I/O failures in Acquire.
var (
	osLink               = os.Link
	osReadFile           = os.ReadFile
	osRemove             = os.Remove
	acquireRetrySleepFn  = time.Sleep
	acquireRetryJitterFn = func(limit time.Duration) time.Duration {
		if limit <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(limit)))
	}
)

// Status describes the current state of a lock file's holder.
type Status int

const (
	// StatusInactive means the lock file is missing, empty, invalid, or held by
	// a dead process.
	StatusInactive Status = iota
	// StatusActive means the lock holder is still alive.
	StatusActive
	// StatusUnknown means the lock file could not be read, so its state could
	// not be determined.
	StatusUnknown
)

const (
	acquireMaxAttempts    = 5
	acquireRetryBaseDelay = 1 * time.Millisecond
	acquireRetryJitterMax = 4 * time.Millisecond
)

// Metadata captures the parsed state of a lock file.
type Metadata struct {
	PID      int
	Identity string
	Status   Status
}

// IsActive reports whether the metadata describes a live lock holder.
func (m Metadata) IsActive() bool {
	return m.Status == StatusActive
}

// CheckHeld checks whether a lock file at the given path exists and is held
// by a live process. Unlike IsHeld, it returns an error when the file exists
// but cannot be read, allowing callers to distinguish unreadable files from
// absent or dead locks.
func CheckHeld(lockPath string) (bool, error) {
	meta, err := ReadMetadata(lockPath)
	if err != nil {
		return false, err
	}
	return meta.IsActive(), nil
}

// ReadMetadata reads a lock file once and returns its parsed holder metadata.
// Missing, empty, invalid, or dead locks are reported as StatusInactive.
// Unreadable locks return StatusUnknown along with the read error.
func ReadMetadata(lockPath string) (Metadata, error) {
	data, err := osReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Metadata{Status: StatusInactive}, nil
		}
		return Metadata{Status: StatusUnknown}, err
	}
	return metadataFromContent(string(data)), nil
}

func metadataFromContent(content string) Metadata {
	meta := Metadata{
		Identity: strings.TrimSpace(content),
		Status:   StatusInactive,
	}
	if meta.Identity == "" {
		return meta
	}

	parts := strings.SplitN(meta.Identity, ":", 2)
	if pid, err := strconv.Atoi(parts[0]); err == nil && pid > 0 {
		meta.PID = pid
	}
	if process.IsLockHolderAlive(meta.Identity) {
		meta.Status = StatusActive
	}
	return meta
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

	for attempt := 0; attempt < acquireMaxAttempts; attempt++ {
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
		linkErr := osLink(tmpPath, lockFile)
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
				sleepBeforeRetry(attempt)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: read %s lock: %v\n", name, readErr)
			return nil, false
		}

		if metadataFromContent(string(data)).IsActive() {
			return nil, false
		}
		// Empty content (crash between create and write) or dead PID: reclaim.
		if removeErr := osRemove(lockFile); removeErr != nil && !os.IsNotExist(removeErr) {
			fmt.Fprintf(os.Stderr, "warning: remove stale %s lock: %v\n", name, removeErr)
			return nil, false
		}
		sleepBeforeRetry(attempt)
	}

	return nil, false
}

func sleepBeforeRetry(attempt int) {
	if attempt+1 >= acquireMaxAttempts {
		return
	}
	acquireRetrySleepFn(acquireRetryBaseDelay + acquireRetryJitterFn(acquireRetryJitterMax))
}
