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

var warningWriter io.Writer = os.Stderr

// Show writes durable task history to stdout.
func Show(repo string, limit int, format string) error {
	return ShowTo(os.Stdout, repo, limit, format)
}

// ShowTo writes durable task history to w.
func ShowTo(w io.Writer, repo string, limit int, format string) error {
	if format != "text" && format != "json" {
		return fmt.Errorf("unsupported format %q", format)
	}

	repoRoot, err := git.ResolveRepoRoot(repo)
	if err != nil {
		return err
	}
	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := requireTasksDir(tasksDir); err != nil {
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

func requireTasksDir(tasksDir string) error {
	info, err := os.Stat(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(".mato/ directory not found - run 'mato init' first")
		}
		return fmt.Errorf("stat %s: %w", tasksDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", tasksDir)
	}
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
		warnf("warning: %v\n", completionErr)
	}
	if taskStatus == sourceFailed && completionStatus == sourceRead && taskErr != nil {
		warnf("warning: %v\n", taskErr)
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
			warnf("warning: could not read completion detail %s: %v\n", entry.Name(), err)
			continue
		}

		var detail messaging.CompletionDetail
		if err := json.Unmarshal(data, &detail); err != nil {
			warnf("warning: could not parse completion detail %s: %v\n", entry.Name(), err)
			continue
		}
		if err := validateCompletionDetail(detail); err != nil {
			warnf("warning: could not parse completion detail %s: %v\n", entry.Name(), err)
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
			warnf("warning: could not read queue directory %s: %v\n", dir, err)
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
				warnf("warning: could not read task file %s/%s: %v\n", dir, name, err)
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

func renderText(w io.Writer, events []Event) {
	if len(events) == 0 {
		fmt.Fprintln(w, "(no history)")
		return
	}

	taskWidth := len("task")
	for _, event := range events {
		if len(event.TaskFile) > taskWidth {
			taskWidth = len(event.TaskFile)
		}
	}

	for _, event := range events {
		fmt.Fprintf(w, "%s  %-8s  %-*s", event.Timestamp.UTC().Format(time.RFC3339), event.Type, taskWidth, event.TaskFile)
		detail := textDetail(event)
		if detail != "" {
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

func warnf(format string, args ...any) {
	if warningWriter == nil {
		return
	}
	fmt.Fprintf(warningWriter, format, args...)
}
