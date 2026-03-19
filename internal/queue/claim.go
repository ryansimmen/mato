package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/frontmatter"
)

// errFailedDirUnavailable is the sentinel wrapped by FailedDirUnavailableError.
var errFailedDirUnavailable = errors.New("failed directory unavailable for retry-exhausted task")

// FailedDirUnavailableError is returned when a retry-exhausted task cannot be
// moved to failed/ but was safely rolled back to backlog/. It carries the task
// filename so the host loop can exclude it from future selection and avoid
// livelocking on the same task.
type FailedDirUnavailableError struct {
	TaskFilename string // backlog filename that was rolled back
	MoveErr      error  // underlying rename/permission error
}

func (e *FailedDirUnavailableError) Error() string {
	return fmt.Sprintf("%s rolled back to backlog (move-to-failed: %v): %s",
		e.TaskFilename, e.MoveErr, errFailedDirUnavailable)
}

func (e *FailedDirUnavailableError) Unwrap() error { return errFailedDirUnavailable }

// IsFailedDirUnavailable reports whether err wraps the sentinel returned when
// a retry-exhausted task could not be moved to failed/ and was rolled back to
// backlog/.
func IsFailedDirUnavailable(err error) bool {
	return errors.Is(err, errFailedDirUnavailable)
}

// Testing hooks for the claim path. Default to real implementations.
// Tests can override these to inject failures without filesystem permission
// tricks.
var (
	claimPrependFn         = prependClaimedBy
	claimRollbackFn        = os.Rename
	retryExhaustedMoveFn   = os.Rename
	retryExhaustedRollback = os.Rename
)

// ClaimedTask holds the pre-resolved metadata for a task claimed on the host
// side, before the agent container is launched.
type ClaimedTask struct {
	Filename string // e.g. "add-hello.md"
	Branch   string // e.g. "task/add-hello"
	Title    string // first heading or filename stem
	TaskPath string // host-side path in in-progress/
}

// SelectAndClaimTask picks the highest-priority available task, atomically
// moves it to in-progress/, stamps the claimed-by header, and checks the
// retry budget. Tasks whose retry budget is exhausted are moved directly to
// failed/ and skipped. Returns nil when no claimable task remains.
func SelectAndClaimTask(tasksDir, agentID string, deferred map[string]struct{}) (*ClaimedTask, error) {
	candidates, err := selectCandidates(tasksDir, deferred)
	if err != nil {
		return nil, err
	}

	inProgressDir := filepath.Join(tasksDir, "in-progress")
	failedDir := filepath.Join(tasksDir, "failed")
	backlogDir := filepath.Join(tasksDir, "backlog")

	for _, name := range candidates {
		src := filepath.Join(backlogDir, name)
		dst := filepath.Join(inProgressDir, name)

		// Parse metadata and check retry budget before claiming so the
		// claimed-by header doesn't interfere with frontmatter parsing.
		meta, body, parseErr := frontmatter.ParseTaskFile(src)
		maxRetries := 3
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse task metadata for %s, using defaults: %v\n", name, parseErr)
		} else {
			maxRetries = meta.MaxRetries
		}
		failures, failErr := CountFailureLines(src)
		if failErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not count failures for %s, skipping: %v\n", name, failErr)
			continue
		}

		if err := os.Rename(src, dst); err != nil {
			// Another agent may have claimed it; try next.
			continue
		}

		if failures >= maxRetries {
			if err := retryExhaustedMoveFn(dst, filepath.Join(failedDir, name)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move retry-exhausted task %s to failed: %v\n", name, err)
				// Move back to backlog so the task is not left orphaned
				// in in-progress/ without a claimed-by marker.
				if rbErr := retryExhaustedRollback(dst, src); rbErr != nil {
					return nil, fmt.Errorf("retry-exhausted rollback failed for %s (task stranded in in-progress): move-to-failed: %v, rollback: %w", name, err, rbErr)
				}
				// Rollback succeeded, but the task is now back in backlog/
				// while still retry-exhausted. Return a hard error so the
				// host does not immediately re-claim and livelock.
				return nil, &FailedDirUnavailableError{TaskFilename: name, MoveErr: err}
			}
			continue
		}

		claimedAt := time.Now().UTC().Format(time.RFC3339)
		if err := claimPrependFn(dst, agentID, claimedAt); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write claimed-by header for %s: %v\n", name, err)
			// Move the task back to backlog so it is not left in in-progress
			// without ownership metadata, which would confuse RecoverOrphanedTasks
			// and other agents.
			if rbErr := claimRollbackFn(dst, src); rbErr != nil {
				// Both the claimed-by write and the rollback rename failed.
				// The task is now stranded in in-progress/ without ownership.
				// Return a hard error so the host can act instead of silently
				// leaving an orphan.
				return nil, fmt.Errorf("claim rollback failed for %s (task stranded in in-progress): prepend: %v, rollback: %w", name, err, rbErr)
			}
			continue
		}

		branch := "task/" + frontmatter.SanitizeBranchName(name)
		title := frontmatter.ExtractTitle(name, body)

		return &ClaimedTask{
			Filename: name,
			Branch:   branch,
			Title:    title,
			TaskPath: dst,
		}, nil
	}

	return nil, nil
}

// selectCandidates returns the ordered list of claimable task filenames.
// It reads .queue if present, otherwise lists backlog/ alphabetically.
func selectCandidates(tasksDir string, deferred map[string]struct{}) ([]string, error) {
	queueFile := filepath.Join(tasksDir, ".queue")
	backlogDir := filepath.Join(tasksDir, "backlog")

	var candidates []string

	if data, err := os.ReadFile(queueFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.HasSuffix(line, ".md") {
				continue
			}
			if deferred != nil {
				if _, excluded := deferred[line]; excluded {
					continue
				}
			}
			if _, err := os.Stat(filepath.Join(backlogDir, line)); err != nil {
				continue
			}
			candidates = append(candidates, line)
		}
	} else {
		entries, err := os.ReadDir(backlogDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("read backlog dir: %w", err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if deferred != nil {
				if _, excluded := deferred[e.Name()]; excluded {
					continue
				}
			}
			candidates = append(candidates, e.Name())
		}
	}

	return candidates, nil
}

func prependClaimedBy(taskPath, agentID, claimedAt string) error {
	existing, err := os.ReadFile(taskPath)
	if err != nil {
		return fmt.Errorf("read task file for claimed-by header: %w", err)
	}
	header := fmt.Sprintf("<!-- claimed-by: %s  claimed-at: %s -->\n", agentID, claimedAt)
	content := append([]byte(header), existing...)

	dir := filepath.Dir(taskPath)
	tmpFile, err := os.CreateTemp(dir, "."+filepath.Base(taskPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for claimed-by header: %w", err)
	}
	tmpName := tmpFile.Name()
	cleanup := func() {
		tmpFile.Close()
		os.Remove(tmpName)
	}

	if err := tmpFile.Chmod(0o644); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file for claimed-by header: %w", err)
	}
	if _, err := tmpFile.Write(content); err != nil {
		cleanup()
		return fmt.Errorf("write temp file for claimed-by header: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file for claimed-by header: %w", err)
	}
	if err := os.Rename(tmpName, taskPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file for claimed-by header: %w", err)
	}
	return nil
}

// CountFailureLines counts the number of <!-- failure: ... --> HTML comment
// lines in a task file. Used to check retry budgets for both task and review paths.
func CountFailureLines(taskPath string) (int, error) {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return 0, fmt.Errorf("count failure lines: %w", err)
	}
	return strings.Count(string(data), "<!-- failure:"), nil
}
