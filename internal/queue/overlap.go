package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mato/internal/frontmatter"
	"mato/internal/taskfile"
)

type backlogTask struct {
	name     string
	dir      string // source directory (e.g., "backlog", "in-progress", "ready-to-merge")
	path     string
	priority int
	affects  []string
}

func hasActiveOverlap(tasksDir string, affects []string) bool {
	if len(affects) == 0 {
		return false
	}
	// Only check in-progress, ready-for-review, and ready-to-merge — these represent
	// tasks that are actively being worked on, under review, or awaiting merge.
	// We intentionally exclude backlog/
	// because DeferredOverlappingTasks handles backlog-vs-backlog conflicts with
	// proper priority ordering. Including backlog here would cause priority
	// inversion: a high-priority waiting task would be blocked by a lower-priority
	// backlog task that hasn't even been claimed yet.
	for _, dir := range []string{DirInProgress, DirReadyReview, DirReadyMerge} {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := ListTaskFiles(dirPath)
		if err != nil {
			continue
		}
		for _, name := range names {
			meta, _, err := frontmatter.ParseTaskFile(filepath.Join(dirPath, name))
			if err != nil {
				continue
			}
			if len(overlappingAffects(affects, meta.Affects)) > 0 {
				return true
			}
		}
	}
	return false
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
func DeferredOverlappingTasks(tasksDir string) map[string]struct{} {
	detailed := DeferredOverlappingTasksDetailed(tasksDir)
	simple := make(map[string]struct{}, len(detailed))
	for name := range detailed {
		simple[name] = struct{}{}
	}
	return simple
}

// DeferredOverlappingTasksDetailed returns deferred tasks with the reason for deferral.
func DeferredOverlappingTasksDetailed(tasksDir string) map[string]DeferralInfo {
	deferred := make(map[string]DeferralInfo)
	backlogDir := filepath.Join(tasksDir, DirBacklog)
	names, err := ListTaskFiles(backlogDir)
	if err != nil {
		return deferred
	}

	tasks := make([]backlogTask, 0, len(names))
	for _, name := range names {
		path := filepath.Join(backlogDir, name)
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for overlap detection: %v\n", name, err)
			continue
		}
		tasks = append(tasks, backlogTask{
			name:     name,
			path:     path,
			priority: meta.Priority,
			affects:  meta.Affects,
		})
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})

	active := taskfile.CollectActiveAffects(tasksDir)
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
