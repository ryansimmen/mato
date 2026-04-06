// Package dirs defines the canonical names for task queue directories.
// Both the queue and taskfile packages import this package to avoid
// duplicating these constants and risking silent drift.
package dirs

import (
	"errors"
	"fmt"
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
// directory. It returns an error when queue liveness cannot be determined
// because one or more active directories could not be read.
func IsActive(tasksDir, taskFilename string) (bool, error) {
	var errs []error
	for _, dir := range active {
		dirPath := filepath.Join(tasksDir, dir)
		info, err := os.Stat(dirPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("stat %s: %w", dirPath, err))
			continue
		}
		if !info.IsDir() {
			errs = append(errs, fmt.Errorf("stat %s: not a directory", dirPath))
			continue
		}
		path := filepath.Join(dirPath, taskFilename)
		_, err = os.Stat(path)
		if err == nil {
			return true, nil
		}
		if os.IsNotExist(err) {
			continue
		}
		errs = append(errs, fmt.Errorf("stat %s: %w", path, err))
	}
	if len(errs) > 0 {
		return false, errors.Join(errs...)
	}
	return false, nil
}
