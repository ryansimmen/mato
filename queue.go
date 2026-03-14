package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var claimedByRe = regexp.MustCompile(`<!-- claimed-by:\s*(\S+)`)

// hasAvailableTasks reports whether there is at least one .md task file
// in backlog/. After orphan recovery, any task still in in-progress/
// belongs to an active agent, so only backlog/ matters for new agents.
func hasAvailableTasks(tasksDir string) bool {
	entries, err := os.ReadDir(filepath.Join(tasksDir, "backlog"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			return true
		}
	}
	return false
}

// registerAgent writes a PID lock file so concurrent mato instances
// know this agent is still alive. Returns a cleanup function.
func registerAgent(tasksDir, agentID string) (func(), error) {
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create locks dir: %w", err)
	}
	lockFile := filepath.Join(locksDir, agentID+".pid")
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, fmt.Errorf("write agent lock: %w", err)
	}
	return func() { os.Remove(lockFile) }, nil
}

// isAgentActive checks whether the agent that wrote a lock file is still running.
func isAgentActive(tasksDir, agentID string) bool {
	if agentID == "" {
		return false
	}
	lockFile := filepath.Join(tasksDir, ".locks", agentID+".pid")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// parseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func parseClaimedBy(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := claimedByRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// cleanStaleLocks removes lock files for agents that are no longer running.
func cleanStaleLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, ".locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".pid")
		if !isAgentActive(tasksDir, agentID) {
			os.Remove(filepath.Join(locksDir, e.Name()))
		}
	}
}

// recoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
func recoverOrphanedTasks(tasksDir string) {
	inProgress := filepath.Join(tasksDir, "in-progress")
	entries, err := os.ReadDir(inProgress)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		src := filepath.Join(inProgress, e.Name())

		// If the task is claimed by an agent that's still running, skip it.
		if agent := parseClaimedBy(src); agent != "" && isAgentActive(tasksDir, agent) {
			fmt.Printf("Skipping in-progress task %s (agent %s still active)\n", e.Name(), agent)
			continue
		}

		dst := filepath.Join(tasksDir, "backlog", e.Name())

		// Append a failure record so the retry count increments.
		f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "\n<!-- failure: mato-recovery at %s — agent was interrupted -->\n",
				time.Now().UTC().Format(time.RFC3339))
			f.Close()
		}

		if err := os.Rename(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not recover orphaned task %s: %v\n", e.Name(), err)
			continue
		}
		fmt.Printf("Recovered orphaned task %s back to backlog\n", e.Name())
	}
}

func reconcileReadyQueue(tasksDir string) int {
	completedIDs := completedTaskIDs(tasksDir)
	waitingDir := filepath.Join(tasksDir, "waiting")
	entries, err := os.ReadDir(waitingDir)
	if err != nil {
		return 0
	}

	promoted := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}

		src := filepath.Join(waitingDir, e.Name())
		meta, _, err := parseTaskFile(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse waiting task %s: %v\n", e.Name(), err)
			continue
		}

		ready := true
		for _, dep := range meta.DependsOn {
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: waiting task %s depends on missing task ID %q\n", e.Name(), dep)
			ready = false
		}
		if !ready {
			continue
		}

		dst := filepath.Join(tasksDir, "backlog", e.Name())
		if err := os.Rename(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not promote waiting task %s: %v\n", e.Name(), err)
			continue
		}
		promoted++
	}

	return promoted
}

func completedTaskIDs(tasksDir string) map[string]struct{} {
	completedDir := filepath.Join(tasksDir, "completed")
	entries, err := os.ReadDir(completedDir)
	if err != nil {
		return map[string]struct{}{}
	}

	ids := make(map[string]struct{}, len(entries)*2)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(completedDir, e.Name())
		meta, _, err := parseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse completed task %s: %v\n", e.Name(), err)
			continue
		}
		ids[taskFileStem(e.Name())] = struct{}{}
		ids[meta.ID] = struct{}{}
	}
	return ids
}

type queueEntry struct {
	name     string
	priority int
}

func writeQueueManifest(tasksDir string) error {
	entries, err := os.ReadDir(filepath.Join(tasksDir, "backlog"))
	if err != nil {
		return err
	}

	queue := make([]queueEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		meta, _, err := parseTaskFile(filepath.Join(tasksDir, "backlog", e.Name()))
		if err != nil {
			return fmt.Errorf("parse backlog task %s: %w", e.Name(), err)
		}
		queue = append(queue, queueEntry{name: e.Name(), priority: meta.Priority})
	}

	sort.Slice(queue, func(i, j int) bool {
		if queue[i].priority != queue[j].priority {
			return queue[i].priority < queue[j].priority
		}
		return queue[i].name < queue[j].name
	})

	lines := make([]string, 0, len(queue))
	for _, entry := range queue {
		lines = append(lines, entry.name)
	}
	manifest := strings.Join(lines, "\n")
	if manifest != "" {
		manifest += "\n"
	}
	return os.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest), 0o644)
}
