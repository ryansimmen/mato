package taskfile

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Compiled regexes for all HTML comment metadata markers in task files.
// These are the canonical definitions — no other package should define its own.
var (
	branchCommentRe    = regexp.MustCompile(`<!-- branch:\s*(\S+)\s*-->`)
	claimedByRe        = regexp.MustCompile(`<!-- claimed-by:\s*(\S+)`)
	claimedAtRe        = regexp.MustCompile(`claimed-at:\s*(\S+)`)
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

// AppendFailureRecord appends a <!-- failure: ... --> record to the task file
// at path using O_APPEND for safe concurrent writes.
func AppendFailureRecord(path, agentID, step, errMsg string) error {
	step = SanitizeCommentText(step)
	errMsg = SanitizeCommentText(errMsg)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open task file to append failure record: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "\n<!-- failure: %s at %s step=%s error=%s -->\n",
		agentID, time.Now().UTC().Format(time.RFC3339), step, errMsg)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write failure record: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close after failure record: %w", closeErr)
	}
	return nil
}

// cycleFailurePrefix is the marker prefix for cycle-failure records.
var cycleFailurePrefix = "<!-- cycle-failure:"

// AppendCycleFailureRecord appends a <!-- cycle-failure: ... --> record to the
// task file at path using O_APPEND. Cycle failures are structural (circular
// dependency) and do not consume normal retry budget.
func AppendCycleFailureRecord(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open task file to append cycle-failure: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "\n<!-- cycle-failure: mato at %s — circular dependency -->\n",
		time.Now().UTC().Format(time.RFC3339))
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write cycle-failure record: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close after cycle-failure record: %w", closeErr)
	}
	return nil
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open task file to append review-failure: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "\n<!-- review-failure: %s at %s step=REVIEW error=%s -->\n",
		agentID, time.Now().UTC().Format(time.RFC3339), reason)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write review-failure record: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close after review-failure record: %w", closeErr)
	}
	return nil
}
