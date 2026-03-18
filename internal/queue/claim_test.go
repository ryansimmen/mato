package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupClaimTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSelectAndClaimTask_Normal(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "alpha.md"), "# Alpha\nDo alpha.\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "beta.md"), "# Beta\nDo beta.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "alpha.md\nbeta.md\n")

	task, err := SelectAndClaimTask(dir, "agent-1", nil)
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
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "alpha.md")); err != nil {
		t.Fatalf("task not in in-progress: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backlog", "alpha.md")); !os.IsNotExist(err) {
		t.Fatal("task still in backlog after claim")
	}

	// Check claimed-by header
	data, _ := os.ReadFile(filepath.Join(dir, "in-progress", "alpha.md"))
	if !strings.HasPrefix(string(data), "<!-- claimed-by: agent-1  claimed-at: ") {
		t.Fatalf("missing claimed-by header: %q", string(data))
	}

	// Beta should still be in backlog
	if _, err := os.Stat(filepath.Join(dir, "backlog", "beta.md")); err != nil {
		t.Fatalf("beta should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "retry.md"), strings.Join([]string{
		"# Retry task",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, ".queue"), "retry.md\n")

	task, err := SelectAndClaimTask(dir, "agent-2", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (retry exhausted), got %+v", task)
	}

	// Task should be in failed/
	if _, err := os.Stat(filepath.Join(dir, "failed", "retry.md")); err != nil {
		t.Fatalf("task not in failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "backlog", "retry.md")); !os.IsNotExist(err) {
		t.Fatal("task still in backlog after retry exhaustion")
	}
}

func TestSelectAndClaimTask_SkipsExhaustedClaimsNext(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "bad.md"), strings.Join([]string{
		"# Bad",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, "backlog", "good.md"), "# Good\nDo it.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "bad.md\ngood.md\n")

	task, err := SelectAndClaimTask(dir, "agent-3", nil)
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
	if _, err := os.Stat(filepath.Join(dir, "failed", "bad.md")); err != nil {
		t.Fatalf("bad.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_AllClaimed(t *testing.T) {
	dir := setupClaimTestDir(t)
	// backlog is empty
	writeTestFile(t, filepath.Join(dir, ".queue"), "missing.md\n")

	task, err := SelectAndClaimTask(dir, "agent-4", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (no tasks), got %+v", task)
	}
}

func TestSelectAndClaimTask_EmptyQueue(t *testing.T) {
	dir := setupClaimTestDir(t)

	task, err := SelectAndClaimTask(dir, "agent-5", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (empty queue), got %+v", task)
	}
}

func TestSelectAndClaimTask_DeferredExclusion(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "high.md"), "# High\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "low.md"), "# Low\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "high.md\nlow.md\n")

	deferred := map[string]struct{}{"high.md": {}}
	task, err := SelectAndClaimTask(dir, "agent-6", deferred)
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
	if _, err := os.Stat(filepath.Join(dir, "backlog", "high.md")); err != nil {
		t.Fatalf("high.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_QueueFileOrdering(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "z-last.md"), "# Z Last\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "a-first.md"), "# A First\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "z-last.md\na-first.md\n")

	task, err := SelectAndClaimTask(dir, "agent-7", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected z-last.md to be claimed, got nil")
	}
	if task.Filename != "z-last.md" {
		t.Fatalf("Filename = %q, want %q (should respect .queue order)", task.Filename, "z-last.md")
	}
}

func TestSelectAndClaimTask_NoQueueFileUsesAlphabetical(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "z-last.md"), "# Z Last\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "a-first.md"), "# A First\n")

	task, err := SelectAndClaimTask(dir, "agent-8", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task == nil {
		t.Fatal("expected a-first.md to be claimed, got nil")
	}
	if task.Filename != "a-first.md" {
		t.Fatalf("Filename = %q, want %q (alphabetical without .queue)", task.Filename, "a-first.md")
	}
}

func TestSelectAndClaimTask_FrontmatterMaxRetries(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "custom.md"), strings.Join([]string{
		"---",
		"max_retries: 1",
		"---",
		"# Custom retries",
		"<!-- failure: one -->",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, ".queue"), "custom.md\n")

	task, err := SelectAndClaimTask(dir, "agent-9", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (custom max_retries=1 exhausted), got %+v", task)
	}

	if _, err := os.Stat(filepath.Join(dir, "failed", "custom.md")); err != nil {
		t.Fatalf("custom.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_ClaimedByWriteFailure_FallsBack(t *testing.T) {
	dir := setupClaimTestDir(t)
	// Create two tasks; the first will have its file made unreadable so
	// prependClaimedBy fails, and the second should be claimed instead.
	writeTestFile(t, filepath.Join(dir, "backlog", "broken.md"), "# Broken\nDo broken.\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "fallback.md"), "# Fallback\nDo fallback.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "broken.md\nfallback.md\n")

	// Make the first file unreadable so prependClaimedBy fails on os.ReadFile.
	// os.Rename only needs directory permissions, so the rename to in-progress
	// and back to backlog will still succeed.
	if err := os.Chmod(filepath.Join(dir, "backlog", "broken.md"), 0o000); err != nil {
		t.Fatal(err)
	}

	task, err := SelectAndClaimTask(dir, "agent-cb1", nil)
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
	if _, err := os.Stat(filepath.Join(dir, "backlog", "broken.md")); err != nil {
		t.Fatalf("broken.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "broken.md")); !os.IsNotExist(err) {
		t.Fatal("broken.md should NOT be in in-progress after claimed-by failure")
	}

	// fallback.md should be in in-progress with claimed-by header
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "fallback.md")); err != nil {
		t.Fatalf("fallback.md not in in-progress: %v", err)
	}
}

func TestSelectAndClaimTask_ClaimedByWriteFailure_AllFail(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "only.md"), "# Only\nDo only.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "only.md\n")

	// Make the file unreadable so prependClaimedBy fails.
	if err := os.Chmod(filepath.Join(dir, "backlog", "only.md"), 0o000); err != nil {
		t.Fatal(err)
	}

	task, err := SelectAndClaimTask(dir, "agent-cb2", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (all candidates failed claimed-by), got %+v", task)
	}

	// The task should be back in backlog, not stuck in in-progress
	if _, err := os.Stat(filepath.Join(dir, "backlog", "only.md")); err != nil {
		t.Fatalf("only.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "only.md")); !os.IsNotExist(err) {
		t.Fatal("only.md should NOT be in in-progress")
	}
}

func TestPrependClaimedBy(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "task.md")

	original := "---\npriority: 10\n---\n# My Task\nDo the thing.\n"
	writeTestFile(t, taskPath, original)

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
	writeTestFile(t, filepath.Join(dir, "backlog", "zero-retry.md"), strings.Join([]string{
		"---",
		"max_retries: 0",
		"---",
		"# Zero retries",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, ".queue"), "zero-retry.md\n")

	task, err := SelectAndClaimTask(dir, "agent-10", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if task != nil {
		t.Fatalf("expected nil (max_retries=0 means no retries allowed), got %+v", task)
	}

	if _, err := os.Stat(filepath.Join(dir, "failed", "zero-retry.md")); err != nil {
		t.Fatalf("zero-retry.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_RollbackFailure_ReturnsError(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "stuck.md"), "# Stuck\nDo stuck.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "stuck.md\n")

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

	task, err := SelectAndClaimTask(dir, "agent-rb1", nil)
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
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "stuck.md")); err != nil {
		t.Fatalf("stuck.md should remain in in-progress after double failure: %v", err)
	}
}

func TestSelectAndClaimTask_RollbackFailure_SkipsFurtherCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "first.md"), "# First\nDo first.\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "second.md"), "# Second\nDo second.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "first.md\nsecond.md\n")

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

	task, err := SelectAndClaimTask(dir, "agent-rb2", nil)
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
	if _, err := os.Stat(filepath.Join(dir, "backlog", "second.md")); err != nil {
		t.Fatalf("second.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_PrependFails_RollbackSucceeds_ContinuesToNext(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "broken.md"), "# Broken\nDo broken.\n")
	writeTestFile(t, filepath.Join(dir, "backlog", "healthy.md"), "# Healthy\nDo healthy.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "broken.md\nhealthy.md\n")

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

	task, err := SelectAndClaimTask(dir, "agent-rb3", nil)
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
	if _, err := os.Stat(filepath.Join(dir, "backlog", "broken.md")); err != nil {
		t.Fatalf("broken.md should be back in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted_MoveToFailedFails_RollbackToBacklog(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "exhausted.md"), strings.Join([]string{
		"# Exhausted",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, "backlog", "healthy.md"), "# Healthy\nDo healthy.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "exhausted.md\nhealthy.md\n")

	origMove := retryExhaustedMoveFn
	t.Cleanup(func() { retryExhaustedMoveFn = origMove })

	retryExhaustedMoveFn = func(src, dst string) error {
		return fmt.Errorf("simulated move-to-failed failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-re1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task == nil {
		t.Fatal("expected healthy.md to be claimed, got nil")
	}
	if task.Filename != "healthy.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "healthy.md")
	}

	// exhausted.md should be back in backlog (rollback succeeded)
	if _, err := os.Stat(filepath.Join(dir, "backlog", "exhausted.md")); err != nil {
		t.Fatalf("exhausted.md should be back in backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "exhausted.md")); !os.IsNotExist(err) {
		t.Fatal("exhausted.md should NOT be in in-progress")
	}
}

func TestSelectAndClaimTask_RetryExhausted_DoubleFailure_ReturnsError(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "stuck.md"), strings.Join([]string{
		"# Stuck",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, ".queue"), "stuck.md\n")

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

	task, err := SelectAndClaimTask(dir, "agent-re2", nil)
	if err == nil {
		t.Fatal("expected error when both move-to-failed and rollback fail, got nil")
	}
	if task != nil {
		t.Fatalf("expected nil task on double failure, got %+v", task)
	}
	if !strings.Contains(err.Error(), "retry-exhausted rollback failed") {
		t.Fatalf("error should mention retry-exhausted rollback failure: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated move-to-failed failure") {
		t.Fatalf("error should include move-to-failed cause: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated rollback failure") {
		t.Fatalf("error should include rollback cause: %v", err)
	}

	// Task should be stranded in in-progress (both moves failed)
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "stuck.md")); err != nil {
		t.Fatalf("stuck.md should remain in in-progress after double failure: %v", err)
	}
}

func TestSelectAndClaimTask_RetryExhausted_DoubleFailure_SkipsFurtherCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)
	writeTestFile(t, filepath.Join(dir, "backlog", "exhausted.md"), strings.Join([]string{
		"# Exhausted",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	writeTestFile(t, filepath.Join(dir, "backlog", "second.md"), "# Second\nDo second.\n")
	writeTestFile(t, filepath.Join(dir, ".queue"), "exhausted.md\nsecond.md\n")

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

	task, err := SelectAndClaimTask(dir, "agent-re3", nil)
	if err == nil {
		t.Fatal("expected error on double failure")
	}
	if task != nil {
		t.Fatalf("expected nil task, got %+v", task)
	}

	// second.md should still be in backlog (not attempted after hard error)
	if _, err := os.Stat(filepath.Join(dir, "backlog", "second.md")); err != nil {
		t.Fatalf("second.md should still be in backlog: %v", err)
	}
}

func TestRecoverOrphanedTasks_HandlesStrandedWithoutClaimedBy(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Simulate a task stranded in in-progress without a claimed-by marker
	// (the scenario that occurs after a double failure).
	writeTestFile(t, filepath.Join(dir, "in-progress", "orphan.md"), "# Orphan\nDo orphan.\n")

	RecoverOrphanedTasks(dir)

	// Task should be recovered to backlog
	if _, err := os.Stat(filepath.Join(dir, "backlog", "orphan.md")); err != nil {
		t.Fatalf("orphan.md should be recovered to backlog: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "in-progress", "orphan.md")); !os.IsNotExist(err) {
		t.Fatal("orphan.md should no longer be in in-progress")
	}

	// Verify a failure record was appended
	data, err := os.ReadFile(filepath.Join(dir, "backlog", "orphan.md"))
	if err != nil {
		t.Fatalf("read recovered task: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure:") {
		t.Fatal("recovered task should have a failure record appended")
	}
}
