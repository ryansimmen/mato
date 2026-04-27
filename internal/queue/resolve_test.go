package queue

import (
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
)

func TestResolveTask(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, tasksDir string)
		taskRef string
		assert  func(t *testing.T, match TaskMatch, err error)
	}{
		{
			name: "stem match",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, dirs.Backlog, "task-a.md", "---\nid: task-a\n---\n")
			},
			taskRef: "task-a",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err != nil {
					t.Fatalf("ResolveTask: %v", err)
				}
				if match.State != dirs.Backlog || match.Filename != "task-a.md" || match.Snapshot == nil {
					t.Fatalf("unexpected match: %+v", match)
				}
			},
		},
		{
			name: "md suffix match",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, dirs.Waiting, "task-b.md", "---\nid: task-b\n---\n")
			},
			taskRef: "task-b.md",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err != nil {
					t.Fatalf("ResolveTask: %v", err)
				}
				if match.State != dirs.Waiting || match.Filename != "task-b.md" {
					t.Fatalf("unexpected match: %+v", match)
				}
			},
		},
		{
			name: "explicit id match",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, dirs.ReadyReview, "different-name.md", "---\nid: explicit-id\n---\n")
			},
			taskRef: "explicit-id",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err != nil {
					t.Fatalf("ResolveTask: %v", err)
				}
				if match.Filename != "different-name.md" || match.Snapshot == nil || match.Snapshot.Meta.ID != "explicit-id" {
					t.Fatalf("unexpected match: %+v", match)
				}
			},
		},
		{
			name: "explicit id with slash",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, dirs.ReadyMerge, "task-c.md", "---\nid: group/task-c\n---\n")
			},
			taskRef: "group/task-c",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err != nil {
					t.Fatalf("ResolveTask: %v", err)
				}
				if match.Filename != "task-c.md" || match.Snapshot == nil || match.Snapshot.Meta.ID != "group/task-c" {
					t.Fatalf("unexpected match: %+v", match)
				}
			},
		},
		{
			name: "parse failure match",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, dirs.Failed, "broken.md", "---\npriority: nope\n---\n")
			},
			taskRef: "broken",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err != nil {
					t.Fatalf("ResolveTask: %v", err)
				}
				if match.ParseFailure == nil || match.Snapshot != nil || match.Filename != "broken.md" {
					t.Fatalf("unexpected parse failure match: %+v", match)
				}
			},
		},
		{
			name:    "not found",
			setup:   func(t *testing.T, tasksDir string) {},
			taskRef: "missing",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err == nil || !strings.Contains(err.Error(), "task not found: missing") {
					t.Fatalf("err = %v, want not found", err)
				}
			},
		},
		{
			name: "ambiguous match",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, dirs.Backlog, "dup.md", "---\nid: dup\n---\n")
				writeTask(t, tasksDir, dirs.Failed, "dup.md", "---\nid: dup\n---\n")
			},
			taskRef: "dup",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err == nil {
					t.Fatal("expected ambiguity error")
				}
				if !strings.Contains(err.Error(), `task reference "dup" is ambiguous:`) {
					t.Fatalf("unexpected err: %v", err)
				}
				if !strings.Contains(err.Error(), "backlog/dup.md (id: dup)") || !strings.Contains(err.Error(), "failed/dup.md (id: dup)") {
					t.Fatalf("ambiguity error missing matches: %v", err)
				}
			},
		},
		{
			name:    "empty ref",
			setup:   func(t *testing.T, tasksDir string) {},
			taskRef: "   ",
			assert: func(t *testing.T, match TaskMatch, err error) {
				if err == nil || !strings.Contains(err.Error(), "task reference must not be empty") {
					t.Fatalf("err = %v, want empty-ref error", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := setupIndexDirs(t)
			tt.setup(t, tasksDir)
			match, err := ResolveTask(BuildIndex(tasksDir), tt.taskRef)
			tt.assert(t, match, err)
		})
	}
}
