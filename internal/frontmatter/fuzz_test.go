package frontmatter

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzParseTaskData ensures ParseTaskData never panics on arbitrary byte
// inputs. ParseTaskData is the entry point for all task-file parsing and
// receives untrusted contents from the filesystem queue.
func FuzzParseTaskData(f *testing.F) {
	seeds := [][]byte{
		// Fully populated frontmatter.
		[]byte("---\nid: a\npriority: 7\ndepends_on: [b, c]\naffects:\n  - api\nmax_retries: 5\n---\n# Title\nBody.\n"),
		// Minimal frontmatter.
		[]byte("---\nid: solo\n---\n"),
		// Empty frontmatter block.
		[]byte("---\n---\nbody\n"),
		// Unterminated frontmatter (must return an error, not panic).
		[]byte("---\nid: x\nno closing delimiter\n"),
		// Body only, no frontmatter.
		[]byte("# Just a body\nNo frontmatter at all.\n"),
		// Empty input.
		[]byte(""),
		// Managed comments before frontmatter (claim.go prepends these).
		[]byte("<!-- claimed-by: agent-abc -->\n<!-- branch: task/foo -->\n---\nid: foo\n---\nBody\n"),
		// CRLF line endings.
		[]byte("---\r\nid: crlf\r\npriority: 1\r\n---\r\nBody\r\n"),
		// Path-traversal and absolute affects entries (sanitizer must strip).
		[]byte("---\nid: t\naffects:\n  - ../escape\n  - /etc/passwd\n  - internal/foo.go\n---\n"),
		// Negative max_retries (must return an error, not panic).
		[]byte("---\nid: n\nmax_retries: -1\n---\n"),
		// Explicit YAML null values.
		[]byte("---\nid: defaults\npriority: null\nmax_retries: null\n---\n"),
		// Glob patterns in affects.
		[]byte("---\nid: g\naffects:\n  - \"**/*.go\"\n  - \"cmd/*\"\n---\n"),
		// Malformed YAML inside frontmatter (must return an error, not panic).
		[]byte("---\nid: [unclosed\n---\n"),
		// Unknown YAML field (KnownFields(true) should reject).
		[]byte("---\nid: u\nbogus_field: 1\n---\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Property: ParseTaskData must not panic on any input.
		// Errors are an acceptable outcome.
		_, _, _ = ParseTaskData(data, "fuzz.md")
	})
}

// FuzzExtractTitle ensures ExtractTitle never panics and always returns a
// non-empty string (it falls back to TaskFileStem(filename) when the body
// produces no candidate line).
func FuzzExtractTitle(f *testing.F) {
	seeds := []struct {
		filename string
		body     string
	}{
		{"task.md", "# Heading\nBody"},
		{"task.md", "<!-- comment -->\n# Heading"},
		{"task.md", ""},
		{"empty.md", "\n\n   \n"},
		{"task.md", "###   Triple hash heading"},
		{"task.md", "no heading just text"},
		{"weird-name.md", "<!-- only -->\n<!-- comments -->\n"},
		{"", ""},
	}
	for _, s := range seeds {
		f.Add(s.filename, s.body)
	}

	f.Fuzz(func(t *testing.T, filename, body string) {
		// Property: must not panic. When filename is non-empty, the documented
		// fallback to TaskFileStem(filename) must yield a non-empty string.
		// Production callers always supply a real filename; the empty-filename
		// case is excluded from the non-empty assertion.
		got := ExtractTitle(filename, body)
		if filename != "" && TaskFileStem(filename) != "" && got == "" {
			t.Fatalf("ExtractTitle(%q, %q) returned empty string; should fall back to TaskFileStem",
				filename, body)
		}
	})
}

// FuzzSanitizeBranchName verifies the branch-name sanitizer's invariants for
// any input string: result is non-empty, contains only [a-zA-Z0-9-], has no
// leading/trailing dash, and contains no consecutive dashes.
func FuzzSanitizeBranchName(f *testing.F) {
	seeds := []string{
		"simple-task",
		"task with spaces.md",
		"",
		"---",
		"already-clean",
		"UPPER_lower-Mixed.md",
		"lots////of-slashes.md",
		"unicode-\u00e9\u4e2d\u6587.md",
		strings.Repeat("a", 300),
		"!!!.md",
		"-leading-and-trailing-",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got := SanitizeBranchName(name)

		if got == "" {
			t.Fatalf("SanitizeBranchName(%q) returned empty string", name)
		}
		if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
			t.Fatalf("SanitizeBranchName(%q) = %q has leading or trailing dash", name, got)
		}
		if strings.Contains(got, "--") {
			t.Fatalf("SanitizeBranchName(%q) = %q contains consecutive dashes", name, got)
		}
		for _, r := range got {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') &&
				(r < '0' || r > '9') && r != '-' {
				t.Fatalf("SanitizeBranchName(%q) = %q contains invalid rune %q",
					name, got, r)
			}
		}
		if !utf8.ValidString(got) {
			t.Fatalf("SanitizeBranchName(%q) = %q is not valid UTF-8", name, got)
		}
	})
}

// FuzzValidateAffectsGlobs ensures the glob validator never panics on
// arbitrary inputs. Returning an error is acceptable; only panics fail.
func FuzzValidateAffectsGlobs(f *testing.F) {
	seeds := []string{
		"**/*.go",
		"cmd/*",
		"plain/path.go",
		"trailing/slash/",
		"glob/with/trailing/",
		"**/[invalid",
		"",
		"{a,b,c}/*.md",
		"a/**/b/**/c",
		"???",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, entry string) {
		// Property: must not panic. Error returns are fine.
		_ = ValidateAffectsGlobs([]string{entry})
	})
}
