package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

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

func renderText(w io.Writer, result inspectResult) {
	fmt.Fprintf(w, "Task: %s\n", result.TaskID)
	if result.Title != "" && result.Title != result.TaskID {
		fmt.Fprintf(w, "Title: %s\n", result.Title)
	}
	fmt.Fprintf(w, "File: %s/%s\n", result.State, result.Filename)
	fmt.Fprintf(w, "State: %s\n", result.State)
	fmt.Fprintf(w, "Status: %s\n", result.Status)
	fmt.Fprintf(w, "Reason: %s\n", result.Reason)
	fmt.Fprintf(w, "Next step: %s\n", result.NextStep)

	if result.QueuePosition > 0 {
		fmt.Fprintf(w, "Queue position: %d of %d\n", result.QueuePosition, result.QueueTotal)
	}
	if result.Branch != "" {
		fmt.Fprintf(w, "Branch: %s\n", result.Branch)
	}
	if result.MaxRetries > 0 && (result.FailureCount > 0 || result.ReviewFailureCount > 0 || result.Status == "failed" || result.Status == "invalid") {
		fmt.Fprintf(w, "Max retries: %d\n", result.MaxRetries)
	}
	if result.ClaimedBy != "" {
		if result.ClaimedAt.IsZero() {
			fmt.Fprintf(w, "Claimed by: %s\n", result.ClaimedBy)
		} else {
			fmt.Fprintf(w, "Claimed by: %s at %s\n", result.ClaimedBy, result.ClaimedAt.UTC().Format(time.RFC3339))
		}
	}
	if result.ReviewFailureCount > 0 {
		fmt.Fprintf(w, "Review failures: %d\n", result.ReviewFailureCount)
	}
	if len(result.BlockingDependencies) > 0 {
		fmt.Fprintln(w, "Blocking dependencies:")
		for _, dep := range result.BlockingDependencies {
			if dep.Filename != "" {
				fmt.Fprintf(w, "- %s (%s/%s)\n", dep.ID, dep.State, dep.Filename)
			} else {
				fmt.Fprintf(w, "- %s (%s)\n", dep.ID, dep.State)
			}
		}
	}
	if result.BlockingTask != nil {
		fmt.Fprintf(w, "Blocking task: %s/%s\n", result.BlockingTask.State, result.BlockingTask.Filename)
	}
	if len(result.ConflictingAffects) > 0 {
		fmt.Fprintf(w, "Conflicting affects: %s\n", joinList(result.ConflictingAffects))
	}
	if result.FailureKind != "" {
		fmt.Fprintf(w, "Failure: %s", result.FailureKind)
		if result.MaxRetries > 0 {
			fmt.Fprintf(w, " (%d/%d)", result.FailureCount, result.MaxRetries)
		}
		fmt.Fprintln(w)
	}
	if result.LastFailureReason != "" {
		fmt.Fprintf(w, "Last failure: %s\n", result.LastFailureReason)
	}
	if result.LastCycleReason != "" {
		fmt.Fprintf(w, "Cycle failure: %s\n", result.LastCycleReason)
	}
	if result.LastTerminalReason != "" {
		fmt.Fprintf(w, "Terminal failure: %s\n", result.LastTerminalReason)
	}
	if result.ReviewRejectionReason != "" {
		fmt.Fprintf(w, "Review history: previously rejected: %s\n", result.ReviewRejectionReason)
	}
	if result.ParseError != "" {
		fmt.Fprintf(w, "Parse error: %s\n", result.ParseError)
	}
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
