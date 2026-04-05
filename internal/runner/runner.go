// Package runner manages the agent lifecycle including Docker-based task
// execution, review orchestration, and the top-level poll loop that drives
// claiming, running, and merging tasks.
package runner

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/identity"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/queue"
	"mato/internal/sessionmeta"
	"mato/internal/taskstate"
	"mato/internal/ui"
)

const resumeDetectionBufferLimit = 8192

type resumeDetectionBuffer struct {
	matched bool
	buf     bytes.Buffer
}

func (b *resumeDetectionBuffer) Write(p []byte) (int, error) {
	b.buf.Write(p)
	if !b.matched && resumeRejectedBytes(b.buf.Bytes()) {
		b.matched = true
	}
	if b.buf.Len() > resumeDetectionBufferLimit {
		data := b.buf.Bytes()
		tail := data[len(data)-resumeDetectionBufferLimit:]
		b.buf.Reset()
		b.buf.Write(tail)
	}
	return len(p), nil
}

func (b *resumeDetectionBuffer) Matched() bool {
	return b.matched || resumeRejectedBytes(b.buf.Bytes())
}

// execCommandContext creates an *exec.Cmd bound to a context. It is a
// variable so tests can inject a stub without spawning real processes.
//
// NOTE: This is a package-level mutable variable used as a test seam.
// It prevents t.Parallel() within this package. Struct-based dependency
// injection would be needed for true parallel test safety.
var execCommandContext = exec.CommandContext

//go:embed task-instructions.md
var taskInstructions string

//go:embed review-instructions.md
var reviewInstructions string

// DefaultAgentTimeout is the default execution timeout for Docker agent
// containers.
const DefaultAgentTimeout = 30 * time.Minute

const defaultAgentTimeout = DefaultAgentTimeout

const DefaultTaskModel = "claude-opus-4.6"
const DefaultReviewModel = "gpt-5.4"
const DefaultReasoningEffort = "high"

// gracefulShutdownDelay is the time to wait after sending SIGTERM to a
// Docker container before escalating to SIGKILL.
const gracefulShutdownDelay = 10 * time.Second

const (
	// basePollInterval is the default polling interval between loop iterations.
	basePollInterval = 10 * time.Second

	// maxPollInterval is the upper bound for exponential backoff.
	maxPollInterval = 5 * time.Minute

	// errBackoffThreshold is the number of consecutive poll errors before
	// the loop enters backoff mode.
	errBackoffThreshold = 5

	// idleHeartbeatThreshold is the number of consecutive idle polls before
	// the runner switches from the initial idle message to throttled heartbeats.
	idleHeartbeatThreshold = 5

	// heartbeatInterval is the minimum time between throttled heartbeat
	// messages once the idle threshold is reached.
	heartbeatInterval = time.Minute
)

// pollBackoff returns the poll interval given the number of consecutive errors.
// Below errBackoffThreshold it returns basePollInterval. Above the threshold it
// doubles the interval for each additional error, capped at maxPollInterval.
func pollBackoff(consecutiveErrors int) time.Duration {
	if consecutiveErrors < errBackoffThreshold {
		return basePollInterval
	}
	d := basePollInterval
	for i := 0; i < consecutiveErrors-errBackoffThreshold+1; i++ {
		d *= 2
		if d >= maxPollInterval {
			return maxPollInterval
		}
	}
	return d
}

// idleHeartbeat tracks the idle heartbeat state so the runner can provide
// periodic output when the queue is empty for an extended period.
type idleHeartbeat struct {
	consecutiveIdlePolls int
	lastActivityTime     time.Time
	lastHeartbeatTime    time.Time
	startTime            time.Time
}

// RunMode controls how long runner.Run keeps polling before exiting.
type RunMode int

const (
	// RunModeDaemon is the default long-running polling loop.
	RunModeDaemon RunMode = iota
	// RunModeOnce runs exactly one poll iteration, then exits.
	RunModeOnce
	// RunModeUntilIdle keeps polling until no actionable queue work remains.
	RunModeUntilIdle
)

// RunOptions holds configuration values for a mato run.
//
// TaskModel, ReviewModel, TaskReasoningEffort, and ReviewReasoningEffort
// must already be resolved to non-empty values before calling Run or DryRun.
// DockerImage, AgentTimeout, RetryCooldown, and Mode may be left zero to use
// downstream defaults.
type RunOptions struct {
	DockerImage                string
	Mode                       RunMode
	TaskModel                  string
	ReviewModel                string
	ReviewSessionResumeEnabled bool
	TaskReasoningEffort        string
	ReviewReasoningEffort      string
	AgentTimeout               time.Duration
	RetryCooldown              time.Duration
}

func normalizeAndValidateRunOptions(opts RunOptions) (RunOptions, error) {
	opts.TaskModel = strings.TrimSpace(opts.TaskModel)
	opts.ReviewModel = strings.TrimSpace(opts.ReviewModel)
	opts.TaskReasoningEffort = strings.TrimSpace(opts.TaskReasoningEffort)
	opts.ReviewReasoningEffort = strings.TrimSpace(opts.ReviewReasoningEffort)

	switch opts.Mode {
	case RunModeDaemon, RunModeOnce, RunModeUntilIdle:
	default:
		return opts, fmt.Errorf("invalid run mode %d", opts.Mode)
	}

	if opts.TaskModel == "" {
		return opts, fmt.Errorf("task model must not be empty")
	}
	if opts.ReviewModel == "" {
		return opts, fmt.Errorf("review model must not be empty")
	}
	if opts.TaskReasoningEffort == "" {
		return opts, fmt.Errorf("task reasoning effort must not be empty")
	}
	if opts.ReviewReasoningEffort == "" {
		return opts, fmt.Errorf("review reasoning effort must not be empty")
	}

	return opts, nil
}

// newIdleHeartbeat creates an idleHeartbeat initialised with the given time.
func newIdleHeartbeat(now time.Time) idleHeartbeat {
	return idleHeartbeat{
		startTime:        now,
		lastActivityTime: now,
	}
}

// recordActivity resets the idle counters when work is performed (task
// claimed, merge processed, review completed).
func (h *idleHeartbeat) recordActivity(now time.Time) {
	h.consecutiveIdlePolls = 0
	h.lastActivityTime = now
	h.lastHeartbeatTime = time.Time{}
}

// idleMessage increments the idle counter and returns the message to print,
// or "" if nothing should be printed this cycle. nextPoll is the duration
// until the next poll iteration.
func (h *idleHeartbeat) idleMessage(now time.Time, nextPoll time.Duration) string {
	h.consecutiveIdlePolls++
	if h.consecutiveIdlePolls == 1 {
		return fmt.Sprintf("[mato] idle — waiting for tasks (next poll in %s)", formatDurationShort(nextPoll))
	}
	if h.consecutiveIdlePolls <= idleHeartbeatThreshold {
		return ""
	}
	if !h.lastHeartbeatTime.IsZero() && now.Sub(h.lastHeartbeatTime) < heartbeatInterval {
		return ""
	}
	h.lastHeartbeatTime = now
	uptime := now.Sub(h.startTime)
	lastAct := now.Sub(h.lastActivityTime)
	return fmt.Sprintf("[mato] idle — no tasks available (uptime: %s, last activity: %s ago)",
		formatDurationShort(uptime), formatDurationShort(lastAct))
}

// pausedMessage returns the paused heartbeat message or "" when nothing should
// be printed this cycle. When priorPaused is false, the message is emitted
// immediately and the paused heartbeat cadence is reset.
func (h *idleHeartbeat) pausedMessage(now time.Time, priorPaused bool) string {
	if !priorPaused {
		h.lastHeartbeatTime = now
		return "[mato] paused - run 'mato resume' to continue"
	}
	if !h.lastHeartbeatTime.IsZero() && now.Sub(h.lastHeartbeatTime) < heartbeatInterval {
		return ""
	}
	h.lastHeartbeatTime = now
	return "[mato] paused - run 'mato resume' to continue"
}

// formatDurationShort formats a duration in a compact human-readable form
// (e.g. "10s", "5m", "1h30m").
func formatDurationShort(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

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

// DryRun validates the task queue setup without launching Docker containers.
// It runs one iteration of queue management (dependency promotion, overlap
// detection, manifest writing) and reports the results to w, then exits.
func DryRun(w io.Writer, repoRoot, branch string, opts RunOptions) error {
	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	opts, err = normalizeAndValidateRunOptions(opts)
	if err != nil {
		return fmt.Errorf("validate run options: %w", err)
	}

	tasksDir := filepath.Join(repoRoot, dirs.Root)

	subdirs := queue.AllDirs

	// Verify directory structure
	missingDirs := 0
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			ui.Warnf("warning: missing directory: %s/\n", sub)
			missingDirs++
		}
	}
	if missingDirs > 0 {
		return fmt.Errorf("%d required queue directories missing — run `mato init` to create them", missingDirs)
	}

	// Build a single shared index for all sections.
	idx := queue.BuildIndex(tasksDir)
	surfaceBuildWarnings(idx)

	r := &DryRunRenderer{
		W:     w,
		Color: ui.NewColorSet(),
		Width: writerWidthFn(w, defaultDryRunWidth),
	}

	parseFailures := idx.ParseFailures()
	totalTasks := len(parseFailures)
	for _, sub := range subdirs {
		totalTasks += len(idx.TasksByState(sub))
	}
	r.RenderValidation(parseFailures, totalTasks)

	promotable := queue.CountPromotableWaitingTasks(tasksDir, idx)
	r.RenderDependencyResolution(promotable)

	r.RenderDependencySummary(tasksDir, idx)

	backlogView := queue.ComputeRunnableBacklogView(tasksDir, idx)
	r.RenderAffectsConflicts(backlogView)

	deferredSet := make(map[string]struct{}, len(backlogView.Deferred))
	for name := range backlogView.Deferred {
		deferredSet[name] = struct{}{}
	}
	r.RenderExecutionOrder(backlogView.Runnable)

	r.RenderBacklogSummary(idx, deferredSet, backlogView.DependencyBlocked)

	r.RenderResolvedSettings(opts)

	parseFailuresByDir := make(map[string]int)
	for _, pf := range parseFailures {
		parseFailuresByDir[pf.State]++
	}
	r.RenderQueueSummary(idx, subdirs, parseFailuresByDir, len(backlogView.Deferred))

	fmt.Fprintln(r.W, "\nDry run complete (read-only). No files were modified and no Docker containers were launched.")
	return nil
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

func Run(repoRoot, branch string, opts RunOptions) error {
	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	opts, err = normalizeAndValidateRunOptions(opts)
	if err != nil {
		return fmt.Errorf("validate run options: %w", err)
	}

	branchResult, err := git.EnsureBranch(repoRoot, branch)
	if err != nil {
		return fmt.Errorf("ensure branch: %w", err)
	}
	reportBranchResolution(branchResult)

	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := checkDocker(); err != nil {
		return fmt.Errorf("check docker: %w", err)
	}

	for _, sub := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create %s subdirectory %s: %w", dirs.Root, sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		return fmt.Errorf("init messaging: %w", err)
	}

	agentID, err := identity.GenerateAgentID()
	if err != nil {
		return fmt.Errorf("generate agent ID: %w", err)
	}

	cleanupLock, err := queue.RegisterAgent(tasksDir, agentID)
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	defer cleanupLock()

	gitName, gitEmail := resolveGitIdentity(repoRoot)

	changed, err := git.EnsureGitignoreContains(repoRoot, "/"+dirs.Root+"/")
	if err != nil {
		return fmt.Errorf("update .gitignore: %w", err)
	}
	if changed {
		if err := git.CommitGitignore(repoRoot, "chore: add /"+dirs.Root+"/ to .gitignore"); err != nil {
			return fmt.Errorf("commit .gitignore: %w", err)
		}
	}

	tools, err := discoverHostTools()
	if err != nil {
		return fmt.Errorf("discover host tools: %w", err)
	}

	cfg, run := buildEnvAndRunContext(branch, tools, agentID, gitName, gitEmail, repoRoot, tasksDir, opts)

	ctx, cancel := setupSignalContext()
	defer cancel()
	defer signal.Stop(signalChan(ctx))

	if err := ensureDockerImage(ctx, cfg.image); err != nil {
		return fmt.Errorf("ensure docker image: %w", err)
	}

	cleanStaleClones(os.TempDir(), time.Now(), staleCloneMaxAge)

	return pollLoop(ctx, cfg, run, repoRoot, tasksDir, branch, agentID, opts.RetryCooldown, opts.Mode)
}

func reportBranchResolution(result git.EnsureBranchResult) {
	if result.Source == git.BranchSourceLocal {
		return
	}

	switch result.Source {
	case git.BranchSourceRemoteCached, git.BranchSourceHeadRemoteUnavailable:
		ui.Warnf("warning: using branch %s (%s)\n", result.Branch, result.SourceDescription())
	default:
		fmt.Printf("Using branch %s (%s)\n", result.Branch, result.SourceDescription())
	}
}

// resolveGitIdentity reads git user.name and user.email from the local
// repo config, falling back to global config and defaults via
// git.ResolveIdentity, and ensures both are set on the local repo for
// use inside Docker containers.
func resolveGitIdentity(repoRoot string) (name, email string) {
	return git.EnsureIdentity(repoRoot)
}

// buildEnvAndRunContext assembles the envConfig and runContext from resolved
// host tools, agent identity, and runtime settings.
func buildEnvAndRunContext(branch string, tools hostTools, agentID, gitName, gitEmail, repoRoot, tasksDir string, opts RunOptions) (envConfig, runContext) {
	image := opts.DockerImage
	if image == "" {
		image = DefaultDockerImage
	}
	timeout := opts.AgentTimeout
	if timeout <= 0 {
		timeout = defaultAgentTimeout
	}
	workdir := "/workspace"

	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/"+dirs.Root)
	prompt = strings.ReplaceAll(prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	prompt = strings.ReplaceAll(prompt, "MESSAGES_DIR_PLACEHOLDER", workdir+"/"+dirs.Root+"/messages")

	env := envConfig{
		image:                      image,
		workdir:                    workdir,
		copilotPath:                tools.copilotPath,
		gitPath:                    tools.gitPath,
		gitUploadPackPath:          tools.gitUploadPackPath,
		gitReceivePackPath:         tools.gitReceivePackPath,
		ghPath:                     tools.ghPath,
		goRoot:                     tools.goRoot,
		copilotConfigDir:           tools.copilotConfigDir,
		copilotCacheDir:            tools.copilotCacheDir,
		gitName:                    gitName,
		gitEmail:                   gitEmail,
		homeDir:                    tools.homeDir,
		ghConfigDir:                tools.ghConfigDir,
		hasGhConfig:                tools.hasGhConfig,
		gitTemplatesDir:            tools.gitTemplatesDir,
		hasGitTemplates:            tools.hasGitTemplates,
		systemCertsDir:             tools.systemCertsDir,
		hasSystemCerts:             tools.hasSystemCerts,
		repoRoot:                   repoRoot,
		tasksDir:                   tasksDir,
		targetBranch:               branch,
		reviewModel:                opts.ReviewModel,
		reviewReasoningEffort:      opts.ReviewReasoningEffort,
		reviewSessionResumeEnabled: opts.ReviewSessionResumeEnabled,
		isTTY:                      isTerminal(os.Stdin),
	}

	run := runContext{
		prompt:          prompt,
		agentID:         agentID,
		model:           opts.TaskModel,
		reasoningEffort: opts.TaskReasoningEffort,
		timeout:         timeout,
	}

	return env, run
}

// setupSignalContext creates a context.Context that is cancelled when
// SIGINT or SIGTERM is received. The caller must defer both the returned
// cancel function and signal.Stop on the signal channel to ensure the
// signal-listener goroutine exits and signal registration is cleaned up.
func setupSignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Store sigCh in ctx before launching goroutine to avoid data race
	// on the ctx variable.
	ctx = context.WithValue(ctx, signalChanKey{}, sigCh)

	go func() {
		select {
		case <-sigCh:
			fmt.Println("\nShutting down, waiting for current task to finish...")
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}

// signalChanKey is the context key for the signal channel.
type signalChanKey struct{}

// signalChan retrieves the signal channel from a context created by
// setupSignalContext. Returns nil if not present.
func signalChan(ctx context.Context) chan<- os.Signal {
	ch, _ := ctx.Value(signalChanKey{}).(chan os.Signal)
	return ch
}

// pollCleanup recovers orphaned tasks, cleans stale locks and review locks,
// removes stale presence files, and purges old messages. These housekeeping
// operations run at the start of every poll cycle.
func pollCleanup(tasksDir string) {
	for _, recovery := range queue.RecoverOrphanedTasks(tasksDir) {
		finalizePushedTask(tasksDir, recovery.TargetBranch, "host-recovery", recovery.Filename, recovery.Branch, recovery.LastHeadSHA, recoveredFilesChanged(tasksDir, recovery.Filename), false)
	}
	queue.CleanStaleLocks(tasksDir)
	queue.CleanStaleReviewLocks(tasksDir)
	messaging.CleanStalePresence(tasksDir)
	messaging.CleanOldMessages(tasksDir, 24*time.Hour)
	if err := taskstate.Sweep(tasksDir); err != nil {
		ui.Warnf("warning: could not clean stale taskstate: %v\n", err)
	}
	if err := sessionmeta.Sweep(tasksDir); err != nil {
		ui.Warnf("warning: could not clean stale sessionmeta: %v\n", err)
	}
}

// cloneDirPrefix is the prefix used by git.CreateClone when creating temp
// directories. Only directories matching this prefix are considered for
// cleanup.
const cloneDirPrefix = "mato-"

// staleCloneMaxAge is the age threshold beyond which a preserved debug clone
// is considered stale and eligible for removal. 24 hours gives operators
// ample time to inspect a failed push before the directory is reclaimed.
const staleCloneMaxAge = 24 * time.Hour

// cleanStaleClones removes clone directories in the given temp directory
// that were preserved for debugging after a failed push (see runOnce in
// task.go).
//
// A directory is removed only when all three conditions are met:
//  1. Its name starts with the "mato-" prefix used by git.CreateClone.
//  2. It contains the ".mato-debug-clone" sentinel marker written by
//     writeDebugMarker when a clone is intentionally preserved after a
//     postAgentPush failure. This positively identifies the directory as
//     a mato debug clone rather than an active or unrelated temp clone.
//  3. Its modification time is older than maxAge relative to now.
//
// This runs once at runner startup, before the poll loop begins, so that
// stale clones from previous runs are reclaimed without risking removal of
// clones that may still be in active use by a currently running agent.
func cleanStaleClones(tmpDir string, now time.Time, maxAge time.Duration) {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		ui.Warnf("warning: could not list temp directory for clone cleanup: %v\n", err)
		return
	}

	cutoff := now.Add(-maxAge)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), cloneDirPrefix) {
			continue
		}
		dirPath := filepath.Join(tmpDir, entry.Name())

		// Only remove directories that contain the debug marker,
		// confirming they were intentionally preserved after a
		// postAgentPush failure.
		markerPath := filepath.Join(dirPath, debugMarkerFile)
		if _, err := os.Stat(markerPath); err != nil {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		if err := os.RemoveAll(dirPath); err != nil {
			ui.Warnf("warning: could not remove stale clone %s: %v\n", dirPath, err)
			continue
		}
		fmt.Printf("Cleaned up stale clone directory: %s (age: %s)\n", dirPath, now.Sub(info.ModTime()).Truncate(time.Minute))
	}
}

// pollReconcile builds a poll index snapshot, surfaces any build warnings,
// and reconciles the ready queue (promoting waiting tasks whose dependencies
// are satisfied and quarantining exhausted ones). If reconciliation moved
// tasks the index is rebuilt. It returns the (possibly refreshed) index and
// whether any directory-level read failure occurred.
func pollReconcile(tasksDir string) (*queue.PollIndex, bool) {
	idx := queue.BuildIndex(tasksDir)
	hadError := surfaceBuildWarnings(idx)

	if queue.ReconcileReadyQueue(tasksDir, idx) {
		idx = queue.BuildIndex(tasksDir)
		if surfaceBuildWarnings(idx) {
			hadError = true
		}
	}

	return idx, hadError
}

// pauseReadFn reads the current pause state. Tests override it to inject
// deterministic paused, malformed, and hard-error states.
//
// NOTE: The poll-loop hook variables below are package-level mutable state
// used as test seams. They prevent t.Parallel() within this package.
// Struct-based dependency injection would be needed for true parallel
// test safety.
var pauseReadFn = pause.Read

// pollWriteManifestFn refreshes .queue from a precomputed backlog view. Tests
// override it to observe or stub manifest updates.
var pollWriteManifestFn = pollWriteManifest

// pollClaimAndRunFn runs the claim-and-run phase. Tests override it to observe
// phase ordering.
var pollClaimAndRunFn = pollClaimAndRun

// pollReviewFn runs the review phase. Tests override it to observe phase
// ordering.
var pollReviewFn = pollReview

// pollMergeFn runs the merge phase. Tests override it to observe phase
// ordering.
var pollMergeFn = pollMerge

// nowFn returns the current time. Tests override it for deterministic heartbeat
// assertions.
var nowFn = time.Now

func pollWriteManifest(tasksDir string, failedDirExcluded map[string]struct{}, idx *queue.PollIndex) (queue.RunnableBacklogView, bool) {
	view := queue.ComputeRunnableBacklogView(tasksDir, idx)
	if err := queue.WriteQueueManifestFromView(tasksDir, failedDirExcluded, idx, view); err != nil {
		ui.Warnf("warning: could not write queue manifest: %v\n", err)
		return view, true
	}
	return view, false
}

// pollClaimAndRun uses the caller-provided runnable backlog view to derive the
// claim-order candidate list, select and claim a task from the backlog, write
// coordination messages, run the agent, and recover the task if the agent left
// it stuck in in-progress/. The failedDirExcluded map may be mutated when a
// FailedDirUnavailableError is encountered. It returns whether a task was
// claimed and whether any non-fatal error occurred.
func pollClaimAndRun(ctx context.Context, env envConfig, run runContext, tasksDir, agentID string, failedDirExcluded map[string]struct{}, cooldown time.Duration, idx *queue.PollIndex, view queue.RunnableBacklogView) (claimed bool, hadError bool) {
	candidates := queue.OrderedRunnableFilenames(view, failedDirExcluded)
	task, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, candidates, cooldown, idx)
	var fdErr *queue.FailedDirUnavailableError
	if errors.As(claimErr, &fdErr) {
		failedDirExcluded[fdErr.TaskFilename] = struct{}{}
		ui.Warnf("warning: excluding retry-exhausted task %s from future polls (failed/ directory unavailable)\n", fdErr.TaskFilename)
	} else if claimErr != nil {
		ui.Warnf("warning: could not claim task: %v\n", claimErr)
		hadError = true
	}

	if task == nil {
		return false, hadError
	}

	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "intent",
		Task:   task.Filename,
		Branch: task.Branch,
		Body:   "Starting work",
	}); err != nil {
		ui.Warnf("warning: could not write intent message: %v\n", err)
	}
	if err := messaging.WritePresence(tasksDir, agentID, task.Filename, task.Branch); err != nil {
		ui.Warnf("warning: could not write presence: %v\n", err)
	}
	if err := messaging.BuildAndWriteFileClaims(tasksDir, task.Filename); err != nil {
		ui.Warnf("warning: could not build file claims: %v\n", err)
	}

	if err := runOnce(ctx, env, run, task); err != nil {
		ui.Warnf("warning: agent run failed: %v\n", err)
	}

	recoverStuckTask(tasksDir, agentID, task)
	return true, hadError
}

// pollReview selects a review candidate from ready-for-review/, acquires a
// review lock, verifies the task branch exists, runs the review agent, and
// performs post-review actions (approve or reject). It returns whether a
// review was processed.
func pollReview(ctx context.Context, env envConfig, run runContext, tasksDir, branch, agentID string, idx *queue.PollIndex) bool {
	if ctx.Err() != nil {
		return false
	}

	reviewTask, reviewCleanup := selectAndLockReview(tasksDir, idx)
	if reviewTask == nil {
		return false
	}
	defer reviewCleanup()

	if !VerifyReviewBranch(env.repoRoot, tasksDir, reviewTask, agentID) {
		return true
	}

	fmt.Printf("Reviewing task %s on branch %s\n", reviewTask.Filename, reviewTask.Branch)
	if err := runReview(ctx, env, run, reviewTask, branch); err != nil {
		ui.Warnf("warning: review agent failed: %v\n", err)
	}
	postReviewAction(tasksDir, agentID, reviewTask)
	return true
}

// pollMerge acquires the merge lock and processes the squash-merge queue.
// It returns the number of tasks successfully merged.
func pollMerge(ctx context.Context, repoRoot, tasksDir, branch string) int {
	if ctx.Err() != nil {
		return 0
	}

	cleanup, ok := merge.AcquireLock(tasksDir)
	if !ok {
		return 0
	}
	defer cleanup()

	count := merge.ProcessQueueContext(ctx, repoRoot, tasksDir, branch)
	if count > 0 {
		fmt.Printf("Merged %d task(s) into %s\n", count, branch)
	}
	return count
}

// pollLoop is the main orchestration loop that claims tasks, runs agents,
// handles reviews, and processes merges. Depending on mode it either runs
// forever, runs exactly once, or exits after the queue becomes idle. The loop
// delegates to focused helpers for each concern and manages idle/backoff state
// locally.
type iterationResult struct {
	claimedTask     bool
	reviewProcessed bool
	mergeCount      int
	pollHadError    bool
	pauseActive     bool
	hasReviewTasks  bool
	hasReadyMerge   bool
}

type boundedRunState struct {
	hasClaimableBacklog bool
	hasReviewTasks      bool
	hasReadyMerge       bool
	pollHadError        bool
}

func (s boundedRunState) isIdle() bool {
	return !s.hasClaimableBacklog && !s.hasReviewTasks && !s.hasReadyMerge
}

func pollIterate(
	ctx context.Context,
	env envConfig,
	run runContext,
	repoRoot, tasksDir, branch, agentID string,
	cooldown time.Duration,
	hb *idleHeartbeat,
	failedDirExcluded map[string]struct{},
	priorPausedState bool,
) iterationResult {
	var result iterationResult

	pollCleanup(tasksDir)

	idx, reconcileHadError := pollReconcile(tasksDir)
	if reconcileHadError {
		result.pollHadError = true
	}

	view, manifestHadError := pollWriteManifestFn(tasksDir, failedDirExcluded, idx)
	if manifestHadError {
		result.pollHadError = true
	}

	ps1 := readPauseState(tasksDir)
	if !ps1.Active {
		claimedTask, claimHadError := pollClaimAndRunFn(ctx, env, run, tasksDir, agentID, failedDirExcluded, cooldown, idx, view)
		result.claimedTask = claimedTask
		if claimHadError {
			result.pollHadError = true
		}
	}

	if ctx.Err() != nil {
		result.pauseActive = ps1.Active
		return result
	}

	ps2 := readPauseState(tasksDir)
	if !ps2.Active {
		result.reviewProcessed = pollReviewFn(ctx, env, run, tasksDir, branch, agentID, idx)
	}

	if ctx.Err() == nil {
		result.mergeCount = pollMergeFn(ctx, repoRoot, tasksDir, branch)
	}
	result.pauseActive = ps2.Active

	now := nowFn().UTC()
	didWork := result.claimedTask || result.reviewProcessed || result.mergeCount > 0
	if didWork && !ps2.Active {
		hb.recordActivity(now)
	}

	pauseProblem := ps2.Problem
	if pauseProblem == "" {
		pauseProblem = ps1.Problem
	}
	if ps2.Active {
		if !priorPausedState {
			hb.recordActivity(now)
		}
		if msg := hb.pausedMessage(now, priorPausedState); msg != "" {
			fmt.Println(msg)
			if pauseProblem != "" {
				ui.Warnf("warning: pause sentinel: %s\n", pauseProblem)
			}
		}
	} else if priorPausedState {
		hb.recordActivity(now)
	}

	result.hasReviewTasks = hasReviewCandidates(tasksDir)
	result.hasReadyMerge = merge.HasReadyTasks(tasksDir)
	return result
}

func readPauseState(tasksDir string) pause.State {
	state, err := pauseReadFn(tasksDir)
	if err != nil {
		return pause.State{
			Active:      true,
			ProblemKind: pause.ProblemUnreadable,
			Problem:     fmt.Sprintf("stat error: %v", err),
		}
	}
	return state
}

func collectBoundedRunState(tasksDir string, failedDirExcluded map[string]struct{}, cooldown time.Duration) boundedRunState {
	var state boundedRunState

	idx, reconcileHadError := pollReconcile(tasksDir)
	if reconcileHadError {
		state.pollHadError = true
	}
	if _, manifestHadError := pollWriteManifestFn(tasksDir, failedDirExcluded, idx); manifestHadError {
		state.pollHadError = true
	}

	state.hasClaimableBacklog = queue.HasClaimableBacklogTask(tasksDir, failedDirExcluded, cooldown, idx)
	state.hasReviewTasks = hasReviewCandidates(tasksDir)
	state.hasReadyMerge = merge.HasReadyTasks(tasksDir)
	return state
}

func boundedRunExit(boundedErrorCount int) error {
	if boundedErrorCount == 0 {
		return nil
	}
	return fmt.Errorf("bounded run encountered %d poll cycle error(s)", boundedErrorCount)
}

func pollLoop(ctx context.Context, env envConfig, run runContext, repoRoot, tasksDir, branch, agentID string, cooldown time.Duration, mode RunMode) error {
	heartbeat := newIdleHeartbeat(nowFn().UTC())
	failedDirExcluded := make(map[string]struct{})
	consecutiveErrors := 0
	boundedErrorCount := 0
	priorPaused := false
	for {
		if ctx.Err() != nil {
			return nil
		}

		result := pollIterate(ctx, env, run, repoRoot, tasksDir, branch, agentID, cooldown, &heartbeat, failedDirExcluded, priorPaused)
		priorPaused = result.pauseActive

		// If a shutdown signal was received during the iteration, exit
		// immediately. This is unconditional — regardless of whether a
		// task was claimed — to avoid starting new work with a cancelled
		// context.
		if ctx.Err() != nil {
			return nil
		}

		cycleHadError := result.pollHadError
		boundedState := boundedRunState{}
		if mode == RunModeUntilIdle {
			boundedState = collectBoundedRunState(tasksDir, failedDirExcluded, cooldown)
			if boundedState.pollHadError {
				cycleHadError = true
			}
		}

		if cycleHadError {
			consecutiveErrors++
			if mode != RunModeDaemon {
				boundedErrorCount++
			}
			if consecutiveErrors == errBackoffThreshold {
				ui.Warnf("warning: entering backoff mode after %d consecutive poll errors\n", consecutiveErrors)
			}
		} else {
			if consecutiveErrors >= errBackoffThreshold {
				fmt.Printf("Poll succeeded, exiting backoff mode (was at %d consecutive errors)\n", consecutiveErrors)
			}
			consecutiveErrors = 0
		}

		switch mode {
		case RunModeOnce:
			return boundedRunExit(boundedErrorCount)
		case RunModeUntilIdle:
			if boundedState.isIdle() {
				return boundedRunExit(boundedErrorCount)
			}
		default:
			didWork := result.claimedTask || result.reviewProcessed || result.mergeCount > 0
			isIdle := !result.pauseActive && !result.claimedTask && !result.hasReviewTasks && !result.hasReadyMerge
			if isIdle {
				nextPoll := pollBackoff(consecutiveErrors)
				if msg := heartbeat.idleMessage(nowFn().UTC(), nextPoll); msg != "" {
					fmt.Println(msg)
				}
			} else if !result.pauseActive && !didWork {
				heartbeat.recordActivity(nowFn().UTC())
			}
		}

		select {
		case <-ctx.Done():
			fmt.Println("\nInterrupted. Exiting.")
			return nil
		case <-time.After(pollBackoff(consecutiveErrors)):
			// Even in bounded modes, keep the normal poll cadence between
			// iterations so queue draining does not devolve into a tight loop.
		}
	}
}

// surfaceBuildWarnings logs non-fatal build warnings from a PollIndex to
// stderr. It returns true when any warning indicates a directory-level read
// failure (incomplete index), which callers should treat as a poll-cycle error
// to trigger backoff signaling.
func surfaceBuildWarnings(idx *queue.PollIndex) bool {
	warnings := idx.BuildWarnings()
	if len(warnings) == 0 {
		return false
	}
	hasDirReadFailure := false
	for _, w := range warnings {
		ui.Warnf("warning: index build: %s (%s): %v\n", w.Path, w.State, w.Err)
		// Directory-level read failures produce paths without a .md
		// suffix; these mean the index is missing an entire queue
		// directory and downstream scheduling may be distorted.
		if !strings.HasSuffix(w.Path, ".md") {
			hasDirReadFailure = true
		}
	}
	return hasDirReadFailure
}

// appendToFileFn is the function used to append text to files in post-agent
// and review flows. It is a variable so tests can inject failures.
//
// NOTE: Package-level test seam — prevents t.Parallel(). See execCommandContext.
var appendToFileFn = atomicwrite.AppendToFile

func recordTaskStateUpdate(tasksDir, filename, action string, fn func(*taskstate.TaskState)) {
	if err := taskstate.Update(tasksDir, filename, fn); err != nil {
		ui.Warnf("warning: could not %s for %s: %v\n", action, filename, err)
	}
}

func recordSessionUpdate(tasksDir, kind, filename, action string, fn func(*sessionmeta.Session)) {
	if err := sessionmeta.Update(tasksDir, kind, filename, fn); err != nil {
		ui.Warnf("warning: could not %s for %s: %v\n", action, filename, err)
	}
}

func loadOrCreateSession(tasksDir, kind, filename, branch string) *sessionmeta.Session {
	session, err := sessionmeta.LoadOrCreate(tasksDir, kind, filename, branch)
	if err != nil {
		ui.Warnf("warning: could not prepare %s session for %s: %v\n", kind, filename, err)
		return nil
	}
	return session
}

func resetSession(tasksDir, kind, filename, branch string) string {
	session, err := sessionmeta.ResetSessionID(tasksDir, kind, filename, branch)
	if err != nil {
		ui.Warnf("warning: could not reset %s session for %s: %v\n", kind, filename, err)
		return ""
	}
	if session == nil {
		return ""
	}
	return session.CopilotSessionID
}

func runCopilotCommand(ctx context.Context, env envConfig, run runContext, extraEnvs []string, extraVolumes []string, label string, resetResumeSession func() string) error {
	runAttempt := func(current runContext) (error, bool) {
		args := buildDockerArgs(env, current, extraEnvs, extraVolumes)
		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, current.timeout)
		defer timeoutCancel()

		cmd := execCommandContext(timeoutCtx, "docker", args...)
		cmd.Cancel = func() error {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		cmd.WaitDelay = gracefulShutdownDelay
		cmd.Stdin = os.Stdin

		var stdoutDetect resumeDetectionBuffer
		var stderrDetect resumeDetectionBuffer
		cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutDetect)
		cmd.Stderr = io.MultiWriter(os.Stderr, &stderrDetect)

		err := cmd.Run()
		if timeoutCtx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "error: %s timed out after %v\n", label, current.timeout)
		} else if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "%s interrupted by signal\n", label)
		}
		return err, stdoutDetect.Matched() || stderrDetect.Matched()
	}

	err, rejected := runAttempt(run)
	if err == nil || strings.TrimSpace(run.resumeSessionID) == "" || resetResumeSession == nil || !rejected {
		return err
	}

	ui.Warnf("warning: Copilot resume rejected; retrying with a fresh session\n")
	if freshSessionID := strings.TrimSpace(resetResumeSession()); freshSessionID != "" && freshSessionID != run.resumeSessionID {
		run.resumeSessionID = freshSessionID
		freshErr, _ := runAttempt(run)
		return freshErr
	}
	return err
}

func resumeRejected(output string) bool {
	for _, rawLine := range strings.Split(output, "\n") {
		if resumeRejectedLine([]byte(rawLine)) {
			return true
		}
	}
	return false
}

func resumeRejectedBytes(output []byte) bool {
	for len(output) > 0 {
		line := output
		if i := bytes.IndexByte(output, '\n'); i >= 0 {
			line = output[:i]
			output = output[i+1:]
		} else {
			output = nil
		}
		if resumeRejectedLine(line) {
			return true
		}
	}
	return false
}

func resumeRejectedLine(rawLine []byte) bool {
	line := bytes.TrimSpace(rawLine)
	if len(line) == 0 {
		return false
	}
	lower := bytes.ToLower(line)
	if !bytes.Contains(lower, []byte("resume")) && !bytes.Contains(lower, []byte("session")) {
		return false
	}

	// Phrases that are unambiguous stale-session indicators on their own,
	// even without an accompanying "error"/"failed"/"invalid" keyword.
	for _, phrase := range [][]byte{
		[]byte("unknown session"),
		[]byte("session not found"),
		[]byte("session expired"),
	} {
		if bytes.Contains(lower, phrase) {
			return true
		}
	}

	// For all other markers, require an error-class keyword to avoid
	// false positives on unrelated output that mentions "session".
	if !bytes.Contains(lower, []byte("error")) && !bytes.Contains(lower, []byte("failed")) && !bytes.Contains(lower, []byte("invalid")) {
		return false
	}
	for _, marker := range [][]byte{
		[]byte("resume session"),
		[]byte("--resume"),
		[]byte("cannot resume"),
		[]byte("failed to resume"),
		[]byte("invalid session"),
		[]byte("resume rejected"),
		[]byte("invalid value for '--resume'"),
		[]byte("unknown option '--resume'"),
	} {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}
