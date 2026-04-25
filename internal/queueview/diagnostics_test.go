package queueview

import (
	"reflect"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
)

func TestDiagnoseDependencies_EmptyQueue(t *testing.T) {
	tasksDir := setupIndexDirs(t)

	diag := DiagnoseDependencies(tasksDir, nil)

	if len(diag.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", diag.Issues)
	}
	if len(diag.Analysis.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want none", diag.Analysis.DepsSatisfied)
	}
}

func TestDiagnoseDependencies_ThreeNodeCycle(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Waiting, "task-a.md", "---\nid: task-a\ndepends_on: [task-b]\n---\n# Task A\n")
	writeTask(t, tasksDir, dirs.Waiting, "task-b.md", "---\nid: task-b\ndepends_on: [task-c]\n---\n# Task B\n")
	writeTask(t, tasksDir, dirs.Waiting, "task-c.md", "---\nid: task-c\ndepends_on: [task-a]\n---\n# Task C\n")

	diag := DiagnoseDependencies(tasksDir, nil)

	wantCycle := []string{"task-a", "task-b", "task-c"}
	if !reflect.DeepEqual(diag.Analysis.Cycles, [][]string{wantCycle}) {
		t.Fatalf("Cycles = %v, want %v", diag.Analysis.Cycles, [][]string{wantCycle})
	}
	assertIssueTasks(t, diag.Issues, DependencyCycle, wantCycle)
}

func TestDiagnoseDependencies_MixedQueueReportsMultipleIssueKinds(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "ambiguous.md", "---\nid: ambiguous\n---\n# Ambiguous done\n")
	writeTask(t, tasksDir, dirs.Failed, "ambiguous-failed.md", "---\nid: ambiguous\n---\n# Ambiguous failed\n")
	writeTask(t, tasksDir, dirs.Waiting, "aaa-shared.md", "---\nid: shared\n---\n# First shared\n")
	writeTask(t, tasksDir, dirs.Waiting, "bbb-shared.md", "---\nid: shared\n---\n# Duplicate shared\n")
	writeTask(t, tasksDir, dirs.Waiting, "self.md", "---\nid: self\ndepends_on: [self]\n---\n# Self\n")
	writeTask(t, tasksDir, dirs.Waiting, "unknown.md", "---\nid: unknown\ndepends_on: [missing]\n---\n# Unknown\n")
	writeTask(t, tasksDir, dirs.Waiting, "blocked-ambiguous.md", "---\nid: blocked-ambiguous\ndepends_on: [ambiguous]\n---\n# Blocked ambiguous\n")

	diag := DiagnoseDependencies(tasksDir, nil)

	assertIssueTasks(t, diag.Issues, DependencyAmbiguousID, []string{"ambiguous"})
	assertIssueTasks(t, diag.Issues, DependencyDuplicateID, []string{"shared"})
	assertIssueTasks(t, diag.Issues, DependencySelfCycle, []string{"self"})
	assertIssueTasks(t, diag.Issues, DependencyUnknownID, []string{"unknown"})

	if got := diag.RetainedFiles["shared"]; got != "aaa-shared.md" {
		t.Fatalf("RetainedFiles[shared] = %q, want %q", got, "aaa-shared.md")
	}
}

func TestDiagnoseDependencies_CompletedTaskAliasesSatisfyWaitingDeps(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "dep-task.md", "---\nid: explicit-dep\n---\n# Completed dependency\n")
	writeTask(t, tasksDir, dirs.Waiting, "consumer-by-stem.md", "---\nid: consumer-by-stem\ndepends_on: [dep-task]\n---\n# Consumer by stem\n")
	writeTask(t, tasksDir, dirs.Waiting, "consumer-by-id.md", "---\nid: consumer-by-id\ndepends_on: [explicit-dep]\n---\n# Consumer by ID\n")

	diag := DiagnoseDependencies(tasksDir, nil)

	if len(diag.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", diag.Issues)
	}
	wantSatisfied := []string{"consumer-by-id", "consumer-by-stem"}
	if !reflect.DeepEqual(diag.Analysis.DepsSatisfied, wantSatisfied) {
		t.Fatalf("DepsSatisfied = %v, want %v", diag.Analysis.DepsSatisfied, wantSatisfied)
	}
	if len(diag.Analysis.Blocked) != 0 {
		t.Fatalf("Blocked = %v, want none", diag.Analysis.Blocked)
	}
}

func assertIssueTasks(t *testing.T, issues []DependencyIssue, kind DependencyIssueKind, want []string) {
	t.Helper()
	var got []string
	for _, issue := range issues {
		if issue.Kind == kind {
			got = append(got, issue.TaskID)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s issue tasks = %v, want %v; all issues: %v", kind, got, want, issues)
	}
}
