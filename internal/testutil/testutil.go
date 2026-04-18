// Package testutil provides shared test helpers for setting up temporary
// git repositories used across multiple test packages.
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/git"
)

// WriteFile creates a file at path with the given content, creating parent dirs.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// WriteTempFile creates a temp file with the given content and returns its path.
func WriteTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.md")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

// MakeUnreadablePath creates a self-referential symlink at path so reads fail
// with a non-NotExist error on platforms that support symlinks.
func MakeUnreadablePath(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove existing path %s: %v", path, err)
	}
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		t.Skipf("create unreadable path %s: %v", path, err)
	}
}

// SetupRepo creates a temporary git repository with one initial commit
// containing a README.md file. It returns the repo root directory.
func SetupRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if _, err := git.Output(dir, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := git.Output(dir, "config", "user.email", "test@test.com"); err != nil {
		t.Fatalf("git config user.email: %v", err)
	}
	if _, err := git.Output(dir, "config", "user.name", "Test"); err != nil {
		t.Fatalf("git config user.name: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md: %v", err)
	}
	if _, err := git.Output(dir, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(dir, "commit", "-m", "initial"); err != nil {
		t.Fatalf("git commit initial: %v", err)
	}
	return dir
}

// SetupRepoWithTasks creates a temporary git repository like SetupRepo but
// additionally checks out a "mato" branch, creates the standard .mato
// subdirectory tree, initialises messaging, and sets
// receive.denyCurrentBranch=updateInstead. It returns (repoRoot, tasksDir).
func SetupRepoWithTasks(t *testing.T) (string, string) {
	t.Helper()

	dir := SetupRepo(t)
	if _, err := git.Output(dir, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}

	tasksDir := filepath.Join(dir, ".mato")
	for _, sub := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, dirs.Locks), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%s): %v", dirs.Locks, err)
	}
	// Initialise messaging directories inline to avoid an import cycle
	// (messaging → taskfile, and taskfile tests import testutil).
	for _, sub := range []string{"messages/events", "messages/presence", "messages/completions"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if _, err := git.Output(dir, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	return dir, tasksDir
}
