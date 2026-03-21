package queue

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/testutil"
)

func setupClaimTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range append(AllDirs, ".locks") {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSelectAndClaimTask_Normal(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "alpha.md"), "# Alpha\nDo alpha.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "beta.md"), "# Beta\nDo beta.\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "alpha.md\nbeta.md\n")

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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "retry.md\n")

	task, err := SelectAndClaimTask(dir, "agent-2", nil)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "bad.md\ngood.md\n")

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
	if _, err := os.Stat(filepath.Join(dir, DirFailed, "bad.md")); err != nil {
		t.Fatalf("bad.md not in failed: %v", err)
	}
}

func TestSelectAndClaimTask_AllClaimed(t *testing.T) {
	dir := setupClaimTestDir(t)
	// backlog is empty
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "missing.md\n")

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
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "high.md"), "# High\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "low.md"), "# Low\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "high.md\nlow.md\n")

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
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "high.md")); err != nil {
		t.Fatalf("high.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_QueueFileOrdering(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "z-last.md"), "# Z Last\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "a-first.md"), "# A First\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "z-last.md\na-first.md\n")

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
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "z-last.md"), "# Z Last\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "a-first.md"), "# A First\n")

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
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "custom.md"), strings.Join([]string{
		"---",
		"max_retries: 1",
		"---",
		"# Custom retries",
		"<!-- failure: one -->",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "custom.md\n")

	task, err := SelectAndClaimTask(dir, "agent-9", nil)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "broken.md\nfallback.md\n")

	// Make the first file unreadable so prependClaimedBy fails on os.ReadFile.
	// os.Rename only needs directory permissions, so the rename to in-progress
	// and back to backlog will still succeed.
	if err := os.Chmod(filepath.Join(dir, DirBacklog, "broken.md"), 0o000); err != nil {
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "only.md\n")

	// Make the file unreadable so prependClaimedBy fails.
	if err := os.Chmod(filepath.Join(dir, DirBacklog, "only.md"), 0o000); err != nil {
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "zero-retry.md\n")

	task, err := SelectAndClaimTask(dir, "agent-10", nil)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "stuck.md\n")

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
	if _, err := os.Stat(filepath.Join(dir, DirInProgress, "stuck.md")); err != nil {
		t.Fatalf("stuck.md should remain in in-progress after double failure: %v", err)
	}
}

func TestSelectAndClaimTask_RollbackFailure_SkipsFurtherCandidates(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "first.md"), "# First\nDo first.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "second.md"), "# Second\nDo second.\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "first.md\nsecond.md\n")

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
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "second.md")); err != nil {
		t.Fatalf("second.md should still be in backlog: %v", err)
	}
}

func TestSelectAndClaimTask_PrependFails_RollbackSucceeds_ContinuesToNext(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "broken.md"), "# Broken\nDo broken.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "healthy.md"), "# Healthy\nDo healthy.\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "broken.md\nhealthy.md\n")

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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "exhausted.md\nhealthy.md\n")

	origMove := retryExhaustedMoveFn
	t.Cleanup(func() { retryExhaustedMoveFn = origMove })

	retryExhaustedMoveFn = func(src, dst string) error {
		return fmt.Errorf("simulated move-to-failed failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-re1", nil)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "stuck.md\n")

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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "exhausted.md\nsecond.md\n")

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
	if _, err := os.Stat(filepath.Join(dir, DirBacklog, "second.md")); err != nil {
		t.Fatalf("second.md should still be in backlog: %v", err)
	}
}

func TestRecoverOrphanedTasks_HandlesStrandedWithoutClaimedBy(t *testing.T) {
	dir := setupClaimTestDir(t)

	// Simulate a task stranded in in-progress without a claimed-by marker
	// (the scenario that occurs after a double failure).
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "orphan.md"), "# Orphan\nDo orphan.\n")

	RecoverOrphanedTasks(dir)

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

func TestSelectAndClaimTask_InvalidYAML_WarnsAndUsesDefaults(t *testing.T) {
	dir := setupClaimTestDir(t)
	// Task with invalid YAML frontmatter
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "bad-yaml.md"), strings.Join([]string{
		"---",
		"priority: [invalid",
		"---",
		"# Bad YAML task",
		"Do something.",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "bad-yaml.md\n")

	// Capture stderr to verify warning is printed.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	task, claimErr := SelectAndClaimTask(dir, "agent-warn", nil)

	w.Close()
	captured, readErr := io.ReadAll(r)
	os.Stderr = origStderr
	if readErr != nil {
		t.Fatal(readErr)
	}

	if claimErr != nil {
		t.Fatalf("SelectAndClaimTask: %v", claimErr)
	}
	if task == nil {
		t.Fatal("expected a claimed task, got nil")
	}

	// Default maxRetries (3) should be used, so the task should be claimed
	// since there are 0 failures.
	if task.Filename != "bad-yaml.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "bad-yaml.md")
	}

	// Verify the warning was printed to stderr.
	stderrOutput := string(captured)
	if !strings.Contains(stderrOutput, "warning: could not parse task metadata for bad-yaml.md") {
		t.Fatalf("expected parse-error warning on stderr, got: %q", stderrOutput)
	}
	if !strings.Contains(stderrOutput, "using defaults") {
		t.Fatalf("expected 'using defaults' in warning, got: %q", stderrOutput)
	}
}

func TestSelectAndClaimTask_InvalidYAML_ExhaustedRetries_UsesDefault(t *testing.T) {
	dir := setupClaimTestDir(t)
	// Task with invalid YAML and 3 failures (matching default max_retries=3).
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "bad-exhausted.md\n")

	// Suppress stderr warning output during test.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	task, claimErr := SelectAndClaimTask(dir, "agent-exhaust", nil)

	w.Close()
	r.Close()
	os.Stderr = origStderr

	if claimErr != nil {
		t.Fatalf("SelectAndClaimTask: %v", claimErr)
	}
	// With default maxRetries=3 and 3 failures, the task should be exhausted
	// and moved to failed/.
	if task != nil {
		t.Fatalf("expected nil (default max_retries=3 exhausted), got %+v", task)
	}
	if _, err := os.Stat(filepath.Join(dir, DirFailed, "bad-exhausted.md")); err != nil {
		t.Fatalf("bad-exhausted.md not in failed: %v", err)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "unreadable.md\nreadable.md\n")

	// Capture stderr to verify warning.
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	task, err := SelectAndClaimTask(dir, "agent-x", nil)

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
	if !strings.Contains(stderrStr, "could not count failures") {
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "add feature.md\n")

	task, err := SelectAndClaimTask(dir, "agent-coll1", nil)
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

func TestSelectAndClaimTask_NoBranchCollision_NormalBranch(t *testing.T) {
	dir := setupClaimTestDir(t)

	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "unique-task.md"), "# Unique Task\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "unique-task.md\n")

	task, err := SelectAndClaimTask(dir, "agent-nocoll", nil)
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

	active := CollectActiveBranches(dir)

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

func TestWriteBranchComment(t *testing.T) {
	dir := t.TempDir()
	taskPath := filepath.Join(dir, "task.md")
	original := "<!-- claimed-by: agent-1 -->\n# My Task\nDo it.\n"
	testutil.WriteFile(t, taskPath, original)

	if err := writeBranchComment(taskPath, "task/my-task"); err != nil {
		t.Fatalf("writeBranchComment: %v", err)
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

func TestSelectAndClaimTask_DestinationExistsInProgress(t *testing.T) {
	dir := setupClaimTestDir(t)
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "dup.md"), "# Dup\nDo dup.\n")
	// Pre-existing file in in-progress/ with same name.
	testutil.WriteFile(t, filepath.Join(dir, DirInProgress, "dup.md"),
		"<!-- claimed-by: other -->\n# Dup\nAlready claimed.\n")
	testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "ok.md"), "# OK\nDo ok.\n")
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "dup.md\nok.md\n")

	task, err := SelectAndClaimTask(dir, "agent-dup", nil)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "old.md\n")

	task, err := SelectAndClaimTask(dir, "agent-fd", nil)
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
	testutil.WriteFile(t, filepath.Join(dir, ".queue"), "race.md\n")

	origPrepend := claimPrependFn
	t.Cleanup(func() { claimPrependFn = origPrepend })

	// Force prepend to fail, then sneak a file back into backlog to
	// simulate a concurrent race so the rollback hits EEXIST.
	claimPrependFn = func(path, agentID, claimedAt string) error {
		testutil.WriteFile(t, filepath.Join(dir, DirBacklog, "race.md"), "# Race (reappeared)\n")
		return fmt.Errorf("simulated prepend failure")
	}

	task, err := SelectAndClaimTask(dir, "agent-race", nil)
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
