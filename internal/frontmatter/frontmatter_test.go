package frontmatter

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseTaskFile_AllFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "complex-task.md")
	content := `---
id: custom-id
priority: 7
depends_on: [task-a, task-b]
affects:
  - api
  - cli
tags: [bug, urgent]
estimated_complexity: high
max_retries: 5
---
# Title
Task body.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{
		ID:                  "custom-id",
		Priority:            7,
		DependsOn:           []string{"task-a", "task-b"},
		Affects:             []string{"api", "cli"},
		Tags:                []string{"bug", "urgent"},
		EstimatedComplexity: "high",
		MaxRetries:          5,
	}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "# Title\nTask body.\n" {
		t.Fatalf("body = %q, want %q", body, "# Title\nTask body.\n")
	}
}

func TestParseTaskFile_PartialFrontmatterUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial-task.md")
	content := `---
priority: 12
tags:
  - ops
---
Body
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	if meta.ID != "partial-task" {
		t.Fatalf("meta.ID = %q, want %q", meta.ID, "partial-task")
	}
	if meta.Priority != 12 {
		t.Fatalf("meta.Priority = %d, want 12", meta.Priority)
	}
	if !reflect.DeepEqual(meta.Tags, []string{"ops"}) {
		t.Fatalf("meta.Tags = %#v, want %#v", meta.Tags, []string{"ops"})
	}
	if meta.MaxRetries != 3 {
		t.Fatalf("meta.MaxRetries = %d, want 3", meta.MaxRetries)
	}
	if meta.DependsOn != nil {
		t.Fatalf("meta.DependsOn = %#v, want nil", meta.DependsOn)
	}
	if body != "Body\n" {
		t.Fatalf("body = %q, want %q", body, "Body\n")
	}
}

func TestParseTaskFile_NoFrontmatterUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain-task.md")
	content := "Do the work.\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "plain-task", Priority: 50, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != content {
		t.Fatalf("body = %q, want %q", body, content)
	}
}

func TestParseTaskFile_EmptyFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-frontmatter.md")
	content := "---\n---\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	if meta.ID != "empty-frontmatter" || meta.Priority != 50 || meta.MaxRetries != 3 {
		t.Fatalf("unexpected defaults: %#v", meta)
	}
	if body != "Body text\n" {
		t.Fatalf("body = %q, want %q", body, "Body text\n")
	}
}

func TestParseTaskFile_StripsHTMLCommentLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commented-task.md")
	content := "<!-- claimed-by: abc -->\n# Title\n<!-- failure: x -->\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	if body != "# Title\nBody text\n" {
		t.Fatalf("body = %q, want %q", body, "# Title\nBody text\n")
	}
}

func TestParseTaskFile_BackwardCompatibleMarkdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-task.md")
	content := "# Title\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "legacy-task", Priority: 50, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != content {
		t.Fatalf("body = %q, want %q", body, content)
	}
}

func TestParseTaskFile_PriorityZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "priority-zero.md")
	content := `---
priority: 0
---
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, _, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}
	if meta.Priority != 0 {
		t.Fatalf("meta.Priority = %d, want 0", meta.Priority)
	}
}

func TestParseTaskFile_NegativePriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "negative-priority.md")
	content := `---
priority: -10
---
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, _, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}
	if meta.Priority != -10 {
		t.Fatalf("meta.Priority = %d, want -10", meta.Priority)
	}
}

func TestParseTaskFile_UnknownFieldsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown-fields.md")
	content := `---
id: known-id
priority: 5
unknown_field: xyz
another: 123
tags: [ops]
---
Body
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "known-id", Priority: 5, Tags: []string{"ops"}, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "Body\n" {
		t.Fatalf("body = %q, want %q", body, "Body\n")
	}
}

func TestParseTaskFile_DuplicatePriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "duplicate-priority.md")
	content := `---
priority: 5
priority: 20
---
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, _, err := ParseTaskFile(path)
	if err == nil {
		t.Fatal("expected error for duplicate YAML key, got nil")
	}
}

func TestParseTaskFile_SpecialCharacterValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "special-values.md")
	content := `---
id: my-task:v2
tags: [front:end, "back-end"]
---
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, _, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}
	if meta.ID != "my-task:v2" {
		t.Fatalf("meta.ID = %q, want %q", meta.ID, "my-task:v2")
	}
	if !reflect.DeepEqual(meta.Tags, []string{"front:end", "back-end"}) {
		t.Fatalf("meta.Tags = %#v, want %#v", meta.Tags, []string{"front:end", "back-end"})
	}
}

func TestParseTaskFile_EmptyFrontmatterEmptyBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-frontmatter-empty-body.md")
	content := "---\n---\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "empty-frontmatter-empty-body", Priority: 50, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "" {
		t.Fatalf("body = %q, want empty body", body)
	}
}

func TestParseTaskFile_DependsOnDropsEmptyItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "depends-on-drops-empty-items.md")
	content := `---
depends_on: ["", real-dep, ""]
---
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, _, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}
	if !reflect.DeepEqual(meta.DependsOn, []string{"real-dep"}) {
		t.Fatalf("meta.DependsOn = %#v, want %#v", meta.DependsOn, []string{"real-dep"})
	}
}

func TestParseTaskFile_ClaimedByBeforeFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claimed-task.md")
	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
---
id: my-task
priority: 10
affects: [main.go]
depends_on: [other-task]
tags: [bugfix]
max_retries: 5
---
# Task title
Body text.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{
		ID:         "my-task",
		Priority:   10,
		Affects:    []string{"main.go"},
		DependsOn:  []string{"other-task"},
		Tags:       []string{"bugfix"},
		MaxRetries: 5,
	}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "# Task title\nBody text.\n" {
		t.Fatalf("body = %q, want %q", body, "# Task title\nBody text.\n")
	}
}

func TestParseTaskFile_MultipleCommentsBeforeFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi-comment-task.md")
	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
<!-- failure: xyz at 2026-01-01T01:00:00Z step=WORK error=build failed files_changed=none -->
---
id: retry-task
priority: 3
affects: [internal/foo.go]
---
# Retry task
Do the thing.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{
		ID:         "retry-task",
		Priority:   3,
		Affects:    []string{"internal/foo.go"},
		MaxRetries: 3,
	}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "# Retry task\nDo the thing.\n" {
		t.Fatalf("body = %q, want %q", body, "# Retry task\nDo the thing.\n")
	}
}

func TestParseTaskFile_ClaimedByNoFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claimed-no-fm.md")
	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->
# Simple task
Just do it.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := TaskMeta{ID: "claimed-no-fm", Priority: 50, MaxRetries: 3}
	if !reflect.DeepEqual(meta, want) {
		t.Fatalf("meta = %#v, want %#v", meta, want)
	}
	if body != "# Simple task\nJust do it.\n" {
		t.Fatalf("body = %q, want %q", body, "# Simple task\nJust do it.\n")
	}
}

func TestParseTaskFile_BlankLinesBetweenCommentsAndFrontmatter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blanks-before-fm.md")
	content := `<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->

---
priority: 8
---
# Title
Body.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	meta, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	if meta.Priority != 8 {
		t.Fatalf("meta.Priority = %d, want 8", meta.Priority)
	}
	if body != "# Title\nBody.\n" {
		t.Fatalf("body = %q, want %q", body, "# Title\nBody.\n")
	}
}

func TestParseTaskFile_InvalidPriority(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "string",
			content: `---
priority: high
---
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.name+"-priority.md")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("os.WriteFile: %v", err)
			}

			if _, _, err := ParseTaskFile(path); err == nil {
				t.Fatalf("ParseTaskFile(%q) error = nil, want error", tt.content)
			}
		})
	}
}

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "simple name", input: "add-feature.md", want: "add-feature"},
		{name: "spaces and special chars", input: "fix the bug (urgent).md", want: "fix-the-bug-urgent"},
		{name: "already clean no extension", input: "my-task", want: "my-task"},
		{name: "consecutive special chars", input: "foo---bar___baz.md", want: "foo-bar-baz"},
		{name: "leading and trailing specials", input: "---hello---.md", want: "hello"},
		{name: "empty after strip", input: ".md", want: "unnamed"},
		{name: "unicode characters", input: "tâche-résumé.md", want: "t-che-r-sum"},
		{name: "no extension", input: "plain-name", want: "plain-name"},
		{name: "all special chars", input: "!!@@##.md", want: "unnamed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeBranchName(tt.input); got != tt.want {
				t.Errorf("SanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		body     string
		want     string
	}{
		{
			name:     "heading line",
			filename: "my-task.md",
			body:     "# My Task Title\n\nSome body text.",
			want:     "My Task Title",
		},
		{
			name:     "multi-level heading",
			filename: "my-task.md",
			body:     "## Sub Heading\n\nText.",
			want:     "Sub Heading",
		},
		{
			name:     "plain text first line",
			filename: "my-task.md",
			body:     "Just a plain line\nMore text.",
			want:     "Just a plain line",
		},
		{
			name:     "empty body falls back to filename",
			filename: "fallback-task.md",
			body:     "",
			want:     "fallback-task",
		},
		{
			name:     "only blank lines falls back to filename",
			filename: "blank.md",
			body:     "\n\n\n",
			want:     "blank",
		},
		{
			name:     "leading blank lines skipped",
			filename: "task.md",
			body:     "\n\n# Real Title\nBody.",
			want:     "Real Title",
		},
		{
			name:     "heading with only hashes falls back",
			filename: "edge.md",
			body:     "###\nActual content",
			want:     "Actual content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractTitle(tt.filename, tt.body); got != tt.want {
				t.Errorf("ExtractTitle(%q, %q) = %q, want %q", tt.filename, tt.body, got, tt.want)
			}
		})
	}
}
