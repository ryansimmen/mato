package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/dirs"
	"mato/internal/pause"
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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
	if err := os.Chtimes(eventFile, old, old); err != nil {
		t.Fatalf("os.Chtimes(%s): %v", eventFile, err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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
	if err := os.Chtimes(eventFile, old, old); err != nil {
		t.Fatalf("os.Chtimes(%s): %v", eventFile, err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
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
	if err := os.Chtimes(nonJSON, old, old); err != nil {
		t.Fatalf("os.Chtimes(%s): %v", nonJSON, err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

func TestScanPauseSentinel(t *testing.T) {
	tasksDir := t.TempDir()
	tests := []struct {
		name     string
		readFn   func(string) (pause.State, error)
		wantCode string
	}{
		{name: "not paused", readFn: func(string) (pause.State, error) { return pause.State{}, nil }},
		{name: "recently paused", readFn: func(string) (pause.State, error) {
			return pause.State{Active: true, Since: time.Now().UTC().Add(-2 * time.Hour)}, nil
		}},
		{name: "paused too long", readFn: func(string) (pause.State, error) {
			return pause.State{Active: true, Since: time.Now().UTC().Add(-48 * time.Hour)}, nil
		}, wantCode: "hygiene.paused"},
		{name: "malformed", readFn: func(string) (pause.State, error) {
			return pause.State{Active: true, ProblemKind: pause.ProblemMalformed, Problem: `invalid timestamp: "bad"`}, nil
		}, wantCode: "hygiene.invalid_pause_file"},
		{name: "unreadable", readFn: func(string) (pause.State, error) {
			return pause.State{Active: true, ProblemKind: pause.ProblemUnreadable, Problem: "unreadable: boom"}, nil
		}, wantCode: "hygiene.pause_unreadable"},
		{name: "hard error", readFn: func(string) (pause.State, error) { return pause.State{}, fmt.Errorf("stat boom") }, wantCode: "hygiene.pause_unreadable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := scanPauseSentinel(tasksDir, tt.readFn)
			if tt.wantCode == "" {
				if len(findings) != 0 {
					t.Fatalf("unexpected findings: %#v", findings)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %#v, want 1", findings)
			}
			if findings[0].Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", findings[0].Code, tt.wantCode)
			}
			if findings[0].Fixable {
				t.Fatal("pause findings should not be fixable")
			}
		})
	}
}

func TestScanPauseSentinel_StaleThresholdUsesRawAge(t *testing.T) {
	tasksDir := t.TempDir()
	findings := scanPauseSentinel(tasksDir, func(string) (pause.State, error) {
		return pause.State{Active: true, Since: time.Now().UTC().Add(-(24*time.Hour + 20*time.Minute))}, nil
	})
	if len(findings) != 1 {
		t.Fatalf("findings = %#v, want 1 stale finding", findings)
	}
	if findings[0].Code != "hygiene.paused" {
		t.Fatalf("code = %q, want hygiene.paused", findings[0].Code)
	}
}

// ---------- Stale Merge Lock ----------

func TestDoctor_StaleMergeLock_NoLock(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// No merge.lock should produce no finding.
	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

func TestDoctor_StaleMergeLock_Unreadable(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	lockPath := filepath.Join(tasksDir, ".locks", "merge.lock")
	testutil.MakeUnreadablePath(t, lockPath)

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	foundUnreadable := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" {
				t.Fatalf("unreadable merge.lock must not be reported as stale: %+v", f)
			}
			if f.Code == "hygiene.merge_lock_unreadable" {
				foundUnreadable = true
				if f.Path != lockPath {
					t.Fatalf("path = %q, want %q", f.Path, lockPath)
				}
				if !strings.HasPrefix(f.Message, "cannot read merge.lock: ") {
					t.Fatalf("message = %q, want unreadable error prefix", f.Message)
				}
				if len(f.Message) <= len("cannot read merge.lock: ") {
					t.Fatalf("message = %q, want underlying error detail", f.Message)
				}
			}
		}
	}
	if !foundUnreadable {
		t.Fatal("expected hygiene.merge_lock_unreadable finding for unreadable merge.lock")
	}
}

func TestDoctor_UnreadableReviewLock_NotFixedByFix(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	lockPath := filepath.Join(tasksDir, ".locks", "review-test.md.lock")
	testutil.MakeUnreadablePath(t, lockPath)

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Lstat(lockPath); err != nil {
		t.Fatalf("expected unreadable review lock to remain after --fix: %v", err)
	}

	foundUnreadable := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_review" && f.Path == lockPath {
				t.Fatalf("unreadable review lock must not be reported as stale: %+v", f)
			}
			if f.Code == "locks.unreadable_review" {
				foundUnreadable = true
				if f.Path != lockPath {
					t.Fatalf("path = %q, want %q", f.Path, lockPath)
				}
				if f.Fixable {
					t.Fatal("unreadable review lock should not be fixable")
				}
				if f.Fixed {
					t.Fatal("unreadable review lock should not be marked fixed")
				}
				if !strings.HasPrefix(f.Message, "unreadable review lock: review-test.md.lock: ") {
					t.Fatalf("message = %q, want unreadable review lock prefix", f.Message)
				}
			}
		}
	}
	if !foundUnreadable {
		t.Fatal("expected locks.unreadable_review finding for unreadable review lock")
	}
}

func TestDoctor_DoesNotTreatUnreadableAgentLockAsStale(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	lockPath := filepath.Join(tasksDir, ".locks", "liveagent.pid")
	testutil.MakeUnreadablePath(t, lockPath)

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_pid" && strings.Contains(f.Path, "liveagent.pid") {
				t.Fatalf("unreadable live lock must not be reported as stale: %+v", f)
			}
		}
	}
	foundUnreadable := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.unreadable_pid" && strings.Contains(f.Path, "liveagent.pid") {
				foundUnreadable = true
			}
		}
	}
	if !foundUnreadable {
		t.Fatal("expected locks.unreadable_pid finding for unreadable live lock")
	}
}

// ---------- Fix Removal Failure ----------

func TestDoctor_StaleMergeLock_Fix_RemoveError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root to trigger permission error")
	}
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	locksDir := filepath.Join(tasksDir, ".locks")
	lockFile := filepath.Join(locksDir, "merge.lock")
	testutil.WriteFile(t, lockFile, "999999:0")

	// Make locks dir read-only so os.Remove fails with permission denied.
	if err := os.Chmod(locksDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locksDir, 0o755); err != nil {
			t.Errorf("os.Chmod restore permissions: %v", err)
		}
	})

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.stale_merge_lock" {
				found = true
				if f.Fixed {
					t.Error("expected Fixed=false when removal fails")
				}
				if !strings.Contains(f.Message, "fix failed:") {
					t.Errorf("expected 'fix failed:' in message, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.stale_merge_lock finding")
	}
}

func TestDoctor_StaleReviewLock_Fix_RemoveError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root to trigger permission error")
	}
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	locksDir := filepath.Join(tasksDir, ".locks")
	lockFile := filepath.Join(locksDir, "review-test.lock")
	// Write a lock file with a dead PID so it's considered stale.
	testutil.WriteFile(t, lockFile, "999999:0")

	// Make locks dir read-only so os.Remove fails with permission denied.
	if err := os.Chmod(locksDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locksDir, 0o755); err != nil {
			t.Errorf("os.Chmod restore permissions: %v", err)
		}
	})

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_review" {
				found = true
				if f.Fixed {
					t.Error("expected Fixed=false when removal fails")
				}
				if !strings.Contains(f.Message, "fix failed:") {
					t.Errorf("expected 'fix failed:' in message, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_review finding")
	}
}

func TestDoctor_StalePIDLock_Fix_RemoveError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root to trigger permission error")
	}
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	locksDir := filepath.Join(tasksDir, ".locks")
	lockFile := filepath.Join(locksDir, "deadbeef.pid")
	testutil.WriteFile(t, lockFile, "999999:0")

	if err := os.Chmod(locksDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locksDir, 0o755); err != nil {
			t.Errorf("os.Chmod restore permissions: %v", err)
		}
	})

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "locks.stale_pid" && strings.Contains(f.Path, "deadbeef.pid") {
				found = true
				if f.Fixed {
					t.Error("expected Fixed=false when removal fails")
				}
				if !strings.Contains(f.Message, "fix failed:") {
					t.Errorf("expected 'fix failed:' in message, got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected locks.stale_pid finding")
	}
}

// ---------- Leftover Temp Files ----------

func TestDoctor_LeftoverTempFiles_Clean(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// No temp files → no finding.
	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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
	if err := os.Chtimes(tmpFile, old, old); err != nil {
		t.Fatalf("os.Chtimes(%s): %v", tmpFile, err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
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

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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

// ---------- Cross-Device (xdev) Leftover Temp Files ----------

func TestDoctor_LeftoverXdevFiles_Detected(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create a leftover cross-device temp file in a queue directory.
	xdevFile := filepath.Join(tasksDir, "backlog", ".fix-bug.md.xdev-789012")
	testutil.WriteFile(t, xdevFile, "partial xdev write")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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
		t.Error("expected hygiene.leftover_temp_files finding for xdev file")
	}
}

func TestDoctor_LeftoverXdevFiles_MixedWithTmp(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create both .tmp- and .xdev- leftover files.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", ".task.md.tmp-111"), "tmp data")
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", ".task.md.xdev-222"), "xdev data")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				found = true
				if !strings.Contains(f.Message, "2 leftover") {
					t.Errorf("expected 2 temp files (tmp + xdev), got: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.leftover_temp_files finding")
	}
}

func TestDoctor_LeftoverXdevFiles_Fix_OldFiles(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	xdevFile := filepath.Join(tasksDir, "backlog", ".task.md.xdev-999")
	testutil.WriteFile(t, xdevFile, "partial xdev write")
	// Backdate beyond the 1-hour threshold.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(xdevFile, old, old); err != nil {
		t.Fatalf("os.Chtimes(%s): %v", xdevFile, err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(xdevFile); !os.IsNotExist(statErr) {
		t.Error("expected old xdev temp file to be removed by --fix")
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
		t.Error("expected leftover_temp_files to be marked fixed for xdev files")
	}
}

func TestDoctor_LeftoverXdevFiles_InMessageDir(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create xdev temp file in a messaging directory.
	testutil.WriteFile(t, filepath.Join(tasksDir, "messages", "events", ".msg.json.xdev-333"), "xdev data")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				found = true
				if !strings.Contains(f.Message, "1 leftover") {
					t.Errorf("unexpected message: %s", f.Message)
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.leftover_temp_files finding for xdev file in message dir")
	}
}

// ---------- Retry temp file leftovers ----------

func TestDoctor_LeftoverRetryFiles_Detected(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Create a leftover retry temp file in the backlog directory.
	retryFile := filepath.Join(tasksDir, "backlog", ".fix-bug.md.retry-123456")
	testutil.WriteFile(t, retryFile, "partial retry write")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text"})
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
		t.Error("expected hygiene.leftover_temp_files finding for retry temp file")
	}
}

func TestDoctor_LeftoverRetryFiles_Fix_OldFiles(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	retryFile := filepath.Join(tasksDir, "backlog", ".task.md.retry-999")
	testutil.WriteFile(t, retryFile, "partial retry write")
	// Backdate beyond the 1-hour threshold.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(retryFile, old, old); err != nil {
		t.Fatalf("os.Chtimes(%s): %v", retryFile, err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(retryFile); !os.IsNotExist(statErr) {
		t.Error("expected old retry temp file to be removed by --fix")
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
		t.Error("expected leftover_temp_files to be marked fixed for retry files")
	}
}

func TestDoctor_LeftoverRetryFiles_Fix_RecentNotRemoved(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	retryFile := filepath.Join(tasksDir, "backlog", ".task.md.retry-recent")
	testutil.WriteFile(t, retryFile, "in progress retry")

	report, err := Run(context.Background(), repoRoot, Options{Fix: true, Format: "text"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, statErr := os.Stat(retryFile); os.IsNotExist(statErr) {
		t.Error("recent retry temp file should not be removed by --fix")
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "hygiene.leftover_temp_files" {
				found = true
				if f.Fixed {
					t.Error("expected leftover_temp_files NOT to be marked fixed (recent retry file kept)")
				}
			}
		}
	}
	if !found {
		t.Error("expected hygiene.leftover_temp_files finding")
	}
}

func TestIsTempFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{".task.md.tmp-123456", true},
		{".task.md.xdev-789012", true},
		{".task.md.retry-345678", true},
		{".msg.json.tmp-222", true},
		{".msg.json.xdev-333", true},
		{".msg.json.retry-444", true},
		{"real-task.md", false},
		{".hidden-file", false},
		{"tmp-not-dotted", false},
		{"xdev-not-dotted", false},
		{"retry-not-dotted", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTempFile(tt.name); got != tt.want {
				t.Errorf("isTempFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// ---------- Hygiene check with --only filter ----------

func TestDoctor_HygieneOnlyFilter(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{
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
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	report, err := Run(context.Background(), repoRoot, Options{
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
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("No such image: %s", image)
	})

	report, err := Run(context.Background(), repoRoot, Options{
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
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("No such image: %s", image)
	})
	stubDockerImagePull(t, func(ctx context.Context, image string) error {
		return nil
	})

	report, err := Run(context.Background(), repoRoot, Options{
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
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("No such image: %s", image)
	})
	stubDockerImagePull(t, func(ctx context.Context, image string) error {
		return fmt.Errorf("network timeout")
	})

	report, err := Run(context.Background(), repoRoot, Options{
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

func TestDoctor_DockerImage_FromOptions(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Override the inspect stub to record which image was inspected.
	var inspectedImage string
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		inspectedImage = image
		return nil
	})

	report, err := Run(context.Background(), repoRoot, Options{
		Format:      "text",
		Only:        []string{"docker"},
		DockerImage: "my-custom:image",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if inspectedImage != "my-custom:image" {
		t.Errorf("inspected image = %q, want %q", inspectedImage, "my-custom:image")
	}

	// Verify the finding message mentions the custom image.
	foundCustom := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.image_available" && strings.Contains(f.Message, "my-custom:image") {
				foundCustom = true
			}
		}
	}
	if !foundCustom {
		t.Error("expected docker.image_available finding mentioning custom image")
	}
}

func TestDoctor_DockerImage_WhitespaceOnlyEnvFallsBackToDefault(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)
	t.Setenv("MATO_DOCKER_IMAGE", "  \t  ")

	var inspectedImage string
	stubDockerImageInspect(t, func(ctx context.Context, image string) error {
		inspectedImage = image
		return nil
	})

	report, err := Run(context.Background(), repoRoot, Options{
		Format: "text",
		Only:   []string{"docker"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if inspectedImage != "ubuntu:24.04" {
		t.Fatalf("inspected image = %q, want %q", inspectedImage, "ubuntu:24.04")
	}

	foundDefault := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "docker.image_available" && strings.Contains(f.Message, "ubuntu:24.04") {
				foundDefault = true
			}
		}
	}
	if !foundDefault {
		t.Fatal("expected docker.image_available finding mentioning default image")
	}
}

func TestDoctor_Dependencies_FlagsDependencyBlockedBacklogTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, dirs.Backlog, "blocked.md"),
		"---\nid: blocked\ndepends_on: [missing]\npriority: 10\n---\n# Blocked\n")

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"deps"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "deps.backlog_blocked" {
				found = true
				if f.Severity != SeverityWarning {
					t.Fatalf("Severity = %q, want %q", f.Severity, SeverityWarning)
				}
				if !strings.Contains(f.Message, "should be in waiting/") {
					t.Fatalf("Message = %q, want waiting/ guidance", f.Message)
				}
				if f.Path != filepath.Join(tasksDir, dirs.Backlog, "blocked.md") {
					t.Fatalf("Path = %q, want blocked backlog path", f.Path)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected deps.backlog_blocked finding")
	}
}

func TestDoctor_QueueLayout_DirIsFile_CoreQueue(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Replace the backlog directory with a regular file.
	backlogPath := filepath.Join(tasksDir, dirs.Backlog)
	if err := os.RemoveAll(backlogPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backlogPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"queue"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.not_a_directory" && strings.Contains(f.Message, dirs.Backlog) {
				found = true
				if f.Severity != SeverityError {
					t.Errorf("Severity = %q, want %q", f.Severity, SeverityError)
				}
			}
		}
	}
	if !found {
		t.Error("expected queue.not_a_directory finding for backlog path that is a file")
	}
}

func TestDoctor_QueueLayout_DirIsFile_MessagingDir(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Replace the messages/events directory with a regular file.
	eventsPath := filepath.Join(tasksDir, "messages", "events")
	if err := os.RemoveAll(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"queue"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.not_a_directory" && strings.Contains(f.Message, "messages/events") {
				found = true
				if f.Severity != SeverityError {
					t.Errorf("Severity = %q, want %q", f.Severity, SeverityError)
				}
			}
		}
	}
	if !found {
		t.Error("expected queue.not_a_directory finding for messages/events path that is a file")
	}
}

func TestDoctor_QueueLayout_UnreadableDirStatError(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	origStat := osStatFn
	osStatFn = func(name string) (os.FileInfo, error) {
		if name == filepath.Join(tasksDir, dirs.Backlog) {
			return nil, fmt.Errorf("permission denied")
		}
		return origStat(name)
	}
	defer func() { osStatFn = origStat }()

	report, err := Run(context.Background(), repoRoot, Options{Format: "text", Only: []string{"queue"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	found := false
	for _, cr := range report.Checks {
		for _, f := range cr.Findings {
			if f.Code == "queue.unreadable_dir" && strings.Contains(f.Message, dirs.Backlog) {
				found = true
				if f.Severity != SeverityError {
					t.Errorf("Severity = %q, want %q", f.Severity, SeverityError)
				}
				if !strings.Contains(f.Message, "permission denied") {
					t.Errorf("Message = %q, want permission denied detail", f.Message)
				}
			}
		}
	}
	if !found {
		t.Fatal("expected queue.unreadable_dir finding for stat failure")
	}
}
