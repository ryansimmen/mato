package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/runtimecleanup"
	"mato/internal/sessionmeta"
	"mato/internal/taskfile"
	"mato/internal/taskstate"
)

const reviewContextPlaceholder = "REVIEW_CONTEXT_PLACEHOLDER"
const maxReviewContextReasonLen = 500

// reviewedRe matches the approval marker written by the review agent.
var reviewedRe = regexp.MustCompile(`<!-- reviewed:\s+\S+\s+at\s+\S+\s+—\s+approved\s*-->`)

// reviewRejectionRe matches the rejection marker written by the review agent.
// Requires the em-dash separator, a non-empty reason, and the closing -->.
var reviewRejectionRe = regexp.MustCompile(`<!-- review-rejection:\s+\S+\s+at\s+\S+\s+—\s+.+\s*-->`)

// reviewVerdict is the JSON structure written by the review agent to
// communicate its verdict to the host without using shell expansion.
type reviewVerdict struct {
	Verdict string `json:"verdict"` // "approve" or "reject"
	Reason  string `json:"reason"`  // rejection reason (empty for approvals)
}

// reviewCandidates scans ready-for-review/ and returns all review candidates
// sorted by priority (ascending) then filename. Tasks whose review retry
// budget is exhausted are moved to failed/ and excluded from the result.
//
// When idx is non-nil, pre-parsed metadata from the index is used instead of
// re-parsing each file from disk.
func reviewCandidates(tasksDir string, idx *queue.PollIndex) []*queue.ClaimedTask {
	failedDir := filepath.Join(tasksDir, queue.DirFailed)

	type candidate struct {
		task     *queue.ClaimedTask
		priority int
	}

	var candidates []candidate

	// Use index if available; otherwise fall back to filesystem scan.
	if idx != nil {
		// Quarantine malformed review tasks whose frontmatter could not be parsed.
		for _, pf := range idx.ReviewParseFailures() {
			fmt.Fprintf(os.Stderr, "warning: quarantining unparseable review candidate %s: %v\n", pf.Filename, pf.Err)
			if err := os.MkdirAll(failedDir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not create failed dir for %s: %v\n", pf.Filename, err)
				continue
			}
			if err := taskfile.AppendTerminalFailureRecord(pf.Path, fmt.Sprintf("unparseable frontmatter in ready-for-review: %v", pf.Err)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", pf.Filename, err)
			}
			if moveErr := queue.AtomicMove(pf.Path, filepath.Join(failedDir, pf.Filename)); moveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move malformed review task %s to failed: %v\n", pf.Filename, moveErr)
			} else {
				deleteTaskState(tasksDir, pf.Filename)
				fmt.Printf("quarantined malformed review candidate %s to failed/\n", pf.Filename)
			}
		}

		snaps := idx.TasksByState(queue.DirReadyReview)
		for _, snap := range snaps {
			maxRetries := snap.Meta.MaxRetries
			failures := snap.ReviewFailureCount
			if failures >= maxRetries {
				if err := os.MkdirAll(failedDir, 0o755); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not create failed dir for %s: %v\n", snap.Filename, err)
					continue
				}
				if err := taskfile.AppendTerminalFailureRecord(snap.Path, fmt.Sprintf("review retry budget exhausted (%d failures >= max_retries %d)", failures, maxRetries)); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", snap.Filename, err)
				}
				if moveErr := queue.AtomicMove(snap.Path, filepath.Join(failedDir, snap.Filename)); moveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: %v\n", snap.Filename, moveErr)
				} else {
					deleteTaskState(tasksDir, snap.Filename)
					fmt.Printf("review retry budget exhausted for %s (%d failures >= max_retries %d), moved to failed/\n",
						snap.Filename, failures, maxRetries)
				}
				continue
			}

			branch := snap.Branch
			if branch == "" {
				branch = "task/" + frontmatter.SanitizeBranchName(snap.Filename)
				activeBranches := idx.ActiveBranches()
				if _, taken := activeBranches[branch]; taken {
					branch = branch + "-" + frontmatter.BranchDisambiguator(snap.Filename)
				}
			}
			title := frontmatter.ExtractTitle(snap.Filename, snap.Body)
			candidates = append(candidates, candidate{
				task: &queue.ClaimedTask{
					Filename: snap.Filename,
					Branch:   branch,
					Title:    title,
					TaskPath: snap.Path,
				},
				priority: snap.Meta.Priority,
			})
		}
	} else {
		// Fallback: scan filesystem.
		reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
		names, err := queue.ListTaskFiles(reviewDir)
		if err != nil {
			return nil
		}

		for _, name := range names {
			path := filepath.Join(reviewDir, name)
			meta, body, err := frontmatter.ParseTaskFile(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: quarantining unparseable review candidate %s: %v\n", name, err)
				if mkErr := os.MkdirAll(failedDir, 0o755); mkErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not create failed dir for %s: %v\n", name, mkErr)
					continue
				}
				if appendErr := taskfile.AppendTerminalFailureRecord(path, fmt.Sprintf("unparseable frontmatter in ready-for-review: %v", err)); appendErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", name, appendErr)
				}
				if moveErr := queue.AtomicMove(path, filepath.Join(failedDir, name)); moveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not move malformed review task %s to failed: %v\n", name, moveErr)
				} else {
					deleteTaskState(tasksDir, name)
					fmt.Printf("quarantined malformed review candidate %s to failed/\n", name)
				}
				continue
			}

			maxRetries := meta.MaxRetries
			failures, failErr := queue.CountReviewFailureLines(path)
			if failErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not count failures for review candidate %s, skipping: %v\n", name, failErr)
				continue
			}
			if failures >= maxRetries {
				dst := filepath.Join(failedDir, name)
				if err := os.MkdirAll(failedDir, 0o755); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not create failed dir for %s: %v\n", name, err)
					continue
				}
				if err := taskfile.AppendTerminalFailureRecord(path, fmt.Sprintf("review retry budget exhausted (%d failures >= max_retries %d)", failures, maxRetries)); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", name, err)
				}
				if moveErr := queue.AtomicMove(path, dst); moveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: %v\n", name, moveErr)
				} else {
					deleteTaskState(tasksDir, name)
					fmt.Printf("review retry budget exhausted for %s (%d failures >= max_retries %d), moved to failed/\n",
						name, failures, maxRetries)
				}
				continue
			}

			branch := taskfile.ParseBranch(path)
			if branch == "" {
				branch = "task/" + frontmatter.SanitizeBranchName(name)
				if _, taken := queue.CollectActiveBranches(tasksDir, nil)[branch]; taken {
					branch = branch + "-" + frontmatter.BranchDisambiguator(name)
				}
			}
			title := frontmatter.ExtractTitle(name, body)
			candidates = append(candidates, candidate{
				task: &queue.ClaimedTask{
					Filename: name,
					Branch:   branch,
					Title:    title,
					TaskPath: path,
				},
				priority: meta.Priority,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].task.Filename < candidates[j].task.Filename
	})

	result := make([]*queue.ClaimedTask, len(candidates))
	for i, c := range candidates {
		result[i] = c.task
	}
	return result
}

// hasReviewCandidates reports whether ready-for-review/ currently contains at
// least one parseable task that has not exhausted its review retry budget.
//
// This helper is intentionally read-only. It is used by idle reporting after
// merge processing, where we need a fresh filesystem view of ready-for-review/
// without the side effects of quarantining malformed files or failing exhausted
// tasks.
func hasReviewCandidates(tasksDir string) bool {
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	names, err := queue.ListTaskFiles(reviewDir)
	if err != nil {
		return false
	}

	for _, name := range names {
		path := filepath.Join(reviewDir, name)
		meta, _, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			continue
		}
		failures, failErr := queue.CountReviewFailureLines(path)
		if failErr != nil {
			continue
		}
		if failures >= meta.MaxRetries {
			continue
		}
		return true
	}

	return false
}

// selectTaskForReview scans ready-for-review/ and returns the highest-priority
// task that needs review. Returns nil if no tasks need review.
// This does not acquire a lock; use selectAndLockReview for mutual exclusion.
//
// When idx is nil, the filesystem is scanned directly.
func selectTaskForReview(tasksDir string, idx *queue.PollIndex) *queue.ClaimedTask {
	candidates := reviewCandidates(tasksDir, idx)
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

// SelectTaskForReview is the exported entry point for selecting the
// highest-priority review candidate. It delegates to selectTaskForReview.
func SelectTaskForReview(tasksDir string, idx *queue.PollIndex) *queue.ClaimedTask {
	return selectTaskForReview(tasksDir, idx)
}

// selectAndLockReview returns the highest-priority review candidate that this
// agent can exclusively lock, along with a cleanup function to release the
// lock. Returns (nil, nil) when no unlocked review task is available.
//
// When idx is nil, the filesystem is scanned directly.
func selectAndLockReview(tasksDir string, idx *queue.PollIndex) (*queue.ClaimedTask, func()) {
	for _, task := range reviewCandidates(tasksDir, idx) {
		cleanup, ok := queue.AcquireReviewLock(tasksDir, task.Filename)
		if ok {
			return task, cleanup
		}
	}
	return nil, nil
}

func runReview(ctx context.Context, env envConfig, run runContext, task *queue.ClaimedTask, branch string) error {
	run.prompt = strings.ReplaceAll(reviewInstructions, "TASKS_DIR_PLACEHOLDER", env.workdir+"/"+dirs.Root)
	run.prompt = strings.ReplaceAll(run.prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	run.prompt = strings.ReplaceAll(run.prompt, "MESSAGES_DIR_PLACEHOLDER", env.workdir+"/"+dirs.Root+"/messages")
	run.model = env.reviewModel
	run.reasoningEffort = env.reviewReasoningEffort
	currentTip := resolveReviewBranchTip(env.repoRoot, task)
	state := loadTaskStateForReview(env.tasksDir, task.Filename)
	previousRejection := lastReviewRejectionReason(task.TaskPath)
	run.prompt = strings.ReplaceAll(run.prompt, reviewContextPlaceholder, buildReviewContext(task, currentTip, state, previousRejection))

	cloneDir, err := git.CreateClone(env.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone for review: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureReceiveDeny(env.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	run.cloneDir = cloneDir
	if env.reviewSessionResumeEnabled {
		session := loadOrCreateSession(env.tasksDir, sessionmeta.KindReview, task.Filename, task.Branch)
		if session != nil {
			run.resumeSessionID = session.CopilotSessionID
		}
	}
	recordTaskStateUpdate(env.tasksDir, task.Filename, "record review launch taskstate", func(state *taskstate.TaskState) {
		state.TaskBranch = task.Branch
		state.TargetBranch = branch
		state.LastOutcome = "review-launched"
	})
	if env.reviewSessionResumeEnabled {
		recordSessionUpdate(env.tasksDir, sessionmeta.KindReview, task.Filename, "record review session", func(session *sessionmeta.Session) {
			session.TaskBranch = task.Branch
			if currentTip != "unknown" {
				session.LastHeadSHA = currentTip
			}
		})
	}

	fmt.Printf("Launching review agent from %s (clone: %s)\n", env.repoRoot, cloneDir)

	extraEnvs := []string{
		"MATO_REVIEW_MODE=1",
		"MATO_TASK_FILE=" + task.Filename,
		"MATO_TASK_BRANCH=" + task.Branch,
		"MATO_TASK_TITLE=" + task.Title,
		fmt.Sprintf("MATO_TASK_PATH=%s/%s/%s/%s", env.workdir, dirs.Root, queue.DirReadyReview, task.Filename),
		fmt.Sprintf("MATO_REVIEW_VERDICT_PATH=%s/%s/messages/verdict-%s.json", env.workdir, dirs.Root, task.Filename),
	}

	return runCopilotCommand(ctx, env, run, extraEnvs, nil, "review agent", func() string {
		if !env.reviewSessionResumeEnabled {
			return ""
		}
		return resetSession(env.tasksDir, sessionmeta.KindReview, task.Filename, task.Branch)
	})
}

// reviewDisposition captures the three values that differ between an approval
// and a rejection: destination directory, message body, and log prefix.
type reviewDisposition struct {
	dir         string
	messageBody string
	logPrefix   string
}

var approveDisposition = reviewDisposition{
	dir:         queue.DirReadyMerge,
	messageBody: "Review approved, ready for merge",
	logPrefix:   "Review approved",
}

var rejectDisposition = reviewDisposition{
	dir:         queue.DirBacklog,
	messageBody: "Review rejected",
	logPrefix:   "Review rejected",
}

// resolveReviewVerdict reads the task file and checks for approval or rejection
// markers written directly by the review agent (backward compatibility path).
// Returns "approve", "reject", or "" if neither marker is found.
func resolveReviewVerdict(task *queue.ClaimedTask) string {
	taskData, err := os.ReadFile(task.TaskPath)
	if err != nil {
		return ""
	}
	content := string(taskData)
	if reviewedRe.MatchString(content) {
		return "approve"
	}
	if reviewRejectionRe.MatchString(content) {
		return "reject"
	}
	return ""
}

// VerifyReviewBranch checks whether the task branch exists in the host repo
// before launching a review agent. If the branch is missing it records a
// review-failure marker and returns false. Returns true when the branch
// exists and the review may proceed.
func VerifyReviewBranch(repoRoot, tasksDir string, task *queue.ClaimedTask, agentID string) bool {
	if _, err := git.Output(repoRoot, "rev-parse", "--verify", "refs/heads/"+task.Branch); err != nil {
		fmt.Fprintf(os.Stderr, "warning: task branch %s missing from host repo, recording review failure for %s\n", task.Branch, task.Filename)
		appendReviewFailure(task.TaskPath, agentID, "task branch "+task.Branch+" not found in host repo")
		recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
			state.TaskBranch = task.Branch
			state.LastOutcome = "review-branch-missing"
		})
		return false
	}
	return true
}

// PostReviewAction is the exported entry point for the host-side review
// handoff after a review agent exits. It delegates to the internal
// postReviewAction implementation.
func PostReviewAction(tasksDir, agentID string, task *queue.ClaimedTask) {
	postReviewAction(tasksDir, agentID, task)
}

// postReviewAction reads the verdict file written by the review agent and
// handles the result. If approved, the host writes the approval marker and
// moves the task to ready-to-merge/. If rejected, writes rejection marker
// and moves to backlog/. If no verdict file exists, writes a review-failure.
func postReviewAction(tasksDir, agentID string, task *queue.ClaimedTask) {
	// Task must still be in ready-for-review/ (agent no longer moves files).
	if _, err := os.Stat(task.TaskPath); err != nil {
		return
	}

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+task.Filename+".json")
	defer os.Remove(verdictPath) // clean up regardless of outcome

	data, err := os.ReadFile(verdictPath)
	if err != nil {
		// No verdict file: review agent crashed or failed to write verdict.
		// Fall back to checking the task file for markers (backward compat).
		switch resolveReviewVerdict(task) {
		case "approve":
			moveReviewedTask(tasksDir, agentID, task, approveDisposition)
		case "reject":
			moveReviewedTask(tasksDir, agentID, task, rejectDisposition)
		default:
			recorded := appendReviewFailure(task.TaskPath, agentID, "review agent exited without rendering a verdict")
			recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
				state.LastOutcome = "review-incomplete"
			})
			logReviewFailureOutcome("Review incomplete", task.Filename, recorded, "")
		}
		return
	}

	var verdict reviewVerdict
	if err := json.Unmarshal(data, &verdict); err != nil {
		recorded := appendReviewFailure(task.TaskPath, agentID, fmt.Sprintf("could not parse verdict file: %v", err))
		recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
			state.LastOutcome = "review-incomplete"
		})
		logReviewFailureOutcome("Review incomplete", task.Filename, recorded, "malformed verdict file")
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	switch strings.ToLower(strings.TrimSpace(verdict.Verdict)) {
	case "approve":
		// Write approval marker to task file.
		if err := appendToFileFn(task.TaskPath, fmt.Sprintf("\n<!-- reviewed: %s at %s — approved -->\n", agentID, now)); err != nil {
			recorded := appendReviewFailure(task.TaskPath, agentID, fmt.Sprintf("could not write approval marker: %v", err))
			recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
				state.LastOutcome = "review-incomplete"
			})
			fmt.Fprintf(os.Stderr, "warning: could not write approval marker: %v\n", err)
			logReviewFailureOutcome("Review incomplete", task.Filename, recorded, "")
			return
		}
		moveReviewedTask(tasksDir, agentID, task, approveDisposition)

	case "reject":
		reason := taskfile.SanitizeCommentText(verdict.Reason)
		if reason == "" {
			reason = "no reason provided"
		}
		if err := appendToFileFn(task.TaskPath, fmt.Sprintf("\n<!-- review-rejection: %s at %s — %s -->\n", agentID, now, reason)); err != nil {
			recorded := appendReviewFailure(task.TaskPath, agentID, fmt.Sprintf("could not write rejection marker: %v", err))
			recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
				state.LastOutcome = "review-incomplete"
			})
			fmt.Fprintf(os.Stderr, "warning: could not write rejection marker: %v\n", err)
			logReviewFailureOutcome("Review incomplete", task.Filename, recorded, "")
			return
		}
		moveReviewedTask(tasksDir, agentID, task, rejectDisposition)

	case "error":
		reason := taskfile.SanitizeCommentText(verdict.Reason)
		if reason == "" {
			reason = "review agent reported an error"
		}
		recorded := appendReviewFailure(task.TaskPath, agentID, reason)
		recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
			state.LastOutcome = "review-error"
		})
		logReviewFailureOutcome("Review error", task.Filename, recorded, reason)

	default:
		recorded := appendReviewFailure(task.TaskPath, agentID, fmt.Sprintf("unknown verdict: %q", verdict.Verdict))
		recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
			state.LastOutcome = "review-incomplete"
		})
		logReviewFailureOutcome("Review incomplete", task.Filename, recorded, fmt.Sprintf("unknown verdict %q", verdict.Verdict))
	}
}

// moveReviewedTask moves a reviewed task to the destination directory specified
// by the disposition and sends a completion message. It uses queue.AtomicMove
// (os.Link + os.Remove) to prevent silently overwriting an existing file at
// the destination (TOCTOU race defense).
func moveReviewedTask(tasksDir, agentID string, task *queue.ClaimedTask, disp reviewDisposition) {
	dst := filepath.Join(tasksDir, disp.dir, task.Filename)
	if err := queue.AtomicMove(task.TaskPath, dst); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not move reviewed task %s to %s: %v\n", task.Filename, disp.dir, err)
		return
	}
	outcome := "review-rejected"
	if disp.dir == queue.DirReadyMerge {
		outcome = "review-approved"
	}
	recordTaskStateUpdate(tasksDir, task.Filename, "record review outcome taskstate", func(state *taskstate.TaskState) {
		if strings.TrimSpace(state.LastHeadSHA) != "" {
			state.LastReviewedSHA = strings.TrimSpace(state.LastHeadSHA)
		}
		state.LastOutcome = outcome
	})
	messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "completion",
		Task:   task.Filename,
		Branch: task.Branch,
		Body:   disp.messageBody,
	})
	fmt.Printf("%s: moved %s to %s/\n", disp.logPrefix, task.Filename, disp.dir)
}

// appendReviewFailure writes a review-failure comment to the task file.
// The task stays in ready-for-review/ for a future review attempt.
func appendReviewFailure(taskPath, agentID, reason string) bool {
	if err := taskfile.AppendReviewFailure(taskPath, agentID, reason); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return false
	}
	return true
}

func logReviewFailureOutcome(prefix, filename string, recorded bool, detail string) {
	if detail != "" {
		if recorded {
			fmt.Printf("%s: recorded review-failure for %s: %s\n", prefix, filename, detail)
			return
		}
		fmt.Printf("%s: could not record review-failure for %s: %s\n", prefix, filename, detail)
		return
	}
	if recorded {
		fmt.Printf("%s: recorded review-failure for %s\n", prefix, filename)
		return
	}
	fmt.Printf("%s: could not record review-failure for %s\n", prefix, filename)
}

// extractReviewRejections reads review-rejection comments from the task file,
// joined by newlines. Returns "" if none found or file cannot be read.
func extractReviewRejections(taskPath string) string {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return ""
	}
	return taskfile.ExtractReviewRejections(data)
}

func loadTaskStateForReview(tasksDir, filename string) *taskstate.TaskState {
	state, err := taskstate.Load(tasksDir, filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load taskstate for %s: %v\n", filename, err)
		return nil
	}
	return state
}

func resolveReviewBranchTip(repoRoot string, task *queue.ClaimedTask) string {
	tip, err := git.Output(repoRoot, "rev-parse", "refs/heads/"+task.Branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not resolve review branch tip for %s: %v\n", task.Filename, err)
		return "unknown"
	}
	tip = strings.TrimSpace(tip)
	if tip == "" {
		return "unknown"
	}
	return tip
}

func buildReviewContext(task *queue.ClaimedTask, currentTip string, state *taskstate.TaskState, previousRejection string) string {
	// Keep this defensive guard so direct tests and future callers can pass an
	// empty tip without first routing through resolveReviewBranchTip.
	if strings.TrimSpace(currentTip) == "" {
		currentTip = "unknown"
	}
	lastReviewed := "none"
	if state != nil && strings.TrimSpace(state.LastReviewedSHA) != "" {
		lastReviewed = strings.TrimSpace(state.LastReviewedSHA)
	}
	previousRejection = strings.TrimSpace(previousRejection)
	if previousRejection == "" {
		previousRejection = "none"
	} else if utf8.RuneCountInString(previousRejection) > maxReviewContextReasonLen {
		previousRejection = truncateRunes(previousRejection, maxReviewContextReasonLen)
	}
	reviewMode := "initial review; assess the current diff independently"
	if lastReviewed != "none" || previousRejection != "none" {
		reviewMode = "follow-up review; reassess the current diff independently"
	}
	return strings.Join([]string{
		"Review context:",
		"- task branch: " + task.Branch,
		"- current branch tip: " + currentTip,
		"- last reviewed branch tip: " + lastReviewed,
		"- previous rejection: " + previousRejection,
		"- review mode: " + reviewMode,
	}, "\n")
}

func lastReviewRejectionReason(taskPath string) string {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return ""
	}
	return taskfile.LastReviewRejectionReason(data)
}

func deleteTaskState(tasksDir, filename string) {
	runtimecleanup.DeleteAll(tasksDir, filename)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return "... [truncated]"
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "... [truncated]"
}
