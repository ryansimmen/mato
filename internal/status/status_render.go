package status

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"

	"mato/internal/frontmatter"
	"mato/internal/pause"
	"mato/internal/queue"
)

// colorSet holds the color formatting functions used by render helpers.
type colorSet struct {
	bold   func(a ...interface{}) string
	green  func(a ...interface{}) string
	red    func(a ...interface{}) string
	yellow func(a ...interface{}) string
	cyan   func(a ...interface{}) string
	dim    func(a ...interface{}) string
}

func newColorSet() colorSet {
	return colorSet{
		bold:   color.New(color.Bold).SprintFunc(),
		green:  color.New(color.FgGreen).SprintFunc(),
		red:    color.New(color.FgRed).SprintFunc(),
		yellow: color.New(color.FgYellow).SprintFunc(),
		cyan:   color.New(color.FgCyan).SprintFunc(),
		dim:    color.New(color.Faint).SprintFunc(),
	}
}

const compactListLimit = 5

func renderVerboseDashboard(w io.Writer, c colorSet, data statusData) {
	renderQueueOverview(w, c, data)
	renderRunnableBacklog(w, c, data)
	renderActiveAgents(w, c, data)
	renderAgentProgress(w, c, data)
	renderInProgressTasks(w, c, data)
	renderReadyForReview(w, c, data)
	renderReadyToMerge(w, c, data)
	renderDependencyBlocked(w, c, data)
	renderConflictDeferred(w, c, data)
	renderFailedTasks(w, c, data)
	renderRecentCompletions(w, c, data)
	renderRecentMessages(w, c, data)
	renderWarnings(w, c, data)
}

func renderCompactDashboard(w io.Writer, c colorSet, data statusData) {
	renderCompactQueueSummary(w, c, data)
	renderCompactAgents(w, c, data)
	renderCompactAttention(w, c, data)
	renderCompactNextUp(w, c, data)
}

func renderCompactQueueSummary(w io.Writer, c colorSet, data statusData) {
	mergeState := c.dim("idle")
	if data.mergeLockActive {
		mergeState = c.yellow("active")
	}

	fmt.Fprintf(w, "%s %s backlog | %s runnable | %s running | %s review | %s merge | %s failed\n",
		c.bold("Queue:"),
		c.green(data.queueCounts[queue.DirBacklog]),
		c.green(data.runnable),
		c.yellow(data.queueCounts[queue.DirInProgress]),
		c.cyan(data.queueCounts[queue.DirReadyReview]),
		c.cyan(data.queueCounts[queue.DirReadyMerge]),
		c.red(data.queueCounts[queue.DirFailed]),
	)
	fmt.Fprintf(w, "%s %s   %s %s\n",
		c.bold("Pause:"), renderPauseState(c, data.pauseState),
		c.bold("Merge queue:"), mergeState,
	)
}

type compactAgentRow struct {
	agentID string
	task    string
	branch  string
	stage   string
	age     string
}

func renderCompactAgents(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s (%d)\n", c.bold("Agents"), len(data.agents))
	if len(data.agents) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}

	rows := compactAgentRows(data)
	show := rows
	if len(show) > compactListLimit {
		show = show[:compactListLimit]
	}
	for _, row := range show {
		parts := []string{c.yellow(row.agentID)}
		if row.task != "" {
			parts = append(parts, row.task)
		}
		if row.branch != "" {
			parts = append(parts, c.dim(row.branch))
		}
		if row.stage != "" {
			parts = append(parts, c.cyan(row.stage))
		}
		if row.age != "" {
			parts = append(parts, c.dim(row.age))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, "  "))
	}
	if len(rows) > compactListLimit {
		fmt.Fprintf(w, "  %s\n", c.dim(fmt.Sprintf("... +%d more", len(rows)-compactListLimit)))
	}
}

func compactAgentRows(data statusData) []compactAgentRow {
	progressByID := make(map[string]progressEntry, len(data.activeProgress))
	for _, progress := range data.activeProgress {
		id := strings.TrimPrefix(progress.displayID, "agent-")
		progressByID[id] = progress
		progressByID[progress.displayID] = progress
	}

	taskByAgent := make(map[string]taskEntry, len(data.inProgressTasks))
	for _, task := range data.inProgressTasks {
		if task.claimedBy == "" {
			continue
		}
		taskByAgent[task.claimedBy] = task
	}

	rows := make([]compactAgentRow, 0, len(data.agents))
	for _, agent := range data.agents {
		row := compactAgentRow{agentID: agent.displayName()}
		if presence, ok := data.presenceMap[agent.ID]; ok && presence.Task != "" {
			row.task = presence.Task
			row.branch = presence.Branch
		}
		if row.task == "" {
			if progress, ok := progressByID[agent.ID]; ok && progress.task != "" {
				row.task = progress.task
			}
		}
		if row.task == "" {
			if task, ok := taskByAgent[agent.ID]; ok {
				row.task = task.name
			}
		}

		if progress, ok := progressByID[agent.ID]; ok {
			row.stage = compactProgressLabel(progress.body)
			row.age = progress.ago
		} else if task, ok := taskByAgent[agent.ID]; ok && !task.claimedAt.IsZero() {
			row.age = formatDuration(time.Now().UTC().Sub(task.claimedAt))
		}
		rows = append(rows, row)
	}

	return rows
}

func compactProgressLabel(body string) string {
	line := strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))
	if strings.HasPrefix(line, "Step:") {
		stage := strings.TrimSpace(strings.TrimPrefix(line, "Step:"))
		if stage != "" {
			return stage
		}
	}
	if line == "" {
		return ""
	}
	if len(line) > 24 {
		return line[:21] + "..."
	}
	return line
}

func renderCompactAttention(w io.Writer, c colorSet, data statusData) {
	orphaned := compactOrphanedInProgressTasks(data)
	hasAttention := len(data.warnings) > 0 || len(data.failedTasks) > 0 || len(data.waitingTasks) > 0 || len(data.deferredDetail) > 0 || len(orphaned) > 0
	if !hasAttention {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Attention"))

	if len(data.warnings) > 0 {
		if len(data.warnings) <= 3 {
			for _, warn := range data.warnings {
				fmt.Fprintf(w, "  %s %s\n", c.yellow("warning:"), warn)
			}
		} else {
			fmt.Fprintf(w, "  %s warnings\n", c.yellow(len(data.warnings)))
		}
	}
	if len(data.failedTasks) > 0 {
		fmt.Fprintf(w, "  %s failed\n", c.red(len(data.failedTasks)))
	}
	if len(data.waitingTasks) > 0 {
		fmt.Fprintf(w, "  %s blocked by dependencies\n", c.yellow(len(data.waitingTasks)))
	}
	if len(data.deferredDetail) > 0 {
		fmt.Fprintf(w, "  %s conflict-deferred\n", c.yellow(len(data.deferredDetail)))
	}
	for _, task := range orphaned {
		fmt.Fprintf(w, "  %s running without active agent\n", c.yellow(task.name))
	}
}

func compactOrphanedInProgressTasks(data statusData) []taskEntry {
	activeAgents := make(map[string]struct{}, len(data.agents))
	for _, agent := range data.agents {
		activeAgents[agent.ID] = struct{}{}
		activeAgents[agent.displayName()] = struct{}{}
	}

	orphaned := make([]taskEntry, 0)
	for _, task := range data.inProgressTasks {
		if task.claimedBy == "" {
			orphaned = append(orphaned, task)
			continue
		}
		if _, ok := activeAgents[task.claimedBy]; !ok {
			orphaned = append(orphaned, task)
		}
	}
	return orphaned
}

func renderCompactNextUp(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Next Up"))
	if len(data.runnableBacklog) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}

	show := data.runnableBacklog
	if len(show) > compactListLimit {
		show = show[:compactListLimit]
	}
	for i, task := range show {
		label := task.name
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", task.name, task.title)
		}
		fmt.Fprintf(w, "  %d. %s\n", i+1, label)
	}
	if len(data.runnableBacklog) > compactListLimit {
		fmt.Fprintf(w, "  %s\n", c.dim(fmt.Sprintf("... +%d more", len(data.runnableBacklog)-compactListLimit)))
	}
}

func renderQueueOverview(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w, c.bold("Queue Overview"))
	fmt.Fprintln(w, c.bold("──────────────"))
	fmt.Fprintf(w, "  backlog:        %s  %s\n", c.green(data.queueCounts[queue.DirBacklog]), c.dim("(total tasks in backlog/)"))
	fmt.Fprintf(w, "  runnable:       %s\n", c.green(data.runnable))
	fmt.Fprintf(w, "  deferred:       %s  %s\n", c.yellow(len(data.deferredDetail)), c.dim("(conflict-blocked, in backlog)"))
	fmt.Fprintf(w, "  blocked:        %s  %s\n", c.dim(len(data.waitingTasks)), c.dim("(dependency-blocked, including misplaced backlog tasks)"))
	fmt.Fprintf(w, "  in-progress:    %s\n", c.yellow(data.queueCounts[queue.DirInProgress]))
	fmt.Fprintf(w, "  ready-review:   %s\n", c.cyan(data.queueCounts[queue.DirReadyReview]))
	fmt.Fprintf(w, "  ready-to-merge: %s\n", c.cyan(data.queueCounts[queue.DirReadyMerge]))
	fmt.Fprintf(w, "  completed:      %s\n", c.green(data.queueCounts[queue.DirCompleted]))
	fmt.Fprintf(w, "  failed:         %s\n", c.red(data.queueCounts[queue.DirFailed]))
	if data.mergeLockActive {
		fmt.Fprintf(w, "  merge queue:    %s\n", c.yellow("active"))
	} else {
		fmt.Fprintf(w, "  merge queue:    %s\n", c.dim("idle"))
	}
	fmt.Fprintf(w, "  pause state:    %s\n", renderPauseState(c, data.pauseState))
}

func renderPauseState(c colorSet, state pause.State) string {
	if !state.Active {
		return c.dim("not paused")
	}
	if state.ProblemKind != pause.ProblemNone {
		return c.yellow(fmt.Sprintf("paused (problem: %s)", state.Problem))
	}
	return c.yellow("paused since " + state.Since.Format(time.RFC3339))
}

func renderRunnableBacklog(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Runnable Backlog (execution order)"))
	fmt.Fprintln(w, c.bold("──────────────────────────────────"))
	if len(data.runnableBacklog) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	for i, task := range data.runnableBacklog {
		label := task.name
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", task.name, task.title)
		}
		fmt.Fprintf(w, "  %d. %s  %s\n", i+1, label, c.dim(fmt.Sprintf("(priority %d)", task.priority)))
	}
}

func renderActiveAgents(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Active Agents"))
	fmt.Fprintln(w, c.bold("─────────────"))
	if len(data.agents) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	for _, agent := range data.agents {
		if p, ok := data.presenceMap[agent.ID]; ok {
			fmt.Fprintf(w, "  %s (PID %d): %s on %s\n", c.yellow(agent.displayName()), agent.PID, p.Task, c.cyan(p.Branch))
		} else {
			fmt.Fprintf(w, "  %s (PID %d)\n", c.yellow(agent.displayName()), agent.PID)
		}
	}
}

func renderAgentProgress(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Current Agent Progress"))
	fmt.Fprintln(w, c.bold("──────────────────────"))
	if len(data.activeProgress) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	for _, p := range data.activeProgress {
		fmt.Fprintf(w, "  %s: %s (%s) — %s ago\n", c.yellow(p.displayID), p.body, p.task, c.dim(p.ago))
	}
}

func renderInProgressTasks(w io.Writer, c colorSet, data statusData) {
	if len(data.inProgressTasks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("In-Progress Tasks"))
	fmt.Fprintln(w, c.bold("─────────────────"))
	now := time.Now().UTC()
	for _, task := range data.inProgressTasks {
		var parts []string
		if task.claimedBy != "" {
			parts = append(parts, fmt.Sprintf("agent %s", task.claimedBy))
		}
		if !task.claimedAt.IsZero() {
			parts = append(parts, formatDuration(now.Sub(task.claimedAt)))
		}
		if task.failureCount > 0 {
			parts = append(parts, fmt.Sprintf("%s/%d retries used", c.red(task.failureCount), task.maxRetries))
		}
		taskID := task.id
		if taskID == "" {
			taskID = frontmatter.TaskFileStem(task.name)
		}
		if waiters, ok := data.reverseDeps[taskID]; ok {
			parts = append(parts, fmt.Sprintf("%d %s waiting", len(waiters), pluralize(len(waiters), "task", "tasks")))
		}

		label := c.yellow(task.name)
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", c.yellow(task.name), task.title)
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s  (%s)\n", label, strings.Join(parts, ", "))
		} else {
			fmt.Fprintf(w, "  %s\n", label)
		}
	}
}

func renderReadyForReview(w io.Writer, c colorSet, data statusData) {
	if len(data.readyForReview) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Ready for Review"))
	fmt.Fprintln(w, c.bold("────────────────"))
	for _, task := range data.readyForReview {
		var parts []string
		if task.title != "" {
			parts = append(parts, task.title)
		}
		if task.branch != "" {
			parts = append(parts, "on "+c.cyan(task.branch))
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "  %s — %s\n", c.cyan(task.name), strings.Join(parts, " "))
		} else {
			fmt.Fprintf(w, "  %s\n", c.cyan(task.name))
		}
	}
}

func renderReadyToMerge(w io.Writer, c colorSet, data statusData) {
	if len(data.readyToMerge) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Ready to Merge"))
	fmt.Fprintln(w, c.bold("──────────────"))
	for _, task := range data.readyToMerge {
		label := c.cyan(task.name)
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", c.cyan(task.name), task.title)
		}
		fmt.Fprintf(w, "  %s  %s\n", label, c.dim(fmt.Sprintf("(priority %d)", task.priority)))
	}
}

func renderDependencyBlocked(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Dependency-Blocked"))
	fmt.Fprintln(w, c.bold("──────────────────"))
	if len(data.waitingTasks) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	for _, task := range data.waitingTasks {
		label := task.Name
		if task.Title != "" {
			label = fmt.Sprintf("%s — %s", task.Name, task.Title)
		}
		state := task.State
		if state == "" {
			state = queue.DirWaiting
		}
		fmt.Fprintf(w, "  %s  %s\n", label, c.dim("("+state+"/)"))
		if len(task.Dependencies) == 0 {
			fmt.Fprintf(w, "    depends on: none\n")
			continue
		}
		depStrs := make([]string, 0, len(task.Dependencies))
		for _, dep := range task.Dependencies {
			symbol := c.red("✗")
			if dep.Status == queue.DirCompleted {
				symbol = c.green("✓")
			}
			depStrs = append(depStrs, fmt.Sprintf("%s (%s %s)", dep.ID, symbol, dep.Status))
		}
		fmt.Fprintf(w, "    depends on: %s\n", strings.Join(depStrs, ", "))
	}
}

func renderConflictDeferred(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Conflict-Deferred (backlog/, excluded from queue)"))
	fmt.Fprintln(w, c.bold("──────────────────────────────────────────────────"))
	if len(data.deferredDetail) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	deferredNames := make([]string, 0, len(data.deferredDetail))
	for name := range data.deferredDetail {
		deferredNames = append(deferredNames, name)
	}
	sort.Strings(deferredNames)
	for _, name := range deferredNames {
		info := data.deferredDetail[name]
		fmt.Fprintf(w, "  %s\n", c.yellow(name))
		fmt.Fprintf(w, "    blocked by: %s (%s/)\n", info.BlockedBy, info.BlockedByDir)
		fmt.Fprintf(w, "    conflicting affects: %s\n", strings.Join(info.ConflictingAffects, ", "))
	}
}

func renderFailedTasks(w io.Writer, c colorSet, data statusData) {
	if len(data.failedTasks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Failed Tasks"))
	fmt.Fprintln(w, c.bold("────────────"))
	for _, task := range data.failedTasks {
		label := c.red(task.name)
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", c.red(task.name), task.title)
		}

		cycleReason := task.lastCycleFailureReason
		terminalReason := task.lastTerminalFailureReason
		failCount := task.failureCount

		if task.cancelled {
			fmt.Fprintf(w, "  %s  (cancelled)\n", label)
		} else if terminalReason != "" && cycleReason != "" {
			info := fmt.Sprintf("structural failure: %s; also: %s", terminalReason, cycleReason)
			fmt.Fprintf(w, "  %s  (%s)\n", label, info)
		} else if terminalReason != "" && failCount > 0 {
			reason := task.lastFailureReason
			info := fmt.Sprintf("structural failure: %s; %d/%d retries used", terminalReason, failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
			fmt.Fprintf(w, "  %s  (%s)\n", label, info)
		} else if terminalReason != "" {
			fmt.Fprintf(w, "  %s  (structural failure: %s)\n", label, terminalReason)
		} else if cycleReason != "" && failCount > 0 {
			reason := task.lastFailureReason
			info := fmt.Sprintf("%s; %d/%d retries used", cycleReason, failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
			fmt.Fprintf(w, "  %s  (%s)\n", label, info)
		} else if cycleReason != "" {
			fmt.Fprintf(w, "  %s  (%s)\n", label, cycleReason)
		} else {
			reason := task.lastFailureReason
			info := fmt.Sprintf("%d/%d retries exhausted", failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
			fmt.Fprintf(w, "  %s  (%s)\n", label, info)
		}
	}
}

func renderRecentCompletions(w io.Writer, c colorSet, data statusData) {
	if len(data.completions) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Recent Completions"))
	fmt.Fprintln(w, c.bold("──────────────────"))
	show := data.completions
	if len(show) > 5 {
		show = show[:5]
	}
	now := time.Now().UTC()
	for _, comp := range show {
		ago := formatDuration(now.Sub(comp.MergedAt))
		shortSHA := comp.CommitSHA
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		label := c.green(comp.TaskFile)
		if comp.Title != "" {
			label = fmt.Sprintf("%s — %s", c.green(comp.TaskFile), comp.Title)
		}
		fmt.Fprintf(w, "  %s  %s\n", label, c.dim(fmt.Sprintf("(merged %s ago, %d %s, %s)", ago, len(comp.FilesChanged), pluralize(len(comp.FilesChanged), "file", "files"), shortSHA)))
	}
}

func renderRecentMessages(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Recent Messages"))
	fmt.Fprintln(w, c.bold("───────────────"))
	if len(data.recentMessages) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	for i := len(data.recentMessages) - 1; i >= 0; i-- {
		msg := data.recentMessages[i]
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
		fmt.Fprintf(w, "  %s %s: %s\n", c.dim("["+msg.SentAt.Local().Format("15:04:05")+"]"), c.yellow(from), line)
	}
}

// renderWarnings prints any non-fatal warnings collected during data gathering.
func renderWarnings(w io.Writer, c colorSet, data statusData) {
	if len(data.warnings) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold(c.yellow("Warnings")))
	fmt.Fprintln(w, c.bold(c.yellow("────────")))
	for _, warn := range data.warnings {
		fmt.Fprintf(w, "  %s %s\n", c.yellow("⚠"), warn)
	}
}
