package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/queue"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
	"mato/internal/testutil"
	"mato/internal/ui"
)

func TestReviewVerdict_JSONRoundtrip(t *testing.T) {
	tests := []struct {
		name    string
		verdict reviewVerdict
	}{
		{
			name:    "approve",
			verdict: reviewVerdict{Verdict: "approve", Reason: ""},
		},
		{
			name:    "reject with reason",
			verdict: reviewVerdict{Verdict: "reject", Reason: "missing error handling"},
		},
		{
			name:    "error with reason",
			verdict: reviewVerdict{Verdict: "error", Reason: "agent crashed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.verdict)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got reviewVerdict
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Verdict != tt.verdict.Verdict {
				t.Fatalf("verdict mismatch: got %q, want %q", got.Verdict, tt.verdict.Verdict)
			}
			if got.Reason != tt.verdict.Reason {
				t.Fatalf("reason mismatch: got %q, want %q", got.Reason, tt.verdict.Reason)
			}
		})
	}
}

func TestReviewVerdict_JSONTags(t *testing.T) {
	v := reviewVerdict{Verdict: "approve", Reason: "looks good"}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"verdict"`) {
		t.Fatalf("expected JSON key 'verdict', got %s", s)
	}
	if !strings.Contains(s, `"reason"`) {
		t.Fatalf("expected JSON key 'reason', got %s", s)
	}
}

func TestPostReviewAction_TaskAlreadyMoved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task file does NOT exist (already moved by another process).
	task := &queue.ClaimedTask{
		Filename: "gone-task.md",
		Branch:   "task/gone-task",
		Title:    "Gone Task",
		TaskPath: filepath.Join(tasksDir, dirs.ReadyReview, "gone-task.md"),
	}

	// Should return without error or panic.
	postReviewAction(tasksDir, "host-agent", task)

	// No files should have been created in ready-to-merge/ or backlog/.
	files, _ := os.ReadDir(filepath.Join(tasksDir, dirs.ReadyMerge))
	if len(files) > 0 {
		t.Fatal("no files should appear in ready-to-merge/ for already-moved task")
	}
}

func TestPostReviewAction_TaskAlreadyMovedLogsWarning(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task file does NOT exist (already moved by another process).
	task := &queue.ClaimedTask{
		Filename: "moved-task.md",
		Branch:   "task/moved-task",
		Title:    "Moved Task",
		TaskPath: filepath.Join(tasksDir, dirs.ReadyReview, "moved-task.md"),
	}

	// Capture stderr to verify the warning is emitted.
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)

	postReviewAction(tasksDir, "host-agent", task)

	ui.SetWarningWriter(prevWarn)
	w.Close()
	os.Stderr = oldStderr

	var buf []byte
	buf, err = io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	output := string(buf)

	if !strings.Contains(output, "warning: review verdict for moved-task.md discarded") {
		t.Fatalf("expected warning about discarded verdict, got: %q", output)
	}
	if !strings.Contains(output, "task file moved") {
		t.Fatalf("expected 'task file moved' in warning, got: %q", output)
	}
}

func TestPostReviewAction_TaskStatErrorDoesNotClaimMoved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	reviewDir := filepath.Join(tasksDir, dirs.ReadyReview)
	blockedPath := filepath.Join(reviewDir, "blocked.md")
	if err := os.RemoveAll(reviewDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	task := &queue.ClaimedTask{
		Filename: "blocked.md",
		Branch:   "task/blocked",
		Title:    "Blocked",
		TaskPath: blockedPath,
	}

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)

	postReviewAction(tasksDir, "host-agent", task)

	ui.SetWarningWriter(prevWarn)
	w.Close()
	os.Stderr = oldStderr

	buf, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	output := string(buf)

	if !strings.Contains(output, "warning: could not verify review task file blocked.md") {
		t.Fatalf("expected verification warning, got: %q", output)
	}
	if strings.Contains(output, "task file moved") {
		t.Fatalf("unexpected moved-task warning for stat error: %q", output)
	}
}

func TestPostReviewAction_ApprovedCaseInsensitive(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "case-task.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Case Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"APPROVE"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/case-task",
		Title:    "Case Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyMerge, taskFile)); err != nil {
		t.Fatal("APPROVE (uppercase) should be treated as approval")
	}
}

func TestPostReviewAction_RejectedEmptyReason(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "empty-reason.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Empty Reason\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":""}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/empty-reason",
		Title:    "Empty Reason",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, taskFile)); err != nil {
		t.Fatal("rejected task should be moved to backlog/")
	}
	data, _ := os.ReadFile(filepath.Join(tasksDir, dirs.Backlog, taskFile))
	if !strings.Contains(string(data), "no reason provided") {
		t.Fatalf("empty reason should be replaced with 'no reason provided', got:\n%s", string(data))
	}
}

func TestPostReviewAction_MissingVerdictAlwaysRecordsReviewFailure(t *testing.T) {
	tests := []struct {
		name            string
		taskFile        string
		content         string
		wantPreserved   string
		unwantedDstPath string
	}{
		{
			name:            "without legacy markers",
			taskFile:        "missing-verdict.md",
			content:         "# Missing Verdict\n",
			unwantedDstPath: dirs.ReadyMerge,
		},
		{
			name:            "with legacy approval marker",
			taskFile:        "legacy-approval.md",
			content:         "# Legacy Approval\n<!-- reviewed: review-agent at 2026-01-01T00:00:00Z — approved -->\n",
			wantPreserved:   "<!-- reviewed:",
			unwantedDstPath: dirs.ReadyMerge,
		},
		{
			name:            "with legacy rejection marker",
			taskFile:        "legacy-rejection.md",
			content:         "# Legacy Rejection\n<!-- review-rejection: review-agent at 2026-01-01T00:00:00Z — tests missing -->\n",
			wantPreserved:   "<!-- review-rejection:",
			unwantedDstPath: dirs.Backlog,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
				os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
			}

			reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, tt.taskFile)
			os.WriteFile(reviewPath, []byte(tt.content), 0o644)

			task := &queue.ClaimedTask{
				Filename: tt.taskFile,
				Branch:   "task/" + strings.TrimSuffix(tt.taskFile, ".md"),
				Title:    "Missing Verdict",
				TaskPath: reviewPath,
			}

			postReviewAction(tasksDir, "host-agent", task)

			if _, err := os.Stat(reviewPath); err != nil {
				t.Fatalf("task should stay in ready-for-review/: %v", err)
			}
			if _, err := os.Stat(filepath.Join(tasksDir, tt.unwantedDstPath, tt.taskFile)); !os.IsNotExist(err) {
				t.Fatalf("task should not move to %s when the verdict file is missing", tt.unwantedDstPath)
			}
			data, err := os.ReadFile(reviewPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			content := string(data)
			if !strings.Contains(content, "<!-- review-failure:") {
				t.Fatalf("review-failure should be recorded when verdict is missing:\n%s", content)
			}
			if tt.wantPreserved != "" && !strings.Contains(content, tt.wantPreserved) {
				t.Fatalf("legacy marker %q should be preserved:\n%s", tt.wantPreserved, content)
			}
		})
	}
}

func TestPostReviewAction_ErrorVerdictEmptyReason(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "error-empty.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Error Empty\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"error","reason":""}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/error-empty",
		Title:    "Error Empty",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("error verdict task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "review agent reported an error") {
		t.Fatalf("empty error reason should use default message, got:\n%s", string(data))
	}
}

func TestLogReviewFailureOutcome_AllCombinations(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		filename   string
		recorded   bool
		detail     string
		wantStdout string
	}{
		{
			name:       "recorded with detail",
			prefix:     "Review error",
			filename:   "task.md",
			recorded:   true,
			detail:     "branch missing",
			wantStdout: "Review error: recorded review-failure for task.md: branch missing",
		},
		{
			name:       "not recorded with detail",
			prefix:     "Review incomplete",
			filename:   "task.md",
			recorded:   false,
			detail:     "parse error",
			wantStdout: "could not record review-failure for task.md: parse error",
		},
		{
			name:       "recorded without detail",
			prefix:     "Review incomplete",
			filename:   "task.md",
			recorded:   true,
			detail:     "",
			wantStdout: "recorded review-failure for task.md",
		},
		{
			name:       "not recorded without detail",
			prefix:     "Review incomplete",
			filename:   "task.md",
			recorded:   false,
			detail:     "",
			wantStdout: "could not record review-failure for task.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _ := captureStdoutStderr(t, func() {
				logReviewFailureOutcome(tt.prefix, tt.filename, tt.recorded, tt.detail)
			})
			if !strings.Contains(stdout, tt.wantStdout) {
				t.Fatalf("expected stdout to contain %q, got:\n%s", tt.wantStdout, stdout)
			}
		})
	}
}

func TestReviewCandidates_EmptyReviewDir(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, dirs.Failed), 0o755)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(candidates))
	}
}

func TestReviewCandidates_FilesystemFallback_SingleTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskContent := "<!-- branch: task/test-task -->\n---\npriority: 10\nmax_retries: 3\n---\n# Test Task\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "test-task.md"), []byte(taskContent), 0o644)

	// Pass nil index to use filesystem fallback.
	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Filename != "test-task.md" {
		t.Fatalf("expected test-task.md, got %q", candidates[0].Filename)
	}
}

func TestReviewCandidates_FilesystemFallback_PrioritySort(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "low-pri.md"),
		[]byte("<!-- branch: task/low-pri -->\n---\npriority: 50\nmax_retries: 3\n---\n# Low Priority\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "high-pri.md"),
		[]byte("<!-- branch: task/high-pri -->\n---\npriority: 10\nmax_retries: 3\n---\n# High Priority\n"), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Filename != "high-pri.md" {
		t.Fatalf("expected high-pri.md first (lower priority number), got %q", candidates[0].Filename)
	}
}

func TestReviewCandidates_FilesystemFallback_ExhaustedBudget(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task with 3 review failures and max_retries=3 (exhausted).
	content := "---\npriority: 10\nmax_retries: 3\n---\n# Exhausted Task\n" +
		"<!-- review-failure: agent1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: agent2 at 2026-01-02T00:00:00Z — fail 2 -->\n" +
		"<!-- review-failure: agent3 at 2026-01-03T00:00:00Z — fail 3 -->\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md"), []byte(content), 0o644)
	if err := runtimedata.UpdateTaskState(tasksDir, "exhausted.md", func(state *runtimedata.TaskState) {
		state.LastOutcome = runtimedata.OutcomeReviewLaunched
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	stdout, _ := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates (budget exhausted), got %d", len(candidates))
		}
	})

	if !strings.Contains(stdout, "review retry budget exhausted") {
		t.Fatalf("expected budget exhaustion message in stdout, got:\n%s", stdout)
	}

	// Task should be moved to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should be moved to failed/")
	}
	state, err := runtimedata.LoadTaskState(tasksDir, "exhausted.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should be deleted for exhausted review task, got %+v", state)
	}
}

func TestReviewCandidates_FilesystemFallback_ExhaustedBudget_PreservesVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed, "messages"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	content := "---\npriority: 10\nmax_retries: 3\n---\n# Exhausted Task\n" +
		"<!-- review-failure: agent1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: agent2 at 2026-01-02T00:00:00Z — fail 2 -->\n" +
		"<!-- review-failure: agent3 at 2026-01-03T00:00:00Z — fail 3 -->\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md"), []byte(content), 0o644)
	if err := runtimedata.UpdateTaskState(tasksDir, "exhausted.md", func(state *runtimedata.TaskState) {
		state.LastOutcome = runtimedata.OutcomeReviewLaunched
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	// Seed a verdict file that should survive review retry exhaustion.
	verdictPayload, _ := json.Marshal(map[string]string{"verdict": "reject", "reason": "needs work"})
	os.WriteFile(taskfile.VerdictPath(tasksDir, "exhausted.md"), verdictPayload, 0o644)

	captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates (budget exhausted), got %d", len(candidates))
		}
	})

	// Task should be moved to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should be moved to failed/")
	}
	// Taskstate should be deleted.
	state, err := runtimedata.LoadTaskState(tasksDir, "exhausted.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatal("taskstate should be deleted for exhausted review task")
	}
	// Verdict file must be preserved for later retry feedback.
	if _, ok := taskfile.ReadVerdictRejection(tasksDir, "exhausted.md"); !ok {
		t.Fatal("verdict file should be preserved when review retry budget is exhausted")
	}
}

func TestReviewCandidates_FilesystemFallback_BranchFromMarker(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	content := "<!-- branch: task/custom-branch -->\n---\npriority: 10\nmax_retries: 3\n---\n# Branch Task\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "branch-task.md"), []byte(content), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Branch != "task/custom-branch" {
		t.Fatalf("expected branch 'task/custom-branch', got %q", candidates[0].Branch)
	}
}

func TestAppendReviewFailure_WritesMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	recorded := appendReviewFailure(path, "agent1", "test reason")
	if !recorded {
		t.Fatal("expected appendReviewFailure to return true")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure marker not written")
	}
	if !strings.Contains(string(data), "test reason") {
		t.Fatal("reason not included in review-failure marker")
	}
}

func TestAppendReviewFailure_NonexistentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.md")

	_, stderr := captureStdoutStderr(t, func() {
		recorded := appendReviewFailure(path, "agent1", "some reason")
		if recorded {
			t.Fatal("expected appendReviewFailure to return false for nonexistent file")
		}
	})
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected warning in stderr, got:\n%s", stderr)
	}
}

func TestPostReviewAction_VerdictFileCleanedUpOnAllPaths(t *testing.T) {
	tests := []struct {
		name    string
		verdict string
	}{
		{"approve", `{"verdict":"approve"}`},
		{"reject", `{"verdict":"reject","reason":"bad"}`},
		{"error", `{"verdict":"error","reason":"crash"}`},
		{"unknown", `{"verdict":"unknown"}`},
		{"malformed", `{invalid json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
				os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
			}

			taskFile := fmt.Sprintf("cleanup-%s.md", tt.name)
			reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
			os.WriteFile(reviewPath, []byte("# Task\n"), 0o644)

			verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
			os.WriteFile(verdictPath, []byte(tt.verdict), 0o644)

			task := &queue.ClaimedTask{
				Filename: taskFile,
				Branch:   "task/cleanup",
				Title:    "Cleanup",
				TaskPath: reviewPath,
			}

			captureStdoutStderr(t, func() {
				postReviewAction(tasksDir, "host-agent", task)
			})

			if _, err := os.Stat(verdictPath); err == nil {
				t.Fatalf("verdict file should be cleaned up after processing %s verdict", tt.name)
			}
		})
	}
}

func TestBuildReviewContext_TruncatesPreviousRejection(t *testing.T) {
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task"}
	longReason := strings.Repeat("x", maxReviewContextReasonLen+50)
	contextBlock := buildReviewContext(task, "abc123", nil, longReason)
	if !strings.Contains(contextBlock, "... [truncated]") {
		t.Fatalf("expected truncated rejection marker in context:\n%s", contextBlock)
	}
}

func TestPostReviewAction_ErrorVerdictRecordsTaskState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	taskFile := "error.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Task\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{"verdict":"error","reason":"boom"}`), 0o644)
	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/error", Title: "Error", TaskPath: reviewPath})
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeReviewError {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeReviewError)
	}
}

func TestPostReviewAction_MalformedVerdictRecordsIncompleteTaskState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	taskFile := "malformed.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Task\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{bad json`), 0o644)
	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/malformed", Title: "Malformed", TaskPath: reviewPath})
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeReviewIncomplete {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeReviewIncomplete)
	}
}

func TestApproveDisposition_Constants(t *testing.T) {
	if approveDisposition.dir != dirs.ReadyMerge {
		t.Fatalf("approve disposition dir should be %q, got %q", dirs.ReadyMerge, approveDisposition.dir)
	}
	if approveDisposition.messageBody == "" {
		t.Fatal("approve disposition messageBody should not be empty")
	}
}

func TestRejectDisposition_Constants(t *testing.T) {
	if rejectDisposition.dir != dirs.Backlog {
		t.Fatalf("reject disposition dir should be %q, got %q", dirs.Backlog, rejectDisposition.dir)
	}
	if rejectDisposition.messageBody == "" {
		t.Fatal("reject disposition messageBody should not be empty")
	}
}

func TestReviewCandidates_FilesystemFallback_MalformedQuarantined(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Write a malformed task (unterminated frontmatter).
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "malformed.md"),
		[]byte("---\npriority: [oops\n# Malformed\n"), 0o644)

	// Write a valid task to verify it's still returned.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "good.md"),
		[]byte("<!-- branch: task/good -->\n---\npriority: 10\nmax_retries: 3\n---\n# Good Task\n"), 0o644)

	stdout, stderr := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate (malformed should be quarantined), got %d", len(candidates))
		}
		if candidates[0].Filename != "good.md" {
			t.Fatalf("expected good.md, got %q", candidates[0].Filename)
		}
	})

	// Malformed task should be moved to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "malformed.md")); err != nil {
		t.Fatal("malformed task should be moved to failed/")
	}
	// Should no longer exist in ready-for-review/.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, "malformed.md")); !os.IsNotExist(err) {
		t.Fatal("malformed task should no longer be in ready-for-review/")
	}

	// Terminal failure marker should be appended.
	data, _ := os.ReadFile(filepath.Join(tasksDir, dirs.Failed, "malformed.md"))
	if !strings.Contains(string(data), "<!-- terminal-failure:") {
		t.Fatal("terminal-failure marker not written to malformed task")
	}
	if !strings.Contains(string(data), "unparseable frontmatter") {
		t.Fatal("terminal-failure should mention unparseable frontmatter")
	}

	// Stdout should have a quarantine message.
	if !strings.Contains(stdout, "quarantined malformed review candidate") {
		t.Fatalf("expected quarantine message in stdout, got:\n%s", stdout)
	}

	// Stderr should have a warning.
	if !strings.Contains(stderr, "quarantining unparseable review candidate") {
		t.Fatalf("expected quarantine warning in stderr, got:\n%s", stderr)
	}
}

// ---------------------------------------------------------------------------
// hasReviewCandidates tests (read-only idle probe helper)
// ---------------------------------------------------------------------------

func TestHasReviewCandidates_EmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)

	if hasReviewCandidates(tasksDir) {
		t.Fatal("expected false for empty review dir")
	}
}

func TestHasReviewCandidates_NonexistentDir(t *testing.T) {
	if hasReviewCandidates(filepath.Join(t.TempDir(), "nonexistent")) {
		t.Fatal("expected false for nonexistent tasks dir")
	}
}

func TestHasReviewCandidates_ValidTask(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)

	content := "<!-- branch: task/valid -->\n---\npriority: 10\nmax_retries: 3\n---\n# Valid Task\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "valid.md"), []byte(content), 0o644)

	if !hasReviewCandidates(tasksDir) {
		t.Fatal("expected true for valid review task with branch marker")
	}
}

func TestHasReviewCandidates_MalformedTask_NoQuarantine(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, dirs.Failed), 0o755)

	malformedPath := filepath.Join(tasksDir, dirs.ReadyReview, "malformed.md")
	os.WriteFile(malformedPath, []byte("---\npriority: [\n# Broken\n"), 0o644)

	if hasReviewCandidates(tasksDir) {
		t.Fatal("expected false when only malformed tasks exist")
	}

	// The malformed task must remain in ready-for-review/ (not quarantined).
	if _, err := os.Stat(malformedPath); err != nil {
		t.Fatalf("malformed task should remain in ready-for-review/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "malformed.md")); !os.IsNotExist(err) {
		t.Fatal("malformed task must not be moved to failed/ by read-only probe")
	}
}

func TestHasReviewCandidates_ExhaustedBudget_NoMove(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, dirs.Failed), 0o755)

	content := "---\npriority: 10\nmax_retries: 3\n---\n# Exhausted Task\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — fail 2 -->\n" +
		"<!-- review-failure: a3 at 2026-01-03T00:00:00Z — fail 3 -->\n"
	exhaustedPath := filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md")
	os.WriteFile(exhaustedPath, []byte(content), 0o644)

	if hasReviewCandidates(tasksDir) {
		t.Fatal("expected false when only exhausted-budget tasks exist")
	}

	// The exhausted task must remain in ready-for-review/ (not moved).
	if _, err := os.Stat(exhaustedPath); err != nil {
		t.Fatalf("exhausted task should remain in ready-for-review/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "exhausted.md")); !os.IsNotExist(err) {
		t.Fatal("exhausted task must not be moved to failed/ by read-only probe")
	}
}

func TestHasReviewCandidates_MixedTasks_ReturnsTrueForValid(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, dirs.Failed), 0o755)

	// Malformed task.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "bad.md"),
		[]byte("---\npriority: [\n# Broken\n"), 0o644)

	// Exhausted-budget task.
	exhaustedContent := "---\npriority: 10\nmax_retries: 2\n---\n# Exhausted\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — f1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — f2 -->\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md"),
		[]byte(exhaustedContent), 0o644)

	// Valid task with remaining budget and branch marker.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "good.md"),
		[]byte("<!-- branch: task/good -->\n---\npriority: 5\nmax_retries: 3\n---\n# Good Task\n"), 0o644)

	if !hasReviewCandidates(tasksDir) {
		t.Fatal("expected true when at least one valid task exists among malformed/exhausted")
	}

	// Neither the malformed nor the exhausted task should be moved.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, "bad.md")); err != nil {
		t.Fatal("malformed task should remain in ready-for-review/")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should remain in ready-for-review/")
	}
	failedEntries, _ := os.ReadDir(filepath.Join(tasksDir, dirs.Failed))
	if len(failedEntries) != 0 {
		t.Fatalf("nothing should be moved to failed/, found %d entries", len(failedEntries))
	}
}

func TestHasReviewCandidates_BranchlessTask_ReturnsFalse(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, dirs.Failed), 0o755)

	// Task with valid frontmatter and retry budget, but no branch marker.
	content := "---\npriority: 10\nmax_retries: 3\n---\n# Branchless Task\n"
	branchlessPath := filepath.Join(tasksDir, dirs.ReadyReview, "branchless.md")
	os.WriteFile(branchlessPath, []byte(content), 0o644)

	if hasReviewCandidates(tasksDir) {
		t.Fatal("expected false when only a branchless review task exists")
	}

	// The branchless task must remain in ready-for-review/ (not moved).
	if _, err := os.Stat(branchlessPath); err != nil {
		t.Fatalf("branchless task should remain in ready-for-review/: %v", err)
	}
	failedEntries, _ := os.ReadDir(filepath.Join(tasksDir, dirs.Failed))
	if len(failedEntries) != 0 {
		t.Fatalf("nothing should be moved to failed/ by read-only probe, found %d entries", len(failedEntries))
	}
}

func TestHasReviewCandidates_BranchlessMixed_OnlyCountsBranched(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)

	// Branchless task — should not count.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "no-branch.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# No Branch\n"), 0o644)

	// Task with branch marker — should count.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "has-branch.md"),
		[]byte("<!-- branch: task/has-branch -->\n---\npriority: 10\nmax_retries: 3\n---\n# Has Branch\n"), 0o644)

	if !hasReviewCandidates(tasksDir) {
		t.Fatal("expected true when a branched task exists alongside a branchless one")
	}
}

func TestReviewCandidates_Indexed_PrioritySort(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "low-pri.md"),
		[]byte("<!-- branch: task/low-pri -->\n---\npriority: 50\nmax_retries: 3\n---\n# Low Priority\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "high-pri.md"),
		[]byte("<!-- branch: task/high-pri -->\n---\npriority: 5\nmax_retries: 3\n---\n# High Priority\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "mid-pri.md"),
		[]byte("<!-- branch: task/mid-pri -->\n---\npriority: 20\nmax_retries: 3\n---\n# Mid Priority\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	want := []string{"high-pri.md", "mid-pri.md", "low-pri.md"}
	for i, w := range want {
		if candidates[i].Filename != w {
			t.Fatalf("candidate[%d]: expected %q, got %q", i, w, candidates[i].Filename)
		}
	}
}

func TestReviewCandidates_Indexed_SamePrioritySortsByFilename(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "beta.md"),
		[]byte("<!-- branch: task/beta -->\n---\npriority: 10\nmax_retries: 3\n---\n# Beta\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "alpha.md"),
		[]byte("<!-- branch: task/alpha -->\n---\npriority: 10\nmax_retries: 3\n---\n# Alpha\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Filename != "alpha.md" {
		t.Fatalf("expected alpha.md first (alphabetical), got %q", candidates[0].Filename)
	}
	if candidates[1].Filename != "beta.md" {
		t.Fatalf("expected beta.md second, got %q", candidates[1].Filename)
	}
}

func TestReviewCandidates_Indexed_ExhaustedBudget(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	exhaustedContent := "---\npriority: 10\nmax_retries: 2\n---\n# Exhausted\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — fail 2 -->\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md"), []byte(exhaustedContent), 0o644)

	validContent := "<!-- branch: task/valid -->\n---\npriority: 20\nmax_retries: 3\n---\n# Valid\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "valid.md"), []byte(validContent), 0o644)

	idx := queue.BuildIndex(tasksDir)

	stdout, _ := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, idx)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate (exhausted filtered out), got %d", len(candidates))
		}
		if candidates[0].Filename != "valid.md" {
			t.Fatalf("expected valid.md, got %q", candidates[0].Filename)
		}
	})

	if !strings.Contains(stdout, "review retry budget exhausted") {
		t.Fatalf("expected budget exhaustion message in stdout, got:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should be moved to failed/")
	}
}

func TestReviewCandidates_Indexed_ExhaustedBudget_PreservesVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	os.MkdirAll(filepath.Join(tasksDir, "messages"), 0o755)

	exhaustedContent := "---\npriority: 10\nmax_retries: 2\n---\n# Exhausted\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — fail 2 -->\n"
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "exhausted.md"), []byte(exhaustedContent), 0o644)

	// Seed a verdict file that should survive review retry exhaustion.
	verdictPayload, _ := json.Marshal(map[string]string{"verdict": "reject", "reason": "needs improvement"})
	os.WriteFile(taskfile.VerdictPath(tasksDir, "exhausted.md"), verdictPayload, 0o644)

	idx := queue.BuildIndex(tasksDir)

	captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, idx)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates (budget exhausted), got %d", len(candidates))
		}
	})

	// Task should be moved to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should be moved to failed/")
	}
	// Verdict file must be preserved for later retry feedback.
	vr, ok := taskfile.ReadVerdictRejection(tasksDir, "exhausted.md")
	if !ok {
		t.Fatal("verdict file should be preserved when review retry budget is exhausted (indexed path)")
	}
	if vr.Reason != "needs improvement" {
		t.Fatalf("verdict reason = %q, want %q", vr.Reason, "needs improvement")
	}
}

func TestReviewCandidates_Indexed_BranchFromSnapshot(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "with-branch.md"),
		[]byte("<!-- branch: task/custom-branch -->\n---\npriority: 10\nmax_retries: 3\n---\n# With Branch\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Branch != "task/custom-branch" {
		t.Fatalf("expected branch 'task/custom-branch', got %q", candidates[0].Branch)
	}
}

func TestReviewCandidates_IndexedMatchesFilesystemFallback(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "beta.md"),
		[]byte("<!-- branch: task/custom-beta -->\n---\npriority: 10\nmax_retries: 3\n---\n# Beta\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "alpha.md"),
		[]byte("<!-- branch: task/custom-alpha -->\n---\npriority: 10\nmax_retries: 3\n---\n# Alpha\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "top.md"),
		[]byte("<!-- branch: task/top -->\n---\npriority: 5\nmax_retries: 3\n---\n# Top\n"), 0o644)

	noIndex := reviewCandidates(tasksDir, nil)
	indexed := reviewCandidates(tasksDir, queue.BuildIndex(tasksDir))

	if len(noIndex) != len(indexed) {
		t.Fatalf("len(candidates) mismatch: no-index=%d indexed=%d", len(noIndex), len(indexed))
	}
	for i := range noIndex {
		if noIndex[i].Filename != indexed[i].Filename {
			t.Fatalf("candidate[%d] filename mismatch: no-index=%q indexed=%q", i, noIndex[i].Filename, indexed[i].Filename)
		}
		if noIndex[i].Branch != indexed[i].Branch {
			t.Fatalf("candidate[%d] branch mismatch: no-index=%q indexed=%q", i, noIndex[i].Branch, indexed[i].Branch)
		}
		if noIndex[i].Title != indexed[i].Title {
			t.Fatalf("candidate[%d] title mismatch: no-index=%q indexed=%q", i, noIndex[i].Title, indexed[i].Title)
		}
	}
}

func TestReviewCandidates_Indexed_MissingBranchMarkerRecordsFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "no-branch.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# No Branch\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	stdout, stderr := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, idx)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates, got %d", len(candidates))
		}
	})
	if !strings.Contains(stderr, "missing a required branch marker") {
		t.Fatalf("expected missing marker warning, got:\n%s", stderr)
	}
	if !strings.Contains(stdout, "recorded review-failure for no-branch.md") {
		t.Fatalf("expected review-failure log, got:\n%s", stdout)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, "no-branch.md"))
	if err != nil {
		t.Fatalf("ReadFile no-branch.md: %v", err)
	}
	if !strings.Contains(string(data), "missing required") || !strings.Contains(string(data), "ready-for-review") {
		t.Fatalf("expected review-failure marker, got:\n%s", string(data))
	}
}

func TestReviewCandidates_Indexed_TitleExtracted(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "titled.md"),
		[]byte("<!-- branch: task/titled -->\n---\npriority: 10\nmax_retries: 3\n---\n# My Custom Title\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Title != "My Custom Title" {
		t.Fatalf("expected title 'My Custom Title', got %q", candidates[0].Title)
	}
}

func TestVerifyReviewBranch_BranchExists(t *testing.T) {
	repoDir := testutil.SetupRepo(t)

	// Create a task branch.
	if _, err := git.Output(repoDir, "branch", "task/feature-x"); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	task := &queue.ClaimedTask{
		Filename: "feature-x.md",
		Branch:   "task/feature-x",
		Title:    "Feature X",
		TaskPath: filepath.Join(t.TempDir(), "feature-x.md"),
	}
	os.WriteFile(task.TaskPath, []byte("# Feature X\n"), 0o644)

	result := VerifyReviewBranch(repoDir, t.TempDir(), task, "agent-test")
	if !result {
		t.Fatal("expected VerifyReviewBranch to return true for existing branch")
	}

	// Task file should not have a review-failure marker.
	data, _ := os.ReadFile(task.TaskPath)
	if strings.Contains(string(data), "review-failure") {
		t.Fatal("should not write review-failure when branch exists")
	}
}

func TestVerifyReviewBranch_BranchMissing(t *testing.T) {
	repoDir := testutil.SetupRepo(t)

	taskDir := t.TempDir()
	taskPath := filepath.Join(taskDir, "missing-branch.md")
	os.WriteFile(taskPath, []byte("# Missing Branch\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: "missing-branch.md",
		Branch:   "task/nonexistent-branch",
		Title:    "Missing Branch",
		TaskPath: taskPath,
	}

	_, stderr := captureStdoutStderr(t, func() {
		result := VerifyReviewBranch(repoDir, taskDir, task, "agent-test")
		if result {
			t.Fatal("expected VerifyReviewBranch to return false for missing branch")
		}
	})

	if !strings.Contains(stderr, "task branch") {
		t.Fatalf("expected warning about missing branch in stderr, got:\n%s", stderr)
	}

	// Task file should have a review-failure marker.
	data, _ := os.ReadFile(taskPath)
	if !strings.Contains(string(data), "review-failure") {
		t.Fatal("should write review-failure marker when branch is missing")
	}
	if !strings.Contains(string(data), "not found in host repo") {
		t.Fatal("review-failure should mention branch not found")
	}
	state, err := runtimedata.LoadTaskState(taskDir, task.Filename)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("missing branch should record taskstate")
	}
	if state.LastOutcome != runtimedata.OutcomeReviewBranchMissing {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeReviewBranchMissing)
	}
}

func TestReviewCandidates_Indexed_MalformedQuarantined(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed, dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyMerge, dirs.Completed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Write a malformed task.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "malformed.md"),
		[]byte("---\npriority: [oops\n# Malformed\n"), 0o644)

	// Write a valid task.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "good.md"),
		[]byte("<!-- branch: task/good -->\n---\npriority: 10\nmax_retries: 3\n---\n# Good Task\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)

	stdout, stderr := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, idx)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate (malformed should be quarantined), got %d", len(candidates))
		}
		if candidates[0].Filename != "good.md" {
			t.Fatalf("expected good.md, got %q", candidates[0].Filename)
		}
	})

	// Malformed task should be moved to failed/.
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "malformed.md")); err != nil {
		t.Fatal("malformed task should be moved to failed/")
	}

	// Terminal failure marker should be appended.
	data, _ := os.ReadFile(filepath.Join(tasksDir, dirs.Failed, "malformed.md"))
	if !strings.Contains(string(data), "<!-- terminal-failure:") {
		t.Fatal("terminal-failure marker not written to malformed task")
	}

	if !strings.Contains(stdout, "quarantined malformed review candidate") {
		t.Fatalf("expected quarantine message in stdout, got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "quarantining unparseable review candidate") {
		t.Fatalf("expected quarantine warning in stderr, got:\n%s", stderr)
	}
}

func TestRunReview_UsesReviewModelAndReasoningEffort(t *testing.T) {
	origExecCommandContext := execCommandContext
	defer func() { execCommandContext = origExecCommandContext }()

	var capturedArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		cmd := exec.CommandContext(ctx, "true")
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		return cmd
	}

	repoRoot := testutil.SetupRepo(t)
	env := envConfig{
		workdir:               "/workspace",
		repoRoot:              repoRoot,
		reviewModel:           "gpt-5.4",
		reviewReasoningEffort: "xhigh",
		homeDir:               "/home/test",
		image:                 "ubuntu:24.04",
	}
	run := runContext{timeout: time.Second}
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task", Title: "Task", TaskPath: filepath.Join(t.TempDir(), "task.md")}
	if _, err := git.Output(repoRoot, "branch", task.Branch); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	if err := runReview(context.Background(), env, run, task, "main"); err != nil {
		t.Fatalf("runReview: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--model gpt-5.4") {
		t.Fatalf("expected review model in docker args, got %s", joined)
	}
	if !strings.Contains(joined, "--reasoning-effort xhigh") {
		t.Fatalf("expected review reasoning effort in docker args, got %s", joined)
	}
}

func TestBuildReviewContext_InitialReview(t *testing.T) {
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task"}
	contextBlock := buildReviewContext(task, "abc123", nil, "")
	if !strings.Contains(contextBlock, "- current branch tip: abc123") {
		t.Fatalf("current tip missing from context:\n%s", contextBlock)
	}
	if !strings.Contains(contextBlock, "- last reviewed branch tip: none") {
		t.Fatalf("initial context should include neutral last-reviewed value:\n%s", contextBlock)
	}
	if !strings.Contains(contextBlock, "- previous rejection: none") {
		t.Fatalf("initial context should include neutral rejection value:\n%s", contextBlock)
	}
	if !strings.Contains(contextBlock, "initial review; assess the current diff independently") {
		t.Fatalf("initial review mode missing from context:\n%s", contextBlock)
	}
}

func TestBuildReviewContext_FollowUpReview(t *testing.T) {
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task"}
	state := &runtimedata.TaskState{LastReviewedSHA: "def456"}
	contextBlock := buildReviewContext(task, "abc123", state, "missing unit tests")
	if !strings.Contains(contextBlock, "- last reviewed branch tip: def456") {
		t.Fatalf("follow-up context should include prior review SHA:\n%s", contextBlock)
	}
	if !strings.Contains(contextBlock, "- previous rejection: missing unit tests") {
		t.Fatalf("follow-up context should include previous rejection:\n%s", contextBlock)
	}
	if !strings.Contains(contextBlock, "follow-up review; reassess the current diff independently") {
		t.Fatalf("follow-up review mode missing from context:\n%s", contextBlock)
	}
}

func TestBuildReviewContext_TruncatesPreviousRejectionAtRuneBoundary(t *testing.T) {
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task"}
	longReason := strings.Repeat("界", maxReviewContextReasonLen+10)
	contextBlock := buildReviewContext(task, "abc123", nil, longReason)
	if !utf8.ValidString(contextBlock) {
		t.Fatalf("context should remain valid UTF-8, got %q", contextBlock)
	}
	if !strings.Contains(contextBlock, "... [truncated]") {
		t.Fatalf("expected truncation marker in context:\n%s", contextBlock)
	}
}

func TestRunReview_InjectsReviewContextAndRecordsLaunchState(t *testing.T) {
	origExecCommandContext := execCommandContext
	defer func() { execCommandContext = origExecCommandContext }()

	var capturedArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		cmd := exec.CommandContext(ctx, "true")
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		return cmd
	}

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskPath := filepath.Join(tasksDir, dirs.ReadyReview, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — missing tests -->\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/task"); err != nil {
		t.Fatalf("create review branch: %v", err)
	}
	currentTip, err := git.Output(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	currentTip = strings.TrimSpace(currentTip)
	if err := runtimedata.UpdateTaskState(tasksDir, "task.md", func(state *runtimedata.TaskState) {
		state.LastReviewedSHA = "older-sha"
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	env := envConfig{
		workdir:                    "/workspace",
		repoRoot:                   repoRoot,
		tasksDir:                   tasksDir,
		reviewModel:                "gpt-5.4",
		reviewReasoningEffort:      "high",
		reviewSessionResumeEnabled: true,
		homeDir:                    "/home/test",
		image:                      "ubuntu:24.04",
	}
	run := runContext{timeout: time.Second}
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task", Title: "Task", TaskPath: taskPath}

	if err := runReview(context.Background(), env, run, task, "main"); err != nil {
		t.Fatalf("runReview: %v", err)
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "Review context:") {
		t.Fatalf("docker args should contain review context prompt, got %s", joined)
	}
	if !strings.Contains(joined, "- current branch tip: "+currentTip) {
		t.Fatalf("prompt should include current tip, got %s", joined)
	}
	if !strings.Contains(joined, "- last reviewed branch tip: older-sha") {
		t.Fatalf("prompt should include previous reviewed SHA, got %s", joined)
	}
	if !strings.Contains(joined, "- previous rejection: missing tests") {
		t.Fatalf("prompt should include previous rejection, got %s", joined)
	}
	state, err := runtimedata.LoadTaskState(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state.LastReviewedSHA != "older-sha" {
		t.Fatalf("LastReviewedSHA = %q, want %q", state.LastReviewedSHA, "older-sha")
	}
	if state.LastOutcome != runtimedata.OutcomeReviewLaunched {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeReviewLaunched)
	}
	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindReview, "task.md")
	if err != nil {
		t.Fatalf("Load review session: %v", err)
	}
	if session == nil {
		t.Fatal("expected review session to be created")
	}
	if !strings.Contains(joined, "--resume="+session.CopilotSessionID) {
		t.Fatalf("docker args should contain review resume session, got %s", joined)
	}
}

func TestRunReview_BranchChangeRotatesReviewSessionID(t *testing.T) {
	origExecCommandContext := execCommandContext
	defer func() { execCommandContext = origExecCommandContext }()

	var capturedArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		cmd := exec.CommandContext(ctx, "true")
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		return cmd
	}

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskPath := filepath.Join(tasksDir, dirs.ReadyReview, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/task-new"); err != nil {
		t.Fatalf("create review branch: %v", err)
	}

	// Seed a review session on the OLD branch with a known session ID.
	oldSessionID := "stale-review-session-from-old-branch"
	if err := runtimedata.UpdateSession(tasksDir, runtimedata.KindReview, "task.md", func(session *runtimedata.Session) {
		session.CopilotSessionID = oldSessionID
		session.TaskBranch = "task/task-old"
	}); err != nil {
		t.Fatalf("seed review session: %v", err)
	}

	env := envConfig{
		workdir:                    "/workspace",
		repoRoot:                   repoRoot,
		tasksDir:                   tasksDir,
		reviewModel:                "gpt-5.4",
		reviewReasoningEffort:      "high",
		reviewSessionResumeEnabled: true,
		homeDir:                    "/home/test",
		image:                      "ubuntu:24.04",
	}
	run := runContext{timeout: time.Second}
	// The claimed task uses a DIFFERENT branch than the seeded session.
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task-new", Title: "Task", TaskPath: taskPath}

	if err := runReview(context.Background(), env, run, task, "main"); err != nil {
		t.Fatalf("runReview: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	// The resume session ID should NOT be the stale one from the old branch.
	if strings.Contains(joined, "--resume="+oldSessionID) {
		t.Fatalf("expected rotated review session ID after branch change, but got stale ID %q in docker args: %s", oldSessionID, joined)
	}
	// It should still have a --resume arg (with the new rotated ID).
	if !strings.Contains(joined, "--resume=") {
		t.Fatalf("expected --resume with rotated review session ID in docker args, got: %s", joined)
	}
	// Verify the persisted session metadata has the new branch and a new session ID.
	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindReview, "task.md")
	if err != nil {
		t.Fatalf("Load review session: %v", err)
	}
	if session == nil {
		t.Fatal("expected review session to exist after branch change")
	}
	if session.CopilotSessionID == oldSessionID {
		t.Fatal("persisted review session ID should be rotated, not the stale one")
	}
	if session.TaskBranch != "task/task-new" {
		t.Fatalf("TaskBranch = %q, want %q", session.TaskBranch, "task/task-new")
	}
}

func TestPostReviewAction_ApprovedUpdatesLastReviewedSHA(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	taskFile := "approved-state.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if err := os.WriteFile(reviewPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, taskFile, func(state *runtimedata.TaskState) {
		state.LastHeadSHA = "current-tip"
		state.LastReviewedSHA = "older-tip"
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{"verdict":"approve"}`), 0o644); err != nil {
		t.Fatalf("WriteFile verdict: %v", err)
	}

	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/approved-state", Title: "Approved", TaskPath: reviewPath})

	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to exist")
	}
	if state.LastReviewedSHA != "current-tip" {
		t.Fatalf("LastReviewedSHA = %q, want %q", state.LastReviewedSHA, "current-tip")
	}
	if state.LastOutcome != runtimedata.OutcomeReviewApproved {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeReviewApproved)
	}
}

func TestPostReviewAction_RejectedUpdatesLastReviewedSHA(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	taskFile := "rejected-state.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if err := os.WriteFile(reviewPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, taskFile, func(state *runtimedata.TaskState) {
		state.LastHeadSHA = "rejected-tip"
		state.LastReviewedSHA = "older-tip"
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{"verdict":"reject","reason":"missing tests"}`), 0o644); err != nil {
		t.Fatalf("WriteFile verdict: %v", err)
	}

	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/rejected-state", Title: "Rejected", TaskPath: reviewPath})

	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to exist")
	}
	if state.LastReviewedSHA != "rejected-tip" {
		t.Fatalf("LastReviewedSHA = %q, want %q", state.LastReviewedSHA, "rejected-tip")
	}
	if state.LastOutcome != runtimedata.OutcomeReviewRejected {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeReviewRejected)
	}
}

func TestRunReview_DisabledResumeSkipsSessionCreation(t *testing.T) {
	origExecCommandContext := execCommandContext
	defer func() { execCommandContext = origExecCommandContext }()

	var capturedArgs []string
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		cmd := exec.CommandContext(ctx, "true")
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		return cmd
	}

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskPath := filepath.Join(tasksDir, dirs.ReadyReview, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/task"); err != nil {
		t.Fatalf("create review branch: %v", err)
	}

	env := envConfig{
		workdir:                    "/workspace",
		repoRoot:                   repoRoot,
		tasksDir:                   tasksDir,
		reviewModel:                "gpt-5.4",
		reviewReasoningEffort:      "high",
		reviewSessionResumeEnabled: false,
		homeDir:                    "/home/test",
		image:                      "ubuntu:24.04",
	}
	run := runContext{timeout: time.Second}
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task", Title: "Task", TaskPath: taskPath}

	if err := runReview(context.Background(), env, run, task, "main"); err != nil {
		t.Fatalf("runReview: %v", err)
	}
	joined := strings.Join(capturedArgs, " ")
	if strings.Contains(joined, "--resume=") {
		t.Fatalf("docker args should not contain review resume session, got %s", joined)
	}
	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindReview, "task.md")
	if err != nil {
		t.Fatalf("Load review session: %v", err)
	}
	if session != nil {
		t.Fatalf("expected no review session when disabled, got %+v", session)
	}
}

func TestRunReview_CloneFailureDoesNotCreateReviewSessionOrLaunchState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Backlog, dirs.InProgress, dirs.ReadyMerge, dirs.Completed, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
	}
	taskPath := filepath.Join(tasksDir, dirs.ReadyReview, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}

	env := envConfig{
		workdir:                    "/workspace",
		repoRoot:                   filepath.Join(t.TempDir(), "missing-repo"),
		tasksDir:                   tasksDir,
		reviewModel:                "gpt-5.4",
		reviewReasoningEffort:      "high",
		reviewSessionResumeEnabled: true,
		homeDir:                    "/home/test",
		image:                      "ubuntu:24.04",
	}
	run := runContext{timeout: time.Second}
	task := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task", Title: "Task", TaskPath: taskPath}

	err := runReview(context.Background(), env, run, task, "main")
	if err == nil {
		t.Fatal("expected runReview error")
	}
	if !strings.Contains(err.Error(), "create clone for review") {
		t.Fatalf("unexpected error: %v", err)
	}
	state, err := runtimedata.LoadTaskState(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should not be created on clone failure, got %+v", state)
	}
	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindReview, "task.md")
	if err != nil {
		t.Fatalf("Load review session: %v", err)
	}
	if session != nil {
		t.Fatalf("review session should not be created on clone failure, got %+v", session)
	}
}

func TestLoadTaskStateForReview_CorruptFallsBackToNil(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, "runtime", "taskstate", "task.md.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{bad json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, stderr := captureStdoutStderr(t, func() {
		state := loadTaskStateForReview(tasksDir, "task.md")
		if state != nil {
			t.Fatalf("loadTaskStateForReview returned %+v, want nil", state)
		}
	})
	if !strings.Contains(stderr, "could not load taskstate") {
		t.Fatalf("expected corrupt taskstate warning, got:\n%s", stderr)
	}
}

func TestReviewCandidates_FilesystemFallback_MissingBranchMarkersAreSkipped(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "add_feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Underscore\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "add-feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Dash\n"), 0o644)

	stdout, _ := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates, got %d", len(candidates))
		}
	})
	if strings.Count(stdout, "recorded review-failure for") != 2 {
		t.Fatalf("expected review-failure logs for both tasks, got:\n%s", stdout)
	}
	for _, name := range []string{"add_feature.md", "add-feature.md"} {
		data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if !strings.Contains(string(data), "missing required") || !strings.Contains(string(data), "ready-for-review") {
			t.Fatalf("expected review-failure marker for %s, got:\n%s", name, string(data))
		}
	}
}

func TestReviewCandidates_Indexed_MissingBranchMarkersAreSkipped(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "add_feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Underscore\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "add-feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Dash\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	stdout, _ := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, idx)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates, got %d", len(candidates))
		}
	})
	if strings.Count(stdout, "recorded review-failure for") != 2 {
		t.Fatalf("expected review-failure logs for both tasks, got:\n%s", stdout)
	}
	for _, name := range []string{"add_feature.md", "add-feature.md"} {
		data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, name))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		if !strings.Contains(string(data), "missing required") || !strings.Contains(string(data), "ready-for-review") {
			t.Fatalf("expected review-failure marker for %s, got:\n%s", name, string(data))
		}
	}
}

func TestReviewCandidates_ExplicitBranchUnchanged(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// One task with an explicit branch, one without.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "add-feature.md"),
		[]byte("<!-- branch: task/add-feature -->\n---\npriority: 10\nmax_retries: 3\n---\n# Explicit\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "add_feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Synthesized\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	// The explicit branch must remain exactly as written.
	var explicit *queue.ClaimedTask
	for _, c := range candidates {
		if c.Filename == "add-feature.md" {
			explicit = c
		}
	}
	if explicit == nil {
		t.Fatal("could not find add-feature.md candidate")
	}
	if explicit.Branch != "task/add-feature" {
		t.Fatalf("explicit branch changed to %q, want %q", explicit.Branch, "task/add-feature")
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, "add_feature.md"))
	if err != nil {
		t.Fatalf("ReadFile add_feature.md: %v", err)
	}
	if !strings.Contains(string(data), "missing required") || !strings.Contains(string(data), "ready-for-review") {
		t.Fatalf("expected review-failure marker on markerless task, got:\n%s", string(data))
	}
}

func TestPostReviewAction_ApproveMoveFails_NoMarkerVerdictPreserved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "move-fail-approve.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Move Fail Approve\n"), 0o644)

	// Pre-create a file at the destination to cause the move to fail.
	dstPath := filepath.Join(tasksDir, dirs.ReadyMerge, taskFile)
	os.WriteFile(dstPath, []byte("# Existing\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"approve"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/move-fail-approve",
		Title:    "Move Fail Approve",
		TaskPath: reviewPath,
	}

	captureStdoutStderr(t, func() {
		postReviewAction(tasksDir, "host-agent", task)
	})

	// Task should stay in ready-for-review/.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task should stay in ready-for-review/ when move fails")
	}
	// No approval marker should be written.
	srcData, _ := os.ReadFile(reviewPath)
	if strings.Contains(string(srcData), "<!-- reviewed:") {
		t.Fatal("approval marker should NOT be written when move fails")
	}
	// Review-failure should be recorded.
	if !strings.Contains(string(srcData), "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded when move fails:\n%s", string(srcData))
	}
	// Verdict file should be preserved for retry.
	if _, err := os.Stat(verdictPath); err != nil {
		t.Fatal("verdict file should be preserved when move fails")
	}
	// TaskState should reflect the move failure.
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeReviewMoveFailed {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeReviewMoveFailed)
	}
}

func TestPostReviewAction_RejectMoveFails_NoMarkerVerdictPreserved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "move-fail-reject.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Move Fail Reject\n"), 0o644)

	// Pre-create a file at the destination to cause the move to fail.
	dstPath := filepath.Join(tasksDir, dirs.Backlog, taskFile)
	os.WriteFile(dstPath, []byte("# Existing\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":"missing tests"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/move-fail-reject",
		Title:    "Move Fail Reject",
		TaskPath: reviewPath,
	}

	captureStdoutStderr(t, func() {
		postReviewAction(tasksDir, "host-agent", task)
	})

	// Task should stay in ready-for-review/.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task should stay in ready-for-review/ when move fails")
	}
	// No rejection marker should be written.
	srcData, _ := os.ReadFile(reviewPath)
	if strings.Contains(string(srcData), "<!-- review-rejection:") {
		t.Fatal("rejection marker should NOT be written when move fails")
	}
	// Review-failure should be recorded.
	if !strings.Contains(string(srcData), "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded when move fails:\n%s", string(srcData))
	}
	// Verdict file should be preserved for retry.
	if _, err := os.Stat(verdictPath); err != nil {
		t.Fatal("verdict file should be preserved when move fails")
	}
	// TaskState should reflect the move failure.
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeReviewMoveFailed {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeReviewMoveFailed)
	}
}

func TestMoveReviewedTask_MoveFails_RecordsReviewFailure(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, dirs.ReadyReview)
	dstDir := filepath.Join(tasksDir, dirs.ReadyMerge)
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "fail-move.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# Fail Move\n"), 0o644)

	// Pre-create destination.
	os.WriteFile(filepath.Join(dstDir, taskFile), []byte("# Existing\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/fail-move",
		Title:    "Fail Move",
		TaskPath: srcPath,
	}

	marker := "\n<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->\n"
	captureStdoutStderr(t, func() {
		ok := moveReviewedTask(tasksDir, "agent1", task, approveDisposition, marker)
		if ok {
			t.Fatal("moveReviewedTask should return false when move fails")
		}
	})

	// Source should still exist without the marker.
	srcData, _ := os.ReadFile(srcPath)
	if strings.Contains(string(srcData), "<!-- reviewed:") {
		t.Fatal("marker should not be written when move fails")
	}
	// Review-failure should be recorded in the source file.
	if !strings.Contains(string(srcData), "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded when move fails:\n%s", string(srcData))
	}
	// TaskState should reflect the move failure.
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeReviewMoveFailed {
		t.Fatalf("taskstate = %+v, want LastOutcome=%s", state, runtimedata.OutcomeReviewMoveFailed)
	}
}

func TestMoveReviewedTask_RejectionMarkerAppendFails_FallbackSucceeds(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, dirs.ReadyReview)
	dstDir := filepath.Join(tasksDir, dirs.Backlog)
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "reject-marker-fallback.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# Reject Marker Fallback\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/reject-marker-fallback",
		Title:    "Reject Marker Fallback",
		TaskPath: srcPath,
	}

	// Stub appendToFileFn to fail — the fallback (read-modify-write) should
	// still succeed since it uses atomicwrite.WriteFile directly.
	origAppend := appendToFileFn
	t.Cleanup(func() { appendToFileFn = origAppend })
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated append failure")
	}

	marker := "\n<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — tests missing -->\n"
	_, stderr := captureStdoutStderr(t, func() {
		ok := moveReviewedTask(tasksDir, "agent1", task, rejectDisposition, marker)
		if !ok {
			t.Fatal("moveReviewedTask should return true when fallback write succeeds")
		}
	})

	if !strings.Contains(stderr, "could not write verdict marker") {
		t.Fatalf("expected primary write warning, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "rejection marker written via fallback") {
		t.Fatalf("expected fallback success message, got:\n%s", stderr)
	}
	// The marker must be in the file so extractReviewRejections can read it.
	dstPath := filepath.Join(dstDir, taskFile)
	feedback := extractReviewRejections(dstPath)
	if !strings.Contains(feedback, "tests missing") {
		dstData, _ := os.ReadFile(dstPath)
		t.Fatalf("review feedback should be available via extractReviewRejections, got %q; file:\n%s", feedback, string(dstData))
	}
}

func TestMoveReviewedTask_RejectionMarkerBothWritesFail_ReturnsFalse(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, dirs.ReadyReview)
	dstDir := filepath.Join(tasksDir, dirs.Backlog)
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "reject-marker-both-fail.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# Reject Both Fail\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/reject-marker-both-fail",
		Title:    "Reject Both Fail",
		TaskPath: srcPath,
	}

	origAppend := appendToFileFn
	origFallback := fallbackMarkerWrite
	t.Cleanup(func() {
		appendToFileFn = origAppend
		fallbackMarkerWrite = origFallback
	})
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated append failure")
	}
	fallbackMarkerWrite = func(dst, marker string) error {
		return fmt.Errorf("simulated fallback failure")
	}

	marker := "\n<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — tests missing -->\n"
	_, stderr := captureStdoutStderr(t, func() {
		ok := moveReviewedTask(tasksDir, "agent1", task, rejectDisposition, marker)
		if ok {
			t.Fatal("moveReviewedTask should return false when both writes fail")
		}
	})

	if !strings.Contains(stderr, "could not write verdict marker") {
		t.Fatalf("expected primary write warning, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fallback marker write also failed") {
		t.Fatalf("expected fallback failure warning, got:\n%s", stderr)
	}
}

func TestMoveReviewedTask_ApprovalMarkerWriteFails_ReturnsTrue(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, dirs.ReadyReview)
	dstDir := filepath.Join(tasksDir, dirs.ReadyMerge)
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "approve-marker-fail.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# Approve Marker Fail\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/approve-marker-fail",
		Title:    "Approve Marker Fail",
		TaskPath: srcPath,
	}

	origAppend := appendToFileFn
	t.Cleanup(func() { appendToFileFn = origAppend })
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated approval write failure")
	}

	marker := "\n<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->\n"
	_, stderr := captureStdoutStderr(t, func() {
		ok := moveReviewedTask(tasksDir, "agent1", task, approveDisposition, marker)
		if !ok {
			t.Fatal("moveReviewedTask should return true for approval even when marker write fails")
		}
	})

	if _, err := os.Stat(filepath.Join(dstDir, taskFile)); err != nil {
		t.Fatal("task should be in ready-to-merge/ after successful move")
	}
	if !strings.Contains(stderr, "could not write verdict marker") {
		t.Fatalf("expected marker write warning in stderr, got:\n%s", stderr)
	}
}

func TestPostReviewAction_RejectionBothWritesFail_VerdictFallback(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.ReadyMerge, dirs.Backlog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "rejection-verdict-fallback.md"
	reviewPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Rejection Verdict Fallback\n"), 0o644)
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":"missing tests and docs"}`), 0o644)

	origAppend := appendToFileFn
	origFallback := fallbackMarkerWrite
	t.Cleanup(func() {
		appendToFileFn = origAppend
		fallbackMarkerWrite = origFallback
	})
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated append failure")
	}
	fallbackMarkerWrite = func(dst, marker string) error {
		return fmt.Errorf("simulated fallback failure")
	}

	task := &queue.ClaimedTask{Filename: taskFile, Branch: "task/rejection-feedback", Title: "Rejection Verdict Fallback", TaskPath: reviewPath}
	captureStdoutStderr(t, func() {
		postReviewAction(tasksDir, "host-agent", task)
	})

	// Task is in backlog/ (move succeeded).
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, taskFile)); err != nil {
		t.Fatal("task should be moved to backlog/")
	}
	// Verdict file must be preserved — both write methods failed.
	if _, err := os.Stat(verdictPath); err != nil {
		t.Fatal("verdict file must be preserved when both marker writes fail")
	}
	// The task file should NOT have the rejection marker.
	dstData, _ := os.ReadFile(filepath.Join(tasksDir, dirs.Backlog, taskFile))
	if strings.Contains(string(dstData), "<!-- review-rejection:") {
		t.Fatal("rejection marker should not be present when both writes failed")
	}
	// extractReviewRejectionsWithVerdictFallback should still find the
	// feedback via the preserved verdict file — proving the next work
	// agent can consume it via MATO_REVIEW_FEEDBACK.
	dstPath := filepath.Join(tasksDir, dirs.Backlog, taskFile)
	feedback := extractReviewRejectionsWithVerdictFallback(dstPath, tasksDir, taskFile)
	if !strings.Contains(feedback, "missing tests and docs") {
		t.Fatalf("review feedback should be available via verdict fallback, got %q", feedback)
	}
}
