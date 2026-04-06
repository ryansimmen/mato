package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mato/internal/doctor"
	"mato/internal/runner"
	"mato/internal/runtimedata"
	"mato/internal/testutil"
)

func TestDoctor_Integration_FixCycle(t *testing.T) {
	// Stub tools and docker to avoid external dependencies.
	origTools := doctor.ExportInspectHostToolsFn()
	doctor.SetInspectHostToolsFn(func() runner.ToolReport {
		return runner.ToolReport{}
	})
	t.Cleanup(func() { doctor.SetInspectHostToolsFn(origTools) })

	origLookPath := doctor.ExportDockerLookPathFn()
	doctor.SetDockerLookPathFn(func() error { return nil })
	t.Cleanup(func() { doctor.SetDockerLookPathFn(origLookPath) })

	origDocker := doctor.ExportDockerProbe()
	doctor.SetDockerProbe(func(ctx context.Context) error { return nil })
	t.Cleanup(func() { doctor.SetDockerProbe(origDocker) })

	origImageInspect := doctor.ExportDockerImageInspectFn()
	doctor.SetDockerImageInspectFn(func(ctx context.Context, image string) error { return nil })
	t.Cleanup(func() { doctor.SetDockerImageInspectFn(origImageInspect) })

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Create a stale PID lock file.
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "deadbeef.pid"), "999999:0")

	// Create an orphaned in-progress task (no live agent).
	testutil.WriteFile(t, filepath.Join(tasksDir, "in-progress", "orphan.md"),
		"<!-- claimed-by: deadbeef -->\n---\nid: orphan\n---\nOrphan task\n")
	if err := runtimedata.UpdateTaskState(tasksDir, "orphan.md", func(state *runtimedata.TaskState) {
		state.TaskBranch = "task/orphan"
		state.LastOutcome = runtimedata.OutcomeWorkLaunched
	}); err != nil {
		t.Fatalf("seed work-launched taskstate: %v", err)
	}

	// Step 1: Run without --fix. Expect warnings (exit code 1).
	report1, err := doctor.Run(context.Background(), repoRoot, doctor.Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run without --fix: %v", err)
	}
	if report1.ExitCode == 0 {
		t.Error("expected non-zero exit code without --fix")
	}

	// Verify findings are present.
	hasStalePID := false
	hasOrphan := false
	for _, cr := range report1.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_pid" && f.Fixable {
				hasStalePID = true
			}
			if f.Code == "locks.orphaned_task" && f.Fixable {
				hasOrphan = true
			}
		}
	}
	if !hasStalePID {
		t.Error("expected locks.stale_pid finding")
	}
	if !hasOrphan {
		t.Error("expected locks.orphaned_task finding")
	}

	// Step 2: Run with --fix. Expect repairs.
	report2, err := doctor.Run(context.Background(), repoRoot, doctor.Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run with --fix: %v", err)
	}

	// Verify filesystem was repaired.
	if _, statErr := os.Stat(filepath.Join(tasksDir, ".locks", "deadbeef.pid")); !os.IsNotExist(statErr) {
		t.Error("expected stale PID lock to be removed")
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, "in-progress", "orphan.md")); !os.IsNotExist(statErr) {
		t.Error("expected orphan.md to be moved from in-progress")
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, "backlog", "orphan.md")); statErr != nil {
		t.Error("expected orphan.md in backlog/ after fix")
	}

	// Verify findings are marked fixed.
	for _, cr := range report2.Checks {
		for _, f := range cr.Findings {
			if (f.Code == "locks.stale_pid" || f.Code == "locks.orphaned_task") && !f.Fixed {
				t.Errorf("expected %s finding to be marked fixed", f.Code)
			}
		}
	}

	// Step 3: Run again. Everything should be clean (exit 0).
	report3, err := doctor.Run(context.Background(), repoRoot, doctor.Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run post-fix: %v", err)
	}
	if report3.ExitCode != 0 {
		t.Errorf("expected exit code 0 post-fix, got %d", report3.ExitCode)
	}
}
