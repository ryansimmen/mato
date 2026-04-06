package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mato/internal/config"
	"mato/internal/process"
	"mato/internal/runner"
	"mato/internal/runtimedata"
	"mato/internal/testutil"
)

func writeDoctorConfig(t *testing.T, repoRoot, content string) {
	t.Helper()
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"), content)
}

// stubTools overrides inspectHostToolsFn for tests and restores it on cleanup.
func stubTools(t *testing.T, fn func() runner.ToolReport) {
	t.Helper()
	orig := inspectHostToolsFn
	inspectHostToolsFn = fn
	t.Cleanup(func() { inspectHostToolsFn = orig })
}

// stubDockerLookPath overrides dockerLookPathFn for tests and restores it
// on cleanup.
func stubDockerLookPath(t *testing.T, fn func() error) {
	t.Helper()
	orig := dockerLookPathFn
	dockerLookPathFn = fn
	t.Cleanup(func() { dockerLookPathFn = orig })
}

// stubDocker overrides dockerProbe for tests and restores it on cleanup.
func stubDocker(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	orig := dockerProbe
	dockerProbe = fn
	t.Cleanup(func() { dockerProbe = orig })
}

// stubDockerImageInspect overrides dockerImageInspectFn for tests and
// restores it on cleanup.
func stubDockerImageInspect(t *testing.T, fn func(context.Context, string) error) {
	t.Helper()
	orig := dockerImageInspectFn
	dockerImageInspectFn = fn
	t.Cleanup(func() { dockerImageInspectFn = orig })
}

// stubDockerImagePull overrides dockerImagePullFn for tests and restores
// it on cleanup.
func stubDockerImagePull(t *testing.T, fn func(context.Context, string) error) {
	t.Helper()
	orig := dockerImagePullFn
	dockerImagePullFn = fn
	t.Cleanup(func() { dockerImagePullFn = orig })
}

// allOK stubs tools, docker, and image inspect to report no issues.
func allOK(t *testing.T) {
	t.Helper()
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: "copilot", Path: "/usr/bin/copilot", Required: true, Found: true, Message: "copilot: /usr/bin/copilot"},
			{Name: "git", Path: "/usr/bin/git", Required: true, Found: true, Message: "git: /usr/bin/git"},
		}}
	})
	stubDockerLookPath(t, func() error { return nil })
	stubDocker(t, func(ctx context.Context) error { return nil })
	stubDockerImageInspect(t, func(ctx context.Context, image string) error { return nil })
}

func TestDoctor_HealthyRepo(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Add a simple valid task to backlog.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "test-task.md"),
		"---\nid: test-task\npriority: 10\n---\nDo something\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", report.ExitCode)
	}
	if report.Summary.Errors != 0 {
		t.Errorf("expected 0 errors, got %d", report.Summary.Errors)
	}
	if report.Summary.Warnings != 0 {
		t.Errorf("expected 0 warnings, got %d", report.Summary.Warnings)
	}

	// Verify all checks ran.
	for _, cr := range report.Checks {
		if cr.Status != CheckRan {
			t.Errorf("check %s: expected status %q, got %q", cr.Name, CheckRan, cr.Status)
		}
	}
}

func TestDoctor_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	allOK(t)

	report, err := Run(context.Background(), dir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", report.ExitCode)
	}

	// Should have git.not_a_repo finding.
	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "git.not_a_repo" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected git.not_a_repo finding")
	}
}

func TestDoctor_ConfigIncludedInFullRun(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		if cr.Name == "config" {
			if cr.Status != CheckRan {
				t.Fatalf("config status = %q, want %q", cr.Status, CheckRan)
			}
			return
		}
	}
	t.Fatal("expected config check in full run")
}

func TestDoctor_OnlyConfigRunsOnlyConfig(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"config"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		if cr.Name == "config" {
			if cr.Status != CheckRan {
				t.Fatalf("config status = %q, want %q", cr.Status, CheckRan)
			}
			continue
		}
		if cr.Status != CheckSkipped {
			t.Fatalf("check %s status = %q, want skipped", cr.Name, cr.Status)
		}
	}
	if report.Summary.Errors != 0 {
		t.Fatalf("errors = %d, want 0", report.Summary.Errors)
	}
}

func TestDoctor_ConfigInvalidBranchFromConfig(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	t.Setenv("MATO_BRANCH", "")
	writeDoctorConfig(t, repoRoot, "branch: foo..bar\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"config"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		if cr.Name != "config" {
			continue
		}
		for _, f := range cr.Findings {
			if f.Code == "config.invalid_branch" {
				if f.Path == "" {
					t.Fatal("expected config-path attribution")
				}
				return
			}
		}
	}
	t.Fatal("expected config.invalid_branch finding")
}

func TestDoctor_ConfigInvalidEnvValuesUseEnvAttribution(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	t.Setenv("MATO_REVIEW_SESSION_RESUME_ENABLED", "maybe")
	t.Setenv("MATO_AGENT_TIMEOUT", "bad")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"config"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundBool := false
	foundDuration := false
	for _, cr := range report.Checks {
		if cr.Name != "config" {
			continue
		}
		for _, f := range cr.Findings {
			if f.Code == "config.invalid_bool" && strings.Contains(f.Message, "MATO_REVIEW_SESSION_RESUME_ENABLED") {
				foundBool = true
			}
			if f.Code == "config.invalid_duration" && strings.Contains(f.Message, "MATO_AGENT_TIMEOUT") {
				foundDuration = true
			}
		}
	}
	if !foundBool || !foundDuration {
		t.Fatalf("foundBool=%v foundDuration=%v", foundBool, foundDuration)
	}
}

func TestDoctor_ConfigParseErrorBecomesFinding(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	writeDoctorConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		if cr.Name != "config" {
			continue
		}
		for _, f := range cr.Findings {
			if f.Code == "config.parse_error" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected config.parse_error finding")
	}
}

func TestDoctor_ConfigOnlyNonRepoReportsConfigSpecificError(t *testing.T) {
	dir := t.TempDir()
	allOK(t)

	report, err := Run(context.Background(), dir, Options{Format: "text", Only: []string{"config"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundConfigNoRepo := false
	foundGitNoRepo := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "config.no_repo" {
				foundConfigNoRepo = true
			}
			if f.Code == "git.not_a_repo" {
				foundGitNoRepo = true
			}
		}
	}
	if !foundConfigNoRepo {
		t.Fatal("expected config.no_repo finding")
	}
	if foundGitNoRepo {
		t.Fatal("did not expect git.not_a_repo in config-only run")
	}
}

func TestDoctor_FullRunNonRepoSuppressesConfigNoRepo(t *testing.T) {
	dir := t.TempDir()
	allOK(t)

	report, err := Run(context.Background(), dir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundGitNoRepo := false
	foundConfigNoRepo := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "git.not_a_repo" {
				foundGitNoRepo = true
			}
			if f.Code == "config.no_repo" {
				foundConfigNoRepo = true
			}
		}
	}
	if !foundGitNoRepo {
		t.Fatal("expected git.not_a_repo finding")
	}
	if foundConfigNoRepo {
		t.Fatal("did not expect duplicate config.no_repo finding")
	}
}

func TestDoctor_DockerUsesCachedConfigImage(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	writeDoctorConfig(t, repoRoot, "docker_image: custom:latest\n")

	var inspectedImage string
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		inspectedImage = image
		return nil
	})

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"config", "docker"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inspectedImage != "custom:latest" {
		t.Fatalf("inspected image = %q, want custom:latest", inspectedImage)
	}
	if report.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", report.ExitCode)
	}
}

func TestDoctor_DockerStillChecksImageWhenOtherConfigFindingsExist(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	writeDoctorConfig(t, repoRoot, "docker_image: custom:latest\ntask_reasoning_effort: nope\n")

	var inspectedImage string
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		inspectedImage = image
		return fmt.Errorf("No such image: %s", image)
	})

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"config", "docker"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inspectedImage != "custom:latest" {
		t.Fatalf("inspected image = %q, want custom:latest", inspectedImage)
	}
	foundMissing := false
	for _, cr := range report.Checks {
		if cr.Name != "docker" {
			continue
		}
		for _, f := range cr.Findings {
			if f.Code == "docker.image_missing" {
				foundMissing = true
			}
		}
	}
	if !foundMissing {
		t.Fatal("expected docker.image_missing finding")
	}
}

func TestDoctor_DockerSkipsImageInspectionAfterFatalConfigFailure(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	writeDoctorConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	inspected := false
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		inspected = true
		return nil
	})

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"config", "docker"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inspected {
		t.Fatal("expected docker image inspection to be skipped")
	}
	foundReachable := false
	foundImageFinding := false
	for _, cr := range report.Checks {
		if cr.Name != "docker" {
			continue
		}
		for _, f := range cr.Findings {
			if f.Code == "docker.reachable" {
				foundReachable = true
			}
			if strings.HasPrefix(f.Code, "docker.image_") {
				foundImageFinding = true
			}
		}
	}
	if !foundReachable {
		t.Fatal("expected docker.reachable finding")
	}
	if foundImageFinding {
		t.Fatal("did not expect docker image findings after fatal config failure")
	}
}

func TestDoctor_FullRunNonRepoDockerFallsBackToDefaultImage(t *testing.T) {
	dir := t.TempDir()
	allOK(t)

	var inspectedImage string
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		inspectedImage = image
		return nil
	})

	report, err := Run(context.Background(), dir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inspectedImage != config.DefaultDockerImage {
		t.Fatalf("inspected image = %q, want %q", inspectedImage, config.DefaultDockerImage)
	}
	foundConfigNoRepo := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "config.no_repo" {
				foundConfigNoRepo = true
			}
		}
	}
	if foundConfigNoRepo {
		t.Fatal("did not expect config.no_repo in full run")
	}
}

func TestDoctor_JSONIncludesConfigCheck(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{Format: "json"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var parsed struct {
		Checks []struct {
			Name string `json:"name"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, check := range parsed.Checks {
		if check.Name == "config" {
			return
		}
	}
	t.Fatal("expected config check in JSON output")
}

func TestDoctor_MissingQueueDir(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Remove a directory to trigger the check.
	os.RemoveAll(filepath.Join(tasksDir, "waiting"))

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", report.ExitCode)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.missing_dir" && strings.Contains(f.Message, "waiting") {
				found = true
				if !f.Fixable {
					t.Error("expected missing_dir finding to be fixable")
				}
			}
		}
	}
	if !found {
		t.Error("expected queue.missing_dir finding for waiting")
	}
}

func TestDoctor_MissingQueueDir_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	os.RemoveAll(filepath.Join(tasksDir, "waiting"))

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// After fix, the directory should exist and finding should be marked fixed.
	if _, statErr := os.Stat(filepath.Join(tasksDir, "waiting")); statErr != nil {
		t.Error("expected waiting/ directory to be created by --fix")
	}

	fixedFound := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.missing_dir" && f.Fixed {
				fixedFound = true
			}
		}
	}
	if !fixedFound {
		t.Error("expected queue.missing_dir finding to be marked fixed")
	}

	if report.ExitCode != 0 {
		t.Errorf("expected exit code 0 after fix, got %d", report.ExitCode)
	}
}

func TestDoctor_MalformedTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "broken.md"),
		"---\n  bad yaml: [unclosed\n---\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", report.ExitCode)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "tasks.parse_error" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected tasks.parse_error finding")
	}
}

func TestDoctor_StalePIDLock(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a .pid file with a dead PID.
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "deadbeef.pid"), "999999:0")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_pid" {
				found = true
				if !f.Fixable {
					t.Error("expected stale_pid to be fixable")
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_pid finding")
	}
}

func TestDoctor_StalePIDLock_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	pidFile := filepath.Join(tasksDir, ".locks", "deadbeef.pid")
	testutil.WriteFile(t, pidFile, "999999:0")

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(pidFile); !os.IsNotExist(statErr) {
		t.Error("expected stale PID lock to be removed by --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_pid" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_pid finding marked fixed")
	}
}

func TestDoctor_StaleReviewLock(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	lockFile := filepath.Join(tasksDir, ".locks", "review-test.md.lock")
	testutil.WriteFile(t, lockFile, "999999:0")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_review" {
				found = true
				if !f.Fixable {
					t.Error("expected stale_review to be fixable")
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_review finding")
	}
}

func TestDoctor_StaleReviewLock_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	lockFile := filepath.Join(tasksDir, ".locks", "review-test.md.lock")
	testutil.WriteFile(t, lockFile, "999999:0")

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(lockFile); !os.IsNotExist(statErr) {
		t.Error("expected stale review lock to be removed by --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_review" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_review finding marked fixed")
	}
}

func TestDoctor_OrphanedInProgress(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create an in-progress task with a dead agent claim.
	testutil.WriteFile(t, filepath.Join(tasksDir, "in-progress", "orphan.md"),
		"<!-- claimed-by: deadbeef -->\n---\nid: orphan\n---\nOrphan task\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.orphaned_task" {
				found = true
				if !f.Fixable {
					t.Error("expected orphaned_task to be fixable")
				}
				if !strings.Contains(f.Message, "deadbeef") {
					t.Errorf("expected message to include agent ID, got %q", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.orphaned_task finding")
	}
}

func TestDoctor_UnclaimedInProgress(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create an in-progress task with no claimed-by marker at all.
	testutil.WriteFile(t, filepath.Join(tasksDir, "in-progress", "unclaimed.md"),
		"---\nid: unclaimed\n---\nUnclaimed task\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.unclaimed_in_progress" {
				found = true
				if !f.Fixable {
					t.Error("expected unclaimed_in_progress to be fixable")
				}
				if !strings.Contains(f.Message, "no claimed-by marker") {
					t.Errorf("expected message about missing marker, got %q", f.Message)
				}
			}
			if f.Code == "locks.orphaned_task" {
				t.Error("unclaimed task should not produce locks.orphaned_task finding")
			}
		}
	}
	if !found {
		t.Error("expected locks.unclaimed_in_progress finding")
	}
}

func TestDoctor_UnclaimedInProgress_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	inProgressFile := filepath.Join(tasksDir, "in-progress", "unclaimed.md")
	testutil.WriteFile(t, inProgressFile,
		"---\nid: unclaimed\n---\nUnclaimed task\n")
	if err := runtimedata.UpdateTaskState(tasksDir, "unclaimed.md", func(state *runtimedata.TaskState) {
		state.TaskBranch = "task/unclaimed"
		state.LastOutcome = runtimedata.OutcomeWorkLaunched
	}); err != nil {
		t.Fatalf("seed work-launched taskstate: %v", err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Task should be moved to backlog.
	if _, statErr := os.Stat(inProgressFile); !os.IsNotExist(statErr) {
		t.Error("expected unclaimed in-progress task to be removed by --fix")
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, "backlog", "unclaimed.md")); statErr != nil {
		t.Error("expected unclaimed.md in backlog/ after --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.unclaimed_in_progress" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected locks.unclaimed_in_progress finding marked fixed")
	}
}

func TestDoctor_OrphanedInProgress_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	inProgressFile := filepath.Join(tasksDir, "in-progress", "orphan.md")
	testutil.WriteFile(t, inProgressFile,
		"<!-- claimed-by: deadbeef -->\n---\nid: orphan\n---\nOrphan task\n")
	if err := runtimedata.UpdateTaskState(tasksDir, "orphan.md", func(state *runtimedata.TaskState) {
		state.TaskBranch = "task/orphan"
		state.LastOutcome = runtimedata.OutcomeWorkLaunched
	}); err != nil {
		t.Fatalf("seed work-launched taskstate: %v", err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Task should be moved to backlog.
	if _, statErr := os.Stat(inProgressFile); !os.IsNotExist(statErr) {
		t.Error("expected in-progress task to be removed by --fix")
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, "backlog", "orphan.md")); statErr != nil {
		t.Error("expected orphan.md in backlog/ after --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.orphaned_task" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected locks.orphaned_task finding marked fixed")
	}
}

func TestDoctor_SelfDependency(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "self-dep.md"),
		"---\nid: self-dep\ndepends_on:\n  - self-dep\n---\nSelf dep\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "deps.self_dependency" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected deps.self_dependency finding")
	}
}

func TestDoctor_CircularDependency(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "a.md"),
		"---\nid: a\ndepends_on:\n  - b\n---\nTask A\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "b.md"),
		"---\nid: b\ndepends_on:\n  - a\n---\nTask B\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := 0
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "deps.cycle" {
				found++
			}
		}
	}
	if found < 2 {
		t.Errorf("expected at least 2 deps.cycle findings, got %d", found)
	}
}

func TestDoctor_UnknownDependencyID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "dep-unknown.md"),
		"---\nid: dep-unknown\ndepends_on:\n  - nonexistent\n---\nDep unknown\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "deps.unknown_id" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected deps.unknown_id finding")
	}
}

func TestDoctor_AmbiguousID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Same ID in both completed and waiting.
	testutil.WriteFile(t, filepath.Join(tasksDir, "completed", "amb-task.md"),
		"---\nid: amb-task\n---\nCompleted\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "amb-task.md"),
		"---\nid: amb-task\n---\nWaiting\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "deps.ambiguous_id" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected deps.ambiguous_id finding")
	}
}

func TestDoctor_DuplicateWaitingID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "dup1.md"),
		"---\nid: dup-id\n---\nFirst\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, "waiting", "dup2.md"),
		"---\nid: dup-id\n---\nSecond\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "deps.duplicate_id" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected deps.duplicate_id finding")
	}
}

func TestDoctor_InvalidGlobSyntax(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "bad-glob.md"),
		"---\nid: bad-glob\naffects:\n  - \"internal/*/\"\n---\nBad glob\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2 (error), got %d", report.ExitCode)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "tasks.invalid_glob" {
				found = true
				if f.Severity != SeverityError {
					t.Errorf("expected tasks.invalid_glob severity %q, got %q", SeverityError, f.Severity)
				}
			}
		}
	}
	if !found {
		t.Error("expected tasks.invalid_glob finding")
	}
}

func TestDoctor_UnsafeAffectsEntries(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Task with absolute path in affects.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "abs-path.md"),
		"---\nid: abs-path\naffects:\n  - /etc/passwd\n---\nAbsolute path task\n")

	// Task with path traversal in affects.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "traversal.md"),
		"---\nid: traversal\naffects:\n  - ../../secret\n---\nTraversal task\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2 (error), got %d", report.ExitCode)
	}

	foundAbs := false
	foundTraversal := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "tasks.unsafe_affects" {
				if f.Severity != SeverityError {
					t.Errorf("expected tasks.unsafe_affects severity %q, got %q", SeverityError, f.Severity)
				}
				if strings.Contains(f.Message, "absolute path") {
					foundAbs = true
				}
				if strings.Contains(f.Message, "path traversal") {
					foundTraversal = true
				}
			}
		}
	}
	if !foundAbs {
		t.Error("expected tasks.unsafe_affects finding for absolute path")
	}
	if !foundTraversal {
		t.Error("expected tasks.unsafe_affects finding for path traversal")
	}
}

func TestDoctor_OnlyFilter(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{
		Format: "text",
		Only:   []string{"git", "queue"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		switch cr.Name {
		case "git", "queue":
			if cr.Status != CheckRan {
				t.Errorf("check %s: expected ran, got %s", cr.Name, cr.Status)
			}
		default:
			if cr.Status != CheckSkipped {
				t.Errorf("check %s: expected skipped, got %s", cr.Name, cr.Status)
			}
		}
	}
}

func TestDoctor_OnlyFilter_InvalidName(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{
		Format: "text",
		Only:   []string{"bogus"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", report.ExitCode)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "doctor.invalid_only" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected doctor.invalid_only finding")
	}
}

func TestDoctor_OnlyFilter_PrereqFailure(t *testing.T) {
	dir := t.TempDir()
	allOK(t)

	// --only queue with a bad repo.
	report, err := Run(context.Background(), dir, Options{
		Format: "text",
		Only:   []string{"queue"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", report.ExitCode)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.no_tasks_dir" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected queue.no_tasks_dir finding")
	}
}

func TestDoctor_DockerTimeout(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{}
	})
	stubDockerLookPath(t, func() error { return nil })
	stubDocker(t, func(ctx context.Context) error {
		return fmt.Errorf("docker daemon unreachable: timeout")
	})

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.daemon_unreachable" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected docker.daemon_unreachable finding")
	}
}

func TestDoctor_FixReporting(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Remove directory and add a stale lock. After fix, only the
	// directory is fixable.
	os.RemoveAll(filepath.Join(tasksDir, "waiting"))
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "dead.pid"), "999999:0")

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.Summary.Fixed < 2 {
		t.Errorf("expected at least 2 fixed items, got %d", report.Summary.Fixed)
	}

	// Exit code should be 0 if everything was fixed.
	if report.ExitCode != 0 {
		t.Errorf("expected exit code 0 after fix, got %d", report.ExitCode)
	}
}

func TestDoctor_FixJSONValid(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	os.RemoveAll(filepath.Join(tasksDir, "waiting"))

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "json"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if renderErr := RenderJSON(&buf, report); renderErr != nil {
		t.Fatalf("RenderJSON: %v", renderErr)
	}

	var parsed map[string]interface{}
	if jsonErr := json.Unmarshal(buf.Bytes(), &parsed); jsonErr != nil {
		t.Fatalf("invalid JSON output: %v\n%s", jsonErr, buf.String())
	}
}

func TestDoctor_JSONIncludesExitCode(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{Format: "json"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if renderErr := RenderJSON(&buf, report); renderErr != nil {
		t.Fatalf("RenderJSON: %v", renderErr)
	}

	var parsed map[string]interface{}
	if jsonErr := json.Unmarshal(buf.Bytes(), &parsed); jsonErr != nil {
		t.Fatalf("invalid JSON output: %v", jsonErr)
	}

	if _, ok := parsed["exit_code"]; !ok {
		t.Error("JSON output missing exit_code field")
	}
}

func TestDoctor_ContextCancellation(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := Run(ctx, repoRoot, Options{Format: "text"})
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

func TestRenderText(t *testing.T) {
	report := Report{
		RepoInput: "/repo",
		RepoRoot:  "/repo",
		TasksDir:  "/repo/.mato",
		Checks: []CheckReport{
			{Name: "git", Status: CheckRan, Findings: []Finding{
				{Code: "git.repo_root", Severity: SeverityInfo, Message: "repo root: /repo"},
			}},
			{Name: "tools", Status: CheckRan, Findings: []Finding{
				{Code: "tools.missing_optional", Severity: SeverityWarning, Message: "gh config dir not found"},
			}},
			{Name: "docker", Status: CheckSkipped, Findings: []Finding{}},
		},
		Summary:  Summary{Warnings: 1, Errors: 0, Fixed: 0},
		ExitCode: 1,
	}

	var buf bytes.Buffer
	if err := RenderText(&buf, report); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	output := buf.String()

	if !strings.Contains(output, "[OK] git") {
		t.Error("expected [OK] git in output")
	}
	if !strings.Contains(output, "[WARN] tools") {
		t.Error("expected [WARN] tools in output")
	}
	if !strings.Contains(output, "[SKIP] docker") {
		t.Error("expected [SKIP] docker in output")
	}
	if !strings.Contains(output, "mato doctor: 1 warning") {
		t.Errorf("expected summary line with 1 warning, got: %s", output)
	}
}

func TestRenderJSON(t *testing.T) {
	report := Report{
		RepoInput: "/repo",
		RepoRoot:  "/repo",
		TasksDir:  "/repo/.mato",
		Checks: []CheckReport{
			{Name: "git", Status: CheckRan, Findings: []Finding{
				{Code: "git.repo_root", Severity: SeverityInfo, Message: "repo root: /repo"},
			}},
		},
		Summary:  Summary{},
		ExitCode: 0,
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, report); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var parsed Report
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}

	if parsed.RepoRoot != "/repo" {
		t.Errorf("expected repo_root /repo, got %s", parsed.RepoRoot)
	}
	if len(parsed.Checks) != 1 {
		t.Errorf("expected 1 check, got %d", len(parsed.Checks))
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name     string
		warnings int
		errors   int
		want     int
	}{
		{"healthy", 0, 0, 0},
		{"warnings only", 1, 0, 1},
		{"errors", 0, 1, 2},
		{"both", 2, 1, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Report{
				Checks: []CheckReport{{
					Name:   "test",
					Status: CheckRan,
					Findings: func() []Finding {
						var fs []Finding
						for i := 0; i < tt.warnings; i++ {
							fs = append(fs, Finding{Code: "w", Severity: SeverityWarning, Message: "warn"})
						}
						for i := 0; i < tt.errors; i++ {
							fs = append(fs, Finding{Code: "e", Severity: SeverityError, Message: "err"})
						}
						return fs
					}(),
				}},
			}
			computeSummary(&r)
			if r.ExitCode != tt.want {
				t.Errorf("exit code: got %d, want %d", r.ExitCode, tt.want)
			}
		})
	}
}

func TestDoctor_StaleDuplicate(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Task exists in both in-progress and completed.
	testutil.WriteFile(t, filepath.Join(tasksDir, "in-progress", "dup-task.md"),
		"<!-- claimed-by: deadbeef -->\n---\nid: dup-task\n---\nDup task\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, "completed", "dup-task.md"),
		"---\nid: dup-task\n---\nDup task\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_duplicate" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_duplicate finding")
	}
}

func TestDoctor_ActiveAgent(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a .pid lock for the current process (alive).
	pid := os.Getpid()
	identity := process.LockIdentity(pid)
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "liveid.pid"), identity)

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.active_agents" {
				found = true
				if !strings.Contains(f.Message, "1") {
					t.Errorf("expected 1 active agent, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.active_agents finding")
	}
}

func TestDoctor_MissingTasksRoot_Fix(t *testing.T) {
	// A git repo without .mato should have a fixable missing-root finding.
	repoRoot := testutil.SetupRepo(t)
	allOK(t)

	tasksDir := filepath.Join(repoRoot, ".mato")

	// Without --fix: should report missing_tasks_root as fixable.
	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var rootFinding *Finding
	for _, cr := range report.Checks {
		for i, f := range cr.Findings {
			if f.Code == "queue.missing_tasks_root" {
				rootFinding = &cr.Findings[i]
			}
		}
	}
	if rootFinding == nil {
		t.Fatal("expected queue.missing_tasks_root finding")
	}
	if !rootFinding.Fixable {
		t.Error("missing_tasks_root should be fixable")
	}
	if rootFinding.Fixed {
		t.Error("missing_tasks_root should not be fixed without --fix")
	}

	// Confirm .mato still does not exist.
	if _, statErr := os.Stat(tasksDir); !os.IsNotExist(statErr) {
		t.Fatalf(".mato should not exist yet, stat: %v", statErr)
	}

	// With --fix: should create .mato and subdirectories.
	report2, err := Run(context.Background(), repoRoot, Options{Format: "text", Fix: true})
	if err != nil {
		t.Fatalf("Run --fix: %v", err)
	}

	// Root should be fixed.
	rootFixed := false
	for _, cr := range report2.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.missing_tasks_root" && f.Fixed {
				rootFixed = true
			}
		}
	}
	if !rootFixed {
		t.Error("expected queue.missing_tasks_root to be fixed with --fix")
	}

	// Subdirectories should also be created (they'll show as fixed).
	subdirsFixed := 0
	for _, cr := range report2.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.missing_dir" && f.Fixed {
				subdirsFixed++
			}
		}
	}
	if subdirsFixed == 0 {
		t.Error("expected at least one queue.missing_dir to be fixed")
	}

	// Verify .mato now exists.
	info, statErr := os.Stat(tasksDir)
	if statErr != nil {
		t.Fatalf(".mato should exist after --fix, stat: %v", statErr)
	}
	if !info.IsDir() {
		t.Error(".mato should be a directory")
	}
}

func TestDoctor_GitResolveFailed(t *testing.T) {
	// Use a path that doesn't exist at all — git rev-parse will fail
	// with an error that is NOT "not a git repository" but rather a
	// path resolution error.
	allOK(t)

	badPath := filepath.Join(t.TempDir(), "nonexistent-subdir")

	report, err := Run(context.Background(), badPath, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.ExitCode != 2 {
		t.Errorf("expected exit code 2, got %d", report.ExitCode)
	}

	// Should have git.resolve_failed (not git.not_a_repo) since the
	// path doesn't exist.
	var gitFinding *Finding
	for _, cr := range report.Checks {
		for i, f := range cr.Findings {
			if f.Code == "git.resolve_failed" {
				gitFinding = &cr.Findings[i]
			}
		}
	}
	if gitFinding == nil {
		// Dump all findings for diagnosis.
		for _, cr := range report.Checks {
			for _, f := range cr.Findings {
				t.Logf("  %s: %s", f.Code, f.Message)
			}
		}
		t.Fatal("expected git.resolve_failed finding")
	}
	if gitFinding.Severity != SeverityError {
		t.Errorf("expected severity error, got %s", gitFinding.Severity)
	}

	// Dependent checks should include the real stderr from git,
	// not a bare "exit status 128".
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if strings.HasSuffix(f.Code, ".no_tasks_dir") {
				if strings.Contains(f.Message, "exit status") {
					t.Errorf("no_tasks_dir finding should not contain bare exit status, got: %s", f.Message)
				}
			}
		}
	}
}

func TestDoctor_RepoErrDetail_IgnoresParenthesesInPath(t *testing.T) {
	allOK(t)

	cc := &checkContext{
		repoInput: "/tmp/foo(bar)",
		repoErr:   fmt.Errorf("resolve repo root: git -C /tmp/foo(bar) rev-parse --show-toplevel: exit status 128 (fatal: not a git repository: /tmp/foo(bar))"),
	}

	got := cc.repoErrDetail()
	want := "fatal: not a git repository: /tmp/foo(bar)"
	if got != want {
		t.Fatalf("repoErrDetail() = %q, want %q", got, want)
	}
}

func TestDoctor_UnreadableLocksDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-based test not reliable on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("cannot test permission errors as root")
	}

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	locksDir := filepath.Join(tasksDir, ".locks")

	// Make .locks unreadable.
	if err := os.Chmod(locksDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(locksDir, 0o755) })

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Should have a locks.unreadable finding.
	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.unreadable" {
				found = true
				if f.Severity != SeverityError {
					t.Errorf("expected severity error, got %s", f.Severity)
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.unreadable finding when .locks is not readable")
	}
}

func TestDoctor_ToolsMissingCopilotDir(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: ".copilot", Path: "", Required: true, Found: false, Message: ".copilot directory not found"},
			{Name: "git", Path: "/usr/bin/git", Required: true, Found: true, Message: "git: /usr/bin/git"},
		}}
	})
	stubDocker(t, func(ctx context.Context) error { return nil })

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var foundCopilot, foundGit bool
	for _, cr := range report.Checks {
		if cr.Name != "tools" {
			continue
		}
		for _, f := range cr.Findings {
			switch f.Code {
			case "tools.missing_copilot_dir":
				foundCopilot = true
				if f.Severity != SeverityError {
					t.Errorf("missing_copilot_dir severity: got %q, want %q", f.Severity, SeverityError)
				}
			case "tools.found":
				if f.Message == "git: /usr/bin/git" {
					foundGit = true
					if f.Severity != SeverityInfo {
						t.Errorf("found tool severity: got %q, want %q", f.Severity, SeverityInfo)
					}
				}
			}
		}
	}
	if !foundCopilot {
		t.Error("expected tools.missing_copilot_dir finding for missing .copilot")
	}
	if !foundGit {
		t.Error("expected tools.found finding for git")
	}
}

func TestDoctor_ToolsMissingOptionalAndRequired(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: "copilot", Path: "", Required: true, Found: false, Message: "copilot not found"},
			{Name: "gh", Path: "", Required: false, Found: false, Message: "gh not found"},
			{Name: "git", Path: "/usr/bin/git", Required: true, Found: true, Message: "git: /usr/bin/git"},
		}}
	})
	stubDocker(t, func(ctx context.Context) error { return nil })

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var foundRequired, foundOptional, foundInfo bool
	for _, cr := range report.Checks {
		if cr.Name != "tools" {
			continue
		}
		for _, f := range cr.Findings {
			switch f.Code {
			case "tools.missing_required":
				foundRequired = true
				if f.Severity != SeverityError {
					t.Errorf("missing_required severity: got %q, want %q", f.Severity, SeverityError)
				}
			case "tools.missing_optional":
				foundOptional = true
				if f.Severity != SeverityWarning {
					t.Errorf("missing_optional severity: got %q, want %q", f.Severity, SeverityWarning)
				}
			case "tools.found":
				foundInfo = true
				if f.Severity != SeverityInfo {
					t.Errorf("found tool severity: got %q, want %q", f.Severity, SeverityInfo)
				}
			}
		}
	}
	if !foundRequired {
		t.Error("expected tools.missing_required finding for missing copilot")
	}
	if !foundOptional {
		t.Error("expected tools.missing_optional finding for missing gh")
	}
	if !foundInfo {
		t.Error("expected tools.found finding for git")
	}
}

func TestDoctor_ToolsAllFound(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	stubTools(t, func() runner.ToolReport {
		return runner.ToolReport{Findings: []runner.ToolFinding{
			{Name: "copilot", Path: "/usr/bin/copilot", Required: true, Found: true, Message: "copilot: /usr/bin/copilot"},
			{Name: "git", Path: "/usr/bin/git", Required: true, Found: true, Message: "git: /usr/bin/git"},
			{Name: "gh", Path: "/usr/bin/gh", Required: false, Found: true, Message: "gh: /usr/bin/gh"},
		}}
	})
	stubDocker(t, func(ctx context.Context) error { return nil })

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		if cr.Name != "tools" {
			continue
		}
		for _, f := range cr.Findings {
			if f.Code != "tools.found" {
				t.Errorf("expected all findings to be tools.found, got %q", f.Code)
			}
			if f.Severity != SeverityInfo {
				t.Errorf("expected info severity for found tools, got %q for %q", f.Severity, f.Message)
			}
		}
	}
}
