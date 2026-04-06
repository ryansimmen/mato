package status

import (
	"fmt"
	"io"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/ui"
)

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
		c.Green(data.queueCounts[dirs.Backlog]),
		c.Green(data.runnable),
		c.Yellow(data.queueCounts[dirs.InProgress]),
		c.Cyan(data.queueCounts[dirs.ReadyReview]),
		c.Cyan(data.queueCounts[dirs.ReadyMerge]),
		c.Red(data.queueCounts[dirs.Failed]),
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
