package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/sessionmeta"
	"mato/internal/taskfile"
	"mato/internal/taskstate"
)

var createCloneFn = git.CreateClone
var removeCloneFn = git.RemoveClone
var ensureBranchFn = git.EnsureBranch
var writeBranchMarkerFn = queue.WriteBranchMarker

func allowRecordedBranchResume(source git.BranchSource) bool {
	switch source {
	case git.BranchSourceLocal, git.BranchSourceRemote:
		return true
	default:
		return false
	}
}

func runOnce(ctx context.Context, env envConfig, run runContext, claimed *queue.ClaimedTask) error {
	cloneDir, err := createCloneFn(env.repoRoot)
	if err != nil {
		return fmt.Errorf("create clone: %w", err)
	}
	cleanupClone := true
	defer func() {
		if cleanupClone {
			removeCloneFn(cloneDir)
		}
	}()

	if err := configureReceiveDeny(env.repoRoot); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	run.cloneDir = cloneDir

	fmt.Printf("Launching agent from %s (clone: %s)\n", env.repoRoot, cloneDir)

	maxRetries := 3
	extraEnvs := []string{}
	startingTip := ""
	if claimed != nil {
		if meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath); err == nil {
			maxRetries = meta.MaxRetries
		}

		hasRecordedBranch := claimed.HadRecordedBranchMark
		branchResult, err := ensureBranchFn(cloneDir, claimed.Branch)
		if err != nil {
			return fmt.Errorf("ensure task branch %s: %w", claimed.Branch, err)
		}
		if hasRecordedBranch && !allowRecordedBranchResume(branchResult.Source) {
			return fmt.Errorf("resume recorded task branch %s: unsupported branch source %s", claimed.Branch, branchResult.SourceDescription())
		}

		startingTip, err = git.Output(cloneDir, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("capture starting tip for %s: %w", claimed.Branch, err)
		}
		startingTip = strings.TrimSpace(startingTip)
		session := loadOrCreateSession(env.tasksDir, sessionmeta.KindWork, claimed.Filename, claimed.Branch)
		if session != nil {
			run.resumeSessionID = session.CopilotSessionID
		}
		recordSessionUpdate(env.tasksDir, sessionmeta.KindWork, claimed.Filename, "record work session", func(session *sessionmeta.Session) {
			session.TaskBranch = claimed.Branch
			session.LastHeadSHA = startingTip
		})

		extraEnvs = append(extraEnvs,
			"MATO_TASK_FILE="+claimed.Filename,
			"MATO_TASK_BRANCH="+claimed.Branch,
			"MATO_TASK_TITLE="+claimed.Title,
			fmt.Sprintf("MATO_TASK_PATH=%s/%s/%s/%s", env.workdir, dirs.Root, queue.DirInProgress, claimed.Filename),
			fmt.Sprintf("MATO_FILE_CLAIMS=%s/%s/messages/file-claims.json", env.workdir, dirs.Root),
		)
		if depCtxPath := writeDependencyContextFile(env.tasksDir, claimed); depCtxPath != "" {
			defer removeDependencyContextFile(env.tasksDir, claimed.Filename)
			extraEnvs = append(extraEnvs, fmt.Sprintf(
				"MATO_DEPENDENCY_CONTEXT=%s/%s/messages/dependency-context-%s.json",
				env.workdir, dirs.Root, claimed.Filename,
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
	agentErr := runCopilotCommand(ctx, env, run, extraEnvs, nil, "agent", func() string {
		if claimed == nil {
			return ""
		}
		return resetSession(env.tasksDir, sessionmeta.KindWork, claimed.Filename, claimed.Branch)
	})

	// Post-agent: if the task is still in in-progress/ and the agent made
	// commits, push the branch and move the task to ready-for-review/.
	var postPushErr error
	if claimed != nil {
		if err := postAgentPush(env, run.agentID, claimed, cloneDir, startingTip); err != nil {
			cleanupClone = false
			postPushErr = fmt.Errorf("post-agent push failed; preserving clone at %s: %w", cloneDir, err)
		}
	}

	if agentErr != nil && postPushErr != nil {
		return errors.Join(agentErr, postPushErr)
	}
	if postPushErr != nil {
		return postPushErr
	}
	return agentErr
}

// postAgentPush checks whether the agent committed work on the task branch.
// If commits exist and the task is still in in-progress/, the host pushes the
// branch, writes the branch marker, and moves the task to ready-for-review/.
func postAgentPush(env envConfig, agentID string, claimed *queue.ClaimedTask, cloneDir, startingTip string) error {
	// Task must still be in in-progress/ (agent no longer moves files).
	if _, err := os.Stat(claimed.TaskPath); err != nil {
		return nil
	}

	currentTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("determine current task branch tip: %w", err)
	}
	currentTip = strings.TrimSpace(currentTip)
	if currentTip == startingTip {
		return nil // no commits; recoverStuckTask will handle recovery
	}

	// Pre-check: verify ready-for-review/ destination is clear before pushing.
	// If a stale file exists (e.g., from a prior incomplete cycle), skip the
	// push to avoid corrupting its metadata.
	if _, err := os.Stat(filepath.Join(env.tasksDir, queue.DirReadyReview, claimed.Filename)); err == nil {
		fmt.Fprintf(os.Stderr, "warning: %s already exists in ready-for-review/; skipping push (task is likely already being reviewed)\n", claimed.Filename)
		return fmt.Errorf("ready-for-review/%s already exists: skipping push to avoid overwriting", claimed.Filename)
	}

	// Push the task branch to the host repo.
	if _, err := git.Output(cloneDir, "push", "--force-with-lease", "origin", claimed.Branch); err != nil {
		return fmt.Errorf("push task branch %s: %w", claimed.Branch, err)
	}
	recordTaskStateUpdate(env.tasksDir, claimed.Filename, "record pushed branch taskstate", func(state *taskstate.TaskState) {
		state.TaskBranch = claimed.Branch
		state.TargetBranch = env.targetBranch
		state.LastHeadSHA = currentTip
		state.LastOutcome = taskstate.OutcomeWorkBranchPushed
	})
	recordSessionUpdate(env.tasksDir, sessionmeta.KindWork, claimed.Filename, "record work session", func(session *sessionmeta.Session) {
		session.TaskBranch = claimed.Branch
		session.LastHeadSHA = currentTip
	})

	// Move task to ready-for-review/ and write branch marker.
	if err := moveTaskToReviewWithMarker(env.tasksDir, claimed, claimed.Branch); err != nil {
		return err
	}
	finalizePushedTask(env.tasksDir, env.targetBranch, agentID, claimed.Filename, claimed.Branch, currentTip, changedFilesSinceTarget(cloneDir, env.targetBranch), true)
	return nil
}

func changedFilesSinceTarget(cloneDir, targetBranch string) []string {
	filesOut, _ := git.Output(cloneDir, "diff", "--name-only", targetBranch+"..HEAD")
	var filesChanged []string
	for _, f := range strings.Split(strings.TrimSpace(filesOut), "\n") {
		if f != "" {
			filesChanged = append(filesChanged, f)
		}
	}
	return filesChanged
}

func finalizePushedTask(tasksDir, targetBranch, agentID, filename, branch, currentTip string, filesChanged []string, logMove bool) {
	recordTaskStateUpdate(tasksDir, filename, "record work push taskstate", func(state *taskstate.TaskState) {
		state.TaskBranch = branch
		state.TargetBranch = targetBranch
		state.LastHeadSHA = currentTip
		state.LastOutcome = "work-pushed"
	})
	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not rebuild file claims after pushing %s: %v\n", filename, err)
	}

	// Send conflict-warning with changed files.
	messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "conflict-warning",
		Task:   filename,
		Branch: branch,
		Files:  filesChanged,
		Body:   "About to push",
	})

	// Send completion message.
	messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "completion",
		Task:   filename,
		Branch: branch,
		Files:  filesChanged,
		Body:   "Task complete, ready for review",
	})
	if logMove {
		fmt.Printf("Pushed %s and moved %s to ready-for-review/\n", branch, filename)
	}
}

// moveTaskToReviewWithMarker atomically moves a task from in-progress/ to
// ready-for-review/ and writes the branch marker. If the marker write fails,
// the move is rolled back by moving the file back to in-progress/.
func moveTaskToReviewWithMarker(tasksDir string, claimed *queue.ClaimedTask, branch string) error {
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, claimed.Filename)

	// AtomicMove uses os.Link + os.Remove to prevent silently overwriting a
	// file that appeared at the destination after the pre-check (TOCTOU defense).
	if err := queue.AtomicMove(claimed.TaskPath, readyPath); err != nil {
		return fmt.Errorf("move task to ready-for-review: %w", err)
	}

	// Write branch marker AFTER the move so that a failed move does not
	// leave the in-progress file with an incorrect marker.
	if err := writeBranchMarkerFn(readyPath, branch); err != nil {
		// Roll back: move file from ready-for-review/ back to in-progress/.
		if rollbackErr := queue.AtomicMove(readyPath, claimed.TaskPath); rollbackErr != nil {
			fmt.Fprintf(os.Stderr, "error: branch marker write failed and rollback to in-progress/ also failed: %v\n", rollbackErr)
			return fmt.Errorf("write branch marker to %s: %w (rollback failed: %v)", readyPath, err, rollbackErr)
		}
		return fmt.Errorf("write branch marker to %s: %w (rolled back to in-progress/)", readyPath, err)
	}
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

	if recoverPushedTaskToReview(tasksDir, claimed) {
		return
	}

	dst := filepath.Join(tasksDir, queue.DirBacklog, claimed.Filename)
	if err := queue.AtomicMove(claimed.TaskPath, dst); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not recover stuck task %s: %v\n", claimed.Filename, err)
		return
	}

	// Only append a generic failure record if the agent did not already write
	// one (via ON_FAILURE). This prevents double-counting retries.
	if !agentWroteFailureRecord(dst, agentID) {
		content := fmt.Sprintf("\n<!-- failure: %s at %s — agent container exited without cleanup -->\n",
			agentID, time.Now().UTC().Format(time.RFC3339))
		if err := atomicwrite.AppendToFile(dst, content); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", claimed.Filename, err)
		}
	}

	fmt.Printf("Recovered task %s after agent exit\n", claimed.Filename)
}

func recoverPushedTaskToReview(tasksDir string, claimed *queue.ClaimedTask) bool {
	state, err := taskstate.Load(tasksDir, claimed.Filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load taskstate for %s during pushed-task recovery: %v\n", claimed.Filename, err)
		return false
	}
	if state == nil || state.LastOutcome != taskstate.OutcomeWorkBranchPushed {
		return false
	}

	branch := strings.TrimSpace(state.TaskBranch)
	if branch == "" {
		branch = claimed.Branch
	}
	if strings.TrimSpace(branch) == "" {
		fmt.Fprintf(os.Stderr, "warning: pushed task %s is missing task branch metadata; leaving it in in-progress/\n", claimed.Filename)
		return true
	}
	if err := moveTaskToReviewWithMarker(tasksDir, claimed, branch); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not recover pushed task %s to ready-for-review: %v\n", claimed.Filename, err)
		return true
	}
	finalizePushedTask(tasksDir, state.TargetBranch, "host-recovery", claimed.Filename, branch, state.LastHeadSHA, nil, false)
	fmt.Printf("Recovered pushed task %s to ready-for-review/\n", claimed.Filename)
	return true
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
	return taskfile.ContainsFailureFrom(data, agentID)
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
	if err := atomicwrite.WriteFile(depCtxPath, data); err != nil {
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
	return taskfile.ExtractFailureLines(data)
}
