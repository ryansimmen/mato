package queue

import (
	"os"
	"strings"

	"mato/internal/identity"
	"mato/internal/lockfile"

	"path/filepath"
)

// CleanStaleLocks removes lock files for agents that are no longer running.
func CleanStaleLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, ".locks")
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
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Acquire(locksDir, "review-"+taskFilename)
}

// CleanStaleReviewLocks removes review lock files for processes that are no
// longer running, so that review tasks are not permanently blocked by dead
// agents.
func CleanStaleReviewLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, ".locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "review-") || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		lockPath := filepath.Join(locksDir, e.Name())
		if !lockfile.IsHeld(lockPath) {
			os.Remove(lockPath)
		}
	}
}
