package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/taskfile"
	"mato/internal/testutil"
	"mato/internal/ui"
)

func setupClaimTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range append(AllDirs, DirLocks) {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func candidates(names ...string) []string {
	return append([]string(nil), names...)
}

func runnableCandidates(t *testing.T, dir string, exclude map[string]struct{}, idx *PollIndex) []string {
	t.Helper()
	idx = ensureIndex(dir, idx)
	view := ComputeRunnableBacklogView(dir, idx)
	return OrderedRunnableFilenames(view, exclude)
}

func TestSelectAndClaimTask_Normal(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "alpha.md"), "# Alpha\nDo alpha.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "beta.md"), "# Beta\nDo beta.\n")

	task, err := SelectAndClaimTask(dir, "agent-1", candidates("alpha.md", "beta.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task, got nil")
	}
	if task.Filename != "alpha.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "alpha.md")
	}
	if task.Branch != "task/alpha" {
		t.Fatalf("Branch = %q, want %q", task.Branch, "task/alpha")
	}
	if task.Title != "Alpha" {
		t.Fatalf("Title = %q, want %q", task.Title, "Alpha")
	}

	// File should be in in-progress, not backlog
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "alpha.md")); err != nil {
		t.Fatalf("task not in in-progress: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "alpha.md")); !os.IsNotExist(err) {
		t.Fatal("task still in backlog after claim")
	}

	// Check claimed-by header
	data, _ := os.ReadFile(filepath.Join(dir, DirInProgress, "alpha.md"))
	if !strings.HasPrefix(string(data), "<!-- claimed-by: agent-1  claimed-at: ") {
		t.Fatalf("missing claimed-by header: %q", string(data))
	}

	// Beta should still be in backlog
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "beta.md")); err != nil {
		t.Fatalf("beta should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "retry.md"), strings.Join([]string{
		"# Retry task",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-2", candidates("retry.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (retry exhausted), got %+v", task)
	}

	// Task should be in failed/
	if _, err := os.Stat(filepath.Join(dir, DirFailed, "retry.md")); err != nil {
		t.Fatalf("task not in failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "retry.md")); !os.IsNotExist(err) {
		t.Fatal("task still in backlog after retry exhaustion")
	}
}

func TestSelectAndClaimTask_RetryExhausted_PreservesVerdictFallback(t *testing.T) {
	dir := setupClaimTestDir(t)
	filename := "verdict-retry.md"
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, filename), strings.Join([]string{
		"# Verdict retry task",
		"<!-- failure: a1 at 2026-01-01T00:00:00Z step=WORK error=oops -->",
		"<!-- failure: a2 at 2026-01-02T00:00:00Z step=WORK error=oops -->",
		"<!-- failure: a3 at 2026-01-03T00:00:00Z step=WORK error=oops -->",
		"",
	}, "\n"))

	// Seed a preserved verdict file with a reject reason — this is the only
	// review context for this task.
	msgDir := filepath.Join(dir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := map[string]string{"verdict": "reject", "reason": "needs tests"}
	vdata, _ := json.Marshal(verdict)
	verdictPath := taskfile.VerdictPath(dir, filename)
	if err := os.WriteFile(verdictPath, vdata, 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify verdict is readable before claim.
	if _, ok := taskfile.ReadVerdictRejection(dir, filename); !ok {
		t.Fatal("expected verdict to be readable before claim")
	}

	task, err := SelectAndClaimTask(dir, "agent-v", candidates(filename), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (retry exhausted), got %+v", task)
	}

	// Task should be in failed/.
	if _, err := os.Stat(filepath.Join(dir, DirFailed, filename)); err != nil {
		t.Fatalf("task not in failed: %v", err)
	}

	// Verdict file must still exist so a subsequent mato retry can surface
	// the rejection reason as MATO_REVIEW_FEEDBACK.
	vr, ok := taskfile.ReadVerdictRejection(dir, filename)
	if !ok {
		t.Fatal("verdict file should be preserved after retry-exhausted move to failed/")
	}
	if vr.Reason != "needs tests" {
		t.Fatalf("verdict reason = %q, want %q", vr.Reason, "needs tests")
	}
}

func TestSelectAndClaimTask_SkipsExhaustedClaimsNext(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "bad.md"), strings.Join([]string{
		"# Bad",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "good.md"), "# Good\nDo it.\n")

	task, err := SelectAndClaimTask(dir, "agent-3", candidates("bad.md", "good.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected good.md to be claimed, got nil")
	}
	if task.Filename != "good.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "good.md")
	}

	// bad.md should be in failed/
	if _, err := os.Stat(filepath.Join(dir, DirFailed, "bad.md")); err != nil {
		t.Fatalf("bad.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_AllClaimed(t *testing.T) {
	dir := setupClaimTestDir(t)
	// backlog is empty

	task, err := SelectAndClaimTask(dir, "agent-4", candidates("missing.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (no tasks), got %+v", task)
	}
}

func TestSelectAndClaimTask_EmptyCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)

	task, err := SelectAndClaimTask(dir, "agent-5", nil, 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (empty queue), got %+v", task)
	}
}

func TestSelectAndClaimTask_InvalidCandidateNamesSkipped(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "real.md"), "# Real\n")
	testutil.WriteFile(t, filepath.Join(dir, "outside.md"), "# Outside\n")

	task, err := SelectAndClaimTask(dir, "agent-invalid", candidates("../outside.md", "/tmp/evil.md", "", "real.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil || task.Filename != "real.md" {
		t.Fatalf("expected real.md after invalid candidates are skipped, got %+v", task)
	}
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "real.md")); !os.IsNotExist(err) {
		t.Fatal("real.md should be moved out of backlog after claim")
	}
	if _, err := os.Stat(filepath.Join(dir, "outside.md")); err != nil {
		t.Fatalf("outside.md should be untouched: %v", err)
	}
}

func TestSelectAndClaimTask_DeferredExclusion(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "high.md"), "# High\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "low.md"), "# Low\n")
	task, err := SelectAndClaimTask(dir, "agent-6", candidates("low.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected low.md to be claimed, got nil")
	}
	if task.Filename != "low.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "low.md")
	}

	// high.md should remain in backlog
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "high.md")); err != nil {
		t.Fatalf("high.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_DemotesDependencyBlockedBacklogTask(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "blocked.md"), "---\nid: blocked\ndepends_on: [missing]\n---\n# Blocked\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "runnable.md"), "# Runnable\n")

	task, err := SelectAndClaimTask(dir, "agent-dep", candidates("blocked.md", "runnable.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected runnable.md to be claimed, got nil")
	}
	if task.Filename != "runnable.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "runnable.md")
	}
	if _, err := os.Stat(filepath.Join(dir, DirWaiting, "blocked.md")); err != nil {
		t.Fatalf("blocked.md should be moved to waiting/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "blocked.md")); !os.IsNotExist(err) {
		t.Fatal("blocked.md should not remain in backlog/")
	}
}

func TestSelectAndClaimTask_FreshlyEditedDependencyBlockedTaskIsDemoted(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "edited.md"), "# Edited\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "fallback.md"), "# Fallback\n")

	idx := BuildIndex(dir)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "edited.md"), "---\nid: edited\ndepends_on: [missing]\n---\n# Edited\n")

	task, err := SelectAndClaimTask(dir, "agent-fresh", candidates("edited.md", "fallback.md"), 0, idx)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected fallback.md to be claimed, got nil")
	}
	if task.Filename != "fallback.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "fallback.md")
	}
	if _, err := os.Stat(filepath.Join(dir, DirWaiting, "edited.md")); err != nil {
		t.Fatalf("edited.md should be moved to waiting/: %v", err)
	}
}

func TestSelectAndClaimTask_CandidateOrdering(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "z-last.md"), "# Z Last\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "a-first.md"), "# A First\n")

	task, err := SelectAndClaimTask(dir, "agent-7", candidates("z-last.md", "a-first.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected z-last.md to be claimed, got nil")
	}
	if task.Filename != "z-last.md" {
		t.Fatalf("Filename = %q, want %q (should respect candidate order)", task.Filename, "z-last.md")
	}
}

func TestOrderedRunnableFilenames_UsesRunnableViewOrder(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "z-last.md"), "---\npriority: 20\n---\n# Z Last\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "a-first.md"), "---\npriority: 20\n---\n# A First\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "top.md"), "---\npriority: 5\n---\n# Top\n")

	got := runnableCandidates(t, dir, nil, nil)
	want := []string{"top.md", "a-first.md", "z-last.md"}
	if len(got) != len(want) {
		t.Fatalf("len(runnableCandidates) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, name := range want {
		if got[i] != name {
			t.Fatalf("runnableCandidates[%d] = %q, want %q", i, got[i], name)
		}
	}
}

func TestClaimSelection_IndexedMatchesFilesystemFallback(t *testing.T) {
	setup := func(t *testing.T, dir string) {
		t.Helper()
		testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "active.md"), "<!-- branch: task/add-feature -->\n# Active\n")
		testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "add feature.md"), "---\npriority: 5\n---\n# Add Feature\n")
		testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "later.md"), "---\npriority: 20\n---\n# Later\n")
	}

	dirNoIndex := setupClaimTestDir(t)
	setup(t, dirNoIndex)
	dirIndexed := setupClaimTestDir(t)
	setup(t, dirIndexed)

	noIndexOrder := runnableCandidates(t, dirNoIndex, nil, nil)
	idx := BuildIndex(dirIndexed)
	indexedOrder := runnableCandidates(t, dirIndexed, nil, idx)

	if len(noIndexOrder) != len(indexedOrder) {
		t.Fatalf("len(order) mismatch: no-index=%d indexed=%d", len(noIndexOrder), len(indexedOrder))
	}
	for i := range noIndexOrder {
		if noIndexOrder[i] != indexedOrder[i] {
			t.Fatalf("order[%d]: no-index=%q indexed=%q", i, noIndexOrder[i], indexedOrder[i])
		}
	}

	noIndexTask, err := SelectAndClaimTask(dirNoIndex, "agent-noidx", noIndexOrder, 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask(no index): %v", err)
	}
	indexedTask, err := SelectAndClaimTask(dirIndexed, "agent-idx", indexedOrder, 0, idx)
	if err != nil {
		t.Fatalf("SelectAndClaimTask(indexed): %v", err)
	}
	if noIndexTask == nil || indexedTask == nil {
		t.Fatalf("expected both selection paths to claim a task, got no-index=%+v indexed=%+v", noIndexTask, indexedTask)
	}
	if noIndexTask.Filename != indexedTask.Filename {
		t.Fatalf("Filename mismatch: no-index=%q indexed=%q", noIndexTask.Filename, indexedTask.Filename)
	}
	if noIndexTask.Branch != indexedTask.Branch {
		t.Fatalf("Branch mismatch: no-index=%q indexed=%q", noIndexTask.Branch, indexedTask.Branch)
	}
	if noIndexTask.Title != indexedTask.Title {
		t.Fatalf("Title mismatch: no-index=%q indexed=%q", noIndexTask.Title, indexedTask.Title)
	}
}

func TestSelectAndClaimTask_FrontmatterMaxRetries(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "custom.md"), strings.Join([]string{
		"---",
		"max_retries: 1",
		"---",
		"# Custom retries",
		"<!-- failure: one -->",
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-9", candidates("custom.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (custom max_retries=1 exhausted), got %+v", task)
	}

	if _, err := os.Stat(filepath.Join(dir, DirFailed, "custom.md")); err != nil {
		t.Fatalf("custom.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_ClaimedByWriteFailure_FallsBack(t *testing.T) {
	dir := setupClaimTestDir(t)
	// Create two tasks; the first will have its file made unreadable so
	// prependClaimedBy fails, and the second should be claimed instead.
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "broken.md"), "# Broken\nDo broken.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "fallback.md"), "# Fallback\nDo fallback.\n")

	// Make the first file unreadable so prependClaimedBy fails on os.ReadFile.
	// os.Rename only needs directory permissions, so the rename to in-progress
	// and back to backlog will still succeed.
	if err := os.Chmod(filepath.Join(dir, DirBacklog, "broken.md"), 0o000); err != nil {
		t.Fatal(err)
	}

	task, err := SelectAndClaimTask(dir, "agent-cb1", candidates("broken.md", "fallback.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected fallback.md to be claimed, got nil")
	}
	if task.Filename != "fallback.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "fallback.md")
	}

	// broken.md should be back in backlog, not stuck in in-progress
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "broken.md")); err != nil {
		t.Fatalf("broken.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "broken.md")); !os.IsNotExist(err) {
		t.Fatal("broken.md should NOT be in in-progress after claimed-by failure")
	}

	// fallback.md should be in in-progress with claimed-by header
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "fallback.md")); err != nil {
		t.Fatalf("fallback.md not in in-progress: %v", err)
	}
}

func TestSelectAndClaimTask_ClaimedByWriteFailure_AllFail(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "only.md"), "# Only\nDo only.\n")

	// Make the file unreadable so prependClaimedBy fails.
	if err := os.Chmod(filepath.Join(dir, DirBacklog, "only.md"), 0o000); err != nil {
		t.Fatal(err)
	}

	task, err := SelectAndClaimTask(dir, "agent-cb2", candidates("only.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (all candidates failed claimed-by), got %+v", task)
	}

	// The task should be back in backlog, not stuck in in-progress
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "only.md")); err != nil {
		t.Fatalf("only.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "only.md")); !os.IsNotExist(err) {
		t.Fatal("only.md should NOT be in in-progress")
	}
}

func TestPrependClaimedBy(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "task.md")

	original := "---\npriority: 10\n---\n# My Task\nDo the thing.\n"
	testutil.WriteFile(t, taskPath, original)

	if err := prependClaimedBy(taskPath, "agent-42", "2026-01-15T10:00:00Z"); err != nil {
		t.Fatalf("prependClaimedBy: %v", err)
	}

	data, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}

	got := string(data)
	wantPrefix := "<!-- claimed-by: agent-42  claimed-at: 2026-01-15T10:00:00Z -->\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("missing claimed-by header:\ngot:  %q\nwant prefix: %q", got, wantPrefix)
	}

	// Original content must be preserved after the header
	if rest := got[len(wantPrefix):]; rest != original {
		t.Fatalf("original content not preserved:\ngot:  %q\nwant: %q", rest, original)
	}
}

func TestPrependClaimedBy_NonexistentFile(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "missing.md")

	err := prependClaimedBy(taskPath, "agent-1", "2026-01-15T10:00:00Z")
	if err == nil {
		t.Fatal("expected error for nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "read task file for claimed-by header") {
		t.Fatalf("error missing context: %v", err)
	}
}

func TestSelectAndClaimTask_ZeroMaxRetries(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "zero-retry.md"), strings.Join([]string{
		"---",
		"max_retries: 0",
		"---",
		"# Zero retries",
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-10", candidates("zero-retry.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (max_retries=0 means no retries allowed), got %+v", task)
	}

	if _, err := os.Stat(filepath.Join(dir, DirFailed, "zero-retry.md")); err != nil {
		t.Fatalf("zero-retry.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_RollbackFailure_ReturnsError(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "stuck.md"), "# Stuck\nDo stuck.\n")

	origPrepend := claimPrependFn
	origRollback := claimRollbackFn
	t.Cleanup(func() {
		claimPrependFn = origPrepend
		claimRollbackFn = origRollback
	})

	claimPrependFn = func(path, agentID, claimedAt string) error {
		return fmt.Errorf("simulated prepend failure")
	}
	claimRollbackFn = func(src, dst string) error {
		return fmt.Errorf("simulated rollback failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-rb1", candidates("stuck.md"), 0, nil)
	if err == nil {
		t.Fatal("expected error when both prepend and rollback fail, got nil")
	}
	if task != nil {
		t.Fatalf("expected nil task on double failure, got %+v", task)
	}
	if !strings.Contains(err.Error(), "claim rollback failed") {
		t.Fatalf("error should mention claim rollback failure: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated prepend failure") {
		t.Fatalf("error should include prepend cause: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated rollback failure") {
		t.Fatalf("error should include rollback cause: %v", err)
	}

	// Task should be stranded in in-progress (the rollback failed)
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "stuck.md")); err != nil {
		t.Fatalf("stuck.md should remain in in-progress after double failure: %v", err)
	}
}

func TestSelectAndClaimTask_RollbackFailure_SkipsFurtherCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "first.md"), "# First\nDo first.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "second.md"), "# Second\nDo second.\n")

	origPrepend := claimPrependFn
	origRollback := claimRollbackFn
	t.Cleanup(func() {
		claimPrependFn = origPrepend
		claimRollbackFn = origRollback
	})

	// Only the first task triggers double failure; second should not be tried.
	calls := 0
	claimPrependFn = func(path, agentID, claimedAt string) error {
		calls++
		return fmt.Errorf("simulated prepend failure")
	}
	claimRollbackFn = func(src, dst string) error {
		return fmt.Errorf("simulated rollback failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-rb2", candidates("first.md", "second.md"), 0, nil)
	if err == nil {
		t.Fatal("expected error on double failure")
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
	if calls != 1 {
		t.Fatalf("prependClaimedBy called %d times, want 1 (should stop after first double failure)", calls)
	}

	// second.md should still be in backlog (not attempted)
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "second.md")); err != nil {
		t.Fatalf("second.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_PrependFails_RollbackSucceeds_ContinuesToNext(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "broken.md"), "# Broken\nDo broken.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "healthy.md"), "# Healthy\nDo healthy.\n")

	origPrepend := claimPrependFn
	t.Cleanup(func() { claimPrependFn = origPrepend })

	calls := 0
	claimPrependFn = func(path, agentID, claimedAt string) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("simulated prepend failure")
		}
		// Second call succeeds (real implementation)
		return prependClaimedBy(path, agentID, claimedAt)
	}

	task, err := SelectAndClaimTask(dir, "agent-rb3", candidates("broken.md", "healthy.md"), 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task == nil {
		t.Fatal("expected healthy.md to be claimed, got nil")
	}
	if task.Filename != "healthy.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "healthy.md")
	}

	// broken.md should be back in backlog (rollback succeeded)
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "broken.md")); err != nil {
		t.Fatalf("broken.md should be back in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted_MoveToFailedFails_RollbackToBacklog(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "exhausted.md"), strings.Join([]string{
		"# Exhausted",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "healthy.md"), "# Healthy\nDo healthy.\n")

	origMove := retryExhaustedMoveFn
	t.Cleanup(func() { retryExhaustedMoveFn = origMove })

	retryExhaustedMoveFn = func(src, dst string) error {
		return fmt.Errorf("simulated move-to-failed failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-re1", candidates("exhausted.md", "healthy.md"), 0, nil)
	if err == nil {
		t.Fatal("expected error when move-to-failed fails and rollback succeeds, got nil")
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
	if !errors.Is(err, errFailedDirUnavailable) {
		t.Fatalf("error should wrap errFailedDirUnavailable: %v", err)
	}
	var fdErr *FailedDirUnavailableError
	if !errors.As(err, &fdErr) {
		t.Fatalf("error should be a *FailedDirUnavailableError: %v", err)
	}
	if fdErr.TaskFilename != "exhausted.md" {
		t.Fatalf("TaskFilename = %q, want %q", fdErr.TaskFilename, "exhausted.md")
	}
	if !strings.Contains(err.Error(), "simulated move-to-failed failure") {
		t.Fatalf("error should include move-to-failed cause: %v", err)
	}

	// exhausted.md should be back in backlog (rollback succeeded)
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "exhausted.md")); err != nil {
		t.Fatalf("exhausted.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "exhausted.md")); !os.IsNotExist(err) {
		t.Fatal("exhausted.md should NOT be in in-progress")
	}

	// healthy.md should still be in backlog (not attempted after hard error)
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "healthy.md")); err != nil {
		t.Fatalf("healthy.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted_DoubleFailure_ReturnsError(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "stuck.md"), strings.Join([]string{
		"# Stuck",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))

	origMove := retryExhaustedMoveFn
	origRollback := retryExhaustedRollback
	t.Cleanup(func() {
		retryExhaustedMoveFn = origMove
		retryExhaustedRollback = origRollback
	})

	retryExhaustedMoveFn = func(src, dst string) error {
		return fmt.Errorf("simulated move-to-failed failure")
	}
	retryExhaustedRollback = func(src, dst string) error {
		return fmt.Errorf("simulated rollback failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-re2", candidates("stuck.md"), 0, nil)
	if err == nil {
		t.Fatal("expected error when both move-to-failed and rollback fail, got nil")
	}
	if task != nil {
		t.Fatalf("expected nil task on double failure, got %+v", task)
	}
	if !strings.Contains(err.Error(), "retry-exhausted rollback failed") {
		t.Fatalf("error should mention retry-exhausted rollback failure: %v", err)
	}
	if errors.Is(err, errFailedDirUnavailable) {
		t.Fatal("double-failure error should NOT wrap errFailedDirUnavailable (task is stranded, not rolled back)")
	}
	if !strings.Contains(err.Error(), "simulated move-to-failed failure") {
		t.Fatalf("error should include move-to-failed cause: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated rollback failure") {
		t.Fatalf("error should include rollback cause: %v", err)
	}

	// Task should be stranded in in-progress (both moves failed)
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "stuck.md")); err != nil {
		t.Fatalf("stuck.md should remain in in-progress after double failure: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted_DoubleFailure_SkipsFurtherCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "exhausted.md"), strings.Join([]string{
		"# Exhausted",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "second.md"), "# Second\nDo second.\n")

	origMove := retryExhaustedMoveFn
	origRollback := retryExhaustedRollback
	t.Cleanup(func() {
		retryExhaustedMoveFn = origMove
		retryExhaustedRollback = origRollback
	})

	retryExhaustedMoveFn = func(src, dst string) error {
		return fmt.Errorf("simulated move-to-failed failure")
	}
	retryExhaustedRollback = func(src, dst string) error {
		return fmt.Errorf("simulated rollback failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-re3", candidates("exhausted.md", "second.md"), 0, nil)
	if err == nil {
		t.Fatal("expected error on double failure")
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}

	// second.md should still be in backlog (not attempted after hard error)
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "second.md")); err != nil {
		t.Fatalf("second.md should still be in backlog: %v", err)
	}
}

func TestRecoverOrphanedTasks_HandlesStrandedWithoutClaimedBy(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Simulate a task stranded in in-progress without a claimed-by marker
	// (the scenario that occurs after a double failure).
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "orphan.md"), "# Orphan\nDo orphan.\n")

	_ = RecoverOrphanedTasks(dir)

	// Task should be recovered to backlog
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "orphan.md")); err != nil {
		t.Fatalf("orphan.md should be recovered to backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "orphan.md")); !os.IsNotExist(err) {
		t.Fatal("orphan.md should no longer be in in-progress")
	}

	// Verify a failure record was appended
	data, err := os.ReadFile(filepath.Join(dir, DirBacklog, "orphan.md"))
	if err != nil {
		t.Fatalf("read recovered task: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure:") {
		t.Fatal("recovered task should have a failure record appended")
	}
}

func TestIsFailedDirUnavailable(t *testing.T) {
	// Wrapped error should match.
	err := &FailedDirUnavailableError{TaskFilename: "test.md", MoveErr: fmt.Errorf("perm denied")}
	if !IsFailedDirUnavailable(err) {
		t.Fatal("IsFailedDirUnavailable should return true for FailedDirUnavailableError")
	}

	// Plain error should not match.
	if IsFailedDirUnavailable(fmt.Errorf("unrelated error")) {
		t.Fatal("IsFailedDirUnavailable should return false for unrelated errors")
	}

	// nil should not match.
	if IsFailedDirUnavailable(nil) {
		t.Fatal("IsFailedDirUnavailable should return false for nil")
	}
}

func TestSelectAndClaimTask_InvalidYAML_Skipped(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "bad-yaml.md"), strings.Join([]string{
		"---",
		"priority: [invalid",
		"---",
		"# Bad YAML task",
		"Do something.",
		"",
	}, "\n"))

	// Capture stderr to verify warning is printed.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)

	task, claimErr := SelectAndClaimTask(dir, "agent-warn", candidates("bad-yaml.md"), 0, nil)

	ui.SetWarningWriter(prevWarn)
	w.Close()
	captured, readErr := io.ReadAll(r)
	os.Stderr = origStderr
	if readErr != nil {
		t.Fatal(readErr)
	}

	if claimErr != nil {
		t.Fatalf("SelectAndClaimTask: %v", claimErr)
	}
	if task != nil {
		t.Fatalf("expected nil claimed task for malformed backlog task, got %+v", task)
	}
	stderrOutput := string(captured)
	if !strings.Contains(stderrOutput, "warning: could not parse task metadata for bad-yaml.md") {
		t.Fatalf("expected parse-error warning on stderr, got: %q", stderrOutput)
	}
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "bad-yaml.md")); err != nil {
		t.Fatalf("malformed task should remain in backlog until reconciled: %v", err)
	}
}

func TestSelectAndClaimTask_InvalidYAML_ExhaustedRetries_Skipped(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "bad-exhausted.md"), strings.Join([]string{
		"---",
		"max_retries: !!invalid",
		"---",
		"# Exhausted bad YAML",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))

	// Suppress stderr warning output during test.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)

	task, claimErr := SelectAndClaimTask(dir, "agent-exhaust", candidates("bad-exhausted.md"), 0, nil)

	ui.SetWarningWriter(prevWarn)
	w.Close()
	r.Close()
	os.Stderr = origStderr

	if claimErr != nil {
		t.Fatalf("SelectAndClaimTask: %v", claimErr)
	}
	if task != nil {
		t.Fatalf("expected nil claimed task for malformed backlog task, got %+v", task)
	}
	if _, err := os.Stat(filepath.Join(dir, DirFailed, "bad-exhausted.md")); !os.IsNotExist(err) {
		t.Fatalf("malformed task should not be moved to failed by claim path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "bad-exhausted.md")); err != nil {
		t.Fatalf("malformed task should remain in backlog until reconciled: %v", err)
	}
}

func TestCountFailureLines_NonexistentFile(t *testing.T) {
	count, err := CountFailureLines("/nonexistent/path/task.md")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if count != 0 {
		t.Fatalf("expected count 0, got %d", count)
	}
}

func TestCountFailureLines_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	testutil.WriteFile(t, path, strings.Join([]string{
		"# Task",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- failure: agent-2 at 2025-01-02T00:00:00Z step=PUSH error=push -->",
	}, "\n"))

	count, err := CountFailureLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 failures, got %d", count)
	}
}

func TestCountFailureLines_IgnoresBodyText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	testutil.WriteFile(t, path, strings.Join([]string{
		"# Separate review retry budget",
		"",
		"`CountFailureLines()` counts `<!-- failure: ... -->` records in the file.",
		"Review failures use `<!-- failure: agent-id at timestamp -->` markers.",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
	}, "\n"))

	count, err := CountFailureLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 actual failure (ignoring body text), got %d", count)
	}
}

func TestCountFailureLines_IgnoresReviewFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	testutil.WriteFile(t, path, strings.Join([]string{
		"# Task",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- review-failure: rev-1 at 2025-01-02T00:00:00Z step=DIFF error=fetch -->",
		"<!-- failure: agent-2 at 2025-01-03T00:00:00Z step=PUSH error=push -->",
		"<!-- review-failure: rev-2 at 2025-01-04T00:00:00Z step=DIFF error=timeout -->",
	}, "\n"))

	count, err := CountFailureLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 task failures (ignoring review-failures), got %d", count)
	}
}

func TestCountReviewFailureLines_NonexistentFile(t *testing.T) {
	count, err := CountReviewFailureLines("/nonexistent/path/task.md")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
	if count != 0 {
		t.Fatalf("expected count 0, got %d", count)
	}
}

func TestCountReviewFailureLines_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	testutil.WriteFile(t, path, strings.Join([]string{
		"# Task",
		"<!-- review-failure: rev-1 at 2025-01-01T00:00:00Z step=DIFF error=fetch -->",
		"<!-- review-failure: rev-2 at 2025-01-02T00:00:00Z step=DIFF error=timeout -->",
	}, "\n"))

	count, err := CountReviewFailureLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 review failures, got %d", count)
	}
}

func TestCountReviewFailureLines_IgnoresTaskFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	testutil.WriteFile(t, path, strings.Join([]string{
		"# Task",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- review-failure: rev-1 at 2025-01-02T00:00:00Z step=DIFF error=fetch -->",
		"<!-- failure: agent-2 at 2025-01-03T00:00:00Z step=PUSH error=push -->",
	}, "\n"))

	count, err := CountReviewFailureLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 review failure (ignoring task failures), got %d", count)
	}
}

func TestCountReviewFailureLines_IgnoresBodyText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	testutil.WriteFile(t, path, strings.Join([]string{
		"# Task",
		"",
		"Review failures use `<!-- review-failure: -->` markers.",
		"<!-- review-failure: rev-1 at 2025-01-01T00:00:00Z step=DIFF error=fetch -->",
	}, "\n"))

	count, err := CountReviewFailureLines(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 actual review failure (ignoring body text), got %d", count)
	}
}

func TestSelectAndClaimTask_UnreadableFile_Skipped(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Create a task file in backlog, then make it unreadable.
	taskPath := filepath.Join(dir, DirBacklog, "unreadable.md")
	testutil.WriteFile(t, taskPath, "# Unreadable\nDo stuff.\n")
	if err := os.Chmod(taskPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(taskPath, 0o644) })

	// Also add a readable fallback task.
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "readable.md"), "# Readable\nDo stuff.\n")

	// Capture stderr to verify warning.
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)

	task, err := SelectAndClaimTask(dir, "agent-x", candidates("unreadable.md", "readable.md"), 0, nil)

	ui.SetWarningWriter(prevWarn)
	w.Close()
	stderrBytes, _ := io.ReadAll(r)
	os.Stderr = oldStderr

	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected readable.md to be claimed, got nil")
	}
	if task.Filename != "readable.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "readable.md")
	}

	stderrStr := string(stderrBytes)
	if !strings.Contains(stderrStr, "could not parse task metadata for unreadable.md") {
		t.Fatalf("expected warning about unreadable file in stderr, got: %s", stderrStr)
	}
}

func TestSelectAndClaimTask_BranchCollisionAddsDisambiguator(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Create a task already in-progress with branch comment matching what
	// the new task would get.
	inProgressContent := "<!-- branch: task/add-feature -->\n<!-- claimed-by: agent-0  claimed-at: 2026-01-01T00:00:00Z -->\n# Add Feature\n"
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "add-feature.md"), inProgressContent)

	// Create a new backlog task whose sanitized name also resolves to
	// "task/add-feature" (spaces become dashes).
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "add feature.md"), "# Add Feature (v2)\n")

	task, err := SelectAndClaimTask(dir, "agent-coll1", candidates("add feature.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task, got nil")
	}

	// The branch should have a disambiguation suffix since "task/add-feature"
	// is already taken.
	if task.Branch == "task/add-feature" {
		t.Fatalf("Branch should have been disambiguated, got %q", task.Branch)
	}
	if !strings.HasPrefix(task.Branch, "task/add-feature-") {
		t.Fatalf("Branch should start with task/add-feature-, got %q", task.Branch)
	}
	// Suffix should be 6 hex chars.
	suffix := strings.TrimPrefix(task.Branch, "task/add-feature-")
	if len(suffix) != 6 {
		t.Fatalf("disambiguation suffix should be 6 chars, got %q", suffix)
	}

	// The branch comment should be written to the in-progress file.
	data, err := os.ReadFile(task.TaskPath)
	if err != nil {
		t.Fatalf("read claimed task: %v", err)
	}
	if !strings.Contains(string(data), "<!-- branch: "+task.Branch+" -->") {
		t.Fatalf("branch comment not found in claimed task:\n%s", string(data))
	}
}

func TestSelectAndClaimTask_ReusesExistingBranchMarker(t *testing.T) {
	dir := setupClaimTestDir(t)
	content := strings.Join([]string{
		"<!-- branch: task/existing-branch -->",
		"# Retry task",
		"",
	}, "\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "retry.md"), content)

	task, err := SelectAndClaimTask(dir, "agent-reuse", candidates("retry.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected claimed task, got nil")
	}
	if task.Branch != "task/existing-branch" {
		t.Fatalf("Branch = %q, want %q", task.Branch, "task/existing-branch")
	}
	if !task.HadRecordedBranchMark {
		t.Fatal("HadRecordedBranchMark = false, want true")
	}
	data, err := os.ReadFile(task.TaskPath)
	if err != nil {
		t.Fatalf("read claimed task: %v", err)
	}
	if strings.Count(string(data), "<!-- branch:") != 1 {
		t.Fatalf("expected exactly one branch marker, got:\n%s", string(data))
	}
}

func TestSelectAndClaimTask_NewTaskDoesNotSetRecordedBranchFlag(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "fresh.md"), "# Fresh\n")

	task, err := SelectAndClaimTask(dir, "agent-fresh", candidates("fresh.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected claimed task, got nil")
	}
	if task.HadRecordedBranchMark {
		t.Fatal("HadRecordedBranchMark = true, want false")
	}
}

func TestSelectAndClaimTask_RewritesCollidingExistingBranchMarker(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirReadyReview, "active.md"), "<!-- branch: task/existing-branch -->\n# Active\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "retry.md"), "<!-- branch: task/existing-branch -->\n# Retry\n")

	task, err := SelectAndClaimTask(dir, "agent-collision", candidates("retry.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected claimed task, got nil")
	}
	if task.Branch == "task/existing-branch" {
		t.Fatalf("expected branch collision rewrite, got %q", task.Branch)
	}
	data, err := os.ReadFile(task.TaskPath)
	if err != nil {
		t.Fatalf("read claimed task: %v", err)
	}
	if strings.Contains(string(data), "<!-- branch: task/existing-branch -->") {
		t.Fatalf("stale branch marker should be replaced, got:\n%s", string(data))
	}
	if !strings.Contains(string(data), "<!-- branch: "+task.Branch+" -->") {
		t.Fatalf("new branch marker missing, got:\n%s", string(data))
	}
}

func TestSelectAndClaimTask_BranchMarkerWriteFailureRollsBackToBacklog(t *testing.T) {
	dir := setupClaimTestDir(t)
	taskPath := filepath.Join(dir, DirBacklog, "retry.md")
	original := "# Retry\n"
	testutil.WriteFile(t, taskPath, original)

	origWrite := claimWriteFileFn
	t.Cleanup(func() { claimWriteFileFn = origWrite })
	claimWriteFileFn = func(path string, data []byte) error {
		if path == filepath.Join(dir, DirInProgress, "retry.md") && strings.Contains(string(data), "<!-- branch:") {
			return fmt.Errorf("simulated branch marker write failure")
		}
		return origWrite(path, data)
	}

	task, err := SelectAndClaimTask(dir, "agent-fail", candidates("retry.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil task after rollback, got %+v", task)
	}
	mustBacklog := filepath.Join(dir, DirBacklog, "retry.md")
	data, err := os.ReadFile(mustBacklog)
	if err != nil {
		t.Fatalf("read rolled-back task: %v", err)
	}
	if string(data) != original {
		t.Fatalf("rolled-back task = %q, want %q", string(data), original)
	}
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "retry.md")); !os.IsNotExist(err) {
		t.Fatal("task should not remain in in-progress/ after rollback")
	}
}

func TestSelectAndClaimTask_BranchCollisionFromMalformedActiveTask(t *testing.T) {
	dir := setupClaimTestDir(t)
	broken := strings.Join([]string{
		"<!-- branch: task/add-feature -->",
		"---",
		"priority: [broken",
		"---",
		"# Broken Active",
		"",
	}, "\n")
	testutil.WriteFile(t, filepath.Join(dir, DirReadyReview, "broken.md"), broken)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "add feature.md"), "# Add Feature (v2)\n")

	idx := BuildIndex(dir)
	task, err := SelectAndClaimTask(dir, "agent-coll2", candidates("add feature.md"), 0, idx)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task, got nil")
	}
	if task.Branch == "task/add-feature" {
		t.Fatalf("Branch should have been disambiguated, got %q", task.Branch)
	}
}

func TestSelectAndClaimTask_NoBranchCollision_NormalBranch(t *testing.T) {
	dir := setupClaimTestDir(t)

	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "unique-task.md"), "# Unique Task\n")

	task, err := SelectAndClaimTask(dir, "agent-nocoll", candidates("unique-task.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task, got nil")
	}

	// No collision, so branch should be the normal sanitized name.
	if task.Branch != "task/unique-task" {
		t.Fatalf("Branch = %q, want %q", task.Branch, "task/unique-task")
	}
}

func TestCollectActiveBranches(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Write two in-progress tasks with branch comments.
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "a.md"), "<!-- branch: task/alpha -->\n# A\n")
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "b.md"), "<!-- branch: task/beta -->\n# B\n")

	// One without a branch comment.
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "c.md"), "# C (no branch)\n")

	// Tasks in ready-for-review and ready-to-merge should also be found.
	testutil.WriteFile(t, filepath.Join(dir, DirReadyReview, "d.md"), "<!-- branch: task/delta -->\n# D\n")
	testutil.WriteFile(t, filepath.Join(dir, DirReadyMerge, "e.md"), "<!-- branch: task/epsilon -->\n# E\n")

	active := CollectActiveBranches(dir, nil)

	for _, want := range []string{"task/alpha", "task/beta", "task/delta", "task/epsilon"} {
		if _, ok := active[want]; !ok {
			t.Errorf("expected %s in active branches", want)
		}
	}
	if len(active) != 4 {
		t.Fatalf("expected 4 active branches, got %d: %v", len(active), active)
	}
}

func TestReadBranchFromFile(t *testing.T) {
	dir := t.TempDir()

	// File with branch comment.
	withBranch := filepath.Join(dir, "with.md")
	testutil.WriteFile(t, withBranch, "<!-- branch: task/my-branch -->\n<!-- claimed-by: agent -->\n# Title\n")
	if b := readBranchFromFile(withBranch); b != "task/my-branch" {
		t.Fatalf("readBranchFromFile = %q, want %q", b, "task/my-branch")
	}

	// File without branch comment.
	without := filepath.Join(dir, "without.md")
	testutil.WriteFile(t, without, "<!-- claimed-by: agent -->\n# Title\n")
	if b := readBranchFromFile(without); b != "" {
		t.Fatalf("readBranchFromFile = %q, want empty", b)
	}

	// Nonexistent file.
	if b := readBranchFromFile(filepath.Join(dir, "missing.md")); b != "" {
		t.Fatalf("readBranchFromFile on missing file = %q, want empty", b)
	}
}

func TestWriteBranchMarker(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "task.md")
	original := "<!-- claimed-by: agent-1 -->\n# My Task\nDo it.\n"
	testutil.WriteFile(t, taskPath, original)

	if err := WriteBranchMarker(taskPath, "task/my-task"); err != nil {
		t.Fatalf("WriteBranchMarker: %v", err)
	}

	data, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}

	got := string(data)
	// The branch comment should be inserted after the claimed-by line.
	want := "<!-- claimed-by: agent-1 -->\n<!-- branch: task/my-task -->\n# My Task\nDo it.\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteBranchMarker_ReplacesExistingStandaloneMarker(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "task.md")
	original := "<!-- claimed-by: agent-1 -->\n<!-- branch: task/old -->\n# My Task\n"
	testutil.WriteFile(t, taskPath, original)

	if err := WriteBranchMarker(taskPath, "task/new"); err != nil {
		t.Fatalf("WriteBranchMarker: %v", err)
	}
	data, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "task/old") {
		t.Fatalf("old branch marker still present:\n%s", got)
	}
	if !strings.Contains(got, "<!-- branch: task/new -->") {
		t.Fatalf("new branch marker missing:\n%s", got)
	}
}

func TestSelectAndClaimTask_DestinationExistsInProgress(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "dup.md"), "# Dup\nDo dup.\n")
	// Pre-existing file in in-progress/ with same name.
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "dup.md"),
		"<!-- claimed-by: other -->\n# Dup\nAlready claimed.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "ok.md"), "# OK\nDo ok.\n")

	task, err := SelectAndClaimTask(dir, "agent-dup", candidates("dup.md", "ok.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	// dup.md should be skipped (destination collision); ok.md should be claimed.
	if task == nil {
		t.Fatal("expected ok.md to be claimed, got nil")
	}
	if task.Filename != "ok.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "ok.md")
	}

	// The in-progress copy must not be overwritten.
	data, _ := os.ReadFile(filepath.Join(dir, DirInProgress, "dup.md"))
	if !strings.Contains(string(data), "Already claimed") {
		t.Fatal("in-progress/dup.md was overwritten by the claim move")
	}

	// The backlog copy of dup.md must still exist (source not removed).
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "dup.md")); err != nil {
		t.Fatalf("backlog/dup.md should still exist after destination collision: %v", err)
	}
}

func TestSelectAndClaimTask_DestinationExistsInFailed(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "old.md"), strings.Join([]string{
		"# Old task",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	// Pre-existing file in failed/ with same name.
	testutil.WriteFile(t, filepath.Join(dir, DirFailed, "old.md"), "# Old task (original)\n")

	task, err := SelectAndClaimTask(dir, "agent-fd", candidates("old.md"), 0, nil)
	// Retry-exhausted move to failed/ fails (EEXIST), rollback succeeds,
	// so FailedDirUnavailableError is returned.
	if err == nil {
		t.Fatal("expected error when move-to-failed destination exists")
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
	if !IsFailedDirUnavailable(err) {
		t.Fatalf("expected FailedDirUnavailableError, got: %v", err)
	}

	// The failed/ copy must not be overwritten.
	data, _ := os.ReadFile(filepath.Join(dir, DirFailed, "old.md"))
	if !strings.Contains(string(data), "original") {
		t.Fatal("failed/old.md was overwritten")
	}

	// Task should be rolled back to backlog.
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "old.md")); err != nil {
		t.Fatalf("old.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "old.md")); !os.IsNotExist(err) {
		t.Fatal("old.md should NOT be in in-progress after rollback")
	}
}

func TestSelectAndClaimTask_RollbackDestinationExists(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "race.md"), "# Race\n")

	origPrepend := claimPrependFn
	t.Cleanup(func() { claimPrependFn = origPrepend })

	// Force prepend to fail, then sneak a file back into backlog to
	// simulate a concurrent race so the rollback hits EEXIST.
	claimPrependFn = func(path, agentID, claimedAt string) error {
		testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "race.md"), "# Race (reappeared)\n")
		return fmt.Errorf("simulated prepend failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-race", candidates("race.md"), 0, nil)
	// Rollback via AtomicMove fails because backlog/race.md reappeared,
	// resulting in a hard error (task stranded in in-progress).
	if err == nil {
		t.Fatal("expected hard error when rollback destination exists")
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}
	if !strings.Contains(err.Error(), "claim rollback failed") {
		t.Fatalf("expected 'claim rollback failed' in error, got: %v", err)
	}

	// backlog/race.md should be the reappeared copy (not overwritten).
	data, _ := os.ReadFile(filepath.Join(dir, DirBacklog, "race.md"))
	if !strings.Contains(string(data), "reappeared") {
		t.Fatal("backlog/race.md should contain the reappeared copy")
	}
}

func TestOrderedRunnableFilenames_ExcludesNames(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "keep.md"), "---\npriority: 10\n---\n# Keep\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "skip.md"), "---\npriority: 5\n---\n# Skip\n")

	got := runnableCandidates(t, dir, map[string]struct{}{"skip.md": {}}, nil)
	if len(got) != 1 || got[0] != "keep.md" {
		t.Fatalf("runnableCandidates = %v, want [keep.md]", got)
	}
}

func TestSelectAndClaimTask_MissingCandidatesSkipped(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "real.md"), "# Real\n")

	task, err := SelectAndClaimTask(dir, "agent-m1", candidates("ghost-a.md", "ghost-b.md", "real.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected real.md to be claimable after missing candidates are skipped, got nil")
	}
	if task.Filename != "real.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "real.md")
	}
}

func TestSelectAndClaimTask_CandidateOrderPreservesOrder(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "a-first.md"), "# A\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "b-second.md"), "# B\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "c-third.md"), "# C\n")

	want := []string{"b-second.md", "a-first.md", "c-third.md"}
	for i, wantName := range want {
		task, err := SelectAndClaimTask(dir, fmt.Sprintf("agent-x%d", i), want[i:], 0, nil)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if task == nil {
			t.Fatalf("claim %d: expected %s, got nil", i, wantName)
		}
		if task.Filename != wantName {
			t.Fatalf("claim %d: Filename = %q, want %q", i, task.Filename, wantName)
		}
	}

	// No more tasks should be claimable.
	task, err := SelectAndClaimTask(dir, "agent-x3", nil, 0, nil)
	if err != nil {
		t.Fatalf("final claim: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil after all tasks claimed, got %+v", task)
	}
}

func TestSelectAndClaimTask_DuplicateCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "dup.md"), "# Dup\n")

	task, err := SelectAndClaimTask(dir, "agent-d1", candidates("dup.md", "dup.md", "dup.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil || task.Filename != "dup.md" {
		t.Fatalf("expected dup.md, got %v", task)
	}

	// After claiming, no more candidates should exist.
	task2, err := SelectAndClaimTask(dir, "agent-d2", candidates("dup.md", "dup.md", "dup.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task2 != nil {
		t.Fatalf("expected nil (duplicate should not produce extra candidates), got %+v", task2)
	}
}

func TestLastFailureTime(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		wantOK bool
		wantTS string
	}{
		{
			name:   "no failures",
			data:   "# Task\nDo stuff.\n",
			wantOK: false,
		},
		{
			name:   "single failure with timestamp",
			data:   "# Task\n<!-- failure: agent-1 at 2026-03-01T10:00:00Z step=WORK error=build_failed -->\n",
			wantOK: true,
			wantTS: "2026-03-01T10:00:00Z",
		},
		{
			name: "multiple failures returns last",
			data: strings.Join([]string{
				"# Task",
				"<!-- failure: agent-1 at 2026-03-01T10:00:00Z step=WORK error=first -->",
				"<!-- failure: agent-2 at 2026-03-01T12:00:00Z step=WORK error=second -->",
			}, "\n"),
			wantOK: true,
			wantTS: "2026-03-01T12:00:00Z",
		},
		{
			name:   "failure without timestamp",
			data:   "# Task\n<!-- failure: agent-1 -->\n",
			wantOK: false,
		},
		{
			name:   "review-failure excluded",
			data:   "# Task\n<!-- review-failure: agent-1 at 2026-03-01T10:00:00Z step=WORK error=oops -->\n",
			wantOK: false,
		},
		{
			name: "failure marker in fenced code block ignored",
			data: strings.Join([]string{
				"# Task",
				"```",
				"<!-- failure: agent-1 at 2026-03-01T10:00:00Z step=WORK error=build_failed -->",
				"```",
			}, "\n"),
			wantOK: false,
		},
		{
			name: "failure marker in prose ignored",
			data: strings.Join([]string{
				"# Task",
				"The marker format is <!-- failure: agent-1 at 2026-03-01T10:00:00Z step=WORK error=example -->.",
			}, "\n"),
			wantOK: false,
		},
		{
			name: "real marker outside fence with fake marker inside fence",
			data: strings.Join([]string{
				"# Task",
				"```",
				"<!-- failure: agent-1 at 2026-03-01T10:00:00Z step=WORK error=fake -->",
				"```",
				"<!-- failure: agent-2 at 2026-03-01T14:00:00Z step=WORK error=real -->",
			}, "\n"),
			wantOK: true,
			wantTS: "2026-03-01T14:00:00Z",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lastFailureTime([]byte(tt.data))
			if ok != tt.wantOK {
				t.Fatalf("lastFailureTime ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK {
				want, _ := time.Parse(time.RFC3339, tt.wantTS)
				if !got.Equal(want) {
					t.Fatalf("lastFailureTime = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestRetryCooldown(t *testing.T) {
	tests := []struct {
		name     string
		cooldown time.Duration
		want     time.Duration
	}{
		{"default", 0, DefaultRetryCooldown},
		{"custom valid", 5 * time.Minute, 5 * time.Minute},
		{"custom seconds", 30 * time.Second, 30 * time.Second},
		{"negative falls back", -1 * time.Minute, DefaultRetryCooldown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retryCooldown(tt.cooldown)
			if got != tt.want {
				t.Fatalf("retryCooldown(%v) = %v, want %v", tt.cooldown, got, tt.want)
			}
		})
	}
}

func TestSelectAndClaimTask_RecentFailure_Skipped(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Task with a very recent failure — should be skipped.
	recentTS := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "hot.md"), strings.Join([]string{
		"# Hot task",
		fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", recentTS),
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-2", candidates("hot.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (task in cooldown), got %+v", task)
	}

	// Task should still be in backlog, not moved.
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "hot.md")); err != nil {
		t.Fatalf("task should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_OldFailure_Claimed(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Task with an old failure (well past cooldown) — should be claimed.
	oldTS := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "cold.md"), strings.Join([]string{
		"# Cold task",
		fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", oldTS),
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-2", candidates("cold.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task (old failure past cooldown), got nil")
	}
	if task.Filename != "cold.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "cold.md")
	}
}

func TestSelectAndClaimTask_NoFailures_ClaimedImmediately(t *testing.T) {
	dir := setupClaimTestDir(t)

	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "fresh.md"), "# Fresh task\nDo stuff.\n")

	task, err := SelectAndClaimTask(dir, "agent-3", candidates("fresh.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task (no failures), got nil")
	}
	if task.Filename != "fresh.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "fresh.md")
	}
}

func TestSelectAndClaimTask_CustomCooldown(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Failure at 90 seconds ago. Default cooldown is 2m, so it would be
	// skipped. But with a 1m cooldown it should be claimable.
	ts := time.Now().UTC().Add(-90 * time.Second).Format(time.RFC3339)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "custom.md"), strings.Join([]string{
		"# Custom cooldown task",
		fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", ts),
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-4", candidates("custom.md"), time.Minute, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task (past custom 1m cooldown), got nil")
	}
	if task.Filename != "custom.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "custom.md")
	}
}

func TestSelectAndClaimTask_RecentFailure_SkipsToNext(t *testing.T) {
	dir := setupClaimTestDir(t)

	// First task has a recent failure — should be skipped.
	recentTS := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "aaa-hot.md"), strings.Join([]string{
		"# Hot task",
		fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", recentTS),
		"",
	}, "\n"))
	// Second task has no failures — should be claimed.
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "bbb-ok.md"), "# OK task\nDo stuff.\n")

	task, err := SelectAndClaimTask(dir, "agent-5", candidates("aaa-hot.md", "bbb-ok.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected bbb-ok.md to be claimed, got nil")
	}
	if task.Filename != "bbb-ok.md" {
		t.Fatalf("Filename = %q, want %q (should skip hot task)", task.Filename, "bbb-ok.md")
	}

	// Hot task should still be in backlog.
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "aaa-hot.md")); err != nil {
		t.Fatalf("hot task should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_FencedCodeFailure_NoCooldown(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Task with a recent failure marker inside a fenced code block should
	// NOT trigger retry cooldown — the marker is not standalone.
	recentTS := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "fenced.md"), strings.Join([]string{
		"# Fenced task",
		"```",
		fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", recentTS),
		"```",
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-6", candidates("fenced.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task (fenced code failure should not trigger cooldown), got nil")
	}
	if task.Filename != "fenced.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "fenced.md")
	}
}

func TestSelectAndClaimTask_ProseFailure_NoCooldown(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Task with a failure marker embedded in prose (not on its own line)
	// should NOT trigger retry cooldown.
	recentTS := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "prose.md"), strings.Join([]string{
		"# Prose task",
		fmt.Sprintf("The marker format is <!-- failure: agent-1 at %s step=WORK error=crash -->.", recentTS),
		"",
	}, "\n"))

	task, err := SelectAndClaimTask(dir, "agent-7", candidates("prose.md"), 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a claimed task (prose failure should not trigger cooldown), got nil")
	}
	if task.Filename != "prose.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "prose.md")
	}
}

func TestHasClaimableBacklogTask(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, dir string)
		cooldown time.Duration
		want     bool
	}{
		{
			name: "fresh backlog task is claimable",
			setup: func(t *testing.T, dir string) {
				testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "fresh.md"), "# Fresh\n")
			},
			want: true,
		},
		{
			name: "recent failure in cooldown is not claimable",
			setup: func(t *testing.T, dir string) {
				recentTS := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
				testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "hot.md"), strings.Join([]string{
					"# Hot",
					fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", recentTS),
					"",
				}, "\n"))
			},
			want: false,
		},
		{
			name: "retry exhausted task remains actionable for failure handling",
			setup: func(t *testing.T, dir string) {
				ts1 := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
				ts2 := time.Now().UTC().Add(-9 * time.Minute).Format(time.RFC3339)
				testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "exhausted.md"), strings.Join([]string{
					"---",
					"max_retries: 2",
					"---",
					"# Exhausted",
					fmt.Sprintf("<!-- failure: agent-1 at %s step=WORK error=crash -->", ts1),
					fmt.Sprintf("<!-- failure: agent-2 at %s step=WORK error=crash -->", ts2),
					"",
				}, "\n"))
			},
			want: true,
		},
		{
			name: "dependency blocked backlog task is not claimable",
			setup: func(t *testing.T, dir string) {
				testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "blocked.md"), strings.Join([]string{
					"---",
					"depends_on: [missing-task]",
					"---",
					"# Blocked",
					"",
				}, "\n"))
			},
			want: false,
		},
		{
			name: "excluded runnable task is not claimable",
			setup: func(t *testing.T, dir string) {
				testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "skip.md"), "# Skip\n")
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupClaimTestDir(t)
			tt.setup(t, dir)
			exclude := map[string]struct{}(nil)
			if tt.name == "excluded runnable task is not claimable" {
				exclude = map[string]struct{}{"skip.md": {}}
			}
			got := HasClaimableBacklogTask(dir, exclude, tt.cooldown, nil)
			if got != tt.want {
				t.Fatalf("HasClaimableBacklogTask() = %v, want %v", got, tt.want)
			}

			idx := BuildIndex(dir)
			gotWithIndex := HasClaimableBacklogTask(dir, exclude, tt.cooldown, idx)
			if gotWithIndex != got {
				t.Fatalf("HasClaimableBacklogTask(indexed) = %v, want %v", gotWithIndex, got)
			}
		})
	}
}
