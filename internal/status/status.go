package status

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

func listTasksInDir(tasksDir, dir string) []taskEntry {
	names, err := queue.ListTaskFiles(filepath.Join(tasksDir, dir))
	if err != nil {
		return nil
	}
	tasks := make([]taskEntry, 0, len(names))
	for _, name := range names {
		meta, body, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, dir, name))
		priority := 50
		maxRetries := 3
		var title, id string
		if err == nil {
			priority = meta.Priority
			maxRetries = meta.MaxRetries
			id = meta.ID
			title = frontmatter.ExtractTitle(name, body)
		}
		tasks = append(tasks, taskEntry{name: name, title: title, id: id, priority: priority, maxRetries: maxRetries})
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

func countMarkdownFiles(dir string) int {
	names, err := queue.ListTaskFiles(dir)
	if err != nil {
		return 0
	}
	return len(names)
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

func waitingTasksStatus(tasksDir string) ([]waitingTaskSummary, error) {
	stateByID, err := taskStatesByID(tasksDir)
	if err != nil {
		return nil, err
	}

	names, err := queue.ListTaskFiles(filepath.Join(tasksDir, queue.DirWaiting))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read waiting dir: %w", err)
	}

	waiting := make([]waitingTaskSummary, 0, len(names))
	for _, name := range names {
		path := filepath.Join(tasksDir, queue.DirWaiting, name)
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse %s: %v\n", path, err)
			continue
		}
		title := frontmatter.ExtractTitle(name, body)

		deps := make([]string, 0, len(meta.DependsOn))
		greenSym := color.New(color.FgGreen).SprintFunc()
		redSym := color.New(color.FgRed).SprintFunc()
		for _, dep := range meta.DependsOn {
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
			Name:         name,
			Title:        title,
			Priority:     meta.Priority,
			Dependencies: deps,
		})
	}

	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].Priority != waiting[j].Priority {
			return waiting[i].Priority < waiting[j].Priority
		}
		return waiting[i].Name < waiting[j].Name
	})
	return waiting, nil
}

func taskStatesByID(tasksDir string) (map[string]string, error) {
	dirStates := []struct {
		Dir   string
		State string
	}{
		{Dir: queue.DirWaiting, State: queue.DirWaiting},
		{Dir: queue.DirBacklog, State: queue.DirBacklog},
		{Dir: queue.DirInProgress, State: queue.DirInProgress},
		{Dir: queue.DirReadyReview, State: queue.DirReadyReview},
		{Dir: queue.DirReadyMerge, State: queue.DirReadyMerge},
		{Dir: queue.DirCompleted, State: queue.DirCompleted},
		{Dir: queue.DirFailed, State: queue.DirFailed},
	}

	states := make(map[string]string)
	for _, dirState := range dirStates {
		names, err := queue.ListTaskFiles(filepath.Join(tasksDir, dirState.Dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s dir: %w", dirState.Dir, err)
		}
		for _, name := range names {
			path := filepath.Join(tasksDir, dirState.Dir, name)
			meta, _, err := frontmatter.ParseTaskFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not parse %s: %v\n", path, err)
				continue
			}
			states[frontmatter.TaskFileStem(name)] = dirState.State
			if meta.ID != "" {
				states[meta.ID] = dirState.State
			}
		}
	}
	return states, nil
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

var claimedAtRe = regexp.MustCompile(`claimed-at:\s*(\S+)`)
var branchCommentRe = regexp.MustCompile(`<!-- branch:\s*(\S+)\s*-->`)

// parseBranchComment extracts the branch name from a <!-- branch: ... --> comment.
func parseBranchComment(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := branchCommentRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// parseClaimedAt extracts the claimed-at timestamp from a task file's HTML comment.
func parseClaimedAt(path string) time.Time {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	m := claimedAtRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		return time.Time{}
	}
	return t
}

var failureLineRe = regexp.MustCompile(`<!-- failure:.*?—\s*(.+?)\s*-->`)

// lastFailureReason extracts the reason from the last <!-- failure: ... --> comment.
func lastFailureReason(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	matches := failureLineRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// reverseDependencies scans waiting/ tasks and returns a map from dependency ID
// to the list of task filenames that depend on it.
func reverseDependencies(tasksDir string) map[string][]string {
	names, err := queue.ListTaskFiles(filepath.Join(tasksDir, queue.DirWaiting))
	if err != nil {
		return nil
	}
	result := make(map[string][]string)
	for _, name := range names {
		meta, _, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, queue.DirWaiting, name))
		if err != nil {
			continue
		}
		for _, dep := range meta.DependsOn {
			result[dep] = append(result[dep], name)
		}
	}
	return result
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// Watch calls Show in a loop, redrawing the terminal without flicker.
// It buffers all output, then writes it atomically: cursor-home, single write,
// clear remaining lines. It runs until the context is cancelled (e.g. via
// Ctrl+C signal).
func Watch(ctx context.Context, repoRoot, tasksDir string, interval time.Duration) error {
	dim := color.New(color.Faint).SprintFunc()
	for {
		var buf bytes.Buffer
		if err := ShowTo(&buf, repoRoot, tasksDir); err != nil {
			return err
		}
		fmt.Fprintf(&buf, "\n%s\n", dim(fmt.Sprintf("Refreshing every %s — press Ctrl+C to stop", interval)))

		// Atomic redraw: move cursor home, write content, clear any leftover
		// lines below the new output.
		os.Stdout.Write([]byte("\033[H"))
		os.Stdout.Write(buf.Bytes())
		os.Stdout.Write([]byte("\033[J"))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
