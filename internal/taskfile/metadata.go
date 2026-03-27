// Package taskfile provides task file metadata parsing and mutation utilities.
// It reads YAML frontmatter from markdown task files and supports in-place
// updates to fields such as priority, dependencies, and affected paths.
package taskfile

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"mato/internal/atomicwrite"
)

// Compiled regexes for all HTML comment metadata markers in task files.
// These are the canonical definitions — no other package should define its own.
var (
	branchCommentRe    = regexp.MustCompile(`<!-- branch:\s*(\S+)\s*-->`)
	claimedByRe        = regexp.MustCompile(`<!-- claimed-by:\s*(\S+).*-->`)
	claimedAtRe        = regexp.MustCompile(`<!-- claimed-by:\s*\S+\s+claimed-at:\s*(\S+)\s*-->`)
	reviewRejectionStr = "<!-- review-rejection:"
	failurePrefix      = "<!-- failure:"
	reviewFailureStr   = "<!-- review-failure:"
)

// ParseBranchComment extracts the branch name from a <!-- branch: ... -->
// comment in the given data. Returns the branch name and true if found.
func ParseBranchComment(data []byte) (string, bool) {
	m := branchCommentRe.FindSubmatch(data)
	if len(m) < 2 {
		return "", false
	}
	return string(m[1]), true
}

// ParseClaimedBy extracts the agent ID from a <!-- claimed-by: ... -->
// comment in the given data. Returns the agent ID and true if found.
func ParseClaimedBy(data []byte) (string, bool) {
	m := claimedByRe.FindSubmatch(data)
	if len(m) < 2 {
		return "", false
	}
	return string(m[1]), true
}

// ParseClaimedAt extracts the claimed-at timestamp from a
// <!-- claimed-by: ... claimed-at: ... --> comment. Returns the parsed time
// and true if a valid RFC3339 timestamp is found.
func ParseClaimedAt(data []byte) (time.Time, bool) {
	m := claimedAtRe.FindSubmatch(data)
	if len(m) < 2 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, string(m[1]))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CountFailureMarkers counts <!-- failure: ... --> lines in data, excluding
// lines that start with <!-- review-failure: ... -->.
func CountFailureMarkers(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, failurePrefix) && !strings.HasPrefix(trimmed, reviewFailureStr) {
			count++
		}
	}
	return count
}

// CountReviewFailureMarkers counts <!-- review-failure: ... --> lines in data.
func CountReviewFailureMarkers(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), reviewFailureStr) {
			count++
		}
	}
	return count
}

// ExtractFailureLines returns all <!-- failure: ... --> lines joined by
// newlines. Returns "" if none are found.
func ExtractFailureLines(data []byte) string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), failurePrefix) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return strings.Join(lines, "\n")
}

// ExtractReviewRejections returns all <!-- review-rejection: ... --> lines
// joined by newlines. Returns "" if none are found.
func ExtractReviewRejections(data []byte) string {
	var rejections []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, reviewRejectionStr) {
			rejections = append(rejections, strings.TrimSpace(line))
		}
	}
	return strings.Join(rejections, "\n")
}

// ContainsFailureFrom reports whether data contains a failure record written
// by the given agent (matching the pattern "<!-- failure: <agentID> ").
func ContainsFailureFrom(data []byte, agentID string) bool {
	return strings.Contains(string(data), "<!-- failure: "+agentID+" ")
}

// LastFailureReason extracts the reason from the last <!-- failure: ... -->
// comment in data. Returns "" if no failure comments are found.
func LastFailureReason(data []byte) string {
	last := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, failurePrefix) || strings.HasPrefix(trimmed, reviewFailureStr) {
			continue
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
	}
	return last
}

// LastReviewRejectionReason extracts the reason from the last
// <!-- review-rejection: ... --> comment in data. Returns "" if none found.
func LastReviewRejectionReason(data []byte) string {
	last := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, reviewRejectionStr) {
			continue
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
	}
	return last
}

func failureReasonFromLine(line string) string {
	if idx := strings.Index(line, "—"); idx >= 0 {
		return trimCommentSuffix(line[idx+len("—"):])
	}
	if idx := strings.Index(line, "error="); idx >= 0 {
		reason := trimCommentSuffix(line[idx+len("error="):])
		if metaIdx := strings.Index(reason, " files_changed="); metaIdx >= 0 {
			reason = reason[:metaIdx]
		}
		return strings.TrimSpace(reason)
	}
	return ""
}

func trimCommentSuffix(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "-->")
	return strings.TrimSpace(s)
}

// SanitizeCommentText normalizes text before embedding it in an HTML comment.
func SanitizeCommentText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "--", "—")
	return strings.TrimSpace(text)
}

// WriteBranchComment writes a <!-- branch: ... --> comment to w.
func WriteBranchComment(w io.Writer, branch string) error {
	_, err := fmt.Fprintf(w, "<!-- branch: %s -->", branch)
	return err
}

// WriteClaimedByComment writes a <!-- claimed-by: ... --> comment to w.
func WriteClaimedByComment(w io.Writer, agentID, claimedAt string) error {
	_, err := fmt.Fprintf(w, "<!-- claimed-by: %s  claimed-at: %s -->", agentID, claimedAt)
	return err
}

// failureMarkerPrefixes lists all failure-related HTML comment prefixes that
// should be stripped when retrying a task. This is the single source of truth
// for retry marker cleanup — no other package should define its own list.
var failureMarkerPrefixes = []string{
	failurePrefix,
	reviewFailureStr,
	cycleFailurePrefix,
	terminalFailurePrefix,
}

// StripFailureMarkers removes all failure-related HTML comment lines from
// content (failure, review-failure, cycle-failure, terminal-failure) while
// preserving non-failure comments such as review-rejection feedback.
// It collapses runs of 3+ consecutive newlines down to 2 and normalizes the
// trailing newline.
func StripFailureMarkers(content string) string {
	lines := strings.Split(content, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		strip := false
		for _, prefix := range failureMarkerPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				strip = true
				break
			}
		}
		if !strip {
			kept = append(kept, line)
		}
	}
	result := strings.Join(kept, "\n")
	// Collapse runs of 3+ newlines down to 2.
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(result, "\n") + "\n"
}

// AppendFailureRecord appends a <!-- failure: ... --> record to the task file
// at path using O_APPEND for safe concurrent writes.
func AppendFailureRecord(path, agentID, step, errMsg string) error {
	step = SanitizeCommentText(step)
	errMsg = SanitizeCommentText(errMsg)
	content := fmt.Sprintf("\n<!-- failure: %s at %s step=%s error=%s -->\n",
		agentID, time.Now().UTC().Format(time.RFC3339), step, errMsg)
	return atomicwrite.AppendToFile(path, content)
}

// cycleFailurePrefix is the marker prefix for cycle-failure records.
var cycleFailurePrefix = "<!-- cycle-failure:"

// AppendCycleFailureRecord appends a <!-- cycle-failure: ... --> record to the
// task file at path using O_APPEND. Cycle failures are structural (circular
// dependency) and do not consume normal retry budget.
func AppendCycleFailureRecord(path string) error {
	content := fmt.Sprintf("\n<!-- cycle-failure: mato at %s — circular dependency -->\n",
		time.Now().UTC().Format(time.RFC3339))
	return atomicwrite.AppendToFile(path, content)
}

// ContainsCycleFailure reports whether data contains a <!-- cycle-failure: ... -->
// marker. Used for idempotency checks before appending a new record.
func ContainsCycleFailure(data []byte) bool {
	return strings.Contains(string(data), cycleFailurePrefix)
}

// CountCycleFailureMarkers counts <!-- cycle-failure: ... --> lines in data.
func CountCycleFailureMarkers(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), cycleFailurePrefix) {
			count++
		}
	}
	return count
}

// LastCycleFailureReason extracts the reason from the last
// <!-- cycle-failure: ... --> comment in data. Returns "" if none found.
func LastCycleFailureReason(data []byte) string {
	last := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, cycleFailurePrefix) {
			continue
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
	}
	return last
}

// AppendReviewFailure appends a <!-- review-failure: ... --> record to the
// task file at path using O_APPEND. The task stays in its current directory
// for a future review attempt.
func AppendReviewFailure(path, agentID, reason string) error {
	reason = SanitizeCommentText(reason)
	content := fmt.Sprintf("\n<!-- review-failure: %s at %s step=REVIEW error=%s -->\n",
		agentID, time.Now().UTC().Format(time.RFC3339), reason)
	return atomicwrite.AppendToFile(path, content)
}

// terminalFailurePrefix is the marker prefix for terminal-failure records.
var terminalFailurePrefix = "<!-- terminal-failure:"

// AppendTerminalFailureRecord appends a <!-- terminal-failure: ... --> record
// to the task file at path using O_APPEND. Terminal failures are written by
// the host before automatically moving a task to failed/ (e.g. unparseable
// frontmatter, invalid glob syntax, review retry exhaustion). They do not
// consume the normal agent retry budget.
func AppendTerminalFailureRecord(path, reason string) error {
	reason = SanitizeCommentText(reason)
	content := fmt.Sprintf("\n<!-- terminal-failure: mato at %s — %s -->\n",
		time.Now().UTC().Format(time.RFC3339), reason)
	return atomicwrite.AppendToFile(path, content)
}

// ContainsTerminalFailure reports whether data contains a
// <!-- terminal-failure: ... --> marker.
func ContainsTerminalFailure(data []byte) bool {
	return strings.Contains(string(data), terminalFailurePrefix)
}

// CountTerminalFailureMarkers counts <!-- terminal-failure: ... --> lines in data.
func CountTerminalFailureMarkers(data []byte) int {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), terminalFailurePrefix) {
			count++
		}
	}
	return count
}

// LastTerminalFailureReason extracts the reason from the last
// <!-- terminal-failure: ... --> comment in data. Returns "" if none found.
func LastTerminalFailureReason(data []byte) string {
	last := ""
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, terminalFailurePrefix) {
			continue
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
	}
	return last
}
