package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/atomicwrite"
	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/messaging"
	"github.com/ryansimmen/mato/internal/queue"
	"github.com/ryansimmen/mato/internal/runtimedata"
	"github.com/ryansimmen/mato/internal/taskfile"
	"github.com/ryansimmen/mato/internal/ui"
)

// NOTE: The hook variables below are package-level mutable state used as test
// seams. They prevent t.Parallel() within this package. Struct-based
// dependency injection would be needed for true parallel test safety.
var createCloneFn = git.CreateClone
var removeCloneFn = git.RemoveClone
var ensureBranchFn = git.EnsureBranch
var prepareCloneOriginForContainerFn = prepareCloneOriginForContainer
var writeBranchMarkerFn = queue.WriteBranchMarker
var writeDebugMarkerFn = writeDebugMarker
var moveTaskFileFn = queue.AtomicMove

// debugMarkerFile is a sentinel file written into a clone directory when it
// is intentionally preserved after a postAgentPush failure. Its presence
// positively identifies the directory as a mato debug clone eligible for
// later cleanup.
const debugMarkerFile = ".mato-debug-clone"

// writeDebugMarker creates the sentinel marker file inside dir. Errors are
// logged but not fatal — the clone is still preserved for debugging even if
// the marker write fails.
func writeDebugMarker(dir string) {
	path := filepath.Join(dir, debugMarkerFile)
	if err := os.WriteFile(path, []byte("preserved after post-agent push failure\n"), 0o644); err != nil {
		ui.Warnf("warning: could not write debug marker in %s: %v\n", dir, err)
	}
}

func allowRecordedBranchResume(source git.BranchSource) bool {
	switch source {
	case git.BranchSourceLocal, git.BranchSourceRemote:
		return true
	default:
		return false
	}
}

func canRepairRecordedBranchResume(source git.BranchSource) bool {
	return source == git.BranchSourceHeadRemoteMissing
}

func repairRecordedBranchResume(tasksDir string, claimed *queue.ClaimedTask, agentID string, branchResult git.EnsureBranchResult) {
	if claimed == nil {
		return
	}
	reason := fmt.Sprintf("recorded task branch %s missing on origin; recreated from %s", claimed.Branch, branchResult.SourceDescription())
	if err := taskfile.AppendBranchRepairRecord(claimed.TaskPath, prelaunchFailureAgentID(agentID), reason); err != nil {
		ui.Warnf("warning: could not record branch repair for %s: %v\n", claimed.Filename, err)
	}
	resetSession(tasksDir, runtimedata.KindWork, claimed.Filename, claimed.Branch)
	ui.Warnf("warning: %s; starting a fresh work session\n", reason)
}

func recordWorkLaunchState(tasksDir, targetBranch string, claimed *queue.ClaimedTask, startingTip string) {
	if claimed == nil {
		return
	}
	recordTaskStateUpdate(tasksDir, claimed.Filename, "record work launch taskstate", func(state *runtimedata.TaskState) {
		state.TaskBranch = claimed.Branch
		state.TargetBranch = targetBranch
		state.LastHeadSHA = strings.TrimSpace(startingTip)
		state.LastOutcome = runtimedata.OutcomeWorkLaunched
	})
}

func prelaunchFailureAgentID(agentID string) string {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return "mato"
	}
	return agentID
}

func recordTaskPrelaunchFailure(claimed *queue.ClaimedTask, agentID string, err error) error {
	if claimed == nil || err == nil {
		return err
	}
	if appendErr := taskfile.AppendFailureRecord(claimed.TaskPath, prelaunchFailureAgentID(agentID), "WORK", err.Error()); appendErr != nil {
		ui.Warnf("warning: could not record prelaunch failure for %s: %v\n", claimed.Filename, appendErr)
	}
	return err
}

func runOnce(ctx context.Context, env envConfig, run runContext, claimed *queue.ClaimedTask) error {
	recordWorkLaunchState(env.tasksDir, env.targetBranch, claimed, "")

	cloneDir, err := createCloneFn(env.repoRoot)
	if err != nil {
		return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("create clone: %w", err))
	}
	if err := prepareCloneQueueMounts(cloneDir); err != nil {
		removeCloneFn(cloneDir)
		return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("prepare clone queue mounts: %w", err))
	}
	if err := validateQueueMountAccess(taskQueueMountChecks(env.tasksDir, env.workdir)); err != nil {
		removeCloneFn(cloneDir)
		return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("validate task queue mounts: %w", err))
	}
	cleanupClone := true
	defer func() {
		if cleanupClone {
			removeCloneFn(cloneDir)
		}
	}()

	if err := configureReceiveDeny(env.repoRoot); err != nil {
		ui.Warnf("warning: could not set receive.denyCurrentBranch=updateInstead: %v\n", err)
	}

	run.cloneDir = cloneDir

	emitDockerLaunchSummary(env.verbose, "task-agent", env.image, run.model, claimed)
	fmt.Printf("Launching agent from %s (clone: %s)\n", env.repoRoot, cloneDir)

	maxRetries := 3
	extraEnvs := []string{}
	startingTip := ""
	if claimed != nil {
		freshWorkSession := false
		if meta, _, err := frontmatter.ParseTaskFile(claimed.TaskPath); err == nil {
			maxRetries = meta.MaxRetries
		}

		hasRecordedBranch := claimed.HadRecordedBranchMark
		branchResult, err := ensureBranchFn(cloneDir, claimed.Branch)
		if err != nil {
			return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("ensure task branch %s: %w", claimed.Branch, err))
		}
		if hasRecordedBranch {
			if canRepairRecordedBranchResume(branchResult.Source) {
				repairRecordedBranchResume(env.tasksDir, claimed, run.agentID, branchResult)
				freshWorkSession = true
			} else if !allowRecordedBranchResume(branchResult.Source) {
				return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("resume recorded task branch %s: unsupported branch source %s", claimed.Branch, branchResult.SourceDescription()))
			}
		}

		startingTip, err = git.Output(cloneDir, "rev-parse", "HEAD")
		if err != nil {
			return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("capture starting tip for %s: %w", claimed.Branch, err))
		}
		startingTip = strings.TrimSpace(startingTip)
		recordWorkLaunchState(env.tasksDir, env.targetBranch, claimed, startingTip)
		session := loadOrCreateSession(env.tasksDir, runtimedata.KindWork, claimed.Filename, claimed.Branch)
		if session != nil && !freshWorkSession {
			run.resumeSessionID = session.CopilotSessionID
		}
		recordSessionUpdate(env.tasksDir, runtimedata.KindWork, claimed.Filename, "record work session", func(session *runtimedata.Session) {
			session.TaskBranch = claimed.Branch
			session.LastHeadSHA = startingTip
		})

		extraEnvs = append(extraEnvs,
			"MATO_TASK_FILE="+claimed.Filename,
			"MATO_TASK_BRANCH="+claimed.Branch,
			"MATO_TASK_TITLE="+claimed.Title,
			fmt.Sprintf("MATO_TASK_PATH=%s/%s/%s/%s", env.workdir, dirs.Root, dirs.InProgress, claimed.Filename),
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
		if reviewFeedback := extractReviewRejectionsWithVerdictFallback(claimed.TaskPath, env.tasksDir, claimed.Filename); reviewFeedback != "" {
			extraEnvs = append(extraEnvs, "MATO_REVIEW_FEEDBACK="+reviewFeedback)
		}
	}
	extraEnvs = append(extraEnvs, fmt.Sprintf("MATO_MAX_RETRIES=%d", maxRetries))
	restoreOrigin, err := prepareCloneOriginForContainerFn(cloneDir)
	if err != nil {
		return recordTaskPrelaunchFailure(claimed, run.agentID, fmt.Errorf("prepare clone origin for container: %w", err))
	}
	agentErr := runCopilotCommand(ctx, env, run, extraEnvs, nil, "agent", func() string {
		if claimed == nil {
			return ""
		}
		return resetSession(env.tasksDir, runtimedata.KindWork, claimed.Filename, claimed.Branch)
	})
	// Post-agent: if the task is still in in-progress/ and the agent made
	// commits, push the branch and move the task to ready-for-review/.
	var postPushErr error
	if claimed != nil {
		if err := postAgentPush(env, run.agentID, claimed, cloneDir, startingTip); err != nil {
			cleanupClone = false
			writeDebugMarkerFn(cloneDir)
			postPushErr = fmt.Errorf("post-agent push failed; preserving clone at %s: %w", cloneDir, err)
		}
	}

	restoreErr := restoreOrigin()

	if agentErr != nil || postPushErr != nil || restoreErr != nil {
		return errors.Join(agentErr, postPushErr, restoreErr)
	}
	return nil
}

// postAgentPush checks whether the agent committed work on the task branch.
// If commits exist and the task is still in in-progress/, the host pushes the
// branch, writes the branch marker, and moves the task to ready-for-review/.
func postAgentPush(env envConfig, agentID string, claimed *queue.ClaimedTask, cloneDir, startingTip string) error {
	// Task must still be in in-progress/ (agent no longer moves files).
	if _, err := os.Stat(claimed.TaskPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat claimed task %s: %w", claimed.TaskPath, err)
		}
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
	if _, err := os.Stat(filepath.Join(env.tasksDir, dirs.ReadyReview, claimed.Filename)); err == nil {
		ui.Warnf("warning: %s already exists in ready-for-review/; skipping push (task is likely already being reviewed)\n", claimed.Filename)
		return fmt.Errorf("ready-for-review/%s already exists: skipping push to avoid overwriting", claimed.Filename)
	}

	// Push directly to the host repo when available so work handoff does not
	// depend on the clone's temporary in-container origin rewrite being restored
	// first. Tests that call postAgentPush directly can still rely on origin.
	pushTarget := "origin"
	if repoRoot := strings.TrimSpace(env.repoRoot); repoRoot != "" {
		pushTarget = repoRoot
	}
	if _, err := git.Output(cloneDir, "push", "--force-with-lease", pushTarget, claimed.Branch); err != nil {
		return fmt.Errorf("push task branch %s: %w", claimed.Branch, err)
	}
	recordTaskStateUpdate(env.tasksDir, claimed.Filename, "record pushed branch taskstate", func(state *runtimedata.TaskState) {
		state.TaskBranch = claimed.Branch
		state.TargetBranch = env.targetBranch
		state.LastHeadSHA = currentTip
		state.LastOutcome = runtimedata.OutcomeWorkBranchPushed
	})
	recordSessionUpdate(env.tasksDir, runtimedata.KindWork, claimed.Filename, "record work session", func(session *runtimedata.Session) {
		session.TaskBranch = claimed.Branch
		session.LastHeadSHA = currentTip
	})

	// Move task to ready-for-review/ and write branch marker.
	if err := moveTaskToReviewWithMarker(env.tasksDir, claimed, claimed.Branch); err != nil {
		return fmt.Errorf("move task to review: %w", err)
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
	recordTaskStateUpdate(tasksDir, filename, "record work push taskstate", func(state *runtimedata.TaskState) {
		state.TaskBranch = branch
		state.TargetBranch = targetBranch
		state.LastHeadSHA = currentTip
		state.LastOutcome = runtimedata.OutcomeWorkPushed
	})
	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		ui.Warnf("warning: could not rebuild file claims after pushing %s: %v\n", filename, err)
	}

	// Send conflict-warning with changed files.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "conflict-warning",
		Task:   filename,
		Branch: branch,
		Files:  filesChanged,
		Body:   "About to push",
	}); err != nil {
		ui.Warnf("warning: could not write conflict-warning for %s: %v\n", filename, err)
	}

	// Send completion message.
	if err := messaging.WriteMessage(tasksDir, messaging.Message{
		From:   agentID,
		Type:   "completion",
		Task:   filename,
		Branch: branch,
		Files:  filesChanged,
		Body:   "Task complete, ready for review",
	}); err != nil {
		ui.Warnf("warning: could not write completion message for %s: %v\n", filename, err)
	}
	if logMove {
		fmt.Printf("Pushed %s and moved %s to ready-for-review/\n", branch, filename)
	}
}

// moveTaskToReviewWithMarker atomically moves a task from in-progress/ to
// ready-for-review/ and writes the branch marker. If the marker write fails,
// the move is rolled back by moving the file back to in-progress/. If that
// rollback also fails, the authoritative ready-for-review copy is quarantined
// to failed/ with a terminal-failure marker.
func moveTaskToReviewWithMarker(tasksDir string, claimed *queue.ClaimedTask, branch string) error {
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, claimed.Filename)

	// AtomicMove uses os.Link + os.Remove to prevent silently overwriting a
	// file that appeared at the destination after the pre-check (TOCTOU defense).
	if err := moveTaskFileFn(claimed.TaskPath, readyPath); err != nil {
		return fmt.Errorf("move task to ready-for-review: %w", err)
	}

	// Write branch marker AFTER the move so that a failed move does not
	// leave the in-progress file with an incorrect marker.
	if err := writeBranchMarkerFn(readyPath, branch); err != nil {
		// Roll back: move file from ready-for-review/ back to in-progress/.
		if rollbackErr := moveTaskFileFn(readyPath, claimed.TaskPath); rollbackErr != nil {
			detail := fmt.Sprintf("write branch marker to %s: %v (rollback failed: %v)", readyPath, err, rollbackErr)
			if quarantineErr := queue.QuarantinePushedTaskHandoff(tasksDir, claimed.Filename, readyPath, detail); quarantineErr != nil {
				fmt.Fprintf(os.Stderr, "error: branch marker write failed, rollback to in-progress/ also failed, and quarantine to failed/ also failed: %v\n", quarantineErr)
				return fmt.Errorf("write branch marker to %s: %w (rollback failed: %w; quarantine to failed/ also failed: %w)", readyPath, err, rollbackErr, quarantineErr)
			}
			return fmt.Errorf("write branch marker to %s: %w (rollback failed: %w; moved task to failed/)", readyPath, err, rollbackErr)
		}
		return fmt.Errorf("write branch marker to %s: %w (rolled back to in-progress/)", readyPath, err)
	}
	return nil
}

// recoverStuckTask checks whether a claimed task is still in in-progress/
// after the agent container exits and post-agent push completes. If the
// runtime taskstate still shows a pre-push work launch, the host moves the
// task back to backlog/. Non-cancellation recoveries also receive a generic
// failure record when the agent did not already write one. If pushed-task
// handoff metadata is missing or unusable, the host quarantines the task to
// failed/ with a terminal marker instead of leaving it stranded in in-progress/.
func recoverStuckTask(tasksDir, agentID string, claimed *queue.ClaimedTask, cancelled bool) {
	if _, err := os.Stat(claimed.TaskPath); err != nil {
		// Task was moved (to ready-for-review by post-agent push); nothing to do.
		return
	}

	if recovery, recovered, err := queue.RecoverPushedTaskHandoff(tasksDir, claimed.Filename, claimed.TaskPath, writeBranchMarkerFn); recovered {
		if err != nil {
			ui.Warnf("warning: could not recover pushed task %s to ready-for-review: %v\n", claimed.Filename, err)
		} else if recovery != nil {
			fmt.Printf("Recovered pushed task %s to ready-for-review/\n", claimed.Filename)
			finalizePushedTask(tasksDir, recovery.TargetBranch, "host-recovery", claimed.Filename, recovery.Branch, recovery.LastHeadSHA, recoveredFilesChanged(tasksDir, claimed.Filename), false)
		}
		return
	}

	dst := filepath.Join(tasksDir, dirs.Backlog, claimed.Filename)
	if err := queue.AtomicMove(claimed.TaskPath, dst); err != nil {
		ui.Warnf("warning: could not recover stuck task %s: %v\n", claimed.Filename, err)
		return
	}

	// Only append a generic failure record if the agent did not already write
	// one (via ON_FAILURE). Cancellation-driven shutdown recovery should not
	// burn retry budget for work the agent never completed.
	if !cancelled && !agentWroteFailureRecord(dst, agentID) {
		content := fmt.Sprintf("\n<!-- failure: %s at %s — agent container exited without cleanup -->\n",
			agentID, time.Now().UTC().Format(time.RFC3339))
		if err := atomicwrite.AppendToFile(dst, content); err != nil {
			ui.Warnf("warning: could not write failure record for %s: %v\n", claimed.Filename, err)
		}
	}

	fmt.Printf("Recovered task %s after agent exit\n", claimed.Filename)
}

func recoveredFilesChanged(tasksDir, filename string) []string {
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, filename)
	meta, _, err := frontmatter.ParseTaskFile(readyPath)
	if err != nil || len(meta.Affects) == 0 {
		return nil
	}
	filesChanged := append([]string(nil), meta.Affects...)
	sort.Strings(filesChanged)
	return filesChanged
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
	idx := queue.BuildIndex(tasksDir)
	var details []messaging.CompletionDetail
	for _, dep := range meta.DependsOn {
		detail, err := readResolvedDependencyCompletionDetail(tasksDir, idx, dep)
		if err != nil {
			ui.Warnf("warning: could not read completion detail for dependency %s of task %s: %v\n", dep, claimed.Filename, err)
			continue
		}
		if detail == nil {
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
		ui.Warnf("warning: could not write dependency context file: %v\n", err)
		return ""
	}
	return depCtxPath
}

func readResolvedDependencyCompletionDetail(tasksDir string, idx *queue.PollIndex, dep string) (*messaging.CompletionDetail, error) {
	for _, taskID := range queue.CompletedDependencyTaskIDs(idx, dep) {
		detail, err := messaging.ReadCompletionDetail(tasksDir, taskID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if taskID != dep {
				return nil, fmt.Errorf("resolved as %s: %w", taskID, err)
			}
			return nil, err
		}
		return detail, nil
	}
	return nil, nil
}

// removeDependencyContextFile removes the dependency context file for the
// given task, if it exists. Non-"not found" errors are logged to stderr.
func removeDependencyContextFile(tasksDir string, filename string) {
	p := filepath.Join(tasksDir, "messages", "dependency-context-"+filename+".json")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		ui.Warnf("warning: could not remove dependency context file %s: %v\n", p, err)
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
