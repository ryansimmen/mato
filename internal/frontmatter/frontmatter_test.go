package frontmatter

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestParseTaskFile_StripsOnlyManagedCommentLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commented-task.md")
	content := "<!-- claimed-by: abc -->\n# Title\n<!-- failure: x -->\n<!-- This is a user note -->\nBody text\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, body, err := ParseTaskFile(path)
	if err != nil {
		t.Fatalf("ParseTaskFile: %v", err)
	}

	want := "# Title\n<!-- This is a user note -->\nBody text\n"
	if body != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestParseTaskFile_PreservesNonManagedHTMLComments(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "user HTML comment preserved",
			content: "# Title\n<!-- TODO: fix later -->\nBody\n",
			want:    "# Title\n<!-- TODO: fix later -->\nBody\n",
		},
		{
			name:    "example comment in instructions preserved",
			content: "# Title\nUse `<!-- failure: ... -->` to record failures.\n<!-- An example comment -->\n",
			want:    "# Title\nUse `<!-- failure: ... -->` to record failures.\n<!-- An example comment -->\n",
		},
		{
			name:    "managed markers still stripped",
			content: "<!-- branch: task/foo -->\n# Title\n<!-- review-failure: x at T step=REVIEW error=e -->\n<!-- reviewed: x at T — approved -->\n<!-- cycle-failure: mato at T — circular dependency -->\n<!-- terminal-failure: mato at T — reason -->\n<!-- merged: merge-queue at T -->\n<!-- review-rejection: x at T — feedback -->\nBody\n",
			want:    "# Title\nBody\n",
		},
		{
			name:    "mixed managed and non-managed",
			content: "<!-- failure: agent at T step=WORK error=e -->\n# Title\n<!-- user note -->\n<!-- branch: task/bar -->\nBody\n",
			want:    "# Title\n<!-- user note -->\nBody\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test-task.md")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("os.WriteFile: %v", err)
			}
			_, body, err := ParseTaskFile(path)
			if err != nil {
				t.Fatalf("ParseTaskFile: %v", err)
			}
			if body != tt.want {
				t.Fatalf("body = %q, want %q", body, tt.want)
			}
		})
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

func TestParseTaskFile_DefaultsNotSuppressedBySimilarKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "similar-keys.md")
	content := `---
not_priority: 7
custom_max_retries: 9
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
	if meta.Priority != 50 {
		t.Fatalf("meta.Priority = %d, want 50", meta.Priority)
	}
	if meta.MaxRetries != 3 {
		t.Fatalf("meta.MaxRetries = %d, want 3", meta.MaxRetries)
	}
	if body != "Body\n" {
		t.Fatalf("body = %q, want %q", body, "Body\n")
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
		{
			name:     "skips leading HTML comment",
			filename: "task.md",
			body:     "<!-- some user comment -->\n# Real Title\nBody.",
			want:     "Real Title",
		},
		{
			name:     "skips multiple leading HTML comments",
			filename: "task.md",
			body:     "<!-- comment one -->\n<!-- comment two -->\n# Actual Title",
			want:     "Actual Title",
		},
		{
			name:     "skips HTML comments and blank lines",
			filename: "task.md",
			body:     "<!-- note -->\n\n<!-- another -->\n\nPlain text title",
			want:     "Plain text title",
		},
		{
			name:     "only HTML comments falls back to filename",
			filename: "only-comments.md",
			body:     "<!-- just a comment -->\n<!-- another -->",
			want:     "only-comments",
		},
		{
			name:     "partial HTML comment is not skipped",
			filename: "task.md",
			body:     "<!-- unterminated comment\n# Heading",
			want:     "<!-- unterminated comment",
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

func TestIsGlob(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"plain file", "foo.go", false},
		{"directory prefix", "pkg/client/", false},
		{"star", "*.go", true},
		{"doublestar", "internal/**", true},
		{"question mark", "file?.go", true},
		{"char class", "data[1].csv", true},
		{"brace expansion", "internal/{a,b}/*.go", true},
		{"star in middle", "internal/runner/*.go", true},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGlob(tt.s); got != tt.want {
				t.Errorf("IsGlob(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestParseTaskData_ValidGlob(t *testing.T) {
	tests := []struct {
		name    string
		affects string
	}{
		{"single star", "internal/runner/*.go"},
		{"doublestar", "internal/**/*.go"},
		{"question mark", "internal/runner/task?.go"},
		{"char class", "data/file[0-9].csv"},
		{"brace expansion", "internal/{runner,queue}/*.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := "---\naffects:\n  - " + tt.affects + "\n---\nBody.\n"
			meta, _, err := ParseTaskData([]byte(content), "valid-glob.md")
			if err != nil {
				t.Fatalf("ParseTaskData returned error for valid glob %q: %v", tt.affects, err)
			}
			if len(meta.Affects) != 1 || meta.Affects[0] != tt.affects {
				t.Fatalf("Affects = %v, want [%s]", meta.Affects, tt.affects)
			}
		})
	}
}

func TestParseTaskData_InvalidGlob(t *testing.T) {
	// ParseTaskData should succeed even with invalid glob syntax —
	// glob validation is done separately by ValidateAffectsGlobs.
	content := "---\naffects:\n  - \"internal/[bad\"\n---\nBody.\n"
	meta, _, err := ParseTaskData([]byte(content), "invalid-glob.md")
	if err != nil {
		t.Fatalf("ParseTaskData should not reject invalid globs: %v", err)
	}
	if len(meta.Affects) != 1 || meta.Affects[0] != "internal/[bad" {
		t.Fatalf("Affects = %v, want [internal/[bad]", meta.Affects)
	}
}

func TestParseTaskData_GlobWithTrailingSlash(t *testing.T) {
	// ParseTaskData should succeed even with glob+trailing-slash —
	// glob validation is done separately by ValidateAffectsGlobs.
	content := "---\naffects:\n  - \"internal/*/\"\n---\nBody.\n"
	meta, _, err := ParseTaskData([]byte(content), "glob-slash.md")
	if err != nil {
		t.Fatalf("ParseTaskData should not reject glob with trailing /: %v", err)
	}
	if len(meta.Affects) != 1 || meta.Affects[0] != "internal/*/" {
		t.Fatalf("Affects = %v, want [internal/*/]", meta.Affects)
	}
}

func TestValidateAffectsGlobs_InvalidGlob(t *testing.T) {
	err := ValidateAffectsGlobs([]string{"internal/[bad"})
	if err == nil {
		t.Fatal("expected error for invalid glob, got nil")
	}
	if !strings.Contains(err.Error(), "invalid glob in affects") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "invalid glob in affects")
	}
}

func TestValidateAffectsGlobs_GlobWithTrailingSlash(t *testing.T) {
	err := ValidateAffectsGlobs([]string{"internal/*/"})
	if err == nil {
		t.Fatal("expected error for glob with trailing /, got nil")
	}
	if !strings.Contains(err.Error(), "combines glob syntax with trailing /") {
		t.Fatalf("error = %q, want it to contain %q", err.Error(), "combines glob syntax with trailing /")
	}
}

func TestValidateAffectsGlobs_ValidEntries(t *testing.T) {
	tests := []struct {
		name    string
		affects []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"exact paths", []string{"main.go", "pkg/util.go"}},
		{"directory prefix", []string{"pkg/client/"}},
		{"valid glob", []string{"internal/runner/*.go"}},
		{"doublestar", []string{"internal/**/*.go"}},
		{"mixed", []string{"main.go", "pkg/client/", "internal/runner/*.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateAffectsGlobs(tt.affects); err != nil {
				t.Fatalf("ValidateAffectsGlobs(%v) = %v, want nil", tt.affects, err)
			}
		})
	}
}

func TestParseTaskData_AffectsPathTraversal(t *testing.T) {
	tests := []struct {
		name               string
		affects            string
		wantAffects        []string
		wantStrippedLen    int
		wantStrippedReason string
	}{
		{
			name:               "dotdot escape",
			affects:            "../../etc/passwd",
			wantAffects:        nil,
			wantStrippedLen:    1,
			wantStrippedReason: "path traversal",
		},
		{
			name:               "absolute path",
			affects:            "/absolute/path",
			wantAffects:        nil,
			wantStrippedLen:    1,
			wantStrippedReason: "absolute path",
		},
		{
			name:               "sibling traversal",
			affects:            "../sibling/file.go",
			wantAffects:        nil,
			wantStrippedLen:    1,
			wantStrippedReason: "path traversal",
		},
		{
			name:            "internal dotdot cleaned",
			affects:         "internal/../internal/foo.go",
			wantAffects:     []string{"internal/foo.go"},
			wantStrippedLen: 0,
		},
		{
			name:            "normal path unchanged",
			affects:         "src/main.go",
			wantAffects:     []string{"src/main.go"},
			wantStrippedLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := "---\naffects:\n  - " + tt.affects + "\n---\nBody.\n"
			meta, _, err := ParseTaskData([]byte(content), "traversal-test.md")
			if err != nil {
				t.Fatalf("ParseTaskData returned error: %v", err)
			}
			if !reflect.DeepEqual(meta.Affects, tt.wantAffects) {
				t.Fatalf("Affects = %v, want %v", meta.Affects, tt.wantAffects)
			}
			if len(meta.StrippedAffects) != tt.wantStrippedLen {
				t.Fatalf("StrippedAffects len = %d, want %d", len(meta.StrippedAffects), tt.wantStrippedLen)
			}
			if tt.wantStrippedLen > 0 {
				if meta.StrippedAffects[0].Entry != tt.affects {
					t.Errorf("StrippedAffects[0].Entry = %q, want %q", meta.StrippedAffects[0].Entry, tt.affects)
				}
				if meta.StrippedAffects[0].Reason != tt.wantStrippedReason {
					t.Errorf("StrippedAffects[0].Reason = %q, want %q", meta.StrippedAffects[0].Reason, tt.wantStrippedReason)
				}
			}
		})
	}
}

func TestParseTaskData_AffectsPathTraversal_MixedValid(t *testing.T) {
	content := `---
affects:
  - ../../etc/passwd
  - src/main.go
  - /absolute/path
  - internal/foo.go
---
Body.
`
	meta, _, err := ParseTaskData([]byte(content), "mixed-test.md")
	if err != nil {
		t.Fatalf("ParseTaskData returned error: %v", err)
	}
	want := []string{"src/main.go", "internal/foo.go"}
	if !reflect.DeepEqual(meta.Affects, want) {
		t.Fatalf("Affects = %v, want %v", meta.Affects, want)
	}
	if len(meta.StrippedAffects) != 2 {
		t.Fatalf("StrippedAffects len = %d, want 2", len(meta.StrippedAffects))
	}
	if meta.StrippedAffects[0].Reason != "path traversal" {
		t.Errorf("StrippedAffects[0].Reason = %q, want %q", meta.StrippedAffects[0].Reason, "path traversal")
	}
	if meta.StrippedAffects[1].Reason != "absolute path" {
		t.Errorf("StrippedAffects[1].Reason = %q, want %q", meta.StrippedAffects[1].Reason, "absolute path")
	}
}

func TestParseTaskData_SanitizeAffectsNoStderr(t *testing.T) {
	// Parsing unsafe affects must not write warnings to stderr. The
	// stripped entries are captured in StrippedAffects for structured
	// diagnostics (queue build warnings, mato doctor).
	content := "---\naffects:\n  - /absolute/path\n  - ../../etc/passwd\n---\nBody.\n"

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = w

	_, _, parseErr := ParseTaskData([]byte(content), "stderr-test.md")

	w.Close()
	os.Stderr = origStderr

	if parseErr != nil {
		t.Fatalf("ParseTaskData returned error: %v", parseErr)
	}

	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	if n > 0 {
		t.Fatalf("sanitizeAffects wrote to stderr: %s", string(buf[:n]))
	}
}

func TestBranchDisambiguator(t *testing.T) {
	// Deterministic: same input always produces the same suffix.
	s1 := BranchDisambiguator("add-feature.md")
	s2 := BranchDisambiguator("add-feature.md")
	if s1 != s2 {
		t.Fatalf("BranchDisambiguator not deterministic: %q vs %q", s1, s2)
	}

	// Length is always 6 hex characters.
	if len(s1) != 6 {
		t.Fatalf("expected 6-char suffix, got %q (len=%d)", s1, len(s1))
	}

	// Different inputs produce different suffixes (with overwhelming probability).
	s3 := BranchDisambiguator("fix-bug.md")
	if s1 == s3 {
		t.Fatalf("expected different suffixes for different filenames, both got %q", s1)
	}

	// Filenames that sanitize to the same branch should still produce
	// different disambiguators.
	sA := BranchDisambiguator("fix the bug (urgent).md")
	sB := BranchDisambiguator("fix-the-bug-urgent.md")
	if sA == sB {
		t.Fatalf("expected different disambiguators for %q and %q, both got %q",
			"fix the bug (urgent).md", "fix-the-bug-urgent.md", sA)
	}
}

func TestParseTaskData_NegativeMaxRetries(t *testing.T) {
	data := []byte(`---
max_retries: -1
---
# Negative retries
Body.
`)
	_, _, err := ParseTaskData(data, "neg-retries.md")
	if err == nil {
		t.Fatal("expected error for negative max_retries, got nil")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Fatalf("error should mention negative, got: %v", err)
	}
}

func TestParseTaskData_ZeroMaxRetries(t *testing.T) {
	data := []byte(`---
max_retries: 0
---
# Zero retries
Body.
`)
	meta, _, err := ParseTaskData(data, "zero-retries.md")
	if err != nil {
		t.Fatalf("unexpected error for max_retries: 0: %v", err)
	}
	if meta.MaxRetries != 0 {
		t.Fatalf("MaxRetries = %d, want 0", meta.MaxRetries)
	}
}

func TestParseTaskData_StrippedAffectsNilWhenSafe(t *testing.T) {
	content := "---\naffects:\n  - src/main.go\n  - internal/foo.go\n---\nBody.\n"
	meta, _, err := ParseTaskData([]byte(content), "safe-test.md")
	if err != nil {
		t.Fatalf("ParseTaskData returned error: %v", err)
	}
	if meta.StrippedAffects != nil {
		t.Fatalf("StrippedAffects = %v, want nil for safe entries", meta.StrippedAffects)
	}
}

func TestParseTaskData_StrippedAffectsNilWithNoFrontmatter(t *testing.T) {
	content := "# Simple task\nDo something.\n"
	meta, _, err := ParseTaskData([]byte(content), "no-fm.md")
	if err != nil {
		t.Fatalf("ParseTaskData returned error: %v", err)
	}
	if meta.StrippedAffects != nil {
		t.Fatalf("StrippedAffects = %v, want nil for no-frontmatter task", meta.StrippedAffects)
	}
}
