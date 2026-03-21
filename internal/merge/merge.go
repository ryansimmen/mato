package merge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/lockfile"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/taskfile"
)

type mergeQueueTask struct {
	name     string
	path     string
	title    string
	priority int
	branch   string
	id       string
	affects  []string
}

var errTaskBranchNotPushed = errors.New("task branch not pushed by agent")
var errSquashMergeConflict = errors.New("squash merge conflict")
var errPushAfterSquashFailed = errors.New("push failed after squash merge")

type mergeResult struct {
	commitSHA    string
	filesChanged []string
}

const mergedTaskRecordPrefix = "<!-- merged: merge-queue at "

// ProcessQueue merges completed task branches into the target branch.
// It scans ready-to-merge/ for task files, prefers branch metadata recorded in
// each task file, falls back to the filename-derived branch name for backward
// compatibility, and performs a squash merge.
// Returns the number of tasks successfully merged.
func ProcessQueue(repoRoot, tasksDir, branch string) int {
	readyDir := filepath.Join(tasksDir, queue.DirReadyMerge)
	entries, err := os.ReadDir(readyDir)
	if err != nil {
		return 0
	}

	activeBranches := queue.CollectActiveBranches(tasksDir)

	tasks := make([]mergeQueueTask, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(readyDir, entry.Name())
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse ready-to-merge task %s: %v\n", entry.Name(), err)
			if failureErr := failMergeTask(path, filepath.Join(tasksDir, queue.DirBacklog, entry.Name()), fmt.Sprintf("parse task file: %v", err)); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not requeue task %s: %v\n", entry.Name(), failureErr)
			}
			continue
		}

		taskBranch := taskfile.ParseBranch(path)
		if taskBranch == "" {
			taskBranch = "task/" + frontmatter.SanitizeBranchName(entry.Name())
			if _, taken := activeBranches[taskBranch]; taken {
				taskBranch = taskBranch + "-" + frontmatter.BranchDisambiguator(entry.Name())
			}
		}

		tasks = append(tasks, mergeQueueTask{
			name:     entry.Name(),
			path:     path,
			title:    frontmatter.ExtractTitle(entry.Name(), body),
			priority: meta.Priority,
			branch:   taskBranch,
			id:       meta.ID,
			affects:  meta.Affects,
		})
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})

	merged := 0
	for _, task := range tasks {
		completedPath := filepath.Join(tasksDir, queue.DirCompleted, task.name)
		if taskHasMergeSuccessRecord(task.path) {
			if err := moveTaskWithRetry(task.path, completedPath); err != nil {
				// If the destination already exists, the task was already
				// moved to completed/ by a prior cycle. Remove the
				// ready-to-merge copy to avoid an infinite retry loop.
				if _, statErr := os.Stat(completedPath); statErr == nil {
					if removeErr := os.Remove(task.path); removeErr != nil {
						fmt.Fprintf(os.Stderr, "warning: could not remove duplicate ready-to-merge task %s: %v\n", task.name, removeErr)
					}
				} else {
					fmt.Fprintf(os.Stderr, "warning: merged task %s but could not move to completed: %v\n", task.name, err)
				}
				continue
			}
			merged++
			continue
		}

		result, err := mergeReadyTask(repoRoot, branch, task)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not merge task %s: %v\n", task.name, err)
			if failureErr := handleMergeFailure(repoRoot, tasksDir, task, err); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not record merge failure for task %s: %v\n", task.name, failureErr)
			}
			continue
		}
		// Clean up the now-merged task branch (best-effort).
		cleanupTaskBranch(repoRoot, taskBranchName(task))
		if result != nil {
			detail := messaging.CompletionDetail{
				TaskID:       task.id,
				TaskFile:     task.name,
				Branch:       taskBranchName(task),
				CommitSHA:    result.commitSHA,
				FilesChanged: result.filesChanged,
				Title:        task.title,
			}
			if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write completion detail for task %s: %v\n", task.name, err)
			}
		}
		if err := markTaskMerged(task.path); err != nil {
			fmt.Fprintf(os.Stderr, "warning: merged task %s but could not mark completion: %v\n", task.name, err)
			// Continue to moveTaskWithRetry: moving to completed/ is
			// more important than the merged record.  If the move also
			// fails, the next cycle will detect the already-merged
			// branch via the idempotent squash check.
		}
		if err := moveTaskWithRetry(task.path, completedPath); err != nil {
			if _, statErr := os.Stat(completedPath); statErr == nil {
				if removeErr := os.Remove(task.path); removeErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not remove duplicate ready-to-merge task %s: %v\n", task.name, removeErr)
				}
			} else {
				fmt.Fprintf(os.Stderr, "warning: merged task %s but could not move to completed: %v\n", task.name, err)
				continue
			}
		}
		merged++
	}

	return merged
}

func HasReadyTasks(tasksDir string) bool {
	entries, err := os.ReadDir(filepath.Join(tasksDir, queue.DirReadyMerge))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			return true
		}
	}
	return false
}

func mergeReadyTask(repoRoot, branch string, task mergeQueueTask) (*mergeResult, error) {
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("create temp clone: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureMergeCloneIdentity(repoRoot, cloneDir); err != nil {
		return nil, err
	}
	if _, err := git.Output(cloneDir, "fetch", "origin"); err != nil {
		return nil, fmt.Errorf("fetch origin: %w", err)
	}
	if _, err := git.Output(cloneDir, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return nil, fmt.Errorf("checkout target branch %s: %w", branch, err)
	}

	taskBranch := taskBranchName(task)
	if _, err := git.Output(cloneDir, "rev-parse", "--verify", "origin/"+taskBranch); err != nil {
		return nil, fmt.Errorf("%w: task branch %s not found on origin (agent may not have pushed)", errTaskBranchNotPushed, taskBranch)
	}

	// Extract the agent's commit messages before squashing so we can
	// incorporate them into the squash commit for richer context.
	agentLog, _ := git.Output(cloneDir, "log", "--format=%B", "origin/"+branch+"..origin/"+taskBranch)

	if _, err := git.Output(cloneDir, "merge", "--squash", "origin/"+taskBranch); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errSquashMergeConflict, taskBranch, err)
	}

	// If the squash produced no staged changes, the task branch is already
	// fully merged into the target (e.g. a prior push succeeded but
	// post-push bookkeeping failed).  Return success without a duplicate
	// commit so the caller can finish the bookkeeping.
	if _, err := git.Output(cloneDir, "diff", "--cached", "--quiet"); err == nil {
		return nil, nil
	}

	if _, err := git.Output(cloneDir, "commit", "-m", formatSquashCommitMessage(task, agentLog)); err != nil {
		return nil, fmt.Errorf("commit squash merge: %w", err)
	}
	if _, err := git.Output(cloneDir, "push", "origin", branch); err != nil {
		return nil, fmt.Errorf("%w: push %s: %v", errPushAfterSquashFailed, branch, err)
	}

	// Capture merge result for completion detail.
	sha, _ := git.Output(cloneDir, "rev-parse", "HEAD")
	filesOut, _ := git.Output(cloneDir, "diff", "--name-only", "HEAD~1..HEAD")
	var filesChanged []string
	for _, f := range strings.Split(strings.TrimSpace(filesOut), "\n") {
		if f != "" {
			filesChanged = append(filesChanged, f)
		}
	}

	return &mergeResult{
		commitSHA:    strings.TrimSpace(sha),
		filesChanged: filesChanged,
	}, nil
}

// formatSquashCommitMessage builds the squash-merge commit message.
// It prefers the agent's commit message (from agentLog) for the subject and
// body, falling back to the task title when no agent log is available.
// Task-ID and Affects trailers are always appended when present.
func formatSquashCommitMessage(task mergeQueueTask, agentLog string) string {
	subject, body := parseAgentCommitLog(agentLog)
	if subject == "" {
		subject = task.title
	}

	var trailers []string
	if task.id != "" {
		trailers = append(trailers, "Task-ID: "+task.id)
	}
	if len(task.affects) > 0 {
		trailers = append(trailers, "Affects: "+strings.Join(task.affects, ", "))
	}

	var parts []string
	parts = append(parts, subject)
	if body != "" || len(trailers) > 0 {
		parts = append(parts, "") // blank line after subject
	}
	if body != "" {
		parts = append(parts, body)
	}
	if len(trailers) > 0 {
		if body != "" {
			parts = append(parts, "") // blank line before trailers
		}
		parts = append(parts, strings.Join(trailers, "\n"))
	}

	return strings.Join(parts, "\n")
}

// parseAgentCommitLog extracts the subject and body from the agent's commit
// log output. For multi-commit branches, only the first commit's message is
// used (the agent is expected to make one primary commit). Lines matching
// "Task: <filename>" and "Changed files:" sections are stripped from the body
// since that metadata is redundant with the trailers.
func parseAgentCommitLog(log string) (subject, body string) {
	log = strings.TrimSpace(log)
	if log == "" {
		return "", ""
	}

	lines := strings.Split(log, "\n")

	// First non-empty line is the subject.
	var subjectLine string
	bodyStart := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			subjectLine = strings.TrimSpace(line)
			bodyStart = i + 1
			break
		}
	}
	if subjectLine == "" {
		return "", ""
	}

	// Skip the blank line after the subject.
	if bodyStart < len(lines) && strings.TrimSpace(lines[bodyStart]) == "" {
		bodyStart++
	}

	// Collect body lines, filtering out mechanical "Task:" and "Changed files:" sections.
	var bodyLines []string
	skipChangedFiles := false
	for i := bodyStart; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])

		// Stop at the next commit boundary (double blank lines typically
		// separate commits in git log --format=%B output).
		if skipChangedFiles && trimmed == "" {
			// End of the changed files block; stop processing this commit.
			break
		}
		if skipChangedFiles {
			continue
		}

		if strings.HasPrefix(trimmed, "Task:") {
			continue
		}
		if trimmed == "Changed files:" {
			skipChangedFiles = true
			continue
		}

		bodyLines = append(bodyLines, lines[i])
	}

	// Trim trailing blank lines.
	for len(bodyLines) > 0 && strings.TrimSpace(bodyLines[len(bodyLines)-1]) == "" {
		bodyLines = bodyLines[:len(bodyLines)-1]
	}

	return subjectLine, strings.Join(bodyLines, "\n")
}

func configureMergeCloneIdentity(repoRoot, cloneDir string) error {
	name, _ := git.Output(repoRoot, "config", "user.name")
	if strings.TrimSpace(name) == "" {
		name, _ = git.Output("", "config", "--global", "user.name")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "mato"
	}

	email, _ := git.Output(repoRoot, "config", "user.email")
	if strings.TrimSpace(email) == "" {
		email, _ = git.Output("", "config", "--global", "user.email")
	}
	email = strings.TrimSpace(email)
	if email == "" {
		email = "mato@local.invalid"
	}

	if _, err := git.Output(cloneDir, "config", "user.name", name); err != nil {
		return fmt.Errorf("configure merge user.name: %w", err)
	}
	if _, err := git.Output(cloneDir, "config", "user.email", email); err != nil {
		return fmt.Errorf("configure merge user.email: %w", err)
	}
	return nil
}

func taskBranchName(task mergeQueueTask) string {
	if task.branch != "" {
		return task.branch
	}
	return "task/" + frontmatter.SanitizeBranchName(task.name)
}

func handleMergeFailure(repoRoot, tasksDir string, task mergeQueueTask, mergeErr error) error {
	if err := failMergeTask(task.path, mergeFailureDestination(tasksDir, task.path, task.name), mergeErr.Error()); err != nil {
		return err
	}
	if errors.Is(mergeErr, errSquashMergeConflict) {
		cleanupTaskBranch(repoRoot, taskBranchName(task))
	}
	return nil
}

func mergeFailureDestination(tasksDir, taskPath, taskName string) string {
	dir := queue.DirBacklog
	if shouldFailTask(taskPath) {
		dir = queue.DirFailed
	}
	return filepath.Join(tasksDir, dir, taskName)
}

func shouldFailTask(taskPath string) bool {
	maxRetries := 3
	meta, _, err := frontmatter.ParseTaskFile(taskPath)
	if err == nil {
		maxRetries = meta.MaxRetries
	}

	failures, failErr := queue.CountFailureLines(taskPath)
	if failErr != nil {
		// Can't read the file — conservative choice: don't move to failed.
		return false
	}

	return failures >= maxRetries
}

func cleanupTaskBranch(repoRoot, branchName string) {
	// Clean up the stale task branch so the next agent can push a fresh one.
	// Cleanup is best-effort: log warnings but never abort the merge flow.
	if _, err := git.Output(repoRoot, "branch", "-D", branchName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete local task branch %s: %v\n", branchName, err)
	}
	if _, err := git.Output(repoRoot, "push", "origin", "--delete", branchName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete remote task branch %s: %v\n", branchName, err)
	}
}

func failMergeTask(src, dst, reason string) error {
	reason = strings.TrimSpace(reason)
	reason = strings.ReplaceAll(reason, "\r", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	reason = strings.ReplaceAll(reason, "--", "—")
	if reason == "" {
		reason = "merge queue failure"
	}

	appendErr := appendTaskRecord(src, "<!-- failure: merge-queue at %s — %s -->", time.Now().UTC().Format(time.RFC3339), reason)
	if appendErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not append failure record to %s: %v\n", filepath.Base(src), appendErr)
	}
	if dst == "" {
		return appendErr
	}
	if err := queue.AtomicMove(src, dst); err != nil {
		if appendErr != nil {
			return fmt.Errorf("move task file after merge failure: %w (also failed to append failure record: %v)", err, appendErr)
		}
		return fmt.Errorf("move task file after merge failure: %w", err)
	}
	return nil
}

func markTaskMerged(path string) error {
	if taskHasMergeSuccessRecord(path) {
		return nil
	}
	if err := appendTaskRecord(path, "%s%s -->", mergedTaskRecordPrefix, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("append merged record: %w", err)
	}
	return nil
}

func taskHasMergeSuccessRecord(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), mergedTaskRecordPrefix)
}

func appendTaskRecord(path, format string, args ...any) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read task file for merge record: %w", err)
	}

	record := fmt.Sprintf(format, args...)
	updated := string(existing) + "\n" + record + "\n"

	if err := atomicwrite.WriteFile(path, []byte(updated)); err != nil {
		return fmt.Errorf("write merge record: %w", err)
	}
	return nil
}

func moveTaskWithRetry(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create task destination dir: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := queue.AtomicMove(src, dst); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}

// AcquireLock attempts to acquire an exclusive merge lock.
// Returns a cleanup function and true if acquired, or nil and false if already held.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireLock(tasksDir string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Acquire(locksDir, "merge")
}
