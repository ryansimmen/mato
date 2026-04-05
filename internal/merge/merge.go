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
var errSquashMergeConflict = errors.New("squash merge conflict")
var errPushAfterSquashFailed = errors.New("push failed after squash merge")
var removeTaskFileFn = os.Remove

type mergeResult struct {
	commitSHA    string
	filesChanged []string
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
// priority-sorted slice of candidates. Unparseable or marker-less files are
// routed through the normal failure/requeue path with a stderr warning.
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

		taskBranch := strings.TrimSpace(taskfile.ParseBranch(path))
		if taskBranch == "" {
			ui.Warnf("warning: ready-to-merge task %s is missing a required branch marker\n", name)
			if failureErr := failMergeTask(path, mergeFailureDestination(tasksDir, path, name), errTaskBranchMarkerMissing.Error()); failureErr != nil {
				ui.Warnf("warning: could not requeue task %s after missing branch marker: %v\n", name, failureErr)
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
			recoverCompletionDetailForMergedTask(repoRoot, tasksDir, branch, task)
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
		if result != nil {
			detail := messaging.CompletionDetail{
				TaskID:       task.id,
				TaskFile:     task.name,
				Branch:       taskBranchName(task),
				CommitSHA:    result.commitSHA,
				FilesChanged: result.filesChanged,
				Title:        task.title,
			}
			if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
				ui.Warnf("warning: could not write completion detail for task %s: %v\n", task.name, err)
			}
		}
		if err := markTaskMerged(task.path); err != nil {
			ui.Warnf("warning: merged task %s but could not mark completion: %v\n", task.name, err)
			// Continue to moveTaskWithRetry: moving to completed/ is more
			// important than the merged record. If the move also fails,
			// leave the task branch in place so a later cycle can recover
			// using the already-merged detection path.
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
func recoverCompletionDetailForMergedTask(repoRoot, tasksDir, branch string, task mergeQueueTask) {
	if task.id == "" {
		return
	}
	if _, err := messaging.ReadCompletionDetail(tasksDir, task.id); err == nil {
		return
	}

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		ui.Warnf("warning: could not clone for completion detail recovery of %s: %v\n", task.name, err)
		return
	}
	defer git.RemoveClone(cloneDir)

	if _, err := gitOutput(cloneDir, "fetch", "origin"); err != nil {
		ui.Warnf("warning: could not fetch for completion detail recovery of %s: %v\n", task.name, err)
		return
	}

	sha, filesChanged := recoverMergedTaskMetadata(cloneDir, branch, task)
	detail := messaging.CompletionDetail{
		TaskID:       task.id,
		TaskFile:     task.name,
		Branch:       taskBranchName(task),
		CommitSHA:    sha,
		FilesChanged: filesChanged,
		Title:        task.title,
	}
	if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
		ui.Warnf("warning: could not write completion detail for recovered task %s: %v\n", task.name, err)
	}
}

// AcquireLock attempts to acquire an exclusive merge lock.
// Returns a cleanup function and true if acquired, or nil and false if already held.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireLock(tasksDir string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Acquire(locksDir, "merge")
}
