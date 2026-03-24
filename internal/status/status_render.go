package status

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"

	"mato/internal/frontmatter"
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

func renderQueueOverview(w io.Writer, c colorSet, data statusData) {
	fmt.Fprintln(w, c.bold("Queue Overview"))
	fmt.Fprintln(w, c.bold("──────────────"))
	fmt.Fprintf(w, "  backlog:        %s  %s\n", c.green(data.queueCounts[queue.DirBacklog]), c.dim("(total tasks in backlog/)"))
	fmt.Fprintf(w, "  runnable:       %s\n", c.green(data.runnable))
	fmt.Fprintf(w, "  deferred:       %s  %s\n", c.yellow(len(data.deferredDetail)), c.dim("(conflict-blocked, in backlog)"))
	fmt.Fprintf(w, "  waiting:        %s  %s\n", c.dim(data.queueCounts[queue.DirWaiting]), c.dim("(dependency-blocked)"))
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

func renderInProgressTasks(w io.Writer, c colorSet, tasksDir string, data statusData) {
	if len(data.inProgressTasks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("In-Progress Tasks"))
	fmt.Fprintln(w, c.bold("─────────────────"))
	now := time.Now().UTC()
	for _, task := range data.inProgressTasks {
		taskPath := filepath.Join(tasksDir, queue.DirInProgress, task.name)
		claimedBy := queue.ParseClaimedBy(taskPath)

		var parts []string
		if claimedBy != "" {
			parts = append(parts, fmt.Sprintf("agent %s", claimedBy))
		}
		if claimedAt := parseClaimedAt(taskPath); !claimedAt.IsZero() {
			parts = append(parts, formatDuration(now.Sub(claimedAt)))
		}
		failCount := countFailureRecords(taskPath)
		if failCount > 0 {
			parts = append(parts, fmt.Sprintf("%s/%d retries used", c.red(failCount), task.maxRetries))
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

func renderReadyForReview(w io.Writer, c colorSet, tasksDir string, data statusData) {
	if len(data.readyForReview) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Ready for Review"))
	fmt.Fprintln(w, c.bold("────────────────"))
	for _, task := range data.readyForReview {
		taskPath := filepath.Join(tasksDir, queue.DirReadyReview, task.name)
		branch := parseBranchComment(taskPath)
		var parts []string
		if task.title != "" {
			parts = append(parts, task.title)
		}
		if branch != "" {
			parts = append(parts, "on "+c.cyan(branch))
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
	fmt.Fprintln(w, c.bold("Dependency-Blocked (waiting/)"))
	fmt.Fprintln(w, c.bold("─────────────────────────────"))
	if len(data.waitingTasks) == 0 {
		fmt.Fprintln(w, c.dim("  (none)"))
		return
	}
	for _, task := range data.waitingTasks {
		label := task.Name
		if task.Title != "" {
			label = fmt.Sprintf("%s — %s", task.Name, task.Title)
		}
		fmt.Fprintf(w, "  %s\n", label)
		fmt.Fprintf(w, "    depends on: %s\n", strings.Join(task.Dependencies, ", "))
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

func renderFailedTasks(w io.Writer, c colorSet, tasksDir string, data statusData) {
	if len(data.failedTasks) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, c.bold("Failed Tasks"))
	fmt.Fprintln(w, c.bold("────────────"))
	for _, task := range data.failedTasks {
		taskPath := filepath.Join(tasksDir, queue.DirFailed, task.name)
		label := c.red(task.name)
		if task.title != "" {
			label = fmt.Sprintf("%s — %s", c.red(task.name), task.title)
		}

		cycleReason := lastCycleFailureReason(taskPath)
		failCount := countFailureRecords(taskPath)

		if cycleReason != "" && failCount > 0 {
			// Mixed: both cycle-failure and regular failure markers.
			// Show cycle reason and retry budget info.
			reason := lastFailureReason(taskPath)
			info := fmt.Sprintf("%s; %d/%d retries used", cycleReason, failCount, task.maxRetries)
			if reason != "" {
				info += fmt.Sprintf(", last: %s", reason)
			}
			fmt.Fprintf(w, "  %s  (%s)\n", label, info)
		} else if cycleReason != "" {
			// Cycle-failed task: show cycle reason instead of retry budget.
			fmt.Fprintf(w, "  %s  (%s)\n", label, cycleReason)
		} else {
			reason := lastFailureReason(taskPath)
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
