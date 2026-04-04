package taskstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/dirs"
)

func TestLoad_MissingReturnsNil(t *testing.T) {
	state, err := Load(t.TempDir(), "missing.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state != nil {
		t.Fatalf("Load = %+v, want nil", state)
	}
}

func TestUpdate_CreatesAndPreservesFields(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Update(tasksDir, "task.md", func(state *TaskState) {
		state.TaskBranch = "task/task"
		state.LastHeadSHA = "abc123"
		state.LastOutcome = OutcomeWorkPushed
	}); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	first, err := Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load after first update: %v", err)
	}
	if first == nil {
		t.Fatal("Load returned nil state")
	}
	if first.Version != version {
		t.Fatalf("Version = %d, want %d", first.Version, version)
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

	if err := Update(tasksDir, "task.md", func(state *TaskState) {
		state.LastReviewedSHA = "def456"
		state.LastOutcome = OutcomeReviewApproved
	}); err != nil {
		t.Fatalf("second Update: %v", err)
	}
	second, err := Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load after second update: %v", err)
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

func TestLoad_CorruptJSONReturnsError(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, "runtime", "taskstate", "task.md.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state, err := Load(tasksDir, "task.md")
	if err == nil {
		t.Fatal("Load should fail for corrupt JSON")
	}
	if state != nil {
		t.Fatalf("Load returned %+v, want nil on error", state)
	}
}

func TestUpdate_CorruptJSONRecreatesFreshState(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, "runtime", "taskstate", "task.md.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Update(tasksDir, "task.md", func(state *TaskState) {
		state.LastOutcome = OutcomeReviewRejected
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	state, err := Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.LastOutcome != OutcomeReviewRejected {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, OutcomeReviewRejected)
	}
	if state.TaskFile != "task.md" {
		t.Fatalf("TaskFile = %q, want %q", state.TaskFile, "task.md")
	}
}

func TestDelete_RemovesFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := Update(tasksDir, "task.md", func(state *TaskState) {
		state.LastOutcome = "done"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := Delete(tasksDir, "task.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	state, err := Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load after Delete: %v", err)
	}
	if state != nil {
		t.Fatalf("Load after Delete = %+v, want nil", state)
	}
}

func TestSweep_RemovesTerminalStateAndKeepsActive(t *testing.T) {
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
				if err := Update(tasksDir, name, func(state *TaskState) {
					state.LastOutcome = name
				}); err != nil {
					t.Fatalf("Update %s: %v", name, err)
				}
			}

			if err := Sweep(tasksDir); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			for _, name := range []string{"done.md", "failed.md", "gone.md"} {
				state, err := Load(tasksDir, name)
				if err != nil {
					t.Fatalf("Load %s: %v", name, err)
				}
				if state != nil {
					t.Fatalf("Load(%s) = %+v, want nil after sweep", name, state)
				}
			}
			active, err := Load(tasksDir, "active.md")
			if err != nil {
				t.Fatalf("Load active: %v", err)
			}
			if active == nil {
				t.Fatalf("active taskstate should be preserved for %s", activeDir)
			}
		})
	}
}

func TestUpdate_EmptyTasksDirFails(t *testing.T) {
	err := Update("", "task.md", func(state *TaskState) {
		state.LastOutcome = OutcomeReviewLaunched
	})
	if err == nil {
		t.Fatal("Update should fail for empty tasksDir")
	}
	if !strings.Contains(err.Error(), "tasks directory must not be empty") {
		t.Fatalf("Update error = %v, want empty tasksDir error", err)
	}
}

func TestLoad_InvalidTaskFilenameFails(t *testing.T) {
	state, err := Load(t.TempDir(), "../escape.md")
	if err == nil {
		t.Fatal("Load should fail for invalid task filename")
	}
	if state != nil {
		t.Fatalf("Load returned %+v, want nil on error", state)
	}
}

func TestLoad_EmptyTaskFilenameFails(t *testing.T) {
	state, err := Load(t.TempDir(), "")
	if err == nil {
		t.Fatal("Load should fail for empty task filename")
	}
	if state != nil {
		t.Fatalf("Load returned %+v, want nil on error", state)
	}
}

func TestDelete_MissingFileReturnsNil(t *testing.T) {
	if err := Delete(t.TempDir(), "missing.md"); err != nil {
		t.Fatalf("Delete missing file: %v", err)
	}
}

func TestOutcomeConstants_CoverFullLifecycle(t *testing.T) {
	// Verify each constant has the expected wire-format string so that
	// existing JSON state files remain backward-compatible.
	tests := []struct {
		name     string
		constant string
		wire     string
	}{
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
			if err := Update(tasksDir, filename, func(state *TaskState) {
				state.LastOutcome = outcome
			}); err != nil {
				t.Fatalf("Update: %v", err)
			}
			loaded, err := Load(tasksDir, filename)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if loaded.LastOutcome != outcome {
				t.Fatalf("LastOutcome = %q, want %q", loaded.LastOutcome, outcome)
			}
		})
	}
}
