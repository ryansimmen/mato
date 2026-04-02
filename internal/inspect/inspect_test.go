package inspect

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/queue"
	"mato/internal/testutil"
)

func TestShowTo_TextStatuses(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, tasksDir string)
		taskRef string
		want    []string
		notWant []string
	}{
		{
			name: "snapshot task without explicit id falls back to filename stem",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirBacklog, "no-id.md", "---\npriority: 5\n---\n# No ID\n")
			},
			taskRef: "no-id",
			want:    []string{"Task: no-id", "Title: No ID", "Status: runnable"},
		},
		{
			name: "waiting blocked by failed dependency",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "dep.md", "---\nid: dep\n---\n# Done\n<!-- failure: agent at 2026-01-01T00:00:00Z step=WORK error=broken -->\n")
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [dep]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			want:    []string{"File: waiting/consumer.md", "Status: blocked", "dep (failed/dep.md)", "Next step: complete or fix the blocking dependencies so this task can leave waiting/"},
		},
		{
			name: "waiting unknown dependency",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [missing]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			want:    []string{"Status: blocked", "missing (unknown)"},
		},
		{
			name: "waiting ambiguous dependency",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirCompleted, "done.md", "---\nid: shared\n---\n# Done\n")
				writeTask(t, tasksDir, queue.DirWaiting, "shared.md", "---\nid: shared\n---\n# Shared\n")
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [shared]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			want:    []string{"Status: blocked", "shared (ambiguous)"},
		},
		{
			name: "waiting invalid glob",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "bad-waiting.md", "---\nid: bad-waiting\naffects: ['foo[']\n---\n# Bad Waiting\n")
			},
			taskRef: "bad-waiting",
			want:    []string{"Status: invalid", "invalid affects glob syntax"},
		},
		{
			name: "waiting self cycle invalid",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "cycle.md", "---\nid: cycle\ndepends_on: [cycle]\n---\n# Cycle\n")
			},
			taskRef: "cycle",
			want:    []string{"Status: invalid", "depends on itself", "fix the dependency cycle"},
		},
		{
			name: "waiting multi-node cycle invalid",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "task-a.md", "---\nid: task-a\ndepends_on: [task-b]\n---\n# Task A\n")
				writeTask(t, tasksDir, queue.DirWaiting, "task-b.md", "---\nid: task-b\ndepends_on: [task-a]\n---\n# Task B\n")
			},
			taskRef: "task-a",
			want:    []string{"Status: invalid", "circular dependency", "task-a -> task-b"},
		},
		{
			name: "waiting duplicate invalid for non retained copy",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "aaa.md", "---\nid: dup\n---\n# First\n")
				writeTask(t, tasksDir, queue.DirWaiting, "zzz.md", "---\nid: dup\n---\n# Second\n")
			},
			taskRef: "zzz",
			want:    []string{"Status: invalid", "duplicate waiting task id", "aaa.md is the retained copy"},
		},
		{
			name: "waiting retained duplicate still uses normal dependency analysis",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "aaa.md", "---\nid: dup\ndepends_on: [missing]\n---\n# First\n")
				writeTask(t, tasksDir, queue.DirWaiting, "zzz.md", "---\nid: dup\n---\n# Second\n")
			},
			taskRef: "aaa",
			want:    []string{"Status: blocked", "missing (unknown)"},
			notWant: []string{"duplicate waiting task id"},
		},
		{
			name: "waiting deps satisfied but blocked by active overlap",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirCompleted, "dep.md", "---\nid: dep\n---\n# Dep\n")
				writeTask(t, tasksDir, queue.DirInProgress, "active.md", "---\nid: active\naffects: [pkg/file.go]\n---\n# Active\n")
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [dep]\naffects: [pkg/file.go]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			want:    []string{"Status: blocked", "active overlapping work still prevents promotion"},
		},
		{
			name: "backlog invalid glob",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirBacklog, "bad-glob.md", "---\nid: bad-glob\naffects: ['foo[']\n---\n# Bad Glob\n")
			},
			taskRef: "bad-glob",
			want:    []string{"Status: invalid", "invalid affects glob syntax"},
		},
		{
			name: "backlog dependency blocked before reconcile",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "dep.md", "---\nid: dep\n---\n# Dep\n")
				writeTask(t, tasksDir, queue.DirBacklog, "consumer.md", "---\nid: consumer\ndepends_on: [dep]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			want:    []string{"Status: blocked", "reconcile will move this task back to waiting/"},
		},
		{
			name: "backlog deferred with review history",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirInProgress, "active.md", "---\nid: active\naffects: [pkg/file.go]\n---\n# Active\n")
				writeTask(t, tasksDir, queue.DirBacklog, "deferred.md", "---\nid: deferred\naffects: [pkg/file.go]\n---\n# Deferred\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — add tests -->\n")
			},
			taskRef: "deferred",
			want:    []string{"Status: deferred", "Blocking task: in-progress/active.md", "Review history: previously rejected: add tests"},
		},
		{
			name: "backlog runnable with queue position and review history",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirBacklog, "first.md", "---\nid: first\npriority: 1\n---\n# First\n")
				writeTask(t, tasksDir, queue.DirBacklog, "second.md", "---\nid: second\npriority: 5\n---\n# Second\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — handle edge case -->\n")
			},
			taskRef: "second",
			want:    []string{"Status: runnable", "Queue position: 2 of 2", "Review history: previously rejected: handle edge case"},
		},
		{
			name: "in progress claim metadata",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirInProgress, "running.md", "<!-- claimed-by: agent-1  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: running\n---\n# Running\n")
			},
			taskRef: "running",
			want:    []string{"Status: running", "Claimed by: agent-1 at 2026-01-01T00:00:00Z"},
		},
		{
			name: "ready for review exhausted budget",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "review.md", "<!-- branch: task/review -->\n<!-- review-failure: a at 2026-01-01T00:00:00Z step=REVIEW error=one -->\n<!-- review-failure: b at 2026-01-01T00:01:00Z step=REVIEW error=two -->\n---\nid: review\nmax_retries: 2\n---\n# Review\n")
			},
			taskRef: "review",
			want:    []string{"Status: invalid", "review retry budget exhausted", "Review failures: 2"},
		},
		{
			name: "ready for review normal case",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "review-ok.md", "<!-- branch: task/review-ok -->\n<!-- review-failure: a at 2026-01-01T00:00:00Z step=REVIEW error=one -->\n---\nid: review-ok\nmax_retries: 3\n---\n# Review OK\n")
			},
			taskRef: "review-ok",
			want:    []string{"Status: ready_for_review", "queued for AI review", "Branch: task/review-ok"},
			notWant: []string{"Status: invalid"},
		},
		{
			name: "ready to merge",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyMerge, "merge.md", "---\nid: merge\n---\n# Merge\n")
			},
			taskRef: "merge",
			want:    []string{"Status: ready_to_merge", "queued for host squash merge"},
		},
		{
			name: "completed task",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirCompleted, "done.md", "---\nid: done\n---\n# Done\n")
			},
			taskRef: "done",
			want:    []string{"Status: completed", "merged and completed"},
		},
		{
			name: "failed retry",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "retry.md", "<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=first -->\n<!-- failure: b at 2026-01-01T00:01:00Z step=WORK error=second -->\n---\nid: retry\nmax_retries: 2\n---\n# Retry\n")
			},
			taskRef: "retry",
			want:    []string{"Status: failed", "Failure: retry (2/2)", "Last failure: second"},
		},
		{
			name: "failed cycle",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "cycle.md", "<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n---\nid: cycle\n---\n# Cycle\n")
			},
			taskRef: "cycle",
			want:    []string{"Status: failed", "Failure: cycle", "Cycle failure: circular dependency"},
		},
		{
			name: "failed terminal",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "terminal.md", "<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — invalid glob syntax -->\n---\nid: terminal\n---\n# Terminal\n")
			},
			taskRef: "terminal",
			want:    []string{"Status: failed", "Failure: terminal", "Terminal failure: invalid glob syntax"},
		},
		{
			name: "failed cancelled",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: cancelled\n---\n# Cancelled\n")
			},
			taskRef: "cancelled",
			want:    []string{"Status: failed", "Failure: cancelled", "task was deliberately cancelled by an operator", "use mato retry to requeue if you want to run it again"},
		},
		{
			name: "parse failed task in review",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "broken.md", "<!-- branch: task/broken -->\n---\npriority: nope\n---\n# Broken\n")
			},
			taskRef: "broken",
			want:    []string{"Status: invalid", "Parse error:", "quarantines it to failed/", "File: ready-for-review/broken.md"},
		},
		{
			name: "cancelled parse failed task",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "broken-cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n# Broken\n")
			},
			taskRef: "broken-cancelled",
			want:    []string{"Status: failed", "Failure: cancelled", "Parse error:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
			tt.setup(t, tasksDir)

			var buf bytes.Buffer
			if err := ShowTo(&buf, repoRoot, tt.taskRef, "text"); err != nil {
				t.Fatalf("ShowTo: %v", err)
			}
			output := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q:\n%s", want, output)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(output, notWant) {
					t.Fatalf("output unexpectedly contains %q:\n%s", notWant, output)
				}
			}
		})
	}
}

func TestShowTo_JSONFields(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, tasksDir string)
		taskRef string
		assert  func(t *testing.T, got map[string]any)
	}{
		{
			name: "runnable with review history",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirBacklog, "first.md", "---\nid: first\npriority: 1\n---\n# First\n")
				writeTask(t, tasksDir, queue.DirBacklog, "second.md", "---\nid: second\npriority: 2\n---\n# Second\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — add coverage -->\n")
			},
			taskRef: "second",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "runnable" {
					t.Fatalf("status = %v, want runnable", got["status"])
				}
				if got["queue_position"] != float64(2) {
					t.Fatalf("queue_position = %v, want 2", got["queue_position"])
				}
				if got["review_rejection_reason"] != "add coverage" {
					t.Fatalf("review_rejection_reason = %v, want add coverage", got["review_rejection_reason"])
				}
				if got["filename"] != "second.md" {
					t.Fatalf("filename = %v, want second.md", got["filename"])
				}
				if got["title"] != "Second" {
					t.Fatalf("title = %v, want Second", got["title"])
				}
				if _, ok := got["claimed_at"]; ok {
					t.Fatal("claimed_at should be omitted when unset")
				}
			},
		},
		{
			name: "blocked waiting dependency fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "dep.md", "---\nid: dep\n---\n# Dep\n")
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [dep]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "blocked" {
					t.Fatalf("status = %v, want blocked", got["status"])
				}
				deps, ok := got["blocking_dependencies"].([]any)
				if !ok || len(deps) != 1 {
					t.Fatalf("blocking_dependencies = %v, want single entry", got["blocking_dependencies"])
				}
				dep, ok := deps[0].(map[string]any)
				if !ok {
					t.Fatalf("blocking dependency = %T, want object", deps[0])
				}
				if dep["id"] != "dep" || dep["state"] != "failed" || dep["filename"] != "dep.md" || dep["reason"] != "external" {
					t.Fatalf("blocking dependency = %v, want id/state/filename/reason fields", dep)
				}
			},
		},
		{
			name: "deferred backlog fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirInProgress, "active.md", "---\nid: active\naffects: [pkg/file.go]\n---\n# Active\n")
				writeTask(t, tasksDir, queue.DirBacklog, "deferred.md", "---\nid: deferred\naffects: [pkg/file.go]\n---\n# Deferred\n")
			},
			taskRef: "deferred",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "deferred" {
					t.Fatalf("status = %v, want deferred", got["status"])
				}
				blockingTask, ok := got["blocking_task"].(map[string]any)
				if !ok || blockingTask["filename"] != "active.md" || blockingTask["state"] != queue.DirInProgress {
					t.Fatalf("blocking_task = %v, want active in-progress task", got["blocking_task"])
				}
				affects, ok := got["conflicting_affects"].([]any)
				if !ok || len(affects) != 1 || affects[0] != "pkg/file.go" {
					t.Fatalf("conflicting_affects = %v, want pkg/file.go", got["conflicting_affects"])
				}
			},
		},
		{
			name: "failed terminal fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "terminal.md", "<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — invalid glob syntax -->\n---\nid: terminal\n---\n# Terminal\n")
			},
			taskRef: "terminal",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "failed" || got["failure_kind"] != "terminal" {
					t.Fatalf("status/failure_kind = %v/%v, want failed/terminal", got["status"], got["failure_kind"])
				}
				if got["last_terminal_reason"] != "invalid glob syntax" {
					t.Fatalf("last_terminal_reason = %v, want invalid glob syntax", got["last_terminal_reason"])
				}
			},
		},
		{
			name: "failed cancelled fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\nid: cancelled\n---\n# Cancelled\n")
			},
			taskRef: "cancelled",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "failed" || got["failure_kind"] != "cancelled" {
					t.Fatalf("status/failure_kind = %v/%v, want failed/cancelled", got["status"], got["failure_kind"])
				}
			},
		},
		{
			name: "invalid glob fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirBacklog, "bad-glob.md", "---\nid: bad-glob\naffects: ['foo[']\n---\n# Bad Glob\n")
			},
			taskRef: "bad-glob",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "invalid" {
					t.Fatalf("status = %v, want invalid", got["status"])
				}
				if got["reason"] == nil || !strings.Contains(got["reason"].(string), "invalid affects glob syntax") {
					t.Fatalf("reason = %v, want invalid affects glob syntax", got["reason"])
				}
			},
		},
		{
			name: "invalid parse failure fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "broken.md", "<!-- branch: task/broken -->\n---\npriority: nope\n---\n# Broken\n")
			},
			taskRef: "broken",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "invalid" {
					t.Fatalf("status = %v, want invalid", got["status"])
				}
				if _, ok := got["parse_error"].(string); !ok {
					t.Fatalf("parse_error = %v, want string", got["parse_error"])
				}
			},
		},
		{
			name: "invalid cancelled parse failure fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "broken-cancelled.md", "<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n---\npriority: nope\n---\n# Broken\n")
			},
			taskRef: "broken-cancelled",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "failed" {
					t.Fatalf("status = %v, want failed", got["status"])
				}
				if got["failure_kind"] != "cancelled" {
					t.Fatalf("failure_kind = %v, want cancelled", got["failure_kind"])
				}
			},
		},
		{
			name: "invalid ready for review exhausted budget fields",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "review.md", "<!-- review-failure: a at 2026-01-01T00:00:00Z step=REVIEW error=one -->\n<!-- review-failure: b at 2026-01-01T00:01:00Z step=REVIEW error=two -->\n---\nid: review\nmax_retries: 2\n---\n# Review\n")
			},
			taskRef: "review",
			assert: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["status"] != "invalid" {
					t.Fatalf("status = %v, want invalid", got["status"])
				}
				if got["review_failure_count"] != float64(2) {
					t.Fatalf("review_failure_count = %v, want 2", got["review_failure_count"])
				}
				if got["max_retries"] != float64(2) {
					t.Fatalf("max_retries = %v, want 2", got["max_retries"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
			tt.setup(t, tasksDir)

			var buf bytes.Buffer
			if err := ShowTo(&buf, repoRoot, tt.taskRef, "json"); err != nil {
				t.Fatalf("ShowTo: %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			tt.assert(t, got)
		})
	}
}

func TestShowTo_TaskResolutionErrors(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		repoRoot, _ := testutil.SetupRepoWithTasks(t)
		var buf bytes.Buffer
		err := ShowTo(&buf, repoRoot, "missing", "text")
		if err == nil || !strings.Contains(err.Error(), "task not found") {
			t.Fatalf("err = %v, want task not found", err)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
		writeTask(t, tasksDir, queue.DirBacklog, "shared.md", "---\nid: shared-one\n---\n# Shared\n")
		writeTask(t, tasksDir, queue.DirCompleted, "shared.md", "---\nid: shared-two\n---\n# Shared\n")

		var buf bytes.Buffer
		err := ShowTo(&buf, repoRoot, "shared", "text")
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("err = %v, want ambiguous", err)
		}
		if !strings.Contains(err.Error(), "backlog/shared.md") || !strings.Contains(err.Error(), "completed/shared.md") {
			t.Fatalf("ambiguity error missing candidates: %v", err)
		}
	})
}

func writeTask(t *testing.T, tasksDir, dir, name, content string) {
	t.Helper()
	path := filepath.Join(tasksDir, dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestShowTo_MissingMatoDir(t *testing.T) {
	repoDir := testutil.SetupRepo(t)
	// Do NOT create .mato/ — the repo is uninitialized.

	formats := []string{"text", "json"}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			err := ShowTo(&buf, repoDir, "sample-task", format)
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

func TestShowTo_JSONBlockingDependencies(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, tasksDir string)
		taskRef string
		assert  func(t *testing.T, deps []any)
	}{
		{
			name: "single blocking dependency in waiting",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "blocker.md", "---\nid: blocker\n---\n# Blocker\n<!-- failure: a at 2026-01-01T00:00:00Z step=WORK error=broken -->\n")
				writeTask(t, tasksDir, queue.DirWaiting, "blocked.md", "---\nid: blocked\ndepends_on: [blocker]\n---\n# Blocked\n")
			},
			taskRef: "blocked",
			assert: func(t *testing.T, deps []any) {
				t.Helper()
				if len(deps) != 1 {
					t.Fatalf("blocking_dependencies length = %d, want 1", len(deps))
				}
				dep := deps[0].(map[string]any)
				if dep["id"] != "blocker" {
					t.Fatalf("dependency id = %v, want blocker", dep["id"])
				}
				if dep["state"] != "failed" {
					t.Fatalf("dependency state = %v, want failed", dep["state"])
				}
			},
		},
		{
			name: "multiple blocking dependencies",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "dep-a.md", "---\nid: dep-a\n---\n# Dep A\n")
				writeTask(t, tasksDir, queue.DirFailed, "dep-b.md", "---\nid: dep-b\n---\n# Dep B\n")
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [dep-a, dep-b]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			assert: func(t *testing.T, deps []any) {
				t.Helper()
				if len(deps) != 2 {
					t.Fatalf("blocking_dependencies length = %d, want 2", len(deps))
				}
				ids := make(map[string]string)
				for _, d := range deps {
					dep := d.(map[string]any)
					id, _ := dep["id"].(string)
					state, _ := dep["state"].(string)
					ids[id] = state
				}
				if ids["dep-a"] != "waiting" {
					t.Fatalf("dep-a state = %v, want waiting", ids["dep-a"])
				}
				if ids["dep-b"] != "failed" {
					t.Fatalf("dep-b state = %v, want failed", ids["dep-b"])
				}
			},
		},
		{
			name: "unknown dependency in JSON",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirWaiting, "consumer.md", "---\nid: consumer\ndepends_on: [nonexistent]\n---\n# Consumer\n")
			},
			taskRef: "consumer",
			assert: func(t *testing.T, deps []any) {
				t.Helper()
				if len(deps) != 1 {
					t.Fatalf("blocking_dependencies length = %d, want 1", len(deps))
				}
				dep := deps[0].(map[string]any)
				if dep["id"] != "nonexistent" {
					t.Fatalf("dependency id = %v, want nonexistent", dep["id"])
				}
				if dep["state"] != "unknown" {
					t.Fatalf("dependency state = %v, want unknown", dep["state"])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
			tt.setup(t, tasksDir)

			var buf bytes.Buffer
			if err := ShowTo(&buf, repoRoot, tt.taskRef, "json"); err != nil {
				t.Fatalf("ShowTo: %v", err)
			}

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			deps, ok := got["blocking_dependencies"].([]any)
			if !ok || len(deps) == 0 {
				t.Fatalf("blocking_dependencies missing or empty: %v", got["blocking_dependencies"])
			}
			tt.assert(t, deps)
		})
	}
}

func TestShowTo_TextConflictingAffects(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirInProgress, "active.md", "---\nid: active\naffects: [pkg/api.go, pkg/handler.go]\n---\n# Active\n")
	writeTask(t, tasksDir, queue.DirBacklog, "deferred.md", "---\nid: deferred\naffects: [pkg/api.go, pkg/handler.go]\n---\n# Deferred\n")

	var buf bytes.Buffer
	if err := ShowTo(&buf, repoRoot, "deferred", "text"); err != nil {
		t.Fatalf("ShowTo: %v", err)
	}
	output := buf.String()

	wantParts := []string{
		"Status: deferred",
		"Conflicting affects:",
		"pkg/api.go",
		"pkg/handler.go",
	}
	for _, want := range wantParts {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestShowTo_TextReviewFailureCount(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, tasksDir string)
		taskRef string
		want    []string
	}{
		{
			name: "single review failure marker",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "review-one.md",
					"<!-- branch: task/review-one -->\n"+
						"<!-- review-failure: agent at 2026-01-01T00:00:00Z step=REVIEW error=bad_tests -->\n"+
						"---\nid: review-one\nmax_retries: 3\n---\n# Review One\n")
			},
			taskRef: "review-one",
			want:    []string{"Review failures: 1"},
		},
		{
			name: "multiple review failure markers",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirReadyReview, "review-multi.md",
					"<!-- branch: task/review-multi -->\n"+
						"<!-- review-failure: a at 2026-01-01T00:00:00Z step=REVIEW error=first -->\n"+
						"<!-- review-failure: b at 2026-01-01T00:01:00Z step=REVIEW error=second -->\n"+
						"<!-- review-failure: c at 2026-01-01T00:02:00Z step=REVIEW error=third -->\n"+
						"---\nid: review-multi\nmax_retries: 5\n---\n# Review Multi\n")
			},
			taskRef: "review-multi",
			want:    []string{"Review failures: 3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
			tt.setup(t, tasksDir)

			var buf bytes.Buffer
			if err := ShowTo(&buf, repoRoot, tt.taskRef, "text"); err != nil {
				t.Fatalf("ShowTo: %v", err)
			}
			output := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestShowTo_ParseFailureStates(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, tasksDir string)
		taskRef string
		want    []string
	}{
		{
			name: "cancelled parse failure in failed",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "pf-cancelled.md",
					"<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n"+
						"---\npriority: nope\n---\n# PF Cancelled\n")
			},
			taskRef: "pf-cancelled",
			want: []string{
				"Status: failed",
				"Failure: cancelled",
				"Parse error:",
				"fix the task frontmatter, then requeue with mato retry",
			},
		},
		{
			name: "terminal failure parse failure in failed",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "pf-terminal.md",
					"<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — bad glob syntax -->\n"+
						"---\npriority: nope\n---\n# PF Terminal\n")
			},
			taskRef: "pf-terminal",
			want: []string{
				"Status: failed",
				"Failure: terminal",
				"Parse error:",
				"fix the task frontmatter and the structural failure",
			},
		},
		{
			name: "cycle failure parse failure in failed",
			setup: func(t *testing.T, tasksDir string) {
				writeTask(t, tasksDir, queue.DirFailed, "pf-cycle.md",
					"<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dep -->\n"+
						"---\npriority: nope\n---\n# PF Cycle\n")
			},
			taskRef: "pf-cycle",
			want: []string{
				"Status: failed",
				"Failure: cycle",
				"Parse error:",
				"fix the task frontmatter and dependency cycle",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
			tt.setup(t, tasksDir)

			var buf bytes.Buffer
			if err := ShowTo(&buf, repoRoot, tt.taskRef, "text"); err != nil {
				t.Fatalf("ShowTo: %v", err)
			}
			output := buf.String()
			for _, want := range tt.want {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestShowTo_InvalidFormat(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirBacklog, "sample.md", "---\nid: sample\n---\n# Sample\n")

	var buf bytes.Buffer
	err := ShowTo(&buf, repoRoot, "sample", "yaml")
	if err == nil {
		t.Fatal("expected error for unsupported format, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("error = %q, want unsupported format", err.Error())
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Fatalf("error = %q, should mention the invalid format name", err.Error())
	}
}
