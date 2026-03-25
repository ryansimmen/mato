package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/git"
	"mato/internal/runner"
	"mato/internal/setup"
	"mato/internal/testutil"
)

func TestInitRepo_EndToEnd(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoRoot, ".mato")

	result, err := setup.InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if result.BranchName != "mato" {
		t.Fatalf("BranchName = %q, want %q", result.BranchName, "mato")
	}
	if result.BranchSource != git.BranchSourceHeadRemoteUnavailable {
		t.Fatalf("BranchSource = %q, want %q", result.BranchSource, git.BranchSourceHeadRemoteUnavailable)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog")); err != nil {
		t.Fatalf("expected backlog dir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "/.mato/") {
		t.Fatalf(".gitignore should contain /.mato/, got %q", string(data))
	}
}

func TestInitRepo_IdempotentEndToEnd(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	if _, err := setup.InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("first InitRepo: %v", err)
	}
	result, err := setup.InitRepo(repoRoot, "mato")
	if err != nil {
		t.Fatalf("second InitRepo: %v", err)
	}
	if len(result.DirsCreated) != 0 {
		t.Fatalf("expected no dirs created on second init, got %v", result.DirsCreated)
	}
}

func TestInitRepo_ThenDryRunWorks(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	if _, err := setup.InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	if err := runner.DryRun(repoRoot, "mato"); err != nil {
		t.Fatalf("DryRun after init: %v", err)
	}
}

func TestInitRepo_EmptyRepoBootstrapCommit(t *testing.T) {
	repoRoot := t.TempDir()
	if _, err := git.Output(repoRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := setup.InitRepo(repoRoot, "mato"); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	log, err := git.Output(repoRoot, "log", "--oneline", "-1")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(log, "chore: initialize mato") {
		t.Fatalf("expected bootstrap commit, got %q", log)
	}
}
