package merge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/lockfile"
	"mato/internal/queue"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
	"mato/internal/ui"
)

var removeBranchMarkerFn = removeBranchMarker
var cleanupTaskBranchFn = cleanupTaskBranch
var atomicMoveFn = queue.AtomicMove
var taskRecordLockSleepFn = time.Sleep
var removeBranchMarkerBeforeWriteHook = func() {}

const (
	taskRecordLockRetryDelay = 5 * time.Millisecond
	taskRecordLockWaitLimit  = 5 * time.Second
)

func handleMergeFailure(repoRoot, tasksDir string, task mergeQueueTask, mergeErr error) error {
	dst := mergeFailureDestination(tasksDir, task.path, task.name)
	if err := failMergeTask(task.path, dst, mergeErr.Error()); err != nil {
		return err
	}
	if filepath.Dir(dst) == filepath.Join(tasksDir, dirs.Failed) {
		runtimedata.DeleteRuntimeArtifacts(tasksDir, task.name)
		cleanupTaskBranchFn(repoRoot, taskBranchName(task))
	}
	if errors.Is(mergeErr, errSquashMergeConflict) && filepath.Dir(dst) == filepath.Join(tasksDir, dirs.Backlog) {
		cleanupTaskBranchFn(repoRoot, taskBranchName(task))
		if err := removeBranchMarkerFn(dst); err != nil {
			ui.Warnf("warning: could not clear branch marker after merge-conflict cleanup for %s: %v\n", task.name, err)
		}
		if err := runtimedata.UpdateTaskState(tasksDir, task.name, func(state *runtimedata.TaskState) {
			state.TaskBranch = taskBranchName(task)
			state.LastOutcome = runtimedata.OutcomeMergeConflictCleanup
		}); err != nil {
			ui.Warnf("warning: could not record merge-conflict cleanup taskstate for %s: %v\n", task.name, err)
		}
	}
	return nil
}
func mergeFailureDestination(tasksDir, taskPath, taskName string) string {
	dir := dirs.Backlog
	if shouldFailTaskAfterNextFailure(taskPath) {
		dir = dirs.Failed
	}
	return filepath.Join(tasksDir, dir, taskName)
}

func shouldFailTaskAfterNextFailure(taskPath string) bool {
	maxRetries := 3
	meta, _, err := frontmatter.ParseTaskFile(taskPath)
	if err == nil {
		maxRetries = meta.MaxRetries
	}

	failures, failErr := queue.CountFailureLines(taskPath)
	if failErr != nil {
		// Can't read the file — conservative choice: don't move to failed.
		return false
	}

	return failures+1 >= maxRetries
}

func failMergeTask(src, dst, reason string) error {
	reason = taskfile.SanitizeCommentText(reason)
	if reason == "" {
		reason = "merge queue failure"
	}

	if dst == "" {
		return appendMergeFailureRecord(src, reason)
	}
	if err := queue.AtomicMove(src, dst); err != nil {
		if errors.Is(err, queue.ErrDestinationExists) {
			if removeErr := removeTaskFileFn(src); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("remove duplicate merge-failed task after destination collision: %w", removeErr)
			}
			return nil
		}
		return fmt.Errorf("move task file after merge failure: %w", err)
	}
	if appendErr := appendMergeFailureRecord(dst, reason); appendErr != nil {
		ui.Warnf("warning: could not append failure record to %s: %v\n", filepath.Base(dst), appendErr)
	}
	return nil
}

func appendMergeFailureRecord(path, reason string) error {
	return appendTaskRecord(path, "<!-- failure: merge-queue at %s — %s -->", time.Now().UTC().Format(time.RFC3339), reason)
}

func markTaskMerged(path string) error {
	return markTaskMergedAt(path, time.Time{})
}

func markTaskMergedAt(path string, mergedAt time.Time) error {
	cleanup, err := acquireTaskRecordLock(path)
	if err != nil {
		return fmt.Errorf("acquire task record lock: %w", err)
	}
	defer cleanup()

	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task file for merged record: %w", err)
	}
	if taskfile.HasMergedMarker(existing) {
		return nil
	}
	if mergedAt.IsZero() {
		mergedAt = time.Now().UTC()
	} else {
		mergedAt = mergedAt.UTC()
	}
	if err := appendTaskRecordLocked(path, "%s%s -->", mergedTaskRecordPrefix, mergedAt.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("append merged record: %w", err)
	}
	return nil
}

func taskHasMergeSuccessRecord(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return taskfile.HasMergedMarker(data)
}

func appendTaskRecord(path, format string, args ...any) error {
	cleanup, err := acquireTaskRecordLock(path)
	if err != nil {
		return fmt.Errorf("acquire task record lock: %w", err)
	}
	defer cleanup()

	return appendTaskRecordLocked(path, format, args...)
}

func appendTaskRecordLocked(path, format string, args ...any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open task file for merge record append: %w", err)
	}

	record := fmt.Sprintf(format, args...)
	if _, err := file.WriteString("\n" + record + "\n"); err != nil {
		file.Close()
		return fmt.Errorf("append merge record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close task file after merge record append: %w", err)
	}
	return nil
}

func removeBranchMarker(path string) error {
	cleanup, err := acquireTaskRecordLock(path)
	if err != nil {
		return fmt.Errorf("acquire task record lock: %w", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task file for branch marker removal: %w", err)
	}
	updated, _, removed := taskfile.RemoveBranchMarkerLine(data)
	if !removed {
		return nil
	}
	removeBranchMarkerBeforeWriteHook()
	if err := atomicwrite.WriteFile(path, updated); err != nil {
		return fmt.Errorf("write task file without branch marker: %w", err)
	}
	return nil
}

func acquireTaskRecordLock(path string) (func(), error) {
	locksDir := filepath.Join(filepath.Dir(path), dirs.Locks)
	lockName := "merge-task-record-" + filepath.Base(path)
	lockPath := filepath.Join(locksDir, lockName+".lock")
	deadline := time.Now().Add(taskRecordLockWaitLimit)

	for {
		if cleanup, ok := lockfile.Acquire(locksDir, lockName); ok {
			return cleanup, nil
		}

		held, err := lockfile.CheckHeld(lockPath)
		if err != nil {
			return nil, fmt.Errorf("check task record lock: %w", err)
		}
		if !held {
			if cleanup, ok := lockfile.Acquire(locksDir, lockName); ok {
				return cleanup, nil
			}
			held, err = lockfile.CheckHeld(lockPath)
			if err != nil {
				return nil, fmt.Errorf("check task record lock after retry: %w", err)
			}
			if !held {
				return nil, fmt.Errorf("task record lock unavailable")
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for task record lock")
		}
		taskRecordLockSleepFn(taskRecordLockRetryDelay)
	}
}

// isPermanentMoveError reports whether err is clearly not transient and
// retrying the move would be pointless. Destination-already-exists,
// source-not-found, and permission errors fall into this category.
func isPermanentMoveError(err error) bool {
	return errors.Is(err, queue.ErrDestinationExists) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, os.ErrPermission)
}

func moveTaskWithRetry(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create task destination dir: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := atomicMoveFn(src, dst); err == nil {
			return nil
		} else {
			lastErr = err
			if isPermanentMoveError(err) {
				return err
			}
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return lastErr
}
