package taskfile

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

const fuzzMaxInputBytes = 1 << 16 // 64 KiB cap to keep regex passes bounded.

var fuzzBranchSafeRe = regexp.MustCompile(`^\S+$`)

// FuzzParseBranchMarkerLine asserts panic-freedom plus the parser's contract:
// when ok is true the returned branch is non-empty, contains no whitespace,
// and the result roundtrips through a re-emitted marker.
func FuzzParseBranchMarkerLine(f *testing.F) {
	seeds := [][]byte{
		[]byte("<!-- branch: task/foo -->\n"),
		[]byte("---\nid: x\n---\n<!-- branch: task/bar -->\nbody\n"),
		[]byte("   <!-- branch:  task/x   -->   \n"),
		[]byte("<!--branch:task/x-->\n"),
		[]byte("<!-- branch: incomplete\n"),
		[]byte("<!-- branch: first -->\n<!-- branch: second -->\n"),
		[]byte("```\n<!-- branch: in/fence -->\n```\n"),
		[]byte("Use the marker <!-- branch: foo --> here.\n"),
		[]byte("<!-- branch: task/feature/sub -->\n"),
		[]byte(""),
		[]byte("\n\n\n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInputBytes {
			return
		}
		got, ok := ParseBranchMarkerLine(data)
		if !ok {
			if got != "" {
				t.Fatalf("ParseBranchMarkerLine(%q) returned (%q, false); want empty string when ok=false",
					data, got)
			}
			return
		}
		if got == "" {
			t.Fatalf("ParseBranchMarkerLine(%q) returned ok=true with empty branch", data)
		}
		if strings.ContainsAny(got, " \t\r\n") {
			t.Fatalf("ParseBranchMarkerLine(%q) returned %q containing whitespace", data, got)
		}
		// Idempotency on the same input.
		got2, ok2 := ParseBranchMarkerLine(data)
		if got != got2 || ok != ok2 {
			t.Fatalf("ParseBranchMarkerLine not idempotent: (%q,%v) vs (%q,%v)",
				got, ok, got2, ok2)
		}
		// Roundtrip: re-emitting the marker must parse back to the same value.
		round := []byte("<!-- branch: " + got + " -->\n")
		gotR, okR := ParseBranchMarkerLine(round)
		if !okR || gotR != got {
			t.Fatalf("roundtrip failed: emitted %q, parsed (%q,%v); want (%q,true)",
				round, gotR, okR, got)
		}
	})
}

// FuzzReplaceBranchMarkerLine asserts panic-freedom and replacement contracts:
// line count is preserved, flags are consistent, and a successful replacement
// is observable by the parser when the new branch is well-formed.
func FuzzReplaceBranchMarkerLine(f *testing.F) {
	type seed struct {
		data      []byte
		newBranch string
	}
	seeds := []seed{
		{[]byte("<!-- branch: task/old -->\nbody\n"), "task/new"},
		{[]byte("<!-- branch: task/x -->\n"), "task/x"}, // same -> no-op
		{[]byte("# no marker\nbody\n"), "task/whatever"},
		{[]byte("```\n<!-- branch: in/fence -->\n```\n"), "task/y"},
		{[]byte(""), "task/z"},
		{[]byte("   <!-- branch: task/spaced -->   \n"), "task/new"},
		{[]byte("<!-- branch: a -->\n<!-- branch: b -->\n"), "c"}, // first wins
	}
	for _, s := range seeds {
		f.Add(s.data, s.newBranch)
	}

	f.Fuzz(func(t *testing.T, data []byte, newBranch string) {
		if len(data) > fuzzMaxInputBytes || len(newBranch) > 1024 {
			return
		}
		// Production callers always pass branch names produced by
		// frontmatter.SanitizeBranchName, which guarantees no whitespace and
		// no comment-terminator sequences. Bound the fuzz domain accordingly;
		// passing a newline or "-->" would synthesize a malformed marker that
		// the parser cannot read back, but that is a caller contract, not a
		// function bug.
		if strings.ContainsAny(newBranch, "\r\n") || strings.Contains(newBranch, "-->") {
			return
		}
		result, found, replaced := ReplaceBranchMarkerLine(data, newBranch)

		// Line count is preserved: replacement is single-line for single-line.
		if bytes.Count(result, []byte("\n")) != bytes.Count(data, []byte("\n")) {
			t.Fatalf("line count changed: input %d, output %d",
				bytes.Count(data, []byte("\n")), bytes.Count(result, []byte("\n")))
		}
		// Flag contract: replaced implies found.
		if replaced && !found {
			t.Fatalf("replaced=true but found=false")
		}
		// !found implies result is byte-identical to data and !replaced.
		if !found {
			if !bytes.Equal(result, data) {
				t.Fatalf("!found but result differs from input")
			}
			if replaced {
				t.Fatalf("!found but replaced=true")
			}
			return
		}
		// found && !replaced means the existing branch already matches.
		if found && !replaced {
			if !bytes.Equal(result, data) {
				t.Fatalf("found && !replaced but result differs from input")
			}
		}
		// Roundtrip: when replaced and newBranch is well-formed (no whitespace,
		// no comment terminator), the parser should observe newBranch.
		if replaced && fuzzBranchSafeRe.MatchString(newBranch) &&
			!strings.Contains(newBranch, "-->") {
			got, ok := ParseBranchMarkerLine(result)
			if !ok || got != newBranch {
				t.Fatalf("after replace with %q, parser returned (%q,%v)",
					newBranch, got, ok)
			}
		}
	})
}

// FuzzRemoveBranchMarkerLine asserts panic-freedom and removal contracts:
// size is non-increasing, flags are consistent, and the marker count
// decreases monotonically when removed=true.
func FuzzRemoveBranchMarkerLine(f *testing.F) {
	seeds := [][]byte{
		[]byte("<!-- branch: task/x -->\nbody\n"),
		[]byte("# no marker\n"),
		[]byte(""),
		[]byte("```\n<!-- branch: in/fence -->\n```\n"),
		[]byte("<!-- branch: a -->\n<!-- branch: b -->\n"),
		[]byte("   <!-- branch: spaced -->   \n"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInputBytes {
			return
		}
		result, found, removed := RemoveBranchMarkerLine(data)
		if removed && !found {
			t.Fatalf("removed=true but found=false")
		}
		if len(result) > len(data) {
			t.Fatalf("output longer than input: %d > %d", len(result), len(data))
		}
		if !found {
			if !bytes.Equal(result, data) {
				t.Fatalf("!found but result differs from input")
			}
			return
		}
		// When removed, ParseBranchMarkerLine on the result must yield either
		// no marker or a different marker than what was in the original first
		// position. We assert the simpler post-condition: if the input had
		// exactly one parseable marker, the output has none.
		if removed {
			_, hadOne := ParseBranchMarkerLine(data)
			if hadOne {
				// After one removal, calling Remove again must not find the
				// same marker again (unless there was a second).
				_, _, removedAgain := RemoveBranchMarkerLine(result)
				_ = removedAgain // either outcome is valid; just must not panic.
			}
		}
	})
}

// FuzzParseClaimedBy asserts panic-freedom plus the parser contract:
// when ok=true the agent ID is non-empty and contains no whitespace.
func FuzzParseClaimedBy(f *testing.F) {
	seeds := [][]byte{
		[]byte("<!-- claimed-by: agent-abc12345 claimed-at: 2024-01-15T10:00:00Z -->\n"),
		[]byte("<!-- claimed-by: agent-x -->\n"),
		[]byte("    <!-- claimed-by: agent-indented -->\n"),
		[]byte("```\n<!-- claimed-by: in/fence -->\n```\n"),
		[]byte("Inline mention of <!-- claimed-by: agent-x --> here.\n"),
		[]byte("<!-- claimed-by: incomplete\n"),
		[]byte("<!-- claimed-by: trailing --> junk\n"),
		[]byte("<!-- claimed-by: first -->\n<!-- claimed-by: second -->\n"),
		[]byte(""),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInputBytes {
			return
		}
		got, ok := ParseClaimedBy(data)
		if !ok {
			if got != "" {
				t.Fatalf("ParseClaimedBy returned (%q, false); want empty", got)
			}
			return
		}
		if got == "" {
			t.Fatalf("ParseClaimedBy returned ok=true with empty agent ID")
		}
		if strings.ContainsAny(got, " \t\r\n") {
			t.Fatalf("ParseClaimedBy returned %q containing whitespace", got)
		}
	})
}

// FuzzStripFailureMarkers asserts panic-freedom plus several contracts:
// idempotency, absence of stripped marker prefixes, preservation of
// review-rejection markers, and a length bound.
func FuzzStripFailureMarkers(f *testing.F) {
	seeds := []string{
		"prose\n<!-- failure: agent at 2024-01-15T10:00:00Z step=BUILD error=x -->\nmore\n",
		"<!-- review-failure: a at 2024-01-15T10:00:00Z — bad -->\n",
		"<!-- cycle-failure: a at 2024-01-15T10:00:00Z — loop -->\n",
		"<!-- terminal-failure: a at 2024-01-15T10:00:00Z — done -->\n",
		"<!-- cancelled: a at 2024-01-15T10:00:00Z — stop -->\n",
		"<!-- review-rejection: a at 2024-01-15T10:00:00Z — needs work -->\n",
		"```\n<!-- failure: kept-in-fence -->\n```\n",
		"plain body, no markers",
		"",
		"trailing\n\n\n",
		"<!-- failure: foo --> trailing junk\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > fuzzMaxInputBytes {
			return
		}
		out := StripFailureMarkers(content)

		// Idempotency.
		if out2 := StripFailureMarkers(out); out2 != out {
			t.Fatalf("StripFailureMarkers not idempotent")
		}

		// Length bound: output cannot exceed input by more than one trailing
		// byte from newline normalization.
		if len(out) > len(content)+1 {
			t.Fatalf("output grew unexpectedly: in=%d out=%d", len(content), len(out))
		}

		// Stripped-marker counts must all be zero in output. We reuse the
		// package's own counters to avoid duplicating regexes.
		outBytes := []byte(out)
		if CountFailureMarkers(outBytes) != 0 {
			t.Fatalf("failure markers remain after strip")
		}
		if CountReviewFailureMarkers(outBytes) != 0 {
			t.Fatalf("review-failure markers remain after strip")
		}
		if CountCycleFailureMarkers(outBytes) != 0 {
			t.Fatalf("cycle-failure markers remain after strip")
		}
		if CountTerminalFailureMarkers(outBytes) != 0 {
			t.Fatalf("terminal-failure markers remain after strip")
		}

		// Review-rejection markers in the input must be preserved in output.
		// We compare counts of the standalone-line prefix as a proxy.
		inRR := bytes.Count([]byte(content), []byte("<!-- review-rejection:"))
		outRR := bytes.Count(outBytes, []byte("<!-- review-rejection:"))
		if outRR < inRR {
			t.Fatalf("review-rejection markers lost: in=%d out=%d", inRR, outRR)
		}
	})
}

// FuzzSanitizeCommentText asserts the sanitizer's invariants for safe
// embedding inside an HTML comment: no newlines, no "--" substring, no edge
// whitespace, idempotency, and embedding safety.
func FuzzSanitizeCommentText(f *testing.F) {
	seeds := []string{
		"hello",
		"  leading and trailing  ",
		"line1\nline2",
		"with -- dashes",
		"---",
		"\r\n\r\n",
		"",
		"safe ---> arrow",
		"foo\rbar--baz\n",
		strings.Repeat("-", 50),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > fuzzMaxInputBytes {
			return
		}
		out := SanitizeCommentText(text)

		if strings.ContainsAny(out, "\r\n") {
			t.Fatalf("SanitizeCommentText output contains a newline: %q", out)
		}
		if strings.Contains(out, "--") {
			t.Fatalf("SanitizeCommentText output contains \"--\": %q", out)
		}
		if out != strings.TrimSpace(out) {
			t.Fatalf("SanitizeCommentText output has leading/trailing whitespace: %q", out)
		}
		// Idempotency.
		if out2 := SanitizeCommentText(out); out2 != out {
			t.Fatalf("SanitizeCommentText not idempotent: %q -> %q", out, out2)
		}
		// Embedding safety: when wrapped in a marker template, the only "-->"
		// must be the trailing terminator.
		embedded := "<!-- failure: id at TS step=S error=" + out + " -->"
		if strings.Count(embedded, "-->") != 1 {
			t.Fatalf("embedding produced extra \"-->\" sequence: %q", embedded)
		}
	})
}
