package queue

import "github.com/ryansimmen/mato/internal/queueview"

// TaskSnapshot aliases queueview.TaskSnapshot for queue compatibility.
type TaskSnapshot = queueview.TaskSnapshot

// ParseFailure aliases queueview.ParseFailure for queue compatibility.
type ParseFailure = queueview.ParseFailure

// BuildWarning aliases queueview.BuildWarning for queue compatibility.
type BuildWarning = queueview.BuildWarning

// PollIndex aliases queueview.PollIndex for queue compatibility.
type PollIndex = queueview.PollIndex

const (
	// DependencyAmbiguousID forwards queueview.DependencyAmbiguousID.
	DependencyAmbiguousID = queueview.DependencyAmbiguousID

	// DependencyDuplicateID forwards queueview.DependencyDuplicateID.
	DependencyDuplicateID = queueview.DependencyDuplicateID

	// DependencySelfCycle forwards queueview.DependencySelfCycle.
	DependencySelfCycle = queueview.DependencySelfCycle

	// DependencyCycle forwards queueview.DependencyCycle.
	DependencyCycle = queueview.DependencyCycle

	// DependencyUnknownID forwards queueview.DependencyUnknownID.
	DependencyUnknownID = queueview.DependencyUnknownID
)

// DependencyIssue aliases queueview.DependencyIssue for queue compatibility.
type DependencyIssue = queueview.DependencyIssue

// DependencyDiagnostics aliases queueview.DependencyDiagnostics for queue compatibility.
type DependencyDiagnostics = queueview.DependencyDiagnostics

// TaskMatch aliases queueview.TaskMatch for queue compatibility.
type TaskMatch = queueview.TaskMatch

// DependencyBlock aliases queueview.DependencyBlock for queue compatibility.
type DependencyBlock = queueview.DependencyBlock

// RunnableBacklogView aliases queueview.RunnableBacklogView for queue compatibility.
type RunnableBacklogView = queueview.RunnableBacklogView

// BuildIndex forwards to queueview.BuildIndex.
func BuildIndex(tasksDir string) *PollIndex {
	return queueview.BuildIndex(tasksDir)
}

// DiagnoseDependencies forwards to queueview.DiagnoseDependencies.
func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics {
	return queueview.DiagnoseDependencies(tasksDir, idx)
}

// DeferredOverlappingTasks forwards to queueview.DeferredOverlappingTasks.
func DeferredOverlappingTasks(tasksDir string, idx *PollIndex) map[string]struct{} {
	return queueview.DeferredOverlappingTasks(tasksDir, idx)
}

// DependencyBlockedBacklogTasksDetailed forwards to queueview.DependencyBlockedBacklogTasksDetailed.
func DependencyBlockedBacklogTasksDetailed(tasksDir string, idx *PollIndex) map[string][]DependencyBlock {
	return queueview.DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
}

// ComputeRunnableBacklogView forwards to queueview.ComputeRunnableBacklogView.
func ComputeRunnableBacklogView(tasksDir string, idx *PollIndex) RunnableBacklogView {
	return queueview.ComputeRunnableBacklogView(tasksDir, idx)
}

// OrderedRunnableFilenames forwards to queueview.OrderedRunnableFilenames.
func OrderedRunnableFilenames(view RunnableBacklogView, exclude map[string]struct{}) []string {
	return queueview.OrderedRunnableFilenames(view, exclude)
}

// FormatDependencyBlocks forwards to queueview.FormatDependencyBlocks.
func FormatDependencyBlocks(blocks []DependencyBlock) string {
	return queueview.FormatDependencyBlocks(blocks)
}

// ResolveTask forwards to queueview.ResolveTask.
func ResolveTask(idx *PollIndex, taskRef string) (TaskMatch, error) {
	return queueview.ResolveTask(idx, taskRef)
}

// CompletedDependencyTaskIDs forwards to queueview.CompletedDependencyTaskIDs.
func CompletedDependencyTaskIDs(idx *PollIndex, depRef string) []string {
	return queueview.CompletedDependencyTaskIDs(idx, depRef)
}
