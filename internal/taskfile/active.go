package taskfile

import (
	"os"
	"path/filepath"
	"strings"

	"mato/internal/frontmatter"
)

// Active directory names — duplicated from queue.Dir* to avoid a circular
// import between taskfile and queue.
const (
	dirInProgress  = "in-progress"
	dirReadyReview = "ready-for-review"
	dirReadyMerge  = "ready-to-merge"
)

// activeDirs lists the directories that contain tasks currently being
// worked on, under review, or awaiting merge.
var activeDirs = []string{dirInProgress, dirReadyReview, dirReadyMerge}

// ActiveTask describes a task currently being worked on or awaiting merge,
// along with the files it declares in its affects: metadata.
type ActiveTask struct {
	Name    string
	Dir     string // "in-progress", "ready-for-review", or "ready-to-merge"
	Affects []string
}

// CollectActiveAffects returns tasks in in-progress/, ready-for-review/, and
// ready-to-merge/ that have non-empty affects: metadata.
func CollectActiveAffects(tasksDir string) []ActiveTask {
	var active []ActiveTask
	for _, dir := range activeDirs {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := listTaskFiles(dirPath)
		if err != nil {
			continue
		}
		for _, name := range names {
			path := filepath.Join(dirPath, name)
			meta, _, err := frontmatter.ParseTaskFile(path)
			if err != nil {
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
	return active
}

// listTaskFiles returns the names of .md files in dir, sorted by name.
func listTaskFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}
