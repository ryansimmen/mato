package dirs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllContainsExpectedDirs(t *testing.T) {
	expected := map[string]bool{
		Waiting:     false,
		Backlog:     false,
		InProgress:  false,
		ReadyReview: false,
		ReadyMerge:  false,
		Completed:   false,
		Failed:      false,
	}

	for _, d := range All {
		if _, ok := expected[d]; !ok {
			t.Errorf("unexpected directory in All: %s", d)
		}
		expected[d] = true
	}
	for name, seen := range expected {
		if !seen {
			t.Errorf("expected directory %s missing from All", name)
		}
	}
}

func TestLocksConstant(t *testing.T) {
	if Locks != ".locks" {
		t.Errorf("Locks = %q, want %q", Locks, ".locks")
	}
}

func TestIsActive(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{name: "waiting", dir: Waiting, want: true},
		{name: "backlog", dir: Backlog, want: true},
		{name: "in progress", dir: InProgress, want: true},
		{name: "ready review", dir: ReadyReview, want: true},
		{name: "ready merge", dir: ReadyMerge, want: true},
		{name: "completed", dir: Completed, want: false},
		{name: "failed", dir: Failed, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, dir := range active {
				if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
					t.Fatalf("MkdirAll %s: %v", dir, err)
				}
			}
			taskPath := filepath.Join(tasksDir, tt.dir, "task.md")
			if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(taskPath, []byte("test"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			got, err := IsActive(tasksDir, "task.md")
			if err != nil {
				t.Fatalf("IsActive() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Fatalf("IsActive() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		tasksDir := t.TempDir()
		for _, dir := range active {
			if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
				t.Fatalf("MkdirAll %s: %v", dir, err)
			}
		}

		got, err := IsActive(tasksDir, "missing.md")
		if err != nil {
			t.Fatalf("IsActive() error = %v, want nil", err)
		}
		if got {
			t.Fatal("IsActive() = true, want false")
		}
	})

	t.Run("active directory missing", func(t *testing.T) {
		tasksDir := t.TempDir()
		for _, dir := range active {
			if dir == InProgress {
				continue
			}
			if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
				t.Fatalf("MkdirAll %s: %v", dir, err)
			}
		}

		got, err := IsActive(tasksDir, "task.md")
		if err == nil {
			t.Fatal("IsActive() error = nil, want missing directory failure")
		}
		if got {
			t.Fatal("IsActive() = true, want false when liveness is unknown")
		}
	})

	t.Run("active directory unreadable", func(t *testing.T) {
		tasksDir := t.TempDir()
		for _, dir := range active {
			if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
				t.Fatalf("MkdirAll %s: %v", dir, err)
			}
		}
		activeDir := filepath.Join(tasksDir, InProgress)
		taskPath := filepath.Join(activeDir, "task.md")
		if err := os.WriteFile(taskPath, []byte("test"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := os.Chmod(activeDir, 0o000); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(activeDir, 0o755)
		})

		got, err := IsActive(tasksDir, "task.md")
		if err == nil {
			t.Fatal("IsActive() error = nil, want stat failure")
		}
		if got {
			t.Fatal("IsActive() = true, want false when liveness is unknown")
		}
	})
}
