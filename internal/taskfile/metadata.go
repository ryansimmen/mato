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

// MarkerRecord is a typed durable marker parsed from a task file.
type MarkerRecord struct {
	Timestamp time.Time
	AgentID   string
	Reason    string
}

func forEachTaskLine(content string, fn func(line, trimmed string, inFence bool) bool) {
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if fn(line, trimmed, inFence) {
			return
		}
		if isFenceLine(trimmed) {
			inFence = !inFence
		}
	}
}

func forEachMarkerLine(data []byte, fn func(trimmed string) bool) {
	forEachTaskLine(string(data), func(_ string, trimmed string, inFence bool) bool {
		if inFence || isFenceLine(trimmed) {
			return false
		}
		return fn(trimmed)
	})
}

func isFenceLine(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	if strings.HasPrefix(trimmed, "```") {
		return true
	}
	if strings.HasPrefix(trimmed, "~~~") {
		return true
	}
	return false
}

// Compiled regexes for all HTML comment metadata markers in task files.
// These are the canonical definitions — no other package should define its own.
var (
	branchCommentRe    = regexp.MustCompile(`<!-- branch:\s*(\S+)\s*-->`)
	claimedByRe        = regexp.MustCompile(`<!-- claimed-by:\s*(\S+).*-->`)
	claimedAtRe        = regexp.MustCompile(`<!-- claimed-by:\s*\S+\s+claimed-at:\s*(\S+)\s*-->`)
	mergedMarkerRe     = regexp.MustCompile(`<!-- merged:\s*merge-queue at\s+(\S+)\s*-->`)
	reviewRejectionStr = "<!-- review-rejection:"
	failurePrefix      = "<!-- failure:"
	reviewFailureStr   = "<!-- review-failure:"
	cancelledMarkerStr = "<!-- cancelled:"
)

func parseBranchCommentLine(trimmed string) (string, bool) {
	m := branchCommentRe.FindStringSubmatch(trimmed)
	if len(m) < 2 || m[0] != trimmed {
		return "", false
	}
	return m[1], true
}

// parseBranchComment extracts the branch name from a <!-- branch: ... -->
// comment in the given data. Returns the branch name and true if found.
func parseBranchComment(data []byte) (string, bool) {
	m := branchCommentRe.FindSubmatch(data)
	if len(m) < 2 {
		return "", false
	}
	return string(m[1]), true
}

// ParseBranchMarkerLine extracts the first branch marker that appears as a
// standalone line outside code fences. Marker-like text embedded in prose or
// code blocks is ignored.
func ParseBranchMarkerLine(data []byte) (string, bool) {
	var branch string
	var ok bool
	forEachMarkerLine(data, func(trimmed string) bool {
		branch, ok = parseBranchCommentLine(trimmed)
		return ok
	})
	return branch, ok
}

// HasMergedMarker reports whether the task contains a standalone
// <!-- merged: merge-queue at ... --> marker outside code fences.
func HasMergedMarker(data []byte) bool {
	found := false
	forEachMarkerLine(data, func(trimmed string) bool {
		m := mergedMarkerRe.FindStringSubmatch(trimmed)
		if len(m) >= 2 && m[0] == trimmed {
			found = true
			return true
		}
		return false
	})
	return found
}

// ReplaceBranchMarkerLine replaces the first standalone branch marker line
// outside code fences. It preserves leading and trailing horizontal whitespace
// on the replaced line.
func ReplaceBranchMarkerLine(data []byte, newBranch string) (result []byte, found bool, replaced bool) {
	comment := "<!-- branch: " + newBranch + " -->"
	var lines []string
	forEachTaskLine(string(data), func(line, trimmed string, inFence bool) bool {
		if !found && !inFence && !isFenceLine(trimmed) {
			branch, ok := parseBranchCommentLine(trimmed)
			if ok {
				found = true
				if branch != newBranch {
					replaced = true
					leadingLen := len(line) - len(strings.TrimLeft(line, " \t"))
					trailingLen := len(line) - len(strings.TrimRight(line, " \t"))
					prefix := line[:leadingLen]
					suffix := ""
					if trailingLen > 0 {
						suffix = line[len(line)-trailingLen:]
					}
					line = prefix + comment + suffix
				}
			}
		}
		lines = append(lines, line)
		return false
	})
	return []byte(strings.Join(lines, "\n")), found, replaced
}

// RemoveBranchMarkerLine removes the first standalone branch marker line
// outside code fences. It preserves all other file content.
func RemoveBranchMarkerLine(data []byte) (result []byte, found bool, removed bool) {
	var lines []string
	forEachTaskLine(string(data), func(line, trimmed string, inFence bool) bool {
		if !found && !inFence && !isFenceLine(trimmed) {
			if _, ok := parseBranchCommentLine(trimmed); ok {
				found = true
				removed = true
				return false
			}
		}
		lines = append(lines, line)
		return false
	})
	return []byte(strings.Join(lines, "\n")), found, removed
}

// ParseClaimedBy extracts the agent ID from a <!-- claimed-by: ... -->
// comment that appears as a standalone line. Marker-like text embedded in
// prose or code blocks is ignored.
func ParseClaimedBy(data []byte) (string, bool) {
	var claimedBy string
	var ok bool
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, "<!-- claimed-by:") {
			return false
		}
		m := claimedByRe.FindStringSubmatch(trimmed)
		if len(m) >= 2 {
			claimedBy = m[1]
			ok = true
			return true
		}
		return false
	})
	return claimedBy, ok
}

// ParseClaimedAt extracts the claimed-at timestamp from a
// <!-- claimed-by: ... claimed-at: ... --> comment that appears as a
// standalone line. Marker-like text embedded in prose is ignored.
func ParseClaimedAt(data []byte) (time.Time, bool) {
	var claimedAt time.Time
	var ok bool
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, "<!-- claimed-by:") {
			return false
		}
		m := claimedAtRe.FindStringSubmatch(trimmed)
		if len(m) >= 2 {
			t, err := time.Parse(time.RFC3339, m[1])
			if err != nil {
				return false
			}
			claimedAt = t
			ok = true
			return true
		}
		return false
	})
	return claimedAt, ok
}

// CountFailureMarkers counts <!-- failure: ... --> lines in data, excluding
// lines that start with <!-- review-failure: ... -->.
func CountFailureMarkers(data []byte) int {
	count := 0
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, failurePrefix) && !strings.HasPrefix(trimmed, reviewFailureStr) {
			count++
		}
		return false
	})
	return count
}

// CountReviewFailureMarkers counts <!-- review-failure: ... --> lines in data.
func CountReviewFailureMarkers(data []byte) int {
	count := 0
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, reviewFailureStr) {
			count++
		}
		return false
	})
	return count
}

// ExtractFailureLines returns all <!-- failure: ... --> lines joined by
// newlines. Returns "" if none are found.
func ExtractFailureLines(data []byte) string {
	var lines []string
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, failurePrefix) {
			lines = append(lines, trimmed)
		}
		return false
	})
	return strings.Join(lines, "\n")
}

// ExtractReviewRejections returns all <!-- review-rejection: ... --> lines
// joined by newlines. Only standalone marker lines are matched; marker-like
// text embedded in prose or code blocks is ignored.
func ExtractReviewRejections(data []byte) string {
	var rejections []string
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, reviewRejectionStr) {
			rejections = append(rejections, trimmed)
		}
		return false
	})
	return strings.Join(rejections, "\n")
}

// ParseFailureMarkers returns typed records for standalone
// <!-- failure: ... --> markers outside code fences. Malformed markers are
// skipped silently.
func ParseFailureMarkers(data []byte) []MarkerRecord {
	var records []MarkerRecord
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, failurePrefix) || strings.HasPrefix(trimmed, reviewFailureStr) {
			return false
		}
		record, ok := parseMarkerRecord(trimmed, failurePrefix)
		if ok {
			records = append(records, record)
		}
		return false
	})
	return records
}

// ParseReviewRejectionMarkers returns typed records for standalone
// <!-- review-rejection: ... --> markers outside code fences. Malformed
// markers are skipped silently.
func ParseReviewRejectionMarkers(data []byte) []MarkerRecord {
	var records []MarkerRecord
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, reviewRejectionStr) {
			return false
		}
		record, ok := parseMarkerRecord(trimmed, reviewRejectionStr)
		if ok {
			records = append(records, record)
		}
		return false
	})
	return records
}

// ContainsFailureFrom reports whether data contains a failure record written
// by the given agent as a standalone line starting with
// "<!-- failure: <agentID> ". Marker-like text in prose is ignored.
func ContainsFailureFrom(data []byte, agentID string) bool {
	target := "<!-- failure: " + agentID + " "
	found := false
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, target) {
			found = true
			return true
		}
		return false
	})
	return found
}

// LastFailureReason extracts the reason from the last <!-- failure: ... -->
// comment in data. Returns "" if no failure comments are found.
func LastFailureReason(data []byte) string {
	last := ""
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, failurePrefix) || strings.HasPrefix(trimmed, reviewFailureStr) {
			return false
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
		return false
	})
	return last
}

// LastReviewRejectionReason extracts the reason from the last
// <!-- review-rejection: ... --> comment in data. Returns "" if none found.
func LastReviewRejectionReason(data []byte) string {
	last := ""
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, reviewRejectionStr) {
			return false
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
		return false
	})
	return last
}

func parseMarkerRecord(line, prefix string) (MarkerRecord, bool) {
	if !strings.HasSuffix(line, "-->") {
		return MarkerRecord{}, false
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, prefix), "-->"))
	agentID, rest, ok := strings.Cut(body, " at ")
	if !ok {
		return MarkerRecord{}, false
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return MarkerRecord{}, false
	}
	timestampToken, _, ok := strings.Cut(strings.TrimSpace(rest), " ")
	if !ok {
		timestampToken = strings.TrimSpace(rest)
	}
	timestamp, err := time.Parse(time.RFC3339, timestampToken)
	if err != nil {
		return MarkerRecord{}, false
	}
	reason := failureReasonFromLine(line)
	if reason == "" {
		return MarkerRecord{}, false
	}
	return MarkerRecord{
		Timestamp: timestamp,
		AgentID:   agentID,
		Reason:    reason,
	}, true
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
	if idx := strings.Index(line, "reason="); idx >= 0 {
		return strings.TrimSpace(trimCommentSuffix(line[idx+len("reason="):]))
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

// StripRuntimeMarkers removes only the leading scheduler-managed metadata
// lines (claimed-by and branch markers) that the host prepends when claiming a
// task. It stops stripping as soon as it encounters any non-empty line that is
// not one of these markers, so marker-like text appearing in the task body
// (prose, examples, or code blocks) is never removed. Windows newlines are
// normalized and surrounding whitespace is trimmed. Returns nil for empty
// results.
func StripRuntimeMarkers(data []byte) []byte {
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	start := 0
	for start < len(lines) {
		trimmed := strings.TrimSpace(lines[start])
		if trimmed == "" {
			start++
			continue
		}
		if strings.HasPrefix(trimmed, "<!-- claimed-by:") || strings.HasPrefix(trimmed, "<!-- branch:") {
			start++
			continue
		}
		break
	}
	normalized := strings.TrimSpace(strings.Join(lines[start:], "\n"))
	if normalized == "" {
		return nil
	}
	return []byte(normalized)
}

// failureMarkerPrefixes lists all failure-related HTML comment prefixes that
// should be stripped when retrying a task. This is the single source of truth
// for retry marker cleanup — no other package should define its own list.
var failureMarkerPrefixes = []string{
	failurePrefix,
	reviewFailureStr,
	cancelledMarkerStr,
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
	forEachTaskLine(content, func(line, trimmed string, inFence bool) bool {
		strip := false
		if !inFence && !isFenceLine(trimmed) {
			for _, prefix := range failureMarkerPrefixes {
				if strings.HasPrefix(trimmed, prefix) {
					strip = true
					break
				}
			}
		}
		if !strip {
			kept = append(kept, line)
		}
		return false
	})
	if len(lines) > 0 && lines[len(lines)-1] == "" && (len(kept) == 0 || kept[len(kept)-1] != "") {
		kept = append(kept, "")
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

// AppendCancelledRecord appends a <!-- cancelled: ... --> record to the task
// file at path using O_APPEND. Cancel records are operator-written terminal
// markers that can later be removed by RetryTask via StripFailureMarkers.
func AppendCancelledRecord(path string) error {
	content := fmt.Sprintf("\n<!-- cancelled: operator at %s -->\n",
		time.Now().UTC().Format(time.RFC3339))
	return atomicwrite.AppendToFile(path, content)
}

// ContainsCancelledMarker reports whether data contains a <!-- cancelled: ... -->
// marker as a standalone line.
func ContainsCancelledMarker(data []byte) bool {
	found := false
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, cancelledMarkerStr) {
			found = true
			return true
		}
		return false
	})
	return found
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
// marker as a standalone line. Marker-like text in prose or code is ignored.
func ContainsCycleFailure(data []byte) bool {
	found := false
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, cycleFailurePrefix) {
			found = true
			return true
		}
		return false
	})
	return found
}

// CountCycleFailureMarkers counts <!-- cycle-failure: ... --> lines in data.
func CountCycleFailureMarkers(data []byte) int {
	count := 0
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, cycleFailurePrefix) {
			count++
		}
		return false
	})
	return count
}

// LastCycleFailureReason extracts the reason from the last
// <!-- cycle-failure: ... --> comment in data. Returns "" if none found.
func LastCycleFailureReason(data []byte) string {
	last := ""
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, cycleFailurePrefix) {
			return false
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
		return false
	})
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
// <!-- terminal-failure: ... --> marker as a standalone line. Marker-like
// text in prose or code is ignored.
func ContainsTerminalFailure(data []byte) bool {
	found := false
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, terminalFailurePrefix) {
			found = true
			return true
		}
		return false
	})
	return found
}

// CountTerminalFailureMarkers counts <!-- terminal-failure: ... --> lines in data.
func CountTerminalFailureMarkers(data []byte) int {
	count := 0
	forEachMarkerLine(data, func(trimmed string) bool {
		if strings.HasPrefix(trimmed, terminalFailurePrefix) {
			count++
		}
		return false
	})
	return count
}

// LastTerminalFailureReason extracts the reason from the last
// <!-- terminal-failure: ... --> comment in data. Returns "" if none found.
func LastTerminalFailureReason(data []byte) string {
	last := ""
	forEachMarkerLine(data, func(trimmed string) bool {
		if !strings.HasPrefix(trimmed, terminalFailurePrefix) {
			return false
		}
		reason := failureReasonFromLine(trimmed)
		if reason != "" {
			last = reason
		}
		return false
	})
	return last
}
