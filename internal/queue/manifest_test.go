package queue

import (
	"fmt"
	"mato/internal/dirs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeQueueManifest_PriorityOrder(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "low-prio.md",
		"---\nid: low-prio\npriority: 90\n---\n# Low\n")
	writeTask(t, tasksDir, dirs.Backlog, "high-prio.md",
		"---\nid: high-prio\npriority: 10\n---\n# High\n")
	writeTask(t, tasksDir, dirs.Backlog, "mid-prio.md",
		"---\nid: mid-prio\npriority: 50\n---\n# Mid\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	lines := strings.Split(strings.TrimRight(manifest, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), manifest)
	}
	want := []string{"high-prio.md", "mid-prio.md", "low-prio.md"}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestComputeQueueManifest_EqualPrioritySortByFilename(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "zebra.md",
		"---\nid: zebra\npriority: 50\n---\n# Zebra\n")
	writeTask(t, tasksDir, dirs.Backlog, "alpha.md",
		"---\nid: alpha\npriority: 50\n---\n# Alpha\n")
	writeTask(t, tasksDir, dirs.Backlog, "middle.md",
		"---\nid: middle\npriority: 50\n---\n# Middle\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	lines := strings.Split(strings.TrimRight(manifest, "\n"), "\n")
	want := []string{"alpha.md", "middle.md", "zebra.md"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestComputeQueueManifest_EmptyBacklog(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	if manifest != "" {
		t.Errorf("manifest = %q, want empty string", manifest)
	}
}

func TestComputeQueueManifest_OnlyBacklogTasks(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Tasks in other directories should NOT appear in the manifest.
	writeTask(t, tasksDir, dirs.Waiting, "waiting-task.md",
		"---\nid: waiting-task\npriority: 10\n---\n# Waiting\n")
	writeTask(t, tasksDir, dirs.InProgress, "in-progress-task.md",
		"---\nid: in-progress-task\npriority: 10\n---\n# In Progress\n")
	writeTask(t, tasksDir, dirs.Completed, "completed-task.md",
		"---\nid: completed-task\npriority: 10\n---\n# Completed\n")

	// Only backlog task should appear.
	writeTask(t, tasksDir, dirs.Backlog, "backlog-task.md",
		"---\nid: backlog-task\npriority: 50\n---\n# Backlog\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	if manifest != "backlog-task.md\n" {
		t.Errorf("manifest = %q, want %q", manifest, "backlog-task.md\n")
	}
}

func TestComputeQueueManifest_ExcludeSet(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "include.md",
		"---\nid: include\npriority: 50\n---\n# Include\n")
	writeTask(t, tasksDir, dirs.Backlog, "exclude.md",
		"---\nid: exclude\npriority: 10\n---\n# Exclude\n")

	exclude := map[string]struct{}{"exclude.md": {}}

	manifest, err := ComputeQueueManifest(tasksDir, exclude, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	if manifest != "include.md\n" {
		t.Errorf("manifest = %q, want %q", manifest, "include.md\n")
	}
}

func TestComputeQueueManifest_WithIndex(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "z-task.md",
		"---\nid: z-task\npriority: 20\n---\n# Z\n")
	writeTask(t, tasksDir, dirs.Backlog, "a-task.md",
		"---\nid: a-task\npriority: 20\n---\n# A\n")

	idx := BuildIndex(tasksDir)
	manifest, err := ComputeQueueManifest(tasksDir, nil, idx)
	if err != nil {
		t.Fatalf("ComputeQueueManifest with index: %v", err)
	}

	lines := strings.Split(strings.TrimRight(manifest, "\n"), "\n")
	want := []string{"a-task.md", "z-task.md"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestComputeQueueManifest_ExcludesDependencyBlockedBacklog(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "blocked.md",
		"---\nid: blocked\ndepends_on: [missing-dep]\npriority: 10\n---\n# Blocked\n")
	writeTask(t, tasksDir, dirs.Backlog, "runnable.md",
		"---\nid: runnable\npriority: 20\n---\n# Runnable\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	if manifest != "runnable.md\n" {
		t.Fatalf("manifest = %q, want %q", manifest, "runnable.md\n")
	}
}

func TestComputeQueueManifest_TrailingNewline(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "task.md",
		"---\nid: task\n---\n# Task\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	if !strings.HasSuffix(manifest, "\n") {
		t.Errorf("manifest should end with newline: %q", manifest)
	}
}

func TestWriteQueueManifest(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "beta.md",
		"---\nid: beta\npriority: 30\n---\n# Beta\n")
	writeTask(t, tasksDir, dirs.Backlog, "alpha.md",
		"---\nid: alpha\npriority: 10\n---\n# Alpha\n")

	if err := WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read .queue: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{"alpha.md", "beta.md"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(lines), len(want))
	}
	for i, got := range lines {
		if got != want[i] {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
}

func TestWriteQueueManifestFromView(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "blocked.md",
		"---\nid: blocked\ndepends_on: [missing]\npriority: 5\n---\n# Blocked\n")
	writeTask(t, tasksDir, dirs.Backlog, "alpha.md",
		"---\nid: alpha\npriority: 10\n---\n# Alpha\n")
	writeTask(t, tasksDir, dirs.Backlog, "beta.md",
		"---\nid: beta\npriority: 20\n---\n# Beta\n")

	idx := BuildIndex(tasksDir)
	view := ComputeRunnableBacklogView(tasksDir, idx)
	if err := WriteQueueManifestFromView(tasksDir, nil, idx, view); err != nil {
		t.Fatalf("WriteQueueManifestFromView: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read .queue: %v", err)
	}
	if string(data) != "alpha.md\nbeta.md\n" {
		t.Fatalf(".queue = %q, want %q", string(data), "alpha.md\nbeta.md\n")
	}
}

func TestWriteQueueManifest_EmptyBacklogFile(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	if err := WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read .queue: %v", err)
	}

	if string(data) != "" {
		t.Errorf(".queue = %q, want empty", string(data))
	}
}

func TestComputeQueueManifest_ParseFailureSkipped(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Good task.
	writeTask(t, tasksDir, dirs.Backlog, "good.md",
		"---\nid: good\npriority: 50\n---\n# Good\n")

	// Bad YAML — should be skipped, not cause error.
	writeTask(t, tasksDir, dirs.Backlog, "bad.md",
		"---\nbad: [unclosed\n---\n# Bad\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest: %v", err)
	}

	if manifest != "good.md\n" {
		t.Errorf("manifest = %q, want %q", manifest, "good.md\n")
	}
}

func TestComputeQueueManifest_BacklogTaskWarningDoesNotAbort(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "good.md",
		"---\nid: good\npriority: 50\n---\n# Good\n")

	// Simulate per-task backlog warnings (invalid glob, unsafe affects)
	// that should NOT cause manifest generation to fail.
	idx := BuildIndex(tasksDir)
	idx.buildWarnings = append(idx.buildWarnings, BuildWarning{
		State: dirs.Backlog,
		Path:  filepath.Join(tasksDir, dirs.Backlog, "good.md"),
		Err:   fmt.Errorf("invalid glob syntax in affects: [unclosed"),
	})

	view := ComputeRunnableBacklogView(tasksDir, idx)
	manifest, err := ComputeQueueManifestFromView(tasksDir, nil, idx, view)
	if err != nil {
		t.Fatalf("ComputeQueueManifestFromView should not fail on per-task warning: %v", err)
	}
	if manifest != "good.md\n" {
		t.Errorf("manifest = %q, want %q", manifest, "good.md\n")
	}
}

func TestComputeQueueManifest_BacklogDirUnreadableAborts(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Simulate a directory-level read failure for the backlog directory.
	idx := &PollIndex{
		buildWarnings: []BuildWarning{{
			State:    dirs.Backlog,
			Path:     filepath.Join(tasksDir, dirs.Backlog),
			Err:      os.ErrPermission,
			DirLevel: true,
		}},
	}

	view := ComputeRunnableBacklogView(tasksDir, idx)
	_, err := ComputeQueueManifestFromView(tasksDir, nil, idx, view)
	if err == nil {
		t.Fatal("expected error when backlog directory is unreadable")
	}
	if !strings.Contains(err.Error(), "read backlog dir") {
		t.Fatalf("error = %v, want backlog read failure", err)
	}
}

func TestComputeQueueManifest_InvalidGlobStillSucceeds(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, dirs.Backlog, "good.md",
		"---\nid: good\npriority: 50\n---\n# Good\n")
	writeTask(t, tasksDir, dirs.Backlog, "bad-glob.md",
		"---\nid: bad-glob\npriority: 30\naffects:\n  - \"[unclosed\"\n---\n# Bad Glob\n")

	manifest, err := ComputeQueueManifest(tasksDir, nil, nil)
	if err != nil {
		t.Fatalf("ComputeQueueManifest should succeed with invalid glob: %v", err)
	}

	// Both tasks should appear (invalid glob doesn't remove the task).
	if !strings.Contains(manifest, "good.md") {
		t.Errorf("manifest missing good.md: %q", manifest)
	}
	if !strings.Contains(manifest, "bad-glob.md") {
		t.Errorf("manifest missing bad-glob.md: %q", manifest)
	}
}
