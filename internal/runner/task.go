package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
)

func runOnce(ctx context.Context, cfg dockerConfig, claimed *queue.ClaimedTask) error {
	cloneDir, err := git.CreateClone(cfg.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureReceiveDeny(cfg.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	cfg.cloneDir = cloneDir

	fmt.Printf("Launching agent from %s (clone: %s)\n", cfg.repoRoot, cloneDir)

	maxRetries := 3
	extraEnvs := []string{}
	if claimed != nil {
		if meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath); err == nil {
			maxRetries = meta.MaxRetries
		}

		// Create task branch in the clone before launching the agent.
		if _, err := git.Output(cloneDir, "checkout", "-b", claimed.Branch); err != nil {
			return fmt.Errorf("create task branch %s: %w", claimed.Branch, err)
		}

		extraEnvs = append(extraEnvs,
			"MATO_TASK_FILE="+claimed.Filename,
			"MATO_TASK_BRANCH="+claimed.Branch,
			"MATO_TASK_TITLE="+claimed.Title,
			fmt.Sprintf("MATO_TASK_PATH=%s/.tasks/in-progress/%s", cfg.workdir, claimed.Filename),
			fmt.Sprintf("MATO_FILE_CLAIMS=%s/.tasks/messages/file-claims.json", cfg.workdir),
		)
		if depCtxPath := writeDependencyContextFile(cfg.tasksDir, claimed); depCtxPath != "" {
			defer removeDependencyContextFile(cfg.tasksDir, claimed.Filename)
			extraEnvs = append(extraEnvs, fmt.Sprintf(
				"MATO_DEPENDENCY_CONTEXT=%s/.tasks/messages/dependency-context-%s.json",
				cfg.workdir, claimed.Filename,
			))
		}
		if failures := extractFailureLines(claimed.TaskPath); failures != "" {
			extraEnvs = append(extraEnvs, "MATO_PREVIOUS_FAILURES="+failures)
		}
		if reviewFeedback := extractReviewRejections(claimed.TaskPath); reviewFeedback != "" {
			extraEnvs = append(extraEnvs, "MATO_REVIEW_FEEDBACK="+reviewFeedback)
		}
	}
	extraEnvs = append(extraEnvs, fmt.Sprintf("MATO_MAX_RETRIES=%d", maxRetries))

	args := buildDockerArgs(cfg, extraEnvs, nil)

	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, cfg.timeout)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "docker", args...)
	cmd.Cancel = func() error {
		// Gracefully stop the Docker container by sending SIGTERM.
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	agentErr := cmd.Run()
	if timeoutCtx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "error: agent timed out after %v\n", cfg.timeout)
	} else if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "agent interrupted by signal\n")
	}

	// Post-agent: if the task is still in in-progress/ and the agent made
	// commits, push the branch and move the task to ready-for-review/.
	if claimed != nil {
		if err := postAgentPush(cfg, claimed, cloneDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: post-agent push failed: %v\n", err)
		}
	}

	return agentErr
}

// postAgentPush checks whether the agent committed work on the task branch.
// If commits exist and the task is still in in-progress/, the host pushes the
// branch, writes the branch marker, and moves the task to ready-for-review/.
func postAgentPush(cfg dockerConfig, claimed *queue.ClaimedTask, cloneDir string) error {
	// Task must still be in in-progress/ (agent no longer moves files).
	if _, err := os.Stat(claimed.TaskPath); err != nil {
		return nil
	}

	// Check whether the agent made any commits above the target branch.
	logOut, err := git.Output(cloneDir, "log", "--oneline", cfg.targetBranch+"..HEAD")
	if err != nil {
		return nil // can't determine; leave for recoverStuckTask
	}
	if strings.TrimSpace(logOut) == "" {
		return nil // no commits; recoverStuckTask will handle recovery
	}

	// Pre-check: verify ready-for-review/ destination is clear before pushing.
	// If a stale file exists (e.g., from a prior incomplete cycle), skip the
	// push to avoid corrupting its metadata.
	readyPath := filepath.Join(cfg.tasksDir, "ready-for-review", claimed.Filename)
	if _, err := os.Stat(readyPath); err == nil {
		fmt.Fprintf(os.Stderr, "warning: %s already exists in ready-for-review/; skipping push (task is likely already being reviewed)\n", claimed.Filename)
		return fmt.Errorf("ready-for-review/%s already exists: skipping push to avoid overwriting", claimed.Filename)
	}

	// Push the task branch to the host repo.
	if _, err := git.Output(cloneDir, "push", "--force-with-lease", "origin", claimed.Branch); err != nil {
		return fmt.Errorf("push task branch %s: %w", claimed.Branch, err)
	}

	// Move task from in-progress/ to ready-for-review/ using os.Link +
	// os.Remove instead of os.Rename to prevent silently overwriting a file
	// that appeared at the destination after the pre-check (TOCTOU defense).
	// The branch marker is written AFTER the move so that a failed move
	// does not leave the in-progress file with an incorrect marker.
	if err := os.Link(claimed.TaskPath, readyPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("move task to ready-for-review: destination already exists (race): %w", err)
		}
		return fmt.Errorf("move task to ready-for-review: %w", err)
	}
	if err := os.Remove(claimed.TaskPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove in-progress/%s after linking to ready-for-review: %v\n", claimed.Filename, err)
	}

	// Write branch marker to the moved file in ready-for-review/.
	appendToFile(readyPath, fmt.Sprintf("\n<!-- branch: %s -->\n", claimed.Branch))

	// Send conflict-warning with changed files.
	filesOut, _ := git.Output(cloneDir, "diff", "--name-only", cfg.targetBranch+"..HEAD")
	var filesChanged []string
	for _, f := range strings.Split(strings.TrimSpace(filesOut), "\n") {
		if f != "" {
			filesChanged = append(filesChanged, f)
		}
	}
	messaging.WriteMessage(cfg.tasksDir, messaging.Message{
		From:   cfg.agentID,
		Type:   "conflict-warning",
		Task:   claimed.Filename,
		Branch: claimed.Branch,
		Files:  filesChanged,
		Body:   "About to push",
	})

	// Send completion message.
	messaging.WriteMessage(cfg.tasksDir, messaging.Message{
		From:   cfg.agentID,
		Type:   "completion",
		Task:   claimed.Filename,
		Branch: claimed.Branch,
		Files:  filesChanged,
		Body:   "Task complete, ready for review",
	})
	fmt.Printf("Pushed %s and moved %s to ready-for-review/\n", claimed.Branch, claimed.Filename)
	return nil
}

// recoverStuckTask checks whether a claimed task is still in in-progress/
// after the agent container exits and post-agent push completes. If so, the
// agent did not commit successfully (failure, crash, timeout, etc.), so the
// host moves the task back to backlog/ with a failure record.
func recoverStuckTask(tasksDir, agentID string, claimed *queue.ClaimedTask) {
	if _, err := os.Stat(claimed.TaskPath); err != nil {
		// Task was moved (to ready-for-review by post-agent push); nothing to do.
		return
	}

	dst := filepath.Join(tasksDir, "backlog", claimed.Filename)
	// Use os.Link + os.Remove instead of os.Rename to atomically prevent
	// overwriting an existing file at dst (TOCTOU race fix). os.Link fails
	// with os.ErrExist if the destination already exists.
	if err := os.Link(claimed.TaskPath, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "warning: could not recover stuck task %s: destination already exists in backlog\n", claimed.Filename)
		} else {
			fmt.Fprintf(os.Stderr, "warning: could not recover stuck task %s: %v\n", claimed.Filename, err)
		}
		return
	}
	if err := os.Remove(claimed.TaskPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove in-progress task %s after linking to backlog: %v\n", claimed.Filename, err)
	}

	// Only append a generic failure record if the agent did not already write
	// one (via ON_FAILURE). This prevents double-counting retries.
	if !agentWroteFailureRecord(dst, agentID) {
		f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not open task file to append failure record for %s: %v\n", claimed.Filename, err)
		} else {
			_, writeErr := fmt.Fprintf(f, "\n<!-- failure: %s at %s — agent container exited without cleanup -->\n",
				agentID, time.Now().UTC().Format(time.RFC3339))
			closeErr := f.Close()
			if writeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", claimed.Filename, writeErr)
			} else if closeErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", claimed.Filename, closeErr)
			}
		}
	}

	fmt.Printf("Recovered task %s after agent exit\n", claimed.Filename)
}

// agentWroteFailureRecord checks whether the task file already contains a
// failure record written by the given agent. This prevents the host from
// appending a duplicate generic failure record when the agent's ON_FAILURE
// already recorded a specific one.
func agentWroteFailureRecord(taskPath, agentID string) bool {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return false
	}
	// Look for "<!-- failure: <agentID> " — the agent's ON_FAILURE writes this pattern.
	return strings.Contains(string(data), "<!-- failure: "+agentID+" ")
}

// writeDependencyContextFile collects completion details for all resolved
// dependencies of the given task and writes them as a JSON array to a file
// in the messages directory. Returns the file path on success, or "" if the
// task has no dependencies or none have completion files.
// Writing to a file avoids ARG_MAX / Docker env var size limits that can
// occur when the JSON blob is passed as an environment variable.
func writeDependencyContextFile(tasksDir string, claimed *queue.ClaimedTask) string {
	meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath)
	if err != nil || len(meta.DependsOn) == 0 {
		return ""
	}
	var details []messaging.CompletionDetail
	for _, dep := range meta.DependsOn {
		detail, err := messaging.ReadCompletionDetail(tasksDir, dep)
		if err != nil {
			continue
		}
		details = append(details, *detail)
	}
	if len(details) == 0 {
		return ""
	}
	data, err := json.Marshal(details)
	if err != nil {
		return ""
	}

	depCtxPath := filepath.Join(tasksDir, "messages", "dependency-context-"+claimed.Filename+".json")
	if err := os.WriteFile(depCtxPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write dependency context file: %v\n", err)
		return ""
	}
	return depCtxPath
}

// removeDependencyContextFile removes the dependency context file for the
// given task, if it exists. Non-"not found" errors are logged to stderr.
func removeDependencyContextFile(tasksDir string, filename string) {
	p := filepath.Join(tasksDir, "messages", "dependency-context-"+filename+".json")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: could not remove dependency context file %s: %v\n", p, err)
	}
}

// extractFailureLines reads a task file and returns all failure record
// metadata lines (lines starting with "<!-- failure:") joined by newlines.
// References to the marker inside the task body are ignored.
// Returns "" if the file has no failure records or cannot be read.
func extractFailureLines(taskPath string) string {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return ""
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "<!-- failure:") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return strings.Join(lines, "\n")
}
