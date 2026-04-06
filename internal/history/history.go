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
	return renderText(w, events)
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

	for _, dir := range dirs.All {
		dirPath := filepath.Join(tasksDir, dir)
		names, err := taskfile.ListTaskFiles(dirPath)
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
			rejections := taskfile.ParseReviewRejectionMarkers(data)
			for _, record := range rejections {
				events = append(events, Event{
					Timestamp: record.Timestamp,
					Type:      "REJECTED",
					TaskFile:  name,
					Title:     title,
					Reason:    record.Reason,
					AgentID:   record.AgentID,
				})
			}
			// When no durable rejection markers exist in the task file,
			// fall back to the preserved verdict JSON file. This covers
			// the case where both marker write paths failed after a
			// successful move to backlog/.
			if len(rejections) == 0 {
				if vr, ok := taskfile.ReadVerdictRejection(tasksDir, name); ok {
					events = append(events, Event{
						Timestamp: vr.Timestamp,
						Type:      "REJECTED",
						TaskFile:  name,
						Title:     title,
						Reason:    vr.Reason,
					})
				}
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

func renderText(w io.Writer, events []Event) error {
	tw := ui.NewTextWriter(w)
	if len(events) == 0 {
		tw.Println("(no history)")
		return tw.Err()
	}

	termWidth := ui.TermWidth()

	taskWidth := len("task")
	for _, event := range events {
		if len(event.TaskFile) > taskWidth {
			taskWidth = len(event.TaskFile)
		}
	}

	if termWidth > 0 {
		// Fixed prefix: absolute timestamp (20) + "  " + type (8) + "  " = 32.
		const fixedPrefix = 32
		maxTaskWidth := termWidth - fixedPrefix
		if maxTaskWidth < 0 {
			maxTaskWidth = 0
		}
		if taskWidth > maxTaskWidth {
			taskWidth = maxTaskWidth
		}
	}

	now := time.Now().UTC()
	for _, event := range events {
		padded := fmt.Sprintf("%-8s", event.Type)
		absTS := event.Timestamp.UTC().Format(time.RFC3339)
		ts := absTS
		rel := timeutil.RelativeTime(event.Timestamp.UTC(), now)
		if rel != "" {
			ts = absTS + "  " + rel
		}

		taskName := event.TaskFile
		detail := textDetail(event)
		showDetail := detail != ""
		eventTaskWidth := taskWidth

		if termWidth > 0 {
			// Visible line: ts + "  " + type(8) + "  " + task [+ "  " + detail].
			prefixLen := len([]rune(ts)) + 12
			lineLen := prefixLen + eventTaskWidth
			if showDetail {
				lineLen += 2 + len([]rune(detail))
			}

			// Step 1: drop detail if it causes overflow.
			if lineLen > termWidth && showDetail {
				showDetail = false
				lineLen = prefixLen + eventTaskWidth
			}

			// Step 2: drop relative time if still overflowing.
			if lineLen > termWidth && rel != "" {
				ts = absTS
				prefixLen = len([]rune(ts)) + 12
				lineLen = prefixLen + eventTaskWidth
			}

			// Step 3: shrink task column to fit remaining space.
			if lineLen > termWidth {
				eventTaskWidth = termWidth - prefixLen
				if eventTaskWidth < 0 {
					eventTaskWidth = 0
				}
			}

			if eventTaskWidth > 0 && len(taskName) > eventTaskWidth {
				taskName = ui.Truncate(taskName, eventTaskWidth)
			}

			// Step 4: if even the prefix overflows, truncate the
			// timestamp so the line fits within the terminal width.
			if eventTaskWidth == 0 {
				barePrefix := len([]rune(ts)) + 2 + len([]rune(event.Type))
				if barePrefix > termWidth {
					tsBudget := termWidth - 2 - len([]rune(event.Type))
					if tsBudget > 0 {
						ts = ui.Truncate(ts, tsBudget)
					} else {
						// Not even room for type; truncate ts to fill.
						ts = ui.Truncate(ts, termWidth)
					}
				}
			}
		}

		if eventTaskWidth > 0 {
			tw.Printf("%s  %s  %-*s", colors.Dim(ts), colorEventType(padded), eventTaskWidth, taskName)
		} else if termWidth > 0 && len([]rune(ts))+2+len([]rune(event.Type)) > termWidth {
			// Timestamp was truncated so tightly that adding "  TYPE"
			// would still overflow; print only the truncated timestamp.
			tw.Print(colors.Dim(ts))
		} else {
			// No room for task column; omit trailing type padding.
			tw.Printf("%s  %s", colors.Dim(ts), colorEventType(event.Type))
		}

		if showDetail && detail != "" {
			if termWidth > 0 {
				usedWidth := len([]rune(ts)) + 12 + eventTaskWidth + 2
				maxDetail := termWidth - usedWidth
				if maxDetail > 0 {
					detail = ui.Truncate(detail, maxDetail)
					tw.Printf("  %s", detail)
				}
			} else {
				tw.Printf("  %s", detail)
			}
		}
		tw.Println()
	}
	return tw.Err()
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
