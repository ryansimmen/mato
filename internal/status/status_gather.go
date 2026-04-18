package status

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/lockfile"
	"github.com/ryansimmen/mato/internal/messaging"
	"github.com/ryansimmen/mato/internal/pause"
	"github.com/ryansimmen/mato/internal/queueview"
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
	pauseState      pause.State
	agents          []statusAgent
	presenceMap     map[string]messaging.PresenceInfo
	activeProgress  []progressEntry
	inProgressTasks []taskEntry
	readyForReview  []taskEntry
	readyToMerge    []taskEntry
	waitingTasks    []waitingTaskSummary
	deferredDetail  map[string]queueview.DeferralInfo
	failedTasks     []taskEntry
	completions     []messaging.CompletionDetail
	recentMessages  []messaging.Message
	reverseDeps     map[string][]string
	mergeLockStatus lockfile.Status
	warnings        []string
}

var pauseReadFn = pause.Read

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
	idx := queueview.BuildIndex(tasksDir)

	// Queue counts derived from the index snapshot.
	// Include parse-failed files in counts to match old countMarkdownFiles behavior.
	data.queueCounts = make(map[string]int, len(dirs.All))
	for _, dir := range dirs.All {
		data.queueCounts[dir] = len(idx.TasksByState(dir))
	}
	for _, pf := range idx.ParseFailures() {
		data.queueCounts[pf.State]++
	}

	// Active agents.
	agents, lockWarnings, err := activeAgents(tasksDir)
	if err != nil {
		return data, err
	}
	data.agents = agents
	data.warnings = append(data.warnings, lockWarnings...)

	// Presence info.
	presenceMap, presenceErr := messaging.ReadAllPresence(tasksDir)
	if presenceErr != nil {
		data.warnings = append(data.warnings, fmt.Sprintf("could not read agent presence: %v", presenceErr))
	}
	data.presenceMap = presenceMap

	view := queueview.ComputeRunnableBacklogView(tasksDir, idx)

	pauseState, pauseErr := pauseReadFn(tasksDir)
	if pauseErr != nil {
		pauseState = pause.State{
			Active:      true,
			ProblemKind: pause.ProblemUnreadable,
			Problem:     fmt.Sprintf("stat error: %v", pauseErr),
		}
	}
	data.pauseState = pauseState
	if pauseState.ProblemKind != pause.ProblemNone {
		data.warnings = append(data.warnings, pauseState.Problem)
	}

	// Waiting tasks (dependency-blocked) — derived from index and the shared runnable backlog view.
	data.waitingTasks = waitingTasksFromIndex(idx, view.DependencyBlocked)

	// Deferred (conflict-blocked) tasks — derived from the effective runnable backlog view.
	data.deferredDetail = view.Deferred
	deferred := make(map[string]struct{}, len(data.deferredDetail))
	for name := range data.deferredDetail {
		deferred[name] = struct{}{}
	}

	// Runnable count.
	data.runnable = len(view.Runnable)

	// Runnable backlog in priority order — same ordering the host uses to
	// claim the next task, minus conflict-deferred entries.
	runnableSnaps := view.Runnable
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
	data.inProgressTasks = listTasksFromIndex(idx, dirs.InProgress)
	data.readyForReview = listTasksFromIndex(idx, dirs.ReadyReview)
	data.readyToMerge = listTasksFromIndex(idx, dirs.ReadyMerge)
	data.failedTasks = listTasksFromIndex(idx, dirs.Failed)

	// Reverse dependencies — derived from index.
	data.reverseDeps = reverseDepsFromIndex(idx)

	// Completions.
	completions, completionsErr := messaging.ReadAllCompletionDetails(tasksDir)
	if completionsErr != nil {
		data.warnings = append(data.warnings, fmt.Sprintf("could not read completion details: %v", completionsErr))
	}
	data.completions = completions

	// Merge lock.
	data.mergeLockStatus, err = mergeLockState(tasksDir)
	if err != nil {
		data.warnings = append(data.warnings, fmt.Sprintf("could not read merge queue lock: %v", err))
	}

	// Messages — read only the most recent entries to avoid scanning
	// thousands of old files in long-running repos.
	messages, msgWarnings, err := messaging.ReadRecentMessages(tasksDir, statusMessageLimit)
	if err != nil {
		data.warnings = append(data.warnings, fmt.Sprintf("could not read recent messages: %v", err))
	}
	data.warnings = append(data.warnings, msgWarnings...)

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

	// If any active agent's latest progress fell outside the recent-message
	// window, scan older entries to recover it.
	var missingIDs []string
	for _, a := range agents {
		if _, ok := progressByAgent[a.ID]; !ok {
			missingIDs = append(missingIDs, a.ID)
		}
	}
	if len(missingIDs) > 0 {
		olderProgress, olderWarnings, olderErr := messaging.ReadLatestProgressForAgents(tasksDir, missingIDs, statusMessageLimit)
		if olderErr != nil {
			data.warnings = append(data.warnings, fmt.Sprintf("could not read older progress messages: %v", olderErr))
		} else {
			for id, msg := range olderProgress {
				progressByAgent[id] = msg
			}
		}
		data.warnings = append(data.warnings, olderWarnings...)
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

func (d statusData) mergeQueueState() string {
	switch d.mergeLockStatus {
	case lockfile.StatusActive:
		return "active"
	case lockfile.StatusUnknown:
		return "unknown"
	default:
		return "idle"
	}
}

// mergeLockState reads the merge queue lock state without collapsing unreadable
// locks into idle.
func mergeLockState(tasksDir string) (lockfile.Status, error) {
	lockPath := filepath.Join(tasksDir, ".locks", "merge.lock")
	meta, err := lockfile.ReadMetadata(lockPath)
	return meta.Status, err
}
