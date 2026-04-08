// Package merge implements squash-merge queue processing for completed task
// branches. It serialises concurrent branch merges into the target branch,
// handling conflict detection and retry scheduling.
package merge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/lockfile"
	"mato/internal/messaging"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
	"mato/internal/ui"
)

type mergeQueueTask struct {
	name     string
	path     string
	title    string
	priority int
	branch   string
	id       string
	affects  []string
}

var errTaskBranchNotPushed = errors.New("task branch not pushed by agent")
var errTaskBranchMarkerMissing = errors.New("missing required <!-- branch: ... --> marker after work handoff")
var errTaskBranchMarkerInvalid = errors.New("invalid required <!-- branch: ... --> marker after work handoff")
var errSquashMergeConflict = errors.New("squash merge conflict")
var errPushAfterSquashFailed = errors.New("push failed after squash merge")
var removeTaskFileFn = os.Remove
var writeCompletionDetailFn = messaging.WriteCompletionDetail

type mergeResult struct {
	commitSHA    string
	filesChanged []string
	mergedAt     time.Time
}

const mergedTaskRecordPrefix = "<!-- merged: merge-queue at "

// ProcessQueue merges completed task branches into the target branch.
// It uses a background context for callers that do not participate in
// cancellation.
func ProcessQueue(repoRoot, tasksDir, branch string) int {
	return ProcessQueueContext(context.Background(), repoRoot, tasksDir, branch)
}

// ProcessQueueContext merges completed task branches into the target branch.
// It requires each ready-to-merge task to carry an explicit <!-- branch: ... -->
// marker written during the work handoff; tasks missing that marker are routed
// through the normal merge-failure requeue/failed path instead of guessing a
// branch name from the filename.
// Returns the number of tasks successfully merged.
func ProcessQueueContext(ctx context.Context, repoRoot, tasksDir, branch string) int {
	if ctx.Err() != nil {
		return 0
	}

	readyDir := filepath.Join(tasksDir, dirs.ReadyMerge)
	candidates, err := loadMergeCandidates(readyDir, tasksDir)
	if err != nil {
		return 0
	}

	return executeMergeRound(ctx, repoRoot, tasksDir, branch, candidates)
}

// loadMergeCandidates reads task files from dir, parses frontmatter for each
// .md file, requires an explicit recorded branch marker, and returns a
// priority-sorted slice of candidates. Unparseable tasks and tasks with missing
// or invalid branch markers are routed through the normal failure/requeue path
// with a stderr warning.
func loadMergeCandidates(dir, tasksDir string) ([]mergeQueueTask, error) {
	names, err := taskfile.ListTaskFiles(dir)
	if err != nil {
		return nil, err
	}

	tasks := make([]mergeQueueTask, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			ui.Warnf("warning: could not parse ready-to-merge task %s: %v\n", name, err)
			if failureErr := failMergeTask(path, filepath.Join(tasksDir, dirs.Backlog, name), fmt.Sprintf("parse task file: %v", err)); failureErr != nil {
				ui.Warnf("warning: could not requeue task %s: %v\n", name, failureErr)
			}
			continue
		}

		taskBranch, branchErr := loadMergeTaskBranch(path)
		if branchErr != nil {
			if errors.Is(branchErr, errTaskBranchMarkerMissing) {
				ui.Warnf("warning: ready-to-merge task %s is missing a required branch marker\n", name)
			} else {
				ui.Warnf("warning: ready-to-merge task %s has an invalid branch marker: %v\n", name, branchErr)
			}
			if failureErr := failMergeTask(path, mergeFailureDestination(tasksDir, path, name), branchErr.Error()); failureErr != nil {
				ui.Warnf("warning: could not requeue task %s after branch-marker validation failure: %v\n", name, failureErr)
			}
			continue
		}

		tasks = append(tasks, mergeQueueTask{
			name:     name,
			path:     path,
			title:    frontmatter.ExtractTitle(name, body),
			priority: meta.Priority,
			branch:   taskBranch,
			id:       meta.ID,
			affects:  meta.Affects,
		})
	}

	slices.SortFunc(tasks, func(a, b mergeQueueTask) int {
		if c := cmp.Compare(a.priority, b.priority); c != 0 {
			return c
		}
		return cmp.Compare(a.name, b.name)
	})

	return tasks, nil
}

func loadMergeTaskBranch(path string) (string, error) {
	data, err := taskfile.ReadRegularTaskFile(path)
	if err != nil {
		return "", fmt.Errorf("read branch marker: %w", err)
	}

	taskBranch, ok := taskfile.ParseBranchMarkerLine(data)
	if !ok {
		return "", errTaskBranchMarkerMissing
	}
	taskBranch = strings.TrimSpace(taskBranch)
	if taskBranch == "" {
		return "", errTaskBranchMarkerMissing
	}
	if err := git.ValidateBranch(taskBranch); err != nil {
		return "", fmt.Errorf("%w: %v", errTaskBranchMarkerInvalid, err)
	}
	return taskBranch, nil
}

// executeMergeRound iterates sorted candidates, performing squash merges into
// the target branch. It handles already-merged tasks, conflict requeue, retry
// budgets, and completion bookkeeping. Returns the number of tasks merged.
func executeMergeRound(ctx context.Context, repoRoot, tasksDir, branch string, tasks []mergeQueueTask) int {
	merged := 0
	for _, task := range tasks {
		if ctx.Err() != nil {
			return merged
		}

		completedPath := filepath.Join(tasksDir, dirs.Completed, task.name)
		if taskHasMergeSuccessRecord(task.path) {
			if !recoverCompletionDetailForMergedTask(repoRoot, tasksDir, branch, task) {
				continue
			}
			if err := moveTaskWithRetry(ctx, task.path, completedPath); err != nil {
				// If the destination already exists, the task was already
				// moved to completed/ by a prior cycle. Remove the
				// ready-to-merge copy to avoid an infinite retry loop.
				if _, statErr := os.Stat(completedPath); statErr == nil {
					if removeErr := os.Remove(task.path); removeErr != nil {
						ui.Warnf("warning: could not remove duplicate ready-to-merge task %s: %v\n", task.name, removeErr)
					} else {
						runtimedata.DeleteRuntimeArtifacts(tasksDir, task.name)
						cleanupTaskBranch(repoRoot, taskBranchName(task))
						merged++
					}
				} else {
					ui.Warnf("warning: merged task %s but could not move to completed: %v\n", task.name, err)
				}
				continue
			}
			runtimedata.DeleteRuntimeArtifacts(tasksDir, task.name)
			cleanupTaskBranch(repoRoot, taskBranchName(task))
			merged++
			continue
		}

		result, err := mergeReadyTask(repoRoot, branch, task)
		if err != nil {
			ui.Warnf("warning: could not merge task %s: %v\n", task.name, err)
			if failureErr := handleMergeFailure(repoRoot, tasksDir, task, err); failureErr != nil {
				ui.Warnf("warning: could not record merge failure for task %s: %v\n", task.name, failureErr)
			}
			continue
		}
		mergeTime := time.Time{}
		detailWritten := true
		if result != nil {
			mergeTime = result.mergedAt
			detail := messaging.CompletionDetail{
				TaskID:       task.id,
				TaskFile:     task.name,
				Branch:       taskBranchName(task),
				CommitSHA:    result.commitSHA,
				FilesChanged: result.filesChanged,
				Title:        task.title,
				MergedAt:     mergeTime,
			}
			if err := writeCompletionDetailFn(tasksDir, detail); err != nil {
				ui.Warnf("warning: could not write completion detail for task %s: %v\n", task.name, err)
				detailWritten = false
			}
		}
		if err := markTaskMergedAt(task.path, mergeTime); err != nil {
			ui.Warnf("warning: merged task %s but could not mark completion: %v\n", task.name, err)
			// Continue to moveTaskWithRetry: moving to completed/ is more
			// important than the merged record. If the move also fails,
			// leave the task branch in place so a later cycle can recover
			// using the already-merged detection path.
		}
		if !detailWritten {
			continue
		}
		bookkeepingComplete := false
		if err := moveTaskWithRetry(ctx, task.path, completedPath); err != nil {
			if _, statErr := os.Stat(completedPath); statErr == nil {
				if removeErr := removeTaskFileFn(task.path); removeErr != nil {
					ui.Warnf("warning: could not remove duplicate ready-to-merge task %s: %v\n", task.name, removeErr)
					continue
				}
				bookkeepingComplete = true
			} else {
				ui.Warnf("warning: merged task %s but could not move to completed: %v\n", task.name, err)
				continue
			}
		} else {
			bookkeepingComplete = true
		}
		if bookkeepingComplete {
			runtimedata.DeleteRuntimeArtifacts(tasksDir, task.name)
			cleanupTaskBranch(repoRoot, taskBranchName(task))
		}
		merged++
	}

	return merged
}

// HasReadyTasks reports whether any tasks are waiting in ready-to-merge/.
func HasReadyTasks(tasksDir string) bool {
	names, err := taskfile.ListTaskFiles(filepath.Join(tasksDir, dirs.ReadyMerge))
	if err != nil {
		return false
	}
	return len(names) > 0
}

// recoverCompletionDetailForMergedTask attempts to write a CompletionDetail
// for a task that already has a merged marker but may be missing its
// completion detail (e.g., a prior cycle merged successfully but crashed
// before writing the detail). If the detail already exists or the task has
// no ID, this is a no-op. Clone and metadata recovery failures are logged
// as warnings but never block the merge queue.
func recoverCompletionDetailForMergedTask(repoRoot, tasksDir, branch string, task mergeQueueTask) bool {
	if task.id == "" {
		return true
	}
	if _, err := messaging.ReadCompletionDetail(tasksDir, task.id); err == nil {
		return true
	}

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		ui.Warnf("warning: could not clone for completion detail recovery of %s: %v\n", task.name, err)
		return false
	}
	defer git.RemoveClone(cloneDir)

	if _, err := gitOutput(cloneDir, "fetch", "origin"); err != nil {
		ui.Warnf("warning: could not fetch for completion detail recovery of %s: %v\n", task.name, err)
		return false
	}

	result := recoverMergedTaskMetadata(cloneDir, branch, task)
	mergedAt := mergedMarkerTimestamp(task.path)
	if mergedAt.IsZero() {
		mergedAt = result.mergedAt
	}
	detail := messaging.CompletionDetail{
		TaskID:       task.id,
		TaskFile:     task.name,
		Branch:       taskBranchName(task),
		CommitSHA:    result.commitSHA,
		FilesChanged: result.filesChanged,
		Title:        task.title,
		MergedAt:     mergedAt,
	}
	if err := writeCompletionDetailFn(tasksDir, detail); err != nil {
		ui.Warnf("warning: could not write completion detail for recovered task %s: %v\n", task.name, err)
		return false
	}
	return true
}

func mergedMarkerTimestamp(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	ts, ok := taskfile.ParseMergedMarkerTimestamp(data)
	if !ok {
		return time.Time{}
	}
	return ts
}

// AcquireLock attempts to acquire an exclusive merge lock.
// Returns a cleanup function and true if acquired, or nil and false if already held.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireLock(tasksDir string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Acquire(locksDir, "merge")
}
