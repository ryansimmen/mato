package queue

import (
	"sort"
)

type backlogTask struct {
	name     string
	dir      string // source directory (e.g., "backlog", "in-progress", "ready-to-merge")
	path     string
	priority int
	affects  []string
}

// DeferralInfo describes why a task was excluded from the runnable queue.
type DeferralInfo struct {
	BlockedBy    string   // name of the conflicting task
	BlockedByDir string   // directory of the conflicting task (e.g., "in-progress", "backlog")
	OverlapFiles []string // files both tasks claim in affects
}

// DeferredOverlappingTasks returns the set of backlog task filenames that should
// be excluded from the queue because they conflict with higher-priority backlog
// tasks or active tasks in in-progress/ready-for-review/ready-to-merge. Tasks remain in backlog/
// (no file movement) to avoid churn between waiting/ and backlog/.
//
// When idx is nil, a temporary index is built internally.
func DeferredOverlappingTasks(tasksDir string, idx *PollIndex) map[string]struct{} {
	detailed := DeferredOverlappingTasksDetailed(tasksDir, idx)
	simple := make(map[string]struct{}, len(detailed))
	for name := range detailed {
		simple[name] = struct{}{}
	}
	return simple
}

// DeferredOverlappingTasksDetailed returns deferred tasks with the reason for deferral.
//
// When idx is nil, a temporary index is built internally.
func DeferredOverlappingTasksDetailed(tasksDir string, idx *PollIndex) map[string]DeferralInfo {
	idx = ensureIndex(tasksDir, idx)

	deferred := make(map[string]DeferralInfo)

	// Build sorted backlog tasks from the index.
	backlogSnaps := idx.TasksByState(DirBacklog)
	tasks := make([]backlogTask, 0, len(backlogSnaps))
	for _, snap := range backlogSnaps {
		tasks = append(tasks, backlogTask{
			name:     snap.Filename,
			path:     snap.Path,
			priority: snap.Meta.Priority,
			affects:  snap.Meta.Affects,
		})
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})

	// Use index-derived active affects instead of rescanning filesystem.
	active := idx.ActiveAffects()
	kept := make([]backlogTask, 0, len(tasks)+len(active))
	for _, at := range active {
		kept = append(kept, backlogTask{
			name:    at.Name,
			dir:     at.Dir,
			affects: at.Affects,
		})
	}
	for _, task := range tasks {
		isDef := false
		for _, other := range kept {
			overlap := overlappingAffects(task.affects, other.affects)
			if len(overlap) > 0 {
				blockedByDir := other.dir
				if blockedByDir == "" {
					blockedByDir = DirBacklog
				}
				deferred[task.name] = DeferralInfo{
					BlockedBy:    other.name,
					BlockedByDir: blockedByDir,
					OverlapFiles: overlap,
				}
				isDef = true
				break
			}
		}
		if !isDef {
			task.dir = DirBacklog
			kept = append(kept, task)
		}
	}

	return deferred
}

func overlappingAffects(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(a))
	for _, item := range a {
		if item == "" {
			continue
		}
		seen[item] = struct{}{}
	}

	overlap := make([]string, 0)
	added := make(map[string]struct{})
	for _, item := range b {
		if _, ok := seen[item]; !ok {
			continue
		}
		if _, ok := added[item]; ok {
			continue
		}
		added[item] = struct{}{}
		overlap = append(overlap, item)
	}
	sort.Strings(overlap)
	return overlap
}
