package doctor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/testutil"
)

func TestCheckTaskParsing_ParseError(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a task with broken frontmatter to trigger a parse error.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "broken.md"),
		"---\n  bad yaml: [unclosed\n---\nBody\n")

	cc := &checkContext{
		ctx:      context.Background(),
		tasksDir: tasksDir,
	}

	cr := checkTaskParsing(cc)

	if cr.Name != "tasks" {
		t.Fatalf("name = %q, want %q", cr.Name, "tasks")
	}
	if cr.Status != CheckRan {
		t.Fatalf("status = %q, want %q", cr.Status, CheckRan)
	}

	foundParseError := false
	for _, f := range cr.Findings {
		if f.Code == "tasks.parse_error" {
			foundParseError = true
			if f.Severity != SeverityError {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
			}
			if !strings.Contains(f.Message, "broken.md") || !strings.Contains(f.Message, "backlog") {
				t.Errorf("message = %q, want it to contain filename and state", f.Message)
			}
			if f.Path == "" {
				t.Error("expected non-empty path for parse error finding")
			}
		}
	}
	if !foundParseError {
		t.Error("expected tasks.parse_error finding")
	}
}

func TestCheckTaskParsing_InvalidGlob(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a task with an invalid glob pattern in affects.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "bad-glob.md"),
		"---\nid: bad-glob\npriority: 10\naffects:\n  - \"[invalid\"\n---\nBody\n")

	cc := &checkContext{
		ctx:      context.Background(),
		tasksDir: tasksDir,
	}

	cr := checkTaskParsing(cc)

	foundInvalidGlob := false
	for _, f := range cr.Findings {
		if f.Code == "tasks.invalid_glob" {
			foundInvalidGlob = true
			if f.Severity != SeverityError {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
			}
			if !strings.Contains(f.Message, "bad-glob.md") {
				t.Errorf("message = %q, want it to contain filename", f.Message)
			}
		}
	}
	if !foundInvalidGlob {
		t.Error("expected tasks.invalid_glob finding")
	}
}

func TestCheckTaskParsing_UnsafeAffects(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a task with a path-traversal affects entry.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "unsafe.md"),
		"---\nid: unsafe\npriority: 10\naffects:\n  - \"../../../etc/passwd\"\n---\nBody\n")

	cc := &checkContext{
		ctx:      context.Background(),
		tasksDir: tasksDir,
	}

	cr := checkTaskParsing(cc)

	foundUnsafe := false
	for _, f := range cr.Findings {
		if f.Code == "tasks.unsafe_affects" {
			foundUnsafe = true
			if f.Severity != SeverityError {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
			}
			if !strings.Contains(f.Message, "unsafe.md") {
				t.Errorf("message = %q, want it to contain filename", f.Message)
			}
		}
	}
	if !foundUnsafe {
		t.Error("expected tasks.unsafe_affects finding")
	}
}

func TestCheckTaskParsing_TotalCount(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write two valid tasks.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "task-a.md"),
		"---\nid: task-a\npriority: 10\n---\nTask A\n")
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "task-b.md"),
		"---\nid: task-b\npriority: 20\n---\nTask B\n")

	cc := &checkContext{
		ctx:      context.Background(),
		tasksDir: tasksDir,
	}

	cr := checkTaskParsing(cc)

	foundTotal := false
	for _, f := range cr.Findings {
		if f.Code == "tasks.total_count" {
			foundTotal = true
			if f.Severity != SeverityInfo {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityInfo)
			}
			if !strings.Contains(f.Message, "2") {
				t.Errorf("message = %q, want it to contain '2'", f.Message)
			}
		}
	}
	if !foundTotal {
		t.Error("expected tasks.total_count finding")
	}
}

func TestCheckTaskParsing_EmptyQueue(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	cc := &checkContext{
		ctx:      context.Background(),
		tasksDir: tasksDir,
	}

	cr := checkTaskParsing(cc)

	foundTotal := false
	for _, f := range cr.Findings {
		if f.Code == "tasks.total_count" {
			foundTotal = true
			if !strings.Contains(f.Message, "0") {
				t.Errorf("message = %q, want it to contain '0'", f.Message)
			}
		}
	}
	if !foundTotal {
		t.Error("expected tasks.total_count finding even for empty queue")
	}
}

func TestCheckTaskParsing_MixedFindings(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Valid task.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "good.md"),
		"---\nid: good\npriority: 10\n---\nGood task\n")
	// Broken frontmatter.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "broken.md"),
		"---\n  bad: [unclosed\n---\n")
	// Unsafe affects.
	testutil.WriteFile(t, filepath.Join(tasksDir, "backlog", "traversal.md"),
		"---\nid: traversal\npriority: 5\naffects:\n  - \"../../secret\"\n---\nBody\n")

	cc := &checkContext{
		ctx:      context.Background(),
		tasksDir: tasksDir,
	}

	cr := checkTaskParsing(cc)

	codes := make(map[string]bool)
	for _, f := range cr.Findings {
		codes[f.Code] = true
	}

	if !codes["tasks.parse_error"] {
		t.Error("expected tasks.parse_error finding")
	}
	if !codes["tasks.unsafe_affects"] {
		t.Error("expected tasks.unsafe_affects finding")
	}
	if !codes["tasks.total_count"] {
		t.Error("expected tasks.total_count finding")
	}
}
