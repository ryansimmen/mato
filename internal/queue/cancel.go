package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/frontmatter"
	"mato/internal/runtimecleanup"
	"mato/internal/taskfile"
)

// appendCancelledRecordFn is the function used to append the cancelled marker.
// Tests replace it to inject failure deterministically.
var appendCancelledRecordFn = taskfile.AppendCancelledRecord

// CancelResult carries the outcome of a single CancelTask call.
type CancelResult struct {
	Filename   string   `json:"filename"`
	PriorState string   `json:"prior_state"`
	Warnings   []string `json:"warnings,omitempty"`
}

// CancelTask cancels the named task reference.
func CancelTask(tasksDir, taskRef string) (CancelResult, error) {
	ref := strings.TrimSpace(taskRef)
	if ref == "" {
		return CancelResult{}, fmt.Errorf("task name must not be empty")
	}

	idx := BuildIndex(tasksDir)
	match, err := ResolveTask(idx, ref)
	if err != nil {
		return CancelResult{}, err
	}

	stem := frontmatter.TaskFileStem(match.Filename)
	if match.State == DirCompleted {
		return CancelResult{}, fmt.Errorf("cannot cancel %s: task has already been merged", stem)
	}

	result := CancelResult{
		Filename:   match.Filename,
		PriorState: match.State,
		Warnings:   downstreamWarnings(tasksDir, idx, match),
	}

	if match.State == DirFailed {
		if err := appendCancelledRecordFn(match.Path); err != nil {
			return CancelResult{}, fmt.Errorf("write cancelled marker to %s: %w", match.Path, err)
		}
		runtimecleanup.DeleteAll(tasksDir, match.Filename)
		return result, nil
	}

	failedPath := filepath.Join(tasksDir, DirFailed, match.Filename)
	if err := AtomicMove(match.Path, failedPath); err != nil {
		if errors.Is(err, ErrDestinationExists) {
			return CancelResult{}, fmt.Errorf("cannot cancel %s: already exists in failed/", stem)
		}
		return CancelResult{}, fmt.Errorf("move task to failed/: %w", err)
	}

	if err := appendCancelledRecordFn(failedPath); err != nil {
		if rollbackErr := AtomicMove(failedPath, match.Path); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "error: cancelled marker write failed and rollback to %s/ also failed: %v\n", match.State, rollbackErr)
			return CancelResult{}, fmt.Errorf("write cancelled marker: %w (rollback failed: %v)", err, rollbackErr)
		}
		return CancelResult{}, fmt.Errorf("write cancelled marker to %s: %w (rolled back to %s/)", failedPath, err, match.State)
	}
	runtimecleanup.DeleteAll(tasksDir, match.Filename)

	return result, nil
}
func downstreamWarnings(tasksDir string, idx *PollIndex, match TaskMatch) []string {
	stem := frontmatter.TaskFileStem(match.Filename)
	taskID := stem
	if match.Snapshot != nil && match.Snapshot.Meta.ID != "" {
		taskID = match.Snapshot.Meta.ID
	}

	blockedBacklog := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	warnings := make(map[string]struct{})

	for _, snap := range idx.TasksByState(DirWaiting) {
		if dependsOnCancelledTask(snap.Meta.DependsOn, stem, taskID) {
			warnings[DirWaiting+"/"+snap.Filename] = struct{}{}
		}
	}
	for _, snap := range idx.TasksByState(DirBacklog) {
		if _, blocked := blockedBacklog[snap.Filename]; !blocked {
			continue
		}
		if dependsOnCancelledTask(snap.Meta.DependsOn, stem, taskID) {
			warnings[DirBacklog+"/"+snap.Filename] = struct{}{}
		}
	}

	result := make([]string, 0, len(warnings))
	for warning := range warnings {
		result = append(result, warning)
	}
	sort.Strings(result)
	return result
}

func dependsOnCancelledTask(dependsOn []string, stem, taskID string) bool {
	for _, dep := range dependsOn {
		if dep == stem || dep == taskID {
			return true
		}
	}
	return false
}
