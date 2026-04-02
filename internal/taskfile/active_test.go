package taskfile_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"mato/internal/queue"
	"mato/internal/taskfile"
	"mato/internal/testutil"
)

func TestCollectActiveAffects_MultipleDirs(t *testing.T) {
	tasksDir := t.TempDir()

	// in-progress task with affects
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirInProgress, "task-a.md"), `---
id: task-a
affects:
  - internal/foo.go
  - internal/bar.go
---
# Task A
`)

	// ready-for-review task with affects
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirReadyReview, "task-b.md"), `---
id: task-b
affects:
  - cmd/main.go
---
# Task B
`)

	// ready-to-merge task with affects
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirReadyMerge, "task-c.md"), `---
id: task-c
affects:
  - docs/readme.md
---
# Task C
`)

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
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
		{"task-a.md", queue.DirInProgress, []string{"internal/foo.go", "internal/bar.go"}},
		{"task-b.md", queue.DirReadyReview, []string{"cmd/main.go"}},
		{"task-c.md", queue.DirReadyMerge, []string{"docs/readme.md"}},
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
	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings for missing dirs: %v", warnings)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks for empty dir, got %d", len(active))
	}
}

func TestCollectActiveAffects_NoAffectsField(t *testing.T) {
	tasksDir := t.TempDir()

	// Task with no affects field
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirInProgress, "no-affects.md"), `---
id: no-affects
priority: 10
---
# No Affects Task
`)

	// Task with empty affects
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirInProgress, "empty-affects.md"), `---
id: empty-affects
affects: []
---
# Empty Affects Task
`)

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks when no affects present, got %d", len(active))
	}
}

func TestCollectActiveAffects_SkipsNonMarkdown(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, queue.DirInProgress)

	// A non-markdown file should be ignored
	testutil.WriteFile(t, filepath.Join(dir, "notes.txt"), "not a task file")

	// A subdirectory should be ignored
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A valid markdown task with affects
	testutil.WriteFile(t, filepath.Join(dir, "valid.md"), `---
id: valid
affects:
  - internal/valid.go
---
# Valid Task
`)

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
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
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirInProgress, "commented.md"), `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/commented -->
---
id: commented
affects:
  - internal/commented.go
---
# Commented Task
`)

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
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
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirBacklog, "backlog-task.md"), `---
id: backlog-task
affects:
  - internal/backlog.go
---
# Backlog Task
`)
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirCompleted, "done-task.md"), `---
id: done-task
affects:
  - internal/done.go
---
# Done Task
`)

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks from non-active dirs, got %d", len(active))
	}
}

func TestCollectActiveAffects_MalformedTaskFile(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, queue.DirInProgress)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Malformed YAML frontmatter
	testutil.WriteFile(t, filepath.Join(dir, "bad-yaml.md"), "---\naffects: [unterminated\n---\n# Bad\n")

	// Valid task for comparison
	testutil.WriteFile(t, filepath.Join(dir, "good.md"), "---\naffects:\n  - foo.go\n---\n# Good\n")

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(active) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(active))
	}
	if active[0].Name != "good.md" {
		t.Errorf("got Name=%q, want %q", active[0].Name, "good.md")
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if warnings[0].File != "bad-yaml.md" {
		t.Errorf("warning File=%q, want %q", warnings[0].File, "bad-yaml.md")
	}
	if warnings[0].Dir != queue.DirInProgress {
		t.Errorf("warning Dir=%q, want %q", warnings[0].Dir, queue.DirInProgress)
	}
}

func TestCollectActiveAffects_UnreadableTaskFile(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, queue.DirInProgress)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create an unreadable file
	unreadable := filepath.Join(dir, "unreadable.md")
	testutil.WriteFile(t, unreadable, "---\naffects:\n  - foo.go\n---\n# Unreadable\n")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o644) })

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks, got %d", len(active))
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning for unreadable file, got %d: %v", len(warnings), warnings)
	}
	if warnings[0].File != "unreadable.md" {
		t.Errorf("warning File=%q, want %q", warnings[0].File, "unreadable.md")
	}
}

func TestCollectActiveAffects_UnreadableDirectory(t *testing.T) {
	tasksDir := t.TempDir()
	dir := filepath.Join(tasksDir, queue.DirInProgress)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Make the directory unreadable
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	active, warnings := taskfile.CollectActiveAffects(tasksDir)
	if len(active) != 0 {
		t.Fatalf("expected 0 active tasks, got %d", len(active))
	}
	// Should get a warning for the unreadable directory (permission error, not ENOENT)
	if len(warnings) < 1 {
		t.Fatalf("expected at least 1 warning for unreadable dir, got %d", len(warnings))
	}
	found := false
	for _, w := range warnings {
		if w.Dir == queue.DirInProgress && w.File == "" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected a directory-level warning for %s, got: %v", queue.DirInProgress, warnings)
	}
}

func TestCollectWarning_Error(t *testing.T) {
	tests := []struct {
		name string
		warn taskfile.CollectWarning
		want string
	}{
		{
			name: "directory error",
			warn: taskfile.CollectWarning{Dir: "in-progress", Err: os.ErrPermission},
			want: "in-progress: permission denied",
		},
		{
			name: "file error",
			warn: taskfile.CollectWarning{Dir: "in-progress", File: "bad.md", Err: os.ErrPermission},
			want: "in-progress/bad.md: permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.warn.Error()
			if got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
