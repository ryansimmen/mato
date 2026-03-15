package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRecoverOrphanedTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", "completed", "failed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	orphan := filepath.Join(tasksDir, "in-progress", "fix-bug.md")
	os.WriteFile(orphan, []byte("# Fix bug\nDo the thing.\n"), 0o644)

	recoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphaned task was not removed from in-progress/")
	}
	recovered := filepath.Join(tasksDir, "backlog", "fix-bug.md")
	data, err := os.ReadFile(recovered)
	if err != nil {
		t.Fatalf("recovered task not found in backlog/: %v", err)
	}
	if !strings.Contains(string(data), "# Fix bug") {
		t.Error("recovered task lost original content")
	}
	if !strings.Contains(string(data), "<!-- failure: mato-recovery") {
		t.Error("recovered task missing failure record")
	}
}

func TestRecoverOrphanedTasks_IgnoresNonMd(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	other := filepath.Join(tasksDir, "in-progress", "notes.txt")
	os.WriteFile(other, []byte("hello"), 0o644)

	recoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.md file should not be moved: %v", err)
	}
}

func TestRecoverOrphanedTasks_SkipsActiveAgent(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", ".locks"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	agentID := "active-agent"
	task := filepath.Join(tasksDir, "in-progress", "active-task.md")
	content := fmt.Sprintf("<!-- claimed-by: %s  claimed-at: 2026-01-01T00:00:00Z -->\n# Active task\n", agentID)
	os.WriteFile(task, []byte(content), 0o644)
	os.WriteFile(filepath.Join(tasksDir, ".locks", agentID+".pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)

	recoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(task); err != nil {
		t.Fatal("task claimed by active agent should NOT be recovered")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "active-task.md")); err == nil {
		t.Fatal("task claimed by active agent should NOT appear in backlog")
	}
}

func TestParseClaimedBy(t *testing.T) {
	dir := t.TempDir()

	withClaim := filepath.Join(dir, "task.md")
	os.WriteFile(withClaim, []byte("<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n# Do stuff\n"), 0o644)
	if got := parseClaimedBy(withClaim); got != "abc123" {
		t.Errorf("parseClaimedBy = %q, want %q", got, "abc123")
	}

	noClaim := filepath.Join(dir, "plain.md")
	os.WriteFile(noClaim, []byte("# Just a task\n"), 0o644)
	if got := parseClaimedBy(noClaim); got != "" {
		t.Errorf("parseClaimedBy = %q, want empty", got)
	}

	if got := parseClaimedBy(filepath.Join(dir, "missing.md")); got != "" {
		t.Errorf("parseClaimedBy = %q, want empty for missing file", got)
	}
}

func TestIsAgentActive(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	if isAgentActive(tasksDir, "") {
		t.Error("empty agent ID should not be active")
	}
	if isAgentActive(tasksDir, "no-such-agent") {
		t.Error("agent without lock file should not be active")
	}

	os.WriteFile(filepath.Join(locksDir, "live.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if !isAgentActive(tasksDir, "live") {
		t.Error("agent with current PID should be active")
	}

	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647"), 0o644)
	if isAgentActive(tasksDir, "dead") {
		t.Error("agent with dead PID should not be active")
	}
}

func TestHasAvailableTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", "completed", "failed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	if hasAvailableTasks(tasksDir) {
		t.Fatal("expected no available tasks in empty dirs")
	}

	os.WriteFile(filepath.Join(tasksDir, "backlog", "notes.txt"), []byte("hi"), 0o644)
	if hasAvailableTasks(tasksDir) {
		t.Fatal("non-.md file should not count as an available task")
	}

	os.WriteFile(filepath.Join(tasksDir, "backlog", "task1.md"), []byte("# Task 1\n"), 0o644)
	if !hasAvailableTasks(tasksDir) {
		t.Fatal("expected available task in backlog")
	}

	os.Remove(filepath.Join(tasksDir, "backlog", "task1.md"))
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "task2.md"), []byte("# Task 2\n"), 0o644)
	if hasAvailableTasks(tasksDir) {
		t.Fatal("in-progress tasks should not count as available")
	}

	os.WriteFile(filepath.Join(tasksDir, "backlog", "task3.md"), []byte("# Task 3\n"), 0o644)
	if !hasAvailableTasks(tasksDir) {
		t.Fatal("expected available task in backlog")
	}
}

func TestRegisterAgent(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, err := registerAgent(tasksDir, "test-agent")
	if err != nil {
		t.Fatalf("registerAgent: %v", err)
	}

	lockFile := filepath.Join(tasksDir, ".locks", "test-agent.pid")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file PID = %q, want %q", got, strconv.Itoa(os.Getpid()))
	}

	cleanup()

	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove lock file")
	}
}

func TestCleanStaleLocks(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	os.WriteFile(filepath.Join(locksDir, "alive.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647"), 0o644)

	cleanStaleLocks(tasksDir)

	if _, err := os.Stat(filepath.Join(locksDir, "alive.pid")); err != nil {
		t.Error("live lock should not be removed")
	}
	if _, err := os.Stat(filepath.Join(locksDir, "dead.pid")); !os.IsNotExist(err) {
		t.Error("stale lock should be removed")
	}
}

func TestReconcileReadyQueue_PromotesWhenDepsMet(t *testing.T) {
tasksDir := t.TempDir()
for _, sub := range []string{"waiting", "backlog", "completed"} {
os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
}

os.WriteFile(filepath.Join(tasksDir, "completed", "different-name.md"), []byte("---\nid: dep-a\n---\nDone\n"), 0o644)
os.WriteFile(filepath.Join(tasksDir, "completed", "dep-b.md"), []byte("Done\n"), 0o644)

waitingPath := filepath.Join(tasksDir, "waiting", "task.md")
os.WriteFile(waitingPath, []byte("---\ndepends_on: [dep-a, dep-b]\n---\nReady now\n"), 0o644)

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
os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
}

waitingPath := filepath.Join(tasksDir, "waiting", "blocked-task.md")
os.WriteFile(waitingPath, []byte("---\ndepends_on:\n  - missing-task\n---\nStill blocked\n"), 0o644)

if got := reconcileReadyQueue(tasksDir); got != 0 {
t.Fatalf("reconcileReadyQueue() = %d, want 0", got)
}
if _, err := os.Stat(waitingPath); err != nil {
t.Fatalf("task with unmet deps should stay in waiting: %v", err)
}
}

func TestReconcileReadyQueue_PromotesTaskWithNoDeps(t *testing.T) {
tasksDir := t.TempDir()
for _, sub := range []string{"waiting", "backlog", "completed"} {
os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
}

os.WriteFile(filepath.Join(tasksDir, "waiting", "solo-task.md"), []byte("# Solo\n"), 0o644)

if got := reconcileReadyQueue(tasksDir); got != 1 {
t.Fatalf("reconcileReadyQueue() = %d, want 1", got)
}
if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "solo-task.md")); err != nil {
t.Fatalf("promoted task missing from backlog: %v", err)
}
}

func TestWriteQueueManifest_SortsByPriorityThenFilename(t *testing.T) {
tasksDir := t.TempDir()
os.MkdirAll(filepath.Join(tasksDir, "backlog"), 0o755)

for name, content := range map[string]string{
"z-low.md":     "---\npriority: 20\n---\nBody\n",
"b-high.md":    "---\npriority: 5\n---\nBody\n",
"a-high.md":    "---\npriority: 5\n---\nBody\n",
"c-default.md": "Body\n",
} {
os.WriteFile(filepath.Join(tasksDir, "backlog", name), []byte(content), 0o644)
}

if err := writeQueueManifest(tasksDir); err != nil {
t.Fatalf("writeQueueManifest: %v", err)
}

data, _ := os.ReadFile(filepath.Join(tasksDir, ".queue"))
want := "a-high.md\nb-high.md\nz-low.md\nc-default.md\n"
if string(data) != want {
t.Fatalf("manifest = %q, want %q", string(data), want)
}
}

func TestRemoveOverlappingTasks_DefersLowerPriorityTask(t *testing.T) {
tasksDir := t.TempDir()
for _, sub := range []string{"waiting", "backlog"} {
os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
}

for name, content := range map[string]string{
"high-priority.md": "---\npriority: 5\naffects: [pkg/client/http.go, README.md]\n---\nKeep me\n",
"low-priority.md":  "---\npriority: 20\naffects: [pkg/client/http.go]\n---\nDefer me\n",
"independent.md":   "---\npriority: 30\naffects: [docs/guide.md]\n---\nKeep me too\n",
} {
os.WriteFile(filepath.Join(tasksDir, "backlog", name), []byte(content), 0o644)
}

removeOverlappingTasks(tasksDir)

if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "high-priority.md")); err != nil {
t.Fatalf("high priority task should stay in backlog: %v", err)
}
if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "independent.md")); err != nil {
t.Fatalf("independent task should stay in backlog: %v", err)
}
if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "low-priority.md")); !os.IsNotExist(err) {
t.Fatalf("low priority overlapping task should leave backlog, stat err = %v", err)
}
if _, err := os.Stat(filepath.Join(tasksDir, "waiting", "low-priority.md")); err != nil {
t.Fatalf("low priority overlapping task should move to waiting: %v", err)
}
}
