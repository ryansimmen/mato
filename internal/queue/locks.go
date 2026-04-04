package queue

import (
	"os"
	"path/filepath"
	"strings"

	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/ui"
)

// CleanStaleLocks removes lock files for agents that are no longer running.
func CleanStaleLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, DirLocks)
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".pid")
		status, err := identity.DescribeAgentActivity(tasksDir, agentID)
		if err != nil || status != identity.AgentInactive {
			continue
		}
		os.Remove(filepath.Join(locksDir, e.Name()))
	}
}

// AcquireReviewLock attempts to acquire an exclusive lock for reviewing a
// specific task file. Returns a cleanup function and true if acquired, or
// nil and false if the lock is already held by a live process.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireReviewLock(tasksDir, taskFilename string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, DirLocks)
	return lockfile.Acquire(locksDir, "review-"+taskFilename)
}

// CleanStaleReviewLocks removes review lock files for processes that are no
// longer running, so that review tasks are not permanently blocked by dead
// agents.
func CleanStaleReviewLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, DirLocks)
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())
		held, err := lockfile.CheckHeld(lockPath)
		if err != nil {
			ui.Warnf("warning: could not verify review lock %s: %v\n", e.Name(), err)
			continue
		}
		if !held {
			if err := removeFn(lockPath); err != nil && !os.IsNotExist(err) {
				ui.Warnf("warning: could not remove stale review lock %s: %v\n", e.Name(), err)
			}
		}
	}
}
