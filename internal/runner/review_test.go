package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/queue"
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
