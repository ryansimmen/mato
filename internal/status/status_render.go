package status

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/pause"
	"mato/internal/queue"
	"mato/internal/ui"
)

// colorSet is an alias for the shared ui.ColorSet used by render helpers.
type colorSet = ui.ColorSet

func newColorSet() colorSet {
	return ui.NewColorSet()
}

const compactListLimit = 5

// minTruncWidth is the smallest budget allowed when clamping
// width-based truncation on very narrow terminals.
const minTruncWidth = 6

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
	mergeState := c.Dim("idle")
	if data.mergeLockActive {
		mergeState = c.Yellow("active")
	}

	fmt.Fprintf(w, "%s %s backlog | %s runnable | %s running | %s review | %s merge | %s failed\n",
		c.Bold("Queue:"),
		c.Green(data.queueCounts[queue.DirBacklog]),
		c.Green(data.runnable),
		c.Yellow(data.queueCounts[queue.DirInProgress]),
		c.Cyan(data.queueCounts[queue.DirReadyReview]),
		c.Cyan(data.queueCounts[queue.DirReadyMerge]),
		c.Red(data.queueCounts[queue.DirFailed]),
	)
	fmt.Fprintf(w, "%s %s   %s %s\n",
		c.Bold("Pause:"), renderPauseState(c, data.pauseState),
		c.Bold("Merge queue:"), mergeState,
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
	fmt.Fprintf(w, "%s (%d)\n", c.Bold("Agents"), len(data.agents))
	if len(data.agents) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}

	termWidth := ui.TermWidth()
	rows := compactAgentRows(data)
	show := rows
	if len(show) > compactListLimit {
		show = show[:compactListLimit]
	}
	for _, row := range show {
		parts := []string{c.Yellow(row.agentID)}
		if row.task != "" {
			parts = append(parts, row.task)
		}
		if row.branch != "" {
			parts = append(parts, c.Dim(row.branch))
		}
		if row.stage != "" {
			parts = append(parts, c.Cyan(row.stage))
		}
		if row.age != "" {
			parts = append(parts, c.Dim(row.age))
		}
		line := strings.Join(parts, "  ")

		if termWidth > 0 {
			// Visible width: 2 (indent) + agentID + separators + fields.
			visibleLen := 2 + len(row.agentID)
			if row.task != "" {
				visibleLen += 2 + len(row.task)
			}
			if row.branch != "" {
				visibleLen += 2 + len(row.branch)
			}
			if row.stage != "" {
				visibleLen += 2 + len(row.stage)
			}
			if row.age != "" {
				visibleLen += 2 + len(row.age)
			}
			if visibleLen > termWidth {
				budgetForFields := termWidth - 2 - len(row.agentID)
				if row.age != "" {
					budgetForFields -= 2 + len(row.age)
				}
				if row.stage != "" {
					budgetForFields -= 2 + len(row.stage)
				}
				taskBudget := budgetForFields
				branchBudget := 0
				if row.task != "" && row.branch != "" {
					taskBudget = (budgetForFields - 4) * 2 / 3
					branchBudget = budgetForFields - 4 - taskBudget
					if branchBudget < minTruncWidth {
						// Not enough room for branch; drop it and
						// give all remaining space to task.
						branchBudget = 0
						taskBudget = budgetForFields - 2
					}
				} else if row.task != "" {
					taskBudget = budgetForFields - 2
				}
				if taskBudget < 0 {
					taskBudget = 0
				}
				parts = []string{c.Yellow(row.agentID)}
				if row.task != "" && taskBudget > 0 {
					parts = append(parts, ui.Truncate(row.task, taskBudget))
				}
				if row.branch != "" && branchBudget > 0 {
					parts = append(parts, c.Dim(ui.Truncate(row.branch, branchBudget)))
				}
				if row.stage != "" {
					parts = append(parts, c.Cyan(row.stage))
				}
				if row.age != "" {
					parts = append(parts, c.Dim(row.age))
				}
				line = strings.Join(parts, "  ")
			}
		}
		fmt.Fprintf(w, "  %s\n", line)
	}
	if len(rows) > compactListLimit {
		fmt.Fprintf(w, "  %s\n", c.Dim(fmt.Sprintf("... +%d more", len(rows)-compactListLimit)))
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
	fmt.Fprintln(w, c.Bold("Attention"))

	if len(data.warnings) > 0 {
		if len(data.warnings) <= 3 {
			for _, warn := range data.warnings {
				fmt.Fprintf(w, "  %s %s\n", c.Yellow("warning:"), warn)
			}
		} else {
			fmt.Fprintf(w, "  %s warnings\n", c.Yellow(len(data.warnings)))
		}
	}
	if len(data.failedTasks) > 0 {
		fmt.Fprintf(w, "  %s failed\n", c.Red(len(data.failedTasks)))
	}
	if len(data.waitingTasks) > 0 {
		fmt.Fprintf(w, "  %s blocked by dependencies\n", c.Yellow(len(data.waitingTasks)))
	}
	if len(data.deferredDetail) > 0 {
		fmt.Fprintf(w, "  %s conflict-deferred\n", c.Yellow(len(data.deferredDetail)))
	}
	for _, task := range orphaned {
		fmt.Fprintf(w, "  %s running without active agent\n", c.Yellow(task.name))
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
	fmt.Fprintln(w, c.Bold("Next Up"))
	if len(data.runnableBacklog) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}

	termWidth := ui.TermWidth()
	show := data.runnableBacklog
	if len(show) > compactListLimit {
		show = show[:compactListLimit]
	}
	for i, task := range show {
		label := task.name
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", task.name, task.title)
		}
		if termWidth > 0 {
			prefix := fmt.Sprintf("  %d. ", i+1)
			maxLabel := termWidth - len(prefix)
			if maxLabel < minTruncWidth {
				maxLabel = minTruncWidth
			}
			label = ui.Truncate(label, maxLabel)
		}
		fmt.Fprintf(w, "  %d. %s\n", i+1, label)
	}
	if len(data.runnableBacklog) > compactListLimit {
		fmt.Fprintf(w, "  %s\n", c.Dim(fmt.Sprintf("... +%d more", len(data.runnableBacklog)-compactListLimit)))
	}
}

func renderQueueOverview(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w, c.Bold("Queue Overview"))
	fmt.Fprintln(w, c.Bold("──────────────"))
	fmt.Fprintf(w, "  backlog:        %s  %s\n", c.Green(data.queueCounts[queue.DirBacklog]), c.Dim("(total tasks in backlog/)"))
	fmt.Fprintf(w, "  runnable:       %s\n", c.Green(data.runnable))
	fmt.Fprintf(w, "  deferred:       %s  %s\n", c.Yellow(len(data.deferredDetail)), c.Dim("(conflict-blocked, in backlog)"))
	fmt.Fprintf(w, "  blocked:        %s  %s\n", c.Dim(len(data.waitingTasks)), c.Dim("(dependency-blocked, including misplaced backlog tasks)"))
	fmt.Fprintf(w, "  in-progress:    %s\n", c.Yellow(data.queueCounts[queue.DirInProgress]))
	fmt.Fprintf(w, "  ready-review:   %s\n", c.Cyan(data.queueCounts[queue.DirReadyReview]))
	fmt.Fprintf(w, "  ready-to-merge: %s\n", c.Cyan(data.queueCounts[queue.DirReadyMerge]))
	fmt.Fprintf(w, "  completed:      %s\n", c.Green(data.queueCounts[queue.DirCompleted]))
	fmt.Fprintf(w, "  failed:         %s\n", c.Red(data.queueCounts[queue.DirFailed]))
	if data.mergeLockActive {
		fmt.Fprintf(w, "  merge queue:    %s\n", c.Yellow("active"))
	} else {
		fmt.Fprintf(w, "  merge queue:    %s\n", c.Dim("idle"))
	}
	fmt.Fprintf(w, "  pause state:    %s\n", renderPauseState(c, data.pauseState))
}

func renderPauseState(c colorSet, state pause.State) string {
	if !state.Active {
		return c.Dim("not paused")
	}
	if state.ProblemKind != pause.ProblemNone {
		return c.Yellow(fmt.Sprintf("paused (problem: %s)", state.Problem))
	}
	return c.Yellow("paused since " + state.Since.Format(time.RFC3339))
}

func renderRunnableBacklog(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Runnable Backlog (execution order)"))
	fmt.Fprintln(w, c.Bold("──────────────────────────────────"))
	if len(data.runnableBacklog) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}
	termWidth := ui.TermWidth()
	for i, task := range data.runnableBacklog {
		label := task.name
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", task.name, task.title)
		}
		suffix := fmt.Sprintf("(priority %d)", task.priority)
		if termWidth > 0 {
			prefix := fmt.Sprintf("  %d. ", i+1)
			maxLabel := termWidth - len(prefix) - 2 - len(suffix)
			if maxLabel < minTruncWidth {
				maxLabel = minTruncWidth
			}
			label = ui.Truncate(label, maxLabel)
		}
		fmt.Fprintf(w, "  %d. %s  %s\n", i+1, label, c.Dim(suffix))
	}
}

func renderActiveAgents(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Active Agents"))
	fmt.Fprintln(w, c.Bold("─────────────"))
	if len(data.agents) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}
	termWidth := ui.TermWidth()
	for _, agent := range data.agents {
		if p, ok := data.presenceMap[agent.ID]; ok {
			task := p.Task
			branch := p.Branch
			if termWidth > 0 {
				name := agent.displayName()
				pidStr := fmt.Sprintf("%d", agent.PID)
				// Fixed prefix: "  " + name + " (PID " + pid + "): "
				fixedPrefix := 2 + len(name) + 6 + len(pidStr) + 3
				avail := termWidth - fixedPrefix

				if avail < minTruncWidth {
					// Not enough room for both fields; drop branch.
					if avail > 0 {
						task = ui.Truncate(task, avail)
					} else {
						task = ""
					}
					branch = ""
				} else {
					needed := len(task) + 4 + len(branch)
					if needed > avail {
						// " on " costs 4 chars.
						branchBudget := (avail - 4) / 3
						taskBudget := avail - 4 - branchBudget
						if branchBudget < minTruncWidth {
							// Drop branch, give all space to task.
							taskBudget = avail
							task = ui.Truncate(task, taskBudget)
							branch = ""
						} else {
							task = ui.Truncate(task, taskBudget)
							branch = ui.Truncate(branch, branchBudget)
						}
					}
				}
			}
			if branch != "" {
				fmt.Fprintf(w, "  %s (PID %d): %s on %s\n", c.Yellow(agent.displayName()), agent.PID, task, c.Cyan(branch))
			} else if task != "" {
				fmt.Fprintf(w, "  %s (PID %d): %s\n", c.Yellow(agent.displayName()), agent.PID, task)
			} else {
				fmt.Fprintf(w, "  %s (PID %d)\n", c.Yellow(agent.displayName()), agent.PID)
			}
		} else {
			fmt.Fprintf(w, "  %s (PID %d)\n", c.Yellow(agent.displayName()), agent.PID)
		}
	}
}

func renderAgentProgress(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Current Agent Progress"))
	fmt.Fprintln(w, c.Bold("──────────────────────"))
	if len(data.activeProgress) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}
	termWidth := ui.TermWidth()
	for _, p := range data.activeProgress {
		body := p.body
		if termWidth > 0 {
			prefixLen := 2 + len(p.displayID) + 2
			suffixLen := 2 + len(p.task) + 5 + len(p.ago) + 4
			maxBody := termWidth - prefixLen - suffixLen
			if maxBody < minTruncWidth {
				maxBody = minTruncWidth
			}
			body = ui.Truncate(body, maxBody)
		}
		fmt.Fprintf(w, "  %s: %s (%s) — %s ago\n", c.Yellow(p.displayID), body, p.task, c.Dim(p.ago))
	}
}

func renderInProgressTasks(w io.Writer, c colorSet, data statusData) {
	if len(data.inProgressTasks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("In-Progress Tasks"))
	fmt.Fprintln(w, c.Bold("─────────────────"))
	termWidth := ui.TermWidth()
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
			parts = append(parts, fmt.Sprintf("%s/%d retries used", c.Red(task.failureCount), task.maxRetries))
		}
		taskID := task.id
		if taskID == "" {
			taskID = frontmatter.TaskFileStem(task.name)
		}
		if waiters, ok := data.reverseDeps[taskID]; ok {
			parts = append(parts, fmt.Sprintf("%d %s waiting", len(waiters), pluralize(len(waiters), "task", "tasks")))
		}

		labelText := task.name
		if task.title != "" {
			labelText = task.name + " — " + task.title
		}
		if len(parts) > 0 {
			suffix := "  (" + strings.Join(parts, ", ") + ")"
			if termWidth > 0 {
				maxLabel := termWidth - 2 - len(suffix)
				if maxLabel < minTruncWidth {
					maxLabel = minTruncWidth
				}
				labelText = ui.Truncate(labelText, maxLabel)
			}
			fmt.Fprintf(w, "  %s%s\n", c.Yellow(labelText), suffix)
		} else {
			if termWidth > 0 {
				maxLabel := termWidth - 2
				if maxLabel < minTruncWidth {
					maxLabel = minTruncWidth
				}
				labelText = ui.Truncate(labelText, maxLabel)
			}
			fmt.Fprintf(w, "  %s\n", c.Yellow(labelText))
		}
	}
}

func renderReadyForReview(w io.Writer, c colorSet, data statusData) {
	if len(data.readyForReview) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Ready for Review"))
	fmt.Fprintln(w, c.Bold("────────────────"))
	termWidth := ui.TermWidth()
	for _, task := range data.readyForReview {
		var parts []string
		if task.title != "" {
			parts = append(parts, task.title)
		}
		if task.branch != "" {
			parts = append(parts, "on "+c.Cyan(task.branch))
		}
		if len(parts) > 0 {
			detail := strings.Join(parts, " ")
			if termWidth > 0 {
				prefixLen := 2 + len(task.name) + 3
				maxDetail := termWidth - prefixLen
				if maxDetail < minTruncWidth {
					maxDetail = minTruncWidth
				}
				detail = ui.Truncate(detail, maxDetail)
			}
			fmt.Fprintf(w, "  %s — %s\n", c.Cyan(task.name), detail)
		} else {
			fmt.Fprintf(w, "  %s\n", c.Cyan(task.name))
		}
	}
}

func renderReadyToMerge(w io.Writer, c colorSet, data statusData) {
	if len(data.readyToMerge) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Ready to Merge"))
	fmt.Fprintln(w, c.Bold("──────────────"))
	termWidth := ui.TermWidth()
	for _, task := range data.readyToMerge {
		labelText := task.name
		if task.title != "" {
			labelText = task.name + " — " + task.title
		}
		suffix := fmt.Sprintf("(priority %d)", task.priority)
		if termWidth > 0 {
			maxLabel := termWidth - 2 - 2 - len(suffix)
			if maxLabel < minTruncWidth {
				maxLabel = minTruncWidth
			}
			labelText = ui.Truncate(labelText, maxLabel)
		}
		fmt.Fprintf(w, "  %s  %s\n", c.Cyan(labelText), c.Dim(suffix))
	}
}

func renderDependencyBlocked(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Dependency-Blocked"))
	fmt.Fprintln(w, c.Bold("──────────────────"))
	if len(data.waitingTasks) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}
	termWidth := ui.TermWidth()
	for _, task := range data.waitingTasks {
		labelText := task.Name
		if task.Title != "" {
			labelText = task.Name + " — " + task.Title
		}
		state := task.State
		if state == "" {
			state = queue.DirWaiting
		}
		suffix := "(" + state + "/)"
		if termWidth > 0 {
			maxLabel := termWidth - 2 - 2 - len(suffix)
			if maxLabel < minTruncWidth {
				maxLabel = minTruncWidth
			}
			labelText = ui.Truncate(labelText, maxLabel)
		}
		fmt.Fprintf(w, "  %s  %s\n", labelText, c.Dim(suffix))
		if len(task.Dependencies) == 0 {
			fmt.Fprintf(w, "    depends on: none\n")
			continue
		}
		depStrs := make([]string, 0, len(task.Dependencies))
		for _, dep := range task.Dependencies {
			symbol := c.Red("✗")
			if dep.Status == queue.DirCompleted {
				symbol = c.Green("✓")
			}
			depStrs = append(depStrs, fmt.Sprintf("%s (%s %s)", dep.ID, symbol, dep.Status))
		}
		depLine := strings.Join(depStrs, ", ")
		if termWidth > 0 {
			maxDep := termWidth - 19 // "    depends on: " = 16, plus margin
			if maxDep < minTruncWidth {
				maxDep = minTruncWidth
			}
			depLine = ui.Truncate(depLine, maxDep)
		}
		fmt.Fprintf(w, "    depends on: %s\n", depLine)
	}
}

func renderConflictDeferred(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Conflict-Deferred (backlog/, excluded from queue)"))
	fmt.Fprintln(w, c.Bold("──────────────────────────────────────────────────"))
	if len(data.deferredDetail) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}
	termWidth := ui.TermWidth()
	deferredNames := make([]string, 0, len(data.deferredDetail))
	for name := range data.deferredDetail {
		deferredNames = append(deferredNames, name)
	}
	sort.Strings(deferredNames)
	for _, name := range deferredNames {
		info := data.deferredDetail[name]
		fmt.Fprintf(w, "  %s\n", c.Yellow(name))
		blockedLine := fmt.Sprintf("%s (%s/)", info.BlockedBy, info.BlockedByDir)
		affectsLine := strings.Join(info.ConflictingAffects, ", ")
		if termWidth > 0 {
			maxBlocked := termWidth - 18 // "    blocked by: " = 16 + margin
			if maxBlocked < minTruncWidth {
				maxBlocked = minTruncWidth
			}
			blockedLine = ui.Truncate(blockedLine, maxBlocked)
			maxAffects := termWidth - 26 // "    conflicting affects: " = 24 + margin
			if maxAffects < minTruncWidth {
				maxAffects = minTruncWidth
			}
			affectsLine = ui.Truncate(affectsLine, maxAffects)
		}
		fmt.Fprintf(w, "    blocked by: %s\n", blockedLine)
		fmt.Fprintf(w, "    conflicting affects: %s\n", affectsLine)
	}
}

func renderFailedTasks(w io.Writer, c colorSet, data statusData) {
	if len(data.failedTasks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Failed Tasks"))
	fmt.Fprintln(w, c.Bold("────────────"))
	termWidth := ui.TermWidth()
	for _, task := range data.failedTasks {
		labelText := task.name
		if task.title != "" {
			labelText = task.name + " — " + task.title
		}

		cycleReason := task.lastCycleFailureReason
		terminalReason := task.lastTerminalFailureReason
		failCount := task.failureCount

		var info string
		if task.cancelled {
			info = "cancelled"
		} else if terminalReason != "" && cycleReason != "" {
			info = fmt.Sprintf("structural failure: %s; also: %s", terminalReason, cycleReason)
		} else if terminalReason != "" && failCount > 0 {
			reason := task.lastFailureReason
			info = fmt.Sprintf("structural failure: %s; %d/%d retries used", terminalReason, failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
		} else if terminalReason != "" {
			info = fmt.Sprintf("structural failure: %s", terminalReason)
		} else if cycleReason != "" && failCount > 0 {
			reason := task.lastFailureReason
			info = fmt.Sprintf("%s; %d/%d retries used", cycleReason, failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
		} else if cycleReason != "" {
			info = cycleReason
		} else {
			reason := task.lastFailureReason
			info = fmt.Sprintf("%d/%d retries exhausted", failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
		}

		suffix := "  (" + info + ")"
		if termWidth > 0 {
			maxLabel := termWidth - 2 - len(suffix)
			if maxLabel < minTruncWidth {
				maxLabel = minTruncWidth
			}
			labelText = ui.Truncate(labelText, maxLabel)
		}
		fmt.Fprintf(w, "  %s%s\n", c.Red(labelText), suffix)
	}
}

func renderRecentCompletions(w io.Writer, c colorSet, data statusData) {
	if len(data.completions) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Recent Completions"))
	fmt.Fprintln(w, c.Bold("──────────────────"))
	show := data.completions
	if len(show) > 5 {
		show = show[:5]
	}
	termWidth := ui.TermWidth()
	now := time.Now().UTC()
	for _, comp := range show {
		ago := formatDuration(now.Sub(comp.MergedAt))
		shortSHA := comp.CommitSHA
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		suffix := fmt.Sprintf("(merged %s ago, %d %s, %s)", ago, len(comp.FilesChanged), pluralize(len(comp.FilesChanged), "file", "files"), shortSHA)
		labelText := comp.TaskFile
		if comp.Title != "" {
			labelText = comp.TaskFile + " — " + comp.Title
		}
		if termWidth > 0 {
			maxLabel := termWidth - 2 - 2 - len(suffix)
			if maxLabel < minTruncWidth {
				maxLabel = minTruncWidth
			}
			labelText = ui.Truncate(labelText, maxLabel)
		}
		label := c.Green(labelText)
		fmt.Fprintf(w, "  %s  %s\n", label, c.Dim(suffix))
	}
}

func renderRecentMessages(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold("Recent Messages"))
	fmt.Fprintln(w, c.Bold("───────────────"))
	if len(data.recentMessages) == 0 {
		fmt.Fprintln(w, c.Dim("  (none)"))
		return
	}
	termWidth := ui.TermWidth()
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
		if termWidth > 0 {
			timeStr := msg.SentAt.Local().Format("15:04:05")
			prefixLen := 2 + 1 + len(timeStr) + 2 + len(from) + 2
			maxLine := termWidth - prefixLen
			if maxLine < minTruncWidth {
				maxLine = minTruncWidth
			}
			line = ui.Truncate(line, maxLine)
		}
		fmt.Fprintf(w, "  %s %s: %s\n", c.Dim("["+msg.SentAt.Local().Format("15:04:05")+"]"), c.Yellow(from), line)
	}
}

// renderWarnings prints any non-fatal warnings collected during data gathering.
func renderWarnings(w io.Writer, c colorSet, data statusData) {
	if len(data.warnings) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.Bold(c.Yellow("Warnings")))
	fmt.Fprintln(w, c.Bold(c.Yellow("────────")))
	for _, warn := range data.warnings {
		fmt.Fprintf(w, "  %s %s\n", c.Yellow("⚠"), warn)
	}
}
