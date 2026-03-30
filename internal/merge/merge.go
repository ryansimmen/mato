// Package merge implements squash-merge queue processing for completed task
// branches. It serialises concurrent branch merges into the target branch,
// handling conflict detection and retry scheduling.
package merge

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"mato/internal/frontmatter"
	"mato/internal/lockfile"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/runtimecleanup"
	"mato/internal/taskfile"
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
var errSquashMergeConflict = errors.New("squash merge conflict")
var errPushAfterSquashFailed = errors.New("push failed after squash merge")
var removeTaskFileFn = os.Remove

type mergeResult struct {
	commitSHA    string
	filesChanged []string
}

const mergedTaskRecordPrefix = "<!-- merged: merge-queue at "

// ProcessQueue merges completed task branches into the target branch.
// It scans ready-to-merge/ for task files, prefers branch metadata recorded in
// each task file, falls back to the filename-derived branch name for backward
// compatibility, and performs a squash merge.
// Active branches for fallback disambiguation are always resolved via a fresh
// filesystem scan (passing nil to CollectActiveBranches) to avoid stale data
// from a PollIndex snapshot that was built earlier in the poll cycle.
// Returns the number of tasks successfully merged.
func ProcessQueue(repoRoot, tasksDir, branch string) int {
	readyDir := filepath.Join(tasksDir, queue.DirReadyMerge)

	// Pass nil to force a fresh filesystem scan rather than relying on a
	// potentially stale PollIndex snapshot. The index is built at the
	// start of each poll cycle, but by the time ProcessQueue runs, task
	// claiming and review actions may have changed the set of active
	// branches. A fresh scan here ensures correct fallback branch
	// disambiguation for legacy tasks without a <!-- branch: --> marker.
	activeBranches := queue.CollectActiveBranches(tasksDir, nil)

	candidates, err := loadMergeCandidates(readyDir, tasksDir, activeBranches)
	if err != nil {
		return 0
	}

	return executeMergeRound(repoRoot, tasksDir, branch, candidates)
}

// loadMergeCandidates reads task files from dir, parses frontmatter for each
// .md file, resolves branch names (using activeBranches for fallback
// disambiguation), and returns a priority-sorted slice of candidates.
// Unparseable files are requeued to the backlog with a stderr warning.
func loadMergeCandidates(dir, tasksDir string, activeBranches map[string]struct{}) ([]mergeQueueTask, error) {
	names, err := queue.ListTaskFiles(dir)
	if err != nil {
		return nil, err
	}

	tasks := make([]mergeQueueTask, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse ready-to-merge task %s: %v\n", name, err)
			if failureErr := failMergeTask(path, filepath.Join(tasksDir, queue.DirBacklog, name), fmt.Sprintf("parse task file: %v", err)); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not requeue task %s: %v\n", name, failureErr)
			}
			continue
		}

		taskBranch := taskfile.ParseBranch(path)
		if taskBranch == "" {
			taskBranch = "task/" + frontmatter.SanitizeBranchName(name)
			if _, taken := activeBranches[taskBranch]; taken {
				taskBranch = taskBranch + "-" + frontmatter.BranchDisambiguator(name)
			}
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
func executeMergeRound(repoRoot, tasksDir, branch string, tasks []mergeQueueTask) int {
	merged := 0
	for _, task := range tasks {
		completedPath := filepath.Join(tasksDir, queue.DirCompleted, task.name)
		if taskHasMergeSuccessRecord(task.path) {
			if err := moveTaskWithRetry(task.path, completedPath); err != nil {
				// If the destination already exists, the task was already
				// moved to completed/ by a prior cycle. Remove the
				// ready-to-merge copy to avoid an infinite retry loop.
				if _, statErr := os.Stat(completedPath); statErr == nil {
					if removeErr := os.Remove(task.path); removeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: could not remove duplicate ready-to-merge task %s: %v\n", task.name, removeErr)
					} else {
						runtimecleanup.DeleteAll(tasksDir, task.name)
						cleanupTaskBranch(repoRoot, taskBranchName(task))
						merged++
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: merged task %s but could not move to completed: %v\n", task.name, err)
				}
				continue
			}
			runtimecleanup.DeleteAll(tasksDir, task.name)
			cleanupTaskBranch(repoRoot, taskBranchName(task))
			merged++
			continue
		}

		result, err := mergeReadyTask(repoRoot, branch, task)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not merge task %s: %v\n", task.name, err)
			if failureErr := handleMergeFailure(repoRoot, tasksDir, task, err); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not record merge failure for task %s: %v\n", task.name, failureErr)
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
				fmt.Fprintf(os.Stderr, "warning: could not write completion detail for task %s: %v\n", task.name, err)
			}
		}
		if err := markTaskMerged(task.path); err != nil {
			fmt.Fprintf(os.Stderr, "warning: merged task %s but could not mark completion: %v\n", task.name, err)
			// Continue to moveTaskWithRetry: moving to completed/ is more
			// important than the merged record. If the move also fails,
			// leave the task branch in place so a later cycle can recover
			// using the already-merged detection path.
		}
		bookkeepingComplete := false
		if err := moveTaskWithRetry(task.path, completedPath); err != nil {
			if _, statErr := os.Stat(completedPath); statErr == nil {
				if removeErr := removeTaskFileFn(task.path); removeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not remove duplicate ready-to-merge task %s: %v\n", task.name, removeErr)
					continue
				}
				bookkeepingComplete = true
			} else {
				fmt.Fprintf(os.Stderr, "warning: merged task %s but could not move to completed: %v\n", task.name, err)
				continue
			}
		} else {
			bookkeepingComplete = true
		}
		if bookkeepingComplete {
			runtimecleanup.DeleteAll(tasksDir, task.name)
			cleanupTaskBranch(repoRoot, taskBranchName(task))
		}
		merged++
	}

	return merged
}

// HasReadyTasks reports whether any tasks are waiting in ready-to-merge/.
func HasReadyTasks(tasksDir string) bool {
	names, err := queue.ListTaskFiles(filepath.Join(tasksDir, queue.DirReadyMerge))
	if err != nil {
		return false
	}
	return len(names) > 0
}

// AcquireLock attempts to acquire an exclusive merge lock.
// Returns a cleanup function and true if acquired, or nil and false if already held.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireLock(tasksDir string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Acquire(locksDir, "merge")
}
