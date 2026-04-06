package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/ui"
)

const (
	claimBranchAssignmentLockName   = "claim-branch-assignment"
	claimBranchAssignmentRetryDelay = 5 * time.Millisecond
	claimBranchAssignmentWaitLimit  = 5 * time.Second
)

var claimBranchAssignmentSleepFn = time.Sleep

// CleanStaleLocks removes lock files for agents that are no longer running.
func CleanStaleLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, dirs.Locks)
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
	locksDir := filepath.Join(tasksDir, dirs.Locks)
	return lockfile.Acquire(locksDir, "review-"+taskFilename)
}

// acquireClaimBranchAssignmentLock serializes branch selection so concurrent
// claimers cannot choose the same task branch before a marker is written.
func acquireClaimBranchAssignmentLock(tasksDir string) (func(), error) {
	locksDir := filepath.Join(tasksDir, dirs.Locks)
	lockPath := filepath.Join(locksDir, claimBranchAssignmentLockName+".lock")
	deadline := time.Now().Add(claimBranchAssignmentWaitLimit)

	for {
		if cleanup, ok := lockfile.Acquire(locksDir, claimBranchAssignmentLockName); ok {
			return cleanup, nil
		}

		held, err := lockfile.CheckHeld(lockPath)
		if err != nil {
			return nil, fmt.Errorf("check claim branch assignment lock: %w", err)
		}
		if !held {
			if cleanup, ok := lockfile.Acquire(locksDir, claimBranchAssignmentLockName); ok {
				return cleanup, nil
			}
			held, err = lockfile.CheckHeld(lockPath)
			if err != nil {
				return nil, fmt.Errorf("check claim branch assignment lock after retry: %w", err)
			}
			if !held {
				return nil, fmt.Errorf("claim branch assignment lock unavailable")
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for claim branch assignment lock")
		}
		claimBranchAssignmentSleepFn(claimBranchAssignmentRetryDelay)
	}
}

// CleanStaleReviewLocks removes review lock files for processes that are no
// longer running, so that review tasks are not permanently blocked by dead
// agents.
func CleanStaleReviewLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, dirs.Locks)
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
