package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
	"mato/internal/runtimecleanup"
	"mato/internal/taskfile"
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

func normalizeClaimCandidate(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if filepath.Base(name) != name {
		return "", false
	}
	if !strings.HasSuffix(name, ".md") {
		return "", false
	}
	return name, true
}

// defaultRetryCooldown is the default time to wait after a task failure before
// the task becomes eligible for claiming again.
const defaultRetryCooldown = 2 * time.Minute

// Testing hooks for the claim path. Default to real implementations.
// Tests can override these to inject failures without filesystem permission
// tricks.
var (
	claimPrependFn         = prependClaimedBy
	claimRollbackFn        = AtomicMove
	retryExhaustedMoveFn   = AtomicMove
	retryExhaustedRollback = AtomicMove
	claimReadFileFn        = os.ReadFile
	claimWriteFileFn       = atomicwrite.WriteFile
	timeNowFn              = time.Now
)

// ClaimedTask holds the pre-resolved metadata for a task claimed on the host
// side, before the agent container is launched.
type ClaimedTask struct {
	Filename              string // e.g. "add-hello.md"
	Branch                string // e.g. "task/add-hello"
	Title                 string // first heading or filename stem
	TaskPath              string // host-side path in in-progress/
	HadRecordedBranchMark bool   // task already had <!-- branch: ... --> before this claim
}

// CollectActiveBranches scans in-progress/, ready-for-review/, and
// ready-to-merge/ for <!-- branch: ... --> comments and returns a set of
// branch names currently in use. All three directories are checked because a
// task's branch remains active until it is merged or failed.
//
// When idx is non-nil, the index is used instead of scanning the filesystem.
func CollectActiveBranches(tasksDir string, idx *PollIndex) map[string]struct{} {
	if idx != nil {
		return idx.ActiveBranches()
	}
	active := make(map[string]struct{})
	dirs := []string{
		filepath.Join(tasksDir, DirInProgress),
		filepath.Join(tasksDir, DirReadyReview),
		filepath.Join(tasksDir, DirReadyMerge),
	}
	for _, dir := range dirs {
		names, err := ListTaskFiles(dir)
		if err != nil {
			continue
		}
		for _, name := range names {
			if b := readBranchFromFile(filepath.Join(dir, name)); b != "" {
				active[b] = struct{}{}
			}
		}
	}
	return active
}

// readBranchFromFile extracts the branch name from a <!-- branch: ... -->
// comment in a task file. Returns "" if no such comment is found.
func readBranchFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	branch, _ := taskfile.ParseBranchMarkerLine(data)
	return branch
}

// WriteBranchMarker records the branch marker in the task file. It reuses the
// existing marker when it already matches, replaces the first standalone marker
// when it differs, or inserts a new marker after the first claimed-by line.
func WriteBranchMarker(taskPath, branch string) error {
	data, err := claimReadFileFn(taskPath)
	if err != nil {
		return fmt.Errorf("read task file for branch marker: %w", err)
	}

	if existing, ok := taskfile.ParseBranchMarkerLine(data); ok && existing == branch {
		return nil
	}

	if replaced, found, didReplace := taskfile.ReplaceBranchMarkerLine(data, branch); found {
		if !didReplace {
			return nil
		}
		if err := claimWriteFileFn(taskPath, replaced); err != nil {
			return fmt.Errorf("write branch marker: %w", err)
		}
		return nil
	}

	var comment strings.Builder
	if err := taskfile.WriteBranchComment(&comment, branch); err != nil {
		return fmt.Errorf("format branch marker: %w", err)
	}
	commentStr := comment.String()
	lines := strings.Split(string(data), "\n")

	// Find the first claimed-by line and insert after it.
	inserted := false
	var result []string
	for _, line := range lines {
		result = append(result, line)
		if !inserted && strings.HasPrefix(strings.TrimSpace(line), "<!-- claimed-by:") {
			result = append(result, commentStr)
			inserted = true
		}
	}
	if !inserted {
		// No claimed-by found; prepend.
		result = append([]string{commentStr}, result...)
	}

	content := []byte(strings.Join(result, "\n"))

	if err := claimWriteFileFn(taskPath, content); err != nil {
		return fmt.Errorf("write branch marker: %w", err)
	}
	return nil
}

func restoreClaimedTaskContents(path string, content []byte) error {
	if err := claimWriteFileFn(path, content); err != nil {
		return fmt.Errorf("restore claimed task contents: %w", err)
	}
	return nil
}

func chooseClaimBranch(name string, activeBranches map[string]struct{}, existing string) string {
	if existing != "" {
		if _, taken := activeBranches[existing]; !taken {
			return existing
		}
	}
	branch := "task/" + frontmatter.SanitizeBranchName(name)
	if _, taken := activeBranches[branch]; taken {
		branch = branch + "-" + frontmatter.BranchDisambiguator(name)
	}
	return branch
}

// handleRetryExhaustedTask moves a retry-exhausted task from in-progress/ to
// failed/. If the move to failed/ fails, it rolls back to backlog/ and returns
// a FailedDirUnavailableError so the host can avoid livelocking. Returns nil
// when the task was successfully moved to failed/.
func handleRetryExhaustedTask(name, dst, src, failedDir string) error {
	if err := retryExhaustedMoveFn(dst, filepath.Join(failedDir, name)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not move retry-exhausted task %s to failed: %v\n", name, err)
		// Move back to backlog so the task is not left orphaned
		// in in-progress/ without a claimed-by marker.
		if rbErr := retryExhaustedRollback(dst, src); rbErr != nil {
			return fmt.Errorf("retry-exhausted rollback failed for %s (task stranded in in-progress): move-to-failed: %v, rollback: %w", name, err, rbErr)
		}
		// Rollback succeeded, but the task is now back in backlog/
		// while still retry-exhausted. Return a hard error so the
		// host does not immediately re-claim and livelock.
		return &FailedDirUnavailableError{TaskFilename: name, MoveErr: err}
	}
	runtimecleanup.DeleteAll(filepath.Dir(failedDir), name)
	return nil
}

// rollbackClaimToBacklog moves a task from in-progress/ back to backlog/ after
// a claim operation (e.g., writing the claimed-by header) fails. Returns nil
// when the rollback succeeds. Returns a hard error when rollback also fails,
// meaning the task is stranded in in-progress/ without ownership metadata.
func rollbackClaimToBacklog(name, dst, src string, claimErr error) error {
	if rbErr := claimRollbackFn(dst, src); rbErr != nil {
		return fmt.Errorf("claim rollback failed for %s (task stranded in in-progress): prepend: %v, rollback: %w", name, claimErr, rbErr)
	}
	return nil
}

// SelectAndClaimTask picks the first claimable task from the caller-provided
// ordered candidate list, atomically moves it to in-progress/, stamps the
// claimed-by header, and checks the retry budget. Tasks whose retry budget is
// exhausted are moved directly to failed/ and skipped. Returns nil when no
// claimable task remains.
//
// When idx is non-nil, the index is used for active branch lookup and
// pre-parsed metadata. When idx is nil, the filesystem is scanned directly.
func SelectAndClaimTask(tasksDir, agentID string, candidates []string, cooldown time.Duration, idx *PollIndex) (*ClaimedTask, error) {
	idx = ensureIndex(tasksDir, idx)

	inProgressDir := filepath.Join(tasksDir, DirInProgress)
	failedDir := filepath.Join(tasksDir, DirFailed)
	backlogDir := filepath.Join(tasksDir, DirBacklog)

	activeBranches := CollectActiveBranches(tasksDir, idx)
	depLookup := newDependencyLookup(idx)

	for _, name := range candidates {
		name, ok := normalizeClaimCandidate(name)
		if !ok {
			continue
		}
		src := filepath.Join(backlogDir, name)
		dst := filepath.Join(inProgressDir, name)

		// Always re-read the candidate file before claiming so manual edits made
		// after index construction cannot bypass dependency enforcement.
		var meta frontmatter.TaskMeta
		var body string
		var maxRetries int
		var failures int

		var parseErr error
		meta, body, parseErr = frontmatter.ParseTaskFile(src)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse task metadata for %s, skipping until reconciled: %v\n", name, parseErr)
			continue
		}
		maxRetries = meta.MaxRetries
		var failErr error
		failures, failErr = CountFailureLines(src)
		if failErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not count failures for %s, skipping: %v\n", name, failErr)
			continue
		}

		if blocks := depLookup.blockedDependencies(meta.DependsOn); len(blocks) > 0 {
			waitingPath := filepath.Join(tasksDir, DirWaiting, name)
			if err := AtomicMove(src, waitingPath); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move dependency-blocked backlog task %s back to waiting/: %v\n", name, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: moved dependency-blocked backlog task %s back to waiting/ (blocked by %s)\n", name, FormatDependencyBlocks(blocks))
			continue
		}

		// Skip tasks that failed recently (within cooldown window) to
		// prevent rapid retry churn after immediate agent crashes. Tasks that
		// already exhausted their retry budget should still move straight to
		// failed/ without waiting for cooldown.
		if failures > 0 && failures < maxRetries {
			rawData, readErr := os.ReadFile(src)
			if readErr == nil {
				if lastFail, ok := lastFailureTime(rawData); ok {
					if timeNowFn().Sub(lastFail) < retryCooldown(cooldown) {
						continue
					}
				}
			}
		}

		if err := AtomicMove(src, dst); err != nil {
			// Another agent may have claimed it, or the destination
			// already exists (EEXIST). Skip to the next candidate.
			continue
		}

		if failures >= maxRetries {
			if err := handleRetryExhaustedTask(name, dst, src, failedDir); err != nil {
				return nil, err
			}
			continue
		}

		originalData, err := claimReadFileFn(dst)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read claimed task %s before stamping claim metadata: %v\n", name, err)
			if rbErr := rollbackClaimToBacklog(name, dst, src, err); rbErr != nil {
				return nil, rbErr
			}
			continue
		}

		existingBranchBeforeClaim, hadRecordedBranchMark := taskfile.ParseBranchMarkerLine(originalData)

		claimedAt := time.Now().UTC().Format(time.RFC3339)
		if err := claimPrependFn(dst, agentID, claimedAt); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write claimed-by header for %s: %v\n", name, err)
			if rbErr := rollbackClaimToBacklog(name, dst, src, err); rbErr != nil {
				return nil, rbErr
			}
			continue
		}

		claimedData, err := claimReadFileFn(dst)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not read claimed task %s for branch marker: %v\n", name, err)
			if restoreErr := restoreClaimedTaskContents(dst, originalData); restoreErr != nil {
				return nil, fmt.Errorf("read claimed task for branch marker: %w (also failed to restore task contents: %v)", err, restoreErr)
			}
			if rbErr := rollbackClaimToBacklog(name, dst, src, err); rbErr != nil {
				return nil, rbErr
			}
			continue
		}

		existingBranch, _ := taskfile.ParseBranchMarkerLine(claimedData)
		if existingBranch == "" && hadRecordedBranchMark {
			existingBranch = existingBranchBeforeClaim
		}
		branch := chooseClaimBranch(name, activeBranches, existingBranch)
		title := frontmatter.ExtractTitle(name, body)

		if existingBranch != branch {
			if err := WriteBranchMarker(dst, branch); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write branch marker for %s: %v\n", name, err)
				if restoreErr := restoreClaimedTaskContents(dst, originalData); restoreErr != nil {
					return nil, fmt.Errorf("write branch marker for %s: %w (also failed to restore task contents: %v)", name, err, restoreErr)
				}
				if rbErr := rollbackClaimToBacklog(name, dst, src, err); rbErr != nil {
					return nil, rbErr
				}
				continue
			}
		}
		activeBranches[branch] = struct{}{}

		return &ClaimedTask{
			Filename:              name,
			Branch:                branch,
			Title:                 title,
			TaskPath:              dst,
			HadRecordedBranchMark: hadRecordedBranchMark,
		}, nil
	}

	return nil, nil
}

func prependClaimedBy(taskPath, agentID, claimedAt string) error {
	existing, err := os.ReadFile(taskPath)
	if err != nil {
		return fmt.Errorf("read task file for claimed-by header: %w", err)
	}
	var header strings.Builder
	if err := taskfile.WriteClaimedByComment(&header, agentID, claimedAt); err != nil {
		return fmt.Errorf("format claimed-by header: %w", err)
	}
	header.WriteString("\n")
	content := append([]byte(header.String()), existing...)

	if err := atomicwrite.WriteFile(taskPath, content); err != nil {
		return fmt.Errorf("write claimed-by header: %w", err)
	}
	return nil
}

// CountFailureLines counts the number of <!-- failure: ... --> HTML comment
// metadata lines in a task file. Only lines that start with the marker are
// counted so that references to the marker inside the task body are ignored.
// Lines starting with <!-- review-failure: are excluded; those are tracked
// separately via CountReviewFailureLines.
func CountFailureLines(taskPath string) (int, error) {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return 0, fmt.Errorf("count failure lines: %w", err)
	}
	return taskfile.CountFailureMarkers(data), nil
}

// CountReviewFailureLines counts the number of <!-- review-failure: ... -->
// HTML comment metadata lines in a task file. These are review infrastructure
// failures (e.g. network blips during git fetch, diff timeouts) that are
// tracked separately from task agent failures so they don't consume the
// task's retry budget.
func CountReviewFailureLines(taskPath string) (int, error) {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return 0, fmt.Errorf("count review failure lines: %w", err)
	}
	return taskfile.CountReviewFailureMarkers(data), nil
}

// retryCooldown resolves the effective retry cooldown duration, defaulting to
// defaultRetryCooldown when cooldown is zero or negative.
func retryCooldown(cooldown time.Duration) time.Duration {
	if cooldown > 0 {
		return cooldown
	}
	return defaultRetryCooldown
}

// lastFailureTime extracts the timestamp from the most recent
// <!-- failure: ... --> comment in the given data. Returns the zero time
// and false if no failure comment with a valid timestamp is found.
// Lines starting with <!-- review-failure: are excluded.
func lastFailureTime(data []byte) (time.Time, bool) {
	var last time.Time
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "<!-- failure:") || strings.HasPrefix(trimmed, "<!-- review-failure:") {
			continue
		}
		// Format: <!-- failure: AGENTID at 2026-01-01T00:00:00Z step=... -->
		idx := strings.Index(trimmed, " at ")
		if idx < 0 {
			continue
		}
		rest := trimmed[idx+4:]
		spaceIdx := strings.Index(rest, " ")
		if spaceIdx < 0 {
			continue
		}
		ts := rest[:spaceIdx]
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		last = t
		found = true
	}
	return last, found
}
