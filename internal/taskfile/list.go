package taskfile

import (
	"os"
	"strings"
)

// ListTaskFiles returns the names of .md files in dir, sorted by name.
// Directories and non-.md files are skipped. The returned names are
// base filenames (e.g. "add-hello.md"), not full paths.
func ListTaskFiles(dir string) ([]string, error) {
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
