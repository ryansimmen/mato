package merge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/queue"
)

func TestTaskHasMergeSuccessRecord(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "file with merged marker",
			content: "# Task\nSome content\n<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->\n",
			want:    true,
		},
		{
			name:    "file without merged marker",
			content: "# Task\nSome content\n<!-- claimed-by: abc123 -->\n",
			want:    false,
		},
		{
			name:    "empty file",
			content: "",
			want:    false,
		},
		{
			name:    "marker as substring in line",
			content: "blah <!-- merged: merge-queue at 2026-01-01T00:00:00Z --> blah\n",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "task.md")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			got := taskHasMergeSuccessRecord(path)
			if got != tt.want {
				t.Errorf("taskHasMergeSuccessRecord() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("file not found returns false", func(t *testing.T) {
		got := taskHasMergeSuccessRecord("/nonexistent/path/task.md")
		if got {
			t.Error("expected false for nonexistent file")
		}
	})
}

func TestAppendTaskRecord(t *testing.T) {
	t.Run("appends record to existing file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.md")
		original := "# My Task\nSome content.\n"
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		err := appendTaskRecord(path, "<!-- merged: merge-queue at %s -->", "2026-01-01T00:00:00Z")
		if err != nil {
			t.Fatalf("appendTaskRecord: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		content := string(data)
		if !strings.Contains(content, original) {
			t.Error("original content should be preserved")
		}
		if !strings.Contains(content, "<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->") {
			t.Error("record should be appended")
		}
	})

	t.Run("returns error for nonexistent file", func(t *testing.T) {
		err := appendTaskRecord("/nonexistent/path/task.md", "<!-- test -->")
		if err == nil {
			t.Error("expected error for nonexistent file")
		}
	})
}

func TestMarkTaskMerged(t *testing.T) {
	t.Run("appends merged record", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.md")
		if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := markTaskMerged(path); err != nil {
			t.Fatalf("markTaskMerged: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(data), mergedTaskRecordPrefix) {
			t.Error("expected merged record prefix in file")
		}
	})

	t.Run("idempotent when already marked", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.md")
		content := "# Task\n\n<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := markTaskMerged(path); err != nil {
			t.Fatalf("markTaskMerged: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		// Should only contain one merged record (the original one).
		count := strings.Count(string(data), mergedTaskRecordPrefix)
		if count != 1 {
			t.Errorf("expected exactly 1 merged record, got %d", count)
		}
	})
}

func TestMoveTaskWithRetry(t *testing.T) {
	t.Run("successful first attempt", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src", "task.md")
		dst := filepath.Join(dir, "dst", "task.md")

		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(src, []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := moveTaskWithRetry(src, dst); err != nil {
			t.Fatalf("moveTaskWithRetry: %v", err)
		}

		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Error("source file should not exist after move")
		}
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("ReadFile dst: %v", err)
		}
		if string(data) != "# Task\n" {
			t.Errorf("unexpected content: %q", string(data))
		}
	})

	t.Run("creates destination directory", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "task.md")
		dst := filepath.Join(dir, "nested", "deep", "task.md")

		if err := os.WriteFile(src, []byte("content"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := moveTaskWithRetry(src, dst); err != nil {
			t.Fatalf("moveTaskWithRetry: %v", err)
		}

		if _, err := os.Stat(dst); err != nil {
			t.Fatalf("destination should exist: %v", err)
		}
	})

	t.Run("returns error when source does not exist", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "nonexistent.md")
		dst := filepath.Join(dir, "dst", "task.md")

		err := moveTaskWithRetry(src, dst)
		if err == nil {
			t.Error("expected error when source does not exist")
		}
	})
}

func TestRemoveBranchMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := "<!-- branch: task/remove-me -->\n# Task\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := removeBranchMarker(path); err != nil {
		t.Fatalf("removeBranchMarker: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "<!-- branch:") {
		t.Fatalf("branch marker should be removed, got:\n%s", string(data))
	}
}

func TestHandleMergeFailure_ConflictInFailedKeepsBranchMarker(t *testing.T) {
	repoRoot := t.TempDir()
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyMerge, queue.DirFailed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	taskPath := filepath.Join(tasksDir, queue.DirReadyMerge, "conflict.md")
	content := strings.Join([]string{
		"<!-- branch: task/conflict -->",
		"---",
		"max_retries: 1",
		"---",
		"<!-- failure: prior-agent at 2026-01-01T00:00:00Z step=WORK error=prior -->",
		"# Conflict",
		"",
	}, "\n")
	if err := os.WriteFile(taskPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	task := mergeQueueTask{name: "conflict.md", path: taskPath, branch: "task/conflict"}
	if err := handleMergeFailure(repoRoot, tasksDir, task, errSquashMergeConflict); err != nil {
		t.Fatalf("handleMergeFailure: %v", err)
	}
	failedPath := filepath.Join(tasksDir, queue.DirFailed, "conflict.md")
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "<!-- branch: task/conflict -->") {
		t.Fatalf("failed task should retain branch marker, got:\n%s", string(data))
	}
}

func TestMergeFailureDestination(t *testing.T) {
	tests := []struct {
		name     string
		taskName string
		content  string
		wantDir  string
	}{
		{
			name:     "no failures goes to backlog",
			taskName: "task.md",
			content:  "---\nmax_retries: 3\n---\n# Task\n",
			wantDir:  "backlog",
		},
		{
			name:     "at max retries goes to failed",
			taskName: "task.md",
			content: "---\nmax_retries: 2\n---\n# Task\n" +
				"<!-- failure: agent1 at 2026-01-01T00:00:00Z step=WORK error=build -->\n" +
				"<!-- failure: agent2 at 2026-01-02T00:00:00Z step=WORK error=build -->\n",
			wantDir: "failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, sub := range []string{"backlog", "failed"} {
				if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
			}

			taskPath := filepath.Join(tasksDir, "in-progress", tt.taskName)
			if err := os.MkdirAll(filepath.Dir(taskPath), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(taskPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			got := mergeFailureDestination(tasksDir, taskPath, tt.taskName)
			if !strings.Contains(got, tt.wantDir) {
				t.Errorf("mergeFailureDestination() = %q, want dir containing %q", got, tt.wantDir)
			}
		})
	}
}

func TestShouldFailTask(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "below max retries",
			content: "---\nmax_retries: 3\n---\n# Task\n<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=x -->\n",
			want:    false,
		},
		{
			name: "at max retries",
			content: "---\nmax_retries: 2\n---\n# Task\n" +
				"<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=x -->\n" +
				"<!-- failure: b at 2026-01-02T00:00:00Z step=WORK error=y -->\n",
			want: true,
		},
		{
			name: "above max retries",
			content: "---\nmax_retries: 1\n---\n# Task\n" +
				"<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=x -->\n" +
				"<!-- failure: b at 2026-01-02T00:00:00Z step=WORK error=y -->\n",
			want: true,
		},
		{
			name:    "no failures",
			content: "---\nmax_retries: 3\n---\n# Task\n",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "task.md")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			got := shouldFailTask(path)
			if got != tt.want {
				t.Errorf("shouldFailTask() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFailMergeTask(t *testing.T) {
	t.Run("appends failure record and moves file", func(t *testing.T) {
		dir := t.TempDir()
		srcDir := filepath.Join(dir, "in-progress")
		dstDir := filepath.Join(dir, "backlog")
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.MkdirAll(dstDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		src := filepath.Join(srcDir, "task.md")
		dst := filepath.Join(dstDir, "task.md")
		if err := os.WriteFile(src, []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := failMergeTask(src, dst, "merge conflict"); err != nil {
			t.Fatalf("failMergeTask: %v", err)
		}

		// Source should be gone.
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Error("source should not exist after failMergeTask")
		}

		// Destination should exist with failure record.
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("ReadFile dst: %v", err)
		}
		if !strings.Contains(string(data), "<!-- failure:") {
			t.Error("expected failure record in moved file")
		}
		if !strings.Contains(string(data), "merge conflict") {
			t.Error("expected reason in failure record")
		}
	})

	t.Run("empty dst only appends record", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.md")
		if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		err := failMergeTask(path, "", "test failure")
		if err != nil {
			t.Fatalf("failMergeTask: %v", err)
		}

		// File should still be in place with failure record.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(data), "<!-- failure:") {
			t.Error("expected failure record appended")
		}
	})

	t.Run("empty reason uses default", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.md")
		if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := failMergeTask(path, "", ""); err != nil {
			t.Fatalf("failMergeTask: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(data), "merge queue failure") {
			t.Error("expected default reason in failure record")
		}
	})
}
