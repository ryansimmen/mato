package queue

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mato/internal/dirs"
	"mato/internal/taskfile"
)

func withCreateRetryTempFileFn(t *testing.T, fn func(string, string) (*os.File, error)) {
	t.Helper()
	orig := createRetryTempFileFn
	createRetryTempFileFn = fn
	t.Cleanup(func() { createRetryTempFileFn = orig })
}

func TestRetryTask_Success(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, dirs.Failed)
	backlogDir := filepath.Join(tmp, dirs.Backlog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/fix-login -->
---
id: fix-login
priority: 10
---
# Fix login bug

Some instructions here.

<!-- failure: abc123 at 2026-01-01T00:00:00Z step=WORK error=build failed -->

<!-- review-failure: def456 at 2026-01-02T00:00:00Z step=REVIEW error=network_timeout -->

<!-- cycle-failure: mato at 2026-01-03T00:00:00Z — circular dependency -->

<!-- review-rejection: 55aff11d at 2026-01-04T00:00:00Z — code review feedback -->

<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable frontmatter -->
`
	if err := os.WriteFile(filepath.Join(failedDir, "fix-login.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RetryTask(tmp, "fix-login"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	// Task should be in backlog.
	backlogPath := filepath.Join(backlogDir, "fix-login.md")
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task not found in backlog: %v", err)
	}

	// Failure markers should be stripped.
	result := string(data)
	for _, marker := range []string{
		"<!-- failure:",
		"<!-- review-failure:",
		"<!-- cycle-failure:",
		"<!-- terminal-failure:",
	} {
		if strings.Contains(result, marker) {
			t.Errorf("cleaned content still contains %q", marker)
		}
	}

	// Non-failure content should be preserved.
	if !strings.Contains(result, "<!-- claimed-by:") {
		t.Error("claimed-by comment was stripped but should be preserved")
	}
	if !strings.Contains(result, "<!-- branch:") {
		t.Error("branch comment was stripped but should be preserved")
	}
	if !strings.Contains(result, "# Fix login bug") {
		t.Error("task title was stripped")
	}
	if !strings.Contains(result, "Some instructions here.") {
		t.Error("task body was stripped")
	}
	if !strings.Contains(result, "<!-- review-rejection:") {
		t.Error("review rejection feedback was stripped but should be preserved")
	}
	if !strings.Contains(result, "id: fix-login") {
		t.Error("frontmatter was stripped")
	}

	// Source should be removed from failed/.
	if _, err := os.Stat(filepath.Join(failedDir, "fix-login.md")); !os.IsNotExist(err) {
		t.Error("failed/ copy should be removed after successful retry")
	}
}

func TestRetryTask_NotInFailed(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, dirs.Failed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, dirs.Backlog), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := RetryTask(tmp, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if !strings.Contains(err.Error(), "not found in failed/") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRetryTask_DestinationCollision(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, dirs.Failed)
	backlogDir := filepath.Join(tmp, dirs.Backlog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	originalContent := "# Task\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "task.md"), []byte(originalContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-existing file in backlog.
	if err := os.WriteFile(filepath.Join(backlogDir, "task.md"), []byte("# Existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := RetryTask(tmp, "task")
	if err == nil {
		t.Fatal("expected error for destination collision")
	}
	if !strings.Contains(err.Error(), "already exists in backlog/") {
		t.Errorf("unexpected error message: %v", err)
	}

	// The original failed/ file must be unchanged (data-loss safety).
	data, readErr := os.ReadFile(filepath.Join(failedDir, "task.md"))
	if readErr != nil {
		t.Fatalf("failed/ file should still exist after collision: %v", readErr)
	}
	if string(data) != originalContent {
		t.Errorf("failed/ file was mutated during collision\ngot:  %q\nwant: %q", string(data), originalContent)
	}
}

func TestRetryTask_ConcurrentReservationAllowsSingleWinner(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range []string{dirs.Failed, dirs.Backlog} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	content := "---\nid: task\n---\n# Task\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, "task.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, retryErr := RetryTask(tmp, "task")
			results <- retryErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var successCount, collisionCount int
	for err := range results {
		if err == nil {
			successCount++
			continue
		}
		if strings.Contains(err.Error(), "already exists in backlog/") || strings.Contains(err.Error(), "not found in failed/") {
			collisionCount++
			continue
		}
		t.Fatalf("unexpected RetryTask error: %v", err)
	}
	if successCount != 1 {
		t.Fatalf("successCount = %d, want 1", successCount)
	}
	if collisionCount != 1 {
		t.Fatalf("collisionCount = %d, want 1", collisionCount)
	}
}

func TestRetryTask_TempFileFailureLeavesNoPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range []string{dirs.Failed, dirs.Backlog} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, "task.md"), []byte("# Task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backlogPath := filepath.Join(tmp, dirs.Backlog, "task.md")
	withCreateRetryTempFileFn(t, func(dir, pattern string) (*os.File, error) {
		return nil, io.ErrUnexpectedEOF
	})

	_, err := RetryTask(tmp, "task")
	if err == nil || !strings.Contains(err.Error(), "create temp file in backlog") {
		t.Fatalf("err = %v, want create temp file in backlog error", err)
	}
	if _, err := os.Stat(backlogPath); !os.IsNotExist(err) {
		t.Fatalf("backlog file should not exist after temp file creation failure, stat err = %v", err)
	}
}

func TestRetryTask_AppendsMdExtension(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, dirs.Failed)
	backlogDir := filepath.Join(tmp, dirs.Backlog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(failedDir, "my-task.md"), []byte("# My Task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Call with stem only (no .md extension).
	if _, err := RetryTask(tmp, "my-task"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backlogDir, "my-task.md")); err != nil {
		t.Fatalf("task not found in backlog: %v", err)
	}
}

func TestRetryTask_ExplicitIDResolution(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, dirs.Failed)
	backlogDir := filepath.Join(tmp, dirs.Backlog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(failedDir, "filename.md"), []byte("---\nid: explicit-id\n---\n# Task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RetryTask(tmp, "explicit-id"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backlogDir, "filename.md")); err != nil {
		t.Fatalf("task not found in backlog after explicit id retry: %v", err)
	}
}

func TestRetryTask_ExplicitIDWithSlash(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, dirs.Failed)
	backlogDir := filepath.Join(tmp, dirs.Backlog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(failedDir, "filename.md"), []byte("---\nid: group/explicit-id\n---\n# Task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RetryTask(tmp, "group/explicit-id"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backlogDir, "filename.md")); err != nil {
		t.Fatalf("task not found in backlog after slash id retry: %v", err)
	}
}

func TestRetryTask_RejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, dirs.Failed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, dirs.Backlog), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := RetryTask(tmp, "../../README")
	if err == nil {
		t.Fatal("expected error for path traversal name")
	}
	if !strings.Contains(err.Error(), "path traversal is not allowed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRetryTask_DependencyBlockedWarnsAndReconcileMovesToWaiting(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	content := "---\nid: blocked\ndepends_on: [missing-dep]\n---\n# Blocked\n"
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, "blocked.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RetryTask(tmp, "blocked")
	if err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}
	if !result.DependencyBlocked {
		t.Fatal("DependencyBlocked = false, want true")
	}
	if len(result.Warnings) == 0 {
		t.Fatal("expected at least one warning about dependency block")
	}
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "next reconcile will move it to waiting/") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Warnings = %v, want warning about waiting/ demotion", result.Warnings)
	}

	if got := ReconcileReadyQueue(tmp, nil); !got {
		t.Fatal("ReconcileReadyQueue() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tmp, dirs.Waiting, "blocked.md")); err != nil {
		t.Fatalf("blocked.md not found in waiting/: %v", err)
	}
}

func TestStripFailureMarkers(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		notWant []string
	}{
		{
			name: "strips all marker types",
			input: `<!-- branch: task/foo -->
# Title

Body text.

<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->
<!-- review-failure: def at 2026-01-02T00:00:00Z step=REVIEW error=timeout -->
<!-- cycle-failure: mato at 2026-01-03T00:00:00Z — circular dependency -->
<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->
<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable -->
`,
			want: "<!-- branch: task/foo -->",
			notWant: []string{
				"<!-- failure:",
				"<!-- review-failure:",
				"<!-- cycle-failure:",
				"<!-- terminal-failure:",
			},
		},
		{
			name:  "no markers to strip",
			input: "# Title\n\nBody.\n",
			want:  "# Title",
		},
		{
			name: "preserves non-failure comments",
			input: `<!-- claimed-by: abc -->
<!-- branch: task/foo -->
# Title
<!-- failure: x at 2026-01-01T00:00:00Z step=WORK error=e -->
`,
			want:    "<!-- claimed-by: abc -->",
			notWant: []string{"<!-- failure:"},
		},
		{
			name: "preserves review rejection feedback",
			input: `# Title

<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->

<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable -->
`,
			want:    "<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->",
			notWant: []string{"<!-- terminal-failure:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripFailureMarkers(tt.input)
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected output to contain %q, got:\n%s", tt.want, got)
			}
			for _, bad := range tt.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("output should not contain %q, got:\n%s", bad, got)
				}
			}
		})
	}
}

func TestStripFailureMarkers_IgnoresUnterminatedDuringRetry(t *testing.T) {
	// Unterminated or trailing-text failure markers must not be stripped during
	// retry, since they are not valid standalone markers. This protects user
	// content that happens to resemble a marker prefix.
	tests := []struct {
		name    string
		input   string
		want    string
		notWant []string
	}{
		{
			name: "unterminated failure kept during retry strip",
			input: `# Title

Body text.

<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops
<!-- failure: def at 2026-01-02T00:00:00Z step=WORK error=real -->
`,
			want:    "<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops",
			notWant: []string{"<!-- failure: def at 2026-01-02T00:00:00Z step=WORK error=real -->"},
		},
		{
			name: "trailing text failure kept during retry strip",
			input: `# Title

<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops --> trailing
<!-- failure: def at 2026-01-02T00:00:00Z step=WORK error=real -->
`,
			want:    "<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops --> trailing",
			notWant: []string{"<!-- failure: def at 2026-01-02T00:00:00Z step=WORK error=real -->"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripFailureMarkers(tt.input)
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected output to contain %q, got:\n%s", tt.want, got)
			}
			for _, bad := range tt.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("output should not contain %q, got:\n%s", bad, got)
				}
			}
		})
	}
}

func TestRetryTask_CleansRuntimeState(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range []string{dirs.Failed, dirs.Backlog} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	content := "---\nid: cleanup-task\npriority: 10\n---\n# Cleanup task\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, "cleanup-task.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create taskstate and sessionmeta entries to simulate stale runtime state.
	taskstateDir := filepath.Join(tmp, "runtime", "taskstate")
	sessionmetaDir := filepath.Join(tmp, "runtime", "sessionmeta")
	if err := os.MkdirAll(taskstateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessionmetaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskstateDir, "cleanup-task.md.json"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionmetaDir, "work-cleanup-task.md.json"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionmetaDir, "review-cleanup-task.md.json"), []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RetryTask(tmp, "cleanup-task"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	// Verify the task was moved to backlog.
	if _, err := os.Stat(filepath.Join(tmp, dirs.Backlog, "cleanup-task.md")); err != nil {
		t.Fatalf("task not found in backlog: %v", err)
	}

	// Verify taskstate was cleaned up.
	if _, err := os.Stat(filepath.Join(taskstateDir, "cleanup-task.md.json")); !os.IsNotExist(err) {
		t.Errorf("taskstate file should be removed after retry, stat err = %v", err)
	}

	// Verify sessionmeta (work and review) was cleaned up.
	if _, err := os.Stat(filepath.Join(sessionmetaDir, "work-cleanup-task.md.json")); !os.IsNotExist(err) {
		t.Errorf("work sessionmeta file should be removed after retry, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionmetaDir, "review-cleanup-task.md.json")); !os.IsNotExist(err) {
		t.Errorf("review sessionmeta file should be removed after retry, stat err = %v", err)
	}
}

func TestRetryTask_NoEmptyPlaceholderDuringWrite(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range []string{dirs.Failed, dirs.Backlog} {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	content := "---\nid: race-check\npriority: 10\n---\n# Race check task\n\nBody.\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, "race-check.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	backlogPath := filepath.Join(tmp, dirs.Backlog, "race-check.md")

	// Wrap the real createRetryTempFileFn to intercept the moment just
	// before the temp file is created, and verify that no empty file
	// exists at the final backlog path at that point.
	origCreate := createRetryTempFileFn
	createRetryTempFileFn = func(dir, pattern string) (*os.File, error) {
		// At this point, the backlog path must not exist yet.
		if _, err := os.Stat(backlogPath); !os.IsNotExist(err) {
			t.Errorf("backlog file visible before temp file creation; stat err = %v", err)
		}
		return origCreate(dir, pattern)
	}
	t.Cleanup(func() { createRetryTempFileFn = origCreate })

	if _, err := RetryTask(tmp, "race-check"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	// After retry succeeds, verify the file at the backlog path has content.
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("backlog file not found: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("backlog file is empty after retry")
	}
	if !strings.Contains(string(data), "# Race check task") {
		t.Errorf("backlog file missing task title; got:\n%s", string(data))
	}
	if strings.Contains(string(data), "<!-- failure:") {
		t.Errorf("backlog file still contains failure marker; got:\n%s", string(data))
	}
}

func TestRetryTask_PreservesVerdictForNextWorkAgent(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	filename := "stale-verdict-retry.md"
	content := "---\nid: stale-verdict-retry\npriority: 10\n---\n# Stale verdict retry\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a verdict file with a reject reason.
	msgDir := filepath.Join(tmp, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := map[string]string{"verdict": "reject", "reason": "stale from prior cycle"}
	vdata, _ := json.Marshal(verdict)
	verdictPath := taskfile.VerdictPath(tmp, filename)
	if err := os.WriteFile(verdictPath, vdata, 0o644); err != nil {
		t.Fatal(err)
	}

	// Before retry: BuildIndex should surface the verdict rejection reason.
	idx := BuildIndex(tmp)
	snap := idx.Snapshot(dirs.Failed, filename)
	if snap == nil {
		t.Fatal("expected snapshot in failed/ before retry")
	}
	if snap.LastReviewRejectionReason != "stale from prior cycle" {
		t.Fatalf("before retry: LastReviewRejectionReason = %q, want %q",
			snap.LastReviewRejectionReason, "stale from prior cycle")
	}

	// Perform retry.
	if _, err := RetryTask(tmp, "stale-verdict-retry"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	// Verdict file must survive so the next work agent gets MATO_REVIEW_FEEDBACK.
	if _, err := os.Stat(verdictPath); err != nil {
		t.Fatalf("verdict file should be preserved after retry, stat err = %v", err)
	}

	// After retry: BuildIndex should still surface the rejection reason.
	idx2 := BuildIndex(tmp)
	snap2 := idx2.Snapshot(dirs.Backlog, filename)
	if snap2 == nil {
		t.Fatal("expected snapshot in backlog/ after retry")
	}
	if snap2.LastReviewRejectionReason != "stale from prior cycle" {
		t.Fatalf("after retry: LastReviewRejectionReason = %q, want %q",
			snap2.LastReviewRejectionReason, "stale from prior cycle")
	}
}

func TestRetryTask_VerdictFallbackRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tmp, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	filename := "verdict-roundtrip.md"
	// Task in failed/ with NO durable review-rejection markers — only a verdict file.
	content := "---\nid: verdict-roundtrip\npriority: 10\n---\n# Verdict roundtrip\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tmp, dirs.Failed, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed a verdict file as the sole source of rejection feedback.
	msgDir := filepath.Join(tmp, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := map[string]string{"verdict": "reject", "reason": "tests do not cover edge case X"}
	vdata, _ := json.Marshal(verdict)
	verdictPath := taskfile.VerdictPath(tmp, filename)
	if err := os.WriteFile(verdictPath, vdata, 0o644); err != nil {
		t.Fatal(err)
	}

	// Retry the task: moves from failed/ → backlog/.
	if _, err := RetryTask(tmp, "verdict-roundtrip"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	// Verify the verdict file survived.
	vr, ok := taskfile.ReadVerdictRejection(tmp, filename)
	if !ok {
		t.Fatal("ReadVerdictRejection returned false after retry; verdict file should be preserved")
	}
	if vr.Reason != "tests do not cover edge case X" {
		t.Fatalf("ReadVerdictRejection reason = %q, want %q", vr.Reason, "tests do not cover edge case X")
	}

	// Verify the task is in backlog/ and the backlog snapshot surfaces the rejection.
	idx := BuildIndex(tmp)
	snap := idx.Snapshot(dirs.Backlog, filename)
	if snap == nil {
		t.Fatal("expected snapshot in backlog/ after retry")
	}
	if snap.LastReviewRejectionReason != "tests do not cover edge case X" {
		t.Fatalf("backlog snapshot LastReviewRejectionReason = %q, want %q",
			snap.LastReviewRejectionReason, "tests do not cover edge case X")
	}

	// Verify the verdict path still exists on disk.
	if _, err := os.Stat(verdictPath); err != nil {
		t.Fatalf("verdict file should exist after retry, stat err = %v", err)
	}
}
