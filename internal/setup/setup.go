// Package setup bootstraps a repository for mato use without launching the
// Docker runner.
package setup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/messaging"
)

const ignorePattern = "/" + dirs.Root + "/"

// InitResult describes what repository initialization changed.
type InitResult struct {
	DirsCreated        []string
	GitignoreUpdated   bool
	IgnorePattern      string
	BranchName         string
	BranchSource       git.BranchSource
	LocalBranchExisted bool
	AlreadyOnBranch    bool
	TasksDir           string
}

// InitRepo performs the full non-Docker initialization of a mato repository.
// All steps are idempotent.
func InitRepo(repoRoot, branch string) (*InitResult, error) {
	resolvedTasksDir := filepath.Join(repoRoot, dirs.Root)

	result := &InitResult{
		BranchName:    branch,
		TasksDir:      resolvedTasksDir,
		IgnorePattern: ignorePattern,
	}

	currentBranch, err := currentBranch(repoRoot)
	if err == nil && currentBranch == branch {
		result.AlreadyOnBranch = true
	}

	branchExisted, err := branchExists(repoRoot, branch)
	if err != nil {
		return nil, err
	}
	result.LocalBranchExisted = branchExisted

	if !result.AlreadyOnBranch {
		branchResult, err := git.EnsureBranch(repoRoot, branch)
		if err != nil {
			return nil, err
		}
		result.BranchSource = branchResult.Source
	} else {
		result.BranchSource = git.BranchSourceLocal
	}

	hasCommit, err := repoHasCommit(repoRoot)
	if err != nil {
		return nil, err
	}

	dirsCreated, err := initDirs(resolvedTasksDir)
	if err != nil {
		return nil, err
	}
	result.DirsCreated = append(result.DirsCreated, dirsCreated...)

	messagingCreated, err := initMessagingDirs(resolvedTasksDir)
	if err != nil {
		return nil, err
	}
	result.DirsCreated = append(result.DirsCreated, messagingCreated...)
	sort.Strings(result.DirsCreated)

	git.EnsureIdentity(repoRoot)

	contains, err := gitignoreContains(repoRoot, ignorePattern)
	if err != nil {
		return nil, err
	}
	changed := false
	if !contains {
		dirty, err := pathHasLocalChanges(repoRoot, ".gitignore")
		if err != nil {
			return nil, err
		}
		if dirty {
			return nil, fmt.Errorf("cannot update .gitignore: file has local changes; commit, stash, or discard them first")
		}
		changed, err = git.EnsureGitignoreContains(repoRoot, ignorePattern)
		if err != nil {
			return nil, err
		}
	}
	result.GitignoreUpdated = changed
	if changed && hasCommit {
		if err := git.CommitGitignore(repoRoot, "chore: add "+ignorePattern+" to .gitignore"); err != nil {
			return nil, err
		}
	}

	born, err := branchExists(repoRoot, branch)
	if err != nil {
		return nil, err
	}
	if !born {
		if _, err := git.Output(repoRoot, "add", "--", ".gitignore"); err != nil {
			return nil, fmt.Errorf("git add .gitignore: %w", err)
		}
		if _, err := git.Output(repoRoot, "commit", "-m", "chore: initialize mato", "--", ".gitignore"); err != nil {
			return nil, fmt.Errorf("git commit .gitignore: %w", err)
		}
	}

	return result, nil
}

func initDirs(tasksDir string) ([]string, error) {
	rels := append([]string{}, dirs.All...)
	rels = append(rels, dirs.Locks)
	created := make([]string, 0, len(rels))
	for _, rel := range rels {
		path := filepath.Join(tasksDir, rel)
		if info, err := os.Stat(path); err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("queue directory %s is not a directory", rel)
			}
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat queue directory %s: %w", rel, err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, fmt.Errorf("create queue directory %s: %w", rel, err)
		}
		created = append(created, rel)
	}
	return created, nil
}

func initMessagingDirs(tasksDir string) ([]string, error) {
	created := make([]string, 0, len(messaging.MessagingDirs))
	for _, rel := range messaging.MessagingDirs {
		path := filepath.Join(tasksDir, rel)
		if info, err := os.Stat(path); err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("messaging directory %s is not a directory", rel)
			}
			continue
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat messaging directory %s: %w", rel, err)
		}
		created = append(created, rel)
	}
	if err := messaging.Init(tasksDir); err != nil {
		return nil, fmt.Errorf("init messaging: %w", err)
	}
	return created, nil
}

func currentBranch(repoRoot string) (string, error) {
	out, err := git.Output(repoRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func branchExists(repoRoot, branch string) (bool, error) {
	if _, err := git.Output(repoRoot, "rev-parse", "--verify", "refs/heads/"+branch); err != nil {
		if isMissingRevisionError(err) {
			return false, nil
		}
		return false, fmt.Errorf("check branch %s: %w", branch, err)
	}
	return true, nil
}

func repoHasCommit(repoRoot string) (bool, error) {
	if _, err := git.Output(repoRoot, "rev-parse", "HEAD"); err != nil {
		if isMissingRevisionError(err) {
			return false, nil
		}
		return false, fmt.Errorf("check repository HEAD: %w", err)
	}
	return true, nil
}

func isMissingRevisionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Needed a single revision") ||
		strings.Contains(msg, "unknown revision") ||
		strings.Contains(msg, "bad revision") ||
		strings.Contains(msg, "ambiguous argument 'HEAD'")
}

func gitignoreContains(repoRoot, pattern string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read .gitignore: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return true, nil
		}
	}
	return false, nil
}

func pathHasLocalChanges(repoRoot, path string) (bool, error) {
	out, err := git.Output(repoRoot, "status", "--porcelain", "--", path)
	if err != nil {
		return false, fmt.Errorf("check %s status: %w", path, err)
	}
	return strings.TrimSpace(out) != "", nil
}
