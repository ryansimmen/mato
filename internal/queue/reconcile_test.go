package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/taskfile"
)

// setupTasksDirs creates the standard queue directories under a temp dir.
func setupTasksDirs(t *testing.T) string {
	t.Helper()
	tasksDir := t.TempDir()
	for _, sub := range AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	return tasksDir
}

func TestReconcileReadyQueue_PromotesSatisfiedDeps(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Completed dependency.
	writeTask(t, tasksDir, DirCompleted, "dep-done.md",
		"---\nid: dep-done\n---\n# Dep\n")

	// Waiting task whose only dep is completed.
	writeTask(t, tasksDir, DirWaiting, "consumer.md",
		"---\nid: consumer\ndepends_on: [dep-done]\n---\n# Consumer\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	// consumer.md should now be in backlog/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "consumer.md")); err != nil {
		t.Fatalf("consumer.md not found in backlog/: %v", err)
	}
	// Should not remain in waiting/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "consumer.md")); !os.IsNotExist(err) {
		t.Fatalf("consumer.md still in waiting/")
	}
}

func TestReconcileReadyQueue_UnsatisfiedDepsRemain(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Waiting task depends on something that is NOT completed.
	writeTask(t, tasksDir, DirWaiting, "blocked.md",
		"---\nid: blocked\ndepends_on: [not-done-yet]\n---\n# Blocked\n")
	// The dependency exists in backlog (not completed).
	writeTask(t, tasksDir, DirBacklog, "not-done-yet.md",
		"---\nid: not-done-yet\n---\n# Not done\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if moved {
		t.Fatalf("moved = %v, want false", moved)
	}

	// blocked.md should still be in waiting/.
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "blocked.md")); err != nil {
		t.Fatalf("blocked.md should remain in waiting/: %v", err)
	}
}

func TestReconcileReadyQueue_NoDepsPromoted(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Waiting task with no dependencies should be promoted.
	writeTask(t, tasksDir, DirWaiting, "no-deps.md",
		"---\nid: no-deps\n---\n# No deps\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "no-deps.md")); err != nil {
		t.Fatalf("no-deps.md not found in backlog/: %v", err)
	}
}

func TestReconcileReadyQueue_CycleDetection(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Create a 2-node cycle.
	writeTask(t, tasksDir, DirWaiting, "cycle-a.md",
		"---\nid: cycle-a\ndepends_on: [cycle-b]\n---\n# Cycle A\n")
	writeTask(t, tasksDir, DirWaiting, "cycle-b.md",
		"---\nid: cycle-b\ndepends_on: [cycle-a]\n---\n# Cycle B\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	// Both cycle members should be in failed/.
	for _, name := range []string{"cycle-a.md", "cycle-b.md"} {
		failedPath := filepath.Join(tasksDir, DirFailed, name)
		data, err := os.ReadFile(failedPath)
		if err != nil {
			t.Fatalf("%s not found in failed/: %v", name, err)
		}
		if !taskfile.ContainsCycleFailure(data) {
			t.Errorf("%s in failed/ does not contain cycle-failure marker", name)
		}
	}
}

func TestReconcileReadyQueue_SelfCycle(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, DirWaiting, "self-ref.md",
		"---\nid: self-ref\ndepends_on: [self-ref]\n---\n# Self\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	failedPath := filepath.Join(tasksDir, DirFailed, "self-ref.md")
	data, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("self-ref.md not found in failed/: %v", err)
	}
	if !taskfile.ContainsCycleFailure(data) {
		t.Error("self-ref.md should have cycle-failure marker")
	}
}

func TestReconcileReadyQueue_InvalidGlobQuarantined(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Waiting task with invalid glob in affects — should be quarantined to failed/.
	writeTask(t, tasksDir, DirWaiting, "bad-glob.md",
		"---\nid: bad-glob\naffects:\n  - \"[invalid\"\n---\n# Bad glob\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "bad-glob.md")); err != nil {
		t.Fatalf("bad-glob.md not found in failed/: %v", err)
	}
}

func TestReconcileReadyQueue_InvalidGlobInBacklogQuarantined(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Backlog task with invalid glob in affects — should be quarantined to failed/.
	writeTask(t, tasksDir, DirBacklog, "bad-glob-backlog.md",
		"---\nid: bad-glob-backlog\naffects:\n  - \"[invalid\"\n---\n# Bad glob backlog\n")

	ReconcileReadyQueue(tasksDir, nil)

	if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, "bad-glob-backlog.md")); err != nil {
		t.Fatalf("bad-glob-backlog.md not found in failed/: %v", err)
	}
}

func TestReconcileReadyQueue_Idempotency(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, DirCompleted, "dep.md",
		"---\nid: dep\n---\n# Dep\n")
	writeTask(t, tasksDir, DirWaiting, "task-a.md",
		"---\nid: task-a\ndepends_on: [dep]\n---\n# A\n")
	writeTask(t, tasksDir, DirWaiting, "task-b.md",
		"---\nid: task-b\ndepends_on: [dep]\n---\n# B\n")

	moved1 := ReconcileReadyQueue(tasksDir, nil)
	if !moved1 {
		t.Fatalf("first pass moved = %v, want true", moved1)
	}

	// Second call should move nothing (all already moved).
	moved2 := ReconcileReadyQueue(tasksDir, nil)
	if moved2 {
		t.Fatalf("second pass moved = %v, want false", moved2)
	}

	// Both should be in backlog/.
	for _, name := range []string{"task-a.md", "task-b.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, name)); err != nil {
			t.Fatalf("%s not found in backlog/ after idempotent call: %v", name, err)
		}
	}
}

func TestReconcileReadyQueue_CycleIdempotency(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, DirWaiting, "cyc-x.md",
		"---\nid: cyc-x\ndepends_on: [cyc-y]\n---\n# X\n")
	writeTask(t, tasksDir, DirWaiting, "cyc-y.md",
		"---\nid: cyc-y\ndepends_on: [cyc-x]\n---\n# Y\n")

	ReconcileReadyQueue(tasksDir, nil)

	// Read the cycle-failure content after first pass.
	dataX, _ := os.ReadFile(filepath.Join(tasksDir, DirFailed, "cyc-x.md"))
	dataY, _ := os.ReadFile(filepath.Join(tasksDir, DirFailed, "cyc-y.md"))

	// Second pass should not duplicate cycle-failure markers.
	ReconcileReadyQueue(tasksDir, nil)

	dataX2, _ := os.ReadFile(filepath.Join(tasksDir, DirFailed, "cyc-x.md"))
	dataY2, _ := os.ReadFile(filepath.Join(tasksDir, DirFailed, "cyc-y.md"))

	if string(dataX) != string(dataX2) {
		t.Error("cyc-x.md content changed after second reconcile pass")
	}
	if string(dataY) != string(dataY2) {
		t.Error("cyc-y.md content changed after second reconcile pass")
	}
}

func TestReconcileReadyQueue_ParseFailuresMovedToFailed(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Invalid YAML frontmatter in waiting/.
	writeTask(t, tasksDir, DirWaiting, "bad-yaml.md",
		"---\nbad: [unclosed\n---\n# Bad YAML\n")

	// Invalid YAML frontmatter in backlog/.
	writeTask(t, tasksDir, DirBacklog, "bad-backlog.md",
		"---\nbad: [unclosed\n---\n# Bad Backlog\n")

	ReconcileReadyQueue(tasksDir, nil)

	for _, name := range []string{"bad-yaml.md", "bad-backlog.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, DirFailed, name)); err != nil {
			t.Errorf("%s not found in failed/: %v", name, err)
		}
	}
}

func TestReconcileReadyQueue_ActiveOverlapBlocksPromotion(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Active task in in-progress with affects.
	writeTask(t, tasksDir, DirInProgress, "active.md",
		"---\nid: active\naffects:\n  - src/main.go\n---\n# Active\n")

	// Waiting task has overlapping affects — should NOT be promoted.
	writeTask(t, tasksDir, DirWaiting, "overlap.md",
		"---\nid: overlap\naffects:\n  - src/main.go\n---\n# Overlap\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if moved {
		t.Fatalf("moved = %v, want false", moved)
	}

	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "overlap.md")); err != nil {
		t.Fatalf("overlap.md should remain in waiting/: %v", err)
	}
}

func TestReconcileReadyQueue_MixedTasks(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Setup: one completed dep, one waiting with satisfied dep, one waiting blocked.
	writeTask(t, tasksDir, DirCompleted, "done.md",
		"---\nid: done\n---\n# Done\n")
	writeTask(t, tasksDir, DirWaiting, "ready.md",
		"---\nid: ready\ndepends_on: [done]\n---\n# Ready\n")
	writeTask(t, tasksDir, DirWaiting, "still-waiting.md",
		"---\nid: still-waiting\ndepends_on: [not-completed]\n---\n# Waiting\n")
	writeTask(t, tasksDir, DirBacklog, "not-completed.md",
		"---\nid: not-completed\n---\n# Not done\n")

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, "ready.md")); err != nil {
		t.Fatalf("ready.md not found in backlog/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, DirWaiting, "still-waiting.md")); err != nil {
		t.Fatalf("still-waiting.md should remain in waiting/: %v", err)
	}
}

func TestCountPromotableWaitingTasks(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(tasksDir string)
		wantCount int
	}{
		{
			name: "no waiting tasks",
			setup: func(tasksDir string) {
				// Empty queue.
			},
			wantCount: 0,
		},
		{
			name: "one promotable task",
			setup: func(tasksDir string) {
				os.WriteFile(filepath.Join(tasksDir, DirWaiting, "task.md"),
					[]byte("---\nid: task\n---\n# Task\n"), 0o644)
			},
			wantCount: 1,
		},
		{
			name: "blocked task not counted",
			setup: func(tasksDir string) {
				os.WriteFile(filepath.Join(tasksDir, DirWaiting, "blocked.md"),
					[]byte("---\nid: blocked\ndepends_on: [missing]\n---\n# Blocked\n"), 0o644)
			},
			wantCount: 0,
		},
		{
			name: "invalid glob not counted",
			setup: func(tasksDir string) {
				os.WriteFile(filepath.Join(tasksDir, DirWaiting, "bad.md"),
					[]byte("---\nid: bad\naffects:\n  - \"[invalid\"\n---\n# Bad\n"), 0o644)
			},
			wantCount: 0,
		},
		{
			name: "mix of promotable and blocked",
			setup: func(tasksDir string) {
				os.WriteFile(filepath.Join(tasksDir, DirCompleted, "dep.md"),
					[]byte("---\nid: dep\n---\n# Dep\n"), 0o644)
				os.WriteFile(filepath.Join(tasksDir, DirWaiting, "ready.md"),
					[]byte("---\nid: ready\ndepends_on: [dep]\n---\n# Ready\n"), 0o644)
				os.WriteFile(filepath.Join(tasksDir, DirWaiting, "blocked.md"),
					[]byte("---\nid: blocked\ndepends_on: [nope]\n---\n# Blocked\n"), 0o644)
			},
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := setupTasksDirs(t)
			tt.setup(tasksDir)
			got := CountPromotableWaitingTasks(tasksDir, nil)
			if got != tt.wantCount {
				t.Errorf("CountPromotableWaitingTasks() = %d, want %d", got, tt.wantCount)
			}
		})
	}
}

func TestReconcileReadyQueue_EmptyQueue(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	moved := ReconcileReadyQueue(tasksDir, nil)
	if moved {
		t.Fatalf("moved = %v, want false", moved)
	}
}

func TestResolvePromotableTasks_WithActiveOverlap(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Active task claims "src/".
	writeTask(t, tasksDir, DirInProgress, "active.md",
		"---\nid: active\naffects:\n  - src/\n---\n# Active\n")

	// Waiting task's affects falls under the active prefix.
	writeTask(t, tasksDir, DirWaiting, "overlapping.md",
		"---\nid: overlapping\naffects:\n  - src/foo.go\n---\n# Overlapping\n")

	result := resolvePromotableTasks(tasksDir, nil)
	if len(result) != 0 {
		t.Fatalf("resolvePromotableTasks returned %d tasks, want 0 (active overlap)", len(result))
	}
}

func TestResolvePromotableTasks_NoOverlap(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Active task claims "pkg/".
	writeTask(t, tasksDir, DirInProgress, "active.md",
		"---\nid: active\naffects:\n  - pkg/\n---\n# Active\n")

	// Waiting task's affects is in a different directory.
	writeTask(t, tasksDir, DirWaiting, "nonoverlapping.md",
		"---\nid: nonoverlapping\naffects:\n  - cmd/main.go\n---\n# Non-overlapping\n")

	result := resolvePromotableTasks(tasksDir, nil)
	if len(result) != 1 {
		t.Fatalf("resolvePromotableTasks returned %d tasks, want 1", len(result))
	}
	if result[0].name != "nonoverlapping.md" {
		t.Errorf("promoted task = %q, want nonoverlapping.md", result[0].name)
	}
}

func TestReconcileReadyQueue_MultiplePromotions(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// Multiple waiting tasks with no deps — all should be promoted.
	for _, name := range []string{"aaa.md", "bbb.md", "ccc.md"} {
		id := strings.TrimSuffix(name, ".md")
		writeTask(t, tasksDir, DirWaiting, name,
			"---\nid: "+id+"\n---\n# "+id+"\n")
	}

	moved := ReconcileReadyQueue(tasksDir, nil)
	if !moved {
		t.Fatalf("moved = %v, want true", moved)
	}

	for _, name := range []string{"aaa.md", "bbb.md", "ccc.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, DirBacklog, name)); err != nil {
			t.Errorf("%s not found in backlog/: %v", name, err)
		}
	}
}

func TestReconcileReadyQueue_ParseFailureHasTerminalMarker(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, DirWaiting, "bad-yaml.md",
		"---\nbad: [unclosed\n---\n# Bad YAML\n")

	ReconcileReadyQueue(tasksDir, nil)

	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "bad-yaml.md"))
	if err != nil {
		t.Fatalf("bad-yaml.md not found in failed/: %v", err)
	}
	if !taskfile.ContainsTerminalFailure(data) {
		t.Error("bad-yaml.md in failed/ should contain terminal-failure marker")
	}
	if !strings.Contains(string(data), "unparseable frontmatter") {
		t.Error("terminal-failure marker should mention unparseable frontmatter")
	}
}

func TestReconcileReadyQueue_InvalidGlobHasTerminalMarker(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	writeTask(t, tasksDir, DirBacklog, "bad-glob.md",
		"---\nid: bad-glob\naffects:\n  - \"[invalid\"\n---\n# Bad glob\n")

	ReconcileReadyQueue(tasksDir, nil)

	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "bad-glob.md"))
	if err != nil {
		t.Fatalf("bad-glob.md not found in failed/: %v", err)
	}
	if !taskfile.ContainsTerminalFailure(data) {
		t.Error("bad-glob.md in failed/ should contain terminal-failure marker")
	}
	if !strings.Contains(string(data), "invalid glob syntax") {
		t.Error("terminal-failure marker should mention invalid glob syntax")
	}
}

func TestReconcileReadyQueue_WaitingInvalidGlobHasTerminalMarker(t *testing.T) {
	tasksDir := setupTasksDirs(t)

	// A waiting task with satisfied deps but invalid glob — quarantined during
	// the promotion pass rather than the backlog scan.
	writeTask(t, tasksDir, DirCompleted, "dep.md",
		"---\nid: dep\n---\n# Dep\n")
	writeTask(t, tasksDir, DirWaiting, "bad-glob-wait.md",
		"---\nid: bad-glob-wait\ndepends_on: [dep]\naffects:\n  - \"[invalid\"\n---\n# Bad glob waiting\n")

	ReconcileReadyQueue(tasksDir, nil)

	data, err := os.ReadFile(filepath.Join(tasksDir, DirFailed, "bad-glob-wait.md"))
	if err != nil {
		t.Fatalf("bad-glob-wait.md not found in failed/: %v", err)
	}
	if !taskfile.ContainsTerminalFailure(data) {
		t.Error("bad-glob-wait.md in failed/ should contain terminal-failure marker")
	}
}
