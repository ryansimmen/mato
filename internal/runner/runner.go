// Package runner manages the agent lifecycle including Docker-based task
// execution, review orchestration, and the top-level poll loop that drives
// claiming, running, and merging tasks.
package runner

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/identity"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/queue"
)

//go:embed task-instructions.md
var taskInstructions string

//go:embed review-instructions.md
var reviewInstructions string

// defaultAgentTimeout is the default execution timeout for Docker agent
// containers. Override with MATO_AGENT_TIMEOUT (Go duration string).
const defaultAgentTimeout = 30 * time.Minute

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

// DryRun validates the task queue setup without launching Docker containers.
// It runs one iteration of queue management (dependency promotion, overlap
// detection, manifest writing) and reports the results, then exits.
func DryRun(repoRoot, branch, tasksDirOverride string) error {
	repoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	tasksDir := tasksDirOverride
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}
	tasksDir, err = validateTasksDir(tasksDir)
	if err != nil {
		return err
	}

	subdirs := queue.AllDirs

	// Verify directory structure
	missingDirs := 0
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			fmt.Fprintf(os.Stderr, "warning: missing directory: %s/\n", sub)
			missingDirs++
		}
	}
	if missingDirs > 0 {
		return fmt.Errorf("%d required queue directories missing — run `mato init` to create them", missingDirs)
	}

	// Parse all task files and report errors
	fmt.Println("=== Task File Validation ===")
	parseErrors := 0
	totalTasks := 0
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		names, readErr := queue.ListTaskFiles(dir)
		if readErr != nil {
			continue
		}
		for _, name := range names {
			totalTasks++
			path := filepath.Join(dir, name)
			if _, _, parseErr := frontmatter.ParseTaskFile(path); parseErr != nil {
				fmt.Printf("  ERROR %s/%s: %v\n", sub, name, parseErr)
				parseErrors++
			}
		}
	}
	if parseErrors > 0 {
		fmt.Printf("  %d of %d task file(s) have parse errors\n", parseErrors, totalTasks)
	} else {
		fmt.Printf("  All %d task file(s) parsed successfully\n", totalTasks)
	}

	// Run dependency reconciliation (read-only: report what would be promoted)
	fmt.Println("\n=== Dependency Resolution ===")
	promotable := queue.CountPromotableWaitingTasks(tasksDir, nil)
	if promotable > 0 {
		fmt.Printf("  %d task(s) in waiting/ would be promoted to backlog/\n", promotable)
	} else {
		fmt.Println("  No waiting tasks ready for promotion")
	}

	// Detect affects conflicts
	fmt.Println("\n=== Affects Conflict Detection ===")
	detailed := queue.DeferredOverlappingTasksDetailed(tasksDir, nil)
	if len(detailed) > 0 {
		// Sort deferred task names for stable output.
		names := make([]string, 0, len(detailed))
		for name := range detailed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			info := detailed[name]
			fmt.Printf("  DEFERRED %s (blocked by %s in %s/, conflicting affects: %v)\n",
				name, info.BlockedBy, info.BlockedByDir, info.ConflictingAffects)
		}
	} else {
		fmt.Println("  No affects conflicts detected")
	}

	// Compute and display queue manifest (read-only: no file written)
	fmt.Println("\n=== Queue Manifest ===")
	deferredSimple := make(map[string]struct{}, len(detailed))
	for name := range detailed {
		deferredSimple[name] = struct{}{}
	}
	manifest, manifestErr := queue.ComputeQueueManifest(tasksDir, deferredSimple, nil)
	if manifestErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not compute queue manifest: %v\n", manifestErr)
	}
	if len(strings.TrimSpace(manifest)) == 0 {
		fmt.Println("  (queue is empty)")
	} else {
		for i, line := range strings.Split(strings.TrimSpace(manifest), "\n") {
			fmt.Printf("  %d. %s\n", i+1, line)
		}
	}

	// Summary counts
	fmt.Println("\n=== Queue Summary ===")
	for _, sub := range subdirs {
		dir := filepath.Join(tasksDir, sub)
		names, _ := queue.ListTaskFiles(dir)
		fmt.Printf("  %-20s %d\n", sub, len(names))
	}
	if len(detailed) > 0 {
		fmt.Printf("  %-20s %d\n", "deferred", len(detailed))
	}

	fmt.Println("\nDry run complete (read-only). No files were modified and no Docker containers were launched.")
	return nil
}

func Run(repoRoot, branch, tasksDirOverride string, copilotArgs []string) error {
	repoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	if err := git.EnsureBranch(repoRoot, branch); err != nil {
		return err
	}

	tasksDir := tasksDirOverride
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}
	tasksDir, err = validateTasksDir(tasksDir)
	if err != nil {
		return err
	}

	if err := checkDocker(); err != nil {
		return err
	}

	for _, sub := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create .tasks subdirectory %s: %w", sub, err)
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

	changed, err := git.EnsureGitignoreContains(repoRoot, "/.tasks/")
	if err != nil {
		return err
	}
	if changed {
		if err := git.CommitGitignore(repoRoot, "chore: add /.tasks/ to .gitignore"); err != nil {
			return err
		}
	}

	agentTimeout, err := parseAgentTimeout(os.Getenv("MATO_AGENT_TIMEOUT"))
	if err != nil {
		return err
	}

	tools, err := discoverHostTools()
	if err != nil {
		return err
	}

	cfg, run := buildEnvAndRunContext(branch, tools, agentID, gitName, gitEmail, copilotArgs, repoRoot, tasksDir, agentTimeout)

	if err := ensureDockerImage(cfg.image); err != nil {
		return err
	}

	ctx, cancel := setupSignalContext()
	defer cancel()
	defer signal.Stop(signalChan(ctx))

	return pollLoop(ctx, cfg, run, repoRoot, tasksDir, branch, agentID)
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
func buildEnvAndRunContext(branch string, tools hostTools, agentID, gitName, gitEmail string, copilotArgs []string, repoRoot, tasksDir string, timeout time.Duration) (envConfig, runContext) {
	image := os.Getenv("MATO_DOCKER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	workdir := "/workspace"

	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/.tasks")
	prompt = strings.ReplaceAll(prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	prompt = strings.ReplaceAll(prompt, "MESSAGES_DIR_PLACEHOLDER", workdir+"/.tasks/messages")

	env := envConfig{
		image:              image,
		workdir:            workdir,
		copilotPath:        tools.copilotPath,
		gitPath:            tools.gitPath,
		gitUploadPackPath:  tools.gitUploadPackPath,
		gitReceivePackPath: tools.gitReceivePackPath,
		ghPath:             tools.ghPath,
		goRoot:             tools.goRoot,
		copilotConfigDir:   tools.copilotConfigDir,
		copilotCacheDir:    tools.copilotCacheDir,
		gitName:            gitName,
		gitEmail:           gitEmail,
		homeDir:            tools.homeDir,
		ghConfigDir:        tools.ghConfigDir,
		hasGhConfig:        tools.hasGhConfig,
		gitTemplatesDir:    tools.gitTemplatesDir,
		hasGitTemplates:    tools.hasGitTemplates,
		systemCertsDir:     tools.systemCertsDir,
		hasSystemCerts:     tools.hasSystemCerts,
		copilotArgs:        copilotArgs,
		repoRoot:           repoRoot,
		tasksDir:           tasksDir,
		targetBranch:       branch,
		isTTY:              isTerminal(os.Stdin),
	}

	run := runContext{
		prompt:  prompt,
		agentID: agentID,
		timeout: timeout,
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
	queue.RecoverOrphanedTasks(tasksDir)
	queue.CleanStaleLocks(tasksDir)
	queue.CleanStaleReviewLocks(tasksDir)
	messaging.CleanStalePresence(tasksDir)
	messaging.CleanOldMessages(tasksDir, 24*time.Hour)
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

// pollClaimAndRun computes the deferred task set, selects and claims a task
// from the backlog, writes coordination messages, runs the agent, and
// recovers the task if the agent left it stuck in in-progress/. The
// failedDirExcluded map may be mutated when a FailedDirUnavailableError is
// encountered. It returns whether a task was claimed and whether any
// non-fatal error occurred.
func pollClaimAndRun(ctx context.Context, env envConfig, run runContext, tasksDir, agentID string, failedDirExcluded map[string]struct{}, idx *queue.PollIndex) (claimed bool, hadError bool) {
	deferred := queue.DeferredOverlappingTasks(tasksDir, idx)
	for name := range failedDirExcluded {
		deferred[name] = struct{}{}
	}
	if err := queue.WriteQueueManifest(tasksDir, deferred, idx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write queue manifest: %v\n", err)
		hadError = true
	}

	task, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, deferred, idx)
	var fdErr *queue.FailedDirUnavailableError
	if errors.As(claimErr, &fdErr) {
		failedDirExcluded[fdErr.TaskFilename] = struct{}{}
		fmt.Fprintf(os.Stderr, "warning: excluding retry-exhausted task %s from future polls (failed/ directory unavailable)\n", fdErr.TaskFilename)
	} else if claimErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not claim task: %v\n", claimErr)
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
		fmt.Fprintf(os.Stderr, "warning: could not write intent message: %v\n", err)
	}
	if err := messaging.WritePresence(tasksDir, agentID, task.Filename, task.Branch); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write presence: %v\n", err)
	}
	if err := messaging.BuildAndWriteFileClaims(tasksDir, task.Filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not build file claims: %v\n", err)
	}

	if err := runOnce(ctx, env, run, task); err != nil {
		fmt.Fprintf(os.Stderr, "warning: agent run failed: %v\n", err)
	}

	recoverStuckTask(tasksDir, agentID, task)
	return true, hadError
}

// pollReview selects a review candidate from ready-for-review/, acquires a
// review lock, verifies the task branch exists, runs the review agent, and
// performs post-review actions (approve or reject). It returns whether a
// review was processed.
func pollReview(ctx context.Context, env envConfig, run runContext, tasksDir, branch, agentID string, idx *queue.PollIndex) bool {
	reviewTask, reviewCleanup := selectAndLockReview(tasksDir, idx)
	if reviewTask == nil {
		return false
	}
	defer reviewCleanup()

	if !VerifyReviewBranch(env.repoRoot, reviewTask, agentID) {
		return true
	}

	fmt.Printf("Reviewing task %s on branch %s\n", reviewTask.Filename, reviewTask.Branch)
	if err := runReview(ctx, env, run, reviewTask, branch); err != nil {
		fmt.Fprintf(os.Stderr, "warning: review agent failed: %v\n", err)
	}
	postReviewAction(tasksDir, agentID, reviewTask)
	return true
}

// pollMerge acquires the merge lock and processes the squash-merge queue.
// It returns the number of tasks successfully merged.
func pollMerge(repoRoot, tasksDir, branch string) int {
	cleanup, ok := merge.AcquireLock(tasksDir)
	if !ok {
		return 0
	}
	defer cleanup()

	count := merge.ProcessQueue(repoRoot, tasksDir, branch)
	if count > 0 {
		fmt.Printf("Merged %d task(s) into %s\n", count, branch)
	}
	return count
}

// pollLoop is the main orchestration loop that claims tasks, runs agents,
// handles reviews, and processes merges. It runs until the context is
// cancelled (via signal). The loop delegates to focused helpers for each
// concern and manages idle/backoff state locally.
func pollLoop(ctx context.Context, env envConfig, run runContext, repoRoot, tasksDir, branch, agentID string) error {
	heartbeat := newIdleHeartbeat(time.Now())
	failedDirExcluded := make(map[string]struct{})
	consecutiveErrors := 0
	for {
		if ctx.Err() != nil {
			return nil
		}

		pollHadError := false

		pollCleanup(tasksDir)

		idx, reconcileHadError := pollReconcile(tasksDir)
		if reconcileHadError {
			pollHadError = true
		}

		claimedTask, claimHadError := pollClaimAndRun(ctx, env, run, tasksDir, agentID, failedDirExcluded, idx)
		if claimHadError {
			pollHadError = true
		}

		// If a shutdown signal was received during the task run, exit
		// now that the task has been properly recovered. This avoids
		// starting review or merge work with a cancelled context.
		if claimedTask && ctx.Err() != nil {
			return nil
		}

		reviewProcessed := pollReview(ctx, env, run, tasksDir, branch, agentID, idx)
		mergeCount := pollMerge(repoRoot, tasksDir, branch)

		didWork := claimedTask || reviewProcessed || mergeCount > 0
		if didWork {
			heartbeat.recordActivity(time.Now())
		}

		hasReviewTasks := selectTaskForReview(tasksDir, idx) != nil
		isIdle := !claimedTask && !hasReviewTasks && !merge.HasReadyTasks(tasksDir)
		if isIdle {
			nextPoll := pollBackoff(consecutiveErrors)
			if msg := heartbeat.idleMessage(time.Now(), nextPoll); msg != "" {
				fmt.Println(msg)
			}
		} else if !didWork {
			heartbeat.recordActivity(time.Now())
		}

		if pollHadError {
			consecutiveErrors++
			if consecutiveErrors == errBackoffThreshold {
				fmt.Fprintf(os.Stderr, "warning: entering backoff mode after %d consecutive poll errors\n", consecutiveErrors)
			}
		} else {
			if consecutiveErrors >= errBackoffThreshold {
				fmt.Printf("Poll succeeded, exiting backoff mode (was at %d consecutive errors)\n", consecutiveErrors)
			}
			consecutiveErrors = 0
		}

		select {
		case <-ctx.Done():
			fmt.Println("\nInterrupted. Exiting.")
			return nil
		case <-time.After(pollBackoff(consecutiveErrors)):
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
		fmt.Fprintf(os.Stderr, "warning: index build: %s (%s): %v\n", w.Path, w.State, w.Err)
		// Directory-level read failures produce paths without a .md
		// suffix; these mean the index is missing an entire queue
		// directory and downstream scheduling may be distorted.
		if !strings.HasSuffix(w.Path, ".md") {
			hasDirReadFailure = true
		}
	}
	return hasDirReadFailure
}

// checkIdleTransition returns true when the system transitions from active to
// idle, so the caller should print the idle message exactly once per idle period.
func checkIdleTransition(isIdle bool, wasIdle *bool) bool {
	shouldPrint := isIdle && !*wasIdle
	*wasIdle = isIdle
	return shouldPrint
}

// appendToFileFn is the function used to append text to files in post-agent
// and review flows. It is a variable so tests can inject failures.
var appendToFileFn = atomicwrite.AppendToFile
