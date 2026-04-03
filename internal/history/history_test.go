package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/testutil"
	"mato/internal/ui"
)

func TestShowTo_TextMixedHistoryNewestFirst(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirFailed, "cleanup-reconcile.md", "# Cleanup reconcile\n\n<!-- failure: worker-a at 2026-03-20T17:55:31Z step=WORK error=tests_failed -->\n")
	writeTask(t, tasksDir, queue.DirBacklog, "tighten-review-tests.md", "# Tighten review tests\n\n<!-- review-rejection: reviewer-a at 2026-03-20T18:12:04Z — missing integration coverage -->\n")
	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:       "add-log-command",
		TaskFile:     "add-log-command.md",
		Branch:       "task/add-log-command",
		CommitSHA:    "3f1c9a2abcd1234",
		FilesChanged: []string{"cmd/mato/main.go"},
		Title:        "Add log command",
		MergedAt:     mustParseTime(t, "2026-03-20T18:41:10Z"),
	})

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "2026-03-20T18:41:10Z  MERGED") || !strings.Contains(lines[0], "3f1c9a2") || !strings.Contains(lines[0], "task/add-log-command") {
		t.Fatalf("unexpected merged line: %q", lines[0])
	}
	if !strings.Contains(lines[1], "2026-03-20T18:12:04Z  REJECTED") || !strings.Contains(lines[1], "missing integration coverage") {
		t.Fatalf("unexpected rejected line: %q", lines[1])
	}
	if !strings.Contains(lines[2], "2026-03-20T17:55:31Z  FAILED") || !strings.Contains(lines[2], "tests_failed") {
		t.Fatalf("unexpected failed line: %q", lines[2])
	}
}

func TestShowTo_JSONLimit(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirBacklog, "first.md", "# First\n\n<!-- review-rejection: reviewer-a at 2026-03-20T10:00:00Z — first -->\n")
	writeTask(t, tasksDir, queue.DirFailed, "second.md", "# Second\n\n<!-- failure: worker-a at 2026-03-20T11:00:00Z step=WORK error=second -->\n")
	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "third",
		TaskFile:  "third.md",
		CommitSHA: "abcdef1234567890",
		MergedAt:  mustParseTime(t, "2026-03-20T12:00:00Z"),
	})

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 2, "json"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	var events []Event
	if err := json.Unmarshal(buf.Bytes(), &events); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, buf.String())
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Type != "MERGED" || events[0].TaskFile != "third.md" {
		t.Fatalf("events[0] = %#v, want newest merged event", events[0])
	}
	if events[1].Type != "FAILED" || events[1].TaskFile != "second.md" {
		t.Fatalf("events[1] = %#v, want second newest failed event", events[1])
	}
}

func TestShowTo_EmptyHistory(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)

	var text bytes.Buffer
	if err := ShowTo(&text, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo text: %v", err)
	}
	if text.String() != "(no history)\n" {
		t.Fatalf("text = %q, want %q", text.String(), "(no history)\n")
	}

	var jsonBuf bytes.Buffer
	if err := ShowTo(&jsonBuf, repoRoot, 20, "json"); err != nil {
		t.Fatalf("ShowTo json: %v", err)
	}
	if strings.TrimSpace(jsonBuf.String()) != "[]" {
		t.Fatalf("json = %q, want []", jsonBuf.String())
	}
}

func TestShowTo_EmptyHistoryWithoutCompletionsDir_UsesJSONArray(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.RemoveAll(filepath.Join(tasksDir, "messages", "completions")); err != nil {
		t.Fatalf("os.RemoveAll(completions): %v", err)
	}

	var jsonBuf bytes.Buffer
	if err := ShowTo(&jsonBuf, repoRoot, 20, "json"); err != nil {
		t.Fatalf("ShowTo json: %v", err)
	}
	if strings.TrimSpace(jsonBuf.String()) != "[]" {
		t.Fatalf("json = %q, want []", jsonBuf.String())
	}
}

func TestShowTo_WarnsAndSkipsMalformedCompletionAndUnreadableTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	testutil.WriteFile(t, filepath.Join(tasksDir, "messages", "completions", "broken.json"), "{not json")
	testutil.WriteFile(t, filepath.Join(tasksDir, "messages", "completions", "missing-fields.json"), `{"task_id":"missing-fields"}`)
	writeTask(t, tasksDir, queue.DirBacklog, "good.md", "# Good\n\n<!-- review-rejection: reviewer-a at 2026-03-20T10:00:00Z — useful feedback -->\n")
	writeTask(t, tasksDir, queue.DirFailed, "unreadable.md", "# Unreadable\n\n<!-- failure: worker-a at 2026-03-20T09:00:00Z step=WORK error=hidden -->\n")

	unreadablePath := filepath.Join(tasksDir, queue.DirFailed, "unreadable.md")
	if err := os.Chmod(unreadablePath, 0o000); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}
	defer os.Chmod(unreadablePath, 0o644)

	var warnings bytes.Buffer
	prev := ui.SetWarningWriter(&warnings)
	defer ui.SetWarningWriter(prev)

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	if !strings.Contains(buf.String(), "good.md") {
		t.Fatalf("expected readable task event, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "unreadable.md") {
		t.Fatalf("unexpected unreadable task event in output:\n%s", buf.String())
	}
	if !strings.Contains(warnings.String(), "could not parse completion detail broken.json") {
		t.Fatalf("expected malformed completion warning, got %q", warnings.String())
	}
	if !strings.Contains(warnings.String(), "could not parse completion detail missing-fields.json: missing merged_at") {
		t.Fatalf("expected missing-fields completion warning, got %q", warnings.String())
	}
}

func TestShowTo_FailsWhenNoSourceReadableAndOneSourceFails(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	for _, dir := range queue.AllDirs {
		if err := os.RemoveAll(filepath.Join(tasksDir, dir)); err != nil {
			t.Fatalf("os.RemoveAll(%s): %v", dir, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(tasksDir, "messages", "completions")); err != nil {
		t.Fatalf("os.RemoveAll(completions): %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "messages", "completions"), []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completions: %v", err)
	}

	var buf bytes.Buffer
	err := ShowTo(&buf, repoRoot, 20, "text")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "read completions dir") {
		t.Fatalf("error = %v, want completions read error", err)
	}
}

func TestShowTo_WarnsWhenCompletionSourceFailsButTaskSourceSucceeds(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Replace the completions directory with a regular file so ReadDir fails.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	if err := os.RemoveAll(completionsDir); err != nil {
		t.Fatalf("os.RemoveAll(completions): %v", err)
	}
	if err := os.WriteFile(completionsDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completions: %v", err)
	}

	writeTask(t, tasksDir, queue.DirFailed, "good-task.md",
		"# Good task\n\n<!-- failure: worker-a at 2026-03-20T10:00:00Z step=WORK error=build_failed -->\n")

	var warnings bytes.Buffer
	prev := ui.SetWarningWriter(&warnings)
	defer ui.SetWarningWriter(prev)

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo should not error when one source succeeds: %v", err)
	}
	if !strings.Contains(buf.String(), "good-task.md") {
		t.Fatalf("expected task event in output, got:\n%s", buf.String())
	}
	if !strings.Contains(warnings.String(), "read completions dir") {
		t.Fatalf("expected warning about completions dir failure, got: %q", warnings.String())
	}
}

func TestShowTo_JSONPartialFailureWarnsToStderr(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	if err := os.RemoveAll(completionsDir); err != nil {
		t.Fatalf("os.RemoveAll(completions): %v", err)
	}
	if err := os.WriteFile(completionsDir, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("os.WriteFile completions: %v", err)
	}

	writeTask(t, tasksDir, queue.DirFailed, "good-task.md",
		"# Good task\n\n<!-- failure: worker-a at 2026-03-20T10:00:00Z step=WORK error=build_failed -->\n")

	var warnings bytes.Buffer
	prev := ui.SetWarningWriter(&warnings)
	defer ui.SetWarningWriter(prev)

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "json"); err != nil {
		t.Fatalf("ShowTo should not error: %v", err)
	}

	var events []Event
	if err := json.Unmarshal(buf.Bytes(), &events); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, buf.String())
	}
	if len(events) != 1 || events[0].TaskFile != "good-task.md" {
		t.Fatalf("expected one task event, got: %v", events)
	}
	if !strings.Contains(warnings.String(), "read completions dir") {
		t.Fatalf("expected warning about completions dir on stderr, got: %q", warnings.String())
	}
}

func TestShowTo_WarnsWhenTaskSourceFailsButCompletionSourceSucceeds(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Remove all queue directories and replace them with regular files.
	for _, dir := range queue.AllDirs {
		dirPath := filepath.Join(tasksDir, dir)
		if err := os.RemoveAll(dirPath); err != nil {
			t.Fatalf("os.RemoveAll(%s): %v", dir, err)
		}
		if err := os.WriteFile(dirPath, []byte("not a dir"), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", dir, err)
		}
	}

	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "merged-task",
		TaskFile:  "merged-task.md",
		CommitSHA: "abc1234567890",
		MergedAt:  mustParseTime(t, "2026-03-20T12:00:00Z"),
	})

	var warnings bytes.Buffer
	prev := ui.SetWarningWriter(&warnings)
	defer ui.SetWarningWriter(prev)

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo should not error when completions source succeeds: %v", err)
	}
	if !strings.Contains(buf.String(), "merged-task.md") {
		t.Fatalf("expected merged event in output, got:\n%s", buf.String())
	}
	if !strings.Contains(warnings.String(), "read queue directory") {
		t.Fatalf("expected warning about queue directory failure, got: %q", warnings.String())
	}
}

func writeTask(t *testing.T, tasksDir, dir, name, content string) {
	t.Helper()
	testutil.WriteFile(t, filepath.Join(tasksDir, dir, name), content)
}

func writeCompletion(t *testing.T, tasksDir string, detail messaging.CompletionDetail) {
	t.Helper()
	if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", value, err)
	}
	return ts
}

func TestShowTo_TextRelativeTimeAnnotation(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Use recent timestamps so the relative time annotation appears.
	recentMerged := time.Now().UTC().Add(-5 * time.Minute)
	recentFailed := time.Now().UTC().Add(-2 * time.Hour)
	recentMergedStr := recentMerged.Format(time.RFC3339)
	recentFailedStr := recentFailed.Format(time.RFC3339)

	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "recent-merged",
		TaskFile:  "recent-merged.md",
		Branch:    "task/recent-merged",
		CommitSHA: "abc1234567890",
		Title:     "Recent merge",
		MergedAt:  recentMerged,
	})
	writeTask(t, tasksDir, queue.DirFailed, "recent-failed.md",
		"# Recent failed\n\n<!-- failure: worker-a at "+recentFailedStr+" step=WORK error=build_broken -->\n")

	// Text output should include both the absolute timestamp and [X ago].
	var textBuf bytes.Buffer
	if err := ShowTo(&textBuf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo text: %v", err)
	}
	textOutput := textBuf.String()

	if !strings.Contains(textOutput, recentMergedStr) {
		t.Errorf("text output should contain absolute merged timestamp %s, got:\n%s", recentMergedStr, textOutput)
	}
	if !strings.Contains(textOutput, recentFailedStr) {
		t.Errorf("text output should contain absolute failed timestamp %s, got:\n%s", recentFailedStr, textOutput)
	}
	if !strings.Contains(textOutput, "ago]") {
		t.Errorf("text output should contain relative time annotation [X ago], got:\n%s", textOutput)
	}

	// JSON output must NOT contain relative time annotations.
	var jsonBuf bytes.Buffer
	if err := ShowTo(&jsonBuf, repoRoot, 20, "json"); err != nil {
		t.Fatalf("ShowTo json: %v", err)
	}
	jsonOutput := jsonBuf.String()

	if strings.Contains(jsonOutput, "ago") {
		t.Errorf("JSON output must not contain relative time annotation, got:\n%s", jsonOutput)
	}

	// Verify JSON timestamps are pure RFC3339.
	var events []Event
	if err := json.Unmarshal(jsonBuf.Bytes(), &events); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, jsonBuf.String())
	}
	for _, ev := range events {
		ts := ev.Timestamp.UTC().Format(time.RFC3339)
		if strings.Contains(ts, "ago") {
			t.Errorf("JSON event timestamp should be pure RFC3339, got: %q", ts)
		}
	}
}

func TestRenderText_NoColorFallback(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirFailed, "broken.md",
		"# Broken\n\n<!-- failure: worker-a at 2026-03-20T17:55:31Z step=WORK error=tests_failed -->\n")
	writeTask(t, tasksDir, queue.DirBacklog, "rejected.md",
		"# Rejected\n\n<!-- review-rejection: reviewer-a at 2026-03-20T18:12:04Z — missing coverage -->\n")
	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "merged-task",
		TaskFile:  "merged-task.md",
		Branch:    "task/merged-task",
		CommitSHA: "abc1234567890",
		Title:     "Merged task",
		MergedAt:  mustParseTime(t, "2026-03-20T18:41:10Z"),
	})

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), output)
	}

	// Verify all event types are present and readable in no-color mode.
	if !strings.Contains(output, "MERGED") {
		t.Errorf("missing MERGED event type in output:\n%s", output)
	}
	if !strings.Contains(output, "FAILED") {
		t.Errorf("missing FAILED event type in output:\n%s", output)
	}
	if !strings.Contains(output, "REJECTED") {
		t.Errorf("missing REJECTED event type in output:\n%s", output)
	}
	// Verify detail fields are intact.
	if !strings.Contains(output, "abc1234") {
		t.Errorf("missing commit SHA in output:\n%s", output)
	}
	if !strings.Contains(output, "tests_failed") {
		t.Errorf("missing failure reason in output:\n%s", output)
	}
	if !strings.Contains(output, "missing coverage") {
		t.Errorf("missing rejection reason in output:\n%s", output)
	}
}

func TestShowTo_TextNarrowTerminalTruncates(t *testing.T) {
	prev := ui.SetTermWidthFunc(func() int { return 60 })
	defer ui.SetTermWidthFunc(prev)

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "very-long-task-name-that-exceeds-normal-width",
		TaskFile:  "very-long-task-name-that-exceeds-normal-width.md",
		Branch:    "task/very-long-task-name-that-exceeds-normal-width",
		CommitSHA: "abc1234567890",
		Title:     "A task with a very long name",
		MergedAt:  mustParseTime(t, "2026-03-20T18:41:10Z"),
	})

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d:\n%s", len(lines), output)
	}

	if !strings.Contains(output, "…") {
		t.Errorf("expected truncation marker in narrow output, got:\n%s", output)
	}
	if !strings.Contains(output, "MERGED") {
		t.Errorf("missing MERGED type in narrow output:\n%s", output)
	}
}

func TestShowTo_TextVeryNarrowTerminalTruncates(t *testing.T) {
	const termW = 20
	prev := ui.SetTermWidthFunc(func() int { return termW })
	defer ui.SetTermWidthFunc(prev)

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "very-long-task-name-that-exceeds-normal-width",
		TaskFile:  "very-long-task-name-that-exceeds-normal-width.md",
		Branch:    "task/very-long-task-name-that-exceeds-normal-width",
		CommitSHA: "abc1234567890",
		Title:     "A task with a very long name",
		MergedAt:  mustParseTime(t, "2026-03-20T18:41:10Z"),
	})

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d:\n%s", len(lines), output)
	}

	// At width=20, the fixed prefix (timestamp + type) exceeds the budget,
	// so secondary fields (task name, detail) must be dropped entirely.
	if strings.Contains(output, "very-long-task") {
		t.Errorf("task name should be dropped at very narrow width, got:\n%s", output)
	}
	if strings.Contains(output, "abc1234") {
		t.Errorf("detail (SHA) should be dropped at very narrow width, got:\n%s", output)
	}

	// Verify every data line fits within the configured terminal width.
	for _, line := range lines {
		if utf8.RuneCountInString(line) > termW {
			t.Errorf("line exceeds terminal width %d: runes=%d, line=%q", termW, utf8.RuneCountInString(line), line)
		}
	}
}

func TestShowTo_TextNarrowTerminalFitsWidth(t *testing.T) {
	const termW = 40
	prev := ui.SetTermWidthFunc(func() int { return termW })
	defer ui.SetTermWidthFunc(prev)

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeCompletion(t, tasksDir, messaging.CompletionDetail{
		TaskID:    "long-task-name-for-narrow-test",
		TaskFile:  "long-task-name-for-narrow-test.md",
		Branch:    "task/long-task-name-for-narrow-test",
		CommitSHA: "abc1234567890",
		Title:     "Long task title",
		MergedAt:  mustParseTime(t, "2026-03-20T18:41:10Z"),
	})
	writeTask(t, tasksDir, queue.DirFailed, "another-long-named-task.md",
		"# Another long named task\n\n<!-- failure: worker-a at 2026-03-20T17:55:31Z step=WORK error=tests_failed_with_long_reason -->\n")

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for _, line := range lines {
		if utf8.RuneCountInString(line) > termW {
			t.Errorf("line exceeds terminal width %d: runes=%d, line=%q", termW, utf8.RuneCountInString(line), line)
		}
	}
}

func writeVerdictFile(t *testing.T, tasksDir, filename string, verdict map[string]string) {
	t.Helper()
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := json.Marshal(verdict)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(msgDir, "verdict-"+filename+".json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestShowTo_VerdictFallbackShowsRejectedEvent(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Task with no review-rejection markers.
	writeTask(t, tasksDir, queue.DirBacklog, "rework.md", "# Needs Rework\n")
	// Preserved verdict file.
	writeVerdictFile(t, tasksDir, "rework.md", map[string]string{
		"verdict": "reject",
		"reason":  "missing test coverage",
	})

	var textBuf bytes.Buffer
	if err := ShowTo(&textBuf, repoRoot, 20, "text"); err != nil {
		t.Fatalf("ShowTo text: %v", err)
	}
	if !strings.Contains(textBuf.String(), "REJECTED") {
		t.Fatalf("expected REJECTED event in text output:\n%s", textBuf.String())
	}
	if !strings.Contains(textBuf.String(), "missing test coverage") {
		t.Fatalf("expected rejection reason in text output:\n%s", textBuf.String())
	}

	var jsonBuf bytes.Buffer
	if err := ShowTo(&jsonBuf, repoRoot, 20, "json"); err != nil {
		t.Fatalf("ShowTo json: %v", err)
	}
	var events []Event
	if err := json.Unmarshal(jsonBuf.Bytes(), &events); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, jsonBuf.String())
	}
	found := false
	for _, ev := range events {
		if ev.Type == "REJECTED" && ev.Reason == "missing test coverage" && ev.TaskFile == "rework.md" {
			found = true
			if ev.Timestamp.IsZero() {
				t.Error("expected non-zero timestamp from verdict file mod time")
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected REJECTED event with verdict reason, got events: %+v", events)
	}
}

func TestShowTo_VerdictFallbackNotUsedWhenMarkersExist(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Task with a durable rejection marker.
	writeTask(t, tasksDir, queue.DirBacklog, "marked.md",
		"# Marked\n\n<!-- review-rejection: reviewer at 2026-03-20T10:00:00Z — durable reason -->\n")
	// Verdict file with different reason.
	writeVerdictFile(t, tasksDir, "marked.md", map[string]string{
		"verdict": "reject",
		"reason":  "verdict only reason",
	})

	var jsonBuf bytes.Buffer
	if err := ShowTo(&jsonBuf, repoRoot, 20, "json"); err != nil {
		t.Fatalf("ShowTo json: %v", err)
	}
	var events []Event
	if err := json.Unmarshal(jsonBuf.Bytes(), &events); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	rejectedCount := 0
	for _, ev := range events {
		if ev.Type == "REJECTED" {
			rejectedCount++
			if ev.Reason != "durable reason" {
				t.Errorf("expected durable marker reason, got %q", ev.Reason)
			}
		}
	}
	if rejectedCount != 1 {
		t.Fatalf("expected exactly 1 REJECTED event from durable marker, got %d", rejectedCount)
	}
}

func TestShowTo_VerdictFallbackClearedAfterRetry(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	filename := "retry-clears-log-verdict.md"

	// Task in failed/ with no durable rejection marker, plus stale verdict.
	writeTask(t, tasksDir, queue.DirFailed, filename,
		"# Retry Clears Log Verdict\n\n<!-- failure: abc at 2026-03-20T10:00:00Z step=WORK error=build -->\n")
	writeVerdictFile(t, tasksDir, filename, map[string]string{
		"verdict": "reject",
		"reason":  "stale log reason",
	})

	// Before retry: history should include a REJECTED event from verdict.
	var beforeBuf bytes.Buffer
	if err := ShowTo(&beforeBuf, repoRoot, 100, "text"); err != nil {
		t.Fatalf("ShowTo before retry: %v", err)
	}
	if !strings.Contains(beforeBuf.String(), "stale log reason") {
		t.Fatalf("before retry: expected verdict reason in log output:\n%s", beforeBuf.String())
	}

	// Perform retry (reset transition).
	if _, err := queue.RetryTask(tasksDir, filename); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}

	// After retry: history must not surface the stale verdict reason.
	var afterBuf bytes.Buffer
	if err := ShowTo(&afterBuf, repoRoot, 100, "text"); err != nil {
		t.Fatalf("ShowTo after retry: %v", err)
	}
	if strings.Contains(afterBuf.String(), "stale log reason") {
		t.Fatalf("after retry: stale verdict reason should not appear in log:\n%s", afterBuf.String())
	}
}

func TestShowTo_VerdictFallbackClearedAfterCancel(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	filename := "cancel-clears-log-verdict.md"

	// Task in backlog/ with no durable rejection marker, plus stale verdict.
	writeTask(t, tasksDir, queue.DirBacklog, filename,
		"# Cancel Clears Log Verdict\n")
	writeVerdictFile(t, tasksDir, filename, map[string]string{
		"verdict": "reject",
		"reason":  "terminal log reason",
	})

	// Before cancel: history should include a REJECTED event from verdict.
	var beforeBuf bytes.Buffer
	if err := ShowTo(&beforeBuf, repoRoot, 100, "text"); err != nil {
		t.Fatalf("ShowTo before cancel: %v", err)
	}
	if !strings.Contains(beforeBuf.String(), "terminal log reason") {
		t.Fatalf("before cancel: expected verdict reason in log output:\n%s", beforeBuf.String())
	}

	// Cancel the task (terminal transition).
	if _, err := queue.CancelTask(tasksDir, filename); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}

	// After cancel: history must not surface the stale verdict reason.
	var afterBuf bytes.Buffer
	if err := ShowTo(&afterBuf, repoRoot, 100, "text"); err != nil {
		t.Fatalf("ShowTo after cancel: %v", err)
	}
	if strings.Contains(afterBuf.String(), "terminal log reason") {
		t.Fatalf("after cancel: stale verdict reason should not appear in log:\n%s", afterBuf.String())
	}
}
