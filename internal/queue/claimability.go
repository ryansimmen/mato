package queue

import (
	"time"

	"mato/internal/dirs"
)

func HasClaimableBacklogTask(tasksDir string, exclude map[string]struct{}, cooldown time.Duration, idx *PollIndex) bool {
	idx = ensureIndex(tasksDir, idx)
	view := ComputeRunnableBacklogView(tasksDir, idx)
	depLookup := newDependencyLookup(idx)
	for _, name := range OrderedRunnableFilenames(view, exclude) {
		if immediatelyClaimableTask(idx.Snapshot(dirs.Backlog, name), depLookup, cooldown) {
			return true
		}
	}
	return false
}
