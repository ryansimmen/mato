// Package dirs defines the canonical names for task queue directories.
// Both the queue and taskfile packages import this package to avoid
// duplicating these constants and risking silent drift.
package dirs

// Queue directory names.
const (
	Waiting     = "waiting"
	Backlog     = "backlog"
	InProgress  = "in-progress"
	ReadyReview = "ready-for-review"
	ReadyMerge  = "ready-to-merge"
	Completed   = "completed"
	Failed      = "failed"
	Locks       = ".locks"
)

// All is the ordered list of all queue directories (excludes Locks).
var All = []string{
	Waiting, Backlog, InProgress,
	ReadyReview, ReadyMerge,
	Completed, Failed,
}
