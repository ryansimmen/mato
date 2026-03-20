package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Output(dir string, args ...string) (string, error) {
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

func CreateClone(repoRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "mato-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	if _, err := Output("", "clone", "--quiet", repoRoot, dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("clone repo: %w", err)
	}
	return dir, nil
}

func RemoveClone(dir string) {
	os.RemoveAll(dir)
}

// EnsureBranch ensures the target branch exists and checks it out.
// It prefers the remote-tracking branch (origin/<branch>) as the starting point
// when the local branch is missing, falling back to HEAD only when neither exists.
//
// Before checking for the remote-tracking ref, it fetches the branch from origin
// so that refs/remotes/origin/<branch> is up to date. If the fetch fails (e.g.
// offline, no remote configured), the function falls back to whatever
// remote-tracking ref is already cached locally, or ultimately to HEAD.
func EnsureBranch(repoRoot, branch string) error {
	// If the local branch already exists, just check it out.
	if _, err := Output(repoRoot, "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		if _, err := Output(repoRoot, "checkout", branch); err != nil {
			return fmt.Errorf("checkout branch %s: %w", branch, err)
		}
		return nil
	}
	// Fetch the specific branch from origin to refresh the remote-tracking ref.
	// Failure is non-fatal: the repo may be offline or have no remote.
	_, _ = Output(repoRoot, "fetch", "--quiet", "origin", branch)
	// If the remote-tracking branch exists, create the local branch from it.
	if _, err := Output(repoRoot, "rev-parse", "--verify", "refs/remotes/origin/"+branch); err == nil {
		if _, err := Output(repoRoot, "checkout", "-b", branch, "origin/"+branch); err != nil {
			return fmt.Errorf("create branch %s from origin/%s: %w", branch, branch, err)
		}
		return nil
	}
	// Neither local nor remote exists; create from HEAD.
	if _, err := Output(repoRoot, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("create branch %s: %w", branch, err)
	}
	return nil
}

// gitignoreIsDirty checks whether .gitignore has pre-existing uncommitted
// changes (staged or unstaged) that should not be swept into the mato commit.
func gitignoreIsDirty(repoRoot string) bool {
	out, _ := Output(repoRoot, "diff", "--name-only", "--", ".gitignore")
	if strings.TrimSpace(out) != "" {
		return true
	}
	out, _ = Output(repoRoot, "diff", "--cached", "--name-only", "--", ".gitignore")
	return strings.TrimSpace(out) != ""
}

// addPatternToContent ensures the given pattern line is present in content,
// appending it if missing. Returns the (possibly updated) content.
func addPatternToContent(content, pattern string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			return content
		}
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		return content + "\n" + pattern + "\n"
	}
	return content + pattern + "\n"
}

// EnsureGitignored appends pattern to the repo's .gitignore if not already present.
// Uses atomic write (temp file + rename) to prevent partial writes on crash.
//
// If .gitignore has pre-existing uncommitted changes, EnsureGitignored restores
// the committed version before modifying, so the commit contains only the new
// pattern. The pre-existing working-tree changes are restored afterwards.
func EnsureGitignored(repoRoot, pattern string) (retErr error) {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	// Check for pre-existing uncommitted .gitignore changes.
	dirty := gitignoreIsDirty(repoRoot)

	var savedContent []byte
	if dirty {
		// Save current working tree content so we can restore it after commit.
		var err error
		savedContent, err = os.ReadFile(gitignorePath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read .gitignore: %w", err)
		}
		// Restore the committed version so our commit only includes our change.
		if _, err := Output(repoRoot, "checkout", "HEAD", "--", ".gitignore"); err != nil {
			// .gitignore may not exist in HEAD (e.g. newly staged file).
			Output(repoRoot, "rm", "--cached", "--force", "--", ".gitignore")
			os.Remove(gitignorePath)
		}
		defer func() {
			if savedContent == nil {
				return
			}
			if retErr != nil {
				// On error, restore original working tree content unchanged.
				os.WriteFile(gitignorePath, savedContent, 0o644)
			} else {
				// On success, restore working tree with our pattern merged in.
				restored := addPatternToContent(string(savedContent), pattern)
				os.WriteFile(gitignorePath, []byte(restored), 0o644)
			}
		}()
	}

	data, err := os.ReadFile(gitignorePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read .gitignore: %w", err)
	}
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	}

	// Build updated content: existing data + trailing newline if needed + pattern.
	var content string
	if len(data) > 0 && data[len(data)-1] != '\n' {
		content = string(data) + "\n" + pattern + "\n"
	} else {
		content = string(data) + pattern + "\n"
	}

	// Atomic write: temp file in same dir, then rename.
	dir := filepath.Dir(gitignorePath)
	tmpFile, err := os.CreateTemp(dir, ".gitignore.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for .gitignore: %w", err)
	}
	tmpName := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}

	if err := tmpFile.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file for .gitignore: %w", err)
	}
	if _, err := tmpFile.WriteString(content); err != nil {
		cleanup()
		return fmt.Errorf("write .gitignore: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file for .gitignore: %w", err)
	}
	if err := os.Rename(tmpName, gitignorePath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file for .gitignore: %w", err)
	}

	if _, err := Output(repoRoot, "add", "--", ".gitignore"); err != nil {
		return fmt.Errorf("git add .gitignore: %w", err)
	}
	if _, err := Output(repoRoot, "commit", "-m", "chore: add "+pattern+" to .gitignore", "--", ".gitignore"); err != nil {
		return fmt.Errorf("git commit .gitignore: %w", err)
	}
	return nil
}
