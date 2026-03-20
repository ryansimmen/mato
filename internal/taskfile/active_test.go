package taskfile

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeTaskFile creates a task markdown file at dir/name with the given content.
func writeTaskFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectActiveAffects_MultipleDirs(t *testing.T) {
	tasksDir := t.TempDir()

	// in-progress task with affects
	writeTaskFile(t, filepath.Join(tasksDir, "in-progress"), "task-a.md", `---
id: task-a
affects:
  - internal/foo.go
  - internal/bar.go
---
# Task A
`)

	// ready-for-review task with affects
	writeTaskFile(t, filepath.Join(tasksDir, "ready-for-review"), "task-b.md", `---
id: task-b
affects:
  - cmd/main.go
---
# Task B
`)

	// ready-to-merge task with affects
	writeTaskFile(t, filepath.Join(tasksDir, "ready-to-merge"), "task-c.md", `---
id: task-c
affects:
  - docs/readme.md
---
# Task C
`)

	active := CollectActiveAffects(tasksDir)
	if len(active) != 3 {
		t.Fatalf("expected 3 active tasks, got %d", len(active))
	}

	// Sort by name for deterministic checks
	sort.Slice(active, func(i, j int) bool { return active[i].Name < active[j].Name })

	cases := []struct {
		name    string
		dir     string
		affects []string
	}{
		{"task-a.md", "in-progress", []string{"internal/foo.go", "internal/bar.go"}},
		{"task-b.md", "ready-for-review", []string{"cmd/main.go"}},
		{"task-c.md", "ready-to-merge", []string{"docs/readme.md"}},
	}

	for i, tc := range cases {
		if active[i].Name != tc.name {
			t.Errorf("task %d: got Name=%q, want %q", i, active[i].Name, tc.name)
		}
		if active[i].Dir != tc.dir {
			t.Errorf("task %d: got Dir=%q, want %q", i, active[i].Dir, tc.dir)
		}
		if len(active[i].Affects) != len(tc.affects) {
			t.Errorf("task %d: got %d affects, want %d", i, len(active[i].Affects), len(tc.affects))
			continue
		}
		for j, a := range tc.affects {
			if active[i].Affects[j] != a {
				t.Errorf("task %d affects[%d]: got %q, want %q", i, j, active[i].Affects[j], a)
			}
		}
	}
}

func TestCollectActiveAffects_EmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	active := CollectActiveAffects(tasksDir)
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks for empty dir, got %d", len(active))
	}
}

func TestCollectActiveAffects_NoAffectsField(t *testing.T) {
	tasksDir := t.TempDir()

	// Task with no affects field
	writeTaskFile(t, filepath.Join(tasksDir, "in-progress"), "no-affects.md", `---
id: no-affects
priority: 10
---
# No Affects Task
`)

	// Task with empty affects
	writeTaskFile(t, filepath.Join(tasksDir, "in-progress"), "empty-affects.md", `---
id: empty-affects
affects: []
---
# Empty Affects Task
`)

	active := CollectActiveAffects(tasksDir)
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks when no affects present, got %d", len(active))
	}
}

func TestCollectActiveAffects_SkipsNonMarkdown(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, "in-progress")

	// A non-markdown file should be ignored
	writeTaskFile(t, dir, "notes.txt", "not a task file")

	// A subdirectory should be ignored
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A valid markdown task with affects
	writeTaskFile(t, dir, "valid.md", `---
id: valid
affects:
  - internal/valid.go
---
# Valid Task
`)

	active := CollectActiveAffects(tasksDir)
	if len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(active))
	}
	if active[0].Name != "valid.md" {
		t.Errorf("got Name=%q, want %q", active[0].Name, "valid.md")
	}
}

func TestCollectActiveAffects_WithHTMLComments(t *testing.T) {
	tasksDir := t.TempDir()

	// Task file with HTML comment metadata (like claimed-by, branch)
	writeTaskFile(t, filepath.Join(tasksDir, "in-progress"), "commented.md", `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/commented -->
---
id: commented
affects:
  - internal/commented.go
---
# Commented Task
`)

	active := CollectActiveAffects(tasksDir)
	if len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(active))
	}
	if active[0].Affects[0] != "internal/commented.go" {
		t.Errorf("got affects[0]=%q, want %q", active[0].Affects[0], "internal/commented.go")
	}
}

func TestCollectActiveAffects_IgnoresOtherDirs(t *testing.T) {
	tasksDir := t.TempDir()

	// Tasks in backlog/ and completed/ should NOT be collected
	writeTaskFile(t, filepath.Join(tasksDir, "backlog"), "backlog-task.md", `---
id: backlog-task
affects:
  - internal/backlog.go
---
# Backlog Task
`)
	writeTaskFile(t, filepath.Join(tasksDir, "completed"), "done-task.md", `---
id: done-task
affects:
  - internal/done.go
---
# Done Task
`)

	active := CollectActiveAffects(tasksDir)
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks from non-active dirs, got %d", len(active))
	}
}
