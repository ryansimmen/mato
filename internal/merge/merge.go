package merge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/process"
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

var branchRe = regexp.MustCompile(`<!-- branch:\s*(\S+)`)

var errTaskBranchNotPushed = errors.New("task branch not pushed by agent")
var errSquashMergeConflict = errors.New("squash merge conflict")
var errPushAfterSquashFailed = errors.New("push failed after squash merge")

const mergedTaskRecordPrefix = "<!-- merged: merge-queue at "

// ProcessQueue merges completed task branches into the target branch.
// It scans ready-to-merge/ for task files, prefers branch metadata recorded in
// each task file, falls back to the filename-derived branch name for backward
// compatibility, and performs a squash merge.
// Returns the number of tasks successfully merged.
func ProcessQueue(repoRoot, tasksDir, branch string) int {
	readyDir := filepath.Join(tasksDir, "ready-to-merge")
	entries, err := os.ReadDir(readyDir)
	if err != nil {
		return 0
	}

	tasks := make([]mergeQueueTask, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(readyDir, entry.Name())
		meta, body, err := frontmatter.ParseTaskFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse ready-to-merge task %s: %v\n", entry.Name(), err)
			if failureErr := failMergeTask(path, filepath.Join(tasksDir, "backlog", entry.Name()), fmt.Sprintf("parse task file: %v", err)); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not requeue task %s: %v\n", entry.Name(), failureErr)
			}
			continue
		}

		tasks = append(tasks, mergeQueueTask{
			name:     entry.Name(),
			path:     path,
			title:    frontmatter.ExtractTitle(entry.Name(), body),
			priority: meta.Priority,
			branch:   parseBranchFromFile(path),
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
		completedPath := filepath.Join(tasksDir, "completed", task.name)
		if taskHasMergeSuccessRecord(task.path) {
			if err := moveTaskWithRetry(task.path, completedPath); err != nil {
				fmt.Fprintf(os.Stderr, "warning: merged task %s but could not move to completed: %v\n", task.name, err)
				continue
			}
			merged++
			continue
		}

		if err := mergeReadyTask(repoRoot, branch, task); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not merge task %s: %v\n", task.name, err)
			if failureErr := handleMergeFailure(repoRoot, tasksDir, task, err); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not record merge failure for task %s: %v\n", task.name, failureErr)
			}
			continue
		}
		if err := markTaskMerged(task.path); err != nil {
			fmt.Fprintf(os.Stderr, "warning: merged task %s but could not mark completion: %v\n", task.name, err)
			// Continue to moveTaskWithRetry: moving to completed/ is
			// more important than the merged record.  If the move also
			// fails, the next cycle will detect the already-merged
			// branch via the idempotent squash check.
		}
		if err := moveTaskWithRetry(task.path, completedPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: merged task %s but could not move to completed: %v\n", task.name, err)
			continue
		}
		merged++
	}

	return merged
}

func HasReadyTasks(tasksDir string) bool {
	entries, err := os.ReadDir(filepath.Join(tasksDir, "ready-to-merge"))
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

func mergeReadyTask(repoRoot, branch string, task mergeQueueTask) error {
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		return fmt.Errorf("create temp clone: %w", err)
	}
	defer git.RemoveClone(cloneDir)

	if err := configureMergeCloneIdentity(repoRoot, cloneDir); err != nil {
		return err
	}
	if _, err := git.Output(cloneDir, "fetch", "origin"); err != nil {
		return fmt.Errorf("fetch origin: %w", err)
	}
	if _, err := git.Output(cloneDir, "checkout", "-B", branch, "origin/"+branch); err != nil {
		return fmt.Errorf("checkout target branch %s: %w", branch, err)
	}

	taskBranch := taskBranchName(task)
	if _, err := git.Output(cloneDir, "rev-parse", "--verify", "origin/"+taskBranch); err != nil {
		return fmt.Errorf("%w: task branch %s not found on origin (agent may not have pushed)", errTaskBranchNotPushed, taskBranch)
	}
	if _, err := git.Output(cloneDir, "merge", "--squash", "origin/"+taskBranch); err != nil {
		return fmt.Errorf("%w: %s: %v", errSquashMergeConflict, taskBranch, err)
	}

	// If the squash produced no staged changes, the task branch is already
	// fully merged into the target (e.g. a prior push succeeded but
	// post-push bookkeeping failed).  Return success without a duplicate
	// commit so the caller can finish the bookkeeping.
	if _, err := git.Output(cloneDir, "diff", "--cached", "--quiet"); err == nil {
		return nil
	}

	if _, err := git.Output(cloneDir, "commit", "-m", formatSquashCommitMessage(task)); err != nil {
		return fmt.Errorf("commit squash merge: %w", err)
	}
	if _, err := git.Output(cloneDir, "push", "origin", branch); err != nil {
		return fmt.Errorf("%w: push %s: %v", errPushAfterSquashFailed, branch, err)
	}

	return nil
}

func formatSquashCommitMessage(task mergeQueueTask) string {
	var trailers []string
	if task.id != "" {
		trailers = append(trailers, "Task-ID: "+task.id)
	}
	if len(task.affects) > 0 {
		trailers = append(trailers, "Affects: "+strings.Join(task.affects, ", "))
	}
	if len(trailers) == 0 {
		return task.title
	}
	return task.title + "\n\n" + strings.Join(trailers, "\n")
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

func parseBranchFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := branchRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return ""
	}
	return m[1]
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
	dir := "backlog"
	if shouldFailTask(taskPath) {
		dir = "failed"
	}
	return filepath.Join(tasksDir, dir, taskName)
}

func shouldFailTask(taskPath string) bool {
	maxRetries := 3
	meta, _, err := frontmatter.ParseTaskFile(taskPath)
	if err == nil {
		maxRetries = meta.MaxRetries
	}

	data, err := os.ReadFile(taskPath)
	if err != nil {
		return false
	}

	failures := strings.Count(string(data), "<!-- failure:")
	return failures >= maxRetries
}

func cleanupTaskBranch(repoRoot, branchName string) {
	// Clean up the stale task branch so the next agent can push a fresh one.
	_, _ = git.Output(repoRoot, "branch", "-D", branchName)
	_, _ = git.Output(repoRoot, "push", "origin", "--delete", branchName)
}

func failMergeTask(src, dst, reason string) error {
	reason = strings.TrimSpace(reason)
	reason = strings.ReplaceAll(reason, "\r", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	reason = strings.ReplaceAll(reason, "--", "—")
	if reason == "" {
		reason = "merge queue failure"
	}

	if err := appendTaskRecord(src, "<!-- failure: merge-queue at %s — %s -->", time.Now().UTC().Format(time.RFC3339), reason); err != nil {
		return err
	}
	if dst == "" {
		return nil
	}
	if err := moveTaskFile(src, dst); err != nil {
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open task file for merge record: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "\n"+format+"\n", args...)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("append merge record: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close task file after merge record: %w", closeErr)
	}
	return nil
}

func moveTaskWithRetry(src, dst string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := moveTaskFile(src, dst); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}

func moveTaskFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create task destination dir: %w", err)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat destination %s: %w", dst, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("rename task file: %w", err)
	}
	return nil
}

// AcquireLock attempts to acquire an exclusive merge lock.
// Returns a cleanup function and true if acquired, or nil and false if already held.
// The lock file stores "PID:starttime" to detect PID reuse.
func AcquireLock(tasksDir string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: create merge locks dir: %v\n", err)
		return nil, false
	}

	lockFile := filepath.Join(locksDir, "merge.lock")
	identity := process.LockIdentity(os.Getpid())

	for attempts := 0; attempts < 2; attempts++ {
		f, err := os.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if _, writeErr := f.WriteString(identity); writeErr != nil {
				f.Close()
				os.Remove(lockFile)
				fmt.Fprintf(os.Stderr, "warning: write merge lock: %v\n", writeErr)
				return nil, false
			}
			if closeErr := f.Close(); closeErr != nil {
				os.Remove(lockFile)
				fmt.Fprintf(os.Stderr, "warning: close merge lock: %v\n", closeErr)
				return nil, false
			}
			return func() { os.Remove(lockFile) }, true
		}
		if !os.IsExist(err) {
			fmt.Fprintf(os.Stderr, "warning: create merge lock: %v\n", err)
			return nil, false
		}

		data, readErr := os.ReadFile(lockFile)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: read merge lock: %v\n", readErr)
			return nil, false
		}

		content := strings.TrimSpace(string(data))
		if content == "" || process.IsLockHolderAlive(content) {
			return nil, false
		}
		if removeErr := os.Remove(lockFile); removeErr != nil && !os.IsNotExist(removeErr) {
			fmt.Fprintf(os.Stderr, "warning: remove stale merge lock: %v\n", removeErr)
			return nil, false
		}
	}

	return nil, false
}
