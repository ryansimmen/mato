package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/testutil"
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
	origWarnings := warningWriter
	warningWriter = &warnings
	defer func() { warningWriter = origWarnings }()

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
	origWarnings := warningWriter
	warningWriter = &warnings
	defer func() { warningWriter = origWarnings }()

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
	origWarnings := warningWriter
	warningWriter = &warnings
	defer func() { warningWriter = origWarnings }()

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
	origWarnings := warningWriter
	warningWriter = &warnings
	defer func() { warningWriter = origWarnings }()

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
