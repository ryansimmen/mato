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
	messages, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		return err
	}
	if len(messages) > 5 {
		messages = messages[len(messages)-5:]
	}

	fmt.Println("Task Queue Status")
	fmt.Println("─────────────────")
	for _, dir := range queueDirs {
		fmt.Printf("  %-15s %d\n", dir+":", counts[dir])
	}

	fmt.Println()
	fmt.Println("Active Agents")
	fmt.Println("─────────────")
	if len(agents) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, agent := range agents {
			fmt.Printf("  %s (PID %d)\n", agent.displayName(), agent.PID)
		}
	}

	fmt.Println()
	fmt.Println("Waiting Tasks")
	fmt.Println("─────────────")
	if len(waitingTasks) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, task := range waitingTasks {
			fmt.Printf("  %s\n", task.Name)
			fmt.Printf("    depends on: %s\n", strings.Join(task.Dependencies, ", "))
		}
	}

	fmt.Println()
	fmt.Println("Recent Messages")
	fmt.Println("───────────────")
	if len(messages) == 0 {
		fmt.Println("  (none)")
	} else {
		for i := len(messages) - 1; i >= 0; i-- {
			msg := messages[i]
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
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
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
		{Dir: "backlog", State: "pending"},
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
