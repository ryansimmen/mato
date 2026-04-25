package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/testutil"
)

func runMatoCommand(t *testing.T, args ...string) (string, error) {
	return runMatoCommandWithEnv(t, nil, args...)
}

func runMatoCommandWithEnv(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()

	if matoBinaryPath == "" {
		t.Fatal("mato test binary was not built")
	}

	cmd := exec.Command(matoBinaryPath, args...)
	cmd.Dir = integrationModuleRoot()
	cmd.Env = append(filteredHostEnv(
		"MATO_BRANCH",
		"MATO_DOCKER_IMAGE",
		"MATO_TASK_MODEL",
		"MATO_REVIEW_MODEL",
		"MATO_REVIEW_SESSION_RESUME_ENABLED",
		"MATO_TASK_REASONING_EFFORT",
		"MATO_REVIEW_REASONING_EFFORT",
		"MATO_AGENT_TIMEOUT",
		"MATO_RETRY_COOLDOWN",
	), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func filteredHostEnv(excludedKeys ...string) []string {
	excluded := make(map[string]struct{}, len(excludedKeys))
	for _, key := range excludedKeys {
		excluded[key] = struct{}{}
	}

	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, excludedKey := excluded[key]; excludedKey {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func TestConfigFile_EndToEnd(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "agent_timeout: not-a-duration\n")

	out, err := runMatoCommand(t, "run", "--repo", repoRoot)
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if !strings.Contains(out, "invalid agent_timeout \"not-a-duration\" in .mato.yaml") {
		t.Fatalf("output = %q, want invalid agent_timeout error", out)
	}
}

func TestConfigFile_DryRunInvalidBranchFromConfig(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "branch: foo..bar\n")

	out, err := runMatoCommandWithEnv(t, []string{"MATO_BRANCH="}, "run", "--repo", repoRoot, "--dry-run")
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if !strings.Contains(out, "invalid branch name \"foo..bar\"") {
		t.Fatalf("output = %q, want invalid branch error", out)
	}
}

func TestConfigFile_DefaultModelRejected(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "default_model: claude-sonnet-4\n")

	out, err := runMatoCommand(t, "run", "--repo", repoRoot, "--dry-run")
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if !strings.Contains(out, "default_model") {
		t.Fatalf("output = %q, want default_model error", out)
	}
}
