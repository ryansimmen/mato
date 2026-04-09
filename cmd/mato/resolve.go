package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"mato/internal/configresolve"
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

// validateBranch checks that the branch name is a legal git refname by
// delegating to "git check-ref-format --branch".
func validateBranch(branch string) error {
	return git.ValidateBranch(branch)
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
	if err := gitRevParseGitDir(dir); err != nil {
		return ui.WithHint(err, "run this command inside a git repository or pass --repo /path/to/repo")
	}
	return nil
}

func validateResolvedBranch(resolved configresolve.Resolved[string]) error {
	if err := validateBranch(resolved.Value); err != nil {
		return ui.WithHint(err, branchHint(resolved))
	}
	return nil
}

func branchHint(resolved configresolve.Resolved[string]) string {
	switch resolved.Source {
	case configresolve.SourceFlag:
		return "pass --branch a valid git ref name such as mato or feature/my-change"
	case configresolve.SourceEnv:
		envVar := resolved.EnvVar
		if envVar == "" {
			envVar = "MATO_BRANCH"
		}
		return fmt.Sprintf("set %s to a valid git ref name such as mato or feature/my-change, or unset it to use the default", envVar)
	case configresolve.SourceConfig:
		return "set branch in .mato.yaml to a valid git ref name such as mato or feature/my-change"
	default:
		return "pass --branch a valid git ref name such as mato or feature/my-change"
	}
}

func requireTasksDir(tasksDir string) error {
	return ui.RequireTasksDir(tasksDir)
}
