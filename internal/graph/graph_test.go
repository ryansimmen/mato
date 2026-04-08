package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/dirs"
	"mato/internal/queueview"
	"mato/internal/testutil"
)

// writeTask creates a task file in the given state directory.
func writeTask(t *testing.T, tasksDir, state, filename, content string) {
	t.Helper()
	dir := filepath.Join(tasksDir, state)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setupTasksDir creates all queue subdirectories.
func setupTasksDir(t *testing.T) string {
	t.Helper()
	tasksDir := filepath.Join(t.TempDir(), ".mato")
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return tasksDir
}

func TestBuild_EmptyQueue(t *testing.T) {
	tasksDir := setupTasksDir(t)
	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	if len(data.Nodes) != 0 {
		t.Errorf("expected 0 nodes, got %d", len(data.Nodes))
	}
	if len(data.Edges) != 0 {
		t.Errorf("expected 0 edges, got %d", len(data.Edges))
	}
	if len(data.Cycles) != 0 {
		t.Errorf("expected 0 cycles, got %d", len(data.Cycles))
	}
}

func TestBuild_SingleTask(t *testing.T) {
	tasksDir := setupTasksDir(t)
	writeTask(t, tasksDir, "backlog", "my-task.md", "---\nid: my-task\npriority: 10\n---\n# My Task\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	if len(data.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(data.Nodes))
	}
	n := data.Nodes[0]
	if n.Key != "backlog/my-task.md" {
		t.Errorf("key = %q, want %q", n.Key, "backlog/my-task.md")
	}
	if n.ID != "my-task" {
		t.Errorf("id = %q, want %q", n.ID, "my-task")
	}
	if n.Title != "My Task" {
		t.Errorf("title = %q, want %q", n.Title, "My Task")
	}
	if n.State != StateBacklog {
		t.Errorf("state = %q, want %q", n.State, StateBacklog)
	}
	if n.Priority != 10 {
		t.Errorf("priority = %d, want 10", n.Priority)
	}
}

func TestBuild_LinearChain(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// A (completed) → B (backlog, depends on A) → C (waiting, depends on B)
	writeTask(t, tasksDir, "completed", "task-a.md", "---\nid: task-a\npriority: 10\n---\n# Task A\n")
	writeTask(t, tasksDir, "backlog", "task-b.md", "---\nid: task-b\npriority: 20\ndepends_on:\n  - task-a\n---\n# Task B\n")
	writeTask(t, tasksDir, "waiting", "task-c.md", "---\nid: task-c\npriority: 30\ndepends_on:\n  - task-b\n---\n# Task C\n")

	idx := queueview.BuildIndex(tasksDir)

	// showAll=false: completed not included
	data := Build(tasksDir, idx, false)
	nodeKeys := nodeKeySet(data)
	if _, ok := nodeKeys["completed/task-a.md"]; ok {
		t.Error("completed task should not be in graph when showAll=false")
	}
	if _, ok := nodeKeys["backlog/task-b.md"]; !ok {
		t.Error("backlog task should be in graph")
	}
	if _, ok := nodeKeys["waiting/task-c.md"]; !ok {
		t.Error("waiting task should be in graph")
	}

	// task-b should have hidden dep on task-a (satisfied)
	taskB := findNode(data, "backlog/task-b.md")
	if taskB == nil {
		t.Fatal("task-b not found")
	}
	if len(taskB.HiddenDeps) != 1 {
		t.Fatalf("task-b hidden deps = %d, want 1", len(taskB.HiddenDeps))
	}
	if taskB.HiddenDeps[0].DependencyID != "task-a" || taskB.HiddenDeps[0].Status != "satisfied" {
		t.Errorf("task-b hidden dep = %+v, want {task-a, satisfied}", taskB.HiddenDeps[0])
	}

	// task-c should have edge from task-b
	edges := edgesTo(data, "waiting/task-c.md")
	if len(edges) != 1 {
		t.Fatalf("task-c edges = %d, want 1", len(edges))
	}
	if edges[0].From != "backlog/task-b.md" {
		t.Errorf("edge from = %q, want %q", edges[0].From, "backlog/task-b.md")
	}
	if edges[0].Satisfied {
		t.Error("edge to waiting task-c from backlog task-b should not be satisfied")
	}
}

func TestBuild_LinearChain_ShowAll(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "completed", "task-a.md", "---\nid: task-a\npriority: 10\n---\n# Task A\n")
	writeTask(t, tasksDir, "backlog", "task-b.md", "---\nid: task-b\npriority: 20\ndepends_on:\n  - task-a\n---\n# Task B\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, true)

	nodeKeys := nodeKeySet(data)
	if _, ok := nodeKeys["completed/task-a.md"]; !ok {
		t.Error("completed task should be in graph when showAll=true")
	}

	// task-b should have edge to task-a, not hidden dep
	taskB := findNode(data, "backlog/task-b.md")
	if taskB == nil {
		t.Fatal("task-b not found")
	}
	if len(taskB.HiddenDeps) != 0 {
		t.Errorf("task-b hidden deps = %d, want 0 (should be edge)", len(taskB.HiddenDeps))
	}

	edges := edgesTo(data, "backlog/task-b.md")
	if len(edges) != 1 {
		t.Fatalf("task-b edges = %d, want 1", len(edges))
	}
	if edges[0].From != "completed/task-a.md" {
		t.Errorf("edge from = %q, want %q", edges[0].From, "completed/task-a.md")
	}
	if !edges[0].Satisfied {
		t.Error("edge from completed should be satisfied")
	}
}

func TestBuild_Diamond(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// A ← B, A ← C, B ← D, C ← D
	writeTask(t, tasksDir, "waiting", "a.md", "---\nid: a\npriority: 10\n---\n# A\n")
	writeTask(t, tasksDir, "waiting", "b.md", "---\nid: b\npriority: 20\ndepends_on:\n  - a\n---\n# B\n")
	writeTask(t, tasksDir, "waiting", "c.md", "---\nid: c\npriority: 20\ndepends_on:\n  - a\n---\n# C\n")
	writeTask(t, tasksDir, "waiting", "d.md", "---\nid: d\npriority: 30\ndepends_on:\n  - b\n  - c\n---\n# D\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	if len(data.Nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(data.Nodes))
	}

	// D should have edges from B and C
	dEdges := edgesTo(data, "waiting/d.md")
	if len(dEdges) != 2 {
		t.Fatalf("d edges = %d, want 2", len(dEdges))
	}
}

func TestBuild_CycleMutual(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "waiting", "x.md", "---\nid: x\npriority: 10\ndepends_on:\n  - y\n---\n# X\n")
	writeTask(t, tasksDir, "waiting", "y.md", "---\nid: y\npriority: 10\ndepends_on:\n  - x\n---\n# Y\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	if len(data.Cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(data.Cycles))
	}
	cycle := data.Cycles[0]
	if len(cycle) != 2 {
		t.Fatalf("cycle length = %d, want 2", len(cycle))
	}

	nodeX := findNode(data, "waiting/x.md")
	nodeY := findNode(data, "waiting/y.md")
	if nodeX == nil || nodeY == nil {
		t.Fatal("cycle nodes not found")
	}
	if !nodeX.IsCycleMember {
		t.Error("x should be cycle member")
	}
	if !nodeY.IsCycleMember {
		t.Error("y should be cycle member")
	}
}

func TestBuild_CycleSelf(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "waiting", "self.md", "---\nid: self\npriority: 10\ndepends_on:\n  - self\n---\n# Self\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	if len(data.Cycles) != 1 {
		t.Fatalf("expected 1 cycle, got %d", len(data.Cycles))
	}
	cycle := data.Cycles[0]
	if len(cycle) != 1 || cycle[0] != "waiting/self.md" {
		t.Errorf("cycle = %v, want [waiting/self.md]", cycle)
	}

	node := findNode(data, "waiting/self.md")
	if node == nil {
		t.Fatal("self node not found")
	}
	if !node.IsCycleMember {
		t.Error("self should be cycle member")
	}
}

func TestBuild_ShowAllFalse_HiddenCompleted(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "completed", "done.md", "---\nid: done\npriority: 10\n---\n# Done\n")
	writeTask(t, tasksDir, "backlog", "next.md", "---\nid: next\npriority: 20\ndepends_on:\n  - done\n---\n# Next\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	nodeKeys := nodeKeySet(data)
	if _, ok := nodeKeys["completed/done.md"]; ok {
		t.Error("completed node should not be in graph when showAll=false")
	}

	node := findNode(data, "backlog/next.md")
	if node == nil {
		t.Fatal("next node not found")
	}
	if len(node.HiddenDeps) != 1 {
		t.Fatalf("hidden deps = %d, want 1", len(node.HiddenDeps))
	}
	if node.HiddenDeps[0].Status != "satisfied" {
		t.Errorf("hidden dep status = %q, want %q", node.HiddenDeps[0].Status, "satisfied")
	}
}

func TestBuild_ShowAllFalse_HiddenFailed(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "failed", "broken.md", "---\nid: broken\npriority: 10\n---\n# Broken\n")
	writeTask(t, tasksDir, "backlog", "depends.md", "---\nid: depends\npriority: 20\ndepends_on:\n  - broken\n---\n# Depends\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	nodeKeys := nodeKeySet(data)
	if _, ok := nodeKeys["failed/broken.md"]; ok {
		t.Error("failed node should not be in graph when showAll=false")
	}

	node := findNode(data, "backlog/depends.md")
	if node == nil {
		t.Fatal("depends node not found")
	}
	if len(node.HiddenDeps) != 1 {
		t.Fatalf("hidden deps = %d, want 1", len(node.HiddenDeps))
	}
	if node.HiddenDeps[0].Status != "external" {
		t.Errorf("hidden dep status = %q, want %q", node.HiddenDeps[0].Status, "external")
	}
}

func TestBuild_ShowAllTrue_IncludesCompletedFailed(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "completed", "done.md", "---\nid: done\npriority: 10\n---\n# Done\n")
	writeTask(t, tasksDir, "failed", "broken.md", "---\nid: broken\npriority: 10\n---\n# Broken\n")
	writeTask(t, tasksDir, "backlog", "next.md", "---\nid: next\npriority: 20\ndepends_on:\n  - done\n  - broken\n---\n# Next\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, true)

	nodeKeys := nodeKeySet(data)
	if _, ok := nodeKeys["completed/done.md"]; !ok {
		t.Error("completed should be in graph when showAll=true")
	}
	if _, ok := nodeKeys["failed/broken.md"]; !ok {
		t.Error("failed should be in graph when showAll=true")
	}

	// next should have edges, not hidden deps
	node := findNode(data, "backlog/next.md")
	if node == nil {
		t.Fatal("next not found")
	}
	if len(node.HiddenDeps) != 0 {
		t.Errorf("hidden deps = %d, want 0", len(node.HiddenDeps))
	}

	edges := edgesTo(data, "backlog/next.md")
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
}

func TestBuild_AliasResolution_StemVsMetaID(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Task with meta.ID different from filename stem.
	writeTask(t, tasksDir, "backlog", "my-file.md", "---\nid: custom-id\npriority: 10\n---\n# My File\n")
	// Depends on stem reference.
	writeTask(t, tasksDir, "backlog", "dep-stem.md", "---\nid: dep-stem\npriority: 20\ndepends_on:\n  - my-file\n---\n# Dep Stem\n")
	// Depends on meta.ID reference.
	writeTask(t, tasksDir, "backlog", "dep-id.md", "---\nid: dep-id\npriority: 20\ndepends_on:\n  - custom-id\n---\n# Dep ID\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	// Both should have edges to my-file.md
	stemEdges := edgesTo(data, "backlog/dep-stem.md")
	if len(stemEdges) != 1 {
		t.Fatalf("stem dep edges = %d, want 1", len(stemEdges))
	}
	if stemEdges[0].From != "backlog/my-file.md" {
		t.Errorf("stem dep from = %q, want %q", stemEdges[0].From, "backlog/my-file.md")
	}

	idEdges := edgesTo(data, "backlog/dep-id.md")
	if len(idEdges) != 1 {
		t.Fatalf("id dep edges = %d, want 1", len(idEdges))
	}
	if idEdges[0].From != "backlog/my-file.md" {
		t.Errorf("id dep from = %q, want %q", idEdges[0].From, "backlog/my-file.md")
	}
}

func TestBuild_WaitingAliasParityWithScheduler(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Waiting task with meta.ID different from filename stem.
	writeTask(t, tasksDir, "waiting", "target.md", "---\nid: target-id\npriority: 10\n---\n# Target\n")
	// dep-stem depends on the filename stem "target" — the scheduler does
	// NOT recognize stem aliases for waiting-to-waiting resolution.
	writeTask(t, tasksDir, "waiting", "dep-stem.md", "---\nid: dep-stem\npriority: 20\ndepends_on:\n  - target\n---\n# Dep by stem\n")
	// dep-id depends on meta.ID "target-id" — the scheduler honors this.
	writeTask(t, tasksDir, "waiting", "dep-id.md", "---\nid: dep-id\npriority: 20\ndepends_on:\n  - target-id\n---\n# Dep by ID\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	// Stem reference "target" should NOT produce an edge (matches scheduler).
	stemEdges := edgesTo(data, "waiting/dep-stem.md")
	if len(stemEdges) != 0 {
		t.Fatalf("stem dep edges = %d, want 0 (graph should match scheduler: no stem alias for waiting)", len(stemEdges))
	}

	// Instead, "target" should be a HiddenDep since no node is aliased by stem.
	depStem := findNode(data, "waiting/dep-stem.md")
	if depStem == nil {
		t.Fatal("dep-stem node not found")
	}
	if len(depStem.HiddenDeps) != 1 {
		t.Fatalf("dep-stem hidden deps = %d, want 1", len(depStem.HiddenDeps))
	}
	if depStem.HiddenDeps[0].DependencyID != "target" {
		t.Errorf("hidden dep id = %q, want %q", depStem.HiddenDeps[0].DependencyID, "target")
	}
	if depStem.HiddenDeps[0].Status != "external" {
		t.Errorf("hidden dep status = %q, want %q", depStem.HiddenDeps[0].Status, "external")
	}

	// The DAG also classifies "target" as external, so BlockDetails should agree.
	if len(depStem.BlockDetails) == 0 {
		t.Fatal("dep-stem should have block details")
	}
	foundExternal := false
	for _, bd := range depStem.BlockDetails {
		if bd.DependencyID == "target" && bd.Reason == "external" {
			foundExternal = true
		}
	}
	if !foundExternal {
		t.Errorf("expected BlockDetail {target, external}, got %+v", depStem.BlockDetails)
	}

	// Meta.ID reference "target-id" SHOULD produce an edge (matches scheduler).
	idEdges := edgesTo(data, "waiting/dep-id.md")
	if len(idEdges) != 1 {
		t.Fatalf("id dep edges = %d, want 1", len(idEdges))
	}
	if idEdges[0].From != "waiting/target.md" {
		t.Errorf("id dep edge from = %q, want %q", idEdges[0].From, "waiting/target.md")
	}
}

func TestBuild_AmbiguousIDs(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Same ID in both completed and failed (ambiguous).
	writeTask(t, tasksDir, "completed", "ambig.md", "---\nid: ambig\npriority: 10\n---\n# Ambig\n")
	writeTask(t, tasksDir, "failed", "ambig.md", "---\nid: ambig\npriority: 10\n---\n# Ambig\n")
	writeTask(t, tasksDir, "backlog", "dep.md", "---\nid: dep\npriority: 20\ndepends_on:\n  - ambig\n---\n# Dep\n")

	idx := queueview.BuildIndex(tasksDir)

	// showAll=false: ambig nodes not in graph
	data := Build(tasksDir, idx, false)
	dep := findNode(data, "backlog/dep.md")
	if dep == nil {
		t.Fatal("dep not found")
	}
	if len(dep.HiddenDeps) != 1 {
		t.Fatalf("hidden deps = %d, want 1", len(dep.HiddenDeps))
	}
	if dep.HiddenDeps[0].Status != "ambiguous" {
		t.Errorf("hidden dep status = %q, want %q", dep.HiddenDeps[0].Status, "ambiguous")
	}

	// showAll=true: ambig nodes in graph, edge is unsatisfied
	data = Build(tasksDir, idx, true)
	edges := edgesTo(data, "backlog/dep.md")
	unsatisfiedCount := 0
	for _, e := range edges {
		if !e.Satisfied {
			unsatisfiedCount++
		}
	}
	if unsatisfiedCount == 0 {
		t.Error("expected unsatisfied edges for ambiguous ID")
	}
}

func TestBuild_DuplicateCompletedIDs_NotAmbiguous(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Two files in completed with same ID — NOT ambiguous per runtime rules.
	writeTask(t, tasksDir, "completed", "done-a.md", "---\nid: done\npriority: 10\n---\n# Done A\n")
	writeTask(t, tasksDir, "completed", "done-b.md", "---\nid: done\npriority: 10\n---\n# Done B\n")
	writeTask(t, tasksDir, "backlog", "dep.md", "---\nid: dep\npriority: 20\ndepends_on:\n  - done\n---\n# Dep\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	dep := findNode(data, "backlog/dep.md")
	if dep == nil {
		t.Fatal("dep not found")
	}
	// Should be satisfied (duplicates in completed only are NOT ambiguous).
	if len(dep.HiddenDeps) != 1 {
		t.Fatalf("hidden deps = %d, want 1", len(dep.HiddenDeps))
	}
	if dep.HiddenDeps[0].Status != "satisfied" {
		t.Errorf("status = %q, want %q", dep.HiddenDeps[0].Status, "satisfied")
	}
}

func TestBuild_DuplicateNonCompletedStates(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Same ID in backlog and in-progress (non-completed). NOT ambiguous unless also in completed.
	writeTask(t, tasksDir, "backlog", "dup.md", "---\nid: dup\npriority: 10\n---\n# Dup\n")
	writeTask(t, tasksDir, "in-progress", "dup.md", "---\nid: dup\npriority: 10\n---\n# Dup\n")
	writeTask(t, tasksDir, "waiting", "dep.md", "---\nid: dep\npriority: 20\ndepends_on:\n  - dup\n---\n# Dep\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	// dup is known but not completed → "external"
	dep := findNode(data, "waiting/dep.md")
	if dep == nil {
		t.Fatal("dep not found")
	}
	// Should have edges to both dup nodes (alias fanout).
	edges := edgesTo(data, "waiting/dep.md")
	if len(edges) != 2 {
		t.Fatalf("edges = %d, want 2", len(edges))
	}
}

func TestBuild_DuplicateWaitingIDs(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Two files in waiting with same meta.ID.
	writeTask(t, tasksDir, "waiting", "alpha.md", "---\nid: shared\npriority: 10\n---\n# Alpha\n")
	writeTask(t, tasksDir, "waiting", "beta.md", "---\nid: shared\npriority: 10\n---\n# Beta\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	// Both should exist as nodes.
	if len(data.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(data.Nodes))
	}

	// One should have a DuplicateWarning.
	if len(data.DuplicateWarnings) != 1 {
		t.Fatalf("duplicate warnings = %d, want 1", len(data.DuplicateWarnings))
	}
	dw := data.DuplicateWarnings[0]
	if dw.SharedID != "shared" {
		t.Errorf("shared id = %q, want %q", dw.SharedID, "shared")
	}
	// alpha.md is first alphabetically, so it's retained.
	if dw.DuplicateOf != "alpha.md" {
		t.Errorf("duplicate of = %q, want %q", dw.DuplicateOf, "alpha.md")
	}
	if dw.Filename != "beta.md" {
		t.Errorf("duplicate filename = %q, want %q", dw.Filename, "beta.md")
	}
}

func TestBuild_ParseFailures(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Write a file with invalid YAML frontmatter.
	writeTask(t, tasksDir, "backlog", "bad.md", "---\n: invalid yaml [[\n---\n# Bad\n")
	writeTask(t, tasksDir, "backlog", "good.md", "---\nid: good\npriority: 10\n---\n# Good\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	// bad.md should be in parse failures.
	if len(data.ParseFailures) != 1 {
		t.Fatalf("parse failures = %d, want 1", len(data.ParseFailures))
	}
	if data.ParseFailures[0].Filename != "bad.md" {
		t.Errorf("parse failure filename = %q, want %q", data.ParseFailures[0].Filename, "bad.md")
	}

	// bad.md should NOT be a node (it failed parsing).
	for _, n := range data.Nodes {
		if n.Filename == "bad.md" {
			t.Error("bad.md should not be a node")
		}
	}

	// But its stem is still in allIDs and can satisfy/block deps.
	writeTask(t, tasksDir, "waiting", "dep.md", "---\nid: dep\npriority: 20\ndepends_on:\n  - bad\n---\n# Dep\n")
	idx = queueview.BuildIndex(tasksDir)
	data = Build(tasksDir, idx, false)

	dep := findNode(data, "waiting/dep.md")
	if dep == nil {
		t.Fatal("dep not found")
	}
	// "bad" stem is known (registered by BuildIndex) but not completed.
	// No node for it → hidden dep with "external" status.
	if len(dep.HiddenDeps) == 0 {
		t.Fatal("expected hidden deps for bad reference")
	}
}

func TestBuild_ShowTo_DirReadError(t *testing.T) {
	repoDir := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoDir, ".mato")

	// Create only some dirs — make one unreadable to simulate dir-level error.
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// Make waiting dir unreadable.
	waitingDir := filepath.Join(tasksDir, "waiting")
	if err := os.Chmod(waitingDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(waitingDir, 0o755); err != nil {
			t.Errorf("os.Chmod restore permissions: %v", err)
		}
	})

	var buf bytes.Buffer
	err := ShowTo(&buf, repoDir, "json", false)
	if err == nil {
		t.Fatal("expected error for unreadable directory")
	}
	if !strings.Contains(err.Error(), "incomplete index") {
		t.Errorf("error = %q, want containing %q", err.Error(), "incomplete index")
	}
}

func TestBuild_ShowTo_GlobWarningNoError(t *testing.T) {
	repoDir := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoDir, ".mato")
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// A task with an invalid glob in affects produces a glob warning,
	// which should NOT cause ShowTo to fail.
	writeTask(t, tasksDir, "in-progress", "glob-task.md", "---\nid: glob-task\npriority: 10\naffects:\n  - \"internal/[invalid\"\n---\n# Glob Task\n")

	var buf bytes.Buffer
	err := ShowTo(&buf, repoDir, "json", false)
	if err != nil {
		t.Fatalf("ShowTo should not fail for glob warning: %v", err)
	}
}

func TestShowTo_TextWriteErrorPropagates(t *testing.T) {
	repoDir := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoDir, ".mato")
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTask(t, tasksDir, "backlog", "sample.md", "---\nid: sample\npriority: 10\n---\n# Sample task\n")

	writeErr := errors.New("broken pipe")
	fw := &failAfterNWriter{n: 1, err: writeErr}

	err := ShowTo(fw, repoDir, "text", false)
	if err == nil {
		t.Fatal("expected writer error, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want wrapped %v", err, writeErr)
	}
}

func TestShowTo_DOTWriteErrorPropagates(t *testing.T) {
	repoDir := testutil.SetupRepo(t)
	tasksDir := filepath.Join(repoDir, ".mato")
	for _, dir := range dirs.All {
		if err := os.MkdirAll(filepath.Join(tasksDir, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTask(t, tasksDir, "backlog", "sample.md", "---\nid: sample\npriority: 10\n---\n# Sample task\n")

	writeErr := errors.New("broken pipe")
	fw := &failAfterNWriter{n: 1, err: writeErr}

	err := ShowTo(fw, repoDir, "dot", false)
	if err == nil {
		t.Fatal("expected writer error, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want wrapped %v", err, writeErr)
	}
}

func TestBuild_CycleMemberDuplicateID_ShowAll(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Cycle in waiting: x ↔ y.
	writeTask(t, tasksDir, "waiting", "x.md", "---\nid: x\npriority: 10\ndepends_on:\n  - y\n---\n# X\n")
	writeTask(t, tasksDir, "waiting", "y.md", "---\nid: y\npriority: 10\ndepends_on:\n  - x\n---\n# Y\n")
	// Same ID "x" in failed state.
	writeTask(t, tasksDir, "failed", "x.md", "---\nid: x\npriority: 10\n---\n# X Failed\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, true)

	// Only waiting x should be cycle member, not failed x.
	waitingX := findNode(data, "waiting/x.md")
	failedX := findNode(data, "failed/x.md")
	if waitingX == nil {
		t.Fatal("waiting/x.md not found")
	}
	if failedX == nil {
		t.Fatal("failed/x.md not found")
	}
	if !waitingX.IsCycleMember {
		t.Error("waiting x should be cycle member")
	}
	if failedX.IsCycleMember {
		t.Error("failed x should NOT be cycle member")
	}

	// Cycles should contain only waiting keys.
	for _, cycle := range data.Cycles {
		for _, key := range cycle {
			if strings.HasPrefix(key, "failed/") {
				t.Errorf("cycle contains failed key: %q", key)
			}
		}
	}
}

func TestBuild_DeterministicOrdering(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "backlog", "z-task.md", "---\nid: z-task\npriority: 10\n---\n# Z\n")
	writeTask(t, tasksDir, "backlog", "a-task.md", "---\nid: a-task\npriority: 10\n---\n# A\n")
	writeTask(t, tasksDir, "waiting", "m-task.md", "---\nid: m-task\npriority: 5\n---\n# M\n")
	writeTask(t, tasksDir, "in-progress", "ip.md", "---\nid: ip\npriority: 1\n---\n# IP\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	// Order: waiting (m-task) < backlog (a-task, z-task) < in-progress (ip)
	if len(data.Nodes) != 4 {
		t.Fatalf("nodes = %d, want 4", len(data.Nodes))
	}
	if data.Nodes[0].Key != "waiting/m-task.md" {
		t.Errorf("nodes[0] = %q, want %q", data.Nodes[0].Key, "waiting/m-task.md")
	}
	if data.Nodes[1].Key != "backlog/a-task.md" {
		t.Errorf("nodes[1] = %q, want %q", data.Nodes[1].Key, "backlog/a-task.md")
	}
	if data.Nodes[2].Key != "backlog/z-task.md" {
		t.Errorf("nodes[2] = %q, want %q", data.Nodes[2].Key, "backlog/z-task.md")
	}
	if data.Nodes[3].Key != "in-progress/ip.md" {
		t.Errorf("nodes[3] = %q, want %q", data.Nodes[3].Key, "in-progress/ip.md")
	}
}

func TestBuild_NodeKeyUniqueness(t *testing.T) {
	tasksDir := setupTasksDir(t)

	// Same filename in different states.
	writeTask(t, tasksDir, "backlog", "task.md", "---\nid: task\npriority: 10\n---\n# Task\n")
	writeTask(t, tasksDir, "in-progress", "task.md", "---\nid: task\npriority: 10\n---\n# Task\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	seen := make(map[string]bool)
	for _, n := range data.Nodes {
		if seen[n.Key] {
			t.Errorf("duplicate key: %q", n.Key)
		}
		seen[n.Key] = true
	}
}

func TestBuild_UnknownDependency(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "waiting", "dep.md", "---\nid: dep\npriority: 10\ndepends_on:\n  - nonexistent\n---\n# Dep\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	dep := findNode(data, "waiting/dep.md")
	if dep == nil {
		t.Fatal("dep not found")
	}
	if len(dep.HiddenDeps) != 1 {
		t.Fatalf("hidden deps = %d, want 1", len(dep.HiddenDeps))
	}
	if dep.HiddenDeps[0].Status != "unknown" {
		t.Errorf("status = %q, want %q", dep.HiddenDeps[0].Status, "unknown")
	}

	// Should also have block detail
	if len(dep.BlockDetails) == 0 {
		t.Fatal("expected block details")
	}
	for _, bd := range dep.BlockDetails {
		if bd.DependencyID == "nonexistent" && bd.Reason == "unknown" {
			return
		}
	}
	t.Errorf("expected BlockDetail {nonexistent, unknown}, got %+v", dep.BlockDetails)
}

func TestRenderJSON_RoundTrip(t *testing.T) {
	data := GraphData{
		Nodes: []GraphNode{
			{
				Key:      "backlog/test.md",
				ID:       "test",
				Filename: "test.md",
				Title:    "Test Task",
				State:    StateBacklog,
				Priority: 10,
			},
		},
		Edges: []Edge{},
	}

	var buf bytes.Buffer
	if err := RenderJSON(&buf, data); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var decoded GraphData
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(decoded.Nodes) != 1 {
		t.Fatalf("decoded nodes = %d, want 1", len(decoded.Nodes))
	}
	if decoded.Nodes[0].Key != "backlog/test.md" {
		t.Errorf("decoded key = %q, want %q", decoded.Nodes[0].Key, "backlog/test.md")
	}
}

func TestBuild_FailureCount(t *testing.T) {
	tasksDir := setupTasksDir(t)

	content := "---\nid: retried\npriority: 10\n---\n# Retried\n" +
		"<!-- failure: abc at 2025-01-01T00:00:00Z step=WORK error=test files_changed=none -->\n" +
		"<!-- failure: def at 2025-01-02T00:00:00Z step=WORK error=test2 files_changed=none -->\n"
	writeTask(t, tasksDir, "backlog", "retried.md", content)

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	node := findNode(data, "backlog/retried.md")
	if node == nil {
		t.Fatal("retried not found")
	}
	if node.FailureCount != 2 {
		t.Errorf("failure count = %d, want 2", node.FailureCount)
	}
}

func TestBuild_PriorityOrdering(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "backlog", "low.md", "---\nid: low\npriority: 50\n---\n# Low\n")
	writeTask(t, tasksDir, "backlog", "high.md", "---\nid: high\npriority: 5\n---\n# High\n")
	writeTask(t, tasksDir, "backlog", "mid.md", "---\nid: mid\npriority: 25\n---\n# Mid\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	if len(data.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(data.Nodes))
	}
	if data.Nodes[0].ID != "high" {
		t.Errorf("first = %q, want high", data.Nodes[0].ID)
	}
	if data.Nodes[1].ID != "mid" {
		t.Errorf("second = %q, want mid", data.Nodes[1].ID)
	}
	if data.Nodes[2].ID != "low" {
		t.Errorf("third = %q, want low", data.Nodes[2].ID)
	}
}

func TestBuild_DuplicateDependsOn_EdgesDeduped(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "backlog", "target.md", "---\nid: target\npriority: 10\n---\n# Target\n")
	writeTask(t, tasksDir, "backlog", "consumer.md", "---\nid: consumer\npriority: 20\ndepends_on:\n  - target\n  - target\n---\n# Consumer\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	edges := edgesTo(data, "backlog/consumer.md")
	if len(edges) != 1 {
		t.Fatalf("edges to consumer = %d, want 1 (duplicate depends_on should be deduped)", len(edges))
	}
	if edges[0].From != "backlog/target.md" {
		t.Errorf("edge from = %q, want %q", edges[0].From, "backlog/target.md")
	}
}

func TestBuild_DuplicateDependsOn_HiddenDepsDeduped(t *testing.T) {
	tasksDir := setupTasksDir(t)

	writeTask(t, tasksDir, "completed", "gone.md", "---\nid: gone\npriority: 10\n---\n# Gone\n")
	writeTask(t, tasksDir, "backlog", "consumer.md", "---\nid: consumer\npriority: 20\ndepends_on:\n  - gone\n  - gone\n---\n# Consumer\n")

	idx := queueview.BuildIndex(tasksDir)
	data := Build(tasksDir, idx, false)

	node := findNode(data, "backlog/consumer.md")
	if node == nil {
		t.Fatal("consumer node not found")
	}
	if len(node.HiddenDeps) != 1 {
		t.Fatalf("hidden deps = %d, want 1 (duplicate depends_on should be deduped)", len(node.HiddenDeps))
	}
	if node.HiddenDeps[0].DependencyID != "gone" {
		t.Errorf("hidden dep id = %q, want %q", node.HiddenDeps[0].DependencyID, "gone")
	}
	if node.HiddenDeps[0].Status != "satisfied" {
		t.Errorf("hidden dep status = %q, want %q", node.HiddenDeps[0].Status, "satisfied")
	}
}

// --- Test helpers ---

func nodeKeySet(data GraphData) map[string]struct{} {
	m := make(map[string]struct{}, len(data.Nodes))
	for _, n := range data.Nodes {
		m[n.Key] = struct{}{}
	}
	return m
}

func findNode(data GraphData, key string) *GraphNode {
	for i := range data.Nodes {
		if data.Nodes[i].Key == key {
			return &data.Nodes[i]
		}
	}
	return nil
}

func edgesTo(data GraphData, toKey string) []Edge {
	var result []Edge
	for _, e := range data.Edges {
		if e.To == toKey {
			result = append(result, e)
		}
	}
	return result
}

type failAfterNWriter struct {
	n      int
	err    error
	writes int
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.n {
		return 0, w.err
	}
	return len(p), nil
}

func TestShowTo_MissingMatoDir(t *testing.T) {
	repoDir := testutil.SetupRepo(t)
	// Do NOT create .mato/ — the repo is uninitialized.

	formats := []string{"text", "dot", "json"}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			err := ShowTo(&buf, repoDir, format, false)
			if err == nil {
				t.Fatal("expected error for missing .mato directory, got nil")
			}
			want := ".mato/ directory not found - run 'mato init' first"
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
		})
	}
}
