package merge

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mato/internal/frontmatter"
	"mato/internal/git"
)

type mergeQueueTask struct {
	name     string
	path     string
	title    string
	priority int
}

var nonAlphanumDash = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
var multiDash = regexp.MustCompile(`-{2,}`)

// ProcessQueue merges completed task branches into the target branch.
// It scans ready-to-merge/ for task files, determines the task branch name
// from each filename, and performs a squash merge.
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
			title:    taskTitle(entry.Name(), body),
			priority: meta.Priority,
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
		if err := mergeReadyTask(repoRoot, branch, task); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not merge task %s: %v\n", task.name, err)
			if failureErr := failMergeTask(task.path, filepath.Join(tasksDir, "backlog", task.name), err.Error()); failureErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not requeue task %s: %v\n", task.name, failureErr)
			}
			continue
		}
		if err := os.Rename(task.path, filepath.Join(tasksDir, "completed", task.name)); err != nil {
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

	taskBranch := "task/" + sanitizeBranchName(task.name)
	if _, err := git.Output(cloneDir, "merge", "--squash", "origin/"+taskBranch); err != nil {
		return fmt.Errorf("squash merge %s: %w", taskBranch, err)
	}
	if _, err := git.Output(cloneDir, "commit", "-m", task.title); err != nil {
		return fmt.Errorf("commit squash merge: %w", err)
	}
	if _, err := git.Output(cloneDir, "push", "origin", branch); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}

	return nil
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

func taskTitle(name, body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
		if trimmed != "" {
			return trimmed
		}
	}
	return frontmatter.TaskFileStem(name)
}

func failMergeTask(src, dst, reason string) error {
	reason = strings.TrimSpace(reason)
	reason = strings.ReplaceAll(reason, "\r", " ")
	reason = strings.ReplaceAll(reason, "\n", " ")
	reason = strings.ReplaceAll(reason, "--", "—")
	if reason == "" {
		reason = "merge queue failure"
	}

	f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open task file for failure record: %w", err)
	}
	_, writeErr := fmt.Fprintf(f, "\n<!-- failure: merge-queue at %s — %s -->\n",
		time.Now().UTC().Format(time.RFC3339), reason)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("append failure record: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close task file after failure record: %w", closeErr)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move task back to backlog: %w", err)
	}
	return nil
}

// AcquireLock attempts to acquire an exclusive merge lock.
// Returns a cleanup function and true if acquired, or nil and false if already held.
func AcquireLock(tasksDir string) (func(), bool) {
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: create merge locks dir: %v\n", err)
		return nil, false
	}

	lockFile := filepath.Join(locksDir, "merge.lock")
	pidText := strconv.Itoa(os.Getpid())

	for attempts := 0; attempts < 2; attempts++ {
		f, err := os.OpenFile(lockFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if _, writeErr := f.WriteString(pidText); writeErr != nil {
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

		holderPID, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if convErr == nil && isProcessActive(holderPID) {
			return nil, false
		}
		if removeErr := os.Remove(lockFile); removeErr != nil && !os.IsNotExist(removeErr) {
			fmt.Fprintf(os.Stderr, "warning: remove stale merge lock: %v\n", removeErr)
			return nil, false
		}
	}

	return nil, false
}

func isProcessActive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func sanitizeBranchName(name string) string {
	name = strings.TrimSuffix(name, ".md")
	name = nonAlphanumDash.ReplaceAllString(name, "-")
	name = multiDash.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "unnamed"
	}
	return name
}
