package status

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/ui"
)

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
			state = dirs.Waiting
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
			if dep.Status == dirs.Completed {
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
