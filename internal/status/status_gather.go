package status

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/lockfile"
	"mato/internal/messaging"
	"mato/internal/queue"
)

// statusMessageLimit is the maximum number of recent messages read by
// gatherStatus. It is large enough to cover the 5 displayed messages plus
// latest-progress lookups for all active agents, while avoiding the I/O
// cost of reading thousands of old message files.
const statusMessageLimit = 50

// statusData holds all the data gathered for the status dashboard.
type statusData struct {
	queueCounts     map[string]int
	runnable        int
	runnableBacklog []taskEntry
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
	warnings        []string
}

// progressEntry holds a formatted progress message for an active agent.
type progressEntry struct {
	displayID string
	body      string
	task      string
	ago       string
}

// gatherStatus collects all data needed for the status dashboard.
// It builds a single PollIndex to avoid redundant filesystem scans,
// then derives all queue data from that snapshot.
func gatherStatus(tasksDir string) (statusData, error) {
	var data statusData

	// Build one index for the entire gather cycle.
	idx := queue.BuildIndex(tasksDir)

	// Queue counts derived from the index snapshot.
	// Include parse-failed files in counts to match old countMarkdownFiles behavior.
	data.queueCounts = make(map[string]int, len(queue.AllDirs))
	for _, dir := range queue.AllDirs {
		data.queueCounts[dir] = len(idx.TasksByState(dir))
	}
	for _, pf := range idx.ParseFailures() {
		data.queueCounts[pf.State]++
	}

	// Active agents.
	agents, err := activeAgents(tasksDir)
	if err != nil {
		return data, err
	}
	data.agents = agents

	// Presence info.
	presenceMap, presenceErr := messaging.ReadAllPresence(tasksDir)
	if presenceErr != nil {
		data.warnings = append(data.warnings, fmt.Sprintf("could not read agent presence: %v", presenceErr))
	}
	data.presenceMap = presenceMap

	// Waiting tasks (dependency-blocked) — derived from index.
	data.waitingTasks = waitingTasksFromIndex(idx)

	// Deferred (conflict-blocked) tasks — reuse the same index.
	data.deferredDetail = queue.DeferredOverlappingTasksDetailed(tasksDir, idx)
	deferred := make(map[string]struct{}, len(data.deferredDetail))
	for name := range data.deferredDetail {
		deferred[name] = struct{}{}
	}

	// Runnable count.
	data.runnable = data.queueCounts[queue.DirBacklog] - len(deferred)
	if data.runnable < 0 {
		data.runnable = 0
	}

	// Runnable backlog in priority order — same ordering the host uses to
	// claim the next task, minus conflict-deferred entries.
	runnableSnaps := idx.BacklogByPriority(deferred)
	data.runnableBacklog = make([]taskEntry, 0, len(runnableSnaps))
	for _, snap := range runnableSnaps {
		data.runnableBacklog = append(data.runnableBacklog, taskEntry{
			name:       snap.Filename,
			title:      frontmatter.ExtractTitle(snap.Filename, snap.Body),
			id:         snap.Meta.ID,
			priority:   snap.Meta.Priority,
			maxRetries: snap.Meta.MaxRetries,
		})
	}

	// Task lists by state — derived from index.
	data.inProgressTasks = listTasksFromIndex(idx, queue.DirInProgress)
	data.readyForReview = listTasksFromIndex(idx, queue.DirReadyReview)
	data.readyToMerge = listTasksFromIndex(idx, queue.DirReadyMerge)
	data.failedTasks = listTasksFromIndex(idx, queue.DirFailed)

	// Reverse dependencies — derived from index.
	data.reverseDeps = reverseDepsFromIndex(idx)

	// Completions.
	completions, completionsErr := messaging.ReadAllCompletionDetails(tasksDir)
	if completionsErr != nil {
		data.warnings = append(data.warnings, fmt.Sprintf("could not read completion details: %v", completionsErr))
	}
	data.completions = completions

	// Merge lock.
	data.mergeLockActive = isMergeLockActive(tasksDir)

	// Messages — read only the most recent entries to avoid scanning
	// thousands of old files in long-running repos.
	messages, err := messaging.ReadRecentMessages(tasksDir, statusMessageLimit)
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
	lockPath := filepath.Join(tasksDir, ".locks", "merge.lock")
	return lockfile.IsHeld(lockPath)
}
