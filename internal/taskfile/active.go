package taskfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
)

// activeDirs lists the directories that contain tasks currently being
// worked on, under review, or awaiting merge.
var activeDirs = []string{dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge}

// ActiveTask describes a task currently being worked on or awaiting merge,
// along with the files it declares in its affects: metadata.
type ActiveTask struct {
	Name    string
	Dir     string // "in-progress", "ready-for-review", or "ready-to-merge"
	Affects []string
}

// CollectWarning records a non-fatal problem encountered while scanning
// active task directories. Each warning captures enough context for
// operators to understand why file-claims.json may be incomplete.
type CollectWarning struct {
	Dir  string // queue directory name (e.g. "in-progress")
	File string // task filename, empty for directory-level errors
	Err  error
}

func (w CollectWarning) Error() string {
	if w.File != "" {
		return fmt.Sprintf("%s/%s: %v", w.Dir, w.File, w.Err)
	}
	return fmt.Sprintf("%s: %v", w.Dir, w.Err)
}

// CollectActiveAffects returns tasks in in-progress/, ready-for-review/, and
// ready-to-merge/ that have non-empty affects: metadata. Warnings are
// returned for unexpected directory read errors (non-ENOENT) and per-file
// read/parse failures so callers can surface partial-indexing problems.
func CollectActiveAffects(tasksDir string) ([]ActiveTask, []CollectWarning) {
	var active []ActiveTask
	var warnings []CollectWarning
	for _, dir := range activeDirs {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := ListTaskFiles(dirPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			warnings = append(warnings, CollectWarning{Dir: dir, Err: fmt.Errorf("read directory: %w", err)})
			continue
		}
		for _, name := range names {
			path := filepath.Join(dirPath, name)
			meta, _, err := frontmatter.ParseTaskFile(path)
			if err != nil {
				warnings = append(warnings, CollectWarning{Dir: dir, File: name, Err: fmt.Errorf("parse task file: %w", err)})
				continue
			}
			if len(meta.Affects) == 0 {
				continue
			}
			active = append(active, ActiveTask{
				Name:    name,
				Dir:     dir,
				Affects: meta.Affects,
			})
		}
	}
	return active, warnings
}
