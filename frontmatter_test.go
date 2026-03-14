package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseTaskFile_AllFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "complex-task.md")
	content := `---
id: custom-id
priority: 7
depends_on: [task-a, task-b]
affects:
  - api
  - cli
tags: [bug, urgent]
estimated_complexity: high
max_retries: 5
---
# Title
Task body.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	want := TaskMeta{
		ID:                  "custom-id",
		Priority:            7,
		DependsOn:           []string{"task-a", "task-b"},
		Affects:             []string{"api", "cli"},
		Tags:                []string{"bug", "urgent"},
		EstimatedComplexity: "high",
		MaxRetries:          5,
	}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "# Title\nTask body.\n" {
		t.Fatalf("body = %q, want %q", body, "# Title\nTask body.\n")
	}
}

func TestParseTaskFile_PartialFrontmatterUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-task.md")
	content := `---
priority: 12
tags:
  - ops
---
Body
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	if meta.ID != "partial-task" {
		t.Fatalf("meta.ID = %q, want %q", meta.ID, "partial-task")
	}
	if meta.Priority != 12 {
		t.Fatalf("meta.Priority = %d, want 12", meta.Priority)
	}
	if !reflect.DeepEqual(meta.Tags, []string{"ops"}) {
		t.Fatalf("meta.Tags = %#v, want %#v", meta.Tags, []string{"ops"})
	}
	if meta.MaxRetries != 3 {
		t.Fatalf("meta.MaxRetries = %d, want 3", meta.MaxRetries)
	}
	if meta.DependsOn != nil {
		t.Fatalf("meta.DependsOn = %#v, want nil", meta.DependsOn)
	}
	if body != "Body\n" {
		t.Fatalf("body = %q, want %q", body, "Body\n")
	}
}

func TestParseTaskFile_NoFrontmatterUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain-task.md")
	content := "Do the work.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "plain-task", Priority: 50, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != content {
		t.Fatalf("body = %q, want %q", body, content)
	}
}

func TestParseTaskFile_EmptyFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-frontmatter.md")
	content := "---\n---\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	if meta.ID != "empty-frontmatter" || meta.Priority != 50 || meta.MaxRetries != 3 {
		t.Fatalf("unexpected defaults: %#v", meta)
	}
	if body != "Body text\n" {
		t.Fatalf("body = %q, want %q", body, "Body text\n")
	}
}

func TestParseTaskFile_StripsHTMLCommentLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commented-task.md")
	content := "<!-- claimed-by: abc -->\n# Title\n<!-- failure: x -->\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	if body != "# Title\nBody text\n" {
		t.Fatalf("body = %q, want %q", body, "# Title\nBody text\n")
	}
}

func TestParseTaskFile_BackwardCompatibleMarkdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-task.md")
	content := "# Title\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := parseTaskFile(path)
	if err != nil {
		t.Fatalf("parseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "legacy-task", Priority: 50, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != content {
		t.Fatalf("body = %q, want %q", body, content)
	}
}

func TestReconcileReadyQueue_PromotesWhenDepsMet(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}

	completedWithID := `---
id: dep-a
---
Done
`
	if err := os.WriteFile(filepath.Join(tasksDir, "completed", "different-name.md"), []byte(completedWithID), 0o644); err != nil {
		t.Fatalf("os.WriteFile completedWithID: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "completed", "dep-b.md"), []byte("Done\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile dep-b: %v", err)
	}

	waiting := `---
depends_on: [dep-a, dep-b]
---
Ready now
`
	waitingPath := filepath.Join(tasksDir, "waiting", "task.md")
	if err := os.WriteFile(waitingPath, []byte(waiting), 0o644); err != nil {
		t.Fatalf("os.WriteFile waiting: %v", err)
	}

	if got := reconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("reconcileReadyQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "task.md")); err != nil {
		t.Fatalf("promoted task missing from backlog: %v", err)
	}
	if _, err := os.Stat(waitingPath); !os.IsNotExist(err) {
		t.Fatalf("waiting task should be moved, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_LeavesUnmetDepsWaiting(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}

	waiting := `---
depends_on:
  - missing-task
---
Still blocked
`
	waitingPath := filepath.Join(tasksDir, "waiting", "blocked-task.md")
	if err := os.WriteFile(waitingPath, []byte(waiting), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	if got := reconcileReadyQueue(tasksDir); got != 0 {
		t.Fatalf("reconcileReadyQueue() = %d, want 0", got)
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("task with unmet deps should stay in waiting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "blocked-task.md")); !os.IsNotExist(err) {
		t.Fatalf("task with unmet deps should not be in backlog, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_PromotesTaskWithNoDeps(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}

	waitingPath := filepath.Join(tasksDir, "waiting", "solo-task.md")
	if err := os.WriteFile(waitingPath, []byte("# Solo\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	if got := reconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("reconcileReadyQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "solo-task.md")); err != nil {
		t.Fatalf("promoted task missing from backlog: %v", err)
	}
}

func TestWriteQueueManifest_SortsByPriorityThenFilename(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tasksDir, "backlog"), 0o755); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}

	files := map[string]string{
		"z-low.md":     "---\npriority: 20\n---\nBody\n",
		"b-high.md":    "---\npriority: 5\n---\nBody\n",
		"a-high.md":    "---\npriority: 5\n---\nBody\n",
		"c-default.md": "Body\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(tasksDir, "backlog", name), []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", name, err)
		}
	}

	if err := writeQueueManifest(tasksDir); err != nil {
		t.Fatalf("writeQueueManifest: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("os.ReadFile(.queue): %v", err)
	}
	want := "a-high.md\nb-high.md\nz-low.md\nc-default.md\n"
	if string(data) != want {
		t.Fatalf("manifest = %q, want %q", string(data), want)
	}
}
