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
	"mato/internal/testutil"
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
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	backlogDir := filepath.Join(tasksDir, queue.DirBacklog)
	inProgressDir := filepath.Join(tasksDir, queue.DirInProgress)

	for i := 0; i < 10; i++ {
		testutil.WriteFile(t, filepath.Join(backlogDir, fmt.Sprintf("task-%02d.md", i)), fmt.Sprintf("# Task %d\nDo something.\n", i))
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
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	waitingDir := filepath.Join(tasksDir, queue.DirWaiting)
	backlogDir := filepath.Join(tasksDir, queue.DirBacklog)

	for i := 0; i < 5; i++ {
		testutil.WriteFile(t, filepath.Join(waitingDir, fmt.Sprintf("task-%02d.md", i)), fmt.Sprintf("# Task %d\nReady now.\n", i))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	promoted := make([]bool, 5)
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
			promoted[idx] = queue.ReconcileReadyQueue(tasksDir, nil)
		}(i)
	}

	close(start)
	wg.Wait()

	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}

	totalPromoted := 0
	for _, moved := range promoted {
		if moved {
			totalPromoted++
		}
	}
	if totalPromoted == 0 {
		t.Fatalf("expected at least one goroutine to report moves, got 0 (%v)", promoted)
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
	_, tasksDir := testutil.SetupRepoWithTasks(t)

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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"alpha.txt": "alpha\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"beta.txt": "beta\n"}, "beta change")

	writeTask(t, tasksDir, queue.DirReadyMerge, "alpha.md", "<!-- branch: task/alpha -->\n---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, queue.DirReadyMerge, "beta.md", "<!-- branch: task/beta -->\n---\npriority: 10\n---\n# Beta\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 2 {
		t.Fatalf("merge.ProcessQueue() = %d, want 2", got)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "alpha.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "beta.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "alpha.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "beta.md"))

	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:alpha.txt")); got != "alpha" {
		t.Fatalf("alpha.txt contents = %q, want %q", got, "alpha")
	}
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:beta.txt")); got != "beta" {
		t.Fatalf("beta.txt contents = %q, want %q", got, "beta")
	}
}

func TestMergeConflictRecoveryAndRetry(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"README.md": "alpha content\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"README.md": "beta content\n"}, "beta change")

	writeTask(t, tasksDir, queue.DirReadyMerge, "alpha.md", "<!-- branch: task/alpha -->\n---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, queue.DirReadyMerge, "beta.md", "<!-- branch: task/beta -->\n---\npriority: 10\n---\n# Beta\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("first merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "alpha.md"))
	betaBacklog := filepath.Join(tasksDir, queue.DirBacklog, "beta.md")
	mustExist(t, betaBacklog)
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "beta.md"))
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
	testutil.WriteFile(t, filepath.Join(redoClone, "README.md"), "alpha content\nbeta content\n")
	mustGitOutput(t, redoClone, "add", "README.md")
	mustGitOutput(t, redoClone, "commit", "-m", "redo beta")
	mustGitOutput(t, redoClone, "push", "--force", "origin", "task/beta")

	betaReady := filepath.Join(tasksDir, queue.DirReadyMerge, "beta.md")
	mustRename(t, betaBacklog, betaReady)
	if err := queue.WriteBranchMarker(betaReady, "task/beta"); err != nil {
		t.Fatalf("queue.WriteBranchMarker(%s): %v", betaReady, err)
	}

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("second merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "beta.md"))
	mustNotExist(t, betaReady)
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:README.md")); got != "alpha content\nbeta content" {
		t.Fatalf("README on mato = %q, want %q", got, "alpha content\nbeta content")
	}
}

func TestConflictRequeueThenRaceToClaim(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"README.md": "alpha content\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/conflict-task", map[string]string{"README.md": "conflict content\n"}, "conflict change")

	writeTask(t, tasksDir, queue.DirReadyMerge, "alpha.md", "<!-- branch: task/alpha -->\n---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, queue.DirReadyMerge, "conflict-task.md", "<!-- branch: task/conflict-task -->\n---\npriority: 10\n---\n# Conflict Task\n")

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "alpha.md"))
	conflictBacklog := filepath.Join(tasksDir, queue.DirBacklog, "conflict-task.md")
	mustExist(t, conflictBacklog)
	if contents := readFile(t, conflictBacklog); !strings.Contains(contents, "<!-- failure: merge-queue") {
		t.Fatalf("conflict task missing merge failure record: %q", contents)
	}

	conflictInProgress := filepath.Join(tasksDir, queue.DirInProgress, "conflict-task.md")
	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes atomic.Int32
	errCh := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			src := filepath.Join(tasksDir, queue.DirBacklog, "conflict-task.md")
			dst := filepath.Join(tasksDir, queue.DirInProgress, "conflict-task.md")
			if err := os.Rename(src, dst); err != nil {
				errCh <- err
				return
			}
			successes.Add(1)
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)

	errs := make([]error, 0, 2)
	for err := range errCh {
		errs = append(errs, err)
	}

	if got := successes.Load(); got != 1 {
		t.Fatalf("successful claims = %d, want 1", got)
	}
	if len(errs) != 1 {
		t.Fatalf("claim errors = %d, want 1 (%v)", len(errs), errs)
	}

	mustExist(t, conflictInProgress)
	mustNotExist(t, conflictBacklog)

	if got := markdownFileNames(t, filepath.Join(tasksDir, queue.DirInProgress)); len(got) != 1 || got[0] != "conflict-task.md" {
		t.Fatalf("in-progress tasks = %v, want [conflict-task.md]", got)
	}
	if got := markdownFileNames(t, filepath.Join(tasksDir, queue.DirBacklog)); len(got) != 0 {
		t.Fatalf("backlog tasks = %v, want none", got)
	}
}

func TestConcurrentMergeQueueTwoHosts(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	createTaskBranch(t, repoRoot, "task/alpha", map[string]string{"alpha.txt": "alpha\n"}, "alpha change")
	createTaskBranch(t, repoRoot, "task/beta", map[string]string{"beta.txt": "beta\n"}, "beta change")

	writeTask(t, tasksDir, queue.DirReadyMerge, "alpha.md", "<!-- branch: task/alpha -->\n---\npriority: 1\n---\n# Alpha\n")
	writeTask(t, tasksDir, queue.DirReadyMerge, "beta.md", "<!-- branch: task/beta -->\n---\npriority: 10\n---\n# Beta\n")

	start := make(chan struct{})
	results := make([]int, 2)
	var wg sync.WaitGroup

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			if cleanup, ok := merge.AcquireLock(tasksDir); ok {
				results[idx] = merge.ProcessQueue(repoRoot, tasksDir, "mato")
				cleanup()
			}
		}(i)
	}

	close(start)
	wg.Wait()

	totalMerged := 0
	for _, mergedCount := range results {
		totalMerged += mergedCount
	}
	if totalMerged != 2 {
		t.Fatalf("total merged across hosts = %d, want 2 (%v)", totalMerged, results)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "alpha.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "beta.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "alpha.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirReadyMerge, "beta.md"))

	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:alpha.txt")); got != "alpha" {
		t.Fatalf("alpha.txt contents = %q, want %q", got, "alpha")
	}
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:beta.txt")); got != "beta" {
		t.Fatalf("beta.txt contents = %q, want %q", got, "beta")
	}

	logSubjects := strings.Split(strings.TrimSpace(mustGitOutput(t, repoRoot, "log", "--format=%s", "mato")), "\n")
	if len(logSubjects) != 3 {
		t.Fatalf("commit subjects on mato = %v, want 3 commits", logSubjects)
	}

	alphaCommits := 0
	betaCommits := 0
	for _, subject := range logSubjects {
		switch subject {
		case "alpha change":
			alphaCommits++
		case "beta change":
			betaCommits++
		}
	}
	if alphaCommits != 1 || betaCommits != 1 {
		t.Fatalf("merge commit counts alpha=%d beta=%d, want 1 each (log=%v)", alphaCommits, betaCommits, logSubjects)
	}
}

// TestBranchNameCollisionTwoTasks verifies that tasks whose filenames sanitize
// to the same branch name receive disambiguated branches. The first task claims
// task/fix-bug; the second should receive task/fix-bug-<hash> so both can be
// pushed without collision.
func TestBranchNameCollisionTwoTasks(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Place fix-bug.md directly in in-progress with a branch comment,
	// simulating a task already claimed by agent 1.
	writeTask(t, tasksDir, queue.DirInProgress, "fix-bug.md",
		"<!-- claimed-by: agent-1  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"<!-- branch: task/fix-bug -->\n"+
			"# Fix bug\n")

	// Put fix_bug.md in backlog for agent 2 to claim.
	writeTask(t, tasksDir, queue.DirBacklog, "fix_bug.md", "# Fix bug underscore\n")

	// Agent 1 pushes to task/fix-bug.
	clone1, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone agent 1: %v", err)
	}
	defer git.RemoveClone(clone1)

	configureCloneIdentity(t, clone1)
	mustGitOutput(t, clone1, "checkout", "-b", "task/fix-bug", "mato")
	testutil.WriteFile(t, filepath.Join(clone1, "agent-one.txt"), "agent one\n")
	mustGitOutput(t, clone1, "add", "-A")
	mustGitOutput(t, clone1, "commit", "-m", "agent 1 fix")
	mustGitOutput(t, clone1, "push", "origin", "task/fix-bug")

	// Agent 2 claims fix_bug.md; SelectAndClaimTask should disambiguate the branch.
	claimed, err := queue.SelectAndClaimTask(tasksDir, "agent-2", []string{"fix_bug.md"}, 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if claimed == nil {
		t.Fatal("SelectAndClaimTask returned nil; expected fix_bug.md")
	}
	if claimed.Branch == "task/fix-bug" {
		t.Fatal("second task received un-disambiguated branch task/fix-bug; expected suffix")
	}
	if !strings.HasPrefix(claimed.Branch, "task/fix-bug-") {
		t.Fatalf("second task branch = %q, want prefix task/fix-bug-", claimed.Branch)
	}

	// Verify agent 2 can push to its disambiguated branch without collision.
	clone2, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone agent 2: %v", err)
	}
	defer git.RemoveClone(clone2)

	configureCloneIdentity(t, clone2)
	mustGitOutput(t, clone2, "checkout", "-b", claimed.Branch, "mato")
	testutil.WriteFile(t, filepath.Join(clone2, "agent-two.txt"), "agent two\n")
	mustGitOutput(t, clone2, "add", "-A")
	mustGitOutput(t, clone2, "commit", "-m", "agent 2 fix")
	mustGitOutput(t, clone2, "push", "origin", claimed.Branch)
}

func TestOrphanRecoveryDuringConcurrentWork(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	cleanup, err := queue.RegisterAgent(tasksDir, "alive-agent")
	if err != nil {
		t.Fatalf("queue.RegisterAgent(alive-agent): %v", err)
	}
	defer cleanup()

	aliveTask := writeTask(t, tasksDir, queue.DirInProgress, "alive-task.md", "<!-- claimed-by: alive-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Alive\nStill running.\n")
	deadTask := writeTask(t, tasksDir, queue.DirInProgress, "dead-task.md", "<!-- claimed-by: dead-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Dead\nNeeds recovery.\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, ".locks", "dead-agent.pid"), "2147483647")

	_ = queue.RecoverOrphanedTasks(tasksDir)

	mustExist(t, aliveTask)
	mustNotExist(t, filepath.Join(tasksDir, queue.DirBacklog, "alive-task.md"))

	deadBacklog := filepath.Join(tasksDir, queue.DirBacklog, "dead-task.md")
	mustExist(t, deadBacklog)
	mustNotExist(t, deadTask)
	if contents := readFile(t, deadBacklog); !strings.Contains(contents, "<!-- failure: mato-recovery") {
		t.Fatalf("dead task missing recovery failure record: %q", contents)
	}
}

func TestConcurrentMessageWriting(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)

	wantByID := make(map[string]messaging.Message, 20)
	for i := 0; i < 20; i++ {
		msg := messaging.Message{
			ID:     fmt.Sprintf("msg-%02d", i),
			From:   fmt.Sprintf("agent-%02d", i),
			Type:   "intent",
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

func TestReadMessagesDuringCleanup(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	base := time.Date(2024, time.May, 1, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 20; i++ {
		msg := messaging.Message{
			ID:     fmt.Sprintf("cleanup-msg-%02d", i),
			From:   "cleanup-agent",
			Type:   "intent",
			Body:   fmt.Sprintf("message body %02d", i),
			SentAt: base.Add(time.Duration(i) * time.Nanosecond),
		}
		if err := messaging.WriteMessage(tasksDir, msg); err != nil {
			t.Fatalf("messaging.WriteMessage(%s): %v", msg.ID, err)
		}
	}

	start := make(chan struct{})
	errCh := make(chan error, 10)
	var wg sync.WaitGroup
	var panics atomic.Int32

	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() {
			if recover() != nil {
				panics.Add(1)
			}
		}()

		<-start
		for i := 0; i < 10; i++ {
			if _, err := messaging.ReadMessages(tasksDir, time.Time{}); err != nil {
				errCh <- fmt.Errorf("messaging.ReadMessages iteration %d: %w", i, err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer func() {
			if recover() != nil {
				panics.Add(1)
			}
		}()

		<-start
		messaging.CleanOldMessages(tasksDir, 0)
	}()

	close(start)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Error(err)
	}
	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}
}

func TestOverlapPreventionWithConcurrentCompletion(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	highPath := writeTask(t, tasksDir, queue.DirBacklog, "task-high.md", "---\npriority: 5\naffects: [main.go]\n---\n# Task High\n")
	writeTask(t, tasksDir, queue.DirBacklog, "task-low.md", "---\npriority: 20\naffects: [main.go]\n---\n# Task Low\n")

	deferred := queue.DeferredOverlappingTasks(tasksDir, nil)

	if len(deferred) != 1 {
		t.Fatalf("len(deferred) = %d, want 1", len(deferred))
	}
	if _, ok := deferred["task-low.md"]; !ok {
		t.Fatalf("deferred set missing %q: %#v", "task-low.md", deferred)
	}
	mustExist(t, highPath)
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-low.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-low.md"))

	if err := queue.WriteQueueManifest(tasksDir, deferred, nil); err != nil {
		t.Fatalf("queue.WriteQueueManifest first pass: %v", err)
	}
	if got := readFile(t, filepath.Join(tasksDir, ".queue")); got != "task-high.md\n" {
		t.Fatalf("first queue manifest = %q, want %q", got, "task-high.md\n")
	}

	mustRename(t, highPath, filepath.Join(tasksDir, queue.DirCompleted, "task-high.md"))

	if got := queue.ReconcileReadyQueue(tasksDir, nil); got {
		t.Fatalf("queue.ReconcileReadyQueue() = %v, want false", got)
	}

	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if len(deferred) != 0 {
		t.Fatalf("len(deferred) = %d, want 0", len(deferred))
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-low.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirWaiting, "task-low.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-high.md"))

	if err := queue.WriteQueueManifest(tasksDir, deferred, nil); err != nil {
		t.Fatalf("queue.WriteQueueManifest second pass: %v", err)
	}
	if got := readFile(t, filepath.Join(tasksDir, ".queue")); got != "task-low.md\n" {
		t.Fatalf("second queue manifest = %q, want %q", got, "task-low.md\n")
	}
	if !queue.HasAvailableTasks(tasksDir, nil) {
		t.Fatal("queue.HasAvailableTasks() = false, want true")
	}
}

func TestDeferredOnlyBacklogDoesNotTriggerAgent(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	_ = repoRoot

	// Task A in in-progress with overlapping affects
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirInProgress, "active.md"),
		"---\nid: active\npriority: 1\naffects: [main.go]\n---\n# Active task\n")

	// Task B in backlog with same affects — should be deferred
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirBacklog, "blocked.md"),
		"---\nid: blocked\npriority: 10\naffects: [main.go]\n---\n# Blocked task\n")

	// Compute deferred set
	deferred := queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["blocked.md"]; !ok {
		t.Fatal("blocked.md should be in deferred set")
	}

	// Write manifest excluding deferred
	if err := queue.WriteQueueManifest(tasksDir, deferred, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}

	// .queue should be empty (only task is deferred)
	queueContent := readFile(t, filepath.Join(tasksDir, ".queue"))
	if strings.TrimSpace(queueContent) != "" {
		t.Fatalf(".queue should be empty, got %q", queueContent)
	}

	// HasAvailableTasks with deferred set should return false
	if queue.HasAvailableTasks(tasksDir, deferred) {
		t.Fatal("HasAvailableTasks should return false when only deferred tasks in backlog")
	}

	// Without deferred set, it still returns false because deferred tasks are not runnable work.
	if queue.HasAvailableTasks(tasksDir, nil) {
		t.Fatal("HasAvailableTasks(nil) should return false when only deferred tasks exist")
	}
}

func TestConcurrentSelectAndClaimTask(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	backlogDir := filepath.Join(tasksDir, queue.DirBacklog)
	inProgressDir := filepath.Join(tasksDir, queue.DirInProgress)

	const numTasks = 3
	for i := 0; i < numTasks; i++ {
		name := fmt.Sprintf("claim-task-%02d.md", i)
		testutil.WriteFile(t, filepath.Join(backlogDir, name),
			fmt.Sprintf("# Claim task %d\nDo something.\n", i))
	}

	const numGoroutines = 2
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]*queue.ClaimedTask, numGoroutines)
	errs := make([]error, numGoroutines)
	var panics atomic.Int32
	view := queue.ComputeRunnableBacklogView(tasksDir, nil)
	candidates := queue.OrderedRunnableFilenames(view, nil)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if recover() != nil {
					panics.Add(1)
				}
			}()

			<-start
			ct, err := queue.SelectAndClaimTask(tasksDir, fmt.Sprintf("agent-%d", id), candidates, 0, nil)
			results[id] = ct
			errs[id] = err
		}(g)
	}

	close(start)
	wg.Wait()

	if panics.Load() != 0 {
		t.Fatalf("expected no goroutine panics, got %d", panics.Load())
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d returned error: %v", i, err)
		}
	}

	// Both goroutines should have claimed a task (3 tasks, 2 goroutines).
	claimedNames := make(map[string]int)
	for i, ct := range results {
		if ct == nil {
			t.Fatalf("goroutine %d got nil ClaimedTask, expected a claim", i)
		}
		claimedNames[ct.Filename]++
	}

	// No task should be claimed more than once.
	for name, count := range claimedNames {
		if count != 1 {
			t.Errorf("task %s claimed by %d goroutines, want 1", name, count)
		}
	}
	if len(claimedNames) != numGoroutines {
		t.Fatalf("expected %d uniquely claimed tasks, got %d (%v)", numGoroutines, len(claimedNames), claimedNames)
	}

	// Verify claimed tasks are in in-progress/ with claimed-by headers.
	for name := range claimedNames {
		path := filepath.Join(inProgressDir, name)
		mustExist(t, path)
		contents := readFile(t, path)
		if !strings.Contains(contents, "<!-- claimed-by:") {
			t.Errorf("task %s in in-progress/ missing claimed-by header", name)
		}
		mustNotExist(t, filepath.Join(backlogDir, name))
	}

	// Exactly one task should remain in backlog.
	remaining := markdownFileNames(t, backlogDir)
	if len(remaining) != numTasks-numGoroutines {
		t.Fatalf("expected %d tasks remaining in backlog, got %d (%v)",
			numTasks-numGoroutines, len(remaining), remaining)
	}

	// In-progress should have exactly numGoroutines tasks.
	inProgress := markdownFileNames(t, inProgressDir)
	if len(inProgress) != numGoroutines {
		t.Fatalf("expected %d tasks in in-progress, got %d (%v)",
			numGoroutines, len(inProgress), inProgress)
	}
}

// TestOverlapDeferralAndFileClaims verifies that the overlap logic in
// internal/queue defers overlapping tasks from the runnable set and that
// BuildAndWriteFileClaims produces advisory claims consistent with the
// active tasks. The two mechanisms are independent: queue deferral is NOT
// driven by file-claims.json, but their outputs should be coherent.
func TestOverlapDeferralAndFileClaims(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)

	// ── Scenario 1: exact-path overlap ───────────────────────────────────
	// task-A and task-B both affect src/api.go.
	writeTask(t, tasksDir, queue.DirBacklog, "task-a.md",
		"---\npriority: 5\naffects: [src/api.go]\n---\n# Task A\n")
	writeTask(t, tasksDir, queue.DirBacklog, "task-b.md",
		"---\npriority: 20\naffects: [src/api.go]\n---\n# Task B\n")

	deferred := queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["task-b.md"]; !ok {
		t.Fatalf("task-b.md should be deferred (overlap on src/api.go): deferred=%v", deferred)
	}
	if _, ok := deferred["task-a.md"]; ok {
		t.Fatalf("task-a.md should NOT be deferred (higher priority): deferred=%v", deferred)
	}

	// Simulate task-A being claimed → move to in-progress with claimed-by header.
	mustRename(t,
		filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"),
		filepath.Join(tasksDir, queue.DirInProgress, "task-a.md"))
	os.WriteFile(
		filepath.Join(tasksDir, queue.DirInProgress, "task-a.md"),
		[]byte("<!-- claimed-by: agent-1  claimed-at: 2026-01-01T00:00:00Z -->\n---\npriority: 5\naffects: [src/api.go]\n---\n# Task A\n"),
		0o644)

	// Build file-claims.json; verify the advisory claim for src/api.go.
	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims: %v", err)
	}
	claims := readFileClaims(t, tasksDir)
	if c, ok := claims["src/api.go"]; !ok {
		t.Fatal("file-claims.json missing entry for src/api.go")
	} else if c.Task != "task-a.md" {
		t.Fatalf("src/api.go claim task = %q, want task-a.md", c.Task)
	} else if c.Status != queue.DirInProgress {
		t.Fatalf("src/api.go claim status = %q, want %q", c.Status, queue.DirInProgress)
	}

	// task-B should still be deferred (task-A active in in-progress).
	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["task-b.md"]; !ok {
		t.Fatalf("task-b.md should be deferred while task-a is in-progress: deferred=%v", deferred)
	}

	// Complete task-A → move to completed.
	mustRename(t,
		filepath.Join(tasksDir, queue.DirInProgress, "task-a.md"),
		filepath.Join(tasksDir, queue.DirCompleted, "task-a.md"))

	// Rebuild deferred set and file claims.
	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["task-b.md"]; ok {
		t.Fatalf("task-b.md should no longer be deferred after task-a completed: deferred=%v", deferred)
	}
	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims after completion: %v", err)
	}
	claims = readFileClaims(t, tasksDir)
	if _, ok := claims["src/api.go"]; ok {
		t.Fatal("src/api.go should not appear in file-claims.json after task-a completed")
	}

	// Clean up for next scenario.
	os.Remove(filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"))
	os.Remove(filepath.Join(tasksDir, queue.DirCompleted, "task-a.md"))

	// ── Scenario 2: glob pattern overlap ─────────────────────────────────
	// task-C affects internal/runner/*.go, task-D affects internal/runner/review.go.
	writeTask(t, tasksDir, queue.DirBacklog, "task-c.md",
		"---\npriority: 5\naffects: [\"internal/runner/*.go\"]\n---\n# Task C\n")
	writeTask(t, tasksDir, queue.DirBacklog, "task-d.md",
		"---\npriority: 20\naffects: [internal/runner/review.go]\n---\n# Task D\n")

	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["task-d.md"]; !ok {
		t.Fatalf("task-d.md should be deferred (glob overlap with task-c): deferred=%v", deferred)
	}
	if _, ok := deferred["task-c.md"]; ok {
		t.Fatalf("task-c.md should NOT be deferred: deferred=%v", deferred)
	}

	// Clean up for next scenario.
	os.Remove(filepath.Join(tasksDir, queue.DirBacklog, "task-c.md"))
	os.Remove(filepath.Join(tasksDir, queue.DirBacklog, "task-d.md"))

	// ── Scenario 3: directory prefix overlap ─────────────────────────────
	// task-E affects pkg/client/, task-F affects pkg/client/http.go.
	writeTask(t, tasksDir, queue.DirBacklog, "task-e.md",
		"---\npriority: 5\naffects: [\"pkg/client/\"]\n---\n# Task E\n")
	writeTask(t, tasksDir, queue.DirBacklog, "task-f.md",
		"---\npriority: 20\naffects: [pkg/client/http.go]\n---\n# Task F\n")

	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["task-f.md"]; !ok {
		t.Fatalf("task-f.md should be deferred (directory prefix overlap with task-e): deferred=%v", deferred)
	}
	if _, ok := deferred["task-e.md"]; ok {
		t.Fatalf("task-e.md should NOT be deferred: deferred=%v", deferred)
	}

	// Verify file-claims picks up the directory prefix claim when task-E is active.
	mustRename(t,
		filepath.Join(tasksDir, queue.DirBacklog, "task-e.md"),
		filepath.Join(tasksDir, queue.DirInProgress, "task-e.md"))
	os.WriteFile(
		filepath.Join(tasksDir, queue.DirInProgress, "task-e.md"),
		[]byte("<!-- claimed-by: agent-3  claimed-at: 2026-01-01T00:00:00Z -->\n---\npriority: 5\naffects: [\"pkg/client/\"]\n---\n# Task E\n"),
		0o644)

	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("BuildAndWriteFileClaims dir prefix: %v", err)
	}
	claims = readFileClaims(t, tasksDir)
	if c, ok := claims["pkg/client/"]; !ok {
		t.Fatal("file-claims.json missing directory prefix entry for pkg/client/")
	} else if c.Task != "task-e.md" {
		t.Fatalf("pkg/client/ claim task = %q, want task-e.md", c.Task)
	}

	// Clean up for next scenario.
	os.Remove(filepath.Join(tasksDir, queue.DirInProgress, "task-e.md"))
	os.Remove(filepath.Join(tasksDir, queue.DirBacklog, "task-f.md"))

	// ── Scenario 4: no overlap — both tasks remain runnable ──────────────
	writeTask(t, tasksDir, queue.DirBacklog, "task-g.md",
		"---\npriority: 5\naffects: [src/api.go]\n---\n# Task G\n")
	writeTask(t, tasksDir, queue.DirBacklog, "task-h.md",
		"---\npriority: 20\naffects: [src/db.go]\n---\n# Task H\n")

	deferred = queue.DeferredOverlappingTasks(tasksDir, nil)
	if _, ok := deferred["task-g.md"]; ok {
		t.Fatalf("task-g.md should NOT be deferred: deferred=%v", deferred)
	}
	if _, ok := deferred["task-h.md"]; ok {
		t.Fatalf("task-h.md should NOT be deferred: deferred=%v", deferred)
	}

	// Queue manifest should include both tasks.
	if err := queue.WriteQueueManifest(tasksDir, deferred, nil); err != nil {
		t.Fatalf("WriteQueueManifest: %v", err)
	}
	manifest := readFile(t, filepath.Join(tasksDir, ".queue"))
	if !strings.Contains(manifest, "task-g.md") || !strings.Contains(manifest, "task-h.md") {
		t.Fatalf("queue manifest should contain both tasks: %q", manifest)
	}
}

// readFileClaims reads and parses .mato/messages/file-claims.json.
func readFileClaims(t *testing.T, tasksDir string) map[string]messaging.FileClaim {
	t.Helper()
	path := filepath.Join(tasksDir, "messages", "file-claims.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file-claims.json: %v", err)
	}
	var claims map[string]messaging.FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("parse file-claims.json: %v", err)
	}
	return claims
}
