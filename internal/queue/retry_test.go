package queue

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetryTask_Success(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, DirFailed)
	backlogDir := filepath.Join(tmp, DirBacklog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/fix-login -->
---
id: fix-login
priority: 10
---
# Fix login bug

Some instructions here.

<!-- failure: abc123 at 2026-01-01T00:00:00Z step=WORK error=build failed -->

<!-- review-failure: def456 at 2026-01-02T00:00:00Z step=REVIEW error=network_timeout -->

<!-- cycle-failure: mato at 2026-01-03T00:00:00Z — circular dependency -->

<!-- review-rejection: 55aff11d at 2026-01-04T00:00:00Z — code review feedback -->

<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable frontmatter -->
`
	if err := os.WriteFile(filepath.Join(failedDir, "fix-login.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RetryTask(tmp, "fix-login"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	// Task should be in backlog.
	backlogPath := filepath.Join(backlogDir, "fix-login.md")
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task not found in backlog: %v", err)
	}

	// Failure markers should be stripped.
	result := string(data)
	for _, marker := range []string{
		"<!-- failure:",
		"<!-- review-failure:",
		"<!-- cycle-failure:",
		"<!-- review-rejection:",
		"<!-- terminal-failure:",
	} {
		if strings.Contains(result, marker) {
			t.Errorf("cleaned content still contains %q", marker)
		}
	}

	// Non-failure content should be preserved.
	if !strings.Contains(result, "<!-- claimed-by:") {
		t.Error("claimed-by comment was stripped but should be preserved")
	}
	if !strings.Contains(result, "<!-- branch:") {
		t.Error("branch comment was stripped but should be preserved")
	}
	if !strings.Contains(result, "# Fix login bug") {
		t.Error("task title was stripped")
	}
	if !strings.Contains(result, "Some instructions here.") {
		t.Error("task body was stripped")
	}
	if !strings.Contains(result, "id: fix-login") {
		t.Error("frontmatter was stripped")
	}

	// Source should be removed from failed/.
	if _, err := os.Stat(filepath.Join(failedDir, "fix-login.md")); !os.IsNotExist(err) {
		t.Error("failed/ copy should be removed after successful retry")
	}
}

func TestRetryTask_NotInFailed(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, DirFailed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, DirBacklog), 0o755); err != nil {
		t.Fatal(err)
	}

	err := RetryTask(tmp, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if !strings.Contains(err.Error(), "not found in failed/") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRetryTask_DestinationCollision(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, DirFailed)
	backlogDir := filepath.Join(tmp, DirBacklog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	originalContent := "# Task\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "task.md"), []byte(originalContent), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-existing file in backlog.
	if err := os.WriteFile(filepath.Join(backlogDir, "task.md"), []byte("# Existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := RetryTask(tmp, "task")
	if err == nil {
		t.Fatal("expected error for destination collision")
	}
	if !strings.Contains(err.Error(), "already exists in backlog/") {
		t.Errorf("unexpected error message: %v", err)
	}

	// The original failed/ file must be unchanged (data-loss safety).
	data, readErr := os.ReadFile(filepath.Join(failedDir, "task.md"))
	if readErr != nil {
		t.Fatalf("failed/ file should still exist after collision: %v", readErr)
	}
	if string(data) != originalContent {
		t.Errorf("failed/ file was mutated during collision\ngot:  %q\nwant: %q", string(data), originalContent)
	}
}

func TestRetryTask_AppendsMdExtension(t *testing.T) {
	tmp := t.TempDir()
	failedDir := filepath.Join(tmp, DirFailed)
	backlogDir := filepath.Join(tmp, DirBacklog)
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(failedDir, "my-task.md"), []byte("# My Task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Call with stem only (no .md extension).
	if err := RetryTask(tmp, "my-task"); err != nil {
		t.Fatalf("RetryTask() error: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backlogDir, "my-task.md")); err != nil {
		t.Fatalf("task not found in backlog: %v", err)
	}
}

func TestStripFailureMarkers(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		notWant []string
	}{
		{
			name: "strips all marker types",
			input: `<!-- branch: task/foo -->
# Title

Body text.

<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->
<!-- review-failure: def at 2026-01-02T00:00:00Z step=REVIEW error=timeout -->
<!-- cycle-failure: mato at 2026-01-03T00:00:00Z — circular dependency -->
<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->
<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable -->
`,
			want: "<!-- branch: task/foo -->",
			notWant: []string{
				"<!-- failure:",
				"<!-- review-failure:",
				"<!-- cycle-failure:",
				"<!-- review-rejection:",
				"<!-- terminal-failure:",
			},
		},
		{
			name:  "no markers to strip",
			input: "# Title\n\nBody.\n",
			want:  "# Title",
		},
		{
			name: "preserves non-failure comments",
			input: `<!-- claimed-by: abc -->
<!-- branch: task/foo -->
# Title
<!-- failure: x at 2026-01-01T00:00:00Z step=WORK error=e -->
`,
			want:    "<!-- claimed-by: abc -->",
			notWant: []string{"<!-- failure:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripFailureMarkers(tt.input)
			if !strings.Contains(got, tt.want) {
				t.Errorf("expected output to contain %q, got:\n%s", tt.want, got)
			}
			for _, bad := range tt.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("output should not contain %q, got:\n%s", bad, got)
				}
			}
		})
	}
}
