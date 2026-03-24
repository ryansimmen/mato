package integration_test

import (
	"bytes"
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
	"mato/internal/testutil"
)

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
	testutil.WriteFile(t, path, content)
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
		testutil.WriteFile(t, filepath.Join(cloneDir, name), content)
	}
	mustGitOutput(t, cloneDir, "add", "-A")
	mustGitOutput(t, cloneDir, "commit", "-m", message)
	mustGitOutput(t, cloneDir, "push", "origin", branch)
}

func TestFullTaskLifecycleNoDeps(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	backlogTask := writeTask(t, tasksDir, queue.DirBacklog, "add-hello.md", "# Add hello\nCreate hello.txt with \"hello world\"\n")

	if got := queue.ReconcileReadyQueue(tasksDir, nil); got {
		t.Fatalf("queue.ReconcileReadyQueue() = %v, want false", got)
	}
	if err := queue.WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("queue.WriteQueueManifest: %v", err)
	}
	if !queue.HasAvailableTasks(tasksDir, nil) {
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

	inProgressTask := filepath.Join(tasksDir, queue.DirInProgress, "add-hello.md")
	mustRename(t, backlogTask, inProgressTask)

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	defer git.RemoveClone(cloneDir)

	configureCloneIdentity(t, cloneDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/add-hello", "mato")
	testutil.WriteFile(t, filepath.Join(cloneDir, "hello.txt"), "hello world\n")
	mustGitOutput(t, cloneDir, "add", "-A")
	mustGitOutput(t, cloneDir, "commit", "-m", "add hello")
	mustGitOutput(t, cloneDir, "push", "origin", "task/add-hello")

	readyTask := filepath.Join(tasksDir, queue.DirReadyMerge, "add-hello.md")
	mustRename(t, inProgressTask, readyTask)

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	completedTask := filepath.Join(tasksDir, queue.DirCompleted, "add-hello.md")
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
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirWaiting, "task-a.md", "---\nid: task-a\npriority: 1\n---\n# Task A\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-b.md", "---\nid: task-b\npriority: 2\ndepends_on: [task-a]\n---\n# Task B\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-c.md", "---\nid: task-c\npriority: 3\ndepends_on: [task-b]\n---\n# Task C\n")

	if got := queue.ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("first queue.ReconcileReadyQueue() = false, want true")
	}
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-a.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-b.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-c.md"))

	mustRename(t,
		filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"),
		filepath.Join(tasksDir, queue.DirCompleted, "task-a.md"),
	)

	if got := queue.ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("second queue.ReconcileReadyQueue() = false, want true")
	}
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-c.md"))

	mustRename(t,
		filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"),
		filepath.Join(tasksDir, queue.DirCompleted, "task-b.md"),
	)

	if got := queue.ReconcileReadyQueue(tasksDir, nil); !got {
		t.Fatal("third queue.ReconcileReadyQueue() = false, want true")
	}
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-c.md"))
}

func TestOverlapPrevention(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirBacklog, "high.md", "---\npriority: 5\naffects: [main.go]\n---\n# High\n")
	writeTask(t, tasksDir, queue.DirBacklog, "low.md", "---\npriority: 20\naffects: [main.go]\n---\n# Low\n")
	writeTask(t, tasksDir, queue.DirBacklog, "other.md", "---\npriority: 10\naffects: [README.md]\n---\n# Other\n")

	deferred := queue.DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["low.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "low.md", deferred)
	}
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "high.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "other.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "low.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirWaiting, "low.md"))

	mustRename(t,
		filepath.Join(tasksDir, queue.DirBacklog, "high.md"),
		filepath.Join(tasksDir, queue.DirCompleted, "high.md"),
	)

	if got := queue.ReconcileReadyQueue(tasksDir, nil); got {
		t.Fatalf("queue.ReconcileReadyQueue() after completion = %v, want false", got)
	}

	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if len(deferred) != 0 {
		t.Fatalf("len(deferred) = %d, want 0", len(deferred))
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "low.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "other.md"))
}

func TestOrphanRecoveryAndRequeue(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	inProgressTask := writeTask(t, tasksDir, queue.DirInProgress, "recover-me.md", "<!-- claimed-by: dead-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Recover me\nTry again.\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "dead-agent.pid"), "2147483647")

	queue.RecoverOrphanedTasks(tasksDir)

	backlogTask := filepath.Join(tasksDir, queue.DirBacklog, "recover-me.md")
	mustExist(t, backlogTask)
	mustNotExist(t, inProgressTask)
	if contents := readFile(t, backlogTask); !strings.Contains(contents, "<!-- failure: mato-recovery") {
		t.Fatalf("recovered task missing failure record: %q", contents)
	}

	if err := queue.WriteQueueManifest(tasksDir, nil, nil); err != nil {
		t.Fatalf("queue.WriteQueueManifest: %v", err)
	}
	if manifest := readFile(t, filepath.Join(tasksDir, ".queue")); !strings.Contains(manifest, "recover-me.md") {
		t.Fatalf("queue manifest = %q, want recovered task", manifest)
	}
}

func TestMergeConflictHandling(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"README.md": "alpha\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"README.md": "beta\n"}, "beta change")

	writeTask(t, tasksDir, queue.DirReadyMerge, "alpha.md", "---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, queue.DirReadyMerge, "beta.md", "---\npriority: 10\n---\n# Beta\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "alpha.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "alpha.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "beta.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "beta.md"))
	if contents := readFile(t, filepath.Join(tasksDir, queue.DirBacklog, "beta.md")); !strings.Contains(contents, "<!-- failure: merge-queue") {
		t.Fatalf("beta task missing merge failure record: %q", contents)
	}

	readme := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:README.md"))
	if readme != "alpha" {
		t.Fatalf("README on mato = %q, want %q", readme, "alpha")
	}
}

func TestMessagingLifecycle(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	base := time.Unix(1_700_000_000, 0).UTC()
	messages := []messaging.Message{
		{ID: "msg-1", From: "agent-a", Type: "intent", Body: "first", SentAt: base},
		{ID: "msg-2", From: "agent-b", Type: "intent", Body: "second", SentAt: base.Add(time.Second)},
		{ID: "msg-3", From: "agent-c", Type: "completion", Body: "third", SentAt: base.Add(2 * time.Second)},
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

func TestGlobDeferral(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// High-priority task with glob affects.
	writeTask(t, tasksDir, queue.DirBacklog, "high.md", "---\npriority: 5\naffects:\n  - internal/runner/*.go\n---\n# High\n")
	// Low-priority task with overlapping glob.
	writeTask(t, tasksDir, queue.DirBacklog, "low.md", "---\npriority: 20\naffects:\n  - internal/runner/*_test.go\n---\n# Low\n")
	// Non-overlapping task.
	writeTask(t, tasksDir, queue.DirBacklog, "other.md", "---\npriority: 10\naffects:\n  - pkg/client/*.go\n---\n# Other\n")

	deferred := queue.DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1; deferred = %v", len(deferred), deferred)
	}
	if _, ok := deferred["low.md"]; !ok {
		t.Fatalf("deferred set missing %q: %v", "low.md", deferred)
	}
	// All tasks stay in backlog (deferred tasks are skipped, not moved).
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "high.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "low.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "other.md"))
}

func TestMixedGlobExactOverlap(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// Active task with exact file path.
	writeTask(t, tasksDir, queue.DirInProgress, "active.md",
		"<!-- claimed-by: test-agent  claimed-at: 2026-01-01T00:00:00Z -->\n---\npriority: 1\naffects:\n  - internal/runner/task.go\n---\n# Active\n")
	// Backlog task with glob that matches the active task's exact path.
	writeTask(t, tasksDir, queue.DirBacklog, "glob-task.md", "---\npriority: 10\naffects:\n  - internal/runner/*.go\n---\n# Glob Task\n")
	// Backlog task with non-overlapping glob.
	writeTask(t, tasksDir, queue.DirBacklog, "safe-task.md", "---\npriority: 10\naffects:\n  - pkg/**/*.go\n---\n# Safe\n")

	deferred := queue.DeferredOverlappingTasks(tasksDir, nil)

	if _, ok := deferred["glob-task.md"]; !ok {
		t.Fatalf("expected glob-task.md to be deferred (overlaps with active exact path), got deferred = %v", deferred)
	}
	if _, ok := deferred["safe-task.md"]; ok {
		t.Fatalf("safe-task.md should not be deferred (no overlap), got deferred = %v", deferred)
	}
}

func TestStatusWithPopulatedQueue(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirWaiting, "waiting.md", "---\nid: waiting\ndepends_on: [done]\n---\n# Waiting\n")
	writeTask(t, tasksDir, queue.DirBacklog, "backlog.md", "# Backlog\n")
	writeTask(t, tasksDir, queue.DirInProgress, "working.md", "<!-- claimed-by: status-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Working\n")
	writeTask(t, tasksDir, queue.DirReadyMerge, "ready.md", "# Ready\n")
	writeTask(t, tasksDir, queue.DirCompleted, "done.md", "---\nid: done\n---\n# Done\n")
	writeTask(t, tasksDir, queue.DirFailed, "failed.md", "# Failed\n")

	for i := range []string{"one", "two"} {
		msg := messaging.Message{
			ID:     fmt.Sprintf("status-msg-%d", i+1),
			From:   "status-agent",
			Type:   "intent",
			Body:   fmt.Sprintf("message %d", i+1),
			SentAt: time.Unix(1_700_000_100+int64(i), 0).UTC(),
		}
		if err := messaging.WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("messaging.WriteMessage(%s): %v", msg.ID, err)
		}
	}

	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "status-agent.pid"), fmt.Sprintf("%d", os.Getpid()))

	if err := status.Show(repoRoot, ""); err != nil {
		t.Fatalf("status.Show: %v", err)
	}
}

func TestStatusWithParseFailedTasksPreservesRuntimeMetadata(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirInProgress, "broken-active.md",
		"<!-- claimed-by: status-agent  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"---\npriority: nope\n---\n# Broken active\n")
	writeTask(t, tasksDir, queue.DirFailed, "broken-failed.md",
		"<!-- terminal-failure: mato at 2026-01-02T00:00:00Z — invalid frontmatter -->\n"+
			"---\npriority: nope\n---\n# Broken failed\n")

	var buf bytes.Buffer
	if err := status.ShowTo(&buf, repoRoot, ""); err != nil {
		t.Fatalf("status.ShowTo: %v", err)
	}
	output := buf.String()

	if !strings.Contains(output, "broken-active.md") {
		t.Fatalf("status output missing parse-failed in-progress task:\n%s", output)
	}
	if !strings.Contains(output, "agent status-agent") {
		t.Fatalf("status output missing claimed-by metadata for parse-failed task:\n%s", output)
	}
	if !strings.Contains(output, "broken-failed.md") {
		t.Fatalf("status output missing parse-failed failed task:\n%s", output)
	}
	if !strings.Contains(output, "structural failure: invalid frontmatter") {
		t.Fatalf("status output missing terminal failure reason for parse-failed failed task:\n%s", output)
	}
}

func TestDAG_ChainPromotion(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// A is completed, B depends on A, C depends on B.
	writeTask(t, tasksDir, queue.DirCompleted, "task-a.md", "---\nid: task-a\n---\n# A\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-b.md", "---\nid: task-b\ndepends_on: [task-a]\n---\n# B\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-c.md", "---\nid: task-c\ndepends_on: [task-b]\n---\n# C\n")

	// First reconcile: promotes B only.
	moved := queue.ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatal("first reconcile: moved = false, want true")
	}
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-c.md"))

	// C is still blocked by B (now in backlog, not completed).
	moved2 := queue.ReconcileReadyQueue(tasksDir, nil)
	if moved2 {
		t.Fatal("second reconcile: moved = true, want false (B not completed yet)")
	}
}

func TestDAG_CycleMovesToFailed(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// 2-node cycle: A -> B -> A. Downstream C -> A stays in waiting.
	writeTask(t, tasksDir, queue.DirWaiting, "task-a.md", "---\nid: task-a\ndepends_on: [task-b]\n---\n# A\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-b.md", "---\nid: task-b\ndepends_on: [task-a]\n---\n# B\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-c.md", "---\nid: task-c\ndepends_on: [task-a]\n---\n# C (downstream)\n")

	moved := queue.ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatal("moved = false, want true")
	}

	// Cycle members should be in failed/ with cycle-failure markers.
	for _, name := range []string{"task-a.md", "task-b.md"} {
		failedPath := filepath.Join(tasksDir, queue.DirFailed, name)
		mustExist(t, failedPath)
		data := readFile(t, failedPath)
		if !strings.Contains(data, "<!-- cycle-failure:") {
			t.Fatalf("%s should contain cycle-failure marker", name)
		}
		mustNotExist(t, filepath.Join(tasksDir, queue.DirWaiting, name))
	}

	// Downstream task should remain in waiting.
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-c.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirFailed, "task-c.md"))
}

func TestDAG_LongCycleMovesToFailed(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// 3-node cycle: A -> C, B -> A, C -> B. Downstream D -> C.
	writeTask(t, tasksDir, queue.DirWaiting, "task-a.md", "---\nid: task-a\ndepends_on: [task-c]\n---\n# A\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-b.md", "---\nid: task-b\ndepends_on: [task-a]\n---\n# B\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-c.md", "---\nid: task-c\ndepends_on: [task-b]\n---\n# C\n")
	writeTask(t, tasksDir, queue.DirWaiting, "task-d.md", "---\nid: task-d\ndepends_on: [task-c]\n---\n# D\n")

	moved := queue.ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatal("moved = false, want true")
	}

	// All 3 cycle members should be in failed/.
	for _, name := range []string{"task-a.md", "task-b.md", "task-c.md"} {
		failedPath := filepath.Join(tasksDir, queue.DirFailed, name)
		mustExist(t, failedPath)
		data := readFile(t, failedPath)
		if !strings.Contains(data, "<!-- cycle-failure:") {
			t.Fatalf("%s should contain cycle-failure marker", name)
		}
	}

	// Downstream D should stay in waiting.
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-d.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirFailed, "task-d.md"))
}

func TestDAG_AmbiguousID(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// ID "shared" in both completed and waiting — ambiguous.
	writeTask(t, tasksDir, queue.DirCompleted, "shared-done.md", "---\nid: shared\n---\n# Done\n")
	writeTask(t, tasksDir, queue.DirWaiting, "shared-waiting.md", "---\nid: shared\n---\n# Still waiting\n")

	// dependent task depends on the ambiguous ID.
	writeTask(t, tasksDir, queue.DirWaiting, "dependent.md", "---\nid: dependent\ndepends_on: [shared]\n---\n# Dependent\n")

	moved := queue.ReconcileReadyQueue(tasksDir, nil)

	// shared-waiting has no deps so it gets promoted. dependent stays blocked.
	if !moved {
		t.Fatal("moved = false, want true (only shared-waiting)")
	}

	// dependent should remain in waiting (ambiguous ID not satisfied).
	mustExist(t, filepath.Join(tasksDir, queue.DirWaiting, "dependent.md"))
}
