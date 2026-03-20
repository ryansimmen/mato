package runner

import (
	"context"
	"encoding/json"
	"errors"
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
func reviewCandidates(tasksDir string) []*queue.ClaimedTask {
	reviewDir := filepath.Join(tasksDir, "ready-for-review")
	entries, err := os.ReadDir(reviewDir)
	if err != nil {
		return nil
	}

	failedDir := filepath.Join(tasksDir, "failed")

	type candidate struct {
		task     *queue.ClaimedTask
		priority int
	}

	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(reviewDir, entry.Name())
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse review candidate %s: %v\n", entry.Name(), err)
			continue
		}

		// Check review retry budget before including as a candidate.
		// Only count review-specific failures (<!-- review-failure: -->),
		// not task agent failures (<!-- failure: -->).
		maxRetries := meta.MaxRetries
		failures, failErr := queue.CountReviewFailureLines(path)
		if failErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not count failures for review candidate %s, skipping: %v\n", entry.Name(), failErr)
			continue
		}
		if failures >= maxRetries {
			dst := filepath.Join(failedDir, entry.Name())
			if err := os.MkdirAll(failedDir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not create failed dir for %s: %v\n", entry.Name(), err)
				continue
			}
			// Use os.Link + os.Remove instead of os.Rename to prevent
			// silently overwriting an existing file (TOCTOU race defense).
			if linkErr := os.Link(path, dst); linkErr != nil {
				if errors.Is(linkErr, os.ErrExist) || errors.Is(linkErr, syscall.EEXIST) {
					fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: destination already exists\n", entry.Name())
				} else {
					fmt.Fprintf(os.Stderr, "warning: could not move review-exhausted task %s to failed: %v\n", entry.Name(), linkErr)
				}
			} else {
				if rmErr := os.Remove(path); rmErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not remove %s after linking to failed/: %v\n", entry.Name(), rmErr)
				}
				fmt.Printf("review retry budget exhausted for %s (%d failures >= max_retries %d), moved to failed/\n",
					entry.Name(), failures, maxRetries)
			}
			continue
		}

		branch := taskfile.ParseBranch(path)
		if branch == "" {
			branch = "task/" + frontmatter.SanitizeBranchName(entry.Name())
			if _, taken := queue.CollectActiveBranches(tasksDir)[branch]; taken {
				branch = branch + "-" + frontmatter.BranchDisambiguator(entry.Name())
			}
		}
		title := frontmatter.ExtractTitle(entry.Name(), body)
		candidates = append(candidates, candidate{
			task: &queue.ClaimedTask{
				Filename: entry.Name(),
				Branch:   branch,
				Title:    title,
				TaskPath: path,
			},
			priority: meta.Priority,
		})
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
func selectTaskForReview(tasksDir string) *queue.ClaimedTask {
	candidates := reviewCandidates(tasksDir)
	if len(candidates) == 0 {
		return nil
	}
	return candidates[0]
}

// selectAndLockReview returns the highest-priority review candidate that this
// agent can exclusively lock, along with a cleanup function to release the
// lock. Returns (nil, nil) when no unlocked review task is available.
func selectAndLockReview(tasksDir string) (*queue.ClaimedTask, func()) {
	for _, task := range reviewCandidates(tasksDir) {
		cleanup, ok := queue.AcquireReviewLock(tasksDir, task.Filename)
		if ok {
			return task, cleanup
		}
	}
	return nil, nil
}

func runReview(ctx context.Context, cfg dockerConfig, task *queue.ClaimedTask, branch string) error {
	cfg.prompt = strings.ReplaceAll(reviewInstructions, "TASKS_DIR_PLACEHOLDER", cfg.workdir+"/.tasks")
	cfg.prompt = strings.ReplaceAll(cfg.prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	cfg.prompt = strings.ReplaceAll(cfg.prompt, "MESSAGES_DIR_PLACEHOLDER", cfg.workdir+"/.tasks/messages")

	cloneDir, err := git.CreateClone(cfg.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone for review: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureReceiveDeny(cfg.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	cfg.cloneDir = cloneDir

	fmt.Printf("Launching review agent from %s (clone: %s)\n", cfg.repoRoot, cloneDir)

	extraEnvs := []string{
		"MATO_REVIEW_MODE=1",
		"MATO_TASK_FILE=" + task.Filename,
		"MATO_TASK_BRANCH=" + task.Branch,
		"MATO_TASK_TITLE=" + task.Title,
		fmt.Sprintf("MATO_TASK_PATH=%s/.tasks/ready-for-review/%s", cfg.workdir, task.Filename),
		fmt.Sprintf("MATO_REVIEW_VERDICT_PATH=%s/.tasks/messages/verdict-%s.json", cfg.workdir, task.Filename),
	}

	args := buildDockerArgs(cfg, extraEnvs, nil)

	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, cfg.timeout)
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
		fmt.Fprintf(os.Stderr, "error: review agent timed out after %v\n", cfg.timeout)
	} else if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "review agent interrupted by signal\n")
	}
	return runErr
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
		taskData, readErr := os.ReadFile(task.TaskPath)
		if readErr == nil {
			content := string(taskData)
			if reviewedRe.MatchString(content) {
				moveReviewedTask(tasksDir, agentID, task, "ready-to-merge",
					"Review approved, ready for merge", "Review approved")
				return
			}
			if reviewRejectionRe.MatchString(content) {
				moveReviewedTask(tasksDir, agentID, task, "backlog",
					"Review rejected", "Review rejected")
				return
			}
		}
		appendReviewFailure(task.TaskPath, agentID, "review agent exited without rendering a verdict")
		fmt.Printf("Review incomplete: recorded review-failure for %s\n", task.Filename)
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
		moveReviewedTask(tasksDir, agentID, task, "ready-to-merge",
			"Review approved, ready for merge", "Review approved")

	case "reject":
		reason := strings.TrimSpace(verdict.Reason)
		if reason == "" {
			reason = "no reason provided"
		}
		if err := appendToFileFn(task.TaskPath, fmt.Sprintf("\n<!-- review-rejection: %s at %s — %s -->\n", agentID, now, reason)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write rejection marker: %v\n", err)
		}
		moveReviewedTask(tasksDir, agentID, task, "backlog",
			"Review rejected", "Review rejected")

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

// moveReviewedTask moves a reviewed task to the given destination directory
// and sends a completion message. It uses os.Link + os.Remove instead of
// os.Rename to prevent silently overwriting an existing file at the
// destination (TOCTOU race defense).
func moveReviewedTask(tasksDir, agentID string, task *queue.ClaimedTask, dstDir, msgBody, logPrefix string) {
	dst := filepath.Join(tasksDir, dstDir, task.Filename)
	if err := os.Link(task.TaskPath, dst); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			fmt.Fprintf(os.Stderr, "warning: could not move reviewed task %s to %s: destination already exists\n", task.Filename, dstDir)
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not move reviewed task %s to %s: %v\n", task.Filename, dstDir, err)
		}
		return
	}
	if err := os.Remove(task.TaskPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove %s after linking to %s: %v\n", task.Filename, dstDir, err)
	}
	messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "completion",
		Task:   task.Filename,
		Branch: task.Branch,
		Body:   msgBody,
	})
	fmt.Printf("%s: moved %s to %s/\n", logPrefix, task.Filename, dstDir)
}

// appendReviewFailure writes a review-failure comment to the task file.
// The task stays in ready-for-review/ for a future review attempt.
func appendReviewFailure(taskPath, agentID, reason string) {
	f, err := os.OpenFile(taskPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open task file to append review-failure: %v\n", err)
		return
	}
	_, writeErr := fmt.Fprintf(f, "\n<!-- review-failure: %s at %s step=REVIEW error=%s -->\n",
		agentID, time.Now().UTC().Format(time.RFC3339), reason)
	closeErr := f.Close()
	if writeErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write review-failure record: %v\n", writeErr)
	} else if closeErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write review-failure record: %v\n", closeErr)
	}
}

// extractReviewRejections reads review-rejection comments from the task file,
// joined by newlines. Returns "" if none found or file cannot be read.
func extractReviewRejections(taskPath string) string {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return ""
	}
	var rejections []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "<!-- review-rejection:") {
			rejections = append(rejections, strings.TrimSpace(line))
		}
	}
	return strings.Join(rejections, "\n")
}
