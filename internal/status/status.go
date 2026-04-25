// Package status gathers and displays queue state for the mato status command,
// including task counts, agent activity, and dependency information.
package status

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/timeutil"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/fatih/color"

	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/lockfile"
	"github.com/ryansimmen/mato/internal/messaging"
	"github.com/ryansimmen/mato/internal/queueview"
)

type textViewMode int

const (
	textViewCompact textViewMode = iota
	textViewVerbose
)

// Show writes the status dashboard to os.Stdout.
func Show(repoRoot string) error {
	return showToMode(os.Stdout, repoRoot, textViewCompact)
}

// ShowVerbose writes the expanded status dashboard to os.Stdout.
func ShowVerbose(repoRoot string) error {
	return showToMode(os.Stdout, repoRoot, textViewVerbose)
}

// ShowTo writes the status dashboard to the given writer.
func ShowTo(w io.Writer, repoRoot string) error {
	return showToMode(w, repoRoot, textViewCompact)
}

// ShowVerboseTo writes the expanded status dashboard to the given writer.
func ShowVerboseTo(w io.Writer, repoRoot string) error {
	return showToMode(w, repoRoot, textViewVerbose)
}

func showToMode(w io.Writer, repoRoot string, mode textViewMode) error {
	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return err
	}
	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := ui.RequireTasksDir(tasksDir); err != nil {
		return err
	}

	data, err := gatherStatus(tasksDir)
	if err != nil {
		return fmt.Errorf("gather status: %w", err)
	}

	c := newColorSet()
	if mode == textViewVerbose {
		return renderVerboseDashboard(w, c, data)
	}
	return renderCompactDashboard(w, c, data)
}

type taskEntry struct {
	name                      string
	title                     string
	id                        string
	cancelled                 bool
	priority                  int
	maxRetries                int
	branch                    string
	claimedBy                 string
	claimedAt                 time.Time
	failureCount              int
	lastFailureReason         string
	lastCycleFailureReason    string
	lastTerminalFailureReason string
}

// listTasksFromIndex derives a sorted task list from the PollIndex snapshot
// for the given directory, replacing the old listTasksInDir which performed
// its own filesystem scan. Tasks with parse failures are included with
// default metadata to preserve visibility.
func listTasksFromIndex(idx *queueview.PollIndex, dir string) []taskEntry {
	snaps := idx.TasksByState(dir)
	tasks := make([]taskEntry, 0, len(snaps))
	for _, snap := range snaps {
		tasks = append(tasks, taskEntry{
			name:                      snap.Filename,
			title:                     frontmatter.ExtractTitle(snap.Filename, snap.Body),
			id:                        snap.Meta.ID,
			cancelled:                 snap.Cancelled,
			priority:                  snap.Meta.Priority,
			maxRetries:                snap.Meta.MaxRetries,
			branch:                    snap.Branch,
			claimedBy:                 snap.ClaimedBy,
			claimedAt:                 snap.ClaimedAt,
			failureCount:              snap.FailureCount,
			lastFailureReason:         snap.LastFailureReason,
			lastCycleFailureReason:    snap.LastCycleFailureReason,
			lastTerminalFailureReason: snap.LastTerminalFailureReason,
		})
	}
	// Include files that failed frontmatter parsing with defaults,
	// so they remain visible in the status dashboard.
	for _, pf := range idx.ParseFailures() {
		if pf.State != dir {
			continue
		}
		tasks = append(tasks, taskEntry{
			name:                      pf.Filename,
			cancelled:                 pf.Cancelled,
			priority:                  50,
			maxRetries:                3,
			branch:                    pf.Branch,
			claimedBy:                 pf.ClaimedBy,
			claimedAt:                 pf.ClaimedAt,
			failureCount:              pf.FailureCount,
			lastFailureReason:         pf.LastFailureReason,
			lastCycleFailureReason:    pf.LastCycleFailureReason,
			lastTerminalFailureReason: pf.LastTerminalFailureReason,
		})
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})
	return tasks
}

type statusAgent struct {
	ID  string
	PID int
}

func (a statusAgent) displayName() string {
	if strings.HasPrefix(a.ID, "agent-") {
		return a.ID
	}
	return "agent-" + a.ID
}

// waitingDep describes a single dependency and its resolution state.
type waitingDep struct {
	ID     string
	Status string // queue state (e.g. "completed", "ambiguous", "missing", "unknown")
}

type waitingTaskSummary struct {
	Name         string
	Title        string
	Priority     int
	State        string
	Dependencies []waitingDep
}

func activeAgents(tasksDir string) ([]statusAgent, []string, error) {
	entries, err := os.ReadDir(filepath.Join(tasksDir, ".locks"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("read locks dir: %w", err)
	}

	var warnings []string
	agents := make([]statusAgent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(entry.Name(), ".pid")
		lockPath := filepath.Join(tasksDir, ".locks", entry.Name())
		meta, err := lockfile.ReadMetadata(lockPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped unreadable lock file %s: %v", entry.Name(), err))
			continue
		}
		if !meta.IsActive() {
			continue
		}
		agents = append(agents, statusAgent{ID: agentID, PID: meta.PID})
	}

	sort.Slice(agents, func(i, j int) bool {
		return agents[i].displayName() < agents[j].displayName()
	})
	return agents, warnings, nil
}

// waitingTasksFromIndex derives dependency-blocked task summaries from the
// PollIndex snapshot and the already-computed backlog dependency blockers. It
// populates structured dependency data (ID + status) without any presentation
// formatting so the same model can drive both text and JSON output.
func waitingTasksFromIndex(idx *queueview.PollIndex, blockedBacklog map[string][]queueview.DependencyBlock) []waitingTaskSummary {
	snaps := idx.TasksByState(dirs.Waiting)

	// Build ID→state map from the index for satisfied or genuinely missing
	// dependencies. Ambiguity and unsatisfied-state classification come from the
	// shared queueview dependency blocker logic so status matches scheduling.
	stateByID := taskStatesByIDFromIndex(idx)

	waiting := make([]waitingTaskSummary, 0, len(snaps))
	for _, snap := range snaps {
		title := frontmatter.ExtractTitle(snap.Filename, snap.Body)
		blockedByID := make(map[string]string)
		for _, block := range queueview.DependencyBlocksFor(idx, snap.Meta.DependsOn) {
			blockedByID[block.DependencyID] = block.State
		}
		deps := make([]waitingDep, 0, len(snap.Meta.DependsOn))
		for _, dep := range snap.Meta.DependsOn {
			status, blocked := blockedByID[dep]
			fallbackStatus := stateByID[dep]
			if blocked {
				if status == "unknown" && fallbackStatus == "" {
					status = "missing"
				}
			} else {
				status = fallbackStatus
				if status == "" {
					status = "missing"
				}
			}
			deps = append(deps, waitingDep{ID: dep, Status: status})
		}
		waiting = append(waiting, waitingTaskSummary{
			Name:         snap.Filename,
			Title:        title,
			Priority:     snap.Meta.Priority,
			State:        dirs.Waiting,
			Dependencies: deps,
		})
	}

	for _, snap := range idx.TasksByState(dirs.Backlog) {
		blocks, ok := blockedBacklog[snap.Filename]
		if !ok {
			continue
		}
		deps := make([]waitingDep, 0, len(blocks))
		for _, block := range blocks {
			deps = append(deps, waitingDep{ID: block.DependencyID, Status: block.State})
		}
		waiting = append(waiting, waitingTaskSummary{
			Name:         snap.Filename,
			Title:        frontmatter.ExtractTitle(snap.Filename, snap.Body),
			Priority:     snap.Meta.Priority,
			State:        dirs.Backlog,
			Dependencies: deps,
		})
	}

	sort.Slice(waiting, func(i, j int) bool {
		if waiting[i].Priority != waiting[j].Priority {
			return waiting[i].Priority < waiting[j].Priority
		}
		return waiting[i].Name < waiting[j].Name
	})
	return waiting
}

// taskStatesByIDFromIndex builds an ID→state map from the PollIndex snapshot,
// replacing taskStatesByID which performed its own full directory scans.
func taskStatesByIDFromIndex(idx *queueview.PollIndex) map[string]string {
	states := make(map[string]string)
	for _, dir := range dirs.All {
		for _, snap := range idx.TasksByState(dir) {
			states[frontmatter.TaskFileStem(snap.Filename)] = dir
			if snap.Meta.ID != "" {
				states[snap.Meta.ID] = dir
			}
		}
	}
	for _, pf := range idx.ParseFailures() {
		states[frontmatter.TaskFileStem(pf.Filename)] = pf.State
	}
	return states
}

// reverseDepsFromIndex derives reverse dependencies from the PollIndex snapshot,
// replacing reverseDependencies which performed its own filesystem scan.
func reverseDepsFromIndex(idx *queueview.PollIndex) map[string][]string {
	snaps := idx.TasksByState(dirs.Waiting)
	result := make(map[string][]string)
	for _, snap := range snaps {
		for _, dep := range snap.Meta.DependsOn {
			result[dep] = append(result[dep], snap.Filename)
		}
	}
	return result
}

// latestProgressByAgent returns the most recent progress message per agent.
// When two messages share the same timestamp, the one with the lexically
// smallest ID wins. This tie-break rule is the canonical contract shared
// with ReadLatestProgressForAgents so that in-window and fallback progress
// selection always agree.
func latestProgressByAgent(messages []messaging.Message) map[string]messaging.Message {
	result := make(map[string]messaging.Message)
	for _, msg := range messages {
		if msg.Type != "progress" {
			continue
		}
		existing, ok := result[msg.From]
		if !ok || msg.SentAt.After(existing.SentAt) ||
			(msg.SentAt.Equal(existing.SentAt) && msg.ID < existing.ID) {
			result[msg.From] = msg
		}
	}
	return result
}

// formatDuration returns a human-friendly duration string via the shared
// timeutil helper (e.g. "3 sec", "12 min", "2 hr", "1 day").
func formatDuration(d time.Duration) string {
	return timeutil.FormatDuration(d)
}

func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

// Watch calls Show in a loop, redrawing the terminal without flicker.
// It writes to os.Stdout; use WatchTo to write to a different writer.
func Watch(ctx context.Context, repoRoot string, interval time.Duration) error {
	return watchToMode(ctx, os.Stdout, repoRoot, interval, textViewCompact)
}

// WatchVerbose calls ShowVerbose in a loop, redrawing the terminal without flicker.
func WatchVerbose(ctx context.Context, repoRoot string, interval time.Duration) error {
	return watchToMode(ctx, os.Stdout, repoRoot, interval, textViewVerbose)
}

// WatchTo calls ShowTo in a loop, redrawing the given writer without flicker.
// It buffers all output, then writes it atomically: cursor-home, single write
// with per-line erase-to-EOL (\033[K), then clear remaining lines below.
// The per-line erase prevents artifacts when a line shrinks between frames.
// It runs until the context is cancelled or a write error occurs (e.g. stdout
// closed by a pager or pipe).
func WatchTo(ctx context.Context, w io.Writer, repoRoot string, interval time.Duration) error {
	return watchToMode(ctx, w, repoRoot, interval, textViewCompact)
}

func watchToMode(ctx context.Context, w io.Writer, repoRoot string, interval time.Duration, mode textViewMode) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	return watchToModeWithTicks(ctx, w, repoRoot, interval, mode, ticker.C)
}

func watchToModeWithTicks(ctx context.Context, w io.Writer, repoRoot string, interval time.Duration, mode textViewMode, ticks <-chan time.Time) error {
	dim := color.New(color.Faint).SprintFunc()
	for {
		var buf bytes.Buffer
		if err := showToMode(&buf, repoRoot, mode); err != nil {
			return err
		}
		fmt.Fprintf(&buf, "\n%s\n", dim(fmt.Sprintf("Refreshing every %s — press Ctrl+C to stop", interval)))

		// Atomic redraw: move cursor home, write content with per-line
		// clearing, then erase any leftover lines below the new output.
		// Each \n is preceded by \033[K (erase-to-EOL) so that shorter
		// replacement lines don't leave trailing artifacts from the
		// previous frame.
		if _, err := w.Write([]byte("\033[H")); err != nil {
			return fmt.Errorf("redraw cursor-home: %w", err)
		}
		content := bytes.ReplaceAll(buf.Bytes(), []byte("\n"), []byte("\033[K\n"))
		if _, err := w.Write(content); err != nil {
			return fmt.Errorf("redraw content: %w", err)
		}
		if _, err := w.Write([]byte("\033[J")); err != nil {
			return fmt.Errorf("redraw clear-tail: %w", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-ticks:
			if !ok {
				return nil
			}
		}
	}
}
