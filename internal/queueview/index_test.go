package queueview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/ui"
)

func setupIndexDirs(t *testing.T) string {
	t.Helper()
	tasksDir := filepath.Join(t.TempDir(), ".mato")
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", dir, err)
		}
	}
	return tasksDir
}

func writeTask(t *testing.T, tasksDir, state, filename, content string) {
	t.Helper()
	path := filepath.Join(tasksDir, state, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
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

func TestBuildIndex_EmptyQueue(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	idx := BuildIndex(tasksDir)

	if len(idx.TasksByState(dirs.Backlog)) != 0 {
		t.Fatalf("expected 0 backlog tasks, got %d", len(idx.TasksByState(dirs.Backlog)))
	}
	if len(idx.CompletedIDs()) != 0 {
		t.Fatalf("expected 0 completed IDs, got %d", len(idx.CompletedIDs()))
	}
	if len(idx.ParseFailures()) != 0 {
		t.Fatalf("expected 0 parse failures, got %d", len(idx.ParseFailures()))
	}
}

func TestBuildIndex_BasicTask(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "add-feature.md", `---
priority: 10
affects: [main.go, pkg/util.go]
---
# Add Feature
Do the thing.
`)

	idx := BuildIndex(tasksDir)
	if len(idx.TasksByState(dirs.Backlog)) != 1 {
		t.Fatalf("expected 1 backlog task, got %d", len(idx.TasksByState(dirs.Backlog)))
	}

	snap := idx.Snapshot(dirs.Backlog, "add-feature.md")
	if snap == nil {
		t.Fatal("expected snapshot for backlog/add-feature.md, got nil")
	}
	if snap.Meta.Priority != 10 {
		t.Fatalf("priority = %d, want 10", snap.Meta.Priority)
	}
	if !reflect.DeepEqual(snap.Meta.Affects, []string{"main.go", "pkg/util.go"}) {
		t.Fatalf("affects = %v, want [main.go pkg/util.go]", snap.Meta.Affects)
	}
	if snap.State != dirs.Backlog {
		t.Fatalf("state = %q, want %q", snap.State, dirs.Backlog)
	}
}

func TestBuildIndex_CompletedIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "task-a.md", "---\nid: custom-id-a\n---\nDone.\n")
	writeTask(t, tasksDir, dirs.Completed, "task-b.md", "Also done.\n")

	idx := BuildIndex(tasksDir)
	completed := idx.CompletedIDs()
	for _, id := range []string{"task-a", "custom-id-a", "task-b"} {
		if _, ok := completed[id]; !ok {
			t.Errorf("expected %q in completedIDs", id)
		}
	}
}

func TestBuildIndex_NonCompletedIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "task-x.md", "Work.\n")
	writeTask(t, tasksDir, dirs.Completed, "task-y.md", "Done.\n")

	idx := BuildIndex(tasksDir)
	nonCompleted := idx.NonCompletedIDs()
	if _, ok := nonCompleted["task-x"]; !ok {
		t.Error("expected task-x in nonCompletedIDs")
	}
	if _, ok := nonCompleted["task-y"]; ok {
		t.Error("task-y (completed) should not be in nonCompletedIDs")
	}
}

func TestBuildIndex_AllIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "task-1.md", "Work.\n")
	writeTask(t, tasksDir, dirs.Completed, "task-2.md", "Done.\n")
	writeTask(t, tasksDir, dirs.Failed, "task-3.md", "Failed.\n")

	idx := BuildIndex(tasksDir)
	allIDs := idx.AllIDs()
	for _, id := range []string{"task-1", "task-2", "task-3"} {
		if _, ok := allIDs[id]; !ok {
			t.Errorf("expected %q in allIDs", id)
		}
	}
}

func TestBuildIndex_ActiveBranches(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "task-a.md", "<!-- claimed-by: abc  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- branch: task/task-a -->\n---\npriority: 5\n---\n# A\n")
	writeTask(t, tasksDir, dirs.ReadyReview, "task-b.md", "<!-- branch: task/task-b -->\n---\npriority: 5\n---\n# B\n")
	writeTask(t, tasksDir, dirs.Backlog, "task-c.md", "<!-- branch: task/task-c -->\n---\npriority: 5\n---\n# C\n")

	idx := BuildIndex(tasksDir)
	branches := idx.ActiveBranches()
	if _, ok := branches["task/task-a"]; !ok {
		t.Error("expected task/task-a in active branches")
	}
	if _, ok := branches["task/task-b"]; !ok {
		t.Error("expected task/task-b in active branches")
	}
	if _, ok := branches["task/task-c"]; ok {
		t.Error("task/task-c (backlog) should not be in active branches")
	}
}

func TestBuildIndex_HasActiveOverlap(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "active.md", "---\naffects: [main.go, pkg/util.go]\n---\nWorking.\n")
	writeTask(t, tasksDir, dirs.Backlog, "backlog.md", "---\naffects: [other.go]\n---\nWaiting.\n")

	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"overlapping", []string{"main.go"}, true},
		{"no overlap", []string{"other.go"}, false},
		{"empty affects", nil, false},
		{"partial overlap", []string{"unrelated.go", "pkg/util.go"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestBuildIndex_ActiveAffects(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "ip-task.md", "---\naffects: [main.go]\n---\nWorking.\n")
	writeTask(t, tasksDir, dirs.ReadyMerge, "rm-task.md", "---\naffects: [main.go, other.go]\n---\nReady.\n")
	writeTask(t, tasksDir, dirs.Backlog, "bl-task.md", "---\naffects: [main.go]\n---\nBacklog.\n")

	idx := BuildIndex(tasksDir)
	active := idx.ActiveAffects()
	if len(active) != 2 {
		t.Fatalf("expected 2 active tasks with affects, got %d", len(active))
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Name < active[j].Name })
	if active[0].Name != "ip-task.md" {
		t.Errorf("first active = %q, want ip-task.md", active[0].Name)
	}
	if active[1].Name != "rm-task.md" {
		t.Errorf("second active = %q, want rm-task.md", active[1].Name)
	}
}

func TestBuildIndex_BacklogByPriority(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "low.md", "---\npriority: 99\n---\nLow.\n")
	writeTask(t, tasksDir, dirs.Backlog, "high.md", "---\npriority: 1\n---\nHigh.\n")
	writeTask(t, tasksDir, dirs.Backlog, "mid.md", "---\npriority: 50\n---\nMid.\n")
	writeTask(t, tasksDir, dirs.Backlog, "excluded.md", "---\npriority: 0\n---\nExcluded.\n")

	idx := BuildIndex(tasksDir)
	result := idx.BacklogByPriority(map[string]struct{}{"excluded.md": {}})
	if len(result) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(result))
	}
	if result[0].Filename != "high.md" || result[1].Filename != "mid.md" || result[2].Filename != "low.md" {
		t.Fatalf("unexpected order: %s, %s, %s", result[0].Filename, result[1].Filename, result[2].Filename)
	}
}

func TestBuildIndex_BacklogByPriority_TiesBreakByFilename(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "z-task.md", "---\npriority: 10\n---\nZ.\n")
	writeTask(t, tasksDir, dirs.Backlog, "a-task.md", "---\npriority: 10\n---\nA.\n")

	idx := BuildIndex(tasksDir)
	result := idx.BacklogByPriority(nil)
	if len(result) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(result))
	}
	if result[0].Filename != "a-task.md" {
		t.Errorf("first = %q, want a-task.md", result[0].Filename)
	}
}

func TestBuildIndex_ParseFailures(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Waiting, "bad-task.md", "---\npriority: 5\n# No closing delimiter\n")
	writeTask(t, tasksDir, dirs.Waiting, "good-task.md", "---\npriority: 5\n---\n# Good\n")

	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	if failures[0].Filename != "bad-task.md" {
		t.Errorf("failure filename = %q, want bad-task.md", failures[0].Filename)
	}
	if failures[0].FailureCount != 0 {
		t.Errorf("FailureCount = %d, want 0", failures[0].FailureCount)
	}
	if len(idx.WaitingParseFailures()) != 1 {
		t.Fatalf("expected 1 waiting parse failure, got %d", len(idx.WaitingParseFailures()))
	}
	if snap := idx.Snapshot(dirs.Waiting, "good-task.md"); snap == nil {
		t.Fatal("expected good-task.md to be indexed")
	}
}

func TestBuildIndex_ParseFailurePreservesRuntimeMetadata(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	content := "<!-- claimed-by: agent-xyz  claimed-at: 2026-06-15T12:30:00Z -->\n" +
		"<!-- branch: task/bad-task -->\n" +
		"<!-- failure: agent-xyz at 2026-06-15T12:35:00Z — build failed -->\n" +
		"<!-- failure: agent-xyz at 2026-06-15T12:40:00Z — lint errors -->\n" +
		"<!-- review-rejection: reviewer at 2026-06-15T12:45:00Z — missing docs -->\n" +
		"<!-- cycle-failure: mato at 2026-06-15T13:00:00Z — circular dep -->\n" +
		"<!-- terminal-failure: mato at 2026-06-15T14:00:00Z — invalid glob -->\n" +
		"---\npriority: nope\n---\n# Broken task\n"
	writeTask(t, tasksDir, dirs.Failed, "broken-task.md", content)

	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	pf := failures[0]
	if pf.Branch != "task/bad-task" || pf.ClaimedBy != "agent-xyz" {
		t.Fatalf("unexpected parse failure metadata: %+v", pf)
	}
	wantTime := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	if !pf.ClaimedAt.Equal(wantTime) {
		t.Errorf("ClaimedAt = %v, want %v", pf.ClaimedAt, wantTime)
	}
	if pf.FailureCount != 2 || pf.LastFailureReason != "lint errors" || pf.LastCycleFailureReason != "circular dep" || pf.LastTerminalFailureReason != "invalid glob" || pf.LastReviewRejectionReason != "missing docs" {
		t.Fatalf("unexpected failure metadata: %+v", pf)
	}
	if pf.Cancelled {
		t.Error("Cancelled = true, want false")
	}
}

func TestBuildIndex_CancelledSnapshot(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Failed, "cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: cancelled\n---\n# Cancelled\n")
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(dirs.Failed, "cancelled.md")
	if snap == nil || !snap.Cancelled {
		t.Fatal("expected cancelled snapshot")
	}
}

func TestBuildIndex_CancelledParseFailure(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Failed, "broken-cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n")
	idx := BuildIndex(tasksDir)
	if len(idx.ParseFailures()) != 1 || !idx.ParseFailures()[0].Cancelled {
		t.Fatal("expected cancelled parse failure")
	}
}

func TestBuildIndex_FailureAndReviewFailureCounts(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	content := "<!-- claimed-by: abc  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- branch: task/counted -->"
	content += "\n<!-- failure: abc at 2026-01-01T01:00:00Z step=WORK error=build -->"
	content += "\n<!-- failure: def at 2026-01-01T02:00:00Z step=WORK error=test -->"
	content += "\n<!-- review-failure: ghi at 2026-01-01T03:00:00Z step=REVIEW error=timeout -->"
	content += "\n---\npriority: 5\n---\n# Counted\n"
	writeTask(t, tasksDir, dirs.InProgress, "counted.md", content)
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(dirs.InProgress, "counted.md")
	if snap == nil {
		t.Fatal("expected counted.md to be indexed")
	}
	if snap.FailureCount != 2 || snap.ReviewFailureCount != 1 || snap.Branch != "task/counted" {
		t.Fatalf("unexpected snapshot metadata: %+v", snap)
	}
}

func TestBuildIndex_SnapshotMetadataFields(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	content := "<!-- claimed-by: agent-xyz  claimed-at: 2026-06-15T12:30:00Z -->\n" +
		"<!-- branch: task/meta-test -->\n" +
		"<!-- failure: agent-xyz at 2026-06-15T12:35:00Z — build failed -->\n" +
		"<!-- failure: agent-xyz at 2026-06-15T12:40:00Z — lint errors -->\n" +
		"<!-- review-rejection: reviewer at 2026-06-15T12:45:00Z — add tests -->\n" +
		"<!-- cycle-failure: mato at 2026-06-15T13:00:00Z — circular dep -->\n" +
		"<!-- terminal-failure: mato at 2026-06-15T14:00:00Z — invalid glob -->\n" +
		"---\npriority: 10\n---\n# Meta test task\n"
	writeTask(t, tasksDir, dirs.InProgress, "meta-test.md", content)
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(dirs.InProgress, "meta-test.md")
	if snap == nil {
		t.Fatal("expected meta-test.md to be indexed")
	}
	wantTime := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	if snap.ClaimedBy != "agent-xyz" || !snap.ClaimedAt.Equal(wantTime) || snap.LastFailureReason != "lint errors" || snap.LastCycleFailureReason != "circular dep" || snap.LastTerminalFailureReason != "invalid glob" || snap.LastReviewRejectionReason != "add tests" {
		t.Fatalf("unexpected snapshot metadata: %+v", snap)
	}
}

func TestBuildIndex_TasksByState(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "a.md", "Task A.\n")
	writeTask(t, tasksDir, dirs.Backlog, "b.md", "Task B.\n")
	writeTask(t, tasksDir, dirs.Completed, "c.md", "Task C.\n")
	idx := BuildIndex(tasksDir)
	if len(idx.TasksByState(dirs.Backlog)) != 2 || len(idx.TasksByState(dirs.Completed)) != 1 || len(idx.TasksByState(dirs.Failed)) != 0 {
		t.Fatal("unexpected TasksByState counts")
	}
}

func TestBuildIndex_NilIndexMethods(t *testing.T) {
	var idx *PollIndex
	if idx.TasksByState(dirs.Backlog) != nil || idx.CompletedIDs() != nil || idx.NonCompletedIDs() != nil || idx.AllIDs() != nil || idx.HasActiveOverlap([]string{"main.go"}) || idx.ActiveAffects() != nil || idx.ActiveBranches() != nil || idx.BacklogByPriority(nil) != nil || idx.BuildWarnings() != nil || idx.ParseFailures() != nil || idx.WaitingParseFailures() != nil || idx.BacklogParseFailures() != nil || idx.ReviewParseFailures() != nil || idx.Snapshot(dirs.Backlog, "x.md") != nil {
		t.Fatal("nil index methods returned unexpected non-zero values")
	}
}

func TestBuildIndex_MissingDirectories(t *testing.T) {
	tasksDir := filepath.Join(t.TempDir(), ".mato")
	idx := BuildIndex(tasksDir)
	if len(idx.TasksByState(dirs.Backlog)) != 0 {
		t.Fatalf("expected 0 backlog tasks for missing dirs, got %d", len(idx.TasksByState(dirs.Backlog)))
	}
}

func TestBuildIndex_ActiveBranchTrackedForMalformedActiveTask(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.ReadyReview, "broken.md", strings.Join([]string{"<!-- branch: task/broken -->", "---", "priority: [oops", "---", "# Broken", ""}, "\n"))
	idx := BuildIndex(tasksDir)
	if _, ok := idx.ActiveBranches()["task/broken"]; !ok {
		t.Fatal("expected malformed active task branch to be tracked")
	}
}

func TestBuildIndex_ActiveParseFailureRecoversAffects(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.ReadyReview, "broken.md", strings.Join([]string{
		"---",
		"affects:",
		"  - internal/queueview/index.go",
		"  - internal/status/status_gather.go",
		"priority: [oops",
		"---",
		"# Broken",
		"",
	}, "\n"))

	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	if !reflect.DeepEqual(failures[0].RecoveredAffects, []string{"internal/queueview/index.go", "internal/status/status_gather.go"}) {
		t.Fatalf("RecoveredAffects = %v, want recovered affects", failures[0].RecoveredAffects)
	}
	if !idx.HasActiveOverlap([]string{"internal/queueview/index.go"}) {
		t.Fatal("expected recovered affects to participate in active overlap checks")
	}
	if idx.HasActiveOverlap([]string{"docs/readme.md"}) {
		t.Fatal("unexpected overlap for unrelated path")
	}
	active := idx.ActiveAffects()
	if len(active) != 1 {
		t.Fatalf("expected 1 active affects entry, got %d", len(active))
	}
	if active[0].Name != "broken.md" || active[0].Dir != dirs.ReadyReview {
		t.Fatalf("unexpected active task: %+v", active[0])
	}
	if !reflect.DeepEqual(active[0].Affects, failures[0].RecoveredAffects) {
		t.Fatalf("ActiveAffects = %v, want %v", active[0].Affects, failures[0].RecoveredAffects)
	}
}

func TestBuildIndex_UnrecoverableActiveParseFailureFailsClosed(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "broken.md", strings.Join([]string{
		"---",
		"affects: [unterminated",
		"priority: 5",
		"---",
		"# Broken",
		"",
	}, "\n"))

	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	if len(failures[0].RecoveredAffects) != 0 {
		t.Fatalf("RecoveredAffects = %v, want none", failures[0].RecoveredAffects)
	}
	if !idx.HasActiveOverlap([]string{"docs/readme.md"}) {
		t.Fatal("expected unrecoverable active parse failure to block overlap checks conservatively")
	}
	if idx.HasActiveOverlap(nil) {
		t.Fatal("nil affects should not report overlap")
	}
}

func TestBuildIndex_UnsafeRecoveredAffectsStillFailClosed(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "broken.md", strings.Join([]string{
		"---",
		"affects:",
		"  - /tmp/secret.txt",
		"  - ../outside.go",
		"priority: [oops",
		"---",
		"# Broken",
		"",
	}, "\n"))

	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	if len(failures[0].RecoveredAffects) != 0 {
		t.Fatalf("RecoveredAffects = %v, want none", failures[0].RecoveredAffects)
	}
	if !idx.HasActiveOverlap([]string{"docs/readme.md"}) {
		t.Fatal("expected stripped-only recovered affects to block overlap checks conservatively")
	}
	foundUnsafeWarning := false
	for _, warning := range idx.BuildWarnings() {
		if strings.Contains(warning.Err.Error(), "unsafe affects entry") {
			foundUnsafeWarning = true
			break
		}
	}
	if !foundUnsafeWarning {
		t.Fatalf("expected unsafe affects warning, got %v", idx.BuildWarnings())
	}
}

func TestBuildIndex_BacklogParseFailures(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "bad.md", "---\npriority: [oops\n---\n# Bad\n")
	idx := BuildIndex(tasksDir)
	if len(idx.BacklogParseFailures()) != 1 || idx.BacklogParseFailures()[0].Filename != "bad.md" {
		t.Fatal("expected backlog parse failure for bad.md")
	}
}

func TestBuildIndex_ReviewParseFailures(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.ReadyReview, "bad-review.md", "---\npriority: [oops\n---\n# Bad Review\n")
	writeTask(t, tasksDir, dirs.ReadyReview, "good-review.md", "---\npriority: 5\n---\n# Good Review\n")
	idx := BuildIndex(tasksDir)
	if len(idx.ReviewParseFailures()) != 1 || idx.ReviewParseFailures()[0].Filename != "bad-review.md" {
		t.Fatal("expected review parse failure for bad-review.md")
	}
	if idx.Snapshot(dirs.ReadyReview, "good-review.md") == nil {
		t.Fatal("expected good-review.md to be indexed")
	}
}

func TestBuildIndex_ClaimedByBeforeFrontmatter(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/my-task -->
---
id: my-task
priority: 10
affects: [main.go]
---
# My Task
Body.
`
	writeTask(t, tasksDir, dirs.InProgress, "my-task.md", content)
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(dirs.InProgress, "my-task.md")
	if snap == nil {
		t.Fatal("expected my-task.md to be indexed")
	}
	if snap.Meta.ID != "my-task" || snap.Meta.Priority != 10 || snap.Branch != "task/my-task" || !reflect.DeepEqual(snap.Meta.Affects, []string{"main.go"}) {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if _, ok := idx.ActiveBranches()["task/my-task"]; !ok {
		t.Error("expected task/my-task in active branches")
	}
}

func TestBuildIndex_ConsistencyWithParseTaskFile(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	content := `---
id: consistency-check
priority: 7
depends_on: [dep-a]
affects: [api, cli]
max_retries: 5
---
# Title
Body.
`
	writeTask(t, tasksDir, dirs.Backlog, "consistency-check.md", content)
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(dirs.Backlog, "consistency-check.md")
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	meta, body, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, dirs.Backlog, "consistency-check.md"))
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}
	if !reflect.DeepEqual(snap.Meta, meta) || snap.Body != body {
		t.Fatalf("snapshot mismatch: %#v %#v %q %q", snap.Meta, meta, snap.Body, body)
	}
}

func TestBuildIndex_MalformedCompletedTaskStemInIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "broken-task.md", "---\npriority: nope\n---\nBody.\n")
	writeTask(t, tasksDir, dirs.Completed, "good-task.md", "---\npriority: 5\n---\nDone.\n")
	idx := BuildIndex(tasksDir)
	if _, ok := idx.CompletedIDs()["broken-task"]; !ok {
		t.Error("expected broken-task stem in CompletedIDs")
	}
	if _, ok := idx.AllIDs()["broken-task"]; !ok {
		t.Error("expected broken-task stem in AllIDs")
	}
	found := false
	for _, pf := range idx.ParseFailures() {
		if pf.Filename == "broken-task.md" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected broken-task.md in ParseFailures")
	}
	if _, ok := idx.CompletedIDs()["good-task"]; !ok || idx.Snapshot(dirs.Completed, "good-task.md") == nil {
		t.Error("expected good-task to be fully indexed")
	}
}

func TestBuildIndex_MalformedNonCompletedTaskStemInIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "bad-backlog.md", "---\npriority: nope\n---\nBody.\n")
	idx := BuildIndex(tasksDir)
	if _, ok := idx.NonCompletedIDs()["bad-backlog"]; !ok {
		t.Error("expected bad-backlog stem in NonCompletedIDs")
	}
	if _, ok := idx.AllIDs()["bad-backlog"]; !ok {
		t.Error("expected bad-backlog stem in AllIDs")
	}
	if _, ok := idx.CompletedIDs()["bad-backlog"]; ok {
		t.Error("bad-backlog should not be in CompletedIDs")
	}
}

func TestEnsureIndex_NilDoesNotWriteStderr(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)
	idx := ensureIndex(tasksDir, nil)
	ui.SetWarningWriter(prevWarn)
	w.Close()
	os.Stderr = oldStderr
	var buf [512]byte
	n, _ := r.Read(buf[:])
	r.Close()
	if n > 0 {
		t.Errorf("ensureIndex(nil) wrote to stderr: %s", string(buf[:n]))
	}
	if idx == nil {
		t.Fatal("ensureIndex(nil) returned nil")
	}
}

func TestHasActiveOverlap_PrefixMatch(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "refactor-client.md", "---\naffects:\n  - pkg/client/\n---\n# Refactor\n")
	writeTask(t, tasksDir, dirs.InProgress, "fix-merge.md", "---\naffects:\n  - internal/merge/merge.go\n---\n# Fix\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"exact match", []string{"internal/merge/merge.go"}, true},
		{"file under active prefix", []string{"pkg/client/http.go"}, true},
		{"nested file under active prefix", []string{"pkg/client/retry/backoff.go"}, true},
		{"prefix that contains active prefix", []string{"pkg/"}, true},
		{"non-overlapping file", []string{"docs/readme.md"}, false},
		{"non-overlapping prefix", []string{"cmd/"}, false},
		{"empty affects", []string{}, false},
		{"nil affects", nil, false},
		{"prefix matching active exact file", []string{"internal/merge/"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_GlobVsExact(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "refactor-runner.md", "---\naffects:\n  - internal/runner/*.go\n---\n# Refactor\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"matching file", []string{"internal/runner/task.go"}, true},
		{"non-matching file", []string{"internal/queue/queue.go"}, false},
		{"nested file not matched by single star", []string{"internal/runner/sub/deep.go"}, false},
		{"different extension", []string{"internal/runner/task.md"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_GlobVsPrefix(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "glob-task.md", "---\naffects:\n  - internal/runner/*.go\n---\n# Glob\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"overlapping prefix", []string{"internal/runner/"}, true},
		{"parent prefix", []string{"internal/"}, true},
		{"disjoint prefix", []string{"pkg/"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_IncomingGlobVsExact(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "fix-file.md", "---\naffects:\n  - internal/runner/task.go\n---\n# Fix\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"matching glob", []string{"internal/runner/*.go"}, true},
		{"non-matching glob", []string{"pkg/*.go"}, false},
		{"doublestar matching", []string{"**/*.go"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_IncomingGlobVsPrefix(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "refactor-client.md", "---\naffects:\n  - pkg/client/\n---\n# Refactor\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"glob under prefix", []string{"pkg/client/*.go"}, true},
		{"glob outside prefix", []string{"internal/*.go"}, false},
		{"doublestar matches everything", []string{"**/*.go"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_GlobVsGlob(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "glob-task.md", "---\naffects:\n  - internal/runner/*.go\n---\n# Glob\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"overlapping static prefix", []string{"internal/runner/*_test.go"}, true},
		{"disjoint static prefix", []string{"pkg/client/*.go"}, false},
		{"doublestar vs single star", []string{"internal/**/*.go"}, true},
		{"identical glob", []string{"internal/runner/*.go"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_IncomingDoublestar(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "fix-task.md", "---\naffects:\n  - internal/runner/task.go\n---\n# Fix\n")
	writeTask(t, tasksDir, dirs.ReadyReview, "prefix-task.md", "---\naffects:\n  - pkg/client/\n---\n# Prefix\n")
	idx := BuildIndex(tasksDir)
	if !idx.HasActiveOverlap([]string{"**/*.go"}) {
		t.Error("HasActiveOverlap(**/*.go) = false, want true")
	}
}

func TestHasActiveOverlap_ExactFastPath(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "exact-task.md", "---\naffects:\n  - main.go\n  - internal/foo.go\n---\n# Exact\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"exact match", []string{"main.go"}, true},
		{"no match", []string{"other.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestBuildIndex_InvalidGlobStillIndexedWithWarning(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "bad-glob.md", "---\naffects:\n  - \"internal/[bad\"\n  - main.go\n---\n# Bad glob\n")
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(dirs.InProgress, "bad-glob.md")
	if snap == nil || !reflect.DeepEqual(snap.Meta.Affects, []string{"internal/[bad", "main.go"}) {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if !idx.HasActiveOverlap([]string{"internal/[bad"}) {
		t.Error("expected literal invalid glob string to participate in overlap checks")
	}
	found := false
	for _, w := range idx.BuildWarnings() {
		if strings.Contains(w.Err.Error(), "invalid glob in affects") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected build warning about invalid glob, got %v", idx.BuildWarnings())
	}
}

func TestBuildIndex_GlobTrailingSlashStillIndexedWithWarning(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "glob-slash.md", "---\naffects:\n  - \"internal/*/\"\n---\n# Glob slash\n")
	idx := BuildIndex(tasksDir)
	if idx.Snapshot(dirs.InProgress, "glob-slash.md") == nil {
		t.Fatal("expected glob-slash.md to be indexed")
	}
	found := false
	for _, w := range idx.BuildWarnings() {
		if strings.Contains(w.Err.Error(), "combines glob syntax with trailing /") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected build warning about glob with trailing /, got %v", idx.BuildWarnings())
	}
}

func TestHasActiveOverlap_InvalidGlobBlocksOverlapping(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "bad-glob.md", "---\naffects:\n  - \"internal/[bad\"\n---\n# Bad glob\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"file under same prefix", []string{"internal/runner/task.go"}, true},
		{"file under different prefix", []string{"pkg/client/http.go"}, false},
		{"exact prefix match", []string{"internal/foo.go"}, true},
		{"disjoint file", []string{"main.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_GlobTrailingSlashBlocksOverlapping(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "glob-slash.md", "---\naffects:\n  - \"internal/*/\"\n---\n# Glob slash\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"file under prefix", []string{"internal/runner/task.go"}, true},
		{"disjoint file", []string{"pkg/client.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_InvalidGlobVsValidGlob(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "bad-glob.md", "---\naffects:\n  - \"internal/[bad\"\n---\n# Bad glob\n")
	idx := BuildIndex(tasksDir)
	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"doublestar matches everything", []string{"**/*.go"}, true},
		{"overlapping glob prefix", []string{"internal/runner/*.go"}, true},
		{"disjoint glob prefix", []string{"pkg/client/*.go"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.HasActiveOverlap(tt.affects); got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestCompletedIDs_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "done-task.md", "# Done\n")
	idx := BuildIndex(tasksDir)
	ids := idx.CompletedIDs()
	ids["injected"] = struct{}{}
	delete(ids, "done-task")
	ids2 := idx.CompletedIDs()
	if _, ok := ids2["done-task"]; !ok || func() bool { _, ok := ids2["injected"]; return ok }() {
		t.Error("CompletedIDs defensive copy broken")
	}
}

func TestNonCompletedIDs_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "bl-task.md", "# Backlog\n")
	idx := BuildIndex(tasksDir)
	ids := idx.NonCompletedIDs()
	ids["injected"] = struct{}{}
	delete(ids, "bl-task")
	ids2 := idx.NonCompletedIDs()
	if _, ok := ids2["bl-task"]; !ok || func() bool { _, ok := ids2["injected"]; return ok }() {
		t.Error("NonCompletedIDs defensive copy broken")
	}
}

func TestAllIDs_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "any-task.md", "# Any\n")
	idx := BuildIndex(tasksDir)
	ids := idx.AllIDs()
	ids["injected"] = struct{}{}
	delete(ids, "any-task")
	ids2 := idx.AllIDs()
	if _, ok := ids2["any-task"]; !ok || func() bool { _, ok := ids2["injected"]; return ok }() {
		t.Error("AllIDs defensive copy broken")
	}
}

func TestActiveBranches_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.InProgress, "ip-task.md", "<!-- branch: task/ip-task -->\n# In Progress\n")
	idx := BuildIndex(tasksDir)
	branches := idx.ActiveBranches()
	branches["injected"] = struct{}{}
	delete(branches, "task/ip-task")
	branches2 := idx.ActiveBranches()
	if _, ok := branches2["task/ip-task"]; !ok || func() bool { _, ok := branches2["injected"]; return ok }() {
		t.Error("ActiveBranches defensive copy broken")
	}
}

func TestBuildIndex_ReviewRejectionFromVerdictFallback(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "needs-rework.md", "---\npriority: 5\n---\n# Needs Rework\n")
	writeVerdictFile(t, tasksDir, "needs-rework.md", map[string]string{"verdict": "reject", "reason": "missing test coverage"})
	idx := BuildIndex(tasksDir)
	if snap := idx.Snapshot(dirs.Backlog, "needs-rework.md"); snap == nil || snap.LastReviewRejectionReason != "missing test coverage" {
		t.Fatal("expected verdict fallback rejection reason")
	}
}

func TestBuildIndex_ReviewRejectionPrefersDurableMarker(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "has-marker.md", "---\npriority: 5\n---\n# Has Marker\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — durable reason -->\n")
	writeVerdictFile(t, tasksDir, "has-marker.md", map[string]string{"verdict": "reject", "reason": "verdict reason should not appear"})
	idx := BuildIndex(tasksDir)
	if snap := idx.Snapshot(dirs.Backlog, "has-marker.md"); snap == nil || snap.LastReviewRejectionReason != "durable reason" {
		t.Fatal("expected durable marker to win over verdict fallback")
	}
}

func TestBuildIndex_NoVerdictFile_NoRejectionReason(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "clean.md", "---\npriority: 5\n---\n# Clean\n")
	idx := BuildIndex(tasksDir)
	if snap := idx.Snapshot(dirs.Backlog, "clean.md"); snap == nil || snap.LastReviewRejectionReason != "" {
		t.Fatal("expected no rejection reason without marker or verdict file")
	}
}
