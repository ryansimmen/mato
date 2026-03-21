package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/taskfile"
)

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
		snaps := idx.TasksByState(queue.DirReadyReview)
		for _, snap := range snaps {
			maxRetries := snap.Meta.MaxRetries
			failures := snap.ReviewFailureCount
			if failures >= maxRetries {
				if err := os.MkdirAll(failedDir, 0o755); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not create failed dir for %s: %v\n", snap.Filename, err)
					continue
				}
				if moveErr := queue.AtomicMove(snap.Path, filepath.Join(failedDir, snap.Filename)); moveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: %v\n", snap.Filename, moveErr)
				} else {
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
				fmt.Fprintf(os.Stderr, "warning: could not parse review candidate %s: %v\n", name, err)
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
				if moveErr := queue.AtomicMove(path, dst); moveErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: %v\n", name, moveErr)
				} else {
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
	run.prompt = strings.ReplaceAll(reviewInstructions, "TASKS_DIR_PLACEHOLDER", env.workdir+"/.tasks")
	run.prompt = strings.ReplaceAll(run.prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	run.prompt = strings.ReplaceAll(run.prompt, "MESSAGES_DIR_PLACEHOLDER", env.workdir+"/.tasks/messages")

	cloneDir, err := git.CreateClone(env.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone for review: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureReceiveDeny(env.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	run.cloneDir = cloneDir

	fmt.Printf("Launching review agent from %s (clone: %s)\n", env.repoRoot, cloneDir)

	extraEnvs := []string{
		"MATO_REVIEW_MODE=1",
		"MATO_TASK_FILE=" + task.Filename,
		"MATO_TASK_BRANCH=" + task.Branch,
		"MATO_TASK_TITLE=" + task.Title,
		fmt.Sprintf("MATO_TASK_PATH=%s/.tasks/%s/%s", env.workdir, queue.DirReadyReview, task.Filename),
		fmt.Sprintf("MATO_REVIEW_VERDICT_PATH=%s/.tasks/messages/verdict-%s.json", env.workdir, task.Filename),
	}

	args := buildDockerArgs(env, run, extraEnvs, nil)

	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, run.timeout)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "docker", args...)
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	if timeoutCtx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "error: review agent timed out after %v\n", run.timeout)
	} else if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "review agent interrupted by signal\n")
	}
	return runErr
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
			appendReviewFailure(task.TaskPath, agentID, "review agent exited without rendering a verdict")
			fmt.Printf("Review incomplete: recorded review-failure for %s\n", task.Filename)
		}
		return
	}

	var verdict reviewVerdict
	if err := json.Unmarshal(data, &verdict); err != nil {
		appendReviewFailure(task.TaskPath, agentID, fmt.Sprintf("could not parse verdict file: %v", err))
		fmt.Printf("Review incomplete: malformed verdict file for %s\n", task.Filename)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	switch strings.ToLower(strings.TrimSpace(verdict.Verdict)) {
	case "approve":
		// Write approval marker to task file.
		if err := appendToFileFn(task.TaskPath, fmt.Sprintf("\n<!-- reviewed: %s at %s — approved -->\n", agentID, now)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write approval marker: %v\n", err)
		}
		moveReviewedTask(tasksDir, agentID, task, approveDisposition)

	case "reject":
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "no reason provided"
		}
		if err := appendToFileFn(task.TaskPath, fmt.Sprintf("\n<!-- review-rejection: %s at %s — %s -->\n", agentID, now, reason)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write rejection marker: %v\n", err)
		}
		moveReviewedTask(tasksDir, agentID, task, rejectDisposition)

	case "error":
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "review agent reported an error"
		}
		appendReviewFailure(task.TaskPath, agentID, reason)
		fmt.Printf("Review error: recorded review-failure for %s: %s\n", task.Filename, reason)

	default:
		appendReviewFailure(task.TaskPath, agentID, fmt.Sprintf("unknown verdict: %q", verdict.Verdict))
		fmt.Printf("Review incomplete: unknown verdict %q for %s\n", verdict.Verdict, task.Filename)
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
func appendReviewFailure(taskPath, agentID, reason string) {
	if err := taskfile.AppendReviewFailure(taskPath, agentID, reason); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
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
