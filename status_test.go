package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestShowStatusWithPopulatedTasksDir(t *testing.T) {
	repoRoot := setupTestRepo(t)
	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := initMessaging(tasksDir); err != nil {
		t.Fatalf("initMessaging: %v", err)
	}

	files := map[string]string{
		filepath.Join(tasksDir, "waiting", "refactor-api.md"):    "---\nid: refactor-api\ndepends_on: [setup-models, add-auth]\n---\nRefactor API\n",
		filepath.Join(tasksDir, "backlog", "add-auth.md"):        "---\nid: add-auth\npriority: 10\n---\nAdd auth\n",
		filepath.Join(tasksDir, "in-progress", "agent-task.md"):  "In progress\n",
		filepath.Join(tasksDir, "ready-to-merge", "merge-me.md"): "Ready\n",
		filepath.Join(tasksDir, "completed", "setup-models.md"):  "---\nid: setup-models\n---\nDone\n",
		filepath.Join(tasksDir, "failed", "failed-task.md"):      "Failed\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", path, err)
		}
	}
	if err := os.WriteFile(filepath.Join(tasksDir, ".locks", "abcd1234.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("os.WriteFile lock: %v", err)
	}
	if err := writeMessage(tasksDir, Message{
		ID:     "msg1",
		From:   "agent-abcd1234",
		Type:   "intent",
		Task:   "refactor-api.md",
		Body:   "Starting work on refactor-api.md",
		SentAt: time.Date(2024, time.May, 1, 14, 30, 2, 0, time.UTC),
	}); err != nil {
		t.Fatalf("writeMessage: %v", err)
	}

	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = originalStdout }()

	callErr := showStatus([]string{"--repo", repoRoot})
	w.Close()
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if callErr != nil {
		t.Fatalf("showStatus: %v", callErr)
	}
}
