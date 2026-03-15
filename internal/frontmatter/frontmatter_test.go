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
