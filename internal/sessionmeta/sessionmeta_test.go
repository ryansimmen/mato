package sessionmeta

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"mato/internal/dirs"
)

func TestLoad_MissingReturnsNil(t *testing.T) {
	session, err := Load(t.TempDir(), KindWork, "missing.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if session != nil {
		t.Fatalf("Load = %+v, want nil", session)
	}
}

func TestLoadOrCreate_CreatesSeparateWorkAndReviewSessions(t *testing.T) {
	tasksDir := t.TempDir()
	work, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("LoadOrCreate work: %v", err)
	}
	review, err := LoadOrCreate(tasksDir, KindReview, "task.md", "task/task")
	if err != nil {
		t.Fatalf("LoadOrCreate review: %v", err)
	}
	if work == nil || review == nil {
		t.Fatal("expected both session records to be created")
	}
	if work.Kind != KindWork {
		t.Fatalf("work.Kind = %q, want %q", work.Kind, KindWork)
	}
	if review.Kind != KindReview {
		t.Fatalf("review.Kind = %q, want %q", review.Kind, KindReview)
	}
	if work.CopilotSessionID == "" || review.CopilotSessionID == "" {
		t.Fatal("session IDs should be set")
	}
	if work.CopilotSessionID == review.CopilotSessionID {
		t.Fatal("work and review sessions should not share an ID")
	}
	for _, path := range []string{
		filepath.Join(tasksDir, "runtime", "sessionmeta", "work-task.md.json"),
		filepath.Join(tasksDir, "runtime", "sessionmeta", "review-task.md.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected session file %s: %v", path, err)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(work.CopilotSessionID) {
		t.Fatalf("work session ID %q is not a v4 UUID", work.CopilotSessionID)
	}
}

func TestLoadOrCreate_ReusesExistingSessionID(t *testing.T) {
	tasksDir := t.TempDir()
	first, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	second, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if second.CopilotSessionID != first.CopilotSessionID {
		t.Fatalf("session ID = %q, want %q", second.CopilotSessionID, first.CopilotSessionID)
	}
}

func TestLoadOrCreate_DoesNotRewriteWhenUnchanged(t *testing.T) {
	tasksDir := t.TempDir()
	if _, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task"); err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	path := filepath.Join(tasksDir, "runtime", "sessionmeta", "work-task.md.json")
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat before: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task"); err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("expected unchanged session file mtime, before=%v after=%v", infoBefore.ModTime(), infoAfter.ModTime())
	}
}

func TestResetSessionID_RotatesSessionIDAndPreservesState(t *testing.T) {
	tasksDir := t.TempDir()
	created, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := Update(tasksDir, KindWork, "task.md", func(session *Session) {
		session.LastHeadSHA = "abc123"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reset, err := ResetSessionID(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("ResetSessionID: %v", err)
	}
	if reset.CopilotSessionID == created.CopilotSessionID {
		t.Fatal("expected rotated session ID")
	}
	if reset.TaskBranch != "task/task" {
		t.Fatalf("TaskBranch = %q, want %q", reset.TaskBranch, "task/task")
	}
	if reset.LastHeadSHA != "abc123" {
		t.Fatalf("LastHeadSHA = %q, want %q", reset.LastHeadSHA, "abc123")
	}
}

func TestLoadOrCreate_CorruptJSONFallsBackToFreshSession(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, "runtime", "sessionmeta", "work-task.md.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	session, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if session == nil {
		t.Fatal("expected fresh session after corrupt JSON")
	}
	if session.CopilotSessionID == "" {
		t.Fatal("fresh session should have an ID")
	}
	loaded, err := Load(tasksDir, KindWork, "task.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil || loaded.CopilotSessionID != session.CopilotSessionID {
		t.Fatalf("loaded session = %+v, want persisted ID %q", loaded, session.CopilotSessionID)
	}
}

func TestUpdate_PreservesExistingSessionID(t *testing.T) {
	tasksDir := t.TempDir()
	created, err := LoadOrCreate(tasksDir, KindWork, "task.md", "task/task")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := Update(tasksDir, KindWork, "task.md", func(session *Session) {
		session.TaskBranch = "task/updated"
		session.LastHeadSHA = "abc123"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	loaded, err := Load(tasksDir, KindWork, "task.md")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CopilotSessionID != created.CopilotSessionID {
		t.Fatalf("session ID = %q, want %q", loaded.CopilotSessionID, created.CopilotSessionID)
	}
	if loaded.TaskBranch != "task/updated" {
		t.Fatalf("TaskBranch = %q, want %q", loaded.TaskBranch, "task/updated")
	}
	if loaded.LastHeadSHA != "abc123" {
		t.Fatalf("LastHeadSHA = %q, want %q", loaded.LastHeadSHA, "abc123")
	}
	if loaded.Kind != KindWork {
		t.Fatalf("Kind = %q, want %q", loaded.Kind, KindWork)
	}
	if loaded.TaskFile != "task.md" {
		t.Fatalf("TaskFile = %q, want %q", loaded.TaskFile, "task.md")
	}
}

func TestDeleteAll_RemovesWorkAndReviewFiles(t *testing.T) {
	tasksDir := t.TempDir()
	for _, kind := range []string{KindWork, KindReview} {
		if _, err := LoadOrCreate(tasksDir, kind, "task.md", "task/task"); err != nil {
			t.Fatalf("seed %s session: %v", kind, err)
		}
	}
	if err := DeleteAll(tasksDir, "task.md"); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	for _, kind := range []string{KindWork, KindReview} {
		loaded, err := Load(tasksDir, kind, "task.md")
		if err != nil {
			t.Fatalf("Load %s: %v", kind, err)
		}
		if loaded != nil {
			t.Fatalf("Load(%s) = %+v, want nil", kind, loaded)
		}
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
			for _, kind := range []string{KindWork, KindReview} {
				for _, name := range []string{"active.md", "done.md", "gone.md"} {
					if _, err := LoadOrCreate(tasksDir, kind, name, "task/"+name); err != nil {
						t.Fatalf("LoadOrCreate %s %s: %v", kind, name, err)
					}
				}
			}
			if err := Sweep(tasksDir); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			for _, kind := range []string{KindWork, KindReview} {
				active, err := Load(tasksDir, kind, "active.md")
				if err != nil {
					t.Fatalf("Load active %s: %v", kind, err)
				}
				if active == nil {
					t.Fatalf("active %s session should remain", kind)
				}
				for _, name := range []string{"done.md", "gone.md"} {
					loaded, err := Load(tasksDir, kind, name)
					if err != nil {
						t.Fatalf("Load %s %s: %v", kind, name, err)
					}
					if loaded != nil {
						t.Fatalf("stale %s session for %s should be removed", kind, name)
					}
				}
			}
		})
	}
}
