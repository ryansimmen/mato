package queue

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mato/internal/frontmatter"
)

var claimedByRe = regexp.MustCompile(`<!-- claimed-by:\s*(\S+)`)

// HasAvailableTasks reports whether there is at least one .md task file
// in backlog/. After orphan recovery, any task still in in-progress/
// belongs to an active agent, so only backlog/ matters for new agents.
func HasAvailableTasks(tasksDir string) bool {
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

// RegisterAgent writes a PID lock file so concurrent mato instances
// know this agent is still alive. Returns a cleanup function.
func RegisterAgent(tasksDir, agentID string) (func(), error) {
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

// IsAgentActive checks whether the agent that wrote a lock file is still running.
func IsAgentActive(tasksDir, agentID string) bool {
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

// ParseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func ParseClaimedBy(path string) string {
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

// CleanStaleLocks removes lock files for agents that are no longer running.
func CleanStaleLocks(tasksDir string) {
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
		if !IsAgentActive(tasksDir, agentID) {
			os.Remove(filepath.Join(locksDir, e.Name()))
		}
	}
}

// RecoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
func RecoverOrphanedTasks(tasksDir string) {
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

		if agent := ParseClaimedBy(src); agent != "" && IsAgentActive(tasksDir, agent) {
			fmt.Printf("Skipping in-progress task %s (agent %s still active)\n", e.Name(), agent)
			continue
		}

		dst := filepath.Join(tasksDir, "backlog", e.Name())

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

func ReconcileReadyQueue(tasksDir string) int {
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
		meta, _, err := frontmatter.ParseTaskFile(src)
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
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse completed task %s: %v\n", e.Name(), err)
			continue
		}
		ids[frontmatter.TaskFileStem(e.Name())] = struct{}{}
		ids[meta.ID] = struct{}{}
	}
	return ids
}

type queueEntry struct {
	name     string
	priority int
}

type backlogTask struct {
	name     string
	path     string
	priority int
	affects  []string
}

// RemoveOverlappingTasks checks tasks in backlog/ for overlapping affects: metadata.
// If two tasks have overlapping affects patterns, only the higher-priority one
// stays in backlog/; the other is moved back to waiting/.
func RemoveOverlappingTasks(tasksDir string) {
	backlogDir := filepath.Join(tasksDir, "backlog")
	entries, err := os.ReadDir(backlogDir)
	if err != nil {
		return
	}

	tasks := make([]backlogTask, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(backlogDir, entry.Name())
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for overlap detection: %v\n", entry.Name(), err)
			continue
		}
		tasks = append(tasks, backlogTask{
			name:     entry.Name(),
			path:     path,
			priority: meta.Priority,
			affects:  meta.Affects,
		})
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})

	kept := make([]backlogTask, 0, len(tasks))
	waitingDir := filepath.Join(tasksDir, "waiting")
	for _, task := range tasks {
		deferred := false
		for _, other := range kept {
			overlap := overlappingAffects(task.affects, other.affects)
			if len(overlap) == 0 {
				continue
			}
			dst := filepath.Join(waitingDir, task.name)
			if err := os.Rename(task.path, dst); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not defer overlapping task %s: %v\n", task.name, err)
				break
			}
			fmt.Printf("Deferring task %s (conflicts with higher-priority task %s on files %s)\n",
				task.name, other.name, strings.Join(overlap, ", "))
			deferred = true
			break
		}
		if !deferred {
			kept = append(kept, task)
		}
	}
}

func overlappingAffects(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(a))
	for _, item := range a {
		if item == "" {
			continue
		}
		seen[item] = struct{}{}
	}

	overlap := make([]string, 0)
	added := make(map[string]struct{})
	for _, item := range b {
		if _, ok := seen[item]; !ok {
			continue
		}
		if _, ok := added[item]; ok {
			continue
		}
		added[item] = struct{}{}
		overlap = append(overlap, item)
	}
	sort.Strings(overlap)
	return overlap
}

func WriteQueueManifest(tasksDir string) error {
	entries, err := os.ReadDir(filepath.Join(tasksDir, "backlog"))
	if err != nil {
		return err
	}

	queueEntries := make([]queueEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		meta, _, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, "backlog", e.Name()))
		if err != nil {
			return fmt.Errorf("parse backlog task %s: %w", e.Name(), err)
		}
		queueEntries = append(queueEntries, queueEntry{name: e.Name(), priority: meta.Priority})
	}

	sort.Slice(queueEntries, func(i, j int) bool {
		if queueEntries[i].priority != queueEntries[j].priority {
			return queueEntries[i].priority < queueEntries[j].priority
		}
		return queueEntries[i].name < queueEntries[j].name
	})

	lines := make([]string, 0, len(queueEntries))
	for _, entry := range queueEntries {
		lines = append(lines, entry.name)
	}
	manifest := strings.Join(lines, "\n")
	if manifest != "" {
		manifest += "\n"
	}
	return os.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest), 0o644)
}

func GenerateAgentID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
