package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/queueview"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
)

// appendCancelledRecordFn is the function used to append the cancelled marker.
// Tests replace it to inject failure deterministically.
var appendCancelledRecordFn = taskfile.AppendCancelledRecord

var cancelAllStates = []string{
	dirs.Waiting,
	dirs.Backlog,
	dirs.InProgress,
	dirs.ReadyReview,
	dirs.ReadyMerge,
	dirs.Failed,
}

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

	idx := queueview.BuildIndex(tasksDir)
	match, err := queueview.ResolveTask(idx, ref)
	if err != nil {
		return CancelResult{}, err
	}

	return cancelResolvedTask(tasksDir, idx, match)
}

// ListCancellableTasks returns every task eligible for --all cancellation,
// sorted by filename and then queue-state order. completed/ is always excluded.
func ListCancellableTasks(tasksDir string) []TaskMatch {
	idx := queueview.BuildIndex(tasksDir)
	matches := make([]TaskMatch, 0)
	stateSet := make(map[string]struct{}, len(cancelAllStates))
	for _, state := range cancelAllStates {
		stateSet[state] = struct{}{}
		for _, snap := range idx.TasksByState(state) {
			matches = append(matches, TaskMatch{
				Filename: snap.Filename,
				State:    snap.State,
				Path:     snap.Path,
				Snapshot: snap,
			})
		}
	}
	for _, pf := range idx.ParseFailures() {
		if _, ok := stateSet[pf.State]; !ok {
			continue
		}
		pf := pf
		matches = append(matches, TaskMatch{
			Filename:     pf.Filename,
			State:        pf.State,
			Path:         pf.Path,
			ParseFailure: &pf,
		})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Filename != matches[j].Filename {
			return matches[i].Filename < matches[j].Filename
		}
		return cancelStateOrder(matches[i].State) < cancelStateOrder(matches[j].State)
	})
	return matches
}

// CancelTaskMatch cancels a previously resolved task match.
func CancelTaskMatch(tasksDir string, match TaskMatch) (CancelResult, error) {
	idx := queueview.BuildIndex(tasksDir)
	return cancelResolvedTask(tasksDir, idx, match)
}

func cancelResolvedTask(tasksDir string, idx *PollIndex, match TaskMatch) (CancelResult, error) {
	stem := frontmatter.TaskFileStem(match.Filename)
	if match.State == dirs.Completed {
		return CancelResult{}, fmt.Errorf("cannot cancel %s: task has already been merged", stem)
	}

	result := CancelResult{
		Filename:   match.Filename,
		PriorState: match.State,
		Warnings:   downstreamWarnings(tasksDir, idx, match),
	}

	if match.State == dirs.Failed {
		if err := appendCancelledRecordFn(match.Path); err != nil {
			return CancelResult{}, fmt.Errorf("write cancelled marker to %s: %w", match.Path, err)
		}
		runtimedata.DeleteRuntimeArtifacts(tasksDir, match.Filename)
		return result, nil
	}

	failedPath := filepath.Join(tasksDir, dirs.Failed, match.Filename)
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
	runtimedata.DeleteRuntimeArtifacts(tasksDir, match.Filename)

	return result, nil
}

func downstreamWarnings(tasksDir string, idx *PollIndex, match TaskMatch) []string {
	stem := frontmatter.TaskFileStem(match.Filename)
	taskID := stem
	if match.Snapshot != nil && match.Snapshot.Meta.ID != "" {
		taskID = match.Snapshot.Meta.ID
	}

	blockedBacklog := queueview.DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	warnings := make(map[string]struct{})

	for _, snap := range idx.TasksByState(dirs.Waiting) {
		if dependsOnCancelledTask(snap.Meta.DependsOn, stem, taskID) {
			warnings[dirs.Waiting+"/"+snap.Filename] = struct{}{}
		}
	}
	for _, snap := range idx.TasksByState(dirs.Backlog) {
		if _, blocked := blockedBacklog[snap.Filename]; !blocked {
			continue
		}
		if dependsOnCancelledTask(snap.Meta.DependsOn, stem, taskID) {
			warnings[dirs.Backlog+"/"+snap.Filename] = struct{}{}
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

func cancelStateOrder(state string) int {
	for i, candidate := range cancelAllStates {
		if candidate == state {
			return i
		}
	}
	return len(cancelAllStates)
}
