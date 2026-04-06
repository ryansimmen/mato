package queue

import (
	"os"
	"path/filepath"
	"testing"

	"mato/internal/dag"
	"mato/internal/dirs"
)

func TestDiagnoseDependencies_DuplicateWaitingID(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Two waiting files with the same meta.ID.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "aaa-first.md"),
		[]byte("---\nid: shared\n---\n# First\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "bbb-second.md"),
		[]byte("---\nid: shared\n---\n# Second\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	// Should have a DependencyDuplicateID issue.
	found := false
	for _, issue := range diag.Issues {
		if issue.Kind == DependencyDuplicateID && issue.TaskID == "shared" {
			found = true
			if issue.Filename != "bbb-second.md" {
				t.Errorf("duplicate issue Filename = %q, want %q", issue.Filename, "bbb-second.md")
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected DependencyDuplicateID issue for 'shared', got %v", diag.Issues)
	}

	// The first file (by filename sort) should be kept in the analysis.
	depsSatisfied := diag.Analysis.DepsSatisfied
	if len(depsSatisfied) != 1 || depsSatisfied[0] != "shared" {
		t.Fatalf("DepsSatisfied = %v, want [shared] (from first file)", depsSatisfied)
	}
}

func TestDiagnoseDependencies_StemAlias(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Completed task with explicit ID "custom-id" but filename stem "dep-task".
	os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "dep-task.md"),
		[]byte("---\nid: custom-id\n---\n# Dep\n"), 0o644)

	// Waiting task depends on "dep-task" (stem) — should resolve via completedIDs
	// because BuildIndex registers both stem and meta.ID.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "consumer-stem.md"),
		[]byte("---\nid: consumer-stem\ndepends_on: [dep-task]\n---\n# Consumer by stem\n"), 0o644)

	// Waiting task depends on "custom-id" (meta.ID) — should also resolve.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "consumer-id.md"),
		[]byte("---\nid: consumer-id\ndepends_on: [custom-id]\n---\n# Consumer by ID\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	// Both consumers should have deps satisfied.
	satisfied := make(map[string]bool)
	for _, id := range diag.Analysis.DepsSatisfied {
		satisfied[id] = true
	}
	if !satisfied["consumer-stem"] {
		t.Error("consumer-stem should have deps satisfied (stem resolves for completed)")
	}
	if !satisfied["consumer-id"] {
		t.Error("consumer-id should have deps satisfied (meta.ID resolves for completed)")
	}
}

func TestDiagnoseDependencies_AmbiguousID(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Same ID in completed and failed (non-completed).
	os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "ambig.md"),
		[]byte("---\nid: ambig\n---\n# Done\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.Failed, "ambig.md"),
		[]byte("---\nid: ambig\n---\n# Failed\n"), 0o644)

	// Waiting task depends on ambiguous ID.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "downstream.md"),
		[]byte("---\nid: downstream\ndepends_on: [ambig]\n---\n# Downstream\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	// Should produce DependencyAmbiguousID issue.
	foundAmbiguous := false
	for _, issue := range diag.Issues {
		if issue.Kind == DependencyAmbiguousID && issue.TaskID == "ambig" {
			foundAmbiguous = true
			break
		}
	}
	if !foundAmbiguous {
		t.Fatalf("expected DependencyAmbiguousID issue for 'ambig', got %v", diag.Issues)
	}

	// downstream should be blocked with BlockedByAmbiguous.
	blocked := diag.Analysis.Blocked["downstream"]
	if len(blocked) != 1 || blocked[0].Reason != dag.BlockedByAmbiguous {
		t.Fatalf("Blocked[downstream] = %v, want BlockedByAmbiguous for ambig", blocked)
	}

	// downstream should NOT be in DepsSatisfied.
	for _, id := range diag.Analysis.DepsSatisfied {
		if id == "downstream" {
			t.Fatal("downstream should not have deps satisfied when dep is ambiguous")
		}
	}
}

func TestDiagnoseDependencies_UnknownDependencyIssue(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "task.md"),
		[]byte("---\nid: task\ndepends_on: [nonexistent]\n---\n# Task\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	foundUnknown := false
	for _, issue := range diag.Issues {
		if issue.Kind == DependencyUnknownID && issue.TaskID == "task" && issue.DependsOn == "nonexistent" {
			foundUnknown = true
			break
		}
	}
	if !foundUnknown {
		t.Fatalf("expected DependencyUnknownID issue, got %v", diag.Issues)
	}
}

func TestDiagnoseDependencies_CycleIssues(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Self-cycle.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "self.md"),
		[]byte("---\nid: self\ndepends_on: [self]\n---\n# Self\n"), 0o644)

	// 2-node cycle.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "cycle-a.md"),
		[]byte("---\nid: cycle-a\ndepends_on: [cycle-b]\n---\n# A\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "cycle-b.md"),
		[]byte("---\nid: cycle-b\ndepends_on: [cycle-a]\n---\n# B\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	// Should have self-cycle issue.
	foundSelf := false
	for _, issue := range diag.Issues {
		if issue.Kind == DependencySelfCycle && issue.TaskID == "self" {
			foundSelf = true
			break
		}
	}
	if !foundSelf {
		t.Fatalf("expected DependencySelfCycle issue for 'self', got %v", diag.Issues)
	}

	// Should have cycle issues for cycle-a and cycle-b.
	cycleMembers := make(map[string]bool)
	for _, issue := range diag.Issues {
		if issue.Kind == DependencyCycle {
			cycleMembers[issue.TaskID] = true
		}
	}
	if !cycleMembers["cycle-a"] || !cycleMembers["cycle-b"] {
		t.Fatalf("expected DependencyCycle issues for cycle-a and cycle-b, got members %v", cycleMembers)
	}
}

func TestDiagnoseDependencies_IssuesSorted(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create tasks that will produce multiple issue kinds.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "task-a.md"),
		[]byte("---\nid: task-a\ndepends_on: [unknown-z, unknown-a]\n---\n# A\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	// Verify issues are sorted by (Kind, TaskID, DependsOn).
	for i := 1; i < len(diag.Issues); i++ {
		prev := diag.Issues[i-1]
		curr := diag.Issues[i]
		if prev.Kind > curr.Kind || (prev.Kind == curr.Kind && prev.TaskID > curr.TaskID) ||
			(prev.Kind == curr.Kind && prev.TaskID == curr.TaskID && prev.DependsOn > curr.DependsOn) {
			t.Fatalf("issues not sorted at index %d: %v before %v", i, prev, curr)
		}
	}
}

func TestDiagnoseDependencies_ThreeDuplicateWaitingIDs(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range dirs.All {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Three waiting files with the same meta.ID.
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "aaa.md"),
		[]byte("---\nid: shared\n---\n# A\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "bbb.md"),
		[]byte("---\nid: shared\n---\n# B\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.Waiting, "ccc.md"),
		[]byte("---\nid: shared\n---\n# C\n"), 0o644)

	diag := DiagnoseDependencies(tasksDir, nil)

	// Should have two DependencyDuplicateID issues (bbb and ccc).
	var dupIssues []DependencyIssue
	for _, issue := range diag.Issues {
		if issue.Kind == DependencyDuplicateID && issue.TaskID == "shared" {
			dupIssues = append(dupIssues, issue)
		}
	}
	if len(dupIssues) != 2 {
		t.Fatalf("expected 2 duplicate issues, got %d: %v", len(dupIssues), dupIssues)
	}

	// The retained file should be aaa.md (first by filename sort).
	if diag.RetainedFiles["shared"] != "aaa.md" {
		t.Errorf("retained file = %q, want %q", diag.RetainedFiles["shared"], "aaa.md")
	}

	// Both duplicates should reference aaa.md as the first file.
	for _, issue := range dupIssues {
		if issue.DependsOn != "aaa.md" {
			t.Errorf("duplicate issue DependsOn = %q, want %q", issue.DependsOn, "aaa.md")
		}
	}
}
