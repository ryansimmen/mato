package taskfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseBranchComment(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		want   string
		wantOK bool
	}{
		{"simple", "<!-- branch: task/foo-bar -->", "task/foo-bar", true},
		{"extra whitespace", "<!-- branch:   task/spaces   -->", "task/spaces", true},
		{"no whitespace", "<!-- branch:task/nospace -->", "task/nospace", true},
		{"with slashes", "<!-- branch: task/deep/nested -->", "task/deep/nested", true},
		{"in multiline", "line1\n<!-- branch: task/mid -->\nline3", "task/mid", true},
		{"first match wins", "<!-- branch: task/first -->\n<!-- branch: task/second -->", "task/first", true},
		{"unterminated", "<!-- branch: task/open\n", "", false},
		{"empty", "", "", false},
		{"no marker", "# Just a heading\nSome text.", "", false},
		{"missing branch value", "<!-- branch: -->", "", false},
		{"whitespace only value", "<!-- branch:    -->", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseBranchComment([]byte(tt.data))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseClaimedBy(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		want   string
		wantOK bool
	}{
		{"simple", "<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->", "abc123", true},
		{"no claimed-at", "<!-- claimed-by: agent42 -->", "agent42", true},
		{"extra whitespace", "<!-- claimed-by:   def789 -->", "def789", true},
		{"multiline", "line1\n<!-- claimed-by: xyz -->", "xyz", true},
		{"empty", "", "", false},
		{"no marker", "# Task\nNo claim here.", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseClaimedBy([]byte(tt.data))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseClaimedAt(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		wantOK bool
	}{
		{"valid", "<!-- claimed-by: abc  claimed-at: 2026-03-15T10:30:00Z -->", true},
		{"invalid timestamp", "<!-- claimed-by: abc  claimed-at: not-a-date -->", false},
		{"missing", "# No metadata", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseClaimedAt([]byte(tt.data))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK {
				want, _ := time.Parse(time.RFC3339, "2026-03-15T10:30:00Z")
				if !got.Equal(want) {
					t.Fatalf("got %v, want %v", got, want)
				}
			}
		})
	}
}

func TestCountFailureMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{"none", "# Task\nNo failures.", 0},
		{"one", "<!-- failure: agent-1 at 2026-01-01T00:00:00Z step=WORK error=fail -->", 1},
		{"two", "<!-- failure: a1 at T1 step=WORK error=e1 -->\n<!-- failure: a2 at T2 step=WORK error=e2 -->", 2},
		{"ignores review-failure", "<!-- failure: a at T step=WORK error=e -->\n<!-- review-failure: b at T step=REVIEW error=e -->", 1},
		{"ignores body text", "# Task\n`CountFailureLines()` counts `<!-- failure: ... -->` records.\n<!-- failure: a at T step=WORK error=e -->", 1},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountFailureMarkers([]byte(tt.data))
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCountReviewFailureMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{"none", "# Task", 0},
		{"one", "<!-- review-failure: a at T step=REVIEW error=e -->", 1},
		{"two", "<!-- review-failure: a at T1 step=REVIEW error=e1 -->\n<!-- review-failure: b at T2 step=REVIEW error=e2 -->", 2},
		{"ignores task failure", "<!-- failure: a at T step=WORK error=e -->\n<!-- review-failure: b at T step=REVIEW error=e -->", 1},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountReviewFailureMarkers([]byte(tt.data))
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExtractFailureLines(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"none", "# Task\nNo failures.", ""},
		{"single", "<!-- failure: a at T step=WORK error=e -->", "<!-- failure: a at T step=WORK error=e -->"},
		{"multiple", "<!-- failure: a at T1 step=WORK error=e1 -->\nother\n<!-- failure: b at T2 step=WORK error=e2 -->",
			"<!-- failure: a at T1 step=WORK error=e1 -->\n<!-- failure: b at T2 step=WORK error=e2 -->"},
		{"body reference ignored", "# Retry budget\n`CountFailureLines()` counts `<!-- failure: ... -->` records.\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed -->",
			"<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed -->"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFailureLines([]byte(tt.data))
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractReviewRejections(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"none", "# Task", ""},
		{"single", "<!-- review-rejection: a at T — reason -->", "<!-- review-rejection: a at T — reason -->"},
		{"multiple", "<!-- review-rejection: a at T1 — r1 -->\nother\n<!-- review-rejection: b at T2 — r2 -->",
			"<!-- review-rejection: a at T1 — r1 -->\n<!-- review-rejection: b at T2 — r2 -->"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractReviewRejections([]byte(tt.data))
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContainsFailureFrom(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		agentID string
		want    bool
	}{
		{"present", "<!-- failure: abc12345 at 2026-01-01T00:00:00Z step=WORK error=fail -->", "abc12345", true},
		{"absent", "<!-- failure: other at 2026-01-01T00:00:00Z step=WORK error=fail -->", "abc12345", false},
		{"empty", "", "abc12345", false},
		{"no failure lines", "# Task\nJust text.", "abc12345", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainsFailureFrom([]byte(tt.data), tt.agentID)
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastFailureReason(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"none", "# Task\nNo failures.", ""},
		{"single", "<!-- failure: a at T — tests failed -->", "tests failed"},
		{"multiple returns last", "<!-- failure: a at T1 — first error -->\n<!-- failure: b at T2 — second error -->", "second error"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LastFailureReason([]byte(tt.data))
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteBranchComment(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteBranchComment(&buf, "task/my-branch"); err != nil {
		t.Fatalf("WriteBranchComment: %v", err)
	}
	want := "<!-- branch: task/my-branch -->"
	if got := buf.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWriteClaimedByComment(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteClaimedByComment(&buf, "agent-42", "2026-01-15T10:00:00Z"); err != nil {
		t.Fatalf("WriteClaimedByComment: %v", err)
	}
	want := "<!-- claimed-by: agent-42  claimed-at: 2026-01-15T10:00:00Z -->"
	if got := buf.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAppendFailureRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := AppendFailureRecord(path, "agent-1", "WORK", "build_failed"); err != nil {
		t.Fatalf("AppendFailureRecord: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- failure: agent-1 at") {
		t.Fatalf("failure record not found in file: %s", data)
	}
	if !strings.Contains(string(data), "step=WORK error=build_failed") {
		t.Fatalf("failure details not found in file: %s", data)
	}
}

func TestAppendFailureRecord_NonexistentFile(t *testing.T) {
	err := AppendFailureRecord(filepath.Join(t.TempDir(), "missing.md"), "agent-1", "WORK", "err")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestAppendReviewFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := AppendReviewFailure(path, "review-abc", "could not fetch branch"); err != nil {
		t.Fatalf("AppendReviewFailure: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- review-failure: review-abc at") {
		t.Fatalf("review-failure record not found in file: %s", data)
	}
	if !strings.Contains(string(data), "step=REVIEW error=could not fetch branch") {
		t.Fatalf("review-failure details not found in file: %s", data)
	}
}

func TestAppendReviewFailure_NonexistentFile(t *testing.T) {
	err := AppendReviewFailure(filepath.Join(t.TempDir(), "missing.md"), "agent-1", "err")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
