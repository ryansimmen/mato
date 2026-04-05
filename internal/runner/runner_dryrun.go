package runner

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"mato/internal/frontmatter"
	"mato/internal/queue"
	"mato/internal/ui"
)

// DryRunRenderer formats dry-run validation output to a writer with
// optional color and terminal-width-aware layout.
type DryRunRenderer struct {
	W     io.Writer
	Color ui.ColorSet
	Width int
}

// defaultDryRunWidth is used when the output is not a terminal.
const defaultDryRunWidth = 80

// writerWidthFn resolves the terminal width for an io.Writer. Defaults
// to ui.WriterWidth; tests replace it to inject a narrow width.
//
// NOTE: Package-level test seam — prevents t.Parallel(). See execCommandContext.
var writerWidthFn = ui.WriterWidth

// header writes a bold section header with a leading blank line.
func (r *DryRunRenderer) header(title string) {
	fmt.Fprintf(r.W, "\n%s\n", r.Color.Bold("=== "+title+" ==="))
}

// valueWidth returns the available width for a value column, given a
// fixed label column and indentation. The result is always at least 1
// so callers can always truncate to fit narrow terminals.
func (r *DryRunRenderer) valueWidth(indent, labelCol int) int {
	avail := r.Width - indent - labelCol - 1
	if avail < 1 {
		return 1
	}
	return avail
}

// RenderValidation writes the === Task File Validation === section.
func (r *DryRunRenderer) RenderValidation(parseFailures []queue.ParseFailure, totalTasks int) {
	fmt.Fprintln(r.W, r.Color.Bold("=== Task File Validation ==="))
	if len(parseFailures) > 0 {
		for _, pf := range parseFailures {
			fmt.Fprintf(r.W, "  %s %s/%s: %v\n", r.Color.Red("ERROR"), pf.State, pf.Filename, pf.Err)
		}
		fmt.Fprintf(r.W, "  %d of %d task file(s) have parse errors\n", len(parseFailures), totalTasks)
	} else {
		fmt.Fprintf(r.W, "  %s %d task file(s) parsed successfully\n", r.Color.Green("All"), totalTasks)
	}
}

// RenderDependencyResolution writes the === Dependency Resolution === section.
func (r *DryRunRenderer) RenderDependencyResolution(promotable int) {
	r.header("Dependency Resolution")
	if promotable > 0 {
		fmt.Fprintf(r.W, "  %d task(s) in waiting/ would be promoted to backlog/\n", promotable)
	} else {
		fmt.Fprintln(r.W, "  No waiting tasks ready for promotion")
	}
}

// RenderDependencySummary writes the === Dependency Summary === section for
// waiting/ tasks, showing each dependency and its resolved queue state.
func (r *DryRunRenderer) RenderDependencySummary(tasksDir string, idx *queue.PollIndex) {
	waitingTasks := idx.TasksByState(queue.DirWaiting)
	if len(waitingTasks) == 0 {
		return
	}

	diag := queue.DiagnoseDependencies(tasksDir, idx)

	r.header("Dependency Summary")
	for _, snap := range waitingTasks {
		if len(snap.Meta.DependsOn) == 0 {
			fmt.Fprintf(r.W, "  %s: no dependencies\n", snap.Filename)
			continue
		}
		fmt.Fprintf(r.W, "  %s:\n", snap.Filename)
		for _, dep := range snap.Meta.DependsOn {
			state := resolveDepState(dep, idx)
			fmt.Fprintf(r.W, "    - %s (%s)\n", dep, state)
		}
	}

	if len(diag.Issues) > 0 {
		fmt.Fprintln(r.W, "  diagnostics:")
		for _, issue := range diag.Issues {
			switch issue.Kind {
			case queue.DependencyDuplicateID:
				fmt.Fprintf(r.W, "    %s duplicate waiting id %q (files: %s, %s)\n",
					r.Color.Yellow("WARNING"), issue.TaskID, issue.DependsOn, issue.Filename)
			case queue.DependencySelfCycle:
				fmt.Fprintf(r.W, "    %s %s depends on itself\n", r.Color.Yellow("WARNING"), issue.TaskID)
			case queue.DependencyCycle:
				fmt.Fprintf(r.W, "    %s %s is part of a dependency cycle\n", r.Color.Yellow("WARNING"), issue.TaskID)
			case queue.DependencyAmbiguousID:
				fmt.Fprintf(r.W, "    %s id %q is ambiguous (exists in both completed and non-completed directories)\n",
					r.Color.Yellow("WARNING"), issue.TaskID)
			case queue.DependencyUnknownID:
				fmt.Fprintf(r.W, "    %s %s depends on unknown id %q\n",
					r.Color.Yellow("WARNING"), issue.TaskID, issue.DependsOn)
			}
		}
	}
}

// RenderAffectsConflicts writes the === Affects Conflict Detection ===
// section and any dependency-blocked backlog tasks.
func (r *DryRunRenderer) RenderAffectsConflicts(view queue.RunnableBacklogView) {
	r.header("Affects Conflict Detection")
	blockedBacklog := view.DependencyBlocked
	if len(blockedBacklog) > 0 {
		r.header("Dependency-Blocked Backlog Tasks")
		names := make([]string, 0, len(blockedBacklog))
		for name := range blockedBacklog {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(r.W, "  %s %s (depends on %s)\n",
				r.Color.Red("BLOCKED"), name, queue.FormatDependencyBlocks(blockedBacklog[name]))
		}
	}
	detailed := view.Deferred
	if len(detailed) > 0 {
		names := make([]string, 0, len(detailed))
		for name := range detailed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			info := detailed[name]
			fmt.Fprintf(r.W, "  %s %s (blocked by %s in %s/, conflicting affects: %v)\n",
				r.Color.Yellow("DEFERRED"), name, info.BlockedBy, info.BlockedByDir, info.ConflictingAffects)
		}
	} else {
		fmt.Fprintln(r.W, "  No affects conflicts detected")
	}
}

// RenderExecutionOrder writes the === Execution Order === section showing
// runnable backlog tasks in priority order with their priority values.
func (r *DryRunRenderer) RenderExecutionOrder(runnable []*queue.TaskSnapshot) {
	r.header("Execution Order")
	if len(runnable) == 0 {
		fmt.Fprintln(r.W, r.Color.Dim("  (no runnable tasks)"))
		return
	}
	maxName := r.Width - 20 // room for "  N. " prefix + " (priority NNN)"
	for i, snap := range runnable {
		name := snap.Filename
		if maxName > 10 {
			name = ui.Truncate(name, maxName)
		}
		fmt.Fprintf(r.W, "  %d. %s %s\n", i+1, name, r.Color.Dim(fmt.Sprintf("(priority %d)", snap.Meta.Priority)))
	}
}

// RenderBacklogSummary writes the === Backlog Task Summary === section with
// compact frontmatter for every parsed backlog task.
func (r *DryRunRenderer) RenderBacklogSummary(idx *queue.PollIndex, deferred map[string]struct{}, blocked map[string][]queue.DependencyBlock) {
	backlog := idx.TasksByState(queue.DirBacklog)
	if len(backlog) == 0 {
		return
	}
	r.header("Backlog Task Summary")
	for _, snap := range backlog {
		status := r.Color.Green("runnable")
		if _, ok := blocked[snap.Filename]; ok {
			status = r.Color.Red("dependency-blocked")
		} else if _, ok := deferred[snap.Filename]; ok {
			status = r.Color.Yellow("deferred")
		}

		affects := "none"
		if len(snap.Meta.Affects) > 0 {
			joined := strings.Join(snap.Meta.Affects, ", ")
			affects = ui.Truncate(joined, r.valueWidth(4, 9)) // "    affects: "
		}
		dependsOn := "none"
		if len(snap.Meta.DependsOn) > 0 {
			joined := strings.Join(snap.Meta.DependsOn, ", ")
			dependsOn = ui.Truncate(joined, r.valueWidth(4, 12)) // "    depends_on: "
		}

		displayName := snap.Filename
		// Account for "  " prefix + " [" + status text + "]" suffix.
		// The longest plain-text status is "dependency-blocked" (18 chars).
		statusLen := 8 // "runnable"
		if _, ok := blocked[snap.Filename]; ok {
			statusLen = 18 // "dependency-blocked"
		} else if _, ok := deferred[snap.Filename]; ok {
			statusLen = 8 // "deferred"
		}
		overhead := 2 + 2 + statusLen + 1 // "  " + " [" + status + "]"
		maxName := r.Width - overhead
		if maxName > 0 {
			displayName = ui.Truncate(displayName, maxName)
		}
		fmt.Fprintf(r.W, "  %s [%s]\n", displayName, status)
		fmt.Fprintf(r.W, "    id: %s  priority: %d\n", snap.Meta.ID, snap.Meta.Priority)
		fmt.Fprintf(r.W, "    affects: %s\n", affects)
		fmt.Fprintf(r.W, "    depends_on: %s\n", dependsOn)
		if blocks, ok := blocked[snap.Filename]; ok {
			fmt.Fprintf(r.W, "    blocked by: %s\n", queue.FormatDependencyBlocks(blocks))
		}
	}
}

// RenderResolvedSettings writes the === Resolved Settings === section.
func (r *DryRunRenderer) RenderResolvedSettings(opts RunOptions) {
	r.header("Resolved Settings")
	labelW := 24
	vw := r.valueWidth(2, labelW)
	printSetting := func(label, value string) {
		value = ui.Truncate(value, vw)
		fmt.Fprintf(r.W, "  %-*s %s\n", labelW, label, value)
	}
	printSetting("task model:", opts.TaskModel)
	printSetting("review model:", opts.ReviewModel)
	fmt.Fprintf(r.W, "  %-*s %t\n", labelW, "review session resume:", opts.ReviewSessionResumeEnabled)
	printSetting("task reasoning effort:", opts.TaskReasoningEffort)
	printSetting("review reasoning effort:", opts.ReviewReasoningEffort)
}

// RenderQueueSummary writes the === Queue Summary === section.
func (r *DryRunRenderer) RenderQueueSummary(idx *queue.PollIndex, subdirs []string, parseFailuresByDir map[string]int, deferredCount int) {
	r.header("Queue Summary")
	for _, sub := range subdirs {
		fmt.Fprintf(r.W, "  %-20s %d\n", sub, len(idx.TasksByState(sub))+parseFailuresByDir[sub])
	}
	if deferredCount > 0 {
		fmt.Fprintf(r.W, "  %-20s %d\n", "deferred", deferredCount)
	}
}

// resolveDepState determines the queue state label for a dependency ID.
// It checks each queue directory for a task with a matching ID (frontmatter
// ID or filename stem), including parse-failed files that still have a known
// filename stem. Returns "unknown" if not found, or "ambiguous" if multiple
// task files match and the dependency cannot be resolved safely.
func resolveDepState(depID string, idx *queue.PollIndex) string {
	seen := make(map[string]struct{})
	matchedState := ""

	for _, dir := range queue.AllDirs {
		for _, snap := range idx.TasksByState(dir) {
			if snap.Meta.ID != depID && frontmatter.TaskFileStem(snap.Filename) != depID {
				continue
			}
			key := dir + "/" + snap.Filename
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if matchedState == "" {
				matchedState = dir
				continue
			}
			return "ambiguous"
		}
	}

	for _, pf := range idx.ParseFailures() {
		if frontmatter.TaskFileStem(pf.Filename) != depID {
			continue
		}
		key := pf.State + "/" + pf.Filename
		if _, ok := seen[key]; ok {
			continue
		}
		if matchedState == "" {
			matchedState = pf.State
			seen[key] = struct{}{}
			continue
		}
		return "ambiguous"
	}

	if matchedState == "" {
		return "unknown"
	}
	return matchedState
}
