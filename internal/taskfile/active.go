package taskfile

import (
	"os"
	"path/filepath"
	"strings"

	"mato/internal/frontmatter"
)

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
	for _, dir := range []string{"in-progress", "ready-for-review", "ready-to-merge"} {
		dirPath := filepath.Join(tasksDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path := filepath.Join(dirPath, e.Name())
			meta, _, err := frontmatter.ParseTaskFile(path)
			if err != nil {
				continue
			}
			if len(meta.Affects) == 0 {
				continue
			}
			active = append(active, ActiveTask{
				Name:    e.Name(),
				Dir:     dir,
				Affects: meta.Affects,
			})
		}
	}
	return active
}
