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
		state.LastOutcome = "work-pushed"
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
	if first.LastOutcome != "work-pushed" {
		t.Fatalf("LastOutcome = %q, want %q", first.LastOutcome, "work-pushed")
	}
	if strings.TrimSpace(first.UpdatedAt) == "" {
		t.Fatal("UpdatedAt should be set")
	}

	if err := Update(tasksDir, "task.md", func(state *TaskState) {
		state.LastReviewedSHA = "def456"
		state.LastOutcome = "review-approved"
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
	if second.LastOutcome != "review-approved" {
		t.Fatalf("LastOutcome = %q, want %q", second.LastOutcome, "review-approved")
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
		state.LastOutcome = "review-rejected"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	state, err := Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.LastOutcome != "review-rejected" {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, "review-rejected")
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
		state.LastOutcome = "review-launched"
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
