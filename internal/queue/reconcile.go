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
func resolvePromotableTasks(tasksDir string) []promotableTask {
	completedIDs := completedTaskIDs(tasksDir)
	nonCompletedIDs := nonCompletedTaskIDs(tasksDir)

	// Remove ambiguous IDs: if an ID appears in both completed and
	// non-completed directories, we cannot safely assume the dependency
	// is satisfied — it may refer to the non-completed copy.
	for id := range nonCompletedIDs {
		if _, dup := completedIDs[id]; dup {
			delete(completedIDs, id)
		}
	}

	waitingDir := filepath.Join(tasksDir, DirWaiting)
	names, err := ListTaskFiles(waitingDir)
	if err != nil {
		return nil
	}

	type parsedWaiting struct {
		name string
		path string
		meta frontmatter.TaskMeta
	}

	var parsed []parsedWaiting
	waitingDeps := make(map[string][]string, len(names))
	for _, name := range names {
		path := filepath.Join(waitingDir, name)
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			continue
		}
		parsed = append(parsed, parsedWaiting{name: name, path: path, meta: meta})
		waitingDeps[meta.ID] = meta.DependsOn
	}

	var result []promotableTask
	for _, task := range parsed {
		ready := true
		for _, dep := range task.meta.DependsOn {
			if dep == task.meta.ID {
				ready = false
				continue
			}
			if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, task.meta.ID, waitingDeps, map[string]struct{}{}) {
				ready = false
				continue
			}
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			ready = false
		}
		if !ready {
			continue
		}
		if hasActiveOverlap(tasksDir, task.meta.Affects) {
			continue
		}
		result = append(result, promotableTask{name: task.name, path: task.path, meta: task.meta})
	}
	return result
}

func ReconcileReadyQueue(tasksDir string) int {
	// Move unparseable waiting tasks to failed/ before resolving promotions.
	waitingDir := filepath.Join(tasksDir, DirWaiting)
	names, err := ListTaskFiles(waitingDir)
	if err == nil {
		for _, name := range names {
			path := filepath.Join(waitingDir, name)
			if _, _, parseErr := frontmatter.ParseTaskFile(path); parseErr != nil {
				fmt.Fprintf(os.Stderr, "warning: moving unparseable waiting task %s to failed/: %v\n", name, parseErr)
				failedPath := filepath.Join(tasksDir, DirFailed, name)
				if moveErr := AtomicMove(path, failedPath); moveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", name, moveErr)
				}
			}
		}
	}

	// Emit warnings for ambiguous IDs, self-dependencies, circular
	// dependencies, and unknown dependency IDs.
	completedIDs := completedTaskIDs(tasksDir)
	nonCompletedIDs := nonCompletedTaskIDs(tasksDir)
	for id := range nonCompletedIDs {
		if _, dup := completedIDs[id]; dup {
			fmt.Fprintf(os.Stderr, "warning: task ID %q exists in both completed and non-completed directories; dependency on it will not be satisfied\n", id)
		}
	}

	knownIDs := allKnownTaskIDs(tasksDir)
	waitingNames, _ := ListTaskFiles(waitingDir)
	waitingDeps := make(map[string][]string)
	type waitingInfo struct {
		name string
		meta frontmatter.TaskMeta
	}
	var waitingInfos []waitingInfo
	for _, name := range waitingNames {
		meta, _, parseErr := frontmatter.ParseTaskFile(filepath.Join(waitingDir, name))
		if parseErr != nil {
			continue
		}
		waitingDeps[meta.ID] = meta.DependsOn
		waitingInfos = append(waitingInfos, waitingInfo{name: name, meta: meta})
	}

	loggedCircularDeps := make(map[string]struct{})
	for _, task := range waitingInfos {
		for _, dep := range task.meta.DependsOn {
			if dep == task.meta.ID {
				fmt.Fprintf(os.Stderr, "warning: task %s depends on itself\n", task.meta.ID)
				continue
			}
			if _, ok := waitingDeps[dep]; ok && dependsOnWaitingTask(dep, task.meta.ID, waitingDeps, map[string]struct{}{}) {
				logCircularDependency(loggedCircularDeps, task.meta.ID, dep)
				continue
			}
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			if _, ok := knownIDs[dep]; !ok {
				fmt.Fprintf(os.Stderr, "warning: waiting task %s depends on unknown task ID %q (not found in any queue directory)\n", task.name, dep)
			}
		}
	}

	promotable := resolvePromotableTasks(tasksDir)
	promoted := 0
	for _, task := range promotable {
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
// would be promoted, without actually moving any files.
func CountPromotableWaitingTasks(tasksDir string) int {
	return len(resolvePromotableTasks(tasksDir))
}

func completedTaskIDs(tasksDir string) map[string]struct{} {
	completedDir := filepath.Join(tasksDir, DirCompleted)
	names, err := ListTaskFiles(completedDir)
	if err != nil {
		return map[string]struct{}{}
	}

	ids := make(map[string]struct{}, len(names)*2)
	for _, name := range names {
		stem := frontmatter.TaskFileStem(name)
		ids[stem] = struct{}{}
		path := filepath.Join(completedDir, name)
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse completed task %s: %v\n", name, err)
			continue
		}
		ids[meta.ID] = struct{}{}
	}
	return ids
}

// collectTaskIDs scans the given subdirectories under tasksDir and returns the
// set of task IDs found (both filename stems and frontmatter IDs).
func collectTaskIDs(tasksDir string, dirs []string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, dir := range dirs {
		names, err := ListTaskFiles(filepath.Join(tasksDir, dir))
		if err != nil {
			continue
		}
		for _, name := range names {
			ids[frontmatter.TaskFileStem(name)] = struct{}{}
			path := filepath.Join(tasksDir, dir, name)
			if meta, _, err := frontmatter.ParseTaskFile(path); err == nil {
				ids[meta.ID] = struct{}{}
			}
		}
	}
	return ids
}

// nonCompletedTaskIDs returns the set of task IDs found in all directories except completed/.
func nonCompletedTaskIDs(tasksDir string) map[string]struct{} {
	return collectTaskIDs(tasksDir, []string{DirWaiting, DirBacklog, DirInProgress, DirReadyReview, DirReadyMerge, DirFailed})
}

// allKnownTaskIDs returns the set of task IDs found across all queue directories.
func allKnownTaskIDs(tasksDir string) map[string]struct{} {
	return collectTaskIDs(tasksDir, AllDirs)
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
