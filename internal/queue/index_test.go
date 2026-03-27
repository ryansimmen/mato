package queue

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"mato/internal/frontmatter"
)

// setupIndexDirs creates the standard .mato directory tree in a temp dir
// and returns the tasksDir path.
func setupIndexDirs(t *testing.T) string {
	t.Helper()
	tasksDir := filepath.Join(t.TempDir(), ".mato")
	for _, dir := range AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("os.MkdirAll(%s): %v", dir, err)
		}
	}
	return tasksDir
}

// writeTask is a helper to write a task file in the given state directory.
func writeTask(t *testing.T, tasksDir, state, filename, content string) {
	t.Helper()
	path := filepath.Join(tasksDir, state, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%s): %v", path, err)
	}
}

func TestBuildIndex_EmptyQueue(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	idx := BuildIndex(tasksDir)

	if len(idx.tasks) != 0 {
		t.Fatalf("expected 0 tasks, got %d", len(idx.tasks))
	}
	if len(idx.completedIDs) != 0 {
		t.Fatalf("expected 0 completed IDs, got %d", len(idx.completedIDs))
	}
	if len(idx.parseFailures) != 0 {
		t.Fatalf("expected 0 parse failures, got %d", len(idx.parseFailures))
	}
}

func TestBuildIndex_BasicTask(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "add-feature.md", `---
priority: 10
affects: [main.go, pkg/util.go]
---
# Add Feature
Do the thing.
`)

	idx := BuildIndex(tasksDir)

	if len(idx.tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(idx.tasks))
	}

	snap := idx.Snapshot(DirBacklog, "add-feature.md")
	if snap == nil {
		t.Fatal("expected snapshot for backlog/add-feature.md, got nil")
	}
	if snap.Meta.Priority != 10 {
		t.Fatalf("priority = %d, want 10", snap.Meta.Priority)
	}
	if !reflect.DeepEqual(snap.Meta.Affects, []string{"main.go", "pkg/util.go"}) {
		t.Fatalf("affects = %v, want [main.go pkg/util.go]", snap.Meta.Affects)
	}
	if snap.State != DirBacklog {
		t.Fatalf("state = %q, want %q", snap.State, DirBacklog)
	}
}

func TestBuildIndex_CompletedIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirCompleted, "task-a.md", `---
id: custom-id-a
---
Done.
`)
	writeTask(t, tasksDir, DirCompleted, "task-b.md", "Also done.\n")

	idx := BuildIndex(tasksDir)

	// Both filename stems and frontmatter IDs should be in completedIDs.
	for _, id := range []string{"task-a", "custom-id-a", "task-b"} {
		if _, ok := idx.completedIDs[id]; !ok {
			t.Errorf("expected %q in completedIDs", id)
		}
	}
}

func TestBuildIndex_NonCompletedIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task-x.md", "Work.\n")
	writeTask(t, tasksDir, DirCompleted, "task-y.md", "Done.\n")

	idx := BuildIndex(tasksDir)

	if _, ok := idx.nonCompletedIDs["task-x"]; !ok {
		t.Error("expected task-x in nonCompletedIDs")
	}
	if _, ok := idx.nonCompletedIDs["task-y"]; ok {
		t.Error("task-y (completed) should not be in nonCompletedIDs")
	}
}

func TestBuildIndex_AllIDs(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "task-1.md", "Work.\n")
	writeTask(t, tasksDir, DirCompleted, "task-2.md", "Done.\n")
	writeTask(t, tasksDir, DirFailed, "task-3.md", "Failed.\n")

	idx := BuildIndex(tasksDir)

	for _, id := range []string{"task-1", "task-2", "task-3"} {
		if _, ok := idx.allIDs[id]; !ok {
			t.Errorf("expected %q in allIDs", id)
		}
	}
}

func TestBuildIndex_ActiveBranches(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	// Task in in-progress with a branch comment.
	writeTask(t, tasksDir, DirInProgress, "task-a.md",
		"<!-- claimed-by: abc  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- branch: task/task-a -->\n---\npriority: 5\n---\n# A\n")

	// Task in ready-for-review with a branch comment.
	writeTask(t, tasksDir, DirReadyReview, "task-b.md",
		"<!-- branch: task/task-b -->\n---\npriority: 5\n---\n# B\n")

	// Task in backlog should NOT contribute to active branches.
	writeTask(t, tasksDir, DirBacklog, "task-c.md",
		"<!-- branch: task/task-c -->\n---\npriority: 5\n---\n# C\n")

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

	writeTask(t, tasksDir, DirInProgress, "active.md", `---
affects: [main.go, pkg/util.go]
---
Working.
`)
	writeTask(t, tasksDir, DirBacklog, "backlog.md", `---
affects: [other.go]
---
Waiting.
`)

	idx := BuildIndex(tasksDir)

	tests := []struct {
		name    string
		affects []string
		want    bool
	}{
		{"overlapping", []string{"main.go"}, true},
		{"no overlap", []string{"other.go"}, false}, // backlog doesn't count
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

	writeTask(t, tasksDir, DirInProgress, "ip-task.md", `---
affects: [main.go]
---
Working.
`)
	writeTask(t, tasksDir, DirReadyMerge, "rm-task.md", `---
affects: [main.go, other.go]
---
Ready.
`)
	writeTask(t, tasksDir, DirBacklog, "bl-task.md", `---
affects: [main.go]
---
Backlog.
`)

	idx := BuildIndex(tasksDir)
	active := idx.ActiveAffects()

	if len(active) != 2 {
		t.Fatalf("expected 2 active tasks with affects, got %d", len(active))
	}

	// Sort for deterministic comparison.
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

	writeTask(t, tasksDir, DirBacklog, "low.md", "---\npriority: 99\n---\nLow.\n")
	writeTask(t, tasksDir, DirBacklog, "high.md", "---\npriority: 1\n---\nHigh.\n")
	writeTask(t, tasksDir, DirBacklog, "mid.md", "---\npriority: 50\n---\nMid.\n")
	writeTask(t, tasksDir, DirBacklog, "excluded.md", "---\npriority: 0\n---\nExcluded.\n")

	idx := BuildIndex(tasksDir)
	exclude := map[string]struct{}{"excluded.md": {}}
	result := idx.BacklogByPriority(exclude)

	if len(result) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(result))
	}
	if result[0].Filename != "high.md" {
		t.Errorf("first = %q, want high.md", result[0].Filename)
	}
	if result[1].Filename != "mid.md" {
		t.Errorf("second = %q, want mid.md", result[1].Filename)
	}
	if result[2].Filename != "low.md" {
		t.Errorf("third = %q, want low.md", result[2].Filename)
	}
}

func TestBuildIndex_BacklogByPriority_TiesBreakByFilename(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	writeTask(t, tasksDir, DirBacklog, "z-task.md", "---\npriority: 10\n---\nZ.\n")
	writeTask(t, tasksDir, DirBacklog, "a-task.md", "---\npriority: 10\n---\nA.\n")

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

	// Write a file with unterminated frontmatter.
	writeTask(t, tasksDir, DirWaiting, "bad-task.md", "---\npriority: 5\n# No closing delimiter\n")
	// Write a good task to verify it's still indexed.
	writeTask(t, tasksDir, DirWaiting, "good-task.md", "---\npriority: 5\n---\n# Good\n")

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

	waitingFailures := idx.WaitingParseFailures()
	if len(waitingFailures) != 1 {
		t.Fatalf("expected 1 waiting parse failure, got %d", len(waitingFailures))
	}

	// Good task should still be indexed.
	snap := idx.Snapshot(DirWaiting, "good-task.md")
	if snap == nil {
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

	writeTask(t, tasksDir, DirFailed, "broken-task.md", content)
	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	pf := failures[0]
	if pf.Branch != "task/bad-task" {
		t.Errorf("Branch = %q, want %q", pf.Branch, "task/bad-task")
	}
	if pf.ClaimedBy != "agent-xyz" {
		t.Errorf("ClaimedBy = %q, want %q", pf.ClaimedBy, "agent-xyz")
	}
	wantTime := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	if !pf.ClaimedAt.Equal(wantTime) {
		t.Errorf("ClaimedAt = %v, want %v", pf.ClaimedAt, wantTime)
	}
	if pf.FailureCount != 2 {
		t.Errorf("FailureCount = %d, want 2", pf.FailureCount)
	}
	if pf.LastFailureReason != "lint errors" {
		t.Errorf("LastFailureReason = %q, want %q", pf.LastFailureReason, "lint errors")
	}
	if pf.LastCycleFailureReason != "circular dep" {
		t.Errorf("LastCycleFailureReason = %q, want %q", pf.LastCycleFailureReason, "circular dep")
	}
	if pf.LastTerminalFailureReason != "invalid glob" {
		t.Errorf("LastTerminalFailureReason = %q, want %q", pf.LastTerminalFailureReason, "invalid glob")
	}
	if pf.LastReviewRejectionReason != "missing docs" {
		t.Errorf("LastReviewRejectionReason = %q, want %q", pf.LastReviewRejectionReason, "missing docs")
	}
	if pf.Cancelled {
		t.Error("Cancelled = true, want false")
	}
}

func TestBuildIndex_CancelledSnapshot(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirFailed, "cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: cancelled\n---\n# Cancelled\n")

	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(DirFailed, "cancelled.md")
	if snap == nil {
		t.Fatal("expected cancelled snapshot")
	}
	if !snap.Cancelled {
		t.Fatal("Cancelled = false, want true")
	}
}

func TestBuildIndex_CancelledParseFailure(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirFailed, "broken-cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n")

	idx := BuildIndex(tasksDir)
	failures := idx.ParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 parse failure, got %d", len(failures))
	}
	if !failures[0].Cancelled {
		t.Fatal("Cancelled = false, want true")
	}
}

func TestBuildIndex_FailureAndReviewFailureCounts(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	content := `<!-- claimed-by: abc  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/counted -->`
	content += "\n<!-- failure: abc at 2026-01-01T01:00:00Z step=WORK error=build -->"
	content += "\n<!-- failure: def at 2026-01-01T02:00:00Z step=WORK error=test -->"
	content += "\n<!-- review-failure: ghi at 2026-01-01T03:00:00Z step=REVIEW error=timeout -->"
	content += "\n---\npriority: 5\n---\n# Counted\n"

	writeTask(t, tasksDir, DirInProgress, "counted.md", content)

	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(DirInProgress, "counted.md")
	if snap == nil {
		t.Fatal("expected counted.md to be indexed")
	}
	if snap.FailureCount != 2 {
		t.Errorf("FailureCount = %d, want 2", snap.FailureCount)
	}
	if snap.ReviewFailureCount != 1 {
		t.Errorf("ReviewFailureCount = %d, want 1", snap.ReviewFailureCount)
	}
	if snap.Branch != "task/counted" {
		t.Errorf("Branch = %q, want task/counted", snap.Branch)
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

	writeTask(t, tasksDir, DirInProgress, "meta-test.md", content)
	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(DirInProgress, "meta-test.md")
	if snap == nil {
		t.Fatal("expected meta-test.md to be indexed")
	}
	if snap.ClaimedBy != "agent-xyz" {
		t.Errorf("ClaimedBy = %q, want %q", snap.ClaimedBy, "agent-xyz")
	}
	wantTime := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	if !snap.ClaimedAt.Equal(wantTime) {
		t.Errorf("ClaimedAt = %v, want %v", snap.ClaimedAt, wantTime)
	}
	if snap.LastFailureReason != "lint errors" {
		t.Errorf("LastFailureReason = %q, want %q", snap.LastFailureReason, "lint errors")
	}
	if snap.LastCycleFailureReason != "circular dep" {
		t.Errorf("LastCycleFailureReason = %q, want %q", snap.LastCycleFailureReason, "circular dep")
	}
	if snap.LastTerminalFailureReason != "invalid glob" {
		t.Errorf("LastTerminalFailureReason = %q, want %q", snap.LastTerminalFailureReason, "invalid glob")
	}
	if snap.LastReviewRejectionReason != "add tests" {
		t.Errorf("LastReviewRejectionReason = %q, want %q", snap.LastReviewRejectionReason, "add tests")
	}
}

func TestBuildIndex_TasksByState(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "a.md", "Task A.\n")
	writeTask(t, tasksDir, DirBacklog, "b.md", "Task B.\n")
	writeTask(t, tasksDir, DirCompleted, "c.md", "Task C.\n")

	idx := BuildIndex(tasksDir)

	backlog := idx.TasksByState(DirBacklog)
	if len(backlog) != 2 {
		t.Fatalf("expected 2 backlog tasks, got %d", len(backlog))
	}

	completed := idx.TasksByState(DirCompleted)
	if len(completed) != 1 {
		t.Fatalf("expected 1 completed task, got %d", len(completed))
	}

	empty := idx.TasksByState(DirFailed)
	if len(empty) != 0 {
		t.Fatalf("expected 0 failed tasks, got %d", len(empty))
	}
}

func TestBuildIndex_NilIndexMethods(t *testing.T) {
	var idx *PollIndex

	if idx.TasksByState(DirBacklog) != nil {
		t.Error("nil index TasksByState should return nil")
	}
	if idx.CompletedIDs() != nil {
		t.Error("nil index CompletedIDs should return nil")
	}
	if idx.NonCompletedIDs() != nil {
		t.Error("nil index NonCompletedIDs should return nil")
	}
	if idx.AllIDs() != nil {
		t.Error("nil index AllIDs should return nil")
	}
	if idx.HasActiveOverlap([]string{"main.go"}) {
		t.Error("nil index HasActiveOverlap should return false")
	}
	if idx.ActiveAffects() != nil {
		t.Error("nil index ActiveAffects should return nil")
	}
	if idx.ActiveBranches() != nil {
		t.Error("nil index ActiveBranches should return nil")
	}
	if idx.BacklogByPriority(nil) != nil {
		t.Error("nil index BacklogByPriority should return nil")
	}
	if idx.BuildWarnings() != nil {
		t.Error("nil index BuildWarnings should return nil")
	}
	if idx.ParseFailures() != nil {
		t.Error("nil index ParseFailures should return nil")
	}
	if idx.WaitingParseFailures() != nil {
		t.Error("nil index WaitingParseFailures should return nil")
	}
	if idx.BacklogParseFailures() != nil {
		t.Error("nil index BacklogParseFailures should return nil")
	}
	if idx.ReviewParseFailures() != nil {
		t.Error("nil index ReviewParseFailures should return nil")
	}
	if idx.Snapshot(DirBacklog, "x.md") != nil {
		t.Error("nil index Snapshot should return nil")
	}
}

func TestBuildIndex_MissingDirectories(t *testing.T) {
	// BuildIndex should not panic when directories don't exist.
	tasksDir := filepath.Join(t.TempDir(), ".mato")
	// Don't create any directories.

	idx := BuildIndex(tasksDir)
	if len(idx.tasks) != 0 {
		t.Fatalf("expected 0 tasks for missing dirs, got %d", len(idx.tasks))
	}
}

func TestBuildIndex_ActiveBranchTrackedForMalformedActiveTask(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirReadyReview, "broken.md", strings.Join([]string{
		"<!-- branch: task/broken -->",
		"---",
		"priority: [oops",
		"---",
		"# Broken",
		"",
	}, "\n"))

	idx := BuildIndex(tasksDir)
	if _, ok := idx.ActiveBranches()["task/broken"]; !ok {
		t.Fatal("expected malformed active task branch to be tracked")
	}
}

func TestBuildIndex_BacklogParseFailures(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "bad.md", "---\npriority: [oops\n---\n# Bad\n")
	idx := BuildIndex(tasksDir)
	failures := idx.BacklogParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 backlog parse failure, got %d", len(failures))
	}
	if failures[0].Filename != "bad.md" {
		t.Fatalf("failure filename = %q, want bad.md", failures[0].Filename)
	}
}

func TestBuildIndex_ReviewParseFailures(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirReadyReview, "bad-review.md", "---\npriority: [oops\n---\n# Bad Review\n")
	writeTask(t, tasksDir, DirReadyReview, "good-review.md", "---\npriority: 5\n---\n# Good Review\n")
	idx := BuildIndex(tasksDir)
	failures := idx.ReviewParseFailures()
	if len(failures) != 1 {
		t.Fatalf("expected 1 review parse failure, got %d", len(failures))
	}
	if failures[0].Filename != "bad-review.md" {
		t.Fatalf("failure filename = %q, want bad-review.md", failures[0].Filename)
	}

	// Good task should still be indexed.
	snap := idx.Snapshot(DirReadyReview, "good-review.md")
	if snap == nil {
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
	writeTask(t, tasksDir, DirInProgress, "my-task.md", content)

	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(DirInProgress, "my-task.md")
	if snap == nil {
		t.Fatal("expected my-task.md to be indexed")
	}
	if snap.Meta.ID != "my-task" {
		t.Errorf("ID = %q, want my-task", snap.Meta.ID)
	}
	if snap.Meta.Priority != 10 {
		t.Errorf("Priority = %d, want 10", snap.Meta.Priority)
	}
	if snap.Branch != "task/my-task" {
		t.Errorf("Branch = %q, want task/my-task", snap.Branch)
	}
	if !reflect.DeepEqual(snap.Meta.Affects, []string{"main.go"}) {
		t.Errorf("Affects = %v, want [main.go]", snap.Meta.Affects)
	}

	// Should be in active branches.
	if _, ok := idx.activeBranches["task/my-task"]; !ok {
		t.Error("expected task/my-task in active branches")
	}
}

func TestBuildIndex_ConsistencyWithParseTaskFile(t *testing.T) {
	// Verify that BuildIndex produces the same metadata as ParseTaskFile
	// for a representative task.
	tasksDir := setupIndexDirs(t)
	content := `---
id: consistency-check
priority: 7
depends_on: [dep-a]
affects: [api, cli]
tags: [bug]
max_retries: 5
---
# Title
Body.
`
	writeTask(t, tasksDir, DirBacklog, "consistency-check.md", content)

	idx := BuildIndex(tasksDir)
	snap := idx.Snapshot(DirBacklog, "consistency-check.md")
	if snap == nil {
		t.Fatal("expected snapshot")
	}

	meta, body, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, DirBacklog, "consistency-check.md"))
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	if !reflect.DeepEqual(snap.Meta, meta) {
		t.Errorf("meta mismatch:\n  index: %#v\n  parse: %#v", snap.Meta, meta)
	}
	if snap.Body != body {
		t.Errorf("body mismatch:\n  index: %q\n  parse: %q", snap.Body, body)
	}
}

func TestBuildIndex_MalformedCompletedTaskStemInIDs(t *testing.T) {
	// A completed task with broken frontmatter should still have its
	// filename stem registered in CompletedIDs and AllIDs so that
	// depends_on references using the stem are satisfied.
	tasksDir := setupIndexDirs(t)

	// "priority: nope" triggers a parse error (non-integer priority).
	writeTask(t, tasksDir, DirCompleted, "broken-task.md", "---\npriority: nope\n---\nBody.\n")
	writeTask(t, tasksDir, DirCompleted, "good-task.md", "---\npriority: 5\n---\nDone.\n")

	idx := BuildIndex(tasksDir)

	// Filename stem must be present despite parse failure.
	if _, ok := idx.CompletedIDs()["broken-task"]; !ok {
		t.Error("expected \"broken-task\" stem in CompletedIDs")
	}
	if _, ok := idx.AllIDs()["broken-task"]; !ok {
		t.Error("expected \"broken-task\" stem in AllIDs")
	}

	// Parse failure should still be recorded.
	failures := idx.ParseFailures()
	found := false
	for _, pf := range failures {
		if pf.Filename == "broken-task.md" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected broken-task.md in ParseFailures")
	}

	// Good task should be fully indexed as usual.
	if _, ok := idx.CompletedIDs()["good-task"]; !ok {
		t.Error("expected \"good-task\" stem in CompletedIDs")
	}
	snap := idx.Snapshot(DirCompleted, "good-task.md")
	if snap == nil {
		t.Fatal("expected good-task.md snapshot")
	}
}

func TestBuildIndex_MalformedNonCompletedTaskStemInIDs(t *testing.T) {
	// Same as above but for a non-completed directory. The filename stem
	// should appear in NonCompletedIDs and AllIDs.
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "bad-backlog.md", "---\npriority: nope\n---\nBody.\n")

	idx := BuildIndex(tasksDir)

	if _, ok := idx.NonCompletedIDs()["bad-backlog"]; !ok {
		t.Error("expected \"bad-backlog\" stem in NonCompletedIDs")
	}
	if _, ok := idx.AllIDs()["bad-backlog"]; !ok {
		t.Error("expected \"bad-backlog\" stem in AllIDs")
	}
	// Should NOT appear in CompletedIDs.
	if _, ok := idx.CompletedIDs()["bad-backlog"]; ok {
		t.Error("\"bad-backlog\" should not be in CompletedIDs")
	}
}

func TestEnsureIndex_NilDoesNotWriteStderr(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	// Capture stderr to verify ensureIndex does not emit warnings
	// when nil is passed (the designed-in backward compat path).
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	idx := ensureIndex(tasksDir, nil)

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

	// Active task with a directory prefix.
	writeTask(t, tasksDir, DirInProgress, "refactor-client.md",
		"---\naffects:\n  - pkg/client/\n---\n# Refactor\n")
	// Active task with an exact file.
	writeTask(t, tasksDir, DirInProgress, "fix-merge.md",
		"---\naffects:\n  - internal/merge/merge.go\n---\n# Fix\n")

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
			got := idx.HasActiveOverlap(tt.affects)
			if got != tt.want {
				t.Errorf("HasActiveOverlap(%v) = %v, want %v", tt.affects, got, tt.want)
			}
		})
	}
}

func TestHasActiveOverlap_GlobVsExact(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	// Active task with a glob pattern.
	writeTask(t, tasksDir, DirInProgress, "refactor-runner.md",
		"---\naffects:\n  - internal/runner/*.go\n---\n# Refactor\n")

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

	writeTask(t, tasksDir, DirInProgress, "glob-task.md",
		"---\naffects:\n  - internal/runner/*.go\n---\n# Glob\n")

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

	// Active task with exact file path.
	writeTask(t, tasksDir, DirInProgress, "fix-file.md",
		"---\naffects:\n  - internal/runner/task.go\n---\n# Fix\n")

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

	// Active task with directory prefix.
	writeTask(t, tasksDir, DirInProgress, "refactor-client.md",
		"---\naffects:\n  - pkg/client/\n---\n# Refactor\n")

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

	writeTask(t, tasksDir, DirInProgress, "glob-task.md",
		"---\naffects:\n  - internal/runner/*.go\n---\n# Glob\n")

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

	writeTask(t, tasksDir, DirInProgress, "fix-task.md",
		"---\naffects:\n  - internal/runner/task.go\n---\n# Fix\n")
	writeTask(t, tasksDir, DirReadyReview, "prefix-task.md",
		"---\naffects:\n  - pkg/client/\n---\n# Prefix\n")

	idx := BuildIndex(tasksDir)

	// **/*.go has empty static prefix, so it should conflict with everything.
	if !idx.HasActiveOverlap([]string{"**/*.go"}) {
		t.Error("HasActiveOverlap(**/*.go) = false, want true (empty static prefix = always conflicts)")
	}
}

func TestHasActiveOverlap_ExactFastPath(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	// No globs or prefixes — pure exact paths, fast path should work.
	writeTask(t, tasksDir, DirInProgress, "exact-task.md",
		"---\naffects:\n  - main.go\n  - internal/foo.go\n---\n# Exact\n")

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

func TestBuildIndex_ActiveAffectsGlobsDeduplicated(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	writeTask(t, tasksDir, DirInProgress, "task-a.md",
		"---\naffects:\n  - internal/runner/*.go\n---\n# A\n")
	writeTask(t, tasksDir, DirReadyReview, "task-b.md",
		"---\naffects:\n  - internal/runner/*.go\n  - pkg/**/*.go\n---\n# B\n")

	idx := BuildIndex(tasksDir)
	want := []string{"internal/runner/*.go", "pkg/**/*.go"}
	got := append([]string(nil), idx.activeAffectsGlobs...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activeAffectsGlobs = %v, want %v", got, want)
	}
}

func TestBuildIndex_ActiveAffectsPrefixesDeduplicated(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	writeTask(t, tasksDir, DirInProgress, "task-a.md",
		"---\naffects:\n  - pkg/client/\n---\n# A\n")
	writeTask(t, tasksDir, DirReadyReview, "task-b.md",
		"---\naffects:\n  - pkg/client/\n  - internal/server/\n---\n# B\n")

	idx := BuildIndex(tasksDir)
	want := []string{"internal/server/", "pkg/client/"}
	got := append([]string(nil), idx.activeAffectsPrefixes...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activeAffectsPrefixes = %v, want %v", got, want)
	}
}

func TestBuildIndex_InvalidGlobStillIndexedWithWarning(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	// Active task with an invalid glob (unclosed bracket) and a valid
	// exact path. The task should be fully indexed (NOT a parse failure)
	// so its affects remain visible to overlap detection.
	writeTask(t, tasksDir, DirInProgress, "bad-glob.md",
		"---\naffects:\n  - \"internal/[bad\"\n  - main.go\n---\n# Bad glob\n")

	idx := BuildIndex(tasksDir)

	// Task must be indexed.
	snap := idx.Snapshot(DirInProgress, "bad-glob.md")
	if snap == nil {
		t.Fatal("expected bad-glob.md to be indexed, got nil")
	}
	if !reflect.DeepEqual(snap.Meta.Affects, []string{"internal/[bad", "main.go"}) {
		t.Fatalf("Affects = %v, want [internal/[bad main.go]", snap.Meta.Affects)
	}

	// The invalid glob should appear in activeAffects for exact-match
	// lookup (literal string).
	if len(idx.activeAffects["internal/[bad"]) == 0 {
		t.Error("expected invalid glob string in activeAffects map")
	}
	if len(idx.activeAffects["main.go"]) == 0 {
		t.Error("expected main.go in activeAffects map")
	}

	// Must NOT be a parse failure.
	for _, pf := range idx.ParseFailures() {
		if pf.Filename == "bad-glob.md" {
			t.Fatal("bad-glob.md should not be in ParseFailures")
		}
	}

	// Must emit a build warning about the invalid glob.
	warnings := idx.BuildWarnings()
	found := false
	for _, w := range warnings {
		if strings.Contains(w.Err.Error(), "invalid glob in affects") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected build warning about invalid glob, got %v", warnings)
	}
}

func TestBuildIndex_GlobTrailingSlashStillIndexedWithWarning(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	writeTask(t, tasksDir, DirInProgress, "glob-slash.md",
		"---\naffects:\n  - \"internal/*/\"\n---\n# Glob slash\n")

	idx := BuildIndex(tasksDir)

	snap := idx.Snapshot(DirInProgress, "glob-slash.md")
	if snap == nil {
		t.Fatal("expected glob-slash.md to be indexed")
	}

	warnings := idx.BuildWarnings()
	found := false
	for _, w := range warnings {
		if strings.Contains(w.Err.Error(), "combines glob syntax with trailing /") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected build warning about glob with trailing /, got %v", warnings)
	}
}

func TestHasActiveOverlap_InvalidGlobBlocksOverlapping(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	// Active task with invalid glob syntax. Should conservatively block
	// any incoming task whose affects share the static prefix.
	writeTask(t, tasksDir, DirInProgress, "bad-glob.md",
		"---\naffects:\n  - \"internal/[bad\"\n---\n# Bad glob\n")

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

	// Active task with glob + trailing slash (invalid combination).
	// Should conservatively block files under the static prefix.
	writeTask(t, tasksDir, DirInProgress, "glob-slash.md",
		"---\naffects:\n  - \"internal/*/\"\n---\n# Glob slash\n")

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

	// Active task with invalid glob syntax.
	writeTask(t, tasksDir, DirInProgress, "bad-glob.md",
		"---\naffects:\n  - \"internal/[bad\"\n---\n# Bad glob\n")

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

// --- Defensive copy tests ---

func TestCompletedIDs_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirCompleted, "done-task.md", "# Done\n")

	idx := BuildIndex(tasksDir)

	ids := idx.CompletedIDs()
	if _, ok := ids["done-task"]; !ok {
		t.Fatal("expected done-task in CompletedIDs")
	}

	// Mutate the returned map.
	ids["injected"] = struct{}{}
	delete(ids, "done-task")

	// The index must be unaffected.
	ids2 := idx.CompletedIDs()
	if _, ok := ids2["done-task"]; !ok {
		t.Error("CompletedIDs was corrupted by caller mutation: done-task missing")
	}
	if _, ok := ids2["injected"]; ok {
		t.Error("CompletedIDs was corrupted by caller mutation: injected key present")
	}
}

func TestNonCompletedIDs_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "bl-task.md", "# Backlog\n")

	idx := BuildIndex(tasksDir)

	ids := idx.NonCompletedIDs()
	if _, ok := ids["bl-task"]; !ok {
		t.Fatal("expected bl-task in NonCompletedIDs")
	}

	ids["injected"] = struct{}{}
	delete(ids, "bl-task")

	ids2 := idx.NonCompletedIDs()
	if _, ok := ids2["bl-task"]; !ok {
		t.Error("NonCompletedIDs was corrupted by caller mutation: bl-task missing")
	}
	if _, ok := ids2["injected"]; ok {
		t.Error("NonCompletedIDs was corrupted by caller mutation: injected key present")
	}
}

func TestAllIDs_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirBacklog, "any-task.md", "# Any\n")

	idx := BuildIndex(tasksDir)

	ids := idx.AllIDs()
	if _, ok := ids["any-task"]; !ok {
		t.Fatal("expected any-task in AllIDs")
	}

	ids["injected"] = struct{}{}
	delete(ids, "any-task")

	ids2 := idx.AllIDs()
	if _, ok := ids2["any-task"]; !ok {
		t.Error("AllIDs was corrupted by caller mutation: any-task missing")
	}
	if _, ok := ids2["injected"]; ok {
		t.Error("AllIDs was corrupted by caller mutation: injected key present")
	}
}

func TestActiveBranches_DefensiveCopy(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, DirInProgress, "ip-task.md",
		"<!-- branch: task/ip-task -->\n# In Progress\n")

	idx := BuildIndex(tasksDir)

	branches := idx.ActiveBranches()
	if _, ok := branches["task/ip-task"]; !ok {
		t.Fatal("expected task/ip-task in ActiveBranches")
	}

	branches["injected"] = struct{}{}
	delete(branches, "task/ip-task")

	branches2 := idx.ActiveBranches()
	if _, ok := branches2["task/ip-task"]; !ok {
		t.Error("ActiveBranches was corrupted by caller mutation: task/ip-task missing")
	}
	if _, ok := branches2["injected"]; ok {
		t.Error("ActiveBranches was corrupted by caller mutation: injected key present")
	}
}
