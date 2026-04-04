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

type renderWriter struct {
	w   io.Writer
	err error
}

func (rw *renderWriter) print(args ...any) {
	if rw.err != nil {
		return
	}
	_, rw.err = fmt.Fprint(rw.w, args...)
}

func (rw *renderWriter) printf(format string, args ...any) {
	if rw.err != nil {
		return
	}
	_, rw.err = fmt.Fprintf(rw.w, format, args...)
}

func (rw *renderWriter) println(args ...any) {
	if rw.err != nil {
		return
	}
	_, rw.err = fmt.Fprintln(rw.w, args...)
}

func renderVerboseDashboard(w io.Writer, c colorSet, data statusData) error {
	if err := renderQueueOverview(w, c, data); err != nil {
		return err
	}
	if err := renderRunnableBacklog(w, c, data); err != nil {
		return err
	}
	if err := renderActiveAgents(w, c, data); err != nil {
		return err
	}
	if err := renderAgentProgress(w, c, data); err != nil {
		return err
	}
	if err := renderInProgressTasks(w, c, data); err != nil {
		return err
	}
	if err := renderReadyForReview(w, c, data); err != nil {
		return err
	}
	if err := renderReadyToMerge(w, c, data); err != nil {
		return err
	}
	if err := renderDependencyBlocked(w, c, data); err != nil {
		return err
	}
	if err := renderConflictDeferred(w, c, data); err != nil {
		return err
	}
	if err := renderFailedTasks(w, c, data); err != nil {
		return err
	}
	if err := renderRecentCompletions(w, c, data); err != nil {
		return err
	}
	if err := renderRecentMessages(w, c, data); err != nil {
		return err
	}
	return renderWarnings(w, c, data)
}

func renderCompactDashboard(w io.Writer, c colorSet, data statusData) error {
	if err := renderCompactQueueSummary(w, c, data); err != nil {
		return err
	}
	if err := renderCompactAgents(w, c, data); err != nil {
		return err
	}
	if err := renderCompactAttention(w, c, data); err != nil {
		return err
	}
	return renderCompactNextUp(w, c, data)
}

func renderCompactQueueSummary(w io.Writer, c colorSet, data statusData) error {
	mergeState := c.Dim("idle")
	if data.mergeLockActive {
		mergeState = c.Yellow("active")
	}

	rw := renderWriter{w: w}
	rw.printf("%s %s backlog | %s runnable | %s running | %s review | %s merge | %s failed\n",
		c.Bold("Queue:"),
		c.Green(data.queueCounts[queue.DirBacklog]),
		c.Green(data.runnable),
		c.Yellow(data.queueCounts[queue.DirInProgress]),
		c.Cyan(data.queueCounts[queue.DirReadyReview]),
		c.Cyan(data.queueCounts[queue.DirReadyMerge]),
		c.Red(data.queueCounts[queue.DirFailed]),
	)
	rw.printf("%s %s   %s %s\n",
		c.Bold("Pause:"), renderPauseState(c, data.pauseState),
		c.Bold("Merge queue:"), mergeState,
	)
	return rw.err
}

type compactAgentRow struct {
	agentID string
	task    string
	branch  string
	stage   string
	age     string
}

func renderCompactAgents(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.printf("%s (%d)\n", c.Bold("Agents"), len(data.agents))
	if len(data.agents) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
	}

	termWidth := ui.TermWidth()

	// On extremely narrow terminals where even the fixed two-space
	// indent would overflow, shrink the indent so data lines stay
	// within termWidth.
	indentN := 2
	if termWidth > 0 && termWidth <= 2 {
		indentN = termWidth - 1
		if indentN < 0 {
			indentN = 0
		}
	}
	indent := strings.Repeat(" ", indentN)

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
			// Visible width: indent + agentID + separators + fields.
			visibleLen := indentN + len(row.agentID)
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
				budget := termWidth - indentN - len(row.agentID)

				// Truncate the agent ID itself when it alone
				// overflows the terminal width.
				displayID := row.agentID
				if budget < 0 {
					idBudget := termWidth - indentN
					if idBudget < 1 {
						idBudget = 1
					}
					displayID = ui.Truncate(row.agentID, idBudget)
					budget = 0
				}

				// Reserve space for stage and age; drop the
				// least important suffix fields first when there
				// is not enough room.
				showAge := row.age != ""
				showStage := row.stage != ""

				reserved := 0
				if showAge {
					reserved += 2 + len(row.age)
				}
				if showStage {
					reserved += 2 + len(row.stage)
				}
				budgetForTB := budget - reserved
				if budgetForTB < 0 && showAge {
					showAge = false
					reserved -= 2 + len(row.age)
					budgetForTB = budget - reserved
				}
				if budgetForTB < 0 && showStage {
					showStage = false
					reserved -= 2 + len(row.stage)
					budgetForTB = budget - reserved
				}

				taskBudget := budgetForTB
				branchBudget := 0
				if row.task != "" && row.branch != "" {
					taskBudget = (budgetForTB - 4) * 2 / 3
					branchBudget = budgetForTB - 4 - taskBudget
					if branchBudget < minTruncWidth {
						// Not enough room for branch; drop it and
						// give all remaining space to task.
						branchBudget = 0
						taskBudget = budgetForTB - 2
					}
				} else if row.task != "" {
					taskBudget = budgetForTB - 2
				}
				if taskBudget < 0 {
					taskBudget = 0
				}
				parts = []string{c.Yellow(displayID)}
				if row.task != "" && taskBudget > 0 {
					parts = append(parts, ui.Truncate(row.task, taskBudget))
				}
				if row.branch != "" && branchBudget > 0 {
					parts = append(parts, c.Dim(ui.Truncate(row.branch, branchBudget)))
				}
				if showStage {
					parts = append(parts, c.Cyan(row.stage))
				}
				if showAge {
					parts = append(parts, c.Dim(row.age))
				}
				line = strings.Join(parts, "  ")
			}
		}
		rw.printf("%s%s\n", indent, line)
	}
	if len(rows) > compactListLimit {
		summary := fmt.Sprintf("... +%d more", len(rows)-compactListLimit)
		if termWidth > 0 {
			maxSummary := termWidth - indentN
			if maxSummary < 1 {
				maxSummary = 1
			}
			summary = ui.Truncate(summary, maxSummary)
		}
		rw.printf("%s%s\n", indent, c.Dim(summary))
	}
	return rw.err
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

func renderCompactAttention(w io.Writer, c colorSet, data statusData) error {
	orphaned := compactOrphanedInProgressTasks(data)
	hasAttention := len(data.warnings) > 0 || len(data.failedTasks) > 0 || len(data.waitingTasks) > 0 || len(data.deferredDetail) > 0 || len(orphaned) > 0
	if !hasAttention {
		return nil
	}

	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Attention"))

	if len(data.warnings) > 0 {
		if len(data.warnings) <= 3 {
			for _, warn := range data.warnings {
				rw.printf("  %s %s\n", c.Yellow("warning:"), warn)
			}
		} else {
			rw.printf("  %s warnings\n", c.Yellow(len(data.warnings)))
		}
	}
	if len(data.failedTasks) > 0 {
		rw.printf("  %s failed\n", c.Red(len(data.failedTasks)))
	}
	if len(data.waitingTasks) > 0 {
		rw.printf("  %s blocked by dependencies\n", c.Yellow(len(data.waitingTasks)))
	}
	if len(data.deferredDetail) > 0 {
		rw.printf("  %s conflict-deferred\n", c.Yellow(len(data.deferredDetail)))
	}
	for _, task := range orphaned {
		rw.printf("  %s running without active agent\n", c.Yellow(task.name))
	}
	return rw.err
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

func renderCompactNextUp(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Next Up"))
	if len(data.runnableBacklog) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
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
		rw.printf("  %d. %s\n", i+1, label)
	}
	if len(data.runnableBacklog) > compactListLimit {
		rw.printf("  %s\n", c.Dim(fmt.Sprintf("... +%d more", len(data.runnableBacklog)-compactListLimit)))
	}
	return rw.err
}

func renderQueueOverview(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println(c.Bold("Queue Overview"))
	rw.println(c.Bold("──────────────"))
	rw.printf("  backlog:        %s  %s\n", c.Green(data.queueCounts[queue.DirBacklog]), c.Dim("(total tasks in backlog/)"))
	rw.printf("  runnable:       %s\n", c.Green(data.runnable))
	rw.printf("  deferred:       %s  %s\n", c.Yellow(len(data.deferredDetail)), c.Dim("(conflict-blocked, in backlog)"))
	rw.printf("  blocked:        %s  %s\n", c.Dim(len(data.waitingTasks)), c.Dim("(dependency-blocked, including misplaced backlog tasks)"))
	rw.printf("  in-progress:    %s\n", c.Yellow(data.queueCounts[queue.DirInProgress]))
	rw.printf("  ready-review:   %s\n", c.Cyan(data.queueCounts[queue.DirReadyReview]))
	rw.printf("  ready-to-merge: %s\n", c.Cyan(data.queueCounts[queue.DirReadyMerge]))
	rw.printf("  completed:      %s\n", c.Green(data.queueCounts[queue.DirCompleted]))
	rw.printf("  failed:         %s\n", c.Red(data.queueCounts[queue.DirFailed]))
	if data.mergeLockActive {
		rw.printf("  merge queue:    %s\n", c.Yellow("active"))
	} else {
		rw.printf("  merge queue:    %s\n", c.Dim("idle"))
	}
	rw.printf("  pause state:    %s\n", renderPauseState(c, data.pauseState))
	return rw.err
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

func renderRunnableBacklog(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Runnable Backlog (execution order)"))
	rw.println(c.Bold("──────────────────────────────────"))
	if len(data.runnableBacklog) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
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
		rw.printf("  %d. %s  %s\n", i+1, label, c.Dim(suffix))
	}
	return rw.err
}

func renderActiveAgents(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Active Agents"))
	rw.println(c.Bold("─────────────"))
	if len(data.agents) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
	}
	termWidth := ui.TermWidth()
	for _, agent := range data.agents {
		name := agent.displayName()
		if p, ok := data.presenceMap[agent.ID]; ok {
			task := p.Task
			branch := p.Branch
			if termWidth > 0 {
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

				// When task and branch are dropped, the identity
				// line itself may still overflow. Truncate the
				// display name so the line fits within termWidth.
				if task == "" && branch == "" {
					// "  name (PID pid)" = 2 + name + 6 + pidStr + 1
					identLen := 2 + len(name) + 6 + len(pidStr) + 1
					if identLen > termWidth {
						nameBudget := termWidth - 2 - 6 - len(pidStr) - 1
						if nameBudget > 0 {
							name = ui.Truncate(name, nameBudget)
						} else {
							// Not even PID fits; truncate name to
							// fill remaining width after indent.
							nb := termWidth - 2
							if nb > 0 {
								name = ui.Truncate(name, nb)
							} else {
								name = ""
							}
							rw.printf("  %s\n", c.Yellow(name))
							continue
						}
					}
				}
			}
			if branch != "" {
				rw.printf("  %s (PID %d): %s on %s\n", c.Yellow(name), agent.PID, task, c.Cyan(branch))
			} else if task != "" {
				rw.printf("  %s (PID %d): %s\n", c.Yellow(name), agent.PID, task)
			} else {
				rw.printf("  %s (PID %d)\n", c.Yellow(name), agent.PID)
			}
		} else {
			// No presence info; show identity only.
			if termWidth > 0 {
				pidStr := fmt.Sprintf("%d", agent.PID)
				identLen := 2 + len(name) + 6 + len(pidStr) + 1
				if identLen > termWidth {
					nameBudget := termWidth - 2 - 6 - len(pidStr) - 1
					if nameBudget > 0 {
						name = ui.Truncate(name, nameBudget)
					} else {
						nb := termWidth - 2
						if nb > 0 {
							name = ui.Truncate(name, nb)
						} else {
							name = ""
						}
						rw.printf("  %s\n", c.Yellow(name))
						continue
					}
				}
			}
			rw.printf("  %s (PID %d)\n", c.Yellow(name), agent.PID)
		}
	}
	return rw.err
}

func renderAgentProgress(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Current Agent Progress"))
	rw.println(c.Bold("──────────────────────"))
	if len(data.activeProgress) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
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
		rw.printf("  %s: %s (%s) — %s ago\n", c.Yellow(p.displayID), body, p.task, c.Dim(p.ago))
	}
	return rw.err
}

func renderInProgressTasks(w io.Writer, c colorSet, data statusData) error {
	if len(data.inProgressTasks) == 0 {
		return nil
	}
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("In-Progress Tasks"))
	rw.println(c.Bold("─────────────────"))
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
			rw.printf("  %s%s\n", c.Yellow(labelText), suffix)
		} else {
			if termWidth > 0 {
				maxLabel := termWidth - 2
				if maxLabel < minTruncWidth {
					maxLabel = minTruncWidth
				}
				labelText = ui.Truncate(labelText, maxLabel)
			}
			rw.printf("  %s\n", c.Yellow(labelText))
		}
	}
	return rw.err
}

func renderReadyForReview(w io.Writer, c colorSet, data statusData) error {
	if len(data.readyForReview) == 0 {
		return nil
	}
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Ready for Review"))
	rw.println(c.Bold("────────────────"))
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
			rw.printf("  %s — %s\n", c.Cyan(task.name), detail)
		} else {
			rw.printf("  %s\n", c.Cyan(task.name))
		}
	}
	return rw.err
}

func renderReadyToMerge(w io.Writer, c colorSet, data statusData) error {
	if len(data.readyToMerge) == 0 {
		return nil
	}
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Ready to Merge"))
	rw.println(c.Bold("──────────────"))
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
		rw.printf("  %s  %s\n", c.Cyan(labelText), c.Dim(suffix))
	}
	return rw.err
}

func renderDependencyBlocked(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Dependency-Blocked"))
	rw.println(c.Bold("──────────────────"))
	if len(data.waitingTasks) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
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
		rw.printf("  %s  %s\n", labelText, c.Dim(suffix))
		if len(task.Dependencies) == 0 {
			rw.printf("    depends on: none\n")
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
		rw.printf("    depends on: %s\n", depLine)
	}
	return rw.err
}

func renderConflictDeferred(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Conflict-Deferred (backlog/, excluded from queue)"))
	rw.println(c.Bold("──────────────────────────────────────────────────"))
	if len(data.deferredDetail) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
	}
	termWidth := ui.TermWidth()
	deferredNames := make([]string, 0, len(data.deferredDetail))
	for name := range data.deferredDetail {
		deferredNames = append(deferredNames, name)
	}
	sort.Strings(deferredNames)
	for _, name := range deferredNames {
		info := data.deferredDetail[name]
		rw.printf("  %s\n", c.Yellow(name))
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
		rw.printf("    blocked by: %s\n", blockedLine)
		rw.printf("    conflicting affects: %s\n", affectsLine)
	}
	return rw.err
}

func renderFailedTasks(w io.Writer, c colorSet, data statusData) error {
	if len(data.failedTasks) == 0 {
		return nil
	}
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Failed Tasks"))
	rw.println(c.Bold("────────────"))
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
		rw.printf("  %s%s\n", c.Red(labelText), suffix)
	}
	return rw.err
}

func renderRecentCompletions(w io.Writer, c colorSet, data statusData) error {
	if len(data.completions) == 0 {
		return nil
	}
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Recent Completions"))
	rw.println(c.Bold("──────────────────"))
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
		rw.printf("  %s  %s\n", label, c.Dim(suffix))
	}
	return rw.err
}

func renderRecentMessages(w io.Writer, c colorSet, data statusData) error {
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold("Recent Messages"))
	rw.println(c.Bold("───────────────"))
	if len(data.recentMessages) == 0 {
		rw.println(c.Dim("  (none)"))
		return rw.err
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
		rw.printf("  %s %s: %s\n", c.Dim("["+msg.SentAt.Local().Format("15:04:05")+"]"), c.Yellow(from), line)
	}
	return rw.err
}

// renderWarnings prints any non-fatal warnings collected during data gathering.
func renderWarnings(w io.Writer, c colorSet, data statusData) error {
	if len(data.warnings) == 0 {
		return nil
	}
	rw := renderWriter{w: w}
	rw.println()
	rw.println(c.Bold(c.Yellow("Warnings")))
	rw.println(c.Bold(c.Yellow("────────")))
	for _, warn := range data.warnings {
		rw.printf("  %s %s\n", c.Yellow("⚠"), warn)
	}
	return rw.err
}
