package queue

// Queue directory name constants. These replace hardcoded strings like
// "backlog", "in-progress", etc. throughout the codebase.
const (
	DirWaiting     = "waiting"
	DirBacklog     = "backlog"
	DirInProgress  = "in-progress"
	DirReadyReview = "ready-for-review"
	DirReadyMerge  = "ready-to-merge"
	DirCompleted   = "completed"
	DirFailed      = "failed"
)

// AllDirs is the ordered list of all queue directories.
var AllDirs = []string{
	DirWaiting, DirBacklog, DirInProgress,
	DirReadyReview, DirReadyMerge,
	DirCompleted, DirFailed,
}
