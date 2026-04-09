package taskfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ListTaskFiles returns the names of .md files in dir, sorted by name.
// Directories, symlinks, and other non-regular .md entries are skipped. The
// returned names are
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
		if err := CheckRegularTaskFile(filepath.Join(dir, e.Name())); err != nil {
			if errors.Is(err, ErrTaskFileNotRegular) {
				continue
			}
			return nil, err
		}
		names = append(names, e.Name())
	}
	return names, nil
}
