package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/queue"
)

func markdownFileNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func TestConcurrentTaskClaiming(t *testing.T) {
	_, tasksDir := setupTestRepo(t)
	backlogDir := filepath.Join(tasksDir, "backlog")
	inProgressDir := filepath.Join(tasksDir, "in-progress")

	for i := 0; i < 10; i++ {
		writeFile(t, filepath.Join(backlogDir, fmt.Sprintf("task-%02d.md", i)), fmt.Sprintf("# Task %d\nDo something.\n", i))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	claimed := make([]string, 20)
	for i := range claimed {
		claimed[i] = "none"
	}
	errCh := make(chan error, 20)
	var panics atomic.Int32

	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()

			<-start
			entries, err := os.ReadDir(backlogDir)
			if err != nil {
				errCh <- fmt.Errorf("goroutine %d read backlog: %w", id, err)
				return
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
					continue
				}
				src := filepath.Join(backlogDir, entry.Name())
				dst := filepath.Join(inProgressDir, entry.Name())
				if err := os.Rename(src, dst); err == nil {
					claimed[id] = entry.Name()
					return
				}
			}
		}(g)
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}

	inProgress := markdownFileNames(t, inProgressDir)
	if len(inProgress) != 10 {
		t.Fatalf("expected 10 tasks in in-progress, got %d (%v)", len(inProgress), inProgress)
	}

	backlog := markdownFileNames(t, backlogDir)
	if len(backlog) != 0 {
		t.Fatalf("expected 0 tasks in backlog, got %d (%v)", len(backlog), backlog)
	}

	claimCounts := map[string]int{}
	claimedTotal := 0
	for _, name := range claimed {
		if name == "none" {
			continue
		}
		claimedTotal++
		claimCounts[name]++
	}
	if claimedTotal != 10 {
		t.Fatalf("expected 10 successful claims, got %d (%v)", claimedTotal, claimed)
	}
	if len(claimCounts) != 10 {
		t.Fatalf("expected 10 uniquely claimed tasks, got %d (%v)", len(claimCounts), claimCounts)
	}
	for name, count := range claimCounts {
		if count != 1 {
			t.Errorf("task %s claimed by %d goroutines", name, count)
		}
	}
}

func TestConcurrentReconcileReadyQueue(t *testing.T) {
	_, tasksDir := setupTestRepo(t)
	waitingDir := filepath.Join(tasksDir, "waiting")
	backlogDir := filepath.Join(tasksDir, "backlog")

	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(waitingDir, fmt.Sprintf("task-%02d.md", i)), fmt.Sprintf("# Task %d\nReady now.\n", i))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	promoted := make([]int, 5)
	var panics atomic.Int32

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()

			<-start
			promoted[idx] = queue.ReconcileReadyQueue(tasksDir)
		}(i)
	}

	close(start)
	wg.Wait()

	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}

	totalPromoted := 0
	for _, count := range promoted {
		totalPromoted += count
	}
	if totalPromoted != 5 {
		t.Fatalf("expected 5 total promotions, got %d (%v)", totalPromoted, promoted)
	}

	backlog := markdownFileNames(t, backlogDir)
	if len(backlog) != 5 {
		t.Fatalf("expected 5 tasks in backlog, got %d (%v)", len(backlog), backlog)
	}

	waiting := markdownFileNames(t, waitingDir)
	if len(waiting) != 0 {
		t.Fatalf("expected 0 tasks in waiting, got %d (%v)", len(waiting), waiting)
	}
}

func TestConcurrentMergeLock(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	// Acquire the lock — should succeed.
	cleanup1, ok1 := merge.AcquireLock(tasksDir)
	if !ok1 || cleanup1 == nil {
		t.Fatal("first lock acquisition should succeed")
	}

	// Second attempt while held by same process — should fail because
	// the lock file exists and the holder PID (ours) is active.
	_, ok2 := merge.AcquireLock(tasksDir)
	if ok2 {
		t.Fatal("second lock acquisition should fail while first is held")
	}

	// Release and re-acquire — should succeed.
	cleanup1()

	cleanup3, ok3 := merge.AcquireLock(tasksDir)
	if !ok3 || cleanup3 == nil {
		t.Fatal("lock acquisition after release should succeed")
	}
	cleanup3()

	// Simulate a stale lock from a dead process.
	locksDir := filepath.Join(tasksDir, ".locks")
	os.WriteFile(filepath.Join(locksDir, "merge.lock"), []byte("2147483647"), 0o644)

	cleanup4, ok4 := merge.AcquireLock(tasksDir)
	if !ok4 || cleanup4 == nil {
		t.Fatal("lock acquisition should succeed when previous holder is dead")
	}
	cleanup4()
}

func TestConcurrentMergeQueueProcessing(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"alpha.txt": "alpha\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"beta.txt": "beta\n"}, "beta change")

	writeTask(t, tasksDir, "ready-to-merge", "alpha.md", "---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, "ready-to-merge", "beta.md", "---\npriority: 10\n---\n# Beta\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 2 {
		t.Fatalf("merge.ProcessQueue() = %d, want 2", got)
	}

	mustExist(t, filepath.Join(tasksDir, "completed", "alpha.md"))
	mustExist(t, filepath.Join(tasksDir, "completed", "beta.md"))
	mustNotExist(t, filepath.Join(tasksDir, "ready-to-merge", "alpha.md"))
	mustNotExist(t, filepath.Join(tasksDir, "ready-to-merge", "beta.md"))

	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:alpha.txt")); got != "alpha" {
		t.Fatalf("alpha.txt contents = %q, want %q", got, "alpha")
	}
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:beta.txt")); got != "beta" {
		t.Fatalf("beta.txt contents = %q, want %q", got, "beta")
	}
}

func TestMergeConflictRecoveryAndRetry(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"README.md": "alpha content\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"README.md": "beta content\n"}, "beta change")

	writeTask(t, tasksDir, "ready-to-merge", "alpha.md", "---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, "ready-to-merge", "beta.md", "---\npriority: 10\n---\n# Beta\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("first merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, "completed", "alpha.md"))
	betaBacklog := filepath.Join(tasksDir, "backlog", "beta.md")
	mustExist(t, betaBacklog)
	mustNotExist(t, filepath.Join(tasksDir, "ready-to-merge", "beta.md"))
	if contents := readFile(t, betaBacklog); !strings.Contains(contents, "<!-- failure: merge-queue") {
		t.Fatalf("beta task missing merge failure record: %q", contents)
	}

	redoClone, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	defer git.RemoveClone(redoClone)

	configureCloneIdentity(t, redoClone)
	mustGitOutput(t, redoClone, "fetch", "origin")
	mustGitOutput(t, redoClone, "checkout", "-B", "task/beta", "origin/mato")
	writeFile(t, filepath.Join(redoClone, "README.md"), "alpha content\nbeta content\n")
	mustGitOutput(t, redoClone, "add", "README.md")
	mustGitOutput(t, redoClone, "commit", "-m", "redo beta")
	mustGitOutput(t, redoClone, "push", "--force", "origin", "task/beta")

	betaReady := filepath.Join(tasksDir, "ready-to-merge", "beta.md")
	mustRename(t, betaBacklog, betaReady)

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("second merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, "completed", "beta.md"))
	mustNotExist(t, betaReady)
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:README.md")); got != "alpha content\nbeta content" {
		t.Fatalf("README on mato = %q, want %q", got, "alpha content\nbeta content")
	}
}

func TestOrphanRecoveryDuringConcurrentWork(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	cleanup, err := queue.RegisterAgent(tasksDir, "alive-agent")
	if err != nil {
		t.Fatalf("queue.RegisterAgent(alive-agent): %v", err)
	}
	defer cleanup()

	aliveTask := writeTask(t, tasksDir, "in-progress", "alive-task.md", "<!-- claimed-by: alive-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Alive\nStill running.\n")
	deadTask := writeTask(t, tasksDir, "in-progress", "dead-task.md", "<!-- claimed-by: dead-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Dead\nNeeds recovery.\n")
	writeFile(t, filepath.Join(tasksDir, ".locks", "dead-agent.pid"), "2147483647")

	queue.RecoverOrphanedTasks(tasksDir)

	mustExist(t, aliveTask)
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "alive-task.md"))

	deadBacklog := filepath.Join(tasksDir, "backlog", "dead-task.md")
	mustExist(t, deadBacklog)
	mustNotExist(t, deadTask)
	if contents := readFile(t, deadBacklog); !strings.Contains(contents, "<!-- failure: mato-recovery") {
		t.Fatalf("dead task missing recovery failure record: %q", contents)
	}
}

func TestConcurrentMessageWriting(t *testing.T) {
	_, tasksDir := setupTestRepo(t)
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)

	wantByID := make(map[string]messaging.Message, 20)
	for i := 0; i < 20; i++ {
		msg := messaging.Message{
			ID:     fmt.Sprintf("msg-%02d", i),
			From:   fmt.Sprintf("agent-%02d", i),
			Type:   "status",
			Task:   fmt.Sprintf("task-%02d.md", i),
			Branch: fmt.Sprintf("task/branch-%02d", i),
			Body:   fmt.Sprintf("message body %02d", i),
			SentAt: base.Add(time.Duration(i) * time.Nanosecond),
		}
		wantByID[msg.ID] = msg
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errCh := make(chan error, 20)
	var panics atomic.Int32

	for i := 0; i < 20; i++ {
		msg := wantByID[fmt.Sprintf("msg-%02d", i)]
		wg.Add(1)
		go func(msg messaging.Message) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()

			<-start
			if err := messaging.WriteMessage(tasksDir, msg); err != nil {
				errCh <- fmt.Errorf("messaging.WriteMessage(%s): %w", msg.ID, err)
			}
		}(msg)
	}

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}

	messages, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("messaging.ReadMessages: %v", err)
	}
	if len(messages) != 20 {
		t.Fatalf("expected 20 messages, got %d", len(messages))
	}

	gotByID := make(map[string]messaging.Message, len(messages))
	for _, msg := range messages {
		gotByID[msg.ID] = msg
	}
	if len(gotByID) != 20 {
		t.Fatalf("expected 20 unique message IDs, got %d", len(gotByID))
	}
	for id, want := range wantByID {
		got, ok := gotByID[id]
		if !ok {
			t.Fatalf("missing message %s", id)
		}
		if got.From != want.From || got.Type != want.Type || got.Task != want.Task || got.Branch != want.Branch || got.Body != want.Body || !got.SentAt.Equal(want.SentAt) {
			t.Fatalf("message %s = %#v, want %#v", id, got, want)
		}
	}

	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", eventsDir, err)
	}
	if len(entries) != 20 {
		t.Fatalf("expected 20 message files, got %d", len(entries))
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(eventsDir, entry.Name()))
		if err != nil {
			t.Fatalf("os.ReadFile(%s): %v", entry.Name(), err)
		}
		var msg messaging.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("message file %s is not valid JSON: %v", entry.Name(), err)
		}
		want, ok := wantByID[msg.ID]
		if !ok {
			t.Fatalf("unexpected message ID %q in %s", msg.ID, entry.Name())
		}
		if msg.Body != want.Body || msg.From != want.From || msg.Task != want.Task || msg.Branch != want.Branch {
			t.Fatalf("message file %s = %#v, want %#v", entry.Name(), msg, want)
		}
	}
}

func TestOverlapPreventionWithConcurrentCompletion(t *testing.T) {
	_, tasksDir := setupTestRepo(t)

	highPath := writeTask(t, tasksDir, "backlog", "task-high.md", "---\npriority: 5\naffects: [main.go]\n---\n# Task High\n")
	writeTask(t, tasksDir, "backlog", "task-low.md", "---\npriority: 20\naffects: [main.go]\n---\n# Task Low\n")

	queue.RemoveOverlappingTasks(tasksDir)

	mustExist(t, highPath)
	mustExist(t, filepath.Join(tasksDir, "waiting", "task-low.md"))
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "task-low.md"))

	if err := queue.WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("queue.WriteQueueManifest first pass: %v", err)
	}
	if got := readFile(t, filepath.Join(tasksDir, ".queue")); got != "task-high.md\n" {
		t.Fatalf("first queue manifest = %q, want %q", got, "task-high.md\n")
	}

	mustRename(t, highPath, filepath.Join(tasksDir, "completed", "task-high.md"))

	if got := queue.ReconcileReadyQueue(tasksDir); got != 1 {
		t.Fatalf("queue.ReconcileReadyQueue() = %d, want 1", got)
	}

	queue.RemoveOverlappingTasks(tasksDir)

	mustExist(t, filepath.Join(tasksDir, "backlog", "task-low.md"))
	mustNotExist(t, filepath.Join(tasksDir, "waiting", "task-low.md"))
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "task-high.md"))

	if err := queue.WriteQueueManifest(tasksDir); err != nil {
		t.Fatalf("queue.WriteQueueManifest second pass: %v", err)
	}
	if got := readFile(t, filepath.Join(tasksDir, ".queue")); got != "task-low.md\n" {
		t.Fatalf("second queue manifest = %q, want %q", got, "task-low.md\n")
	}
	if !queue.HasAvailableTasks(tasksDir) {
		t.Fatal("queue.HasAvailableTasks() = false, want true")
	}
}
