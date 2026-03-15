package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/status"
)

func setupTestRepo(t *testing.T) (repoRoot string, tasksDir string) {
	t.Helper()

	dir := t.TempDir()
	mustGitOutput(t, dir, "init")
	mustGitOutput(t, dir, "config", "user.email", "test@test.com")
	mustGitOutput(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "README.md"), "# Test\n")
	mustGitOutput(t, dir, "add", "-A")
	mustGitOutput(t, dir, "commit", "-m", "initial")
	mustGitOutput(t, dir, "checkout", "-b", "mato")

	tasksDir = filepath.Join(dir, ".tasks")
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", sub, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("os.MkdirAll(.locks): %v", err)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}
	mustGitOutput(t, dir, "config", "receive.denyCurrentBranch", "updateInstead")
	return dir, tasksDir
}

func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := git.Output(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func configureCloneIdentity(t *testing.T, cloneDir string) {
	t.Helper()
	mustGitOutput(t, cloneDir, "config", "user.email", "test@test.com")
	mustGitOutput(t, cloneDir, "config", "user.name", "Test")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func mustRename(t *testing.T, oldPath, newPath string) {
	t.Helper()
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("os.Rename(%s, %s): %v", oldPath, newPath, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist, got %v", path, err)
	}
}

func writeTask(t *testing.T, tasksDir, queueDir, name, content string) string {
	t.Helper()
	path := filepath.Join(tasksDir, queueDir, name)
	writeFile(t, path, content)
	return path
}

func createTaskBranch(t *testing.T, repoRoot, branch string, files map[string]string, message string) {
	t.Helper()

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	defer git.RemoveClone(cloneDir)

	configureCloneIdentity(t, cloneDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", branch, "mato")
	for name, content := range files {
		writeFile(t, filepath.Join(cloneDir, name), content)
	}
	mustGitOutput(t, cloneDir, "add", "-A")
	mustGitOutput(t, cloneDir, "commit", "-m", message)
	mustGitOutput(t, cloneDir, "push", "origin", branch)
}

func TestFullTaskLifecycleNoDeps(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)

	backlogTask := writeTask(t, tasksDir, "backlog", "add-hello.md", "# Add hello\nCreate hello.txt with \"hello world\"\n")

	if got := queue.ReconcileReadyQueue(tasksDir); got != 0 {
		t.Fatalf("queue.ReconcileReadyQueue() = %d, want 0", got)
	}
	if err := queue.WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("queue.WriteQueueManifest: %v", err)
	}
	if !queue.HasAvailableTasks(tasksDir) {
		t.Fatal("queue.HasAvailableTasks() = false, want true")
	}

	manifest := readFile(t, filepath.Join(tasksDir, ".queue"))
	if manifest != "add-hello.md\n" {
		t.Fatalf("queue manifest = %q, want %q", manifest, "add-hello.md\n")
	}

	meta, body, err := frontmatter.ParseTaskFile(backlogTask)
	if err != nil {
		t.Fatalf("frontmatter.ParseTaskFile: %v", err)
	}
	if meta.Priority != 50 {
		t.Fatalf("plain markdown task priority = %d, want 50", meta.Priority)
	}
	if !strings.Contains(body, "Create hello.txt") {
		t.Fatalf("task body = %q, want instruction text", body)
	}

	inProgressTask := filepath.Join(tasksDir, "in-progress", "add-hello.md")
	mustRename(t, backlogTask, inProgressTask)

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	defer git.RemoveClone(cloneDir)

	configureCloneIdentity(t, cloneDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/add-hello", "mato")
	writeFile(t, filepath.Join(cloneDir, "hello.txt"), "hello world\n")
	mustGitOutput(t, cloneDir, "add", "-A")
	mustGitOutput(t, cloneDir, "commit", "-m", "add hello")
	mustGitOutput(t, cloneDir, "push", "origin", "task/add-hello")

	readyTask := filepath.Join(tasksDir, "ready-to-merge", "add-hello.md")
	mustRename(t, inProgressTask, readyTask)

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	completedTask := filepath.Join(tasksDir, "completed", "add-hello.md")
	mustExist(t, completedTask)
	mustNotExist(t, readyTask)

	hello, err := git.Output(repoRoot, "show", "mato:hello.txt")
	if err != nil {
		t.Fatalf("git show mato:hello.txt: %v", err)
	}
	if strings.TrimSpace(hello) != "hello world" {
		t.Fatalf("hello.txt contents = %q, want %q", strings.TrimSpace(hello), "hello world")
	}
}

func TestDependencyChainPromotion(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	writeTask(t, tasksDir, "waiting", "task-a.md", "---\nid: task-a\npriority: 1\n---\n# Task A\n")
	writeTask(t, tasksDir, "waiting", "task-b.md", "---\nid: task-b\npriority: 2\ndepends_on: [task-a]\n---\n# Task B\n")
	writeTask(t, tasksDir, "waiting", "task-c.md", "---\nid: task-c\npriority: 3\ndepends_on: [task-b]\n---\n# Task C\n")

	if got := queue.ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("first queue.ReconcileReadyQueue() = %d, want 1", got)
	}
	mustExist(t, filepath.Join(tasksDir, "backlog", "task-a.md"))
	mustNotExist(t, filepath.Join(tasksDir, "waiting", "task-a.md"))
	mustExist(t, filepath.Join(tasksDir, "waiting", "task-b.md"))
	mustExist(t, filepath.Join(tasksDir, "waiting", "task-c.md"))

	mustRename(t,
		filepath.Join(tasksDir, "backlog", "task-a.md"),
		filepath.Join(tasksDir, "completed", "task-a.md"),
	)

	if got := queue.ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("second queue.ReconcileReadyQueue() = %d, want 1", got)
	}
	mustExist(t, filepath.Join(tasksDir, "backlog", "task-b.md"))
	mustExist(t, filepath.Join(tasksDir, "waiting", "task-c.md"))

	mustRename(t,
		filepath.Join(tasksDir, "backlog", "task-b.md"),
		filepath.Join(tasksDir, "completed", "task-b.md"),
	)

	if got := queue.ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("third queue.ReconcileReadyQueue() = %d, want 1", got)
	}
	mustExist(t, filepath.Join(tasksDir, "backlog", "task-c.md"))
}

func TestOverlapPrevention(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	writeTask(t, tasksDir, "backlog", "high.md", "---\npriority: 5\naffects: [main.go]\n---\n# High\n")
	writeTask(t, tasksDir, "backlog", "low.md", "---\npriority: 20\naffects: [main.go]\n---\n# Low\n")
	writeTask(t, tasksDir, "backlog", "other.md", "---\npriority: 10\naffects: [README.md]\n---\n# Other\n")

	queue.RemoveOverlappingTasks(tasksDir)

	mustExist(t, filepath.Join(tasksDir, "backlog", "high.md"))
	mustExist(t, filepath.Join(tasksDir, "backlog", "other.md"))
	mustExist(t, filepath.Join(tasksDir, "waiting", "low.md"))
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "low.md"))

	mustRename(t,
		filepath.Join(tasksDir, "backlog", "high.md"),
		filepath.Join(tasksDir, "completed", "high.md"),
	)

	if got := queue.ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("queue.ReconcileReadyQueue() after completion = %d, want 1", got)
	}

	queue.RemoveOverlappingTasks(tasksDir)

	mustExist(t, filepath.Join(tasksDir, "backlog", "low.md"))
	mustExist(t, filepath.Join(tasksDir, "backlog", "other.md"))
}

func TestOrphanRecoveryAndRequeue(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	inProgressTask := writeTask(t, tasksDir, "in-progress", "recover-me.md", "<!-- claimed-by: dead-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Recover me\nTry again.\n")
	writeFile(t, filepath.Join(tasksDir, ".locks", "dead-agent.pid"), "2147483647")

	queue.RecoverOrphanedTasks(tasksDir)

	backlogTask := filepath.Join(tasksDir, "backlog", "recover-me.md")
	mustExist(t, backlogTask)
	mustNotExist(t, inProgressTask)
	if contents := readFile(t, backlogTask); !strings.Contains(contents, "<!-- failure: mato-recovery") {
		t.Fatalf("recovered task missing failure record: %q", contents)
	}

	if err := queue.WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("queue.WriteQueueManifest: %v", err)
	}
	if manifest := readFile(t, filepath.Join(tasksDir, ".queue")); !strings.Contains(manifest, "recover-me.md") {
		t.Fatalf("queue manifest = %q, want recovered task", manifest)
	}
}

func TestMergeConflictHandling(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"README.md": "alpha\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"README.md": "beta\n"}, "beta change")

	writeTask(t, tasksDir, "ready-to-merge", "alpha.md", "---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, "ready-to-merge", "beta.md", "---\npriority: 10\n---\n# Beta\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, "completed", "alpha.md"))
	mustNotExist(t, filepath.Join(tasksDir, "ready-to-merge", "alpha.md"))
	mustExist(t, filepath.Join(tasksDir, "backlog", "beta.md"))
	mustNotExist(t, filepath.Join(tasksDir, "ready-to-merge", "beta.md"))
	if contents := readFile(t, filepath.Join(tasksDir, "backlog", "beta.md")); !strings.Contains(contents, "<!-- failure: merge-queue") {
		t.Fatalf("beta task missing merge failure record: %q", contents)
	}

	readme := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:README.md"))
	if readme != "alpha" {
		t.Fatalf("README on mato = %q, want %q", readme, "alpha")
	}
}

func TestMessagingLifecycle(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	base := time.Unix(1_700_000_000, 0).UTC()
	messages := []messaging.Message{
		{ID: "msg-1", From: "agent-a", Type: "status", Body: "first", SentAt: base},
		{ID: "msg-2", From: "agent-b", Type: "status", Body: "second", SentAt: base.Add(time.Second)},
		{ID: "msg-3", From: "agent-c", Type: "result", Body: "third", SentAt: base.Add(2 * time.Second)},
	}
	for _, msg := range messages {
		if err := messaging.WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("messaging.WriteMessage(%s): %v", msg.ID, err)
		}
	}

	all, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("messaging.ReadMessages(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(all messages) = %d, want 3", len(all))
	}
	for i, want := range messages {
		if all[i].ID != want.ID {
			t.Fatalf("all[%d].ID = %q, want %q", i, all[i].ID, want.ID)
		}
	}

	afterFirst, err := messaging.ReadMessages(tasksDir, messages[0].SentAt)
	if err != nil {
		t.Fatalf("messaging.ReadMessages(after first): %v", err)
	}
	if len(afterFirst) != 2 {
		t.Fatalf("len(messages after first) = %d, want 2", len(afterFirst))
	}
	if afterFirst[0].ID != "msg-2" || afterFirst[1].ID != "msg-3" {
		t.Fatalf("messages after first = [%s %s], want [msg-2 msg-3]", afterFirst[0].ID, afterFirst[1].ID)
	}

	time.Sleep(10 * time.Millisecond)
	messaging.CleanOldMessages(tasksDir, 0)

	remaining, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("messaging.ReadMessages(remaining): %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("len(remaining messages) = %d, want 0", len(remaining))
	}
}

func TestStatusWithPopulatedQueue(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)

	writeTask(t, tasksDir, "waiting", "waiting.md", "---\nid: waiting\ndepends_on: [done]\n---\n# Waiting\n")
	writeTask(t, tasksDir, "backlog", "backlog.md", "# Backlog\n")
	writeTask(t, tasksDir, "in-progress", "working.md", "<!-- claimed-by: status-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Working\n")
	writeTask(t, tasksDir, "ready-to-merge", "ready.md", "# Ready\n")
	writeTask(t, tasksDir, "completed", "done.md", "---\nid: done\n---\n# Done\n")
	writeTask(t, tasksDir, "failed", "failed.md", "# Failed\n")

	for i := range []string{"one", "two"} {
		msg := messaging.Message{
			ID:     fmt.Sprintf("status-msg-%d", i+1),
			From:   "status-agent",
			Type:   "status",
			Body:   fmt.Sprintf("message %d", i+1),
			SentAt: time.Unix(1_700_000_100+int64(i), 0).UTC(),
		}
		if err := messaging.WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("messaging.WriteMessage(%s): %v", msg.ID, err)
		}
	}

	writeFile(t, filepath.Join(tasksDir, ".locks", "status-agent.pid"), fmt.Sprintf("%d", os.Getpid()))

	if err := status.Show(repoRoot, ""); err != nil {
		t.Fatalf("status.Show: %v", err)
	}
}
