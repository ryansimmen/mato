package queue

import (
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
)

func TestFormatDependencyBlocks(t *testing.T) {
	tests := []struct {
		name   string
		blocks []DependencyBlock
		want   string
	}{
		{
			name:   "empty slice returns none",
			blocks: []DependencyBlock{},
			want:   "none",
		},
		{
			name:   "nil slice returns none",
			blocks: nil,
			want:   "none",
		},
		{
			name: "single block with failed state",
			blocks: []DependencyBlock{
				{DependencyID: "setup-db", State: "failed"},
			},
			want: "setup-db (failed)",
		},
		{
			name: "single block with missing state",
			blocks: []DependencyBlock{
				{DependencyID: "init-config", State: "unknown"},
			},
			want: "init-config (unknown)",
		},
		{
			name: "multiple blocks with mixed states",
			blocks: []DependencyBlock{
				{DependencyID: "task-a", State: "failed"},
				{DependencyID: "task-b", State: "unknown"},
				{DependencyID: "task-c", State: "in-progress"},
				{DependencyID: "task-d", State: "ambiguous"},
			},
			want: "task-a (failed), task-b (unknown), task-c (in-progress), task-d (ambiguous)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDependencyBlocks(tt.blocks)
			if got != tt.want {
				t.Errorf("FormatDependencyBlocks() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDependencyBlockedBacklogTasksDetailed_NoBlockedTasks(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	// A backlog task with no dependencies should not be blocked.
	writeTask(t, tasksDir, dirs.Backlog, "simple-task.md", `---
priority: 10
---
# Simple Task
`)
	idx := BuildIndex(tasksDir)
	blocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	if len(blocked) != 0 {
		t.Fatalf("expected 0 blocked tasks, got %d: %v", len(blocked), blocked)
	}
}

func TestDependencyBlockedBacklogTasksDetailed_MissingDependency(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	// A backlog task depending on a task that doesn't exist anywhere.
	writeTask(t, tasksDir, dirs.Backlog, "needs-missing.md", `---
priority: 10
depends_on:
  - nonexistent-task
---
# Needs Missing
`)
	idx := BuildIndex(tasksDir)
	blocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	blocks, ok := blocked["needs-missing.md"]
	if !ok {
		t.Fatal("expected needs-missing.md to be blocked")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].DependencyID != "nonexistent-task" {
		t.Errorf("DependencyID = %q, want %q", blocks[0].DependencyID, "nonexistent-task")
	}
	if blocks[0].State != "unknown" {
		t.Errorf("State = %q, want %q", blocks[0].State, "unknown")
	}
}

func TestDependencyBlockedBacklogTasksDetailed_FailedDependency(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	// The dependency task is in the failed directory.
	writeTask(t, tasksDir, dirs.Failed, "setup-db.md", `---
id: setup-db
priority: 5
---
# Setup DB
`)
	writeTask(t, tasksDir, dirs.Backlog, "migrate-data.md", `---
priority: 10
depends_on:
  - setup-db
---
# Migrate Data
`)
	idx := BuildIndex(tasksDir)
	blocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	blocks, ok := blocked["migrate-data.md"]
	if !ok {
		t.Fatal("expected migrate-data.md to be blocked")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].DependencyID != "setup-db" {
		t.Errorf("DependencyID = %q, want %q", blocks[0].DependencyID, "setup-db")
	}
	if !strings.Contains(blocks[0].State, dirs.Failed) {
		t.Errorf("State = %q, want it to contain %q", blocks[0].State, dirs.Failed)
	}
}

func TestDependencyBlockedBacklogTasksDetailed_AmbiguousDependency(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	// The dependency exists in both completed and another active directory,
	// making the ID ambiguous.
	writeTask(t, tasksDir, dirs.Completed, "shared-id.md", `---
id: shared-id
priority: 5
---
# Shared ID (completed)
`)
	writeTask(t, tasksDir, dirs.Backlog, "shared-id-v2.md", `---
id: shared-id
priority: 5
---
# Shared ID (backlog duplicate)
`)
	writeTask(t, tasksDir, dirs.Backlog, "consumer.md", `---
priority: 20
depends_on:
  - shared-id
---
# Consumer
`)
	idx := BuildIndex(tasksDir)
	blocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	blocks, ok := blocked["consumer.md"]
	if !ok {
		t.Fatal("expected consumer.md to be blocked")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].DependencyID != "shared-id" {
		t.Errorf("DependencyID = %q, want %q", blocks[0].DependencyID, "shared-id")
	}
	if blocks[0].State != "ambiguous" {
		t.Errorf("State = %q, want %q", blocks[0].State, "ambiguous")
	}
}

func TestDependencyBlockedBacklogTasksDetailed_CompletedDependency(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	// A completed dependency should NOT block the dependent task.
	writeTask(t, tasksDir, dirs.Completed, "prerequisite.md", `---
id: prerequisite
priority: 5
---
# Prerequisite
`)
	writeTask(t, tasksDir, dirs.Backlog, "depends-on-completed.md", `---
priority: 10
depends_on:
  - prerequisite
---
# Depends On Completed
`)
	idx := BuildIndex(tasksDir)
	blocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	if _, ok := blocked["depends-on-completed.md"]; ok {
		t.Fatal("expected depends-on-completed.md to NOT be blocked since prerequisite is completed")
	}
}

func TestDependencyBlockedBacklogTasksDetailed_InProgressDependency(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	// A dependency that is in-progress should block the dependent task.
	writeTask(t, tasksDir, dirs.InProgress, "running-task.md", `---
id: running-task
priority: 5
---
# Running Task
`)
	writeTask(t, tasksDir, dirs.Backlog, "waits-for-running.md", `---
priority: 10
depends_on:
  - running-task
---
# Waits For Running
`)
	idx := BuildIndex(tasksDir)
	blocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	blocks, ok := blocked["waits-for-running.md"]
	if !ok {
		t.Fatal("expected waits-for-running.md to be blocked")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].DependencyID != "running-task" {
		t.Errorf("DependencyID = %q, want %q", blocks[0].DependencyID, "running-task")
	}
	if !strings.Contains(blocks[0].State, dirs.InProgress) {
		t.Errorf("State = %q, want it to contain %q", blocks[0].State, dirs.InProgress)
	}
}
