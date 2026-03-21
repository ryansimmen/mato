package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/taskfile"
)

// ParseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func ParseClaimedBy(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	agent, _ := taskfile.ParseClaimedBy(data)
	return agent
}

// HasAvailableTasks reports whether there is at least one claimable .md task
// file in backlog/ that is not in the deferred exclusion set.
func HasAvailableTasks(tasksDir string, deferred map[string]struct{}) bool {
	names, err := ListTaskFiles(filepath.Join(tasksDir, DirBacklog))
	if err != nil {
		return false
	}
	for _, name := range names {
		if deferred != nil {
			if _, excluded := deferred[name]; excluded {
				continue
			}
		}
		return true
	}
	return false
}

// RegisterAgent writes a lock file containing "PID:starttime" so concurrent
// mato instances can detect PID reuse. Falls back to PID-only when start time
// is unavailable (non-Linux). Returns a cleanup function.
func RegisterAgent(tasksDir, agentID string) (func(), error) {
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Register(locksDir, agentID)
}

// RecoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
// If the same task already exists in a later-state directory, the
// in-progress copy is treated as stale and removed instead of recovered.
func RecoverOrphanedTasks(tasksDir string) {
	inProgress := filepath.Join(tasksDir, DirInProgress)
	names, err := ListTaskFiles(inProgress)
	if err != nil {
		return
	}
	for _, name := range names {
		src := filepath.Join(inProgress, name)

		if laterDir := laterStateDuplicateDir(tasksDir, name); laterDir != "" {
			if err := os.Remove(src); err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "warning: could not remove stale in-progress copy %s: %v\n", name, err)
				}
				continue
			}
			fmt.Printf("Removing stale in-progress copy of %s (already in %s/)\n", name, laterDir)
			continue
		}

		if agent := ParseClaimedBy(src); agent != "" && identity.IsAgentActive(tasksDir, agent) {
			fmt.Printf("Skipping in-progress task %s (agent %s still active)\n", name, agent)
			continue
		}

		dst := filepath.Join(tasksDir, DirBacklog, name)
		if err := AtomicMove(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not recover orphaned task %s: %v\n", name, err)
			continue
		}

		f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not open task file to append failure record for %s: %v\n", name, err)
		} else {
			_, writeErr := fmt.Fprintf(f, "\n<!-- failure: mato-recovery at %s — agent was interrupted -->\n",
				time.Now().UTC().Format(time.RFC3339))
			closeErr := f.Close()
			if writeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", name, writeErr)
			} else if closeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", name, closeErr)
			}
		}

		fmt.Printf("Recovered orphaned task %s back to backlog\n", name)
	}
}

func laterStateDuplicateDir(tasksDir, name string) string {
	for _, laterDir := range []string{DirReadyReview, DirReadyMerge, DirCompleted, DirFailed} {
		if _, err := os.Stat(filepath.Join(tasksDir, laterDir, name)); err == nil {
			return laterDir
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not check %s for duplicate %s: %v\n", laterDir, name, err)
		}
	}
	return ""
}

// ErrDestinationExists is returned by AtomicMove when the destination path
// already exists. Callers can check for it with errors.Is.
var ErrDestinationExists = errors.New("destination already exists")

// AtomicMove atomically moves src to dst using os.Link + os.Remove to prevent
// TOCTOU races. If the destination already exists, it returns
// ErrDestinationExists. On cross-device links (EXDEV), it falls back to
// O_CREATE|O_EXCL + copy + remove, which is still TOCTOU-safe at the
// destination.
func AtomicMove(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("atomic move %s → %s: %w", src, dst, ErrDestinationExists)
		}
		// Cross-device link: fall back to exclusive-create + copy.
		if errors.Is(err, syscall.EXDEV) {
			return crossDeviceMove(src, dst)
		}
		return fmt.Errorf("atomic move %s → %s: link: %w", src, dst, err)
	}
	if err := os.Remove(src); err != nil {
		// The move is logically complete (dst exists), so warn but don't fail.
		fmt.Fprintf(os.Stderr, "warning: could not remove source %s after linking to %s: %v\n", src, dst, err)
	}
	return nil
}

// crossDeviceMove handles the EXDEV case where src and dst are on different
// filesystems. It uses O_CREATE|O_EXCL to atomically fail if the destination
// already exists, then copies the content and removes the source.
func crossDeviceMove(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("atomic move %s → %s: read source: %w", src, dst, err)
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("atomic move %s → %s: %w", src, dst, ErrDestinationExists)
		}
		return fmt.Errorf("atomic move %s → %s: create destination: %w", src, dst, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(dst)
		return fmt.Errorf("atomic move %s → %s: write destination: %w", src, dst, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("atomic move %s → %s: close destination: %w", src, dst, err)
	}
	if err := os.Remove(src); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove source %s after copying to %s: %v\n", src, dst, err)
	}
	return nil
}
