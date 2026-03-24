package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/process"
	"mato/internal/testutil"
)

// ---------- Orphaned Message Files ----------

func TestDoctor_OrphanedMessages_Clean(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a fresh event file (within retention window).
	testutil.WriteFile(t, filepath.Join(tasksDir, "messages", "events", "recent.json"),
		`{"id":"recent","type":"progress"}`)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_events" {
				t.Errorf("unexpected stale_events finding for fresh file")
			}
		}
	}
}

func TestDoctor_OrphanedMessages_Stale(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write an event file and backdate it beyond the 24h retention window.
	eventFile := filepath.Join(tasksDir, "messages", "events", "old-event.json")
	testutil.WriteFile(t, eventFile, `{"id":"old","type":"progress"}`)
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(eventFile, old, old)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_events" {
				found = true
				if !f.Fixable {
					t.Error("expected stale_events to be fixable")
				}
				if !strings.Contains(f.Message, "1 event") {
					t.Errorf("unexpected message: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.stale_events finding")
	}
}

func TestDoctor_OrphanedMessages_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	eventFile := filepath.Join(tasksDir, "messages", "events", "old-event.json")
	testutil.WriteFile(t, eventFile, `{"id":"old","type":"progress"}`)
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(eventFile, old, old)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// File should be removed.
	if _, statErr := os.Stat(eventFile); !os.IsNotExist(statErr) {
		t.Error("expected stale event file to be removed by --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_events" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected hygiene.stale_events finding to be marked fixed")
	}
}

func TestDoctor_OrphanedMessages_NonJSONIgnored(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Non-JSON files should not be counted even if old.
	nonJSON := filepath.Join(tasksDir, "messages", "events", "readme.txt")
	testutil.WriteFile(t, nonJSON, "just a text file")
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(nonJSON, old, old)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_events" {
				t.Error("unexpected stale_events finding for non-JSON file")
			}
		}
	}
}

// ---------- Stale Merge Lock ----------

func TestDoctor_StaleMergeLock_NoLock(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// No merge.lock should produce no finding.
	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" {
				t.Error("unexpected stale_merge_lock finding when no lock exists")
			}
		}
	}
}

func TestDoctor_StaleMergeLock_DeadPID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a merge.lock with a dead PID.
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "merge.lock"), "999999:0")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" {
				found = true
				if !f.Fixable {
					t.Error("expected stale_merge_lock to be fixable")
				}
				if !strings.Contains(f.Message, "dead process") {
					t.Errorf("expected 'dead process' in message, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.stale_merge_lock finding")
	}
}

func TestDoctor_StaleMergeLock_Empty(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write an empty merge.lock.
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "merge.lock"), "")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" {
				found = true
				if !strings.Contains(f.Message, "empty") {
					t.Errorf("expected 'empty' in message, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.stale_merge_lock finding for empty lock")
	}
}

func TestDoctor_StaleMergeLock_Fix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	lockFile := filepath.Join(tasksDir, ".locks", "merge.lock")
	testutil.WriteFile(t, lockFile, "999999:0")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(lockFile); !os.IsNotExist(statErr) {
		t.Error("expected stale merge.lock to be removed by --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected hygiene.stale_merge_lock finding to be marked fixed")
	}
}

func TestDoctor_StaleMergeLock_LivePID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write merge.lock with the current (alive) process identity.
	identity := process.LockIdentity(os.Getpid())
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "merge.lock"), identity)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" {
				t.Error("unexpected stale_merge_lock finding for live process")
			}
		}
	}
}

// ---------- Leftover Temp Files ----------

func TestDoctor_LeftoverTempFiles_Clean(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// No temp files → no finding.
	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				t.Error("unexpected leftover_temp_files finding in clean repo")
			}
		}
	}
}

func TestDoctor_LeftoverTempFiles_Detected(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create a leftover temp file in a queue directory.
	tmpFile := filepath.Join(tasksDir, "backlog", ".fix-bug.md.tmp-123456")
	testutil.WriteFile(t, tmpFile, "partial write")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				found = true
				if !f.Fixable {
					t.Error("expected leftover_temp_files to be fixable")
				}
				if !strings.Contains(f.Message, "1 leftover") {
					t.Errorf("unexpected message: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.leftover_temp_files finding")
	}
}

func TestDoctor_LeftoverTempFiles_InMessageDir(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create temp files in both a queue dir and a messaging dir.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", ".task.md.tmp-111"), "data")
	testutil.WriteFile(t, filepath.Join(tasksDir, "messages", "events", ".msg.json.tmp-222"), "data")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				found = true
				if !strings.Contains(f.Message, "2 leftover") {
					t.Errorf("expected 2 temp files, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.leftover_temp_files finding")
	}
}

func TestDoctor_LeftoverTempFiles_Fix_OldFiles(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	tmpFile := filepath.Join(tasksDir, "backlog", ".task.md.tmp-999")
	testutil.WriteFile(t, tmpFile, "partial write")
	// Backdate beyond the 1-hour threshold.
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(tmpFile, old, old)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(tmpFile); !os.IsNotExist(statErr) {
		t.Error("expected old temp file to be removed by --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" && f.Fixed {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected leftover_temp_files to be marked fixed")
	}
}

func TestDoctor_LeftoverTempFiles_Fix_RecentNotRemoved(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create a very recent temp file (less than 1 hour old).
	tmpFile := filepath.Join(tasksDir, "backlog", ".task.md.tmp-recent")
	testutil.WriteFile(t, tmpFile, "in progress write")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Recent temp file should NOT be removed.
	if _, statErr := os.Stat(tmpFile); os.IsNotExist(statErr) {
		t.Error("recent temp file should not be removed by --fix")
	}

	// Finding should still be present but not fully fixed.
	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				found = true
				if f.Fixed {
					t.Error("expected leftover_temp_files NOT to be marked fixed (recent file kept)")
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.leftover_temp_files finding")
	}
}

func TestDoctor_LeftoverTempFiles_NormalFilesIgnored(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Normal files should not match the temp file pattern.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "real-task.md"), "# Real\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", ".hidden-file"), "hidden")

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				t.Error("unexpected leftover_temp_files finding for normal files")
			}
		}
	}
}

// ---------- Hygiene check with --only filter ----------

func TestDoctor_HygieneOnlyFilter(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{
		Format: "text",
		Only:   []string{"hygiene"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	hygieneRan := false
	for _, cr := range report.Checks {
		if cr.Name == "hygiene" {
			if cr.Status != CheckRan {
				t.Errorf("expected hygiene to run, got status %q", cr.Status)
			}
			hygieneRan = true
		} else if cr.Status == CheckRan {
			t.Errorf("expected check %q to be skipped when --only=hygiene", cr.Name)
		}
	}
	if !hygieneRan {
		t.Error("expected hygiene check to be present in report")
	}
}

// ---------- Docker Image Availability ----------

func TestDoctor_DockerImage_Available(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{
		Format: "text",
		Only:   []string{"docker"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundAvailable := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.image_missing" {
				t.Error("unexpected docker.image_missing when image is available")
			}
			if f.Code == "docker.image_available" {
				foundAvailable = true
			}
		}
	}
	if !foundAvailable {
		t.Error("expected docker.image_available finding")
	}
}

func TestDoctor_DockerImage_Missing(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("No such image: %s", image)
	})

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{
		Format: "text",
		Only:   []string{"docker"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.image_missing" {
				found = true
				if f.Severity != SeverityWarning {
					t.Errorf("expected warning severity, got %q", f.Severity)
				}
				if !f.Fixable {
					t.Error("expected docker.image_missing to be fixable")
				}
				if !strings.Contains(f.Message, "not found locally") {
					t.Errorf("unexpected message: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected docker.image_missing finding")
	}
}

func TestDoctor_DockerImage_Fix_PullSucceeds(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("No such image: %s", image)
	})
	stubDockerImagePull(t, func(ctx context.Context, image string) error {
		return nil
	})

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{
		Fix:    true,
		Format: "text",
		Only:   []string{"docker"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.image_missing" && f.Fixed {
				found = true
				if !strings.Contains(f.Message, "pulled successfully") {
					t.Errorf("unexpected message: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected docker.image_missing finding marked as fixed")
	}
}

func TestDoctor_DockerImage_Fix_PullFails(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("No such image: %s", image)
	})
	stubDockerImagePull(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("network timeout")
	})

	report, err := Run(context.Background(), repoRoot, tasksDir, Options{
		Fix:    true,
		Format: "text",
		Only:   []string{"docker"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.image_missing" {
				found = true
				if f.Fixed {
					t.Error("expected finding NOT to be fixed when pull fails")
				}
				if f.Severity != SeverityError {
					t.Errorf("expected error severity after failed pull, got %q", f.Severity)
				}
				if !strings.Contains(f.Message, "pull failed") {
					t.Errorf("unexpected message: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected docker.image_missing finding")
	}
}
