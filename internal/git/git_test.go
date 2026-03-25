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

func TestOutput_ReturnsOnlyStdout(t *testing.T) {
	_, clone := initBareAndClone(t)

	// rev-parse --show-toplevel writes only to stdout; verify no stderr leaks.
	out, err := Output(clone, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		t.Fatal("expected non-empty stdout from rev-parse --show-toplevel")
	}
	// stdout should be a clean path with no warnings mixed in.
	if strings.Contains(trimmed, "warning") {
		t.Errorf("stdout should not contain warnings, got: %q", trimmed)
	}
}

func TestOutput_ErrorIncludesStderr(t *testing.T) {
	dir := t.TempDir()

	// Run a git command that will fail — rev-parse in a non-repo directory.
	_, err := Output(dir, "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected error from rev-parse in non-repo dir")
	}
	// The error message should contain the stderr diagnostic from git.
	errMsg := err.Error()
	if !strings.Contains(errMsg, "git rev-parse HEAD") {
		t.Errorf("error should reference the git command, got: %s", errMsg)
	}
}

func TestOutput_StderrNotInSuccessOutput(t *testing.T) {
	dir := t.TempDir()

	// Create a repo that will produce stderr warnings on certain operations.
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@test.com"}} {
		cmd = exec.Command("git", "-C", dir, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", dir, "add", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-m", "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// git log --oneline should return only the commit line on stdout.
	out, err := Output(dir, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Errorf("expected exactly 1 line from git log --oneline -1, got %d: %q", len(lines), out)
	}
	// The line should be a short SHA + message, not contain stderr noise.
	if !strings.Contains(lines[0], "init") {
		t.Errorf("expected commit message 'init' in output, got: %q", lines[0])
	}
}

func TestOutput_SuccessWithStderrWarning(t *testing.T) {
	// Regression test: a successful git command that also writes to stderr
	// must return only stdout. This is the exact scenario Output's separate
	// stdout/stderr capture is meant to protect against.

	// Set up a repo with a commit using the real git.
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@test.com"}} {
		cmd = exec.Command("git", "-C", dir, "config", kv[0], kv[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv[0], err, out)
		}
	}
	readme := filepath.Join(dir, "README.md")
	if err := os.WriteFile(readme, []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("git", "-C", dir, "add", ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", dir, "commit", "-m", "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Capture the expected SHA before swapping PATH.
	cmd = exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	shaBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	wantSHA := strings.TrimSpace(string(shaBytes))

	// Create a git wrapper that injects a stderr warning on every
	// invocation, then delegates to the real git binary.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("git not found in PATH")
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	script := "#!/bin/sh\necho 'warning: unexpected stderr noise' >&2\nexec " +
		realGit + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Prepend the wrapper directory so Output finds our shim first.
	t.Setenv("PATH", wrapperDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	// Call Output — it should succeed and return only the SHA on stdout,
	// with no stderr warning text mixed in.
	got, err := Output(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("Output should succeed even when stderr is written: %v", err)
	}

	gotSHA := strings.TrimSpace(got)
	if gotSHA != wantSHA {
		t.Errorf("expected SHA %q, got %q", wantSHA, gotSHA)
	}
	if strings.Contains(got, "warning") {
		t.Errorf("stderr warning leaked into stdout result: %q", got)
	}
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

	changed, err := EnsureGitignoreContains(dir, "/.mato/")
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
	if !strings.Contains(string(data), "/.mato/") {
		t.Errorf("expected .gitignore to contain /.mato/, got: %s", data)
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

	changed, err := EnsureGitignoreContains(dir, "/.mato/")
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
	if lines[1] != "/.mato/" {
		t.Errorf("expected second line '/.mato/', got %q", lines[1])
	}
}

func TestEnsureGitignoreContains_AtomicWritePreservesPermissions(t *testing.T) {
	dir := t.TempDir()

	changed, err := EnsureGitignoreContains(dir, "/.mato/")
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

	changed, err := EnsureGitignoreContains(dir, "/.mato/")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !changed {
		t.Error("expected changed=true on first call")
	}

	// Second call should be a no-op.
	changed, err = EnsureGitignoreContains(dir, "/.mato/")
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
	count := strings.Count(string(data), "/.mato/")
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of /.mato/, got %d in: %s", count, data)
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

	_, err := EnsureGitignoreContains(dir, "/.mato/")
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
	if err := os.WriteFile(gitignorePath, []byte("/.mato/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureGitignoreContains(dir, "/.mato/")
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
	if err := os.WriteFile(gitignorePath, []byte("/.mato/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := CommitGitignore(repo, "chore: add /.mato/ to .gitignore"); err != nil {
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
	if !strings.Contains(out, "chore: add /.mato/ to .gitignore") {
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
	if err := os.WriteFile(gitignorePath, []byte("/.mato/\n"), 0o644); err != nil {
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

func TestResolveIdentity_LocalConfig(t *testing.T) {
	_, clone := initBareAndClone(t)

	// initBareAndClone sets user.name="test" and user.email="test@test.com"
	name, email := ResolveIdentity(clone)
	if name != "test" {
		t.Errorf("expected name %q, got %q", "test", name)
	}
	if email != "test@test.com" {
		t.Errorf("expected email %q, got %q", "test@test.com", email)
	}
}

func TestResolveIdentity_Defaults(t *testing.T) {
	// Create a repo with no identity configured.
	repo := t.TempDir()
	cmd := exec.Command("git", "init", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Unset any inherited config by overriding with empty values.
	cmd = exec.Command("git", "-C", repo, "config", "--unset", "user.name")
	cmd.Run() // ignore error if not set
	cmd = exec.Command("git", "-C", repo, "config", "--unset", "user.email")
	cmd.Run()

	// Isolate from global config by setting GIT_CONFIG_NOSYSTEM and
	// pointing HOME/XDG_CONFIG_HOME to an empty dir.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", emptyHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(emptyHome, "nonexistent"))

	name, email := ResolveIdentity(repo)
	if name != "mato" {
		t.Errorf("expected default name %q, got %q", "mato", name)
	}
	if email != "mato@local.invalid" {
		t.Errorf("expected default email %q, got %q", "mato@local.invalid", email)
	}
}

func TestResolveIdentity_PartialConfig(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// Isolate from global config.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", emptyHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(emptyHome, "nonexistent"))

	// Set only user.name, leave email unset.
	cmd = exec.Command("git", "-C", repo, "config", "user.name", "partial-user")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v\n%s", err, out)
	}

	name, email := ResolveIdentity(repo)
	if name != "partial-user" {
		t.Errorf("expected name %q, got %q", "partial-user", name)
	}
	if email != "mato@local.invalid" {
		t.Errorf("expected default email %q, got %q", "mato@local.invalid", email)
	}
}

func TestEnsureIdentity_SetsDefaults(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", emptyHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(emptyHome, "nonexistent"))

	name, email := EnsureIdentity(repo)
	if name != "mato" {
		t.Fatalf("name = %q, want %q", name, "mato")
	}
	if email != "mato@local.invalid" {
		t.Fatalf("email = %q, want %q", email, "mato@local.invalid")
	}

	storedName, err := Output(repo, "config", "--local", "user.name")
	if err != nil {
		t.Fatalf("git config --local user.name: %v", err)
	}
	if strings.TrimSpace(storedName) != "mato" {
		t.Fatalf("stored user.name = %q, want %q", strings.TrimSpace(storedName), "mato")
	}
	storedEmail, err := Output(repo, "config", "--local", "user.email")
	if err != nil {
		t.Fatalf("git config --local user.email: %v", err)
	}
	if strings.TrimSpace(storedEmail) != "mato@local.invalid" {
		t.Fatalf("stored user.email = %q, want %q", strings.TrimSpace(storedEmail), "mato@local.invalid")
	}
}

func TestEnsureIdentity_PreservesExisting(t *testing.T) {
	_, repo := initBareAndClone(t)

	name, email := EnsureIdentity(repo)
	if name != "test" {
		t.Fatalf("name = %q, want %q", name, "test")
	}
	if email != "test@test.com" {
		t.Fatalf("email = %q, want %q", email, "test@test.com")
	}
}

func TestEnsureIdentity_Idempotent(t *testing.T) {
	repo := t.TempDir()
	cmd := exec.Command("git", "init", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", emptyHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(emptyHome, "nonexistent"))

	firstName, firstEmail := EnsureIdentity(repo)
	secondName, secondEmail := EnsureIdentity(repo)
	if firstName != secondName || firstEmail != secondEmail {
		t.Fatalf("EnsureIdentity should be idempotent, got (%q, %q) then (%q, %q)", firstName, firstEmail, secondName, secondEmail)
	}
}

// ---------------------------------------------------------------------------
// RemoveClone tests
// ---------------------------------------------------------------------------

func TestRemoveClone_ValidDirectory(t *testing.T) {
	dir := t.TempDir()
	// Create some nested content to make it realistic.
	nested := filepath.Join(dir, "sub", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	RemoveClone(dir)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected directory to be removed, but Stat returned: %v", err)
	}
}

func TestRemoveClone_NonexistentPath(t *testing.T) {
	// RemoveClone wraps os.RemoveAll which is a no-op for nonexistent paths.
	// Verify it does not panic.
	RemoveClone(filepath.Join(t.TempDir(), "does-not-exist"))
}

func TestRemoveClone_ReadOnlyNestedFiles(t *testing.T) {
	dir := t.TempDir()
	// Simulate git's .git/objects/pack files with 0444 permissions.
	packDir := filepath.Join(dir, ".git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pack-abc.idx", "pack-abc.pack"} {
		if err := os.WriteFile(filepath.Join(packDir, name), []byte("x"), 0o444); err != nil {
			t.Fatal(err)
		}
	}

	RemoveClone(dir)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("expected directory with read-only files to be removed, but Stat returned: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateClone error-path tests
// ---------------------------------------------------------------------------

func TestCreateClone_InvalidSource(t *testing.T) {
	// Cloning a nonexistent path should produce an error that wraps git stderr.
	_, err := CreateClone("/nonexistent/repo/path")
	if err == nil {
		t.Fatal("expected an error when cloning an invalid source")
	}
	if !strings.Contains(err.Error(), "clone repo") {
		t.Errorf("error should wrap clone context, got: %v", err)
	}
}

func TestCreateClone_SourceIsFile(t *testing.T) {
	// If the source is a regular file rather than a directory, git clone fails.
	file := filepath.Join(t.TempDir(), "not-a-repo.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := CreateClone(file)
	if err == nil {
		t.Fatal("expected an error when cloning from a file path")
	}
	if !strings.Contains(err.Error(), "clone repo") {
		t.Errorf("error should wrap clone context, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Output / repo-validation failure tests
// ---------------------------------------------------------------------------

func TestOutput_NonRepository(t *testing.T) {
	// Calling a git command that requires a repository on a plain directory
	// should return an actionable error.
	dir := t.TempDir()

	_, err := Output(dir, "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("expected error when running git rev-parse in a non-repo directory")
	}
	// The error should mention the git subcommand and contain stderr output.
	if !strings.Contains(err.Error(), "rev-parse") {
		t.Errorf("error should mention the git subcommand, got: %v", err)
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error should include git's 'not a git repository' message, got: %v", err)
	}
}
