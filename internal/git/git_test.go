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

func TestEnsureBranch_PrefersRemoteTrackingBranch(t *testing.T) {
	bare, clone := initBareAndClone(t)

	// Create the target branch on the remote via the clone, then remove it locally.
	cmd := exec.Command("git", "-C", clone, "checkout", "-b", "mato")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout -b mato: %v\n%s", err, out)
	}
	// Add a commit so mato diverges from main.
	matoFile := filepath.Join(clone, "mato.txt")
	if err := os.WriteFile(matoFile, []byte("mato content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", clone, "add", "mato.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", clone, "commit", "-m", "mato commit")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", clone, "push", "origin", "mato")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push mato: %v\n%s", err, out)
	}

	// Now make a second clone that has origin/mato but no local mato branch,
	// and whose HEAD has diverged.
	clone2 := t.TempDir()
	cmd = exec.Command("git", "clone", bare, clone2)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second clone: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@test.com"}} {
		cmd = exec.Command("git", "-C", clone2, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}

	// Add a diverging commit on the default branch (main/master).
	diverge := filepath.Join(clone2, "diverge.txt")
	if err := os.WriteFile(diverge, []byte("diverged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", clone2, "add", "diverge.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add diverge: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", clone2, "commit", "-m", "diverging commit")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit diverge: %v\n%s", err, out)
	}

	// Record HEAD before EnsureBranch.
	headBefore, err := Output(clone2, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	headBefore = strings.TrimSpace(headBefore)

	// Record origin/mato SHA.
	remoteMato, err := Output(clone2, "rev-parse", "origin/mato")
	if err != nil {
		t.Fatalf("rev-parse origin/mato: %v", err)
	}
	remoteMato = strings.TrimSpace(remoteMato)

	// Ensure HEAD and origin/mato are different (diverged).
	if headBefore == remoteMato {
		t.Fatal("test setup error: HEAD should differ from origin/mato")
	}

	// EnsureBranch should create local mato from origin/mato, not HEAD.
	if err := EnsureBranch(clone2, "mato"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Verify we're on the mato branch.
	branch, err := Output(clone2, "branch", "--show-current")
	if err != nil {
		t.Fatalf("branch --show-current: %v", err)
	}
	if strings.TrimSpace(branch) != "mato" {
		t.Errorf("expected to be on branch mato, got %q", strings.TrimSpace(branch))
	}

	// Verify HEAD matches origin/mato, not the old diverged HEAD.
	headAfter, err := Output(clone2, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	headAfter = strings.TrimSpace(headAfter)
	if headAfter != remoteMato {
		t.Errorf("expected HEAD to match origin/mato (%s), got %s", remoteMato, headAfter)
	}
	if headAfter == headBefore {
		t.Errorf("HEAD should NOT match the diverged HEAD (%s)", headBefore)
	}
}

func TestEnsureBranch_FallsBackToHEAD(t *testing.T) {
	_, clone := initBareAndClone(t)

	// No local or remote "newbranch" exists; should create from HEAD.
	headBefore, err := Output(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	headBefore = strings.TrimSpace(headBefore)

	if err := EnsureBranch(clone, "newbranch"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	branch, err := Output(clone, "branch", "--show-current")
	if err != nil {
		t.Fatalf("branch --show-current: %v", err)
	}
	if strings.TrimSpace(branch) != "newbranch" {
		t.Errorf("expected to be on branch newbranch, got %q", strings.TrimSpace(branch))
	}

	headAfter, err := Output(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if strings.TrimSpace(headAfter) != headBefore {
		t.Errorf("expected HEAD (%s) to match original HEAD (%s)", strings.TrimSpace(headAfter), headBefore)
	}
}

func TestEnsureBranch_LocalBranchExists(t *testing.T) {
	_, clone := initBareAndClone(t)

	// Create a local branch first.
	cmd := exec.Command("git", "-C", clone, "branch", "existing")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git branch existing: %v\n%s", err, out)
	}

	if err := EnsureBranch(clone, "existing"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	branch, err := Output(clone, "branch", "--show-current")
	if err != nil {
		t.Fatalf("branch --show-current: %v", err)
	}
	if strings.TrimSpace(branch) != "existing" {
		t.Errorf("expected to be on branch existing, got %q", strings.TrimSpace(branch))
	}
}

func TestEnsureBranch_FetchesRemoteBranchCreatedAfterClone(t *testing.T) {
	bare, clone := initBareAndClone(t)

	// Create a second clone that will push a new branch to origin.
	pusher := t.TempDir()
	cmd := exec.Command("git", "clone", bare, pusher)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone pusher: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@test.com"}} {
		cmd = exec.Command("git", "-C", pusher, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}

	// Create a branch "latebranch" and push it from the pusher clone.
	cmd = exec.Command("git", "-C", pusher, "checkout", "-b", "latebranch")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout -b latebranch: %v\n%s", err, out)
	}
	lateFile := filepath.Join(pusher, "late.txt")
	if err := os.WriteFile(lateFile, []byte("late content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", pusher, "add", "late.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", pusher, "commit", "-m", "late commit")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", pusher, "push", "origin", "latebranch")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git push latebranch: %v\n%s", err, out)
	}

	// Record the SHA that the pusher committed.
	pusherSHA, err := Output(pusher, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD in pusher: %v", err)
	}
	pusherSHA = strings.TrimSpace(pusherSHA)

	// The original clone has NO remote-tracking ref for latebranch yet.
	if _, err := Output(clone, "rev-parse", "--verify", "refs/remotes/origin/latebranch"); err == nil {
		t.Fatal("test setup error: clone should NOT have origin/latebranch yet")
	}

	// EnsureBranch should fetch and create the local branch from origin.
	if err := EnsureBranch(clone, "latebranch"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	// Verify we're on the latebranch.
	branch, err := Output(clone, "branch", "--show-current")
	if err != nil {
		t.Fatalf("branch --show-current: %v", err)
	}
	if strings.TrimSpace(branch) != "latebranch" {
		t.Errorf("expected to be on branch latebranch, got %q", strings.TrimSpace(branch))
	}

	// Verify HEAD matches the commit pushed by the other clone.
	headAfter, err := Output(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if strings.TrimSpace(headAfter) != pusherSHA {
		t.Errorf("expected HEAD to match pusher SHA (%s), got %s", pusherSHA, strings.TrimSpace(headAfter))
	}
}

func TestEnsureBranch_FetchFailsFallsBackToHEAD(t *testing.T) {
	_, clone := initBareAndClone(t)

	// Remove the remote so fetch will fail.
	cmd := exec.Command("git", "-C", clone, "remote", "remove", "origin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remote remove origin: %v\n%s", err, out)
	}

	headBefore, err := Output(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	headBefore = strings.TrimSpace(headBefore)

	// EnsureBranch should still work, falling back to HEAD.
	if err := EnsureBranch(clone, "orphan"); err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}

	branch, err := Output(clone, "branch", "--show-current")
	if err != nil {
		t.Fatalf("branch --show-current: %v", err)
	}
	if strings.TrimSpace(branch) != "orphan" {
		t.Errorf("expected to be on branch orphan, got %q", strings.TrimSpace(branch))
	}

	headAfter, err := Output(clone, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if strings.TrimSpace(headAfter) != headBefore {
		t.Errorf("expected HEAD (%s) to match original HEAD (%s)", strings.TrimSpace(headAfter), headBefore)
	}
}

func TestEnsureGitignoreContains_CreatesNewGitignore(t *testing.T) {
	dir := t.TempDir()

	changed, err := EnsureGitignoreContains(dir, "/.tasks/")
	if err != nil {
		t.Fatalf("EnsureGitignoreContains: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when creating new .gitignore")
	}

	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "/.tasks/") {
		t.Errorf("expected .gitignore to contain /.tasks/, got: %s", data)
	}
	// Verify file ends with newline.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		t.Error("expected .gitignore to end with newline")
	}
}

func TestEnsureGitignoreContains_AppendsToFileWithoutTrailingNewline(t *testing.T) {
	dir := t.TempDir()

	// Create a .gitignore without a trailing newline.
	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("*.log"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureGitignoreContains(dir, "/.tasks/")
	if err != nil {
		t.Fatalf("EnsureGitignoreContains: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}

	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d: %s", len(lines), data)
	}
	if lines[0] != "*.log" {
		t.Errorf("expected first line '*.log', got %q", lines[0])
	}
	if lines[1] != "/.tasks/" {
		t.Errorf("expected second line '/.tasks/', got %q", lines[1])
	}
}

func TestEnsureGitignoreContains_AtomicWritePreservesPermissions(t *testing.T) {
	dir := t.TempDir()

	changed, err := EnsureGitignoreContains(dir, "/.tasks/")
	if err != nil {
		t.Fatalf("EnsureGitignoreContains: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}

	gitignorePath := filepath.Join(dir, ".gitignore")
	info, err := os.Stat(gitignorePath)
	if err != nil {
		t.Fatalf("stat .gitignore: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o644 {
		t.Errorf("expected .gitignore permissions 0644, got %o", perm)
	}
}

func TestEnsureGitignoreContains_Idempotent(t *testing.T) {
	dir := t.TempDir()

	changed, err := EnsureGitignoreContains(dir, "/.tasks/")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !changed {
		t.Error("expected changed=true on first call")
	}

	// Second call should be a no-op.
	changed, err = EnsureGitignoreContains(dir, "/.tasks/")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if changed {
		t.Error("expected changed=false on second call (pattern already present)")
	}

	// Verify content has exactly one occurrence of the pattern.
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(data), "/.tasks/")
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of /.tasks/, got %d in: %s", count, data)
	}
}

func TestEnsureGitignoreContains_UnreadableGitignoreReturnsError(t *testing.T) {
	dir := t.TempDir()

	// Create a .gitignore that is not readable.
	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitignorePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(gitignorePath, 0o644)
	})

	_, err := EnsureGitignoreContains(dir, "/.tasks/")
	if err == nil {
		t.Fatal("expected error for unreadable .gitignore, got nil")
	}
	if !strings.Contains(err.Error(), "read .gitignore") {
		t.Errorf("expected 'read .gitignore' in error, got: %v", err)
	}
}

func TestEnsureGitignoreContains_ReturnsFalseWhenAlreadyPresent(t *testing.T) {
	dir := t.TempDir()

	gitignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("/.tasks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureGitignoreContains(dir, "/.tasks/")
	if err != nil {
		t.Fatalf("EnsureGitignoreContains: %v", err)
	}
	if changed {
		t.Error("expected changed=false when pattern is already present")
	}
}

func TestCommitGitignore(t *testing.T) {
	_, repo := initBareAndClone(t)

	// Create a .gitignore to commit.
	gitignorePath := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("/.tasks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CommitGitignore(repo, "chore: add /.tasks/ to .gitignore"); err != nil {
		t.Fatalf("CommitGitignore: %v", err)
	}

	// Verify .gitignore was committed.
	out, err := Output(repo, "log", "--oneline", "--name-only", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(out, ".gitignore") {
		t.Errorf("expected .gitignore in commit, got: %s", out)
	}
	if !strings.Contains(out, "chore: add /.tasks/ to .gitignore") {
		t.Errorf("expected commit message, got: %s", out)
	}
}

func TestCommitGitignore_DoesNotCommitUnrelatedStagedFiles(t *testing.T) {
	_, repo := initBareAndClone(t)

	// Stage an unrelated file.
	unrelated := filepath.Join(repo, "unrelated.txt")
	if err := os.WriteFile(unrelated, []byte("should not be committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", repo, "add", "unrelated.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add unrelated.txt: %v\n%s", err, out)
	}

	// Create .gitignore and commit only it.
	gitignorePath := filepath.Join(repo, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("/.tasks/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CommitGitignore(repo, "chore: gitignore"); err != nil {
		t.Fatalf("CommitGitignore: %v", err)
	}

	// Verify only .gitignore was committed.
	out, err := Output(repo, "log", "--oneline", "--name-only", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.Contains(out, "unrelated.txt") {
		t.Errorf("unrelated.txt should NOT be in the gitignore commit, got: %s", out)
	}

	// Verify unrelated.txt is still staged.
	status, err := Output(repo, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(status, "unrelated.txt") {
		t.Errorf("expected unrelated.txt to still be staged, got status: %s", status)
	}
}
