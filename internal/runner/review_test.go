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

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/queue"
	"mato/internal/sessionmeta"
	"mato/internal/taskstate"
	"mato/internal/testutil"
)

func TestResolveReviewVerdict_Approved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte("# Task\n<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->\n"), 0o644)

	task := &queue.ClaimedTask{TaskPath: path}
	if v := resolveReviewVerdict(task); v != "approve" {
		t.Fatalf("expected 'approve', got %q", v)
	}
}

func TestResolveReviewVerdict_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte("# Task\n<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing tests -->\n"), 0o644)

	task := &queue.ClaimedTask{TaskPath: path}
	if v := resolveReviewVerdict(task); v != "reject" {
		t.Fatalf("expected 'reject', got %q", v)
	}
}

func TestResolveReviewVerdict_NoMarkers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte("# Task\nSome body text\n"), 0o644)

	task := &queue.ClaimedTask{TaskPath: path}
	if v := resolveReviewVerdict(task); v != "" {
		t.Fatalf("expected empty string, got %q", v)
	}
}

func TestResolveReviewVerdict_NonexistentFile(t *testing.T) {
	task := &queue.ClaimedTask{TaskPath: filepath.Join(t.TempDir(), "nonexistent.md")}
	if v := resolveReviewVerdict(task); v != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", v)
	}
}

func TestResolveReviewVerdict_BothMarkers_ApproveTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := "# Task\n" +
		"<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->\n" +
		"<!-- review-rejection: agent2 at 2026-01-02T00:00:00Z — bad code -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	task := &queue.ClaimedTask{TaskPath: path}
	// The function checks approval first, so it should return "approve".
	if v := resolveReviewVerdict(task); v != "approve" {
		t.Fatalf("expected 'approve' when both markers present, got %q", v)
	}
}

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
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task file does NOT exist (already moved by another process).
	task := &queue.ClaimedTask{
		Filename: "gone-task.md",
		Branch:   "task/gone-task",
		Title:    "Gone Task",
		TaskPath: filepath.Join(tasksDir, queue.DirReadyReview, "gone-task.md"),
	}

	// Should return without error or panic.
	postReviewAction(tasksDir, "host-agent", task)

	// No files should have been created in ready-to-merge/ or backlog/.
	files, _ := os.ReadDir(filepath.Join(tasksDir, queue.DirReadyMerge))
	if len(files) > 0 {
		t.Fatal("no files should appear in ready-to-merge/ for already-moved task")
	}
}

func TestPostReviewAction_TaskAlreadyMovedLogsWarning(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task file does NOT exist (already moved by another process).
	task := &queue.ClaimedTask{
		Filename: "moved-task.md",
		Branch:   "task/moved-task",
		Title:    "Moved Task",
		TaskPath: filepath.Join(tasksDir, queue.DirReadyReview, "moved-task.md"),
	}

	// Capture stderr to verify the warning is emitted.
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	postReviewAction(tasksDir, "host-agent", task)

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
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
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

	postReviewAction(tasksDir, "host-agent", task)

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
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "case-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
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

	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyMerge, taskFile)); err != nil {
		t.Fatal("APPROVE (uppercase) should be treated as approval")
	}
}

func TestPostReviewAction_RejectedEmptyReason(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "empty-reason.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
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

	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, taskFile)); err != nil {
		t.Fatal("rejected task should be moved to backlog/")
	}
	data, _ := os.ReadFile(filepath.Join(tasksDir, queue.DirBacklog, taskFile))
	if !strings.Contains(string(data), "no reason provided") {
		t.Fatalf("empty reason should be replaced with 'no reason provided', got:\n%s", string(data))
	}
}

func TestPostReviewAction_FallbackToApprovalMarker(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "fallback-approve.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Fallback\n<!-- reviewed: review-agent at 2026-01-01T00:00:00Z — approved -->\n"), 0o644)

	// No verdict file exists.
	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/fallback-approve",
		Title:    "Fallback Approve",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyMerge, taskFile)); err != nil {
		t.Fatal("fallback to approval marker should move task to ready-to-merge/")
	}
}

func TestPostReviewAction_FallbackToRejectionMarker(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "fallback-reject.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Fallback\n<!-- review-rejection: review-agent at 2026-01-01T00:00:00Z — tests missing -->\n"), 0o644)

	// No verdict file exists.
	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/fallback-reject",
		Title:    "Fallback Reject",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, taskFile)); err != nil {
		t.Fatal("fallback to rejection marker should move task to backlog/")
	}
}

func TestPostReviewAction_ErrorVerdictEmptyReason(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "error-empty.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
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
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, queue.DirFailed), 0o755)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(candidates))
	}
}

func TestReviewCandidates_FilesystemFallback_SingleTask(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskContent := "---\npriority: 10\nmax_retries: 3\n---\n# Test Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "test-task.md"), []byte(taskContent), 0o644)

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
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "low-pri.md"),
		[]byte("---\npriority: 50\nmax_retries: 3\n---\n# Low Priority\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "high-pri.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# High Priority\n"), 0o644)

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
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task with 3 review failures and max_retries=3 (exhausted).
	content := "---\npriority: 10\nmax_retries: 3\n---\n# Exhausted Task\n" +
		"<!-- review-failure: agent1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: agent2 at 2026-01-02T00:00:00Z — fail 2 -->\n" +
		"<!-- review-failure: agent3 at 2026-01-03T00:00:00Z — fail 3 -->\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "exhausted.md"), []byte(content), 0o644)
	if err := taskstate.Update(tasksDir, "exhausted.md", func(state *taskstate.TaskState) {
		state.LastOutcome = "review-launched"
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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should be moved to failed/")
	}
	state, err := taskstate.Load(tasksDir, "exhausted.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should be deleted for exhausted review task, got %+v", state)
	}
}

func TestReviewCandidates_FilesystemFallback_BranchFromMarker(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	content := "<!-- branch: task/custom-branch -->\n---\npriority: 10\nmax_retries: 3\n---\n# Branch Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "branch-task.md"), []byte(content), 0o644)

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

func TestReviewedRe_MatchesVariousFormats(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"standard approval", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->", true},
		{"extra spaces", "<!-- reviewed:  agent1  at  2026-01-01T00:00:00Z  —  approved  -->", true},
		{"missing approved", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — rejected -->", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewedRe.MatchString(tt.input); got != tt.want {
				t.Fatalf("reviewedRe.MatchString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestReviewRejectionRe_MatchesVariousFormats(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"standard rejection", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing tests -->", true},
		{"multi-word reason", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing tests and docs -->", true},
		{"empty reason", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — -->", false},
		{"no em-dash", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z missing tests -->", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewRejectionRe.MatchString(tt.input); got != tt.want {
				t.Fatalf("reviewRejectionRe.MatchString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
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
			for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
				os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
			}

			taskFile := fmt.Sprintf("cleanup-%s.md", tt.name)
			reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
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
	for _, sub := range []string{queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	taskFile := "error.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Task\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{"verdict":"error","reason":"boom"}`), 0o644)
	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/error", Title: "Error", TaskPath: reviewPath})
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-error" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-error", state)
	}
}

func TestPostReviewAction_MalformedVerdictRecordsIncompleteTaskState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	taskFile := "malformed.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Task\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{bad json`), 0o644)
	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/malformed", Title: "Malformed", TaskPath: reviewPath})
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-incomplete" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-incomplete", state)
	}
}

func TestApproveDisposition_Constants(t *testing.T) {
	if approveDisposition.dir != queue.DirReadyMerge {
		t.Fatalf("approve disposition dir should be %q, got %q", queue.DirReadyMerge, approveDisposition.dir)
	}
	if approveDisposition.messageBody == "" {
		t.Fatal("approve disposition messageBody should not be empty")
	}
}

func TestRejectDisposition_Constants(t *testing.T) {
	if rejectDisposition.dir != queue.DirBacklog {
		t.Fatalf("reject disposition dir should be %q, got %q", queue.DirBacklog, rejectDisposition.dir)
	}
	if rejectDisposition.messageBody == "" {
		t.Fatal("reject disposition messageBody should not be empty")
	}
}

func TestReviewCandidates_FilesystemFallback_MalformedQuarantined(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Write a malformed task (unterminated frontmatter).
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "malformed.md"),
		[]byte("---\npriority: [oops\n# Malformed\n"), 0o644)

	// Write a valid task to verify it's still returned.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "good.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Good Task\n"), 0o644)

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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "malformed.md")); err != nil {
		t.Fatal("malformed task should be moved to failed/")
	}
	// Should no longer exist in ready-for-review/.
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyReview, "malformed.md")); !os.IsNotExist(err) {
		t.Fatal("malformed task should no longer be in ready-for-review/")
	}

	// Terminal failure marker should be appended.
	data, _ := os.ReadFile(filepath.Join(tasksDir, queue.DirFailed, "malformed.md"))
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
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)

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
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)

	content := "---\npriority: 10\nmax_retries: 3\n---\n# Valid Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "valid.md"), []byte(content), 0o644)

	if !hasReviewCandidates(tasksDir) {
		t.Fatal("expected true for valid review task")
	}
}

func TestHasReviewCandidates_MalformedTask_NoQuarantine(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, queue.DirFailed), 0o755)

	malformedPath := filepath.Join(tasksDir, queue.DirReadyReview, "malformed.md")
	os.WriteFile(malformedPath, []byte("---\npriority: [\n# Broken\n"), 0o644)

	if hasReviewCandidates(tasksDir) {
		t.Fatal("expected false when only malformed tasks exist")
	}

	// The malformed task must remain in ready-for-review/ (not quarantined).
	if _, err := os.Stat(malformedPath); err != nil {
		t.Fatalf("malformed task should remain in ready-for-review/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "malformed.md")); !os.IsNotExist(err) {
		t.Fatal("malformed task must not be moved to failed/ by read-only probe")
	}
}

func TestHasReviewCandidates_ExhaustedBudget_NoMove(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, queue.DirFailed), 0o755)

	content := "---\npriority: 10\nmax_retries: 3\n---\n# Exhausted Task\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — fail 2 -->\n" +
		"<!-- review-failure: a3 at 2026-01-03T00:00:00Z — fail 3 -->\n"
	exhaustedPath := filepath.Join(tasksDir, queue.DirReadyReview, "exhausted.md")
	os.WriteFile(exhaustedPath, []byte(content), 0o644)

	if hasReviewCandidates(tasksDir) {
		t.Fatal("expected false when only exhausted-budget tasks exist")
	}

	// The exhausted task must remain in ready-for-review/ (not moved).
	if _, err := os.Stat(exhaustedPath); err != nil {
		t.Fatalf("exhausted task should remain in ready-for-review/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "exhausted.md")); !os.IsNotExist(err) {
		t.Fatal("exhausted task must not be moved to failed/ by read-only probe")
	}
}

func TestHasReviewCandidates_MixedTasks_ReturnsTrueForValid(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, queue.DirFailed), 0o755)

	// Malformed task.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "bad.md"),
		[]byte("---\npriority: [\n# Broken\n"), 0o644)

	// Exhausted-budget task.
	exhaustedContent := "---\npriority: 10\nmax_retries: 2\n---\n# Exhausted\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — f1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — f2 -->\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "exhausted.md"),
		[]byte(exhaustedContent), 0o644)

	// Valid task with remaining budget.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "good.md"),
		[]byte("---\npriority: 5\nmax_retries: 3\n---\n# Good Task\n"), 0o644)

	if !hasReviewCandidates(tasksDir) {
		t.Fatal("expected true when at least one valid task exists among malformed/exhausted")
	}

	// Neither the malformed nor the exhausted task should be moved.
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyReview, "bad.md")); err != nil {
		t.Fatal("malformed task should remain in ready-for-review/")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyReview, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should remain in ready-for-review/")
	}
	failedEntries, _ := os.ReadDir(filepath.Join(tasksDir, queue.DirFailed))
	if len(failedEntries) != 0 {
		t.Fatalf("nothing should be moved to failed/, found %d entries", len(failedEntries))
	}
}

func TestReviewCandidates_Indexed_PrioritySort(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "low-pri.md"),
		[]byte("<!-- branch: task/low-pri -->\n---\npriority: 50\nmax_retries: 3\n---\n# Low Priority\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "high-pri.md"),
		[]byte("<!-- branch: task/high-pri -->\n---\npriority: 5\nmax_retries: 3\n---\n# High Priority\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "mid-pri.md"),
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
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "beta.md"),
		[]byte("<!-- branch: task/beta -->\n---\npriority: 10\nmax_retries: 3\n---\n# Beta\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "alpha.md"),
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
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	exhaustedContent := "---\npriority: 10\nmax_retries: 2\n---\n# Exhausted\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — fail 1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — fail 2 -->\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "exhausted.md"), []byte(exhaustedContent), 0o644)

	validContent := "<!-- branch: task/valid -->\n---\npriority: 20\nmax_retries: 3\n---\n# Valid\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "valid.md"), []byte(validContent), 0o644)

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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "exhausted.md")); err != nil {
		t.Fatal("exhausted task should be moved to failed/")
	}
}

func TestReviewCandidates_Indexed_BranchFromSnapshot(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "with-branch.md"),
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

func TestReviewCandidates_Indexed_BranchGeneratedWhenMissing(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "no-branch.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# No Branch\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	expected := "task/" + frontmatter.SanitizeBranchName("no-branch.md")
	if candidates[0].Branch != expected {
		t.Fatalf("expected generated branch %q, got %q", expected, candidates[0].Branch)
	}
}

func TestReviewCandidates_Indexed_TitleExtracted(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "titled.md"),
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
	state, err := taskstate.Load(taskDir, task.Filename)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("missing branch should record taskstate")
	}
	if state.LastOutcome != "review-branch-missing" {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, "review-branch-missing")
	}
}

func TestReviewCandidates_Indexed_MalformedQuarantined(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed, queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Write a malformed task.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "malformed.md"),
		[]byte("---\npriority: [oops\n# Malformed\n"), 0o644)

	// Write a valid task.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "good.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Good Task\n"), 0o644)

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
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "malformed.md")); err != nil {
		t.Fatal("malformed task should be moved to failed/")
	}

	// Terminal failure marker should be appended.
	data, _ := os.ReadFile(filepath.Join(tasksDir, queue.DirFailed, "malformed.md"))
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
	state := &taskstate.TaskState{LastReviewedSHA: "def456"}
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
	taskPath := filepath.Join(tasksDir, queue.DirReadyReview, "task.md")
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
	if err := taskstate.Update(tasksDir, "task.md", func(state *taskstate.TaskState) {
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
	state, err := taskstate.Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state.LastReviewedSHA != "older-sha" {
		t.Fatalf("LastReviewedSHA = %q, want %q", state.LastReviewedSHA, "older-sha")
	}
	if state.LastOutcome != "review-launched" {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, "review-launched")
	}
	session, err := sessionmeta.Load(tasksDir, sessionmeta.KindReview, "task.md")
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
	taskPath := filepath.Join(tasksDir, queue.DirReadyReview, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/task-new"); err != nil {
		t.Fatalf("create review branch: %v", err)
	}

	// Seed a review session on the OLD branch with a known session ID.
	oldSessionID := "stale-review-session-from-old-branch"
	if err := sessionmeta.Update(tasksDir, sessionmeta.KindReview, "task.md", func(session *sessionmeta.Session) {
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
	session, err := sessionmeta.Load(tasksDir, sessionmeta.KindReview, "task.md")
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
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	taskFile := "approved-state.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if err := os.WriteFile(reviewPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := taskstate.Update(tasksDir, taskFile, func(state *taskstate.TaskState) {
		state.LastHeadSHA = "current-tip"
		state.LastReviewedSHA = "older-tip"
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{"verdict":"approve"}`), 0o644); err != nil {
		t.Fatalf("WriteFile verdict: %v", err)
	}

	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/approved-state", Title: "Approved", TaskPath: reviewPath})

	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to exist")
	}
	if state.LastReviewedSHA != "current-tip" {
		t.Fatalf("LastReviewedSHA = %q, want %q", state.LastReviewedSHA, "current-tip")
	}
	if state.LastOutcome != "review-approved" {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, "review-approved")
	}
}

func TestPostReviewAction_RejectedUpdatesLastReviewedSHA(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	taskFile := "rejected-state.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if err := os.WriteFile(reviewPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := taskstate.Update(tasksDir, taskFile, func(state *taskstate.TaskState) {
		state.LastHeadSHA = "rejected-tip"
		state.LastReviewedSHA = "older-tip"
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json"), []byte(`{"verdict":"reject","reason":"missing tests"}`), 0o644); err != nil {
		t.Fatalf("WriteFile verdict: %v", err)
	}

	postReviewAction(tasksDir, "host-agent", &queue.ClaimedTask{Filename: taskFile, Branch: "task/rejected-state", Title: "Rejected", TaskPath: reviewPath})

	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to exist")
	}
	if state.LastReviewedSHA != "rejected-tip" {
		t.Fatalf("LastReviewedSHA = %q, want %q", state.LastReviewedSHA, "rejected-tip")
	}
	if state.LastOutcome != "review-rejected" {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, "review-rejected")
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
	taskPath := filepath.Join(tasksDir, queue.DirReadyReview, "task.md")
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
	session, err := sessionmeta.Load(tasksDir, sessionmeta.KindReview, "task.md")
	if err != nil {
		t.Fatalf("Load review session: %v", err)
	}
	if session != nil {
		t.Fatalf("expected no review session when disabled, got %+v", session)
	}
}

func TestRunReview_CloneFailureDoesNotCreateReviewSessionOrLaunchState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge, queue.DirCompleted, queue.DirFailed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", sub, err)
		}
	}
	taskPath := filepath.Join(tasksDir, queue.DirReadyReview, "task.md")
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
	state, err := taskstate.Load(tasksDir, "task.md")
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should not be created on clone failure, got %+v", state)
	}
	session, err := sessionmeta.Load(tasksDir, sessionmeta.KindReview, "task.md")
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

func TestReviewCandidates_FilesystemFallback_DedupsSynthesizedBranches(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirFailed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Two filenames that sanitize to the same branch name: "add_feature" and "add-feature".
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add_feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Underscore\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add-feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Dash\n"), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	// Both should have the same sanitized base ("add-feature") but the
	// second one in filename order must be disambiguated.
	if candidates[0].Branch == candidates[1].Branch {
		t.Fatalf("two candidates share the same synthesized branch %q; batch dedup should have prevented this", candidates[0].Branch)
	}

	// Verify both start with the expected prefix.
	for i, c := range candidates {
		if !strings.HasPrefix(c.Branch, "task/add-feature") {
			t.Fatalf("candidate[%d] branch %q does not start with expected prefix", i, c.Branch)
		}
	}
}

func TestReviewCandidates_Indexed_DedupsSynthesizedBranches(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Two filenames that sanitize to the same branch name.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add_feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Underscore\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add-feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Add Feature Dash\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	if candidates[0].Branch == candidates[1].Branch {
		t.Fatalf("two candidates share the same synthesized branch %q; batch dedup should have prevented this", candidates[0].Branch)
	}

	for i, c := range candidates {
		if !strings.HasPrefix(c.Branch, "task/add-feature") {
			t.Fatalf("candidate[%d] branch %q does not start with expected prefix", i, c.Branch)
		}
	}
}

func TestReviewCandidates_ExplicitBranchUnchanged(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// One task with an explicit branch, one without (same sanitized base).
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add-feature.md"),
		[]byte("<!-- branch: task/add-feature -->\n---\npriority: 10\nmax_retries: 3\n---\n# Explicit\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "add_feature.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# Synthesized\n"), 0o644)

	idx := queue.BuildIndex(tasksDir)
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
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
}

func TestPostReviewAction_ApproveMoveFails_NoMarkerVerdictPreserved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "move-fail-approve.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Move Fail Approve\n"), 0o644)

	// Pre-create a file at the destination to cause the move to fail.
	dstPath := filepath.Join(tasksDir, queue.DirReadyMerge, taskFile)
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
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-move-failed" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-move-failed", state)
	}
}

func TestPostReviewAction_RejectMoveFails_NoMarkerVerdictPreserved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "move-fail-reject.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Move Fail Reject\n"), 0o644)

	// Pre-create a file at the destination to cause the move to fail.
	dstPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
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
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-move-failed" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-move-failed", state)
	}
}

func TestPostReviewAction_FallbackApproveMoveFails_VerdictNotLost(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "fallback-move-fail.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("# Fallback\n<!-- reviewed: review-agent at 2026-01-01T00:00:00Z — approved -->\n"), 0o644)

	// Pre-create destination to cause move failure.
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyMerge, taskFile), []byte("# Existing\n"), 0o644)

	// No verdict file — backward compat path.
	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/fallback-move-fail",
		Title:    "Fallback Move Fail",
		TaskPath: reviewPath,
	}

	captureStdoutStderr(t, func() {
		postReviewAction(tasksDir, "host-agent", task)
	})

	// Task should remain in ready-for-review/.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task should stay in ready-for-review/ when move fails")
	}
	srcData, _ := os.ReadFile(reviewPath)
	// Review-failure should be recorded.
	if !strings.Contains(string(srcData), "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded when backward-compat move fails:\n%s", string(srcData))
	}
	// Terminal approval marker should be stripped (rollback-safe).
	if reviewedRe.Match(srcData) {
		t.Fatalf("terminal approval marker should be stripped when move fails:\n%s", string(srcData))
	}
	// TaskState should reflect the move failure.
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-move-failed" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-move-failed", state)
	}
}

func TestPostReviewAction_FallbackRejectMoveFails_MarkerStripped(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "fallback-reject-move-fail.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	// Include an older rejection from a prior cycle, then the terminal one.
	os.WriteFile(reviewPath, []byte("# Fallback Reject\n"+
		"<!-- review-rejection: old-agent at 2025-12-01T00:00:00Z — older feedback -->\n"+
		"<!-- review-rejection: review-agent at 2026-01-01T00:00:00Z — tests missing -->\n"), 0o644)

	// Pre-create destination to cause move failure.
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, taskFile), []byte("# Existing\n"), 0o644)

	// No verdict file — backward compat path.
	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/fallback-reject-move-fail",
		Title:    "Fallback Reject Move Fail",
		TaskPath: reviewPath,
	}

	captureStdoutStderr(t, func() {
		postReviewAction(tasksDir, "host-agent", task)
	})

	// Task should remain in ready-for-review/.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task should stay in ready-for-review/ when move fails")
	}
	srcData, _ := os.ReadFile(reviewPath)
	content := string(srcData)
	// Review-failure should be recorded.
	if !strings.Contains(content, "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded when backward-compat move fails:\n%s", content)
	}
	// The terminal (last) rejection marker should be stripped.
	if strings.Contains(content, "tests missing") {
		t.Fatalf("terminal rejection marker should be stripped when move fails:\n%s", content)
	}
	// The older rejection marker must be preserved for MATO_REVIEW_FEEDBACK.
	if !strings.Contains(content, "older feedback") {
		t.Fatalf("older rejection marker should be preserved:\n%s", content)
	}
	// TaskState should reflect the move failure.
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-move-failed" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-move-failed", state)
	}
}

func TestMoveReviewedTask_MoveFails_RecordsReviewFailure(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, queue.DirReadyReview)
	dstDir := filepath.Join(tasksDir, queue.DirReadyMerge)
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
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "review-move-failed" {
		t.Fatalf("taskstate = %+v, want LastOutcome=review-move-failed", state)
	}
}

func TestStripLastTerminalMarker_Approval(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantGone []string
		wantKept []string
	}{
		{
			name:     "strips single approval marker",
			input:    "# Task\n<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->\nsome body\n",
			wantGone: []string{"<!-- reviewed:"},
			wantKept: []string{"# Task", "some body"},
		},
		{
			name:     "preserves review-failure markers",
			input:    "# Task\n<!-- review-failure: agent1 at 2026-01-01T00:00:00Z — move failed -->\n<!-- reviewed: agent1 at 2026-02-01T00:00:00Z — approved -->\n",
			wantGone: []string{"<!-- reviewed:"},
			wantKept: []string{"# Task", "<!-- review-failure:"},
		},
		{
			name:     "preserves rejection markers when stripping approval",
			input:    "# Task\n<!-- review-rejection: a1 at 2026-01-01T00:00:00Z — old feedback -->\n<!-- reviewed: a2 at 2026-02-01T00:00:00Z — approved -->\n",
			wantGone: []string{"<!-- reviewed:"},
			wantKept: []string{"# Task", "<!-- review-rejection:", "old feedback"},
		},
		{
			name:     "no markers leaves file unchanged",
			input:    "# Task\nsome body\n",
			wantGone: nil,
			wantKept: []string{"# Task", "some body"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "task.md")
			os.WriteFile(path, []byte(tt.input), 0o644)

			stripLastTerminalMarker(path, approvalMarkerRe)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			content := string(data)
			for _, s := range tt.wantGone {
				if strings.Contains(content, s) {
					t.Errorf("marker %q should have been stripped from:\n%s", s, content)
				}
			}
			for _, s := range tt.wantKept {
				if !strings.Contains(content, s) {
					t.Errorf("content %q should be preserved in:\n%s", s, content)
				}
			}
		})
	}
}

func TestStripLastTerminalMarker_Rejection_PreservesOlderRejections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	input := "# Task\n<!-- review-rejection: a1 at 2026-01-01T00:00:00Z — first feedback -->\n<!-- review-rejection: a2 at 2026-02-01T00:00:00Z — second feedback -->\n"
	os.WriteFile(path, []byte(input), 0o644)

	stripLastTerminalMarker(path, rejectionMarkerRe)

	data, _ := os.ReadFile(path)
	content := string(data)

	// The last (second) rejection marker should be removed.
	if strings.Contains(content, "second feedback") {
		t.Fatalf("last rejection marker should be stripped, got:\n%s", content)
	}
	// The first (older) rejection marker must be preserved.
	if !strings.Contains(content, "first feedback") {
		t.Fatalf("older rejection marker should be preserved, got:\n%s", content)
	}
}

func TestStripLastTerminalMarker_Rejection_SingleMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	input := "# Task\n<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — tests missing -->\nsome body\n"
	os.WriteFile(path, []byte(input), 0o644)

	stripLastTerminalMarker(path, rejectionMarkerRe)

	data, _ := os.ReadFile(path)
	content := string(data)
	if strings.Contains(content, "<!-- review-rejection:") {
		t.Fatalf("rejection marker should be stripped, got:\n%s", content)
	}
	if !strings.Contains(content, "# Task") || !strings.Contains(content, "some body") {
		t.Fatalf("non-marker content should be preserved, got:\n%s", content)
	}
}
