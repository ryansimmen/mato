package merge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
	"mato/internal/queue"
	"mato/internal/runtimecleanup"
	"mato/internal/taskfile"
	"mato/internal/taskstate"
)

var removeBranchMarkerFn = removeBranchMarker
var cleanupTaskBranchFn = cleanupTaskBranch
var atomicMoveFn = queue.AtomicMove

func handleMergeFailure(repoRoot, tasksDir string, task mergeQueueTask, mergeErr error) error {
	dst := mergeFailureDestination(tasksDir, task.path, task.name)
	if err := failMergeTask(task.path, dst, mergeErr.Error()); err != nil {
		return err
	}
	if filepath.Dir(dst) == filepath.Join(tasksDir, queue.DirFailed) {
		runtimecleanup.DeleteAll(tasksDir, task.name)
		cleanupTaskBranchFn(repoRoot, taskBranchName(task))
	}
	if errors.Is(mergeErr, errSquashMergeConflict) && filepath.Dir(dst) == filepath.Join(tasksDir, queue.DirBacklog) {
		cleanupTaskBranchFn(repoRoot, taskBranchName(task))
		if err := removeBranchMarkerFn(dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not clear branch marker after merge-conflict cleanup for %s: %v\n", task.name, err)
		}
		if err := taskstate.Update(tasksDir, task.name, func(state *taskstate.TaskState) {
			state.TaskBranch = taskBranchName(task)
			state.LastOutcome = taskstate.OutcomeMergeConflictCleanup
		}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record merge-conflict cleanup taskstate for %s: %v\n", task.name, err)
		}
	}
	return nil
}
func mergeFailureDestination(tasksDir, taskPath, taskName string) string {
	dir := queue.DirBacklog
	if shouldFailTaskAfterNextFailure(taskPath) {
		dir = queue.DirFailed
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

	appendErr := appendTaskRecord(src, "<!-- failure: merge-queue at %s — %s -->", time.Now().UTC().Format(time.RFC3339), reason)
	if appendErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not append failure record to %s: %v\n", filepath.Base(src), appendErr)
	}
	if dst == "" {
		return appendErr
	}
	if err := queue.AtomicMove(src, dst); err != nil {
		if appendErr != nil {
			return fmt.Errorf("move task file after merge failure: %w (also failed to append failure record: %v)", err, appendErr)
		}
		return fmt.Errorf("move task file after merge failure: %w", err)
	}
	return nil
}

func markTaskMerged(path string) error {
	if taskHasMergeSuccessRecord(path) {
		return nil
	}
	if err := appendTaskRecord(path, "%s%s -->", mergedTaskRecordPrefix, time.Now().UTC().Format(time.RFC3339)); err != nil {
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
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task file for merge record: %w", err)
	}

	record := fmt.Sprintf(format, args...)
	updated := string(existing) + "\n" + record + "\n"

	if err := atomicwrite.WriteFile(path, []byte(updated)); err != nil {
		return fmt.Errorf("write merge record: %w", err)
	}
	return nil
}

func removeBranchMarker(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task file for branch marker removal: %w", err)
	}
	updated, _, removed := taskfile.RemoveBranchMarkerLine(data)
	if !removed {
		return nil
	}
	if err := atomicwrite.WriteFile(path, updated); err != nil {
		return fmt.Errorf("write task file without branch marker: %w", err)
	}
	return nil
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
