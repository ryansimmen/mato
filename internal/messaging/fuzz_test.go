package messaging

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fuzzMaxInputBytes = 1 << 16 // 64 KiB cap for JSON payloads.

// silenceStderr redirects os.Stderr for the duration of the returned restore
// callback. The read functions print warnings to stderr for malformed payloads
// which would otherwise flood fuzz output.
func silenceStderr(t *testing.T) func() {
	t.Helper()
	orig := os.Stderr
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		// Fall back to a pipe-and-discard if /dev/null cannot be opened.
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatalf("silence stderr: %v", perr)
		}
		go func() { _, _ = io.Copy(io.Discard, r) }()
		os.Stderr = w
		return func() {
			os.Stderr = orig
			_ = w.Close()
			_ = r.Close()
		}
	}
	os.Stderr = devnull
	return func() {
		os.Stderr = orig
		_ = devnull.Close()
	}
}

// FuzzSafeEncode asserts the contracts of the collision-resistant filename
// encoder: result charset is restricted, no path separators or traversal
// sequences appear, length is bounded, and the encoding is injective.
func FuzzSafeEncode(f *testing.F) {
	seeds := []string{
		"",
		"agent-1",
		"foo/bar",
		"foo-bar",
		"../evil",
		"../../etc/passwd",
		"a/../b",
		"agent \"\u96ea\"\nbackslash \\",
		"  spaces  ",
		"\x00\x01\x7f\xff",
		strings.Repeat("a", 200),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > fuzzMaxInputBytes {
			return
		}
		out := safeEncode(s)

		for i := 0; i < len(out); i++ {
			c := out[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
				(c < '0' || c > '9') && c != '-' && c != '_' {
				t.Fatalf("safeEncode(%q) produced invalid byte 0x%02x in %q",
					s, c, out)
			}
		}
		if strings.ContainsAny(out, `/\`) {
			t.Fatalf("safeEncode(%q) = %q contains path separator", s, out)
		}
		if strings.Contains(out, "..") {
			t.Fatalf("safeEncode(%q) = %q contains traversal sequence", s, out)
		}
		if filepath.Clean(out) != out && out != "" {
			t.Fatalf("safeEncode(%q) = %q is not filepath.Clean stable", s, out)
		}
		// Length bound: each input byte produces at most 3 output bytes
		// (underscore + two hex digits).
		if len(out) > 3*len(s) {
			t.Fatalf("safeEncode(%q) length %d exceeds 3x input %d",
				s, len(out), len(s))
		}
		// Injectivity: appending a passthrough character must change the
		// output. This catches collision regressions in the encoding scheme.
		out2 := safeEncode(s + "x")
		if out2 == out {
			t.Fatalf("safeEncode collision: %q and %q both encode to %q",
				s, s+"x", out)
		}
	})
}

// FuzzReadMessagesJSON writes a single fuzz-controlled JSON payload to a
// temporary events directory and asserts that ReadMessages handles any input
// without panicking. Malformed payloads must be silently skipped.
func FuzzReadMessagesJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"id":"abc","from":"agent-1","type":"intent","task":"task.md","branch":"feature/test","body":"hi","sent_at":"2024-05-01T12:34:56.123456789Z"}`),
		[]byte(`{"id":"x","type":"conflict-warning","files":["a","b"],"sent_at":"2024-05-01T12:00:00Z"}`),
		[]byte(`{"id":"y","type":"intent","sent_at":"2024-05-01T12:00:00Z"}`),
		[]byte(`{"id":"x","type":"intent","unknown_field":42,"sent_at":"2024-05-01T12:00:00Z"}`),
		[]byte(`{"sent_at":"not-a-time"}`),
		[]byte(`{"files":"not-an-array"}`),
		[]byte(`{`),
		[]byte(``),
		[]byte(`null`),
		[]byte(`[]`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > fuzzMaxInputBytes {
			return
		}
		restore := silenceStderr(t)
		defer restore()

		dir := t.TempDir()
		if err := Init(dir); err != nil {
			t.Fatalf("Init: %v", err)
		}
		path := filepath.Join(dir, "messages", "events", "fuzz.json")
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}

		msgs, err := ReadMessages(dir, time.Time{})
		if err != nil {
			t.Fatalf("ReadMessages returned error for single-file directory: %v", err)
		}
		if len(msgs) > 1 {
			t.Fatalf("ReadMessages returned %d messages from one file", len(msgs))
		}
	})
}

// FuzzReadCompletionDetailJSON writes a fuzz-controlled JSON payload at the
// path that ReadCompletionDetail will derive from a fuzz-controlled task ID.
// The key invariant is path containment: the resolved path must always live
// inside <tasksDir>/messages/completions/, even for traversal-attempting IDs.
func FuzzReadCompletionDetailJSON(f *testing.F) {
	type seed struct {
		taskID  string
		payload []byte
	}
	seeds := []seed{
		{"add-http-retries", []byte(`{"task_id":"add-http-retries","branch":"task/add-http-retries","commit_sha":"abc","files_changed":["a.go"],"title":"t","merged_at":"2026-03-17T21:35:00Z"}`)},
		{"x", []byte(`{"task_id":"x","merged_at":"2026-03-17T21:35:00Z"}`)},
		{"x", []byte(`{"task_id":"x"}`)},
		{"../../etc/passwd", []byte(`{}`)},
		{"a/../b", []byte(`{}`)},
		{"foo/bar", []byte(`{}`)},
		{"x", []byte(`{`)},
		{"x", []byte(``)},
		{"", []byte(`{}`)},
	}
	for _, s := range seeds {
		f.Add(s.taskID, s.payload)
	}

	f.Fuzz(func(t *testing.T, taskID string, payload []byte) {
		if len(payload) > fuzzMaxInputBytes {
			return
		}
		// Bound taskID so the encoded filename stays within the typical 255-byte
		// filesystem limit (each non-passthrough byte expands to 3 output
		// bytes). This is a harness constraint, not a function constraint.
		if len(taskID) > 80 {
			return
		}
		restore := silenceStderr(t)
		defer restore()

		dir := t.TempDir()
		if err := Init(dir); err != nil {
			t.Fatalf("Init: %v", err)
		}
		completionsRoot := filepath.Join(dir, "messages", "completions")
		filename := completionFilename(taskID) + ".json"
		path := filepath.Join(completionsRoot, filename)

		// Path-containment invariant: the resolved write path must live inside
		// the completions directory, regardless of how malicious taskID is.
		// This is the security contract documented for completion file naming.
		absRoot, err := filepath.Abs(completionsRoot)
		if err != nil {
			t.Fatalf("abs root: %v", err)
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			t.Fatalf("abs path: %v", err)
		}
		rel, err := filepath.Rel(absRoot, absPath)
		if err != nil {
			t.Fatalf("rel: %v", err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("path escape: taskID=%q produced path %q outside %q",
				taskID, absPath, absRoot)
		}

		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}

		// Reader must not panic on any payload.
		_, _ = ReadCompletionDetail(dir, taskID)
	})
}

// FuzzReadAllPresenceJSON writes a fuzz-controlled JSON payload to a
// temporary presence directory and asserts ReadAllPresence handles any input
// without panicking. The map size cannot exceed the number of files written.
func FuzzReadAllPresenceJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"agent_id":"agent-1","task":"task.md","branch":"feature/branch","updated_at":"2024-05-01T12:00:00Z"}`),
		[]byte(`{"task":"x","branch":"y"}`),
		[]byte(`{"agent_id":"","task":"x"}`),
		[]byte(`{"agent_id":"a","updated_at":"not-a-time"}`),
		[]byte(`{"agent_id":"a","unknown":[1,2]}`),
		[]byte(`{"agent_id":"agent \"\u96ea\""}`),
		[]byte(`{`),
		[]byte(``),
		[]byte(`null`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > fuzzMaxInputBytes {
			return
		}
		restore := silenceStderr(t)
		defer restore()

		dir := t.TempDir()
		if err := Init(dir); err != nil {
			t.Fatalf("Init: %v", err)
		}
		path := filepath.Join(dir, "messages", "presence", "fuzz.json")
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("write payload: %v", err)
		}

		got, err := ReadAllPresence(dir)
		if err != nil {
			t.Fatalf("ReadAllPresence returned error for single-file directory: %v", err)
		}
		if len(got) > 1 {
			t.Fatalf("ReadAllPresence returned %d entries from one file", len(got))
		}
	})
}
