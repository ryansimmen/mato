package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"mato/internal/git"
	"mato/internal/ui"
)

func resolveRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return wd, nil
}

// gitResolveRepoRoot is the function used to resolve the repository root.
// It defaults to git.ResolveRepoRoot and can be replaced in tests.
var gitResolveRepoRoot = git.ResolveRepoRoot

func resolveRepoRoot(dir string) (string, error) {
	root, err := gitResolveRepoRoot(dir)
	if err != nil {
		return "", fmt.Errorf("resolve repo root for %q: %w", dir, err)
	}
	return root, nil
}

// gitCheckRefFormat is the function used to validate branch names. It
// defaults to running "git check-ref-format --branch" and can be replaced
// in tests.
var gitCheckRefFormat = func(name string) error {
	out, err := exec.Command("git", "check-ref-format", "--branch", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid branch name %q: git check-ref-format rejected it (%s)", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateBranch checks that the branch name is a legal git refname by
// delegating to "git check-ref-format --branch".
func validateBranch(branch string) error {
	return gitCheckRefFormat(branch)
}

// gitRevParseGitDir is the function used to verify a directory is a git
// repository. It defaults to running "git rev-parse --git-dir" and can
// be replaced in tests.
var gitRevParseGitDir = func(dir string) error {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("repo path %q is not a git repository: %s", dir, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateRepoPath checks that dir exists, is a directory, and is a git
// repository by running a lightweight git command.
func validateRepoPath(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("repo path %q does not exist: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repo path %q is not a directory", dir)
	}
	return gitRevParseGitDir(dir)
}

func requireTasksDir(tasksDir string) error {
	return ui.RequireTasksDir(tasksDir)
}
