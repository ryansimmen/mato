package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mato/internal/testutil"
)

func runMatoCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	cmdArgs := append([]string{"run", "./cmd/mato"}, args...)
	cmd := exec.Command("go", cmdArgs...)
	cmd.Dir = moduleRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestConfigFile_EndToEnd(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "agent_timeout: not-a-duration\n")

	out, err := runMatoCommand(t, "--repo", repoRoot)
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if !strings.Contains(out, "invalid agent_timeout \"not-a-duration\" in .mato.yaml") {
		t.Fatalf("output = %q, want invalid agent_timeout error", out)
	}
}

func TestConfigFile_DryRunWithConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "branch: foo..bar\n")

	out, err := runMatoCommand(t, "--repo", repoRoot, "--dry-run")
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if !strings.Contains(out, "invalid branch name \"foo..bar\"") {
		t.Fatalf("output = %q, want invalid branch error", out)
	}
}
