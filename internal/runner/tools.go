package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// findGitHelper locates a git helper binary (e.g. "git-upload-pack") by
// checking PATH first, then falling back to git's exec-path.
func findGitHelper(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", fmt.Errorf("find %s: git --exec-path failed: %w", name, err)
	}
	candidate := filepath.Join(strings.TrimSpace(string(out)), name)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("find %s: not in PATH or %s: %w", name, candidate, err)
	}
	return candidate, nil
}
