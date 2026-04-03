package queue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/taskfile"
)

func TestCancelTask_MovesTaskToFailed(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{name: "waiting", state: DirWaiting},
		{name: "backlog", state: DirBacklog},
		{name: "in progress", state: DirInProgress},
		{name: "ready for review", state: DirReadyReview},
		{name: "ready to merge", state: DirReadyMerge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := setupIndexDirs(t)
			writeTask(t, tasksDir, tt.state, "task.md", "---\nid: task\n---\n# Task\n")

			result, err := CancelTask(tasksDir, "task")
			if err != nil {
				t.Fatalf("CancelTask: %v", err)
			}
			if result.Filename != "task.md" || result.PriorState != tt.state {
				t.Fatalf("unexpected result: %+v", result)
			}
			if _, err := os.Stat(filepath.Join(tasksDir, tt.state, "task.md")); !os.IsNotExist(err) {
				t.Fatalf("source file still present in %s/: %v", tt.state, err)
			}
			data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "task.md"))
			if err != nil {
				t.Fatalf("read failed task: %v", err)
			}
			if !taskfile.ContainsCancelledMarker(data) {
				t.Fatalf("cancelled marker missing from failed task: %s", data)
			}
		})
	}
}

func TestCancelTask_AlreadyFailed(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirFailed, "task.md", "---\nid: task\n---\n# Task\n")

	result, err := CancelTask(tasksDir, "task")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if result.PriorState != DirFailed {
		t.Fatalf("PriorState = %q, want %q", result.PriorState, DirFailed)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "task.md"))
	if err != nil {
		t.Fatalf("read failed task: %v", err)
	}
	if strings.Count(string(data), "<!-- cancelled:") != 1 {
		t.Fatalf("expected one cancelled marker, got:\n%s", data)
	}
}

func TestCancelTask_ReCancelFailedTask(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirFailed, "task.md", "---\nid: task\n---\n# Task\n")

	if _, err := CancelTask(tasksDir, "task"); err != nil {
		t.Fatalf("first CancelTask: %v", err)
	}
	if _, err := CancelTask(tasksDir, "task"); err != nil {
		t.Fatalf("second CancelTask: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "task.md"))
	if err != nil {
		t.Fatalf("read failed task: %v", err)
	}
	if strings.Count(string(data), "<!-- cancelled:") != 2 {
		t.Fatalf("expected two cancelled markers, got:\n%s", data)
	}
}

func TestCancelTask_ParseFailedTask(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirWaiting, "broken.md", "---\npriority: nope\n---\n")

	result, err := CancelTask(tasksDir, "broken")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if result.PriorState != DirWaiting {
		t.Fatalf("PriorState = %q, want %q", result.PriorState, DirWaiting)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "broken.md"))
	if err != nil {
		t.Fatalf("read failed task: %v", err)
	}
	if !taskfile.ContainsCancelledMarker(data) {
		t.Fatalf("cancelled marker missing from parse-failed task: %s", data)
	}
}

func TestCancelTask_CompletedRefused(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirCompleted, "done.md", "---\nid: done\n---\n# Done\n")

	_, err := CancelTask(tasksDir, "done")
	if err == nil || !strings.Contains(err.Error(), "cannot cancel done: task has already been merged") {
		t.Fatalf("err = %v, want completed-task refusal", err)
	}
}

func TestCancelTask_DestinationCollision(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task.md", "---\nid: unique/source\n---\n# Source\n")
	writeTask(t, tasksDir, DirFailed, "task.md", "---\nid: other\n---\n# Existing\n")

	_, err := CancelTask(tasksDir, "unique/source")
	if err == nil || !strings.Contains(err.Error(), "cannot cancel task: already exists in failed/") {
		t.Fatalf("err = %v, want destination-collision error", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task.md")); err != nil {
		t.Fatalf("source task should remain in backlog: %v", err)
	}
}

func TestCancelTask_AppendFailsAfterMove(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task.md", "---\nid: task\n---\n# Task\n")

	origAppend := appendCancelledRecordFn
	appendCancelledRecordFn = func(path string) error { return fmt.Errorf("append failed") }
	defer func() { appendCancelledRecordFn = origAppend }()

	_, err := CancelTask(tasksDir, "task")
	if err == nil || !strings.Contains(err.Error(), "rolled back to backlog/") {
		t.Fatalf("err = %v, want rollback error", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task.md")); err != nil {
		t.Fatalf("task should be rolled back to backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "task.md")); !os.IsNotExist(err) {
		t.Fatalf("failed copy should be removed after rollback: %v", err)
	}
}

func TestCancelTask_AppendFailsAfterMoveRollbackFails(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task.md", "---\nid: task\n---\n# Task\n")

	origAppend := appendCancelledRecordFn
	origLink := linkFn
	appendCancelledRecordFn = func(path string) error { return fmt.Errorf("append failed") }
	calls := 0
	linkFn = func(src, dst string) error {
		calls++
		if calls == 2 {
			return fmt.Errorf("rollback link failed")
		}
		return os.Link(src, dst)
	}
	defer func() {
		appendCancelledRecordFn = origAppend
		linkFn = origLink
	}()

	stderr := captureStderr(t, func() {
		_, err := CancelTask(tasksDir, "task")
		if err == nil || !strings.Contains(err.Error(), "rollback failed") {
			t.Fatalf("err = %v, want rollback failure", err)
		}
	})
	if !strings.Contains(stderr, "rollback to backlog/ also failed") {
		t.Fatalf("stderr missing rollback warning: %q", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "task.md"))
	if err != nil {
		t.Fatalf("task should remain in failed after rollback failure: %v", err)
	}
	if taskfile.ContainsCancelledMarker(data) {
		t.Fatalf("task should not have cancelled marker after append failure: %s", data)
	}
}

func TestCancelTask_MoveLeavesDuplicateCopies(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task.md", "---\nid: task\n---\n# Task\n")

	origRemove := removeFn
	removeCalls := 0
	removeFn = func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return fmt.Errorf("remove source failed")
		}
		return os.Remove(path)
	}
	defer func() { removeFn = origRemove }()

	_, err := CancelTask(tasksDir, "task")
	if err == nil || !strings.Contains(err.Error(), "remove source after linking") {
		t.Fatalf("err = %v, want source-removal error", err)
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, DirBacklog, "task.md")); statErr != nil {
		t.Fatalf("source task should remain after duplicate-copy detection: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, DirFailed, "task.md")); !os.IsNotExist(statErr) {
		t.Fatalf("failed copy should be cleaned up after duplicate-copy detection: %v", statErr)
	}
}

func TestCancelTask_SucceedsWithoutSourceCleanupPostCheck(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task.md", "---\nid: task\n---\n# Task\n")

	result, err := CancelTask(tasksDir, "task")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if result.Filename != "task.md" {
		t.Fatalf("Filename = %q, want %q", result.Filename, "task.md")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "task.md")); !os.IsNotExist(err) {
		t.Fatalf("backlog task should be removed after cancel, stat err = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "task.md"))
	if err != nil {
		t.Fatalf("failed task missing: %v", err)
	}
	if !taskfile.ContainsCancelledMarker(data) {
		t.Fatalf("cancelled marker missing from failed task: %s", data)
	}
}

func TestCancelTask_DownstreamWarnings(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "dep.md", "---\nid: dep-id\n---\n# Dep\n")
	writeTask(t, tasksDir, DirWaiting, "waiter.md", "---\nid: waiter\ndepends_on: [dep-id]\n---\n# Waiter\n")
	writeTask(t, tasksDir, DirBacklog, "blocked.md", "---\nid: blocked\ndepends_on: [dep]\n---\n# Blocked\n")

	result, err := CancelTask(tasksDir, "dep")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	want := []string{"backlog/blocked.md", "waiting/waiter.md"}
	if len(result.Warnings) != len(want) {
		t.Fatalf("Warnings = %v, want %v", result.Warnings, want)
	}
	for i := range want {
		if result.Warnings[i] != want[i] {
			t.Fatalf("Warnings[%d] = %q, want %q", i, result.Warnings[i], want[i])
		}
	}
}

func TestCancelTask_DownstreamDeduplication(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "dep.md", "---\nid: dep-id\n---\n# Dep\n")
	writeTask(t, tasksDir, DirWaiting, "waiter.md", "---\nid: waiter\ndepends_on: [dep, dep-id]\n---\n# Waiter\n")

	result, err := CancelTask(tasksDir, "dep")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "waiting/waiter.md" {
		t.Fatalf("Warnings = %v, want single deduplicated warning", result.Warnings)
	}
}

func TestCancelTask_NoDownstreamWarnings(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "dep.md", "---\nid: dep\n---\n# Dep\n")

	result, err := CancelTask(tasksDir, "dep")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", result.Warnings)
	}
}

func TestCancelTask_TaskNotFound(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	_, err := CancelTask(tasksDir, "missing")
	if err == nil || !strings.Contains(err.Error(), "task not found: missing") {
		t.Fatalf("err = %v, want not-found error", err)
	}
}

func TestCancelTask_EmptyName(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	_, err := CancelTask(tasksDir, "   ")
	if err == nil || !strings.Contains(err.Error(), "task name must not be empty") {
		t.Fatalf("err = %v, want empty-name error", err)
	}
}

func TestCancelTask_ExplicitIDResolution(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "filename.md", "---\nid: explicit-id\n---\n# Task\n")

	result, err := CancelTask(tasksDir, "explicit-id")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if result.Filename != "filename.md" {
		t.Fatalf("Filename = %q, want filename.md", result.Filename)
	}
}

func TestCancelTask_ExplicitIDWithSlash(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "filename.md", "---\nid: group/explicit-id\n---\n# Task\n")

	result, err := CancelTask(tasksDir, "group/explicit-id")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if result.Filename != "filename.md" {
		t.Fatalf("Filename = %q, want filename.md", result.Filename)
	}
}

func TestRetryTask_StrippedCancelledMarker(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirFailed, "task.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: task\n---\n# Task\n")

	if _, err := RetryTask(tasksDir, "task"); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, DirBacklog, "task.md"))
	if err != nil {
		t.Fatalf("read retried task: %v", err)
	}
	if taskfile.ContainsCancelledMarker(data) {
		t.Fatalf("cancelled marker should be stripped on retry: %s", data)
	}
}

func TestCancelTask_CleansVerdictAndIndexNoLongerSurfacesRejection(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	filename := "stale-verdict-cancel.md"

	writeTask(t, tasksDir, DirBacklog, filename,
		"---\nid: stale-verdict-cancel\npriority: 10\n---\n# Stale verdict cancel\n")

	// Seed a stale verdict file with a reject reason.
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := map[string]string{"verdict": "reject", "reason": "stale cancel reason"}
	vdata, _ := json.Marshal(verdict)
	verdictPath := taskfile.VerdictPath(tasksDir, filename)
	if err := os.WriteFile(verdictPath, vdata, 0o644); err != nil {
		t.Fatal(err)
	}

	// Before cancel: BuildIndex should surface the verdict rejection reason.
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(DirBacklog, filename)
	if snap == nil {
		t.Fatal("expected snapshot in backlog/ before cancel")
	}
	if snap.LastReviewRejectionReason != "stale cancel reason" {
		t.Fatalf("before cancel: LastReviewRejectionReason = %q, want %q",
			snap.LastReviewRejectionReason, "stale cancel reason")
	}

	// Cancel the task (terminal transition).
	if _, err := CancelTask(tasksDir, "stale-verdict-cancel"); err != nil {
		t.Fatalf("CancelTask() error: %v", err)
	}

	// Verdict file must be gone.
	if _, err := os.Stat(verdictPath); !os.IsNotExist(err) {
		t.Fatal("verdict file should be deleted after cancel")
	}

	// After cancel: BuildIndex must not surface a stale rejection reason.
	idx2 := BuildIndex(tasksDir)
	snap2 := idx2.Snapshot(DirFailed, filename)
	if snap2 == nil {
		t.Fatal("expected snapshot in failed/ after cancel")
	}
	if snap2.LastReviewRejectionReason != "" {
		t.Fatalf("after cancel: LastReviewRejectionReason = %q, want empty",
			snap2.LastReviewRejectionReason)
	}
}
