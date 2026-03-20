// Package testutil provides shared test helpers for setting up temporary
// git repositories used across multiple test packages.
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"mato/internal/git"
	"mato/internal/messaging"
)

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
// additionally checks out a "mato" branch, creates the standard .tasks
// subdirectory tree, initialises messaging, and sets
// receive.denyCurrentBranch=updateInstead. It returns (repoRoot, tasksDir).
func SetupRepoWithTasks(t *testing.T) (string, string) {
	t.Helper()

	dir := SetupRepo(t)
	if _, err := git.Output(dir, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}

	tasksDir := filepath.Join(dir, ".tasks")
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-for-review", "ready-to-merge", "completed", "failed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(.locks): %v", err)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}
	if _, err := git.Output(dir, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	return dir, tasksDir
}
