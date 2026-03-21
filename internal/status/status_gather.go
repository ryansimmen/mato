package status

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/messaging"
	"mato/internal/process"
	"mato/internal/queue"
)

// statusData holds all the data gathered for the status dashboard.
type statusData struct {
	queueCounts     map[string]int
	runnable        int
	agents          []statusAgent
	presenceMap     map[string]messaging.PresenceInfo
	activeProgress  []progressEntry
	inProgressTasks []taskEntry
	readyForReview  []taskEntry
	readyToMerge    []taskEntry
	waitingTasks    []waitingTaskSummary
	deferredDetail  map[string]queue.DeferralInfo
	failedTasks     []taskEntry
	completions     []messaging.CompletionDetail
	recentMessages  []messaging.Message
	reverseDeps     map[string][]string
	mergeLockActive bool
}

// progressEntry holds a formatted progress message for an active agent.
type progressEntry struct {
	displayID string
	body      string
	task      string
	ago       string
}

// gatherStatus collects all data needed for the status dashboard.
func gatherStatus(tasksDir string) (statusData, error) {
	var data statusData

	// Queue counts.
	data.queueCounts = make(map[string]int, len(queue.AllDirs))
	for _, dir := range queue.AllDirs {
		data.queueCounts[dir] = countMarkdownFiles(filepath.Join(tasksDir, dir))
	}

	// Active agents.
	agents, err := activeAgents(tasksDir)
	if err != nil {
		return data, err
	}
	data.agents = agents

	// Presence info.
	data.presenceMap, _ = messaging.ReadAllPresence(tasksDir)

	// Waiting tasks (dependency-blocked).
	waitingTasks, err := waitingTasksStatus(tasksDir)
	if err != nil {
		return data, err
	}
	data.waitingTasks = waitingTasks

	// Deferred (conflict-blocked) tasks.
	data.deferredDetail = queue.DeferredOverlappingTasksDetailed(tasksDir)
	deferred := make(map[string]struct{}, len(data.deferredDetail))
	for name := range data.deferredDetail {
		deferred[name] = struct{}{}
	}

	// Runnable count.
	data.runnable = data.queueCounts[queue.DirBacklog] - len(deferred)
	if data.runnable < 0 {
		data.runnable = 0
	}

	// Task lists by state.
	data.inProgressTasks = listTasksInDir(tasksDir, queue.DirInProgress)
	data.readyForReview = listTasksInDir(tasksDir, queue.DirReadyReview)
	data.readyToMerge = listTasksInDir(tasksDir, queue.DirReadyMerge)
	data.failedTasks = listTasksInDir(tasksDir, queue.DirFailed)

	// Reverse dependencies.
	data.reverseDeps = reverseDependencies(tasksDir)

	// Completions.
	data.completions, _ = messaging.ReadAllCompletionDetails(tasksDir)

	// Merge lock.
	data.mergeLockActive = isMergeLockActive(tasksDir)

	// Messages.
	messages, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		return data, err
	}

	// Recent messages (last 5).
	data.recentMessages = messages
	if len(data.recentMessages) > 5 {
		data.recentMessages = data.recentMessages[len(data.recentMessages)-5:]
	}

	// Agent progress (only for active agents).
	progressByAgent := latestProgressByAgent(messages)
	activeAgentIDs := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		activeAgentIDs[a.ID] = struct{}{}
	}
	activeIDs := make([]string, 0)
	for id := range progressByAgent {
		if _, ok := activeAgentIDs[id]; ok {
			activeIDs = append(activeIDs, id)
		}
	}
	sort.Strings(activeIDs)

	now := time.Now().UTC()
	data.activeProgress = make([]progressEntry, 0, len(activeIDs))
	for _, id := range activeIDs {
		pm := progressByAgent[id]
		displayID := id
		if !strings.HasPrefix(displayID, "agent-") {
			displayID = "agent-" + displayID
		}
		data.activeProgress = append(data.activeProgress, progressEntry{
			displayID: displayID,
			body:      pm.Body,
			task:      pm.Task,
			ago:       formatDuration(now.Sub(pm.SentAt)),
		})
	}

	return data, nil
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
