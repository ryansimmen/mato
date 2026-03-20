package status

import (
	"context"
	"fmt"
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
	"mato/internal/messaging"
	"mato/internal/process"
	"mato/internal/queue"
)

func Show(repoRoot, tasksDir string) error {
	resolvedRepoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(resolvedRepoRoot)
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}

	// Collect all data before printing.
	queueDirs := []string{"waiting", "backlog", "in-progress", "ready-for-review", "ready-to-merge", "completed", "failed"}
	counts := make(map[string]int, len(queueDirs))
	for _, dir := range queueDirs {
		counts[dir] = countMarkdownFiles(filepath.Join(tasksDir, dir))
	}

	agents, err := activeAgents(tasksDir)
	if err != nil {
		return err
	}
	waitingTasks, err := waitingTasksStatus(tasksDir)
	if err != nil {
		return err
	}
	deferredDetail := queue.DeferredOverlappingTasksDetailed(tasksDir)
	deferred := make(map[string]struct{}, len(deferredDetail))
	for name := range deferredDetail {
		deferred[name] = struct{}{}
	}
	inProgressTasks := listTasksInDir(tasksDir, "in-progress")
	readyForReviewTasks := listTasksInDir(tasksDir, "ready-for-review")
	readyToMergeTasks := listTasksInDir(tasksDir, "ready-to-merge")
	failedTasks := listTasksInDir(tasksDir, "failed")
	reverseDeps := reverseDependencies(tasksDir)
	completions, _ := messaging.ReadAllCompletionDetails(tasksDir)
	mergeLockActive := isMergeLockActive(tasksDir)
	messages, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		return err
	}
	// Keep all messages for progress extraction; trim for recent display later.
	recentMessages := messages
	if len(recentMessages) > 5 {
		recentMessages = recentMessages[len(recentMessages)-5:]
	}

	runnable := counts["backlog"] - len(deferred)
	if runnable < 0 {
		runnable = 0
	}

	// Color helpers — automatically disabled when stdout is not a TTY
	// (e.g. piped output, CI, or tests redirecting os.Stdout).
	bold := color.New(color.Bold).SprintFunc()
	green := color.New(color.FgGreen).SprintFunc()
	red := color.New(color.FgRed).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()
	cyan := color.New(color.FgCyan).SprintFunc()
	dim := color.New(color.Faint).SprintFunc()

	// ── Queue Overview ──
	fmt.Println(bold("Queue Overview"))
	fmt.Println(bold("──────────────"))
	fmt.Printf("  runnable:       %s\n", green(runnable))
	fmt.Printf("  deferred:       %s  %s\n", yellow(len(deferred)), dim("(conflict-blocked, in backlog)"))
	fmt.Printf("  waiting:        %s  %s\n", dim(counts["waiting"]), dim("(dependency-blocked)"))
	fmt.Printf("  in-progress:    %s\n", yellow(counts["in-progress"]))
	fmt.Printf("  ready-review:   %s\n", cyan(counts["ready-for-review"]))
	fmt.Printf("  ready-to-merge: %s\n", cyan(counts["ready-to-merge"]))
	fmt.Printf("  completed:      %s\n", green(counts["completed"]))
	fmt.Printf("  failed:         %s\n", red(counts["failed"]))
	if mergeLockActive {
		fmt.Printf("  merge queue:    %s\n", yellow("active"))
	} else {
		fmt.Printf("  merge queue:    %s\n", dim("idle"))
	}

	// ── Active Agents ──
	presenceMap, _ := messaging.ReadAllPresence(tasksDir)
	fmt.Println()
	fmt.Println(bold("Active Agents"))
	fmt.Println(bold("─────────────"))
	if len(agents) == 0 {
		fmt.Println(dim("  (none)"))
	} else {
		for _, agent := range agents {
			if p, ok := presenceMap[agent.ID]; ok {
				fmt.Printf("  %s (PID %d): %s on %s\n", yellow(agent.displayName()), agent.PID, p.Task, cyan(p.Branch))
			} else {
				fmt.Printf("  %s (PID %d)\n", yellow(agent.displayName()), agent.PID)
			}
		}
	}

	// ── Current Agent Progress ──
	progressByAgent := latestProgressByAgent(messages)
	// Only show progress for currently active agents.
	activeAgentIDs := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		activeAgentIDs[a.ID] = struct{}{}
	}
	fmt.Println()
	fmt.Println(bold("Current Agent Progress"))
	fmt.Println(bold("──────────────────────"))
	activeProgress := make([]string, 0)
	for id := range progressByAgent {
		if _, ok := activeAgentIDs[id]; ok {
			activeProgress = append(activeProgress, id)
		}
	}
	sort.Strings(activeProgress)
	if len(activeProgress) == 0 {
		fmt.Println(dim("  (none)"))
	} else {
		now := time.Now().UTC()
		for _, id := range activeProgress {
			pm := progressByAgent[id]
			ago := formatDuration(now.Sub(pm.SentAt))
			displayID := id
			if !strings.HasPrefix(displayID, "agent-") {
				displayID = "agent-" + displayID
			}
			fmt.Printf("  %s: %s (%s) — %s ago\n", yellow(displayID), pm.Body, pm.Task, dim(ago))
		}
	}

	// ── In-Progress ──
	if len(inProgressTasks) > 0 {
		fmt.Println()
		fmt.Println(bold("In-Progress Tasks"))
		fmt.Println(bold("─────────────────"))
		now := time.Now().UTC()
		for _, task := range inProgressTasks {
			taskPath := filepath.Join(tasksDir, "in-progress", task.name)
			claimedBy := queue.ParseClaimedBy(taskPath)

			// Build info parts: title, agent, time-in-state, retry budget, reverse deps.
			var parts []string
			if claimedBy != "" {
				parts = append(parts, fmt.Sprintf("agent %s", claimedBy))
			}
			if claimedAt := parseClaimedAt(taskPath); !claimedAt.IsZero() {
				parts = append(parts, formatDuration(now.Sub(claimedAt)))
			}
			failCount := countFailureRecords(taskPath)
			if failCount > 0 {
				parts = append(parts, fmt.Sprintf("%s/%d retries used", red(failCount), task.maxRetries))
			}
			taskID := task.id
			if taskID == "" {
				taskID = frontmatter.TaskFileStem(task.name)
			}
			if waiters, ok := reverseDeps[taskID]; ok {
				parts = append(parts, fmt.Sprintf("%d %s waiting", len(waiters), pluralize(len(waiters), "task", "tasks")))
			}

			label := yellow(task.name)
			if task.title != "" {
				label = fmt.Sprintf("%s — %s", yellow(task.name), task.title)
			}
			if len(parts) > 0 {
				fmt.Printf("  %s  (%s)\n", label, strings.Join(parts, ", "))
			} else {
				fmt.Printf("  %s\n", label)
			}
		}
	}

	// ── Ready for Review ──
	if len(readyForReviewTasks) > 0 {
		fmt.Println()
		fmt.Println(bold("Ready for Review"))
		fmt.Println(bold("────────────────"))
		for _, task := range readyForReviewTasks {
			taskPath := filepath.Join(tasksDir, "ready-for-review", task.name)
			branch := parseBranchComment(taskPath)
			var parts []string
			if task.title != "" {
				parts = append(parts, task.title)
			}
			if branch != "" {
				parts = append(parts, "on "+cyan(branch))
			}
			if len(parts) > 0 {
				fmt.Printf("  %s — %s\n", cyan(task.name), strings.Join(parts, " "))
			} else {
				fmt.Printf("  %s\n", cyan(task.name))
			}
		}
	}

	// ── Ready to Merge ──
	if len(readyToMergeTasks) > 0 {
		fmt.Println()
		fmt.Println(bold("Ready to Merge"))
		fmt.Println(bold("──────────────"))
		for _, task := range readyToMergeTasks {
			label := cyan(task.name)
			if task.title != "" {
				label = fmt.Sprintf("%s — %s", cyan(task.name), task.title)
			}
			fmt.Printf("  %s  %s\n", label, dim(fmt.Sprintf("(priority %d)", task.priority)))
		}
	}

	// ── Dependency-Blocked ──
	fmt.Println()
	fmt.Println(bold("Dependency-Blocked (waiting/)"))
	fmt.Println(bold("─────────────────────────────"))
	if len(waitingTasks) == 0 {
		fmt.Println(dim("  (none)"))
	} else {
		for _, task := range waitingTasks {
			label := task.Name
			if task.Title != "" {
				label = fmt.Sprintf("%s — %s", task.Name, task.Title)
			}
			fmt.Printf("  %s\n", label)
			fmt.Printf("    depends on: %s\n", strings.Join(task.Dependencies, ", "))
		}
	}

	// ── Conflict-Deferred ──
	fmt.Println()
	fmt.Println(bold("Conflict-Deferred (backlog/, excluded from queue)"))
	fmt.Println(bold("──────────────────────────────────────────────────"))
	if len(deferred) == 0 {
		fmt.Println(dim("  (none)"))
	} else {
		deferredNames := make([]string, 0, len(deferred))
		for name := range deferred {
			deferredNames = append(deferredNames, name)
		}
		sort.Strings(deferredNames)
		for _, name := range deferredNames {
			info := deferredDetail[name]
			fmt.Printf("  %s\n", yellow(name))
			fmt.Printf("    blocked by: %s (%s/)\n", info.BlockedBy, info.BlockedByDir)
			fmt.Printf("    overlapping: %s\n", strings.Join(info.OverlapFiles, ", "))
		}
	}

	// ── Failed ──
	if len(failedTasks) > 0 {
		fmt.Println()
		fmt.Println(bold("Failed Tasks"))
		fmt.Println(bold("────────────"))
		for _, task := range failedTasks {
			taskPath := filepath.Join(tasksDir, "failed", task.name)
			failCount := countFailureRecords(taskPath)
			label := red(task.name)
			if task.title != "" {
				label = fmt.Sprintf("%s — %s", red(task.name), task.title)
			}
			reason := lastFailureReason(taskPath)
			info := fmt.Sprintf("%d/%d retries exhausted", failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
			fmt.Printf("  %s  (%s)\n", label, info)
		}
	}

	// ── Recent Completions ──
	if len(completions) > 0 {
		fmt.Println()
		fmt.Println(bold("Recent Completions"))
		fmt.Println(bold("──────────────────"))
		show := completions
		if len(show) > 5 {
			show = show[:5]
		}
		now := time.Now().UTC()
		for _, c := range show {
			ago := formatDuration(now.Sub(c.MergedAt))
			shortSHA := c.CommitSHA
			if len(shortSHA) > 7 {
				shortSHA = shortSHA[:7]
			}
			label := green(c.TaskFile)
			if c.Title != "" {
				label = fmt.Sprintf("%s — %s", green(c.TaskFile), c.Title)
			}
			fmt.Printf("  %s  %s\n", label, dim(fmt.Sprintf("(merged %s ago, %d %s, %s)", ago, len(c.FilesChanged), pluralize(len(c.FilesChanged), "file", "files"), shortSHA)))
		}
	}

	// ── Recent Messages ──
	fmt.Println()
	fmt.Println(bold("Recent Messages"))
	fmt.Println(bold("───────────────"))
	if len(recentMessages) == 0 {
		fmt.Println(dim("  (none)"))
	} else {
		for i := len(recentMessages) - 1; i >= 0; i-- {
			msg := recentMessages[i]
			line := strings.TrimSpace(strings.ReplaceAll(msg.Body, "\n", " "))
			if line == "" {
				line = msg.Type
			} else {
				line = msg.Type + " — " + line
			}
			from := msg.From
			if !strings.HasPrefix(from, "agent-") {
				from = "agent-" + from
			}
			fmt.Printf("  %s %s: %s\n", dim("["+msg.SentAt.Local().Format("15:04:05")+"]"), yellow(from), line)
		}
	}

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
	entries, err := os.ReadDir(filepath.Join(tasksDir, dir))
	if err != nil {
		return nil
	}
	tasks := make([]taskEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		meta, body, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, dir, e.Name()))
		priority := 50
		maxRetries := 3
		var title, id string
		if err == nil {
			priority = meta.Priority
			maxRetries = meta.MaxRetries
			id = meta.ID
			title = frontmatter.ExtractTitle(e.Name(), body)
		}
		tasks = append(tasks, taskEntry{name: e.Name(), title: title, id: id, priority: priority, maxRetries: maxRetries})
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
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			count++
		}
	}
	return count
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
		if !queue.IsAgentActive(tasksDir, agentID) {
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

	entries, err := os.ReadDir(filepath.Join(tasksDir, "waiting"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read waiting dir: %w", err)
	}

	waiting := make([]waitingTaskSummary, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(tasksDir, "waiting", entry.Name())
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse %s: %v\n", path, err)
			continue
		}
		title := frontmatter.ExtractTitle(entry.Name(), body)

		deps := make([]string, 0, len(meta.DependsOn))
		greenSym := color.New(color.FgGreen).SprintFunc()
		redSym := color.New(color.FgRed).SprintFunc()
		for _, dep := range meta.DependsOn {
			status := stateByID[dep]
			if status == "" {
				status = "missing"
			}
			symbol := redSym("✗")
			if status == "completed" {
				symbol = greenSym("✓")
			}
			deps = append(deps, fmt.Sprintf("%s (%s %s)", dep, symbol, status))
		}
		if len(deps) == 0 {
			deps = []string{"none"}
		}

		waiting = append(waiting, waitingTaskSummary{
			Name:         entry.Name(),
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
		{Dir: "waiting", State: "waiting"},
		{Dir: "backlog", State: "backlog"},
		{Dir: "in-progress", State: "in-progress"},
		{Dir: "ready-for-review", State: "ready-for-review"},
		{Dir: "ready-to-merge", State: "ready-to-merge"},
		{Dir: "completed", State: "completed"},
		{Dir: "failed", State: "failed"},
	}

	states := make(map[string]string)
	for _, dirState := range dirStates {
		entries, err := os.ReadDir(filepath.Join(tasksDir, dirState.Dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s dir: %w", dirState.Dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(tasksDir, dirState.Dir, entry.Name())
			meta, _, err := frontmatter.ParseTaskFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not parse %s: %v\n", path, err)
				continue
			}
			states[frontmatter.TaskFileStem(entry.Name())] = dirState.State
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
var branchCommentRe = regexp.MustCompile(`<!-- branch:\s*(\S+)`)

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
	entries, err := os.ReadDir(filepath.Join(tasksDir, "waiting"))
	if err != nil {
		return nil
	}
	result := make(map[string][]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		meta, _, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, "waiting", entry.Name()))
		if err != nil {
			continue
		}
		for _, dep := range meta.DependsOn {
			result[dep] = append(result[dep], entry.Name())
		}
	}
	return result
}

// isMergeLockActive checks whether the merge queue lock is held by a live process.
func isMergeLockActive(tasksDir string) bool {
	data, err := os.ReadFile(filepath.Join(tasksDir, ".locks", "merge.lock"))
	if err != nil {
		return false
	}
	identity := strings.TrimSpace(string(data))
	if identity == "" {
		return false
	}
	return process.IsLockHolderAlive(identity)
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// Watch calls Show in a loop, clearing the terminal before each refresh.
// It runs until the context is cancelled (e.g. via Ctrl+C signal).
func Watch(ctx context.Context, repoRoot, tasksDir string, interval time.Duration) error {
	dim := color.New(color.Faint).SprintFunc()
	for {
		// Move cursor to top-left and clear screen (ANSI escape).
		fmt.Print("\033[H\033[2J")
		if err := Show(repoRoot, tasksDir); err != nil {
			return err
		}
		fmt.Printf("\n%s\n", dim(fmt.Sprintf("Refreshing every %s — press Ctrl+C to stop", interval)))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}
