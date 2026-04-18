package queue

import (
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/queueview"
)

func HasClaimableBacklogTask(tasksDir string, exclude map[string]struct{}, cooldown time.Duration, idx *PollIndex) bool {
	idx = ensureIndex(tasksDir, idx)
	view := queueview.ComputeRunnableBacklogView(tasksDir, idx)
	for _, name := range queueview.OrderedRunnableFilenames(view, exclude) {
		if immediatelyClaimableTask(idx, idx.Snapshot(dirs.Backlog, name), cooldown) {
			return true
		}
	}
	return false
}

func dependencyBlocksFor(idx *PollIndex, dependsOn []string) []DependencyBlock {
	return queueview.DependencyBlocksFor(idx, dependsOn)
}
