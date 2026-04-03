// Package history renders durable task outcome history for the mato log command.
package history

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/taskfile"
	"mato/internal/timeutil"
	"mato/internal/ui"
)

// Event is a durable task outcome shown by mato log.
type Event struct {
	Timestamp    time.Time `json:"timestamp"`
	Type         string    `json:"type"`
	TaskFile     string    `json:"task_file"`
	Title        string    `json:"title,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	CommitSHA    string    `json:"commit_sha,omitempty"`
	FilesChanged []string  `json:"files_changed,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	AgentID      string    `json:"agent_id,omitempty"`
}

type sourceStatus string

const (
	sourceAbsent sourceStatus = "absent"
	sourceRead   sourceStatus = "read"
	sourceFailed sourceStatus = "failed"
)

// Show writes durable task history to stdout.
func Show(repo string, limit int, format string) error {
	return ShowTo(os.Stdout, repo, limit, format)
}

// ShowTo writes durable task history to w.
func ShowTo(w io.Writer, repo string, limit int, format string) error {
	if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
		return err
	}

	repoRoot, err := git.ResolveRepoRoot(repo)
	if err != nil {
		return err
	}
	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := ui.RequireTasksDir(tasksDir); err != nil {
		return err
	}

	events, err := collectEvents(tasksDir)
	if err != nil {
		return fmt.Errorf("collect history events: %w", err)
	}
	sortEvents(events)
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}

	if format == "json" {
		return renderJSON(w, events)
	}
	renderText(w, events)
	return nil
}

func collectEvents(tasksDir string) ([]Event, error) {
	completionEvents, completionStatus, completionErr := collectCompletionEvents(tasksDir)
	taskEvents, taskStatus, taskErr := collectTaskEvents(tasksDir)

	events := append(completionEvents, taskEvents...)

	if completionStatus != sourceRead && taskStatus != sourceRead {
		if completionStatus == sourceFailed && completionErr != nil {
			return nil, completionErr
		}
		if taskStatus == sourceFailed && taskErr != nil {
			return nil, taskErr
		}
	}

	// Surface partial failures: one source failed but the other succeeded.
	if completionStatus == sourceFailed && taskStatus == sourceRead && completionErr != nil {
		ui.Warnf("warning: %v\n", completionErr)
	}
	if taskStatus == sourceFailed && completionStatus == sourceRead && taskErr != nil {
		ui.Warnf("warning: %v\n", taskErr)
	}

	return events, nil
}

func collectCompletionEvents(tasksDir string) ([]Event, sourceStatus, error) {
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	entries, err := os.ReadDir(completionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, sourceAbsent, nil
		}
		return nil, sourceFailed, fmt.Errorf("read completions dir: %w", err)
	}

	events := make([]Event, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(completionsDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			ui.Warnf("warning: could not read completion detail %s: %v\n", entry.Name(), err)
			continue
		}

		var detail messaging.CompletionDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			ui.Warnf("warning: could not parse completion detail %s: %v\n", entry.Name(), err)
			continue
		}
		if err := validateCompletionDetail(detail); err != nil {
			ui.Warnf("warning: could not parse completion detail %s: %v\n", entry.Name(), err)
			continue
		}

		taskName := detail.TaskFile
		if taskName == "" {
			taskName = detail.TaskID
		}
		events = append(events, Event{
			Timestamp:    detail.MergedAt,
			Type:         "MERGED",
			TaskFile:     taskName,
			Title:        detail.Title,
			Branch:       detail.Branch,
			CommitSHA:    detail.CommitSHA,
			FilesChanged: detail.FilesChanged,
		})
	}

	return events, sourceRead, nil
}

func validateCompletionDetail(detail messaging.CompletionDetail) error {
	if detail.MergedAt.IsZero() {
		return fmt.Errorf("missing merged_at")
	}
	if strings.TrimSpace(detail.TaskID) == "" && strings.TrimSpace(detail.TaskFile) == "" {
		return fmt.Errorf("missing task_id and task_file")
	}
	return nil
}

func collectTaskEvents(tasksDir string) ([]Event, sourceStatus, error) {
	var (
		events    []Event
		readAny   bool
		failedErr error
	)

	for _, dir := range queue.AllDirs {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := queue.ListTaskFiles(dirPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			ui.Warnf("warning: could not read queue directory %s: %v\n", dir, err)
			if failedErr == nil {
				failedErr = fmt.Errorf("read queue directory %s: %w", dir, err)
			}
			continue
		}
		readAny = true

		for _, name := range names {
			path := filepath.Join(dirPath, name)
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				ui.Warnf("warning: could not read task file %s/%s: %v\n", dir, name, err)
				continue
			}

			title := extractTitle(name, path, data)
			for _, record := range taskfile.ParseFailureMarkers(data) {
				events = append(events, Event{
					Timestamp: record.Timestamp,
					Type:      "FAILED",
					TaskFile:  name,
					Title:     title,
					Reason:    record.Reason,
					AgentID:   record.AgentID,
				})
			}
			for _, record := range taskfile.ParseReviewRejectionMarkers(data) {
				events = append(events, Event{
					Timestamp: record.Timestamp,
					Type:      "REJECTED",
					TaskFile:  name,
					Title:     title,
					Reason:    record.Reason,
					AgentID:   record.AgentID,
				})
			}
		}
	}

	if readAny {
		return events, sourceRead, nil
	}
	if failedErr != nil {
		return nil, sourceFailed, failedErr
	}
	return nil, sourceAbsent, nil
}

func extractTitle(name, path string, data []byte) string {
	_, body, err := frontmatter.ParseTaskData(data, path)
	if err != nil {
		return frontmatter.TaskFileStem(name)
	}
	return frontmatter.ExtractTitle(name, body)
}

func sortEvents(events []Event) {
	sort.Slice(events, func(i, j int) bool {
		if !events[i].Timestamp.Equal(events[j].Timestamp) {
			return events[i].Timestamp.After(events[j].Timestamp)
		}
		if events[i].TaskFile != events[j].TaskFile {
			return events[i].TaskFile < events[j].TaskFile
		}
		return events[i].Type < events[j].Type
	})
}

var colors = ui.NewColorSet()

// colorEventType applies semantic color to a pre-padded event type
// string. Padding must happen before coloring so ANSI escape sequences
// do not count toward the visible column width.
func colorEventType(padded string) string {
	trimmed := strings.TrimRight(padded, " ")
	switch trimmed {
	case "MERGED":
		return colors.Green(trimmed) + padded[len(trimmed):]
	case "FAILED":
		return colors.Red(trimmed) + padded[len(trimmed):]
	case "REJECTED":
		return colors.Yellow(trimmed) + padded[len(trimmed):]
	default:
		return padded
	}
}

// minTruncWidth is the smallest budget allowed when clamping
// width-based truncation on very narrow terminals.
const minTruncWidth = 6

func renderText(w io.Writer, events []Event) {
	if len(events) == 0 {
		fmt.Fprintln(w, "(no history)")
		return
	}

	termWidth := ui.TermWidth()

	taskWidth := len("task")
	for _, event := range events {
		if len(event.TaskFile) > taskWidth {
			taskWidth = len(event.TaskFile)
		}
	}

	if termWidth > 0 {
		// Fixed columns: timestamp (20) + "  " + type (8) + "  " + "  " (detail sep) = 34
		const fixedCols = 34
		maxTaskWidth := termWidth - fixedCols - 10
		if maxTaskWidth < 10 {
			maxTaskWidth = 10
		}
		if taskWidth > maxTaskWidth {
			taskWidth = maxTaskWidth
		}
	}

	now := time.Now().UTC()
	for _, event := range events {
		padded := fmt.Sprintf("%-8s", event.Type)
		ts := event.Timestamp.UTC().Format(time.RFC3339)
		rel := timeutil.RelativeTime(event.Timestamp.UTC(), now)
		if rel != "" {
			ts += "  " + rel
		}

		taskName := event.TaskFile
		if termWidth > 0 && len(taskName) > taskWidth {
			taskName = ui.Truncate(taskName, taskWidth)
		}
		fmt.Fprintf(w, "%s  %s  %-*s", colors.Dim(ts), colorEventType(padded), taskWidth, taskName)

		detail := textDetail(event)
		if detail != "" {
			if termWidth > 0 {
				prefixWidth := len(ts) + 2 + 8 + 2 + taskWidth + 2
				maxDetail := termWidth - prefixWidth
				if maxDetail < minTruncWidth {
					maxDetail = minTruncWidth
				}
				detail = ui.Truncate(detail, maxDetail)
			}
			fmt.Fprintf(w, "  %s", detail)
		}
		fmt.Fprintln(w)
	}
}

func renderJSON(w io.Writer, events []Event) error {
	if events == nil {
		events = []Event{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(events)
}

func textDetail(event Event) string {
	switch event.Type {
	case "MERGED":
		parts := make([]string, 0, 2)
		if event.CommitSHA != "" {
			parts = append(parts, shortSHA(event.CommitSHA))
		}
		if event.Branch != "" {
			parts = append(parts, event.Branch)
		}
		return strings.Join(parts, "  ")
	case "FAILED", "REJECTED":
		return event.Reason
	default:
		return ""
	}
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
