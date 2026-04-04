// Package dirs defines the canonical names for task queue directories.
// Both the queue and taskfile packages import this package to avoid
// duplicating these constants and risking silent drift.
package dirs

import (
	"os"
	"path/filepath"
)

// Root is the canonical repository-local queue directory.
const Root = ".mato"

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

var active = []string{
	Waiting, Backlog, InProgress,
	ReadyReview, ReadyMerge,
}

// IsActive reports whether a task file exists in any non-terminal queue
// directory.
func IsActive(tasksDir, taskFilename string) bool {
	for _, dir := range active {
		if _, err := os.Stat(filepath.Join(tasksDir, dir, taskFilename)); err == nil {
			return true
		}
	}
	return false
}
