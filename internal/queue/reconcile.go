package queue

import (
	"fmt"
	"os"
	"path/filepath"

	"mato/internal/frontmatter"
)

// promotableTask describes a waiting task whose dependencies are satisfied.
type promotableTask struct {
	name string
	path string
	meta frontmatter.TaskMeta
}

// resolvePromotableTasks determines which waiting tasks have all dependencies
// met and are not blocked by active overlap. It is a pure read-only function:
// no file moves, no warnings to stderr.
//
// When idx is nil, a temporary index is built internally.
func resolvePromotableTasks(tasksDir string, idx *PollIndex) []promotableTask {
	idx = ensureIndex(tasksDir, idx)

	completedIDs := idx.CompletedIDs()
	nonCompletedIDs := idx.NonCompletedIDs()

	// Copy completedIDs so we can remove ambiguous entries without
	// mutating the index's map.
	safeCompleted := make(map[string]struct{}, len(completedIDs))
	for id := range completedIDs {
		safeCompleted[id] = struct{}{}
	}

	// Remove ambiguous IDs: if an ID appears in both completed and
	// non-completed directories, we cannot safely assume the dependency
	// is satisfied — it may refer to the non-completed copy.
	for id := range nonCompletedIDs {
		if _, dup := safeCompleted[id]; dup {
			delete(safeCompleted, id)
		}
	}

	waitingTasks := idx.TasksByState(DirWaiting)
	waitingDeps := make(map[string][]string, len(waitingTasks))
	for _, snap := range waitingTasks {
		waitingDeps[snap.Meta.ID] = snap.Meta.DependsOn
	}

	var result []promotableTask
	for _, snap := range waitingTasks {
		ready := true
		for _, dep := range snap.Meta.DependsOn {
			if dep == snap.Meta.ID {
				ready = false
				continue
			}
			if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, snap.Meta.ID, waitingDeps, map[string]struct{}{}) {
				ready = false
				continue
			}
			if _, ok := safeCompleted[dep]; ok {
				continue
			}
			ready = false
		}
		if !ready {
			continue
		}
		if idx.HasActiveOverlap(snap.Meta.Affects) {
			continue
		}
		result = append(result, promotableTask{name: snap.Filename, path: snap.Path, meta: snap.Meta})
	}
	return result
}

// ReconcileReadyQueue promotes waiting tasks whose dependencies are satisfied
// to backlog/. It also moves unparseable waiting/backlog tasks to failed/.
//
// When idx is nil, a temporary index is built internally.
func ReconcileReadyQueue(tasksDir string, idx *PollIndex) int {
	idx = ensureIndex(tasksDir, idx)

	// Move unparseable waiting/backlog tasks to failed/ using index parse failures.
	for _, pf := range idx.WaitingParseFailures() {
		fmt.Fprintf(os.Stderr, "warning: moving unparseable waiting task %s to failed/: %v\n", pf.Filename, pf.Err)
		failedPath := filepath.Join(tasksDir, DirFailed, pf.Filename)
		if moveErr := AtomicMove(pf.Path, failedPath); moveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", pf.Filename, moveErr)
		}
	}
	for _, pf := range idx.BacklogParseFailures() {
		fmt.Fprintf(os.Stderr, "warning: moving unparseable backlog task %s to failed/: %v\n", pf.Filename, pf.Err)
		failedPath := filepath.Join(tasksDir, DirFailed, pf.Filename)
		if moveErr := AtomicMove(pf.Path, failedPath); moveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", pf.Filename, moveErr)
		}
	}

	// Move backlog tasks with invalid glob syntax to failed/.
	for _, snap := range idx.TasksByState(DirBacklog) {
		if err := frontmatter.ValidateAffectsGlobs(snap.Meta.Affects); err != nil {
			fmt.Fprintf(os.Stderr, "warning: moving backlog task %s with invalid glob to failed/: %v\n", snap.Filename, err)
			failedPath := filepath.Join(tasksDir, DirFailed, snap.Filename)
			if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
			}
		}
	}

	// Emit warnings for ambiguous IDs, self-dependencies, circular
	// dependencies, and unknown dependency IDs.
	completedIDs := idx.CompletedIDs()
	nonCompletedIDs := idx.NonCompletedIDs()
	for id := range nonCompletedIDs {
		if _, dup := completedIDs[id]; dup {
			fmt.Fprintf(os.Stderr, "warning: task ID %q exists in both completed and non-completed directories; dependency on it will not be satisfied\n", id)
		}
	}

	knownIDs := idx.AllIDs()
	waitingTasks := idx.TasksByState(DirWaiting)
	waitingDeps := make(map[string][]string, len(waitingTasks))
	for _, snap := range waitingTasks {
		waitingDeps[snap.Meta.ID] = snap.Meta.DependsOn
	}

	loggedCircularDeps := make(map[string]struct{})
	for _, snap := range waitingTasks {
		for _, dep := range snap.Meta.DependsOn {
			if dep == snap.Meta.ID {
				fmt.Fprintf(os.Stderr, "warning: task %s depends on itself\n", snap.Meta.ID)
				continue
			}
			if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, snap.Meta.ID, waitingDeps, map[string]struct{}{}) {
				logCircularDependency(loggedCircularDeps, snap.Meta.ID, dep)
				continue
			}
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			if _, ok := knownIDs[dep]; !ok {
				fmt.Fprintf(os.Stderr, "warning: waiting task %s depends on unknown task ID %q (not found in any queue directory)\n", snap.Filename, dep)
			}
		}
	}

	promotable := resolvePromotableTasks(tasksDir, idx)
	promoted := 0
	for _, task := range promotable {
		// Quarantine tasks with invalid glob syntax instead of promoting.
		if err := frontmatter.ValidateAffectsGlobs(task.meta.Affects); err != nil {
			fmt.Fprintf(os.Stderr, "warning: moving waiting task %s with invalid glob to failed/: %v\n", task.name, err)
			failedPath := filepath.Join(tasksDir, DirFailed, task.name)
			if moveErr := AtomicMove(task.path, failedPath); moveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", task.name, moveErr)
			}
			continue
		}
		dst := filepath.Join(tasksDir, DirBacklog, task.name)
		if err := AtomicMove(task.path, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not promote waiting task %s: %v\n", task.name, err)
			continue
		}
		promoted++
	}

	return promoted
}

// CountPromotableWaitingTasks is a read-only variant of ReconcileReadyQueue.
// It returns the number of waiting tasks whose dependencies are satisfied and
// would be promoted, without actually moving any files. Tasks with invalid
// glob syntax are excluded (they would be quarantined, not promoted).
//
// When idx is nil, a temporary index is built internally.
func CountPromotableWaitingTasks(tasksDir string, idx *PollIndex) int {
	promotable := resolvePromotableTasks(tasksDir, idx)
	count := 0
	for _, task := range promotable {
		if frontmatter.ValidateAffectsGlobs(task.meta.Affects) == nil {
			count++
		}
	}
	return count
}

func dependsOnWaitingTask(taskID, targetID string, waitingDeps map[string][]string, visited map[string]struct{}) bool {
	if taskID == targetID {
		return true
	}
	if _, ok := visited[taskID]; ok {
		return false
	}
	visited[taskID] = struct{}{}
	for _, dep := range waitingDeps[taskID] {
		if dep == targetID {
			return true
		}
		if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, targetID, waitingDeps, visited) {
			return true
		}
	}
	return false
}

func logCircularDependency(logged map[string]struct{}, a, b string) {
	if a > b {
		a, b = b, a
	}
	key := a + "\x00" + b
	if _, ok := logged[key]; ok {
		return
	}
	logged[key] = struct{}{}
	fmt.Fprintf(os.Stderr, "warning: circular dependency detected between %s and %s\n", a, b)
}
