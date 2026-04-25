package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/testutil"
)

func TestConfigCommand_TextAndJSON(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yml"), "branch: main\nreview_model: gpt-5.4\n")

	textOut, err := runMatoCommandWithEnv(t, []string{"MATO_BRANCH="}, "config", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato config text: %v\n%s", err, textOut)
	}
	if !strings.Contains(textOut, "Config file: "+filepath.Join(repoRoot, ".mato.yml")) {
		t.Fatalf("text output = %q", textOut)
	}
	if !strings.Contains(textOut, "branch:") || !strings.Contains(textOut, "(config)") {
		t.Fatalf("text output = %q", textOut)
	}

	jsonOut, err := runMatoCommandWithEnv(t, []string{"MATO_BRANCH="}, "config", "--repo", repoRoot, "--format", "json")
	if err != nil {
		t.Fatalf("mato config json: %v\n%s", err, jsonOut)
	}
	if !strings.Contains(jsonOut, "\"config_path\"") || !strings.Contains(jsonOut, "\"branch\"") {
		t.Fatalf("json output = %q", jsonOut)
	}
}

func TestConfigCommand_EnvOverride(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "docker_image: from-config:1.0\n")
	out, err := runMatoCommandWithEnv(t, []string{"MATO_DOCKER_IMAGE=from-env:2.0"}, "config", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato config: %v\n%s", err, out)
	}
	if !strings.Contains(out, "env: MATO_DOCKER_IMAGE") {
		t.Fatalf("output = %q, want env attribution", out)
	}
}

func TestConfigCommand_BothConfigFilesError(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), "branch: main\n")
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yml"), "branch: develop\n")
	out, err := runMatoCommand(t, "config", "--repo", repoRoot)
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(out, "found both .mato.yaml and .mato.yml") {
		t.Fatalf("output = %q", out)
	}
}

func TestConfigCommand_WorksWithoutMatoDir(t *testing.T) {
	t.Parallel()

	repoRoot := testutil.SetupRepo(t)
	out, err := runMatoCommandWithEnv(t, []string{"MATO_BRANCH="}, "config", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato config: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Config file: none") {
		t.Fatalf("output = %q", out)
	}
}
