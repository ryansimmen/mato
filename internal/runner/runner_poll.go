package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/queue"
	"mato/internal/queueview"
	"mato/internal/runtimedata"
	"mato/internal/ui"
)

const (
	// basePollInterval is the default polling interval between loop iterations.
	basePollInterval = 10 * time.Second

	// maxPollInterval is the upper bound for exponential backoff.
	maxPollInterval = 5 * time.Minute

	// errBackoffThreshold is the number of consecutive poll errors before
	// the loop enters backoff mode.
	errBackoffThreshold = 5
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
	if err := runtimedata.SweepTaskState(tasksDir); err != nil {
		ui.Warnf("warning: could not clean stale taskstate: %v\n", err)
	}
	if err := runtimedata.SweepSessions(tasksDir); err != nil {
		ui.Warnf("warning: could not clean stale sessionmeta: %v\n", err)
	}
}

// pollReconcile builds a poll index snapshot, surfaces any build warnings,
// and reconciles the ready queue (promoting waiting tasks whose dependencies
// are satisfied and quarantining exhausted ones). If reconciliation moved
// tasks the index is rebuilt. It returns the (possibly refreshed) index and
// whether any queue-structural error occurred during the cycle, including
// unreadable queue directories or parse-failed task files.
func pollReconcile(tasksDir string) (*queueview.PollIndex, bool) {
	idx := queueview.BuildIndex(tasksDir)
	hadError := surfaceBuildWarnings(idx)
	if len(idx.ParseFailures()) > 0 {
		hadError = true
	}

	if queue.ReconcileReadyQueue(tasksDir, idx) {
		idx = queueview.BuildIndex(tasksDir)
		if surfaceBuildWarnings(idx) {
			hadError = true
		}
	}

	return idx, hadError
}

func pollWriteManifest(tasksDir string, failedDirExcluded map[string]struct{}, idx *queueview.PollIndex) (queueview.RunnableBacklogView, bool) {
	view := queueview.ComputeRunnableBacklogView(tasksDir, idx)
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
func pollClaimAndRun(ctx context.Context, env envConfig, run runContext, tasksDir, agentID string, failedDirExcluded map[string]struct{}, cooldown time.Duration, idx *queueview.PollIndex, view queueview.RunnableBacklogView) (claimed bool, hadError bool) {
	summary := pollVerboseSummaryFromContext(ctx)
	candidates := queueview.OrderedRunnableFilenames(view, failedDirExcluded)
	task, claimErr := queue.SelectAndClaimTask(tasksDir, agentID, candidates, cooldown, idx)
	var fdErr *queue.FailedDirUnavailableError
	if errors.As(claimErr, &fdErr) {
		failedDirExcluded[fdErr.TaskFilename] = struct{}{}
		if summary != nil {
			summary.backlog = fmt.Sprintf("skipped %s (retry budget exhausted)", fdErr.TaskFilename)
		}
		ui.Warnf("warning: excluding retry-exhausted task %s from future polls (failed/ directory unavailable)\n", fdErr.TaskFilename)
	} else if claimErr != nil {
		if summary != nil {
			summary.backlog = fmt.Sprintf("claim error (%v)", claimErr)
		}
		ui.Warnf("warning: could not claim task: %v\n", claimErr)
		hadError = true
	}

	if task == nil {
		if summary != nil && summary.backlog == "" {
			if len(candidates) == 0 {
				summary.backlog = summarizeBacklogState(view, failedDirExcluded)
			} else {
				summary.backlog = fmt.Sprintf("skipped %d runnable candidate(s)", len(candidates))
			}
		}
		return false, hadError
	}
	if summary != nil {
		summary.backlog = fmt.Sprintf("claimed %s", task.Filename)
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
func pollReview(ctx context.Context, env envConfig, run runContext, tasksDir, branch, agentID string, idx *queueview.PollIndex) bool {
	summary := pollVerboseSummaryFromContext(ctx)
	if ctx.Err() != nil {
		if summary != nil {
			summary.review = "skipped cancelled"
		}
		return false
	}

	reviewTask, reviewCleanup := selectAndLockReview(tasksDir, idx)
	if reviewTask == nil {
		if summary != nil {
			if hasReviewCandidates(tasksDir) {
				summary.review = "skipped locked review task"
			} else {
				summary.review = "no review tasks"
			}
		}
		return false
	}
	defer reviewCleanup()

	if !VerifyReviewBranch(env.repoRoot, tasksDir, reviewTask, agentID) {
		if summary != nil {
			summary.review = fmt.Sprintf("skipped %s (missing branch)", reviewTask.Filename)
		}
		return true
	}

	fmt.Printf("Reviewing task %s on branch %s\n", reviewTask.Filename, reviewTask.Branch)
	if err := runReview(ctx, env, run, reviewTask, branch); err != nil {
		ui.Warnf("warning: review agent failed: %v\n", err)
	}
	postReviewAction(tasksDir, agentID, reviewTask)
	if summary != nil {
		summary.review = summarizeReviewOutcome(tasksDir, reviewTask)
	}
	return true
}

// pollMerge acquires the merge lock and processes the squash-merge queue.
// It returns the number of tasks successfully merged.
func pollMerge(ctx context.Context, repoRoot, tasksDir, branch string) int {
	summary := pollVerboseSummaryFromContext(ctx)
	if ctx.Err() != nil {
		if summary != nil {
			summary.merge = "skipped cancelled"
		}
		return 0
	}

	hasReadyTasks := false
	if summary != nil {
		hasReadyTasks = merge.HasReadyTasks(tasksDir)
	}
	cleanup, ok := merge.AcquireLock(tasksDir)
	if !ok {
		if summary != nil {
			if hasReadyTasks {
				summary.merge = "skipped merge lock held"
			} else {
				summary.merge = "no ready tasks"
			}
		}
		return 0
	}
	defer cleanup()

	count := merge.ProcessQueueContext(ctx, repoRoot, tasksDir, branch)
	if count > 0 {
		if summary != nil {
			summary.merge = fmt.Sprintf("merged %d into %s", count, branch)
		}
		fmt.Printf("Merged %d task(s) into %s\n", count, branch)
	} else if summary != nil {
		if hasReadyTasks {
			summary.merge = "ready tasks unchanged"
		} else {
			summary.merge = "no ready tasks"
		}
	}
	return count
}

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
	var summary *pollVerboseSummary
	if env.verbose {
		summary = &pollVerboseSummary{}
		ctx = withPollVerboseSummary(ctx, summary)
	}

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
	} else if summary != nil {
		summary.backlog = "skipped paused"
	}

	if ctx.Err() != nil {
		result.pauseActive = ps1.Active
		if summary != nil {
			if summary.review == "" {
				summary.review = "skipped cancelled"
			}
			if summary.merge == "" {
				summary.merge = "skipped cancelled"
			}
			emitPollCycleSummary(tasksDir, idx, view, failedDirExcluded, result, summary)
		}
		return result
	}

	ps2 := readPauseState(tasksDir)
	if !ps2.Active {
		result.reviewProcessed = pollReviewFn(ctx, env, run, tasksDir, branch, agentID, idx)
	} else if summary != nil {
		summary.review = "skipped paused"
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
	emitPollCycleSummary(tasksDir, idx, view, failedDirExcluded, result, summary)
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
