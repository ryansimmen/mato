package queue

import (
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
