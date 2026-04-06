package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/testutil"
)

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod executable %s: %v", path, err)
	}
}

func TestInitDirs_CreatesAll(t *testing.T) {
	tasksDir := t.TempDir()

	created, err := initDirs(tasksDir)
	if err != nil {
		t.Fatalf("initDirs: %v", err)
	}

	want := append([]string{}, dirs.All...)
	want = append(want, dirs.Locks)
	if len(created) != len(want) {
		t.Fatalf("created %d dirs, want %d (%v)", len(created), len(want), created)
	}
	for _, rel := range want {
		if _, err := os.Stat(filepath.Join(tasksDir, rel)); err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
	}
}

func TestInitDirs_Idempotent(t *testing.T) {
	tasksDir := t.TempDir()
	if _, err := initDirs(tasksDir); err != nil {
		t.Fatalf("first initDirs: %v", err)
	}

	created, err := initDirs(tasksDir)
	if err != nil {
		t.Fatalf("second initDirs: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("expected no dirs created on second call, got %v", created)
	}
}

func TestInitDirs_PartialExistence(t *testing.T) {
	tasksDir := t.TempDir()
	for _, rel := range []string{dirs.Backlog, dirs.Locks} {
		if err := os.MkdirAll(filepath.Join(tasksDir, rel), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
	}

	created, err := initDirs(tasksDir)
	if err != nil {
		t.Fatalf("initDirs: %v", err)
	}
	for _, rel := range created {
		if rel == dirs.Backlog || rel == dirs.Locks {
			t.Fatalf("existing dir %s should not be reported as created", rel)
		}
	}
}

func TestInitDirs_UnwritableParent(t *testing.T) {
	tasksDir := t.TempDir()
	backlogPath := filepath.Join(tasksDir, dirs.Backlog)
	if err := os.WriteFile(backlogPath, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatalf("write backlog file: %v", err)
	}

	_, err := initDirs(tasksDir)
	if err == nil {
		t.Fatal("expected error when queue path is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitRepo_DefaultTasksDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	result, err := InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if result.TasksDir != filepath.Join(repoRoot, ".mato") {
		t.Fatalf("TasksDir = %q, want %q", result.TasksDir, filepath.Join(repoRoot, ".mato"))
	}
	for _, rel := range append(append([]string{}, dirs.All...), dirs.Locks) {
		if _, err := os.Stat(filepath.Join(result.TasksDir, rel)); err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
	}
	for _, rel := range messaging.MessagingDirs {
		if _, err := os.Stat(filepath.Join(result.TasksDir, rel)); err != nil {
			t.Fatalf("expected messaging dir %s to exist: %v", rel, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "/.mato/") {
		t.Fatalf(".gitignore should contain /.mato/, got %q", string(data))
	}
	branch, err := git.Output(repoRoot, "branch", "--show-current")
	if err != nil {
		t.Fatalf("git branch --show-current: %v", err)
	}
	if strings.TrimSpace(branch) != "mato" {
		t.Fatalf("current branch = %q, want %q", strings.TrimSpace(branch), "mato")
	}
	if result.BranchSource != git.BranchSourceHeadRemoteUnavailable {
		t.Fatalf("BranchSource = %q, want %q", result.BranchSource, git.BranchSourceHeadRemoteUnavailable)
	}
}

func TestInitRepo_Idempotent(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	if _, err := InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("first InitRepo: %v", err)
	}

	result, err := InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("second InitRepo: %v", err)
	}
	if len(result.DirsCreated) != 0 {
		t.Fatalf("expected no dirs created on second init, got %v", result.DirsCreated)
	}
	if result.GitignoreUpdated {
		t.Fatal("expected GitignoreUpdated=false on second init")
	}
	if !result.AlreadyOnBranch {
		t.Fatal("expected AlreadyOnBranch=true on second init")
	}
	if result.BranchSource != git.BranchSourceLocal {
		t.Fatalf("BranchSource = %q, want %q", result.BranchSource, git.BranchSourceLocal)
	}
}

func TestInitRepo_RemoteBranchLeavesCleanWorktree(t *testing.T) {
	remote := testutil.SetupRepo(t)
	cloneParent := t.TempDir()
	cloneDir := filepath.Join(cloneParent, "clone")
	if _, err := git.Output(cloneParent, "clone", remote, cloneDir); err != nil {
		t.Fatalf("git clone: %v", err)
	}
	if _, err := git.Output(cloneDir, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "remote.txt"), []byte("remote\n"), 0o644); err != nil {
		t.Fatalf("write remote.txt: %v", err)
	}
	if _, err := git.Output(cloneDir, "add", "--", "remote.txt"); err != nil {
		t.Fatalf("git add remote.txt: %v", err)
	}
	if _, err := git.Output(cloneDir, "commit", "-m", "add remote branch commit"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(cloneDir, "push", "-u", "origin", "mato"); err != nil {
		t.Fatalf("git push origin mato: %v", err)
	}

	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "clone", remote, "."); err != nil {
		t.Fatalf("git clone working repo: %v", err)
	}

	result, err := InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	status, err := git.Output(repoRoot, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if strings.TrimSpace(status) != "" {
		t.Fatalf("expected clean worktree after init, got %q", status)
	}
	log, err := git.Output(repoRoot, "log", "--oneline", "-2")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(log, "chore: add /.mato/ to .gitignore") {
		t.Fatalf("expected gitignore commit on fetched branch, got %q", log)
	}
	if !strings.Contains(log, "remote branch commit") {
		t.Fatalf("expected init to stay on fetched remote-backed branch history, got %q", log)
	}
	if result.BranchSource != git.BranchSourceRemote {
		t.Fatalf("BranchSource = %q, want %q", result.BranchSource, git.BranchSourceRemote)
	}
}

func TestInitRepo_EmptyRepo(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}

	result, err := InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if result.LocalBranchExisted {
		t.Fatal("expected LocalBranchExisted=false for empty repo")
	}
	head, err := git.Output(repoRoot, "rev-parse", "--verify", "refs/heads/mato")
	if err != nil {
		t.Fatalf("verify refs/heads/mato: %v", err)
	}
	if strings.TrimSpace(head) == "" {
		t.Fatal("expected born branch after init")
	}
	log, err := git.Output(repoRoot, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(log, "chore: initialize mato") {
		t.Fatalf("expected bootstrap commit, got %q", log)
	}
	if result.BranchSource != git.BranchSourceHeadRemoteUnavailable {
		t.Fatalf("BranchSource = %q, want %q", result.BranchSource, git.BranchSourceHeadRemoteUnavailable)
	}
}

func TestInitRepo_EmptyRepoStagedFilesPreserved(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "notes.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "--", "notes.txt"); err != nil {
		t.Fatalf("git add notes.txt: %v", err)
	}

	if _, err := InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	log, err := git.Output(repoRoot, "log", "--oneline", "--name-only", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if strings.Contains(log, "notes.txt") {
		t.Fatalf("bootstrap commit should not include staged notes.txt, got %q", log)
	}
	status, err := git.Output(repoRoot, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(status, "A  notes.txt") {
		t.Fatalf("expected notes.txt to remain staged, got %q", status)
	}
}

func TestInitRepo_EmptyRepoExistingGitignoreUsesPorcelainEmptyCommit(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	gitignoreContent := "/.mato/\n*.tmp\n"
	if err := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "notes.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("write notes.txt: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "--", "notes.txt"); err != nil {
		t.Fatalf("git add notes.txt: %v", err)
	}

	result, err := InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if result.GitignoreUpdated {
		t.Fatal("expected GitignoreUpdated=false for unchanged .gitignore")
	}
	head, err := git.Output(repoRoot, "rev-parse", "--verify", "refs/heads/mato")
	if err != nil {
		t.Fatalf("verify refs/heads/mato: %v", err)
	}
	if strings.TrimSpace(head) == "" {
		t.Fatal("expected born branch after init")
	}

	show, err := git.Output(repoRoot, "show", "--format=fuller", "--stat", "--name-only", "-1")
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	if !strings.Contains(show, "chore: initialize mato") {
		t.Fatalf("expected bootstrap commit, got %q", show)
	}
	if strings.Contains(show, ".gitignore") {
		t.Fatalf("bootstrap commit should not include unchanged .gitignore, got %q", show)
	}
	if strings.Contains(show, "notes.txt") {
		t.Fatalf("bootstrap commit should not include staged notes.txt, got %q", show)
	}

	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(data) != gitignoreContent {
		t.Fatalf(".gitignore content changed: got %q want %q", string(data), gitignoreContent)
	}

	status, err := git.Output(repoRoot, "status", "--porcelain")
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(status, "A  notes.txt") {
		t.Fatalf("expected notes.txt to remain staged, got %q", status)
	}
	if !strings.Contains(status, "?? .gitignore") {
		t.Fatalf("expected unchanged .gitignore to remain untracked, got %q", status)
	}
}

func TestInitRepo_EmptyRepoExistingGitignoreRunsCommitHook(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".gitignore"), []byte("/.mato/\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	hooksDir := filepath.Join(repoRoot, ".githooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	markerPath := filepath.Join(repoRoot, "hook-ran.txt")
	writeExecutable(t, filepath.Join(hooksDir, "commit-msg"), fmt.Sprintf("#!/bin/sh\nprintf 'hook-ran\\n' >> %q\n", markerPath))
	if _, err := git.Output(repoRoot, "config", "core.hooksPath", hooksDir); err != nil {
		t.Fatalf("git config core.hooksPath: %v", err)
	}

	if _, err := InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read hook marker: %v", err)
	}
	if !strings.Contains(string(data), "hook-ran") {
		t.Fatalf("expected commit hook marker, got %q", string(data))
	}
}

func TestInitRepo_DirtyGitignoreRejected(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	if err := os.WriteFile(gitignorePath, []byte("*.tmp\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}

	_, err := InitRepo(repoRoot, "mato")
	if err == nil {
		t.Fatal("expected error for dirty .gitignore")
	}
	if !strings.Contains(err.Error(), "cannot update .gitignore: file has local changes") {
		t.Fatalf("unexpected error: %v", err)
	}
	status, statusErr := git.Output(repoRoot, "status", "--porcelain")
	if statusErr != nil {
		t.Fatalf("git status: %v", statusErr)
	}
	if !strings.Contains(status, " M .gitignore") && !strings.Contains(status, "M  .gitignore") && !strings.Contains(status, "MM .gitignore") && !strings.Contains(status, "?? .gitignore") {
		t.Fatalf("expected .gitignore to remain dirty, got %q", status)
	}
}

func TestInitRepo_GitIdentitySet(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	t.Setenv("XDG_CONFIG_HOME", emptyHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(emptyHome, "nonexistent"))
	if _, err := InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	name, err := git.Output(repoRoot, "config", "--local", "user.name")
	if err != nil {
		t.Fatalf("git config --local user.name: %v", err)
	}
	if strings.TrimSpace(name) != "mato" {
		t.Fatalf("user.name = %q, want %q", strings.TrimSpace(name), "mato")
	}
}
