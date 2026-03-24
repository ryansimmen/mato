// Package status gathers and displays queue state for the mato status command,
// including task counts, agent activity, and dependency information.
package status

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/identity"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/taskfile"
)

// Show writes the status dashboard to os.Stdout.
func Show(repoRoot, tasksDir string) error {
	return ShowTo(os.Stdout, repoRoot, tasksDir)
}

// ShowTo writes the status dashboard to the given writer.
func ShowTo(w io.Writer, repoRoot, tasksDir string) error {
	resolvedRepoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(resolvedRepoRoot)
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}

	data, err := gatherStatus(tasksDir)
	if err != nil {
		return err
	}

	c := newColorSet()
	renderQueueOverview(w, c, data)
	renderActiveAgents(w, c, data)
	renderAgentProgress(w, c, data)
	renderInProgressTasks(w, c, tasksDir, data)
	renderReadyForReview(w, c, tasksDir, data)
	renderReadyToMerge(w, c, data)
	renderDependencyBlocked(w, c, data)
	renderConflictDeferred(w, c, data)
	renderFailedTasks(w, c, tasksDir, data)
	renderRecentCompletions(w, c, data)
	renderRecentMessages(w, c, data)

	return nil
}

type taskEntry struct {
	name       string
	title      string
	id         string
	priority   int
	maxRetries int
}

// listTasksFromIndex derives a sorted task list from the PollIndex snapshot
// for the given directory, replacing the old listTasksInDir which performed
// its own filesystem scan. Tasks with parse failures are included with
// default metadata to preserve visibility.
func listTasksFromIndex(idx *queue.PollIndex, dir string) []taskEntry {
	snaps := idx.TasksByState(dir)
	tasks := make([]taskEntry, 0, len(snaps))
	for _, snap := range snaps {
		tasks = append(tasks, taskEntry{
			name:       snap.Filename,
			title:      frontmatter.ExtractTitle(snap.Filename, snap.Body),
			id:         snap.Meta.ID,
			priority:   snap.Meta.Priority,
			maxRetries: snap.Meta.MaxRetries,
		})
	}
	// Include files that failed frontmatter parsing with defaults,
	// so they remain visible in the status dashboard.
	for _, pf := range idx.ParseFailures() {
		if pf.State != dir {
			continue
		}
		tasks = append(tasks, taskEntry{
			name:       pf.Filename,
			priority:   50,
			maxRetries: 3,
		})
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})
	return tasks
}

func countFailureRecords(path string) int {
	count, _ := queue.CountFailureLines(path)
	return count
}

type statusAgent struct {
	ID  string
	PID int
}

func (a statusAgent) displayName() string {
	if strings.HasPrefix(a.ID, "agent-") {
		return a.ID
	}
	return "agent-" + a.ID
}

type waitingTaskSummary struct {
	Name         string
	Title        string
	Priority     int
	Dependencies []string
}

func activeAgents(tasksDir string) ([]statusAgent, error) {
	entries, err := os.ReadDir(filepath.Join(tasksDir, ".locks"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read locks dir: %w", err)
	}

	agents := make([]statusAgent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(entry.Name(), ".pid")
		if !identity.IsAgentActive(tasksDir, agentID) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tasksDir, ".locks", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read lock file %s: %w", entry.Name(), err)
		}
		// Lock identity format is "PID:starttime" (or legacy "PID").
		identity := strings.TrimSpace(string(data))
		parts := strings.SplitN(identity, ":", 2)
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		agents = append(agents, statusAgent{ID: agentID, PID: pid})
	}

	sort.Slice(agents, func(i, j int) bool {
		return agents[i].displayName() < agents[j].displayName()
	})
	return agents, nil
}

// waitingTasksFromIndex derives waiting task summaries from the PollIndex snapshot,
// replacing waitingTasksStatus which performed its own filesystem scans.
func waitingTasksFromIndex(idx *queue.PollIndex) []waitingTaskSummary {
	snaps := idx.TasksByState(queue.DirWaiting)

	// Build ID→state map from the index.
	stateByID := taskStatesByIDFromIndex(idx)

	greenSym := color.New(color.FgGreen).SprintFunc()
	redSym := color.New(color.FgRed).SprintFunc()

	waiting := make([]waitingTaskSummary, 0, len(snaps))
	for _, snap := range snaps {
		title := frontmatter.ExtractTitle(snap.Filename, snap.Body)
		deps := make([]string, 0, len(snap.Meta.DependsOn))
		for _, dep := range snap.Meta.DependsOn {
			status := stateByID[dep]
			if status == "" {
				status = "missing"
			}
			symbol := redSym("✗")
			if status == queue.DirCompleted {
				symbol = greenSym("✓")
			}
			deps = append(deps, fmt.Sprintf("%s (%s %s)", dep, symbol, status))
		}
		if len(deps) == 0 {
			deps = []string{"none"}
		}
		waiting = append(waiting, waitingTaskSummary{
			Name:         snap.Filename,
			Title:        title,
			Priority:     snap.Meta.Priority,
			Dependencies: deps,
		})
	}

	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].Priority != waiting[j].Priority {
			return waiting[i].Priority < waiting[j].Priority
		}
		return waiting[i].Name < waiting[j].Name
	})
	return waiting
}

// taskStatesByIDFromIndex builds an ID→state map from the PollIndex snapshot,
// replacing taskStatesByID which performed its own full directory scans.
func taskStatesByIDFromIndex(idx *queue.PollIndex) map[string]string {
	states := make(map[string]string)
	for _, dir := range queue.AllDirs {
		for _, snap := range idx.TasksByState(dir) {
			states[frontmatter.TaskFileStem(snap.Filename)] = dir
			if snap.Meta.ID != "" {
				states[snap.Meta.ID] = dir
			}
		}
	}
	return states
}

// reverseDepsFromIndex derives reverse dependencies from the PollIndex snapshot,
// replacing reverseDependencies which performed its own filesystem scan.
func reverseDepsFromIndex(idx *queue.PollIndex) map[string][]string {
	snaps := idx.TasksByState(queue.DirWaiting)
	result := make(map[string][]string)
	for _, snap := range snaps {
		for _, dep := range snap.Meta.DependsOn {
			result[dep] = append(result[dep], snap.Filename)
		}
	}
	return result
}

// latestProgressByAgent returns the most recent progress message per agent.
func latestProgressByAgent(messages []messaging.Message) map[string]messaging.Message {
	result := make(map[string]messaging.Message)
	for _, msg := range messages {
		if msg.Type != "progress" {
			continue
		}
		if existing, ok := result[msg.From]; !ok || msg.SentAt.After(existing.SentAt) {
			result[msg.From] = msg
		}
	}
	return result
}

// formatDuration returns a human-friendly "X min ago" or "X sec ago" string.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		sec := int(d.Seconds())
		if sec < 1 {
			sec = 1
		}
		return fmt.Sprintf("%d sec", sec)
	}
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

// parseBranchComment extracts the branch name from a <!-- branch: ... --> comment.
func parseBranchComment(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	branch, _ := taskfile.ParseBranchComment(data)
	return branch
}

// parseClaimedAt extracts the claimed-at timestamp from a task file's HTML comment.
func parseClaimedAt(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	t, ok := taskfile.ParseClaimedAt(data)
	if !ok {
		return time.Time{}
	}
	return t
}

// lastFailureReason extracts the reason from the last <!-- failure: ... --> comment.
func lastFailureReason(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return taskfile.LastFailureReason(data)
}

// lastCycleFailureReason extracts the reason from the last <!-- cycle-failure: ... -->
// comment. Returns "" if no cycle-failure markers are found.
func lastCycleFailureReason(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return taskfile.LastCycleFailureReason(data)
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// Watch calls Show in a loop, redrawing the terminal without flicker.
// It writes to os.Stdout; use WatchTo to write to a different writer.
func Watch(ctx context.Context, repoRoot, tasksDir string, interval time.Duration) error {
	return WatchTo(ctx, os.Stdout, repoRoot, tasksDir, interval)
}

// WatchTo calls ShowTo in a loop, redrawing the given writer without flicker.
// It buffers all output, then writes it atomically: cursor-home, single write
// with per-line erase-to-EOL (\033[K), then clear remaining lines below.
// The per-line erase prevents artifacts when a line shrinks between frames.
// It runs until the context is cancelled or a write error occurs (e.g. stdout
// closed by a pager or pipe).
func WatchTo(ctx context.Context, w io.Writer, repoRoot, tasksDir string, interval time.Duration) error {
	dim := color.New(color.Faint).SprintFunc()
	for {
		var buf bytes.Buffer
		if err := ShowTo(&buf, repoRoot, tasksDir); err != nil {
			return err
		}
		fmt.Fprintf(&buf, "\n%s\n", dim(fmt.Sprintf("Refreshing every %s — press Ctrl+C to stop", interval)))

		// Atomic redraw: move cursor home, write content with per-line
		// clearing, then erase any leftover lines below the new output.
		// Each \n is preceded by \033[K (erase-to-EOL) so that shorter
		// replacement lines don't leave trailing artifacts from the
		// previous frame.
		if _, err := w.Write([]byte("\033[H")); err != nil {
			return fmt.Errorf("redraw cursor-home: %w", err)
		}
		content := bytes.ReplaceAll(buf.Bytes(), []byte("\n"), []byte("\033[K\n"))
		if _, err := w.Write(content); err != nil {
			return fmt.Errorf("redraw content: %w", err)
		}
		if _, err := w.Write([]byte("\033[J")); err != nil {
			return fmt.Errorf("redraw clear-tail: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
