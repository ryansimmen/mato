package integration_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"mato/internal/dirs"
	"mato/internal/inspect"
	"mato/internal/testutil"
)

func TestInspect_TextAndJSON(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.InProgress, "active.md", "---\nid: active\naffects: [pkg/client.go]\n---\n# Active\n")
	writeTask(t, tasksDir, dirs.Backlog, "candidate.md", "---\nid: candidate\naffects: [pkg/client.go]\n---\n# Candidate\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — add regression test -->\n")

	var textBuf bytes.Buffer
	if err := inspect.ShowTo(&textBuf, repoRoot, "candidate", "text"); err != nil {
		t.Fatalf("inspect.ShowTo(text): %v", err)
	}
	textOut := textBuf.String()
	if !strings.Contains(textOut, "Status: deferred") {
		t.Fatalf("text output missing deferred status:\n%s", textOut)
	}
	if !strings.Contains(textOut, "Review history: previously rejected: add regression test") {
		t.Fatalf("text output missing review history:\n%s", textOut)
	}
	if !strings.Contains(textOut, "File: backlog/candidate.md") {
		t.Fatalf("text output missing file location:\n%s", textOut)
	}

	var jsonBuf bytes.Buffer
	if err := inspect.ShowTo(&jsonBuf, repoRoot, "candidate", "json"); err != nil {
		t.Fatalf("inspect.ShowTo(json): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(jsonBuf.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got["status"] != "deferred" {
		t.Fatalf("status = %v, want deferred", got["status"])
	}
	blockingTask, ok := got["blocking_task"].(map[string]any)
	if !ok || blockingTask["filename"] != "active.md" {
		t.Fatalf("blocking_task = %v, want active.md", got["blocking_task"])
	}
}
