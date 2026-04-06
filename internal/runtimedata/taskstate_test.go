package runtimedata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/dirs"
)

func TestLoadTaskState_MissingReturnsNil(t *testing.T) {
	state, err := LoadTaskState(t.TempDir(), "missing.md")
	if err != nil {
		t.Fatalf("LoadTaskState: %v", err)
	}
	if state != nil {
		t.Fatalf("LoadTaskState = %+v, want nil", state)
	}
}

func TestUpdateTaskState_CreatesAndPreservesFields(t *testing.T) {
	tasksDir := t.TempDir()
	if err := UpdateTaskState(tasksDir, "task.md", func(state *TaskState) {
		state.TaskBranch = "task/task"
		state.LastHeadSHA = "abc123"
		state.LastOutcome = OutcomeWorkPushed
	}); err != nil {
		t.Fatalf("first UpdateTaskState: %v", err)
	}
	first, err := LoadTaskState(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("LoadTaskState after first update: %v", err)
	}
	if first == nil {
		t.Fatal("LoadTaskState returned nil state")
	}
	if first.Version != taskStateVersion {
		t.Fatalf("Version = %d, want %d", first.Version, taskStateVersion)
	}
	if first.TaskFile != "task.md" {
		t.Fatalf("TaskFile = %q, want %q", first.TaskFile, "task.md")
	}
	if first.TaskBranch != "task/task" {
		t.Fatalf("TaskBranch = %q, want %q", first.TaskBranch, "task/task")
	}
	if first.LastHeadSHA != "abc123" {
		t.Fatalf("LastHeadSHA = %q, want %q", first.LastHeadSHA, "abc123")
	}
	if first.LastOutcome != OutcomeWorkPushed {
		t.Fatalf("LastOutcome = %q, want %q", first.LastOutcome, OutcomeWorkPushed)
	}
	if strings.TrimSpace(first.UpdatedAt) == "" {
		t.Fatal("UpdatedAt should be set")
	}

	if err := UpdateTaskState(tasksDir, "task.md", func(state *TaskState) {
		state.LastReviewedSHA = "def456"
		state.LastOutcome = OutcomeReviewApproved
	}); err != nil {
		t.Fatalf("second UpdateTaskState: %v", err)
	}
	second, err := LoadTaskState(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("LoadTaskState after second update: %v", err)
	}
	if second.TaskBranch != "task/task" {
		t.Fatalf("TaskBranch = %q, want preserved value", second.TaskBranch)
	}
	if second.LastHeadSHA != "abc123" {
		t.Fatalf("LastHeadSHA = %q, want preserved value", second.LastHeadSHA)
	}
	if second.LastReviewedSHA != "def456" {
		t.Fatalf("LastReviewedSHA = %q, want %q", second.LastReviewedSHA, "def456")
	}
	if second.LastOutcome != OutcomeReviewApproved {
		t.Fatalf("LastOutcome = %q, want %q", second.LastOutcome, OutcomeReviewApproved)
	}
}

func TestLoadTaskState_CorruptJSONReturnsError(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, "runtime", "taskstate", "task.md.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state, err := LoadTaskState(tasksDir, "task.md")
	if err == nil {
		t.Fatal("LoadTaskState should fail for corrupt JSON")
	}
	if state != nil {
		t.Fatalf("LoadTaskState returned %+v, want nil on error", state)
	}
}

func TestUpdateTaskState_CorruptJSONRecreatesFreshState(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, "runtime", "taskstate", "task.md.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := UpdateTaskState(tasksDir, "task.md", func(state *TaskState) {
		state.LastOutcome = OutcomeReviewRejected
	}); err != nil {
		t.Fatalf("UpdateTaskState: %v", err)
	}
	state, err := LoadTaskState(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("LoadTaskState: %v", err)
	}
	if state.LastOutcome != OutcomeReviewRejected {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, OutcomeReviewRejected)
	}
	if state.TaskFile != "task.md" {
		t.Fatalf("TaskFile = %q, want %q", state.TaskFile, "task.md")
	}
}

func TestDeleteTaskState_RemovesFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := UpdateTaskState(tasksDir, "task.md", func(state *TaskState) {
		state.LastOutcome = "done"
	}); err != nil {
		t.Fatalf("UpdateTaskState: %v", err)
	}
	if err := DeleteTaskState(tasksDir, "task.md"); err != nil {
		t.Fatalf("DeleteTaskState: %v", err)
	}
	state, err := LoadTaskState(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("LoadTaskState after DeleteTaskState: %v", err)
	}
	if state != nil {
		t.Fatalf("LoadTaskState after DeleteTaskState = %+v, want nil", state)
	}
}

func TestSweepTaskState_RemovesTerminalStateAndKeepsActive(t *testing.T) {
	activeDirs := []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge}
	for _, activeDir := range activeDirs {
		t.Run(activeDir, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, dir := range []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge, dirs.Completed, dirs.Failed} {
				if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
					t.Fatalf("MkdirAll %s: %v", dir, err)
				}
			}
			if err := os.WriteFile(filepath.Join(tasksDir, activeDir, "active.md"), []byte("# Active\n"), 0o644); err != nil {
				t.Fatalf("WriteFile active: %v", err)
			}
			if err := os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "done.md"), []byte("# Done\n"), 0o644); err != nil {
				t.Fatalf("WriteFile done: %v", err)
			}
			if err := os.WriteFile(filepath.Join(tasksDir, dirs.Failed, "failed.md"), []byte("# Failed\n"), 0o644); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
			for _, name := range []string{"active.md", "done.md", "failed.md", "gone.md"} {
				if err := UpdateTaskState(tasksDir, name, func(state *TaskState) {
					state.LastOutcome = name
				}); err != nil {
					t.Fatalf("UpdateTaskState %s: %v", name, err)
				}
			}

			if err := SweepTaskState(tasksDir); err != nil {
				t.Fatalf("SweepTaskState: %v", err)
			}
			for _, name := range []string{"done.md", "failed.md", "gone.md"} {
				state, err := LoadTaskState(tasksDir, name)
				if err != nil {
					t.Fatalf("LoadTaskState %s: %v", name, err)
				}
				if state != nil {
					t.Fatalf("LoadTaskState(%s) = %+v, want nil after sweep", name, state)
				}
			}
			active, err := LoadTaskState(tasksDir, "active.md")
			if err != nil {
				t.Fatalf("LoadTaskState active: %v", err)
			}
			if active == nil {
				t.Fatalf("active taskstate should be preserved for %s", activeDir)
			}
		})
	}
}

func TestSweepTaskState_PreservesStateWhenActiveStateUnknown(t *testing.T) {
	tasksDir := t.TempDir()
	for _, dir := range []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge, dirs.Completed, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	activeDir := filepath.Join(tasksDir, dirs.InProgress)
	if err := os.WriteFile(filepath.Join(activeDir, "active.md"), []byte("# Active\n"), 0o644); err != nil {
		t.Fatalf("WriteFile active: %v", err)
	}
	if err := UpdateTaskState(tasksDir, "active.md", func(state *TaskState) {
		state.LastOutcome = OutcomeReviewLaunched
	}); err != nil {
		t.Fatalf("UpdateTaskState active: %v", err)
	}
	if err := os.Chmod(activeDir, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(activeDir, 0o755)
	})

	err := SweepTaskState(tasksDir)
	if err == nil {
		t.Fatal("SweepTaskState error = nil, want activity check failure")
	}
	if !strings.Contains(err.Error(), "check task activity for active.md") {
		t.Fatalf("SweepTaskState error = %v, want activity warning", err)
	}
	state, loadErr := LoadTaskState(tasksDir, "active.md")
	if loadErr != nil {
		t.Fatalf("LoadTaskState active: %v", loadErr)
	}
	if state == nil {
		t.Fatal("active taskstate should remain when liveness is unknown")
	}
}

func TestSweepTaskState_PreservesStateWhenActiveDirectoryMissing(t *testing.T) {
	tasksDir := t.TempDir()
	for _, dir := range []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge, dirs.Completed, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}
	activeDir := filepath.Join(tasksDir, dirs.InProgress)
	if err := os.WriteFile(filepath.Join(activeDir, "active.md"), []byte("# Active\n"), 0o644); err != nil {
		t.Fatalf("WriteFile active: %v", err)
	}
	if err := UpdateTaskState(tasksDir, "active.md", func(state *TaskState) {
		state.LastOutcome = OutcomeReviewLaunched
	}); err != nil {
		t.Fatalf("UpdateTaskState active: %v", err)
	}
	if err := os.RemoveAll(activeDir); err != nil {
		t.Fatalf("RemoveAll active dir: %v", err)
	}

	err := SweepTaskState(tasksDir)
	if err == nil {
		t.Fatal("SweepTaskState error = nil, want missing directory failure")
	}
	if !strings.Contains(err.Error(), "check task activity for active.md") {
		t.Fatalf("SweepTaskState error = %v, want activity warning", err)
	}
	state, loadErr := LoadTaskState(tasksDir, "active.md")
	if loadErr != nil {
		t.Fatalf("LoadTaskState active: %v", loadErr)
	}
	if state == nil {
		t.Fatal("active taskstate should remain when active directory is missing")
	}
}

func TestUpdateTaskState_EmptyTasksDirFails(t *testing.T) {
	err := UpdateTaskState("", "task.md", func(state *TaskState) {
		state.LastOutcome = OutcomeReviewLaunched
	})
	if err == nil {
		t.Fatal("UpdateTaskState should fail for empty tasksDir")
	}
	if !strings.Contains(err.Error(), "tasks directory must not be empty") {
		t.Fatalf("UpdateTaskState error = %v, want empty tasksDir error", err)
	}
}

func TestLoadTaskState_InvalidTaskFilenameFails(t *testing.T) {
	state, err := LoadTaskState(t.TempDir(), "../escape.md")
	if err == nil {
		t.Fatal("LoadTaskState should fail for invalid task filename")
	}
	if state != nil {
		t.Fatalf("LoadTaskState returned %+v, want nil on error", state)
	}
}

func TestLoadTaskState_EmptyTaskFilenameFails(t *testing.T) {
	state, err := LoadTaskState(t.TempDir(), "")
	if err == nil {
		t.Fatal("LoadTaskState should fail for empty task filename")
	}
	if state != nil {
		t.Fatalf("LoadTaskState returned %+v, want nil on error", state)
	}
}

func TestDeleteTaskState_MissingFileReturnsNil(t *testing.T) {
	if err := DeleteTaskState(t.TempDir(), "missing.md"); err != nil {
		t.Fatalf("DeleteTaskState missing file: %v", err)
	}
}

func TestOutcomeConstants_CoverFullLifecycle(t *testing.T) {
	tests := []struct {
		name     string
		constant string
		wire     string
	}{
		{"work-launched", OutcomeWorkLaunched, "work-launched"},
		{"work-branch-pushed", OutcomeWorkBranchPushed, "work-branch-pushed"},
		{"work-pushed", OutcomeWorkPushed, "work-pushed"},
		{"review-launched", OutcomeReviewLaunched, "review-launched"},
		{"review-approved", OutcomeReviewApproved, "review-approved"},
		{"review-rejected", OutcomeReviewRejected, "review-rejected"},
		{"review-error", OutcomeReviewError, "review-error"},
		{"review-incomplete", OutcomeReviewIncomplete, "review-incomplete"},
		{"review-branch-missing", OutcomeReviewBranchMissing, "review-branch-missing"},
		{"review-branch-marker-missing", OutcomeReviewBranchMarkerMissing, "review-branch-marker-missing"},
		{"review-move-failed", OutcomeReviewMoveFailed, "review-move-failed"},
		{"merge-conflict-cleanup", OutcomeMergeConflictCleanup, "merge-conflict-cleanup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.constant != tt.wire {
				t.Fatalf("Outcome constant %q != expected wire format %q", tt.constant, tt.wire)
			}
		})
	}
}

func TestOutcomeConstants_RoundTripThroughJSON(t *testing.T) {
	tasksDir := t.TempDir()
	outcomes := []string{
		OutcomeWorkLaunched,
		OutcomeWorkBranchPushed,
		OutcomeWorkPushed,
		OutcomeReviewLaunched,
		OutcomeReviewApproved,
		OutcomeReviewRejected,
		OutcomeReviewError,
		OutcomeReviewIncomplete,
		OutcomeReviewBranchMissing,
		OutcomeReviewBranchMarkerMissing,
		OutcomeReviewMoveFailed,
		OutcomeMergeConflictCleanup,
	}
	for _, outcome := range outcomes {
		t.Run(outcome, func(t *testing.T) {
			filename := strings.ReplaceAll(outcome, "-", "") + ".md"
			if err := UpdateTaskState(tasksDir, filename, func(state *TaskState) {
				state.LastOutcome = outcome
			}); err != nil {
				t.Fatalf("UpdateTaskState: %v", err)
			}
			loaded, err := LoadTaskState(tasksDir, filename)
			if err != nil {
				t.Fatalf("LoadTaskState: %v", err)
			}
			if loaded.LastOutcome != outcome {
				t.Fatalf("LastOutcome = %q, want %q", loaded.LastOutcome, outcome)
			}
		})
	}
}
