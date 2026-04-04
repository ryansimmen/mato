package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"

	"mato/internal/timeutil"
	"mato/internal/ui"
)

// rfc3339Re matches RFC3339 timestamps in reason strings so we can annotate
// them with relative time in text output only.
var rfc3339Re = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)

type inspectJSON struct {
	TaskID                string               `json:"task_id"`
	Filename              string               `json:"filename"`
	Title                 string               `json:"title"`
	State                 string               `json:"state"`
	Status                string               `json:"status"`
	Reason                string               `json:"reason"`
	NextStep              string               `json:"next_step"`
	QueuePosition         int                  `json:"queue_position,omitempty"`
	QueueTotal            int                  `json:"queue_total,omitempty"`
	Branch                string               `json:"branch,omitempty"`
	ClaimedBy             string               `json:"claimed_by,omitempty"`
	ClaimedAt             *string              `json:"claimed_at,omitempty"`
	ReviewFailureCount    int                  `json:"review_failure_count,omitempty"`
	BlockingTask          *blockingTask        `json:"blocking_task,omitempty"`
	BlockingDependencies  []blockingDependency `json:"blocking_dependencies,omitempty"`
	ConflictingAffects    []string             `json:"conflicting_affects,omitempty"`
	FailureKind           string               `json:"failure_kind,omitempty"`
	FailureCount          int                  `json:"failure_count,omitempty"`
	MaxRetries            int                  `json:"max_retries,omitempty"`
	LastFailureReason     string               `json:"last_failure_reason,omitempty"`
	LastCycleReason       string               `json:"last_cycle_reason,omitempty"`
	LastTerminalReason    string               `json:"last_terminal_reason,omitempty"`
	ReviewRejectionReason string               `json:"review_rejection_reason,omitempty"`
	ParseError            string               `json:"parse_error,omitempty"`
}

var colors = ui.NewColorSet()

type textWriter struct {
	w   io.Writer
	err error
}

func (tw *textWriter) print(args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprint(tw.w, args...)
}

func (tw *textWriter) printf(format string, args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintf(tw.w, format, args...)
}

func (tw *textWriter) println(args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintln(tw.w, args...)
}

// colorStatus applies semantic color to a status string.
func colorStatus(status string) string {
	switch status {
	case "completed", "runnable":
		return colors.Green(status)
	case "failed", "invalid":
		return colors.Red(status)
	case "blocked", "deferred":
		return colors.Yellow(status)
	case "running":
		return colors.Cyan(status)
	case "ready_for_review", "ready_to_merge":
		return colors.Cyan(status)
	default:
		return status
	}
}

func renderText(w io.Writer, result inspectResult) error {
	tw := textWriter{w: w}
	tw.printf("Task: %s\n", colors.Bold(result.TaskID))
	if result.Title != "" && result.Title != result.TaskID {
		tw.printf("Title: %s\n", result.Title)
	}
	tw.printf("File: %s/%s\n", result.State, result.Filename)
	tw.printf("State: %s\n", result.State)
	tw.printf("Status: %s\n", colorStatus(result.Status))
	tw.printf("Reason: %s\n", annotateTimestamps(result.Reason))
	tw.printf("Next step: %s\n", result.NextStep)

	if result.QueuePosition > 0 {
		tw.printf("Queue position: %d of %d\n", result.QueuePosition, result.QueueTotal)
	}
	if result.Branch != "" {
		tw.printf("Branch: %s\n", colors.Dim(result.Branch))
	}
	if result.MaxRetries > 0 && (result.FailureCount > 0 || result.ReviewFailureCount > 0 || result.Status == "failed" || result.Status == "invalid") {
		tw.printf("Max retries: %d\n", result.MaxRetries)
	}
	if result.ClaimedBy != "" {
		if result.ClaimedAt.IsZero() {
			tw.printf("Claimed by: %s\n", colors.Cyan(result.ClaimedBy))
		} else {
			ts := result.ClaimedAt.UTC().Format(time.RFC3339)
			rel := timeutil.RelativeTime(result.ClaimedAt.UTC(), time.Now().UTC())
			if rel != "" {
				ts += "  " + rel
			}
			tw.printf("Claimed by: %s at %s\n", colors.Cyan(result.ClaimedBy), colors.Dim(ts))
		}
	}
	if result.ReviewFailureCount > 0 {
		tw.printf("Review failures: %s\n", colors.Red(result.ReviewFailureCount))
	}
	if len(result.BlockingDependencies) > 0 {
		tw.println("Blocking dependencies:")
		for _, dep := range result.BlockingDependencies {
			if dep.Filename != "" {
				tw.printf("- %s (%s/%s)\n", dep.ID, dep.State, dep.Filename)
			} else {
				tw.printf("- %s (%s)\n", dep.ID, dep.State)
			}
		}
	}
	if result.BlockingTask != nil {
		tw.printf("Blocking task: %s/%s\n", result.BlockingTask.State, result.BlockingTask.Filename)
	}
	if len(result.ConflictingAffects) > 0 {
		tw.printf("Conflicting affects: %s\n", colors.Yellow(joinList(result.ConflictingAffects)))
	}
	if result.FailureKind != "" {
		tw.printf("Failure: %s", colors.Red(result.FailureKind))
		if result.MaxRetries > 0 {
			tw.printf(" (%d/%d)", result.FailureCount, result.MaxRetries)
		}
		tw.println()
	}
	if result.LastFailureReason != "" {
		tw.printf("Last failure: %s\n", colors.Red(result.LastFailureReason))
	}
	if result.LastCycleReason != "" {
		tw.printf("Cycle failure: %s\n", colors.Red(result.LastCycleReason))
	}
	if result.LastTerminalReason != "" {
		tw.printf("Terminal failure: %s\n", colors.Red(result.LastTerminalReason))
	}
	if result.ReviewRejectionReason != "" {
		tw.printf("Review history: previously rejected: %s\n", colors.Yellow(result.ReviewRejectionReason))
	}
	if result.ParseError != "" {
		tw.printf("Parse error: %s\n", colors.Red(result.ParseError))
	}
	return tw.err
}

func renderJSON(w io.Writer, result inspectResult) error {
	out := inspectJSON{
		TaskID:                result.TaskID,
		Filename:              result.Filename,
		Title:                 result.Title,
		State:                 result.State,
		Status:                result.Status,
		Reason:                result.Reason,
		NextStep:              result.NextStep,
		QueuePosition:         result.QueuePosition,
		QueueTotal:            result.QueueTotal,
		Branch:                result.Branch,
		ClaimedBy:             result.ClaimedBy,
		ReviewFailureCount:    result.ReviewFailureCount,
		BlockingTask:          result.BlockingTask,
		BlockingDependencies:  result.BlockingDependencies,
		ConflictingAffects:    result.ConflictingAffects,
		FailureKind:           result.FailureKind,
		FailureCount:          result.FailureCount,
		MaxRetries:            result.MaxRetries,
		LastFailureReason:     result.LastFailureReason,
		LastCycleReason:       result.LastCycleReason,
		LastTerminalReason:    result.LastTerminalReason,
		ReviewRejectionReason: result.ReviewRejectionReason,
		ParseError:            result.ParseError,
	}
	if !result.ClaimedAt.IsZero() {
		claimedAt := result.ClaimedAt.UTC().Format(time.RFC3339)
		out.ClaimedAt = &claimedAt
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func joinList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	result := items[0]
	for i := 1; i < len(items); i++ {
		result += ", " + items[i]
	}
	return result
}

// annotateTimestamps appends relative time annotations to any RFC3339
// timestamps found in the string, for text-only display.
func annotateTimestamps(s string) string {
	now := time.Now().UTC()
	return rfc3339Re.ReplaceAllStringFunc(s, func(match string) string {
		ts, err := time.Parse(time.RFC3339, match)
		if err != nil {
			return match
		}
		rel := timeutil.RelativeTime(ts, now)
		if rel != "" {
			return match + " " + rel
		}
		return match
	})
}
