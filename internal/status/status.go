package status

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
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
	queueDirs := []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed"}
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
	readyToMergeTasks := listTasksInDir(tasksDir, "ready-to-merge")
	failedTasks := listTasksInDir(tasksDir, "failed")
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

	// ── Queue Overview ──
	fmt.Println("Queue Overview")
	fmt.Println("──────────────")
	fmt.Printf("  runnable:       %d\n", runnable)
	fmt.Printf("  deferred:       %d  (conflict-blocked, in backlog)\n", len(deferred))
	fmt.Printf("  waiting:        %d  (dependency-blocked)\n", counts["waiting"])
	fmt.Printf("  in-progress:    %d\n", counts["in-progress"])
	fmt.Printf("  ready-to-merge: %d\n", counts["ready-to-merge"])
	fmt.Printf("  completed:      %d\n", counts["completed"])
	fmt.Printf("  failed:         %d\n", counts["failed"])

	// ── Active Agents ──
	presenceMap, _ := messaging.ReadAllPresence(tasksDir)
	fmt.Println()
	fmt.Println("Active Agents")
	fmt.Println("─────────────")
	if len(agents) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, agent := range agents {
			if p, ok := presenceMap[agent.ID]; ok {
				fmt.Printf("  %s (PID %d): %s on %s\n", agent.displayName(), agent.PID, p.Task, p.Branch)
			} else {
				fmt.Printf("  %s (PID %d)\n", agent.displayName(), agent.PID)
			}
		}
	}

	// ── Current Agent Progress ──
	progressByAgent := latestProgressByAgent(messages)
	fmt.Println()
	fmt.Println("Current Agent Progress")
	fmt.Println("──────────────────────")
	if len(progressByAgent) == 0 {
		fmt.Println("  (none)")
	} else {
		agentIDs := make([]string, 0, len(progressByAgent))
		for id := range progressByAgent {
			agentIDs = append(agentIDs, id)
		}
		sort.Strings(agentIDs)
		now := time.Now()
		for _, id := range agentIDs {
			pm := progressByAgent[id]
			ago := formatDuration(now.Sub(pm.SentAt))
			displayID := id
			if !strings.HasPrefix(displayID, "agent-") {
				displayID = "agent-" + displayID
			}
			fmt.Printf("  %s: %s (%s) — %s ago\n", displayID, pm.Body, pm.Task, ago)
		}
	}

	// ── In-Progress ──
	if len(inProgressTasks) > 0 {
		fmt.Println()
		fmt.Println("In-Progress Tasks")
		fmt.Println("─────────────────")
		for _, task := range inProgressTasks {
			claimedBy := queue.ParseClaimedBy(filepath.Join(tasksDir, "in-progress", task.name))
			if claimedBy != "" {
				fmt.Printf("  %s  (agent %s)\n", task.name, claimedBy)
			} else {
				fmt.Printf("  %s\n", task.name)
			}
		}
	}

	// ── Ready to Merge ──
	if len(readyToMergeTasks) > 0 {
		fmt.Println()
		fmt.Println("Ready to Merge")
		fmt.Println("──────────────")
		for _, task := range readyToMergeTasks {
			fmt.Printf("  %s  (priority %d)\n", task.name, task.priority)
		}
	}

	// ── Dependency-Blocked ──
	fmt.Println()
	fmt.Println("Dependency-Blocked (waiting/)")
	fmt.Println("─────────────────────────────")
	if len(waitingTasks) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, task := range waitingTasks {
			fmt.Printf("  %s\n", task.Name)
			fmt.Printf("    depends on: %s\n", strings.Join(task.Dependencies, ", "))
		}
	}

	// ── Conflict-Deferred ──
	fmt.Println()
	fmt.Println("Conflict-Deferred (backlog/, excluded from queue)")
	fmt.Println("──────────────────────────────────────────────────")
	if len(deferred) == 0 {
		fmt.Println("  (none)")
	} else {
		deferredNames := make([]string, 0, len(deferred))
		for name := range deferred {
			deferredNames = append(deferredNames, name)
		}
		sort.Strings(deferredNames)
		for _, name := range deferredNames {
			info := deferredDetail[name]
			fmt.Printf("  %s\n", name)
			fmt.Printf("    blocked by: %s (%s/)\n", info.BlockedBy, info.BlockedByDir)
			fmt.Printf("    overlapping: %s\n", strings.Join(info.OverlapFiles, ", "))
		}
	}

	// ── Failed ──
	if len(failedTasks) > 0 {
		fmt.Println()
		fmt.Println("Failed Tasks")
		fmt.Println("────────────")
		for _, task := range failedTasks {
			failCount := countFailureRecords(filepath.Join(tasksDir, "failed", task.name))
			fmt.Printf("  %s  (%d failures)\n", task.name, failCount)
		}
	}

	// ── Recent Messages ──
	fmt.Println()
	fmt.Println("Recent Messages")
	fmt.Println("───────────────")
	if len(recentMessages) == 0 {
		fmt.Println("  (none)")
	} else {
		for i := len(recentMessages) - 1; i >= 0; i-- {
			msg := recentMessages[i]
			line := strings.TrimSpace(strings.ReplaceAll(msg.Body, "\n", " "))
			if line == "" {
				line = msg.Type
			} else {
				line = msg.Type + " — " + line
			}
			fmt.Printf("  [%s] %s: %s\n", msg.SentAt.Local().Format("15:04:05"), msg.From, line)
		}
	}

	return nil
}

type taskEntry struct {
	name     string
	priority int
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
		meta, _, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, dir, e.Name()))
		priority := 50
		if err == nil {
			priority = meta.Priority
		}
		tasks = append(tasks, taskEntry{name: e.Name(), priority: priority})
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
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "<!-- failure:")
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
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse %s: %v\n", path, err)
			continue
		}

		deps := make([]string, 0, len(meta.DependsOn))
		for _, dep := range meta.DependsOn {
			status := stateByID[dep]
			if status == "" {
				status = "missing"
			}
			symbol := "✗"
			if status == "completed" {
				symbol = "✓"
			}
			deps = append(deps, fmt.Sprintf("%s (%s %s)", dep, symbol, status))
		}
		if len(deps) == 0 {
			deps = []string{"none"}
		}

		waiting = append(waiting, waitingTaskSummary{
			Name:         entry.Name(),
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
