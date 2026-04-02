package queue

import "mato/internal/dirs"

// Queue directory name constants. Re-exported from the shared dirs package
// so that existing callers using queue.Dir* continue to work.
const (
	DirWaiting     = dirs.Waiting
	DirBacklog     = dirs.Backlog
	DirInProgress  = dirs.InProgress
	DirReadyReview = dirs.ReadyReview
	DirReadyMerge  = dirs.ReadyMerge
	DirCompleted   = dirs.Completed
	DirFailed      = dirs.Failed
	DirLocks       = dirs.Locks
)

// AllDirs is the ordered list of all queue directories.
var AllDirs = []string{
	DirWaiting, DirBacklog, DirInProgress,
	DirReadyReview, DirReadyMerge,
	DirCompleted, DirFailed,
}
