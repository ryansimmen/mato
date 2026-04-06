// Package queue manages the filesystem-backed task queue with priority-based
// claiming and dependency tracking. It handles task lifecycle transitions,
// atomic file moves between queue directories, and orphan recovery.
package queue

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mato/internal/atomicwrite"
	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/queueview"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
	"mato/internal/ui"
)

var writeBranchMarkerRecoveryFn = WriteBranchMarker

// PushedTaskRecovery records a task recovered from in-progress/ to
// ready-for-review/ after its branch was already pushed.
type PushedTaskRecovery struct {
	Filename     string
	Branch       string
	TargetBranch string
	LastHeadSHA  string
}

// ParseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func ParseClaimedBy(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	agent, _ := taskfile.ParseClaimedBy(data)
	return agent
}

// HasAvailableTasks reports whether there is at least one effective runnable
// backlog task that is not in the deferred exclusion set. This now builds a
// queue index and computes the runnable backlog view so dependency-blocked and
// affects-deferred tasks are excluded consistently with claim selection.
func HasAvailableTasks(tasksDir string, deferred map[string]struct{}) bool {
	idx := queueview.BuildIndex(tasksDir)
	view := queueview.ComputeRunnableBacklogView(tasksDir, idx)
	for _, snap := range view.Runnable {
		if deferred != nil {
			if _, excluded := deferred[snap.Filename]; excluded {
				continue
			}
		}
		return true
	}
	return false
}

// RegisterAgent writes a lock file containing "PID:starttime" so concurrent
// mato instances can detect PID reuse. Falls back to PID-only when start time
// is unavailable (non-Linux). Returns a cleanup function.
func RegisterAgent(tasksDir, agentID string) (func(), error) {
	locksDir := filepath.Join(tasksDir, dirs.Locks)
	return lockfile.Register(locksDir, agentID)
}

// RecoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
// If the same task already exists in a later-state directory, the
// in-progress copy is treated as stale and removed instead of recovered.
func RecoverOrphanedTasks(tasksDir string) []PushedTaskRecovery {
	var pushedRecoveries []PushedTaskRecovery
	inProgress := filepath.Join(tasksDir, dirs.InProgress)
	names, err := taskfile.ListTaskFiles(inProgress)
	if err != nil {
		return nil
	}
	for _, name := range names {
		src := filepath.Join(inProgress, name)

		laterDir, warns := LaterStateDuplicateDir(name, laterStateDirs(tasksDir)...)
		for _, w := range warns {
			ui.Warnf("warning: %v\n", w)
		}
		if laterDir != "" {
			if err := os.Remove(src); err != nil {
				if !os.IsNotExist(err) {
					ui.Warnf("warning: could not remove stale in-progress copy %s: %v\n", name, err)
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "Removing stale in-progress copy of %s (already in %s/)\n", name, laterDir)
			continue
		}

		if agent := ParseClaimedBy(src); agent != "" {
			status, err := identity.DescribeAgentActivity(tasksDir, agent)
			if err != nil {
				ui.Warnf("warning: could not verify agent %s for in-progress task %s: %v\n", agent, name, err)
				continue
			}
			if status == identity.AgentActive {
				fmt.Fprintf(os.Stderr, "Skipping in-progress task %s (agent %s still active)\n", name, agent)
				continue
			}
		}

		if recovery, recovered, err := recoverPushedTaskToReadyReview(tasksDir, name, src); recovered {
			if err != nil {
				ui.Warnf("warning: could not recover pushed task %s to ready-for-review: %v\n", name, err)
			} else if recovery != nil {
				pushedRecoveries = append(pushedRecoveries, *recovery)
			}
			continue
		}

		dst := filepath.Join(tasksDir, dirs.Backlog, name)
		if err := AtomicMove(src, dst); err != nil {
			if !errors.Is(err, ErrDestinationExists) {
				ui.Warnf("warning: could not recover orphaned task %s: %v\n", name, err)
				continue
			}
			resolved, err := resolveOrphanCollision(src, dst)
			if err != nil {
				ui.Warnf("warning: could not resolve orphan collision for %s: %v\n", name, err)
				continue
			}
			if resolved == "" {
				// Identical content — dedup'd, nothing more to do.
				continue
			}
			// Different content — orphan was renamed and moved.
			dst = resolved
		}

		content := fmt.Sprintf("\n<!-- failure: mato-recovery at %s — agent was interrupted -->\n",
			time.Now().UTC().Format(time.RFC3339))
		if err := atomicwrite.AppendToFile(dst, content); err != nil {
			ui.Warnf("warning: could not write failure record for %s: %v\n", name, err)
		}

		fmt.Fprintf(os.Stderr, "Recovered orphaned task %s back to backlog\n", name)
	}
	return pushedRecoveries
}

func recoverPushedTaskToReadyReview(tasksDir, name, src string) (*PushedTaskRecovery, bool, error) {
	state, err := runtimedata.LoadTaskState(tasksDir, name)
	if err != nil {
		return nil, true, fmt.Errorf("pushed-task recovery metadata is unavailable: %w", err)
	}
	if state == nil {
		return nil, true, fmt.Errorf("pushed-task recovery metadata is missing")
	}
	switch state.LastOutcome {
	case runtimedata.OutcomeWorkLaunched:
		return nil, false, nil
	case runtimedata.OutcomeWorkBranchPushed:
		// continue
	default:
		return nil, true, fmt.Errorf("pushed-task recovery metadata is unusable (last outcome %q)", state.LastOutcome)
	}

	branch := strings.TrimSpace(state.TaskBranch)
	if branch == "" {
		return nil, true, fmt.Errorf("taskstate for %s is missing task branch", name)
	}

	dst := filepath.Join(tasksDir, dirs.ReadyReview, name)
	if err := AtomicMove(src, dst); err != nil {
		return nil, true, fmt.Errorf("move task to ready-for-review: %w", err)
	}
	if err := writeBranchMarkerRecoveryFn(dst, branch); err != nil {
		if rollbackErr := AtomicMove(dst, src); rollbackErr != nil {
			return nil, true, fmt.Errorf("write branch marker to %s: %w (rollback failed: %v)", dst, err, rollbackErr)
		}
		return nil, true, fmt.Errorf("write branch marker to %s: %w (rolled back to in-progress/)", dst, err)
	}
	targetBranch := strings.TrimSpace(state.TargetBranch)
	lastHeadSHA := strings.TrimSpace(state.LastHeadSHA)
	if err := runtimedata.UpdateTaskState(tasksDir, name, func(state *runtimedata.TaskState) {
		state.TaskBranch = branch
		state.TargetBranch = targetBranch
		state.LastHeadSHA = lastHeadSHA
		state.LastOutcome = runtimedata.OutcomeWorkPushed
	}); err != nil {
		ui.Warnf("warning: could not record recovered pushed taskstate for %s: %v\n", name, err)
	}
	fmt.Fprintf(os.Stderr, "Recovered pushed task %s to ready-for-review\n", name)
	return &PushedTaskRecovery{Filename: name, Branch: branch, TargetBranch: targetBranch, LastHeadSHA: lastHeadSHA}, true, nil
}

// resolveOrphanCollision handles the case where an orphan in in-progress/
// collides with an existing file in backlog/. If the files are identical the
// in-progress copy is removed (dedup) and an empty string is returned. If they
// differ, the orphan is renamed with a "-recovered-<timestamp>" suffix, moved
// to backlog, and the new path is returned.
func resolveOrphanCollision(src, dst string) (string, error) {
	srcData, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read orphan %s: %w", src, err)
	}
	dstData, err := os.ReadFile(dst)
	if err != nil {
		return "", fmt.Errorf("read existing backlog %s: %w", dst, err)
	}

	if equivalentOrphanContent(src, srcData, dst, dstData) {
		if err := os.Remove(src); err != nil {
			return "", fmt.Errorf("remove duplicate orphan %s: %w", src, err)
		}
		fmt.Fprintf(os.Stderr, "Removed duplicate orphan %s (identical copy already in backlog)\n", filepath.Base(src))
		return "", nil
	}

	// Different content — rename with recovery suffix and move.
	ts := time.Now().UTC().Format("20060102T150405Z")
	base := filepath.Base(src)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	recoveredName := fmt.Sprintf("%s-recovered-%s%s", stem, ts, ext)
	recoveredDst := filepath.Join(filepath.Dir(dst), recoveredName)

	if err := AtomicMove(src, recoveredDst); err != nil {
		return "", fmt.Errorf("move renamed orphan to %s: %w", recoveredDst, err)
	}
	fmt.Fprintf(os.Stderr, "Recovered orphan %s as %s (content differs from backlog copy)\n", base, recoveredName)
	return recoveredDst, nil
}

func equivalentOrphanContent(srcPath string, srcData []byte, dstPath string, dstData []byte) bool {
	return bytes.Equal(normalizeOrphanContent(srcPath, srcData), normalizeOrphanContent(dstPath, dstData))
}

func normalizeOrphanContent(path string, data []byte) []byte {
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	start := 0
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	if start >= len(lines) {
		return nil
	}

	if _, ok := taskfile.ParseClaimedBy([]byte(lines[start])); ok {
		start++
		for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
			start++
		}
		if start < len(lines) {
			if branch, ok := taskfile.ParseBranchMarkerLine([]byte(lines[start])); ok && isRuntimeBranchForPath(path, branch) {
				start++
				for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
					start++
				}
			}
		}
	}

	normalized := strings.TrimSpace(strings.Join(lines[start:], "\n"))
	if normalized == "" {
		return nil
	}
	return []byte(normalized)
}

func isRuntimeBranchForPath(path, branch string) bool {
	base := filepath.Base(path)
	expected := "task/" + frontmatter.SanitizeBranchName(base)
	return branch == expected || branch == expected+"-"+frontmatter.BranchDisambiguator(base)
}

// LaterStateDuplicateDir checks whether filename exists in any of the given
// dirs. It returns filepath.Base of the first directory that contains the
// file, or "" if none do. Non-ENOENT stat errors are collected in the
// second return value so the caller can decide how to report them.
func LaterStateDuplicateDir(filename string, dirs ...string) (string, []error) {
	var warnings []error
	for _, dir := range dirs {
		_, err := os.Stat(filepath.Join(dir, filename))
		if err == nil {
			return filepath.Base(dir), warnings
		}
		if !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Errorf("could not check %s for duplicate %s: %w", filepath.Base(dir), filename, err))
		}
	}
	return "", warnings
}

// laterStateDirs returns the default later-state directory paths rooted at
// tasksDir. Used by callers that need the standard set of directories.
func laterStateDirs(tasksDir string) []string {
	return []string{
		filepath.Join(tasksDir, dirs.ReadyReview),
		filepath.Join(tasksDir, dirs.ReadyMerge),
		filepath.Join(tasksDir, dirs.Completed),
		filepath.Join(tasksDir, dirs.Failed),
	}
}

// ErrDestinationExists is returned by AtomicMove when the destination path
// already exists. Callers can check for it with errors.Is.
var ErrDestinationExists = errors.New("destination already exists")

// linkFn is the function used by AtomicMove to create hard links.
// Tests override it to simulate EXDEV without separate filesystems.
var linkFn = os.Link

// readFileFn, openFileFn, and writeFileFn are used by crossDeviceMove.
// Tests override them to inject failures without relying on filesystem
// permissions, which are not portable across root/container environments.
var readFileFn = os.ReadFile
var openFileFn = os.OpenFile
var removeFn = os.Remove
var writeFileFn = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

// AtomicMove atomically moves src to dst using os.Link + os.Remove to prevent
// TOCTOU races. If the destination already exists, it returns
// ErrDestinationExists. On cross-device links (EXDEV), it falls back to
// O_CREATE|O_EXCL + copy + remove, which is still TOCTOU-safe at the
// destination.
func AtomicMove(src, dst string) error {
	if err := linkFn(src, dst); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("atomic move %s → %s: %w", src, dst, ErrDestinationExists)
		}
		// Cross-device link: fall back to exclusive-create + copy.
		if errors.Is(err, syscall.EXDEV) {
			return crossDeviceMove(src, dst)
		}
		return fmt.Errorf("atomic move %s → %s: link: %w", src, dst, err)
	}
	return finalizeAtomicMove(src, dst, "linking")
}

// crossDeviceMove handles the EXDEV case where src and dst are on different
// filesystems. It uses O_CREATE|O_EXCL to atomically fail if the destination
// already exists, then copies the content and removes the source.
func crossDeviceMove(src, dst string) error {
	data, err := readFileFn(src)
	if err != nil {
		return fmt.Errorf("atomic move %s → %s: read source: %w", src, dst, err)
	}
	f, err := openFileFn(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("atomic move %s → %s: %w", src, dst, ErrDestinationExists)
		}
		return fmt.Errorf("atomic move %s → %s: create destination: %w", src, dst, err)
	}
	if err := writeFileFn(f, data); err != nil {
		f.Close()
		if cleanErr := removeFn(dst); cleanErr != nil {
			ui.Warnf("warning: cross-device move cleanup failed for %s: %v\n", dst, cleanErr)
		}
		return fmt.Errorf("atomic move %s → %s: write destination: %w", src, dst, err)
	}
	if err := f.Close(); err != nil {
		if cleanErr := removeFn(dst); cleanErr != nil {
			ui.Warnf("warning: cross-device move cleanup failed for %s: %v\n", dst, cleanErr)
		}
		return fmt.Errorf("atomic move %s → %s: close destination: %w", src, dst, err)
	}
	return finalizeAtomicMove(src, dst, "copying")
}

func finalizeAtomicMove(src, dst, mode string) error {
	if err := removeFn(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		cleanupErr := removeFn(dst)
		if cleanupErr != nil && !os.IsNotExist(cleanupErr) {
			return fmt.Errorf("atomic move %s → %s: remove source after %s: %w (also failed to remove destination during rollback: %v)", src, dst, mode, err, cleanupErr)
		}
		return fmt.Errorf("atomic move %s → %s: remove source after %s: %w", src, dst, mode, err)
	}
	return nil
}
