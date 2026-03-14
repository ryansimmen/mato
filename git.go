package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func gitOutput(dir string, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if dir != "" {
		cmdArgs = append(cmdArgs, "-C", dir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func createClone(repoRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "mato-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	if _, err := gitOutput("", "clone", "--quiet", repoRoot, dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("clone repo: %w", err)
	}
	return dir, nil
}

func removeClone(dir string) {
	os.RemoveAll(dir)
}

// ensureBranch creates the target branch from HEAD if it doesn't already exist
// and checks it out so subsequent operations (like .gitignore commits) land on it.
func ensureBranch(repoRoot, branch string) error {
	// Check if the branch already exists locally.
	if _, err := gitOutput(repoRoot, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		// Branch exists — make sure we're on it.
		_, err := gitOutput(repoRoot, "checkout", branch)
		return err
	}
	// Branch doesn't exist — create it from HEAD and check it out.
	if _, err := gitOutput(repoRoot, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("create branch %s: %w", branch, err)
	}
	return nil
}

// ensureGitignored appends pattern to the repo's .gitignore if not already present.
func ensureGitignored(repoRoot, pattern string) error {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	}
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	// Add a newline before the pattern if the file doesn't end with one.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(f)
	}
	fmt.Fprintln(f, pattern)
	f.Close()
	if _, err := gitOutput(repoRoot, "add", ".gitignore"); err != nil {
		return fmt.Errorf("git add .gitignore: %w", err)
	}
	if _, err := gitOutput(repoRoot, "commit", "-m", "chore: add "+pattern+" to .gitignore"); err != nil {
		return fmt.Errorf("git commit .gitignore: %w", err)
	}
	return nil
}
