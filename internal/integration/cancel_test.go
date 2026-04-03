package integration_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"mato/internal/inspect"
	"mato/internal/queue"
	"mato/internal/status"
	"mato/internal/testutil"
)

func TestCancelRetryLifecycle(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirBacklog, "task.md"), "---\nid: task\n---\n# Task\n")

	if _, err := queue.CancelTask(tasksDir, "task"); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	var statusBuf bytes.Buffer
	if err := status.ShowJSON(&statusBuf, repoRoot); err != nil {
		t.Fatalf("status.ShowJSON: %v", err)
	}
	var statusResult status.StatusJSON
	if err := json.Unmarshal(statusBuf.Bytes(), &statusResult); err != nil {
		t.Fatalf("json.Unmarshal(status): %v", err)
	}
	if len(statusResult.Failed) != 1 || statusResult.Failed[0].FailureKind != "cancelled" {
		t.Fatalf("status failed = %#v, want cancelled task", statusResult.Failed)
	}

	var inspectBuf bytes.Buffer
	if err := inspect.ShowTo(&inspectBuf, repoRoot, "task", "json"); err != nil {
		t.Fatalf("inspect.ShowTo: %v", err)
	}
	var inspectResult map[string]any
	if err := json.Unmarshal(inspectBuf.Bytes(), &inspectResult); err != nil {
		t.Fatalf("json.Unmarshal(inspect): %v", err)
	}
	if inspectResult["failure_kind"] != "cancelled" {
		t.Fatalf("inspect failure_kind = %v, want cancelled", inspectResult["failure_kind"])
	}

	if _, err := queue.RetryTask(tasksDir, "task"); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	if _, err := queue.ResolveTask(queue.BuildIndex(tasksDir), "task"); err != nil {
		t.Fatalf("ResolveTask after retry: %v", err)
	}
	if snap := queue.BuildIndex(tasksDir).Snapshot(queue.DirBacklog, "task.md"); snap == nil {
		t.Fatal("expected task in backlog after retry")
	}
}
