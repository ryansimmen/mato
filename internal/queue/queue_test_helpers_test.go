package queue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
)

// setupIndexDirs creates the standard .mato directory tree in a temp dir
// and returns the tasksDir path.
func setupIndexDirs(t *testing.T) string {
	t.Helper()
	tasksDir := filepath.Join(t.TempDir(), ".mato")
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", dir, err)
		}
	}
	return tasksDir
}

// writeTask is a helper to write a task file in the given state directory.
func writeTask(t *testing.T, tasksDir, state, filename, content string) {
	t.Helper()
	path := filepath.Join(tasksDir, state, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}
