package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindGitHelper_InPath(t *testing.T) {
	// "git" itself should always be findable in PATH during tests.
	path, err := findGitHelper("git")
	if err != nil {
		t.Fatalf("findGitHelper(\"git\") returned error: %v", err)
	}
	if path == "" {
		t.Fatal("findGitHelper(\"git\") returned empty path")
	}
}

func TestFindGitHelper_FallbackToExecPath(t *testing.T) {
	// git-upload-pack may not be in PATH but should be in git --exec-path.
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	execPath := strings.TrimSpace(string(out))
	candidate := filepath.Join(execPath, "git-upload-pack")
	if _, statErr := os.Stat(candidate); statErr != nil {
		t.Skipf("git-upload-pack not found at %s, skipping fallback test", candidate)
	}

	path, err := findGitHelper("git-upload-pack")
	if err != nil {
		t.Fatalf("findGitHelper(\"git-upload-pack\") returned error: %v", err)
	}
	if path == "" {
		t.Fatal("findGitHelper(\"git-upload-pack\") returned empty path")
	}
}

func TestFindGitHelper_NotFound(t *testing.T) {
	_, err := findGitHelper("nonexistent-git-tool-xyz")
	if err == nil {
		t.Fatal("findGitHelper should return error for nonexistent tool")
	}
}
