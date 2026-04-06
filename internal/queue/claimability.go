package queue

import (
	"time"

	"mato/internal/dirs"
	"mato/internal/queueview"
)

func HasClaimableBacklogTask(tasksDir string, exclude map[string]struct{}, cooldown time.Duration, idx *PollIndex) bool {
	idx = ensureIndex(tasksDir, idx)
	view := ComputeRunnableBacklogView(tasksDir, idx)
	for _, name := range OrderedRunnableFilenames(view, exclude) {
		if immediatelyClaimableTask(idx, idx.Snapshot(dirs.Backlog, name), cooldown) {
			return true
		}
	}
	return false
}

func dependencyBlocksFor(idx *PollIndex, dependsOn []string) []DependencyBlock {
	return queueview.DependencyBlocksFor(idx, dependsOn)
}
