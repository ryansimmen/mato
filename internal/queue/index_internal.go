package queue

import "mato/internal/queueview"

func ensureIndex(tasksDir string, idx *PollIndex) *PollIndex {
	if idx != nil {
		return idx
	}
	return queueview.BuildIndex(tasksDir)
}

func loadTaskSnapshot(tasksDir, state, filename, path string) (*TaskSnapshot, error) {
	return queueview.LoadTaskSnapshot(tasksDir, state, filename, path)
}
