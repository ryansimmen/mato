package status

import (
	"fmt"
	"io"
	"strings"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/queue"
	"mato/internal/ui"
)

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

