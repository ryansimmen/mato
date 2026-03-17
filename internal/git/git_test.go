package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initBareAndClone creates a bare repo, clones it, adds an initial commit,
// and pushes it so the clone has a valid upstream.
func initBareAndClone(t *testing.T) (bare, clone string) {
	t.Helper()
	bare = t.TempDir()
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	clone = t.TempDir()
	cmd = exec.Command("git", "clone", bare, clone)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	// Configure user for commits.
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@test.com"}} {
		cmd = exec.Command("git", "-C", clone, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}

	// Initial commit so HEAD exists.
	readme := filepath.Join(clone, "README.md")
	if err := os.WriteFile(readme, []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", clone, "add", "README.md")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", clone, "commit", "-m", "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", clone, "push")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}
	return bare, clone
}

func TestEnsureGitignored_DoesNotCommitUnrelatedStagedFiles(t *testing.T) {
	_, repo := initBareAndClone(t)

	// Stage an unrelated file before calling EnsureGitignored.
	unrelated := filepath.Join(repo, "unrelated.txt")
	if err := os.WriteFile(unrelated, []byte("should not be committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "add", "unrelated.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add unrelated.txt: %v\n%s", err, out)
	}

	// EnsureGitignored should only commit .gitignore changes.
	if err := EnsureGitignored(repo, "/.tasks/"); err != nil {
		t.Fatalf("EnsureGitignored: %v", err)
	}

	// Verify .gitignore was committed.
	out, err := Output(repo, "log", "--oneline", "--name-only", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(out, ".gitignore") {
		t.Errorf("expected .gitignore in commit, got: %s", out)
	}
	if strings.Contains(out, "unrelated.txt") {
		t.Errorf("unrelated.txt should NOT be in the gitignore commit, got: %s", out)
	}

	// Verify unrelated.txt is still staged (not committed).
	status, err := Output(repo, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(status, "unrelated.txt") {
		t.Errorf("expected unrelated.txt to still be staged, got status: %s", status)
	}
}

func TestEnsureGitignored_Idempotent(t *testing.T) {
	_, repo := initBareAndClone(t)

	if err := EnsureGitignored(repo, "/.tasks/"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call should be a no-op.
	if err := EnsureGitignored(repo, "/.tasks/"); err != nil {
		t.Fatalf("second call should be no-op: %v", err)
	}

	// Only one gitignore commit should exist.
	out, err := Output(repo, "log", "--oneline")
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(out, ".gitignore")
	if count != 1 {
		t.Errorf("expected exactly 1 gitignore commit, got %d in:\n%s", count, out)
	}
}
