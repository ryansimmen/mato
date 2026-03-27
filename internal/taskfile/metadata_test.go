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
		{"unterminated comment", "<!-- claimed-by: abc123\n", "", false},
		{"missing close", "<!-- claimed-by: abc123", "", false},
		{"stray text no comment", "claimed-by: abc123", "", false},
		{"partial open", "<!- claimed-by: abc123 -->", "", false},
		{"truncated line", "<!-- claimed-by: abc123 --", "", false},
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
		{"unterminated comment", "<!-- claimed-by: abc  claimed-at: 2026-03-15T10:30:00Z\n", false},
		{"bare claimed-at", "claimed-at: 2026-03-15T10:30:00Z", false},
		{"no claimed-by prefix", "<!-- claimed-at: 2026-03-15T10:30:00Z -->", false},
		{"truncated marker", "<!-- claimed-by: abc  claimed-at: 2026-03-15T10:30:00Z --", false},
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
		{"step error format", "<!-- failure: a at T1 step=WORK error=tests_failed -->", "tests_failed"},
		{"step error with files changed", "<!-- failure: a at T1 step=WORK error=tests_failed files_changed=main.go -->", "tests_failed"},
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

func TestLastReviewRejectionReason(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"none", "# Task\nNo rejections.", ""},
		{"single", "<!-- review-rejection: a at T — missing tests -->", "missing tests"},
		{"multiple returns last", "<!-- review-rejection: a at T1 — first -->\n<!-- review-rejection: b at T2 — second -->", "second"},
		{"ignores malformed", "<!-- review-rejection: a at T -->", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LastReviewRejectionReason([]byte(tt.data))
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

func TestSanitizeCommentText(t *testing.T) {
	got := SanitizeCommentText("  line one\nline two -->  ")
	if got != "line one line two —>" {
		t.Fatalf("got %q, want %q", got, "line one line two —>")
	}
}

func TestAppendReviewFailure_SanitizesReason(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	if err := os.WriteFile(path, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := AppendReviewFailure(path, "review-abc", "bad\nreason -->"); err != nil {
		t.Fatalf("AppendReviewFailure: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "\n<!-- review-failure: review-abc at") && strings.Contains(string(data), "error=bad reason —>") {
		return
	}
	t.Fatalf("sanitized review-failure not found in file: %s", data)
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

func TestAppendCycleFailureRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	if err := AppendCycleFailureRecord(path); err != nil {
		t.Fatalf("AppendCycleFailureRecord: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- cycle-failure: mato at") {
		t.Fatalf("cycle-failure record not found in file: %s", data)
	}
	if !strings.Contains(string(data), "circular dependency") {
		t.Fatalf("cycle-failure reason not found in file: %s", data)
	}
}

func TestAppendCycleFailureRecord_NonexistentFile(t *testing.T) {
	err := AppendCycleFailureRecord(filepath.Join(t.TempDir(), "missing.md"))
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestAppendCancelledRecord_Format(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	if err := AppendCancelledRecord(path); err != nil {
		t.Fatalf("AppendCancelledRecord: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- cancelled: operator at") {
		t.Fatalf("cancelled record not found in file: %s", data)
	}
}

func TestContainsCancelledMarker(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"present", "# Task\n<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n", true},
		{"absent", "# Task\n<!-- failure: agent at 2026-01-01T00:00:00Z step=WORK error=fail -->\n", false},
		{"inline body text ignored", "# Task\nUse `<!-- cancelled: ... -->` to document operator actions.\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsCancelledMarker([]byte(tt.data)); got != tt.want {
				t.Fatalf("ContainsCancelledMarker() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContainsCycleFailure(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"present", "# Task\n<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n", true},
		{"absent", "# Task\n<!-- failure: agent at 2026-01-01T00:00:00Z step=WORK error=fail -->\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsCycleFailure([]byte(tt.data)); got != tt.want {
				t.Fatalf("ContainsCycleFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCountCycleFailureMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{"none", "# Task\n", 0},
		{"one", "# Task\n<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n", 1},
		{"two", "<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n<!-- cycle-failure: mato at 2026-01-02T00:00:00Z — circular dependency -->\n", 2},
		{"mixed with failure", "<!-- failure: agent at T step=WORK error=e -->\n<!-- cycle-failure: mato at T — circular dependency -->\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountCycleFailureMarkers([]byte(tt.data)); got != tt.want {
				t.Fatalf("CountCycleFailureMarkers() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLastCycleFailureReason(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"present", "<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\n", "circular dependency"},
		{"absent", "<!-- failure: agent at T step=WORK error=fail -->\n", ""},
		{"multiple", "<!-- cycle-failure: mato at T — first -->\n<!-- cycle-failure: mato at T — circular dependency -->\n", "circular dependency"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LastCycleFailureReason([]byte(tt.data)); got != tt.want {
				t.Fatalf("LastCycleFailureReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountFailureMarkers_ExcludesCycleFailure(t *testing.T) {
	// Cycle-failure markers should NOT be counted by CountFailureMarkers.
	data := []byte("<!-- failure: agent at T step=WORK error=e -->\n<!-- cycle-failure: mato at T — circular dependency -->\n")
	if got := CountFailureMarkers(data); got != 1 {
		t.Fatalf("CountFailureMarkers() = %d, want 1 (should exclude cycle-failure)", got)
	}
}

func TestAppendTerminalFailureRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	if err := AppendTerminalFailureRecord(path, "unparseable frontmatter"); err != nil {
		t.Fatalf("AppendTerminalFailureRecord: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- terminal-failure: mato at") {
		t.Fatalf("terminal-failure record not found in file: %s", data)
	}
	if !strings.Contains(string(data), "unparseable frontmatter") {
		t.Fatalf("terminal-failure reason not found in file: %s", data)
	}
}

func TestAppendTerminalFailureRecord_NonexistentFile(t *testing.T) {
	err := AppendTerminalFailureRecord(filepath.Join(t.TempDir(), "missing.md"), "reason")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestAppendTerminalFailureRecord_SanitizesReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	if err := AppendTerminalFailureRecord(path, "bad\nreason -->"); err != nil {
		t.Fatalf("AppendTerminalFailureRecord: %v", err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if strings.Contains(s, "\n<!-- terminal-failure: mato at") && strings.Contains(s, "bad reason —>") {
		return
	}
	t.Fatalf("sanitized terminal-failure not found in file: %s", data)
}

func TestContainsTerminalFailure(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"present", "# Task\n<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\n", true},
		{"absent", "# Task\n<!-- failure: agent at T step=WORK error=fail -->\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsTerminalFailure([]byte(tt.data)); got != tt.want {
				t.Fatalf("ContainsTerminalFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCountTerminalFailureMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{"none", "# Task\n", 0},
		{"one", "<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\n", 1},
		{"two", "<!-- terminal-failure: mato at T1 — reason1 -->\n<!-- terminal-failure: mato at T2 — reason2 -->\n", 2},
		{"mixed with failure", "<!-- failure: agent at T step=WORK error=e -->\n<!-- terminal-failure: mato at T — reason -->\n", 1},
		{"mixed with cycle", "<!-- cycle-failure: mato at T — circular dependency -->\n<!-- terminal-failure: mato at T — reason -->\n", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountTerminalFailureMarkers([]byte(tt.data)); got != tt.want {
				t.Fatalf("CountTerminalFailureMarkers() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLastTerminalFailureReason(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"present", "<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\n", "unparseable frontmatter"},
		{"absent", "<!-- failure: agent at T step=WORK error=fail -->\n", ""},
		{"multiple returns last", "<!-- terminal-failure: mato at T — first -->\n<!-- terminal-failure: mato at T — review retry exhausted -->\n", "review retry exhausted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LastTerminalFailureReason([]byte(tt.data)); got != tt.want {
				t.Fatalf("LastTerminalFailureReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountFailureMarkers_ExcludesTerminalFailure(t *testing.T) {
	data := []byte("<!-- failure: agent at T step=WORK error=e -->\n<!-- terminal-failure: mato at T — reason -->\n")
	if got := CountFailureMarkers(data); got != 1 {
		t.Fatalf("CountFailureMarkers() = %d, want 1 (should exclude terminal-failure)", got)
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
			name: "strips all failure marker types",
			input: `<!-- branch: task/foo -->
# Title

Body text.

<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->
<!-- review-failure: def at 2026-01-02T00:00:00Z step=REVIEW error=timeout -->
<!-- cancelled: operator at 2026-01-02T06:00:00Z -->
<!-- cycle-failure: mato at 2026-01-03T00:00:00Z — circular dependency -->
<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->
<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable -->
`,
			want: "<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->",
			notWant: []string{
				"<!-- failure:",
				"<!-- review-failure:",
				"<!-- cancelled:",
				"<!-- cycle-failure:",
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
		{
			name: "preserves review rejection feedback",
			input: `# Title

<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->

<!-- terminal-failure: mato at 2026-01-05T00:00:00Z — unparseable -->
`,
			want:    "<!-- review-rejection: reviewer at 2026-01-04T00:00:00Z — bad code -->",
			notWant: []string{"<!-- terminal-failure:"},
		},
		{
			name: "collapses excessive blank lines",
			input: `# Title


<!-- failure: a at T step=WORK error=e -->


Body continues.
`,
			want: "Body continues.",
		},
		{
			name:  "handles empty input",
			input: "",
			want:  "\n",
		},
		{
			name: "strips indented failure markers",
			input: `# Title
  <!-- failure: a at T step=WORK error=e -->
Body.
`,
			want:    "Body.",
			notWant: []string{"<!-- failure:"},
		},
		{
			name: "handles multiple consecutive failure markers",
			input: `# Title
<!-- failure: a at T1 step=WORK error=e1 -->
<!-- failure: b at T2 step=WORK error=e2 -->
<!-- review-failure: c at T3 step=REVIEW error=e3 -->
Body.
`,
			want:    "Body.",
			notWant: []string{"<!-- failure:", "<!-- review-failure:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripFailureMarkers(tt.input)
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

func TestStripFailureMarkers_TrailingNewline(t *testing.T) {
	got := StripFailureMarkers("# Title\n")
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("result should end with newline, got: %q", got)
	}
	// Should not end with multiple newlines.
	if strings.HasSuffix(got, "\n\n") {
		t.Errorf("result should not end with double newline, got: %q", got)
	}
}

func TestCountFailureMarkers_IgnoresCancelled(t *testing.T) {
	data := []byte("<!-- failure: agent at T step=WORK error=e -->\n<!-- cancelled: operator at 2026-01-01T00:00:00Z -->\n")
	if got := CountFailureMarkers(data); got != 1 {
		t.Fatalf("CountFailureMarkers() = %d, want 1 (should ignore cancelled)", got)
	}
}

// Regression tests: marker-like text in prose and fenced code must not be
// treated as real scheduler metadata. Only standalone trimmed lines count.

func TestParseClaimedBy_IgnoresProseMarkers(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		want   string
		wantOK bool
	}{
		{
			"inline backtick prose",
			"# Task\nThe `<!-- claimed-by: agent -->` marker is used for claims.\n",
			"", false,
		},
		{
			"fenced code block",
			"# Task\n```\n<!-- claimed-by: agent42 -->\n```\n",
			"agent42", true, // fenced code lines still start with the marker
		},
		{
			"embedded in sentence",
			"# Task\nUse the <!-- claimed-by: agentX --> comment to claim.\n",
			"", false,
		},
		{
			"real marker on own line",
			"# Task\n<!-- claimed-by: real-agent  claimed-at: 2026-01-01T00:00:00Z -->\nSome prose about <!-- claimed-by: fake -->.\n",
			"real-agent", true,
		},
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

func TestParseClaimedAt_IgnoresProseMarkers(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		wantOK bool
	}{
		{
			"inline backtick prose",
			"# Task\nThe `<!-- claimed-by: abc  claimed-at: 2026-03-15T10:30:00Z -->` marker.\n",
			false,
		},
		{
			"embedded in sentence",
			"# Task\nUse <!-- claimed-by: abc  claimed-at: 2026-03-15T10:30:00Z --> to mark.\n",
			false,
		},
		{
			"real marker on own line",
			"# Task\n<!-- claimed-by: abc  claimed-at: 2026-03-15T10:30:00Z -->\nProse about <!-- claimed-by: x  claimed-at: 2025-01-01T00:00:00Z -->.\n",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := ParseClaimedAt([]byte(tt.data))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestContainsFailureFrom_IgnoresProseMarkers(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		agentID string
		want    bool
	}{
		{
			"inline prose",
			"# Task\nLook for `<!-- failure: abc12345 at ... -->` records.\n",
			"abc12345", false,
		},
		{
			"prose sentence",
			"# Task\nThe <!-- failure: abc12345 at 2026-01-01T00:00:00Z step=WORK error=fail --> marker.\n",
			"abc12345", false,
		},
		{
			"real marker on own line",
			"# Task\n<!-- failure: abc12345 at 2026-01-01T00:00:00Z step=WORK error=fail -->\nProse about <!-- failure: abc12345 -->.\n",
			"abc12345", true,
		},
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

func TestContainsCycleFailure_IgnoresProseMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			"inline backtick prose",
			"# Task\nThe `<!-- cycle-failure: mato at ... -->` marker is for cycle detection.\n",
			false,
		},
		{
			"prose sentence",
			"# Task\nSee <!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency --> for details.\n",
			false,
		},
		{
			"real marker on own line",
			"# Task\n<!-- cycle-failure: mato at 2026-01-01T00:00:00Z — circular dependency -->\nProse about <!-- cycle-failure: -->.\n",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsCycleFailure([]byte(tt.data)); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestContainsTerminalFailure_IgnoresProseMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{
			"inline backtick prose",
			"# Task\nUse `<!-- terminal-failure: ... -->` for permanent errors.\n",
			false,
		},
		{
			"prose sentence",
			"# Task\nThe <!-- terminal-failure: mato at T — reason --> marker is terminal.\n",
			false,
		},
		{
			"real marker on own line",
			"# Task\n<!-- terminal-failure: mato at 2026-01-01T00:00:00Z — unparseable frontmatter -->\nProse about <!-- terminal-failure: -->.\n",
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsTerminalFailure([]byte(tt.data)); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractReviewRejections_IgnoresProseMarkers(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			"inline backtick prose",
			"# Task\nThe `<!-- review-rejection: reviewer -->` comment is for rejection feedback.\n",
			"",
		},
		{
			"prose sentence",
			"# Task\nSee the <!-- review-rejection: reviewer at T — reason --> for details.\n",
			"",
		},
		{
			"real marker on own line",
			"# Task\n<!-- review-rejection: reviewer-1 at T — bad code -->\nProse about <!-- review-rejection: fake -->.\n",
			"<!-- review-rejection: reviewer-1 at T — bad code -->",
		},
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
