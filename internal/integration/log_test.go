package integration_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/dirs"
	"mato/internal/messaging"
	"mato/internal/testutil"
)

func TestLogCommand_MergedTaskAppears(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := messaging.WriteCompletionDetail(tasksDir, messaging.CompletionDetail{
		TaskID:    "add-log-command",
		TaskFile:  "add-log-command.md",
		Branch:    "task/add-log-command",
		CommitSHA: "3f1c9a2abcd1234",
		Title:     "Add log command",
		MergedAt:  mustParseRFC3339(t, "2026-03-20T18:41:10Z"),
	}); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	out, err := runMatoCommand(t, "log", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato log failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "MERGED") || !strings.Contains(out, "add-log-command.md") || !strings.Contains(out, "task/add-log-command") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestLogCommand_FailedTaskAppears(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	testutil.WriteFile(t, filepath.Join(tasksDir, dirs.Failed, "cleanup-reconcile.md"), "# Cleanup reconcile\n\n<!-- failure: worker-a at 2026-03-20T17:55:31Z step=WORK error=tests_failed -->\n")

	out, err := runMatoCommand(t, "log", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato log failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "FAILED") || !strings.Contains(out, "cleanup-reconcile.md") || !strings.Contains(out, "tests_failed") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestLogCommand_RejectedTaskAppears(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	testutil.WriteFile(t, filepath.Join(tasksDir, dirs.Backlog, "tighten-review-tests.md"), "# Tighten review tests\n\n<!-- review-rejection: reviewer-a at 2026-03-20T18:12:04Z — missing integration coverage -->\n")

	out, err := runMatoCommand(t, "log", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato log failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "REJECTED") || !strings.Contains(out, "tighten-review-tests.md") || !strings.Contains(out, "missing integration coverage") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestLogCommand_EmptyHistory(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)

	out, err := runMatoCommand(t, "log", "--repo", repoRoot)
	if err != nil {
		t.Fatalf("mato log failed: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "(no history)" {
		t.Fatalf("output = %q, want %q", out, "(no history)\n")
	}
}

func mustParseRFC3339(t *testing.T, value string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("time.Parse(%q): %v", value, err)
	}
	return ts
}
