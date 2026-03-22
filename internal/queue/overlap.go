package queue

import (
	"sort"
	"strings"
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
	BlockedBy          string   // name of the conflicting task
	BlockedByDir       string   // directory of the conflicting task (e.g., "in-progress", "backlog")
	ConflictingAffects []string // affects entries from either task that overlap
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
					BlockedBy:          other.name,
					BlockedByDir:       blockedByDir,
					ConflictingAffects: overlap,
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

// affectsMatch reports whether two affects entries conflict. An entry ending
// with "/" is treated as a directory prefix that matches any path underneath
// it. Two prefix entries conflict if one contains the other.
func affectsMatch(a, b string) bool {
	if a == b {
		return true
	}
	if strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) {
		return true
	}
	if strings.HasSuffix(b, "/") && strings.HasPrefix(a, b) {
		return true
	}
	return false
}

// isDirPrefix reports whether s is a directory-prefix affects entry.
func isDirPrefix(s string) bool {
	return strings.HasSuffix(s, "/")
}

func overlappingAffects(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	// Filter empty strings and detect whether either list has prefix entries.
	aClean := make([]string, 0, len(a))
	hasPrefixA := false
	for _, item := range a {
		if item == "" {
			continue
		}
		aClean = append(aClean, item)
		if isDirPrefix(item) {
			hasPrefixA = true
		}
	}
	bClean := make([]string, 0, len(b))
	hasPrefixB := false
	for _, item := range b {
		if item == "" {
			continue
		}
		bClean = append(bClean, item)
		if isDirPrefix(item) {
			hasPrefixB = true
		}
	}

	if len(aClean) == 0 || len(bClean) == 0 {
		return nil
	}

	// Fast path: no prefix entries, use exact-match map lookup.
	if !hasPrefixA && !hasPrefixB {
		seen := make(map[string]struct{}, len(aClean))
		for _, item := range aClean {
			seen[item] = struct{}{}
		}
		overlap := make([]string, 0)
		added := make(map[string]struct{})
		for _, item := range bClean {
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

	// Slow path: at least one side has prefix entries, do pairwise comparison.
	overlap := make([]string, 0)
	added := make(map[string]struct{})
	for _, ai := range aClean {
		for _, bi := range bClean {
			if affectsMatch(ai, bi) {
				if _, ok := added[ai]; !ok {
					added[ai] = struct{}{}
					overlap = append(overlap, ai)
				}
				if _, ok := added[bi]; !ok {
					added[bi] = struct{}{}
					overlap = append(overlap, bi)
				}
			}
		}
	}
	sort.Strings(overlap)
	return overlap
}
