package queue

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read pipe: %v", err)
	}
	return string(data)
}

func TestSafeRename_MissingSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "missing.md")
	dst := filepath.Join(dir, "moved.md")

	if err := safeRename(src, dst); err == nil {
		t.Fatal("safeRename should return an error for a missing source")
	}
}

func TestSafeRename_DestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "dst.md")

	if err := os.WriteFile(src, []byte("source\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if err := os.WriteFile(dst, []byte("destination\n"), 0o644); err != nil {
		t.Fatalf("write destination: %v", err)
	}

	err := safeRename(src, dst)
	if err == nil {
		t.Fatal("safeRename should fail when destination exists")
	}
	if !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("safeRename error = %q, want destination already exists", err)
	}

	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(data) != "destination\n" {
		t.Fatalf("destination contents changed: got %q", string(data))
	}
}

func TestRecoverOrphanedTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", "completed", "failed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	orphan := filepath.Join(tasksDir, "in-progress", "fix-bug.md")
	os.WriteFile(orphan, []byte("# Fix bug\nDo the thing.\n"), 0o644)

	RecoverOrphanedTasks(tasksDir)

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

	RecoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.md file should not be moved: %v", err)
	}
}

func TestRecoverOrphanedTasks_DoesNotOverwriteExistingBacklogTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	backlogPath := filepath.Join(tasksDir, "backlog", "fix-bug.md")
	orphanPath := filepath.Join(tasksDir, "in-progress", "fix-bug.md")
	os.WriteFile(backlogPath, []byte("# Existing task\n"), 0o644)
	os.WriteFile(orphanPath, []byte("# Recovered task\n"), 0o644)

	stderr := captureStderr(t, func() {
		RecoverOrphanedTasks(tasksDir)
	})

	if !strings.Contains(stderr, "destination already exists") {
		t.Fatalf("expected overwrite warning, got %q", stderr)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("orphan should stay in in-progress after failed recovery: %v", err)
	}
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog task: %v", err)
	}
	if string(data) != "# Existing task\n" {
		t.Fatalf("existing backlog task should be unchanged, got %q", string(data))
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

	RecoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(task); err != nil {
		t.Fatal("task claimed by active agent should NOT be recovered")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "active-task.md")); err == nil {
		t.Fatal("task claimed by active agent should NOT appear in backlog")
	}
}

func TestRecoverOrphanedTasks_RemovesStaleInProgressCopyWhenTaskAlreadyAdvanced(t *testing.T) {
	for _, laterDir := range []string{"ready-to-merge", "completed", "failed"} {
		t.Run(laterDir, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, sub := range []string{"backlog", "in-progress", "ready-to-merge", "completed", "failed"} {
				if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
					t.Fatalf("MkdirAll(%s): %v", sub, err)
				}
			}

			stalePath := filepath.Join(tasksDir, "in-progress", "fix-bug.md")
			authoritativePath := filepath.Join(tasksDir, laterDir, "fix-bug.md")
			if err := os.WriteFile(stalePath, []byte("# Stale task\n"), 0o644); err != nil {
				t.Fatalf("write stale task: %v", err)
			}
			if err := os.WriteFile(authoritativePath, []byte("# Authoritative task\n"), 0o644); err != nil {
				t.Fatalf("write authoritative task: %v", err)
			}

			RecoverOrphanedTasks(tasksDir)

			if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
				t.Fatalf("stale in-progress copy should be removed, stat err = %v", err)
			}
			if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "fix-bug.md")); !os.IsNotExist(err) {
				t.Fatalf("task should not be recovered to backlog when %s copy exists, stat err = %v", laterDir, err)
			}
			data, err := os.ReadFile(authoritativePath)
			if err != nil {
				t.Fatalf("read authoritative task: %v", err)
			}
			if string(data) != "# Authoritative task\n" {
				t.Fatalf("authoritative task should be unchanged, got %q", string(data))
			}
		})
	}
}

func TestRecoverOrphanedTasks_ConcurrentCalls(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("task-%d.md", i)
		path := filepath.Join(tasksDir, "in-progress", name)
		if err := os.WriteFile(path, []byte(fmt.Sprintf("# Task %d\n", i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	start := make(chan struct{})
	panicCh := make(chan any, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicCh <- r
				}
			}()
			<-start
			RecoverOrphanedTasks(tasksDir)
		}()
	}

	close(start)
	wg.Wait()
	close(panicCh)
	for p := range panicCh {
		t.Fatalf("RecoverOrphanedTasks panicked: %v", p)
	}

	backlogEntries, err := os.ReadDir(filepath.Join(tasksDir, "backlog"))
	if err != nil {
		t.Fatalf("ReadDir(backlog): %v", err)
	}
	if len(backlogEntries) != 5 {
		t.Fatalf("backlog entries = %d, want 5", len(backlogEntries))
	}

	inProgressEntries, err := os.ReadDir(filepath.Join(tasksDir, "in-progress"))
	if err != nil {
		t.Fatalf("ReadDir(in-progress): %v", err)
	}
	if len(inProgressEntries) != 0 {
		t.Fatalf("in-progress entries = %d, want 0", len(inProgressEntries))
	}

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("task-%d.md", i)
		data, err := os.ReadFile(filepath.Join(tasksDir, "backlog", name))
		if err != nil {
			t.Fatalf("read %s from backlog: %v", name, err)
		}
		if count := strings.Count(string(data), "<!-- failure: mato-recovery"); count != 1 {
			t.Fatalf("%s failure record count = %d, want 1", name, count)
		}
	}
}

func TestParseClaimedBy(t *testing.T) {
	dir := t.TempDir()

	withClaim := filepath.Join(dir, "task.md")
	os.WriteFile(withClaim, []byte("<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n# Do stuff\n"), 0o644)
	if got := ParseClaimedBy(withClaim); got != "abc123" {
		t.Errorf("ParseClaimedBy = %q, want %q", got, "abc123")
	}

	noClaim := filepath.Join(dir, "plain.md")
	os.WriteFile(noClaim, []byte("# Just a task\n"), 0o644)
	if got := ParseClaimedBy(noClaim); got != "" {
		t.Errorf("ParseClaimedBy = %q, want empty", got)
	}

	if got := ParseClaimedBy(filepath.Join(dir, "missing.md")); got != "" {
		t.Errorf("ParseClaimedBy = %q, want empty for missing file", got)
	}
}

func TestIsAgentActive(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	if IsAgentActive(tasksDir, "") {
		t.Error("empty agent ID should not be active")
	}
	if IsAgentActive(tasksDir, "no-such-agent") {
		t.Error("agent without lock file should not be active")
	}

	os.WriteFile(filepath.Join(locksDir, "live.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if !IsAgentActive(tasksDir, "live") {
		t.Error("agent with current PID should be active")
	}

	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647"), 0o644)
	if IsAgentActive(tasksDir, "dead") {
		t.Error("agent with dead PID should not be active")
	}
}

func TestIsAgentActive_CorruptedPIDFile(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "corrupted.pid"), []byte("not-a-number"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if IsAgentActive(tasksDir, "corrupted") {
		t.Fatal("corrupted pid file should not be considered active")
	}
}

func TestIsAgentActive_NegativePID(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "negative.pid"), []byte("-1"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if IsAgentActive(tasksDir, "negative") {
		t.Fatal("negative pid should not be considered active")
	}
}

func TestHasAvailableTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", "completed", "failed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	if HasAvailableTasks(tasksDir) {
		t.Fatal("expected no available tasks in empty dirs")
	}

	os.WriteFile(filepath.Join(tasksDir, "backlog", "notes.txt"), []byte("hi"), 0o644)
	if HasAvailableTasks(tasksDir) {
		t.Fatal("non-.md file should not count as an available task")
	}

	os.WriteFile(filepath.Join(tasksDir, "backlog", "task1.md"), []byte("# Task 1\n"), 0o644)
	if !HasAvailableTasks(tasksDir) {
		t.Fatal("expected available task in backlog")
	}

	os.Remove(filepath.Join(tasksDir, "backlog", "task1.md"))
	os.WriteFile(filepath.Join(tasksDir, "in-progress", "task2.md"), []byte("# Task 2\n"), 0o644)
	if HasAvailableTasks(tasksDir) {
		t.Fatal("in-progress tasks should not count as available")
	}

	os.WriteFile(filepath.Join(tasksDir, "backlog", "task3.md"), []byte("# Task 3\n"), 0o644)
	if !HasAvailableTasks(tasksDir) {
		t.Fatal("expected available task in backlog")
	}
}

func TestRegisterAgent(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, err := RegisterAgent(tasksDir, "test-agent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
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

func TestRegisterAgent_RacesCleanStaleLocks(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, err := RegisterAgent(tasksDir, "race-agent")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	defer cleanup()

	lockFile := filepath.Join(tasksDir, ".locks", "race-agent.pid")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		CleanStaleLocks(tasksDir)
	}()
	wg.Wait()

	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("active agent lock should survive cleanup: %v", err)
	}
}

func TestCleanStaleLocks(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	os.WriteFile(filepath.Join(locksDir, "alive.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647"), 0o644)

	CleanStaleLocks(tasksDir)

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

	if got := ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("ReconcileReadyQueue() = %d, want 1", got)
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

	if got := ReconcileReadyQueue(tasksDir); got != 0 {
		t.Fatalf("ReconcileReadyQueue() = %d, want 0", got)
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

	if got := ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("ReconcileReadyQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "solo-task.md")); err != nil {
		t.Fatalf("promoted task missing from backlog: %v", err)
	}
}

func TestReconcileReadyQueue_SkipsOverlappingWithActive(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed", "in-progress"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, "in-progress", "task-a.md"), []byte("---\naffects: [main.go]\n---\nActive\n"), 0o644); err != nil {
		t.Fatalf("write active task: %v", err)
	}
	waitingPath := filepath.Join(tasksDir, "waiting", "task-b.md")
	if err := os.WriteFile(waitingPath, []byte("---\naffects: [main.go]\n---\nBlocked by active overlap\n"), 0o644); err != nil {
		t.Fatalf("write waiting task: %v", err)
	}

	if got := ReconcileReadyQueue(tasksDir); got != 0 {
		t.Fatalf("ReconcileReadyQueue() = %d, want 0", got)
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("overlapping waiting task should stay in waiting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "task-b.md")); !os.IsNotExist(err) {
		t.Fatalf("overlapping waiting task should not be promoted, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_PromotesAfterActiveCompletes(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, "completed", "task-a.md"), []byte("---\nid: task-a\naffects: [main.go]\n---\nDone\n"), 0o644); err != nil {
		t.Fatalf("write completed task: %v", err)
	}
	waitingPath := filepath.Join(tasksDir, "waiting", "task-b.md")
	if err := os.WriteFile(waitingPath, []byte("---\ndepends_on: [task-a]\naffects: [main.go]\n---\nReady now\n"), 0o644); err != nil {
		t.Fatalf("write waiting task: %v", err)
	}

	if got := ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("ReconcileReadyQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "task-b.md")); err != nil {
		t.Fatalf("task should be promoted after active completion: %v", err)
	}
	if _, err := os.Stat(waitingPath); !os.IsNotExist(err) {
		t.Fatalf("promoted task should leave waiting, stat err = %v", err)
	}
}

func TestReconcileReadyQueue_DoesNotOverwriteExistingBacklogTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	waitingPath := filepath.Join(tasksDir, "waiting", "task.md")
	backlogPath := filepath.Join(tasksDir, "backlog", "task.md")
	os.WriteFile(waitingPath, []byte("# Ready\n"), 0o644)
	os.WriteFile(backlogPath, []byte("# Existing backlog\n"), 0o644)

	stderr := captureStderr(t, func() {
		if got := ReconcileReadyQueue(tasksDir); got != 0 {
			t.Fatalf("ReconcileReadyQueue() = %d, want 0", got)
		}
	})

	if !strings.Contains(stderr, "destination already exists") {
		t.Fatalf("expected overwrite warning, got %q", stderr)
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("waiting task should remain after failed promotion: %v", err)
	}
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog task: %v", err)
	}
	if string(data) != "# Existing backlog\n" {
		t.Fatalf("existing backlog task should be unchanged, got %q", string(data))
	}
}

func TestReconcileReadyQueue_DetectsSelfDependency(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	waitingPath := filepath.Join(tasksDir, "waiting", "self-task.md")
	os.WriteFile(waitingPath, []byte("---\nid: self-task\ndepends_on: [self-task]\n---\nBlocked\n"), 0o644)

	stderr := captureStderr(t, func() {
		if got := ReconcileReadyQueue(tasksDir); got != 0 {
			t.Fatalf("ReconcileReadyQueue() = %d, want 0", got)
		}
	})

	if !strings.Contains(stderr, "task self-task depends on itself") {
		t.Fatalf("expected self-dependency warning, got %q", stderr)
	}
	if _, err := os.Stat(waitingPath); err != nil {
		t.Fatalf("self-dependent task should remain in waiting: %v", err)
	}
}

func TestReconcileReadyQueue_DetectsCircularDependency(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, "waiting", "task-a.md"), []byte("---\nid: task-a\ndepends_on: [task-b]\n---\nA\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "waiting", "task-b.md"), []byte("---\nid: task-b\ndepends_on: [task-a]\n---\nB\n"), 0o644)

	stderr := captureStderr(t, func() {
		if got := ReconcileReadyQueue(tasksDir); got != 0 {
			t.Fatalf("ReconcileReadyQueue() = %d, want 0", got)
		}
	})

	if !strings.Contains(stderr, "circular dependency detected between task-a and task-b") {
		t.Fatalf("expected circular dependency warning, got %q", stderr)
	}
	if strings.Count(stderr, "circular dependency detected between task-a and task-b") != 1 {
		t.Fatalf("expected one circular dependency warning, got %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "waiting", "task-a.md")); err != nil {
		t.Fatalf("task-a should remain in waiting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "waiting", "task-b.md")); err != nil {
		t.Fatalf("task-b should remain in waiting: %v", err)
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

	if err := WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	data, _ := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	want := "a-high.md\nb-high.md\nz-low.md\nc-default.md\n"
	if string(data) != want {
		t.Fatalf("manifest = %q, want %q", string(data), want)
	}
}

func TestWriteQueueManifest_EmptyBacklog(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tasksDir, "backlog"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	if err := WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(strings.Fields(string(data))) != 0 {
		t.Fatalf("expected empty manifest, got %q", string(data))
	}
}

func TestWriteQueueManifest_SkipsMalformedFiles(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "backlog"), 0o755)

	os.WriteFile(filepath.Join(tasksDir, ".queue"), []byte("stale\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "backlog", "good.md"), []byte("---\npriority: 10\n---\nGood\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "backlog", "bad.md"), []byte("---\npriority: nope\n---\nBad\n"), 0o644)

	stderr := captureStderr(t, func() {
		if err := WriteQueueManifest(tasksDir); err != nil {
			t.Fatalf("WriteQueueManifest: %v", err)
		}
	})

	if !strings.Contains(stderr, "could not parse backlog task bad.md for queue manifest") {
		t.Fatalf("expected malformed file warning, got %q", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(data) != "good.md\n" {
		t.Fatalf("manifest = %q, want %q", string(data), "good.md\n")
	}
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".queue.tmp-") {
			t.Fatalf("temporary manifest file should be cleaned up, found %s", entry.Name())
		}
	}
}

func TestCompletedTaskIDs_UsesFilenameStemWhenParseFails(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "completed"), 0o755)

	os.WriteFile(filepath.Join(tasksDir, "completed", "broken-task.md"), []byte("---\npriority: nope\n---\nDone\n"), 0o644)

	ids := completedTaskIDs(tasksDir)
	if _, ok := ids["broken-task"]; !ok {
		t.Fatal("filename stem should be treated as completed when frontmatter is malformed")
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

	RemoveOverlappingTasks(tasksDir)

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

func TestRemoveOverlappingTasks_ChecksInProgress(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "in-progress"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, "in-progress", "task-a.md"), []byte("---\naffects: [main.go]\n---\nActive\n"), 0o644); err != nil {
		t.Fatalf("write active task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "backlog", "task-b.md"), []byte("---\naffects: [main.go]\n---\nConflicting\n"), 0o644); err != nil {
		t.Fatalf("write backlog task: %v", err)
	}

	RemoveOverlappingTasks(tasksDir)

	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "task-b.md")); !os.IsNotExist(err) {
		t.Fatalf("conflicting backlog task should be deferred, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "waiting", "task-b.md")); err != nil {
		t.Fatalf("conflicting backlog task should move to waiting: %v", err)
	}
}

func TestRemoveOverlappingTasks_ChecksReadyToMerge(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "ready-to-merge"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	if err := os.WriteFile(filepath.Join(tasksDir, "ready-to-merge", "task-a.md"), []byte("---\naffects: [main.go]\n---\nActive\n"), 0o644); err != nil {
		t.Fatalf("write ready-to-merge task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "backlog", "task-b.md"), []byte("---\naffects: [main.go]\n---\nConflicting\n"), 0o644); err != nil {
		t.Fatalf("write backlog task: %v", err)
	}

	RemoveOverlappingTasks(tasksDir)

	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "task-b.md")); !os.IsNotExist(err) {
		t.Fatalf("conflicting backlog task should be deferred, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "waiting", "task-b.md")); err != nil {
		t.Fatalf("conflicting backlog task should move to waiting: %v", err)
	}
}

func TestRemoveOverlappingTasks_DoesNotOverwriteExistingWaitingTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, "waiting", "low-priority.md"), []byte("# Existing waiting\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "backlog", "high-priority.md"), []byte("---\npriority: 5\naffects: [pkg/client/http.go]\n---\nKeep me\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "backlog", "low-priority.md"), []byte("---\npriority: 20\naffects: [pkg/client/http.go]\n---\nDefer me\n"), 0o644)

	stderr := captureStderr(t, func() {
		RemoveOverlappingTasks(tasksDir)
	})

	if !strings.Contains(stderr, "destination already exists") {
		t.Fatalf("expected overwrite warning, got %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "low-priority.md")); err != nil {
		t.Fatalf("task should remain in backlog after failed defer: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, "waiting", "low-priority.md"))
	if err != nil {
		t.Fatalf("read waiting task: %v", err)
	}
	if string(data) != "# Existing waiting\n" {
		t.Fatalf("existing waiting task should be unchanged, got %q", string(data))
	}
}

func TestRemoveOverlappingTasks_AllIdenticalAffects(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	for name, content := range map[string]string{
		"priority-5.md":  "---\npriority: 5\naffects: [main.go]\n---\nKeep me\n",
		"priority-10.md": "---\npriority: 10\naffects: [main.go]\n---\nWait\n",
		"priority-20.md": "---\npriority: 20\naffects: [main.go]\n---\nWait\n",
	} {
		if err := os.WriteFile(filepath.Join(tasksDir, "backlog", name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	RemoveOverlappingTasks(tasksDir)

	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "priority-5.md")); err != nil {
		t.Fatalf("highest-priority task should remain in backlog: %v", err)
	}
	for _, name := range []string{"priority-10.md", "priority-20.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, "backlog", name)); !os.IsNotExist(err) {
			t.Fatalf("%s should leave backlog, stat err = %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(tasksDir, "waiting", name)); err != nil {
			t.Fatalf("%s should move to waiting: %v", name, err)
		}
	}
}

func TestRemoveOverlappingTasks_NoAffects(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	for _, name := range []string{"task-a.md", "task-b.md", "task-c.md"} {
		if err := os.WriteFile(filepath.Join(tasksDir, "backlog", name), []byte("# Task\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	RemoveOverlappingTasks(tasksDir)

	for _, name := range []string{"task-a.md", "task-b.md", "task-c.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, "backlog", name)); err != nil {
			t.Fatalf("%s should remain in backlog: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(tasksDir, "waiting", name)); !os.IsNotExist(err) {
			t.Fatalf("%s should not move to waiting, stat err = %v", name, err)
		}
	}
}

func TestQueueOps_SpecialCharacterFilenames(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "completed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	name := "my task (v2).md"
	waitingPath := filepath.Join(tasksDir, "waiting", name)
	if err := os.WriteFile(waitingPath, []byte("# Special task\n"), 0o644); err != nil {
		t.Fatalf("write waiting task: %v", err)
	}

	if got := ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("ReconcileReadyQueue() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", name)); err != nil {
		t.Fatalf("special-character task missing from backlog: %v", err)
	}
	if _, err := os.Stat(waitingPath); !os.IsNotExist(err) {
		t.Fatalf("special-character task should leave waiting, stat err = %v", err)
	}

	if err := WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, ".queue"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(data), name) {
		t.Fatalf("manifest %q does not include %q", string(data), name)
	}
}

func TestGenerateAgentID(t *testing.T) {
	id, err := GenerateAgentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("expected 8 hex chars, got %q (len %d)", id, len(id))
	}
	id2, err := GenerateAgentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == id2 {
		t.Errorf("two consecutive IDs should differ: %q == %q", id, id2)
	}
}

func TestReconcileReadyQueue_HighPriorityNotBlockedByLowPriorityBacklog(t *testing.T) {
tasksDir := t.TempDir()
for _, sub := range []string{"waiting", "backlog", "completed", "in-progress", "ready-to-merge"} {
os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
}

// Low-priority task already in backlog with overlapping affects
os.WriteFile(filepath.Join(tasksDir, "backlog", "low-priority.md"),
[]byte("---\npriority: 20\naffects: [main.go]\n---\n# Low\n"), 0o644)

// High-priority task in waiting with same affects, no deps
os.WriteFile(filepath.Join(tasksDir, "waiting", "high-priority.md"),
[]byte("---\npriority: 5\naffects: [main.go]\n---\n# High\n"), 0o644)

got := ReconcileReadyQueue(tasksDir)
if got != 1 {
t.Fatalf("ReconcileReadyQueue() = %d, want 1 (high-priority should promote)", got)
}
if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "high-priority.md")); err != nil {
t.Fatal("high-priority task should be promoted to backlog")
}
// Both are now in backlog — RemoveOverlappingTasks will handle the conflict
// by deferring the lower-priority one.
}
