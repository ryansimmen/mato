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

	"github.com/ryansimmen/mato/internal/atomicwrite"
	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/identity"
	"github.com/ryansimmen/mato/internal/lockfile"
	"github.com/ryansimmen/mato/internal/queueview"
	"github.com/ryansimmen/mato/internal/runtimedata"
	"github.com/ryansimmen/mato/internal/taskfile"
	"github.com/ryansimmen/mato/internal/ui"
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

const pushedTaskRecoveryFailurePrefix = "pushed work exists but automatic handoff recovery could not prove a safe ready-for-review destination"

// ParseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func ParseClaimedBy(path string) string {
	data, err := taskfile.ReadRegularTaskFile(path)
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

// RecoverOrphanedTasks repairs files left in in-progress/ after a dead agent.
// Safe pushed-task handoffs are reconstructed into ready-for-review/,
// unrecoverable pushed handoffs are quarantined to failed/ with a
// terminal-failure marker, and confirmed pre-push work is moved back to
// backlog/ with a normal failure record. Tasks claimed by a still-active agent
// are skipped. If the same task already exists in a later-state directory, the
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
				continue
			}
		}

		if recovery, recovered, err := RecoverPushedTaskHandoff(tasksDir, name, src, writeBranchMarkerRecoveryFn); recovered {
			if err != nil {
				ui.Warnf("warning: could not recover pushed task %s to ready-for-review: %v\n", name, err)
			} else if recovery != nil {
				fmt.Fprintf(os.Stderr, "Recovered pushed task %s to ready-for-review\n", name)
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

// RecoverPushedTaskHandoff repairs an in-progress task whose work branch may
// already have been pushed. It only reconstructs ready-for-review/ when the
// review handoff metadata is trustworthy; otherwise it quarantines the task to
// failed/ with a terminal-failure marker instead of leaving it stranded.
func RecoverPushedTaskHandoff(tasksDir, name, src string, writeBranchMarker func(string, string) error) (*PushedTaskRecovery, bool, error) {
	state, err := runtimedata.LoadTaskState(tasksDir, name)
	if err != nil {
		return quarantinePushedTaskRecovery(tasksDir, name, src, fmt.Sprintf("pushed-task recovery metadata is unavailable: %v", err))
	}
	if state == nil {
		return quarantinePushedTaskRecovery(tasksDir, name, src, "pushed-task recovery metadata is missing")
	}
	switch state.LastOutcome {
	case runtimedata.OutcomeWorkLaunched:
		return nil, false, nil
	case runtimedata.OutcomeWorkBranchPushed:
		// continue
	default:
		return quarantinePushedTaskRecovery(tasksDir, name, src, fmt.Sprintf("pushed-task recovery metadata is unusable (last outcome %q)", state.LastOutcome))
	}

	branch := strings.TrimSpace(state.TaskBranch)
	if branch == "" {
		branch = strings.TrimSpace(taskfile.ParseBranch(src))
	}
	if branch == "" {
		return quarantinePushedTaskRecovery(tasksDir, name, src, "pushed-task recovery metadata is missing task branch")
	}

	dst := filepath.Join(tasksDir, dirs.ReadyReview, name)
	if err := AtomicMove(src, dst); err != nil {
		return quarantinePushedTaskRecovery(tasksDir, name, src, fmt.Sprintf("move task to ready-for-review: %v", err))
	}
	if err := writeBranchMarker(dst, branch); err != nil {
		if rollbackErr := AtomicMove(dst, src); rollbackErr != nil {
			return quarantinePushedTaskRecovery(tasksDir, name, dst, fmt.Sprintf("write branch marker to %s: %v (rollback failed: %v)", dst, err, rollbackErr))
		}
		return quarantinePushedTaskRecovery(tasksDir, name, src, fmt.Sprintf("write branch marker to %s: %v (rolled back to in-progress/)", dst, err))
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
	return &PushedTaskRecovery{Filename: name, Branch: branch, TargetBranch: targetBranch, LastHeadSHA: lastHeadSHA}, true, nil
}

// QuarantinePushedTaskHandoff appends a terminal-failure marker to the task at
// taskPath and moves it to failed/ so an untrustworthy review handoff is never
// left in ready-for-review/.
func QuarantinePushedTaskHandoff(tasksDir, name, taskPath, detail string) error {
	_, _, err := quarantinePushedTaskRecovery(tasksDir, name, taskPath, detail)
	return err
}

func quarantinePushedTaskRecovery(tasksDir, name, taskPath, detail string) (*PushedTaskRecovery, bool, error) {
	originalData, haveOriginalData := readPushedTaskRecoveryData(taskPath)
	reason := pushedTaskRecoveryFailureReason(detail)
	ensureTerminalFailureRecord(taskPath, name, reason)

	dst := filepath.Join(tasksDir, dirs.Failed, name)
	if err := AtomicMove(taskPath, dst); err != nil {
		return nil, true, fmt.Errorf("move task to failed/: %w", err)
	}
	if err := removePushedTaskRecoveryDuplicates(tasksDir, name, taskPath, dst, originalData, haveOriginalData); err != nil {
		return nil, true, fmt.Errorf("remove duplicate live copy after quarantine: %w", err)
	}
	runtimedata.DeleteRuntimeArtifacts(tasksDir, name)
	ui.Warnf("warning: %s for %s; moved task to failed/\n", detail, name)
	return nil, true, nil
}

func readPushedTaskRecoveryData(taskPath string) ([]byte, bool) {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return nil, false
	}
	return data, true
}

func removePushedTaskRecoveryDuplicates(tasksDir, name, taskPath, failedPath string, originalData []byte, haveOriginalData bool) error {
	for _, candidate := range pushedTaskRecoveryDuplicateCandidates(tasksDir, name, taskPath) {
		removeCandidate, err := shouldRemovePushedTaskRecoveryDuplicate(candidate, failedPath, originalData, haveOriginalData)
		if err != nil {
			return err
		}
		if !removeCandidate {
			continue
		}
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove duplicate live copy %s: %w", candidate, err)
		}
	}
	return nil
}

func pushedTaskRecoveryDuplicateCandidates(tasksDir, name, taskPath string) []string {
	switch filepath.Base(filepath.Dir(taskPath)) {
	case dirs.InProgress:
		return []string{filepath.Join(tasksDir, dirs.ReadyReview, name)}
	case dirs.ReadyReview:
		return []string{filepath.Join(tasksDir, dirs.InProgress, name)}
	default:
		return nil
	}
}

func shouldRemovePushedTaskRecoveryDuplicate(candidatePath, failedPath string, originalData []byte, haveOriginalData bool) (bool, error) {
	candidateInfo, err := os.Stat(candidatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat duplicate live copy %s: %w", candidatePath, err)
	}
	failedInfo, err := os.Stat(failedPath)
	if err != nil {
		return false, fmt.Errorf("stat failed task %s: %w", failedPath, err)
	}
	if os.SameFile(candidateInfo, failedInfo) {
		return true, nil
	}
	if !haveOriginalData {
		return false, nil
	}
	candidateData, err := os.ReadFile(candidatePath)
	if err != nil {
		return false, fmt.Errorf("read duplicate live copy %s: %w", candidatePath, err)
	}
	return bytes.Equal(candidateData, originalData), nil
}

func pushedTaskRecoveryFailureReason(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return pushedTaskRecoveryFailurePrefix
	}
	return fmt.Sprintf("%s: %s", pushedTaskRecoveryFailurePrefix, detail)
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
	if err := taskfile.CheckRegularTaskFile(src); err != nil {
		return fmt.Errorf("atomic move %s → %s: verify source: %w", src, dst, err)
	}
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
