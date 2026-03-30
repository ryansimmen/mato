package taskfile

import (
	"path/filepath"
	"testing"

	"mato/internal/testutil"
)

func TestParseBranch_ValidComment(t *testing.T) {
	f := testutil.WriteTempFile(t, "<!-- branch: task/foo-bar -->\n# My Task\n")
	got := ParseBranch(f)
	if got != "task/foo-bar" {
		t.Fatalf("got %q, want %q", got, "task/foo-bar")
	}
}

func TestParseBranch_WithFrontmatterAndMetadata(t *testing.T) {
	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- branch: task/full-example -->
---
id: full-example
priority: 10
affects:
  - internal/foo/foo.go
---
# Full Example Task

Some description here.
`
	f := testutil.WriteTempFile(t, content)
	got := ParseBranch(f)
	if got != "task/full-example" {
		t.Fatalf("got %q, want %q", got, "task/full-example")
	}
}

func TestParseBranch_ExtraWhitespace(t *testing.T) {
	f := testutil.WriteTempFile(t, "<!-- branch:   task/spaces   -->\n")
	got := ParseBranch(f)
	if got != "task/spaces" {
		t.Fatalf("got %q, want %q", got, "task/spaces")
	}
}

func TestParseBranch_NoWhitespace(t *testing.T) {
	f := testutil.WriteTempFile(t, "<!-- branch:task/nospace -->\n")
	got := ParseBranch(f)
	if got != "task/nospace" {
		t.Fatalf("got %q, want %q", got, "task/nospace")
	}
}

func TestParseBranch_WithoutClosingArrow(t *testing.T) {
	// An unterminated branch comment must be ignored; callers fall back to
	// filename-derived branch naming.
	f := testutil.WriteTempFile(t, "<!-- branch: task/no-close\n")
	got := ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string for unterminated marker", got)
	}
}

func TestParseBranch_EmptyFile(t *testing.T) {
	f := testutil.WriteTempFile(t, "")
	got := ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranch_NoBranchComment(t *testing.T) {
	f := testutil.WriteTempFile(t, "---\npriority: 5\n---\n# Task\nNo branch here.\n")
	got := ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranch_NonexistentFile(t *testing.T) {
	got := ParseBranch(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranch_MultipleBranchComments(t *testing.T) {
	content := "<!-- branch: task/first -->\n<!-- branch: task/second -->\n"
	f := testutil.WriteTempFile(t, content)
	got := ParseBranch(f)
	if got != "task/first" {
		t.Fatalf("got %q, want %q (first match)", got, "task/first")
	}
}

func TestParseBranch_BranchInMiddleOfContent(t *testing.T) {
	content := `# My Task

Some description.

<!-- branch: task/mid-content -->

More content after the branch comment.
`
	f := testutil.WriteTempFile(t, content)
	got := ParseBranch(f)
	if got != "task/mid-content" {
		t.Fatalf("got %q, want %q", got, "task/mid-content")
	}
}

func TestParseBranch_OnlyFrontmatter(t *testing.T) {
	f := testutil.WriteTempFile(t, "---\npriority: 5\n---\n")
	got := ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranch_BranchWithSlashes(t *testing.T) {
	f := testutil.WriteTempFile(t, "<!-- branch: task/deep/nested/branch -->\n")
	got := ParseBranch(f)
	if got != "task/deep/nested/branch" {
		t.Fatalf("got %q, want %q", got, "task/deep/nested/branch")
	}
}

func TestParseBranch_IgnoresMarkersInCodeFences(t *testing.T) {
	f := testutil.WriteTempFile(t, "```\n<!-- branch: task/fenced -->\n```\n# Task\n")
	got := ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranch_IgnoresInlineMarkerText(t *testing.T) {
	f := testutil.WriteTempFile(t, "branch is <!-- branch: task/inline --> in prose\n")
	got := ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranch_CorruptMarkers(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"partial closing", "<!-- branch: task/partial -\n"},
		{"truncated mid-close", "<!-- branch: task/trunc --\n"},
		{"missing branch token", "<!-- branch: -->\n"},
		{"empty with whitespace only", "<!-- branch:    -->\n"},
		{"no closing newline", "<!-- branch: task/cut"},
		{"broken open tag", "<! branch: task/broken -->\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := testutil.WriteTempFile(t, tt.content)
			got := ParseBranch(f)
			if got != "" {
				t.Fatalf("got %q, want empty string for corrupt marker", got)
			}
		})
	}
}
