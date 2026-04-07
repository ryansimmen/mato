package queueview

import (
	"reflect"
	"testing"

	"mato/internal/dirs"
)

func TestCompletedDependencyTaskIDs_UsesCompletedAliasOrder(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "foo.md", "---\nid: canonical-id\n---\n# Foo\n")
	writeTask(t, tasksDir, dirs.Completed, "zzz.md", "---\nid: foo\n---\n# Zed\n")

	got := CompletedDependencyTaskIDs(BuildIndex(tasksDir), "foo")
	want := []string{"canonical-id", "foo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CompletedDependencyTaskIDs = %v, want %v", got, want)
	}
}

func TestCompletedDependencyTaskIDs_ReturnsNilWhenNonCompletedAlsoUsesToken(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Completed, "foo.md", "---\nid: canonical-id\n---\n# Foo\n")
	writeTask(t, tasksDir, dirs.Backlog, "other.md", "---\nid: foo\n---\n# Other\n")

	got := CompletedDependencyTaskIDs(BuildIndex(tasksDir), "foo")
	if got != nil {
		t.Fatalf("CompletedDependencyTaskIDs = %v, want nil", got)
	}
}
