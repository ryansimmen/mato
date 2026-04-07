package status

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/pause"
	"mato/internal/ui"
)

// StatusJSON is the top-level JSON output for the status command.
type StatusJSON struct {
	Counts          map[string]int    `json:"counts"`
	MergeQueue      string            `json:"merge_queue"`
	Paused          PausedJSON        `json:"paused"`
	RunnableBacklog []TaskSummaryJSON `json:"runnable_backlog"`
	ActiveAgents    []AgentJSON       `json:"active_agents"`
	InProgress      []TaskSummaryJSON `json:"in_progress"`
	Waiting         []WaitingTaskJSON `json:"waiting"`
	ReadyReview     []TaskSummaryJSON `json:"ready_for_review"`
	ReadyMerge      []TaskSummaryJSON `json:"ready_to_merge"`
	Failed          []FailedTaskJSON  `json:"failed"`
	Completions     []CompletionJSON  `json:"recent_completions"`
	Messages        []MessageJSON     `json:"recent_messages"`
	Warnings        []string          `json:"warnings,omitempty"`
}

// PausedJSON holds the machine-readable pause state.
type PausedJSON struct {
	Active bool   `json:"active"`
	Since  string `json:"since,omitempty"`
}

// AgentJSON represents an active agent in JSON output.
type AgentJSON struct {
	ID     string `json:"id"`
	PID    int    `json:"pid"`
	Task   string `json:"task,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// TaskSummaryJSON represents a task in JSON output.
type TaskSummaryJSON struct {
	Name      string `json:"name"`
	Title     string `json:"title,omitempty"`
	Priority  int    `json:"priority"`
	Branch    string `json:"branch,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
}

// WaitingTaskJSON represents a dependency-blocked task in JSON output.
type WaitingTaskJSON struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Priority     int              `json:"priority"`
	State        string           `json:"state,omitempty"`
	Dependencies []DependencyJSON `json:"dependencies"`
}

// DependencyJSON represents a single dependency and its resolution status.
type DependencyJSON struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// FailedTaskJSON represents a failed task in JSON output.
type FailedTaskJSON struct {
	Name           string `json:"name"`
	Title          string `json:"title,omitempty"`
	FailureKind    string `json:"failure_kind"`
	FailCount      int    `json:"fail_count"`
	MaxRetries     int    `json:"max_retries"`
	LastReason     string `json:"last_reason,omitempty"`
	TerminalReason string `json:"terminal_reason,omitempty"`
	CycleReason    string `json:"cycle_reason,omitempty"`
}

// CompletionJSON represents a recently completed task in JSON output.
type CompletionJSON struct {
	TaskFile     string    `json:"task_file"`
	Title        string    `json:"title,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	CommitSHA    string    `json:"commit_sha,omitempty"`
	FilesChanged []string  `json:"files_changed,omitempty"`
	MergedAt     time.Time `json:"merged_at"`
}

// MessageJSON represents a recent event message in JSON output.
type MessageJSON struct {
	ID     string    `json:"id"`
	From   string    `json:"from"`
	Type   string    `json:"type"`
	Task   string    `json:"task,omitempty"`
	Body   string    `json:"body,omitempty"`
	SentAt time.Time `json:"sent_at"`
}

// ShowJSON writes the status dashboard as JSON to os.Stdout.
func ShowJSON(w io.Writer, repoRoot string) error {
	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return err
	}
	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := ui.RequireTasksDir(tasksDir); err != nil {
		return err
	}

	data, err := gatherStatus(tasksDir)
	if err != nil {
		return fmt.Errorf("gather status: %w", err)
	}

	out := statusDataToJSON(data)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// statusDataToJSON converts the gathered statusData into a StatusJSON value.
func statusDataToJSON(data statusData) StatusJSON {
	out := StatusJSON{
		Counts: map[string]int{
			"backlog":        data.queueCounts[dirs.Backlog],
			"runnable":       data.runnable,
			"deferred":       len(data.deferredDetail),
			"waiting":        data.queueCounts[dirs.Waiting],
			"blocked":        len(data.waitingTasks),
			"in_progress":    data.queueCounts[dirs.InProgress],
			"ready_review":   data.queueCounts[dirs.ReadyReview],
			"ready_to_merge": data.queueCounts[dirs.ReadyMerge],
			"completed":      data.queueCounts[dirs.Completed],
			"failed":         data.queueCounts[dirs.Failed],
		},
		MergeQueue: data.mergeQueueState(),
	}
	if data.pauseState.Active {
		out.Paused.Active = true
		if data.pauseState.ProblemKind == pause.ProblemNone {
			out.Paused.Since = data.pauseState.Since.Format(time.RFC3339)
		}
	}

	// Runnable backlog in priority order.
	out.RunnableBacklog = make([]TaskSummaryJSON, 0, len(data.runnableBacklog))
	for _, task := range data.runnableBacklog {
		out.RunnableBacklog = append(out.RunnableBacklog, TaskSummaryJSON{
			Name:     task.name,
			Title:    task.title,
			Priority: task.priority,
		})
	}

	// Active agents.
	out.ActiveAgents = make([]AgentJSON, 0, len(data.agents))
	for _, a := range data.agents {
		aj := AgentJSON{ID: a.displayName(), PID: a.PID}
		if p, ok := data.presenceMap[a.ID]; ok {
			aj.Task = p.Task
			aj.Branch = p.Branch
		}
		out.ActiveAgents = append(out.ActiveAgents, aj)
	}

	// In-progress tasks.
	out.InProgress = make([]TaskSummaryJSON, 0, len(data.inProgressTasks))
	for _, task := range data.inProgressTasks {
		ts := TaskSummaryJSON{
			Name:      task.name,
			Title:     task.title,
			Priority:  task.priority,
			ClaimedBy: task.claimedBy,
			Branch:    task.branch,
		}
		out.InProgress = append(out.InProgress, ts)
	}

	// Waiting (dependency-blocked) tasks — converted from the shared model
	// populated during gatherStatus, avoiding re-derivation.
	out.Waiting = make([]WaitingTaskJSON, 0, len(data.waitingTasks))
	for _, wt := range data.waitingTasks {
		deps := make([]DependencyJSON, 0, len(wt.Dependencies))
		for _, d := range wt.Dependencies {
			deps = append(deps, DependencyJSON{ID: d.ID, Status: d.Status})
		}
		out.Waiting = append(out.Waiting, WaitingTaskJSON{
			Name:         wt.Name,
			Title:        wt.Title,
			Priority:     wt.Priority,
			State:        wt.State,
			Dependencies: deps,
		})
	}

	// Ready for review.
	out.ReadyReview = make([]TaskSummaryJSON, 0, len(data.readyForReview))
	for _, task := range data.readyForReview {
		ts := TaskSummaryJSON{
			Name:     task.name,
			Title:    task.title,
			Priority: task.priority,
			Branch:   task.branch,
		}
		out.ReadyReview = append(out.ReadyReview, ts)
	}

	// Ready to merge.
	out.ReadyMerge = make([]TaskSummaryJSON, 0, len(data.readyToMerge))
	for _, task := range data.readyToMerge {
		out.ReadyMerge = append(out.ReadyMerge, TaskSummaryJSON{
			Name:     task.name,
			Title:    task.title,
			Priority: task.priority,
		})
	}

	// Failed tasks.
	out.Failed = make([]FailedTaskJSON, 0, len(data.failedTasks))
	for _, task := range data.failedTasks {
		ft := FailedTaskJSON{
			Name:           task.name,
			Title:          task.title,
			FailCount:      task.failureCount,
			MaxRetries:     task.maxRetries,
			LastReason:     task.lastFailureReason,
			TerminalReason: task.lastTerminalFailureReason,
			CycleReason:    task.lastCycleFailureReason,
		}
		switch {
		case task.cancelled:
			ft.FailureKind = "cancelled"
		case task.lastTerminalFailureReason != "":
			ft.FailureKind = "terminal"
		case task.lastCycleFailureReason != "":
			ft.FailureKind = "cycle"
		default:
			ft.FailureKind = "retry"
		}
		out.Failed = append(out.Failed, ft)
	}

	// Recent completions.
	completions := data.completions
	if len(completions) > 5 {
		completions = completions[:5]
	}
	out.Completions = make([]CompletionJSON, 0, len(completions))
	for _, comp := range completions {
		out.Completions = append(out.Completions, CompletionJSON{
			TaskFile:     comp.TaskFile,
			Title:        comp.Title,
			Branch:       comp.Branch,
			CommitSHA:    comp.CommitSHA,
			FilesChanged: comp.FilesChanged,
			MergedAt:     comp.MergedAt,
		})
	}

	// Recent messages.
	out.Messages = make([]MessageJSON, 0, len(data.recentMessages))
	for _, msg := range data.recentMessages {
		out.Messages = append(out.Messages, MessageJSON{
			ID:     msg.ID,
			From:   msg.From,
			Type:   msg.Type,
			Task:   msg.Task,
			Body:   msg.Body,
			SentAt: msg.SentAt,
		})
	}

	// Warnings.
	out.Warnings = data.warnings

	return out
}
