package merge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/queue"
	"github.com/ryansimmen/mato/internal/runtimedata"
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
			want:    false,
		},
		{
			name: "marker inside fenced code block",
			content: strings.Join([]string{
				"# Task",
				"```",
				"<!-- merged: merge-queue at 2026-01-01T00:00:00Z -->",
				"```",
			}, "\n"),
			want: false,
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

	t.Run("concurrent appends preserve all records", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "task.md")
		if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		records := []string{
			"<!-- failure: merge-queue at 2026-01-01T00:00:00Z — merge conflict -->",
			"<!-- merged: merge-queue at 2026-01-01T00:00:01Z -->",
		}
		start := make(chan struct{})
		errCh := make(chan error, len(records))
		var wg sync.WaitGroup
		for _, record := range records {
			record := record
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errCh <- appendTaskRecord(path, "%s", record)
			}()
		}

		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatalf("appendTaskRecord: %v", err)
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		content := string(data)
		for _, record := range records {
			if !strings.Contains(content, record) {
				t.Fatalf("missing appended record %q in:\n%s", record, content)
			}
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

		if err := moveTaskWithRetry(context.Background(), src, dst); err != nil {
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

		if err := moveTaskWithRetry(context.Background(), src, dst); err != nil {
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

		err := moveTaskWithRetry(context.Background(), src, dst)
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

func TestRemoveBranchMarker_PreservesConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := strings.Join([]string{
		"<!-- branch: task/remove-me -->",
		"# Task",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	atHook := make(chan struct{})
	releaseRemove := make(chan struct{})
	origHook := removeBranchMarkerBeforeWriteHook
	removeBranchMarkerBeforeWriteHook = func() {
		close(atHook)
		<-releaseRemove
	}
	t.Cleanup(func() {
		removeBranchMarkerBeforeWriteHook = origHook
	})

	removeErrCh := make(chan error, 1)
	go func() {
		removeErrCh <- removeBranchMarker(path)
	}()

	<-atHook

	appendRecord := "<!-- failure: merge-queue at 2026-01-01T00:00:00Z — merge conflict -->"
	appendErrCh := make(chan error, 1)
	go func() {
		appendErrCh <- appendTaskRecord(path, "%s", appendRecord)
	}()

	close(releaseRemove)

	if err := <-removeErrCh; err != nil {
		t.Fatalf("removeBranchMarker: %v", err)
	}
	if err := <-appendErrCh; err != nil {
		t.Fatalf("appendTaskRecord: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "<!-- branch:") {
		t.Fatalf("branch marker should be removed, got:\n%s", got)
	}
	if !strings.Contains(got, appendRecord) {
		t.Fatalf("concurrent append should be preserved, got:\n%s", got)
	}
}

func TestHandleMergeFailure_ConflictInFailedKeepsBranchMarker(t *testing.T) {
	repoRoot := t.TempDir()
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyMerge, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	taskPath := filepath.Join(tasksDir, dirs.ReadyMerge, "conflict.md")
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
	failedPath := filepath.Join(tasksDir, dirs.Failed, "conflict.md")
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "<!-- branch: task/conflict -->") {
		t.Fatalf("failed task should retain branch marker, got:\n%s", string(data))
	}
}

func TestHandleMergeFailure_FailedTaskCleansBranch(t *testing.T) {
	repoRoot := t.TempDir()
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyMerge, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	// Task with max_retries=1 and one prior failure → next failure routes to failed/
	taskPath := filepath.Join(tasksDir, dirs.ReadyMerge, "exhaust.md")
	content := strings.Join([]string{
		"<!-- branch: task/exhaust -->",
		"---",
		"max_retries: 1",
		"---",
		"<!-- failure: prior-agent at 2026-01-01T00:00:00Z step=WORK error=prior -->",
		"# Exhaust retries",
		"",
	}, "\n")
	if err := os.WriteFile(taskPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var cleanedBranch string
	orig := cleanupTaskBranchFn
	cleanupTaskBranchFn = func(root, branch string) {
		cleanedBranch = branch
	}
	t.Cleanup(func() { cleanupTaskBranchFn = orig })

	task := mergeQueueTask{name: "exhaust.md", path: taskPath, branch: "task/exhaust"}
	if err := handleMergeFailure(repoRoot, tasksDir, task, errSquashMergeConflict); err != nil {
		t.Fatalf("handleMergeFailure: %v", err)
	}

	if cleanedBranch != "task/exhaust" {
		t.Fatalf("expected branch cleanup for %q, got %q", "task/exhaust", cleanedBranch)
	}

	// Verify task moved to failed/
	failedPath := filepath.Join(tasksDir, dirs.Failed, "exhaust.md")
	if _, err := os.Stat(failedPath); err != nil {
		t.Fatalf("task should be in failed/: %v", err)
	}
}

func TestHandleMergeFailure_MergeConflictCleanupRecordsTaskState(t *testing.T) {
	repoRoot := t.TempDir()
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyMerge, dirs.Backlog} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	taskPath := filepath.Join(tasksDir, dirs.ReadyMerge, "cleanup.md")
	content := strings.Join([]string{
		"<!-- branch: task/cleanup -->",
		"---",
		"max_retries: 3",
		"---",
		"# Cleanup",
		"",
	}, "\n")
	if err := os.WriteFile(taskPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, "cleanup.md", func(state *runtimedata.TaskState) {
		state.LastOutcome = runtimedata.OutcomeReviewApproved
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	task := mergeQueueTask{name: "cleanup.md", path: taskPath, branch: "task/cleanup"}
	if err := handleMergeFailure(repoRoot, tasksDir, task, errSquashMergeConflict); err != nil {
		t.Fatalf("handleMergeFailure: %v", err)
	}
	state, err := runtimedata.LoadTaskState(tasksDir, task.name)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeMergeConflictCleanup {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeMergeConflictCleanup)
	}
}

func TestHandleMergeFailure_BacklogDestinationCollisionConverges(t *testing.T) {
	repoRoot := t.TempDir()
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyMerge, dirs.Backlog} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	taskPath := filepath.Join(tasksDir, dirs.ReadyMerge, "collision.md")
	content := strings.Join([]string{
		"<!-- branch: task/collision -->",
		"---",
		"max_retries: 3",
		"---",
		"# Collision",
		"",
	}, "\n")
	if err := os.WriteFile(taskPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile taskPath: %v", err)
	}

	backlogPath := filepath.Join(tasksDir, dirs.Backlog, "collision.md")
	backlogContent := strings.Join([]string{
		"# Collision",
		"",
		"<!-- failure: merge-queue at 2026-01-01T00:00:00Z — squash merge conflict -->",
	}, "\n")
	if err := os.WriteFile(backlogPath, []byte(backlogContent), 0o644); err != nil {
		t.Fatalf("WriteFile backlogPath: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, "collision.md", func(state *runtimedata.TaskState) {
		state.LastOutcome = runtimedata.OutcomeReviewApproved
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	var cleanedBranch string
	orig := cleanupTaskBranchFn
	cleanupTaskBranchFn = func(root, branch string) {
		cleanedBranch = branch
	}
	t.Cleanup(func() { cleanupTaskBranchFn = orig })

	task := mergeQueueTask{name: "collision.md", path: taskPath, branch: "task/collision"}
	if err := handleMergeFailure(repoRoot, tasksDir, task, errSquashMergeConflict); err != nil {
		t.Fatalf("handleMergeFailure: %v", err)
	}

	if _, err := os.Stat(taskPath); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge copy should be removed after destination collision, got err=%v", err)
	}
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("ReadFile backlogPath: %v", err)
	}
	if got := strings.Count(string(data), "<!-- failure:"); got != 1 {
		t.Fatalf("backlog task should keep existing failure markers without duplicates, got %d", got)
	}
	if cleanedBranch != "task/collision" {
		t.Fatalf("expected branch cleanup for %q, got %q", "task/collision", cleanedBranch)
	}
	state, err := runtimedata.LoadTaskState(tasksDir, task.name)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeMergeConflictCleanup {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeMergeConflictCleanup)
	}
}

func TestHandleMergeFailure_FailedDestinationCollisionConverges(t *testing.T) {
	repoRoot := t.TempDir()
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyMerge, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	taskPath := filepath.Join(tasksDir, dirs.ReadyMerge, "collision.md")
	content := strings.Join([]string{
		"<!-- branch: task/collision -->",
		"---",
		"max_retries: 1",
		"---",
		"<!-- failure: prior-agent at 2026-01-01T00:00:00Z step=WORK error=prior -->",
		"# Collision",
		"",
	}, "\n")
	if err := os.WriteFile(taskPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile taskPath: %v", err)
	}

	failedPath := filepath.Join(tasksDir, dirs.Failed, "collision.md")
	failedContent := strings.Join([]string{
		"<!-- branch: task/collision -->",
		"# Collision",
		"",
		"<!-- failure: merge-queue at 2026-01-02T00:00:00Z — squash merge conflict -->",
	}, "\n")
	if err := os.WriteFile(failedPath, []byte(failedContent), 0o644); err != nil {
		t.Fatalf("WriteFile failedPath: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, "collision.md", func(state *runtimedata.TaskState) {
		state.LastOutcome = runtimedata.OutcomeReviewApproved
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	var cleanedBranch string
	orig := cleanupTaskBranchFn
	cleanupTaskBranchFn = func(root, branch string) {
		cleanedBranch = branch
	}
	t.Cleanup(func() { cleanupTaskBranchFn = orig })

	task := mergeQueueTask{name: "collision.md", path: taskPath, branch: "task/collision"}
	if err := handleMergeFailure(repoRoot, tasksDir, task, errSquashMergeConflict); err != nil {
		t.Fatalf("handleMergeFailure: %v", err)
	}

	if _, err := os.Stat(taskPath); !os.IsNotExist(err) {
		t.Fatalf("ready-to-merge copy should be removed after failed collision, got err=%v", err)
	}
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("ReadFile failedPath: %v", err)
	}
	if got := strings.Count(string(data), "<!-- failure:"); got != 1 {
		t.Fatalf("failed task should keep existing failure markers without duplicates, got %d", got)
	}
	if cleanedBranch != "task/collision" {
		t.Fatalf("expected branch cleanup for %q, got %q", "task/collision", cleanedBranch)
	}
	state, err := runtimedata.LoadTaskState(tasksDir, task.name)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should be removed after failed collision cleanup, got %+v", state)
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

func TestShouldFailTaskAfterNextFailure(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "below max retries before next failure",
			content: "---\nmax_retries: 3\n---\n# Task\n<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=x -->\n",
			want:    false,
		},
		{
			name: "next failure exhausts retries",
			content: "---\nmax_retries: 2\n---\n# Task\n" +
				"<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=x -->\n",
			want: true,
		},
		{
			name: "already at max retries",
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

			got := shouldFailTaskAfterNextFailure(path)
			if got != tt.want {
				t.Errorf("shouldFailTaskAfterNextFailure() = %v, want %v", got, tt.want)
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

func TestIsPermanentMoveError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"destination exists", queue.ErrDestinationExists, true},
		{"wrapped destination exists", fmt.Errorf("move: %w", queue.ErrDestinationExists), true},
		{"not exist", os.ErrNotExist, true},
		{"wrapped not exist", fmt.Errorf("open: %w", os.ErrNotExist), true},
		{"permission denied", os.ErrPermission, true},
		{"wrapped permission denied", fmt.Errorf("link: %w", os.ErrPermission), true},
		{"generic error", errors.New("temporary glitch"), false},
		{"nil error", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPermanentMoveError(tt.err)
			if got != tt.want {
				t.Errorf("isPermanentMoveError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMoveTaskWithRetry_PermanentErrorNoRetry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "task.md")
	dst := filepath.Join(dir, "dst", "task.md")
	if err := os.WriteFile(src, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var attempts atomic.Int32
	permErr := fmt.Errorf("atomic move: %w", queue.ErrDestinationExists)

	orig := atomicMoveFn
	atomicMoveFn = func(s, d string) error {
		attempts.Add(1)
		return permErr
	}
	t.Cleanup(func() { atomicMoveFn = orig })

	err := moveTaskWithRetry(context.Background(), src, dst)
	if err == nil {
		t.Fatal("expected error from moveTaskWithRetry")
	}
	if !errors.Is(err, queue.ErrDestinationExists) {
		t.Errorf("expected ErrDestinationExists, got %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("permanent error should cause exactly 1 attempt, got %d", got)
	}
}

func TestMoveTaskWithRetry_TransientErrorRetries(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "task.md")
	dst := filepath.Join(dir, "dst", "task.md")
	if err := os.WriteFile(src, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var attempts atomic.Int32
	transientErr := errors.New("temporary I/O glitch")

	orig := atomicMoveFn
	atomicMoveFn = func(s, d string) error {
		attempts.Add(1)
		return transientErr
	}
	t.Cleanup(func() { atomicMoveFn = orig })

	err := moveTaskWithRetry(context.Background(), src, dst)
	if err == nil {
		t.Fatal("expected error from moveTaskWithRetry")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("transient error should cause 3 attempts, got %d", got)
	}
}

func TestMoveTaskWithRetry_TransientThenSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "task.md")
	dst := filepath.Join(dir, "dst", "task.md")
	if err := os.WriteFile(src, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var attempts atomic.Int32

	orig := atomicMoveFn
	atomicMoveFn = func(s, d string) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("temporary glitch")
		}
		return queue.AtomicMove(s, d)
	}
	t.Cleanup(func() { atomicMoveFn = orig })

	if err := moveTaskWithRetry(context.Background(), src, dst); err != nil {
		t.Fatalf("moveTaskWithRetry: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 attempts (2 transient + 1 success), got %d", got)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("destination should exist: %v", err)
	}
}

func TestMoveTaskWithRetry_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "task.md")
	dst := filepath.Join(dir, "dst", "task.md")
	if err := os.WriteFile(src, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var attempts atomic.Int32

	orig := atomicMoveFn
	atomicMoveFn = func(s, d string) error {
		attempts.Add(1)
		return errors.New("temporary glitch")
	}
	t.Cleanup(func() { atomicMoveFn = orig })

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the first backoff select picks up ctx.Done().
	cancel()

	start := time.Now()
	err := moveTaskWithRetry(ctx, src, dst)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from moveTaskWithRetry with cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	// With a 100ms backoff, a non-cancelled run would take ≥200ms for 3 attempts.
	// Cancellation should return well under that.
	if elapsed > 50*time.Millisecond {
		t.Errorf("expected prompt return after cancellation, took %v", elapsed)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt before cancellation, got %d", got)
	}
}
