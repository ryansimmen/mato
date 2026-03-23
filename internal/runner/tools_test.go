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

func TestDiscoverHostTools_MissingCopilotDir(t *testing.T) {
	// Override HOME to a temp dir without a .copilot directory.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when ~/.copilot is missing")
	}
	if !strings.Contains(err.Error(), ".copilot") {
		t.Fatalf("error should mention .copilot, got: %v", err)
	}
}

func TestDiscoverHostTools_CopilotPathIsFile(t *testing.T) {
	// Override HOME to a temp dir where .copilot is a regular file.
	tmpHome := t.TempDir()
	copilotFile := filepath.Join(tmpHome, ".copilot")
	if err := os.WriteFile(copilotFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("create .copilot file: %v", err)
	}
	t.Setenv("HOME", tmpHome)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when ~/.copilot is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error should mention 'not a directory', got: %v", err)
	}
}

func TestDiscoverHostTools_ValidCopilotDir(t *testing.T) {
	// Override HOME to a temp dir with a valid .copilot directory.
	tmpHome := t.TempDir()
	copilotDir := filepath.Join(tmpHome, ".copilot")
	if err := os.Mkdir(copilotDir, 0o755); err != nil {
		t.Fatalf("create .copilot dir: %v", err)
	}
	t.Setenv("HOME", tmpHome)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools should succeed with valid ~/.copilot: %v", err)
	}
	if tools.copilotConfigDir != copilotDir {
		t.Fatalf("copilotConfigDir = %q, want %q", tools.copilotConfigDir, copilotDir)
	}
}
