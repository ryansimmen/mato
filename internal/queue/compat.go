package queue

import "mato/internal/queueview"

type TaskSnapshot = queueview.TaskSnapshot
type ParseFailure = queueview.ParseFailure
type BuildWarning = queueview.BuildWarning
type PollIndex = queueview.PollIndex

const (
	DependencyAmbiguousID = queueview.DependencyAmbiguousID
	DependencyDuplicateID = queueview.DependencyDuplicateID
	DependencySelfCycle   = queueview.DependencySelfCycle
	DependencyCycle       = queueview.DependencyCycle
	DependencyUnknownID   = queueview.DependencyUnknownID
)

type DependencyIssue = queueview.DependencyIssue
type DependencyDiagnostics = queueview.DependencyDiagnostics
type TaskMatch = queueview.TaskMatch
type DependencyBlock = queueview.DependencyBlock
type RunnableBacklogView = queueview.RunnableBacklogView

func BuildIndex(tasksDir string) *PollIndex {
	return queueview.BuildIndex(tasksDir)
}

func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics {
	return queueview.DiagnoseDependencies(tasksDir, idx)
}

func DeferredOverlappingTasks(tasksDir string, idx *PollIndex) map[string]struct{} {
	return queueview.DeferredOverlappingTasks(tasksDir, idx)
}

func DependencyBlockedBacklogTasksDetailed(tasksDir string, idx *PollIndex) map[string][]DependencyBlock {
	return queueview.DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
}

func ComputeRunnableBacklogView(tasksDir string, idx *PollIndex) RunnableBacklogView {
	return queueview.ComputeRunnableBacklogView(tasksDir, idx)
}

func OrderedRunnableFilenames(view RunnableBacklogView, exclude map[string]struct{}) []string {
	return queueview.OrderedRunnableFilenames(view, exclude)
}

func FormatDependencyBlocks(blocks []DependencyBlock) string {
	return queueview.FormatDependencyBlocks(blocks)
}

func ResolveTask(idx *PollIndex, taskRef string) (TaskMatch, error) {
	return queueview.ResolveTask(idx, taskRef)
}
