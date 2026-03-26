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
	"mato/internal/identity"
	"mato/internal/lockfile"
	"mato/internal/taskfile"
)

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
	idx := BuildIndex(tasksDir)
	view := ComputeRunnableBacklogView(tasksDir, idx)
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
	locksDir := filepath.Join(tasksDir, ".locks")
	return lockfile.Register(locksDir, agentID)
}

// RecoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
// If the same task already exists in a later-state directory, the
// in-progress copy is treated as stale and removed instead of recovered.
func RecoverOrphanedTasks(tasksDir string) {
	inProgress := filepath.Join(tasksDir, DirInProgress)
	names, err := ListTaskFiles(inProgress)
	if err != nil {
		return
	}
	for _, name := range names {
		src := filepath.Join(inProgress, name)

		if laterDir := laterStateDuplicateDir(tasksDir, name); laterDir != "" {
			if err := os.Remove(src); err != nil {
				if !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "warning: could not remove stale in-progress copy %s: %v\n", name, err)
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "Removing stale in-progress copy of %s (already in %s/)\n", name, laterDir)
			continue
		}

		if agent := ParseClaimedBy(src); agent != "" && identity.IsAgentActive(tasksDir, agent) {
			fmt.Fprintf(os.Stderr, "Skipping in-progress task %s (agent %s still active)\n", name, agent)
			continue
		}

		dst := filepath.Join(tasksDir, DirBacklog, name)
		if err := AtomicMove(src, dst); err != nil {
			if !errors.Is(err, ErrDestinationExists) {
				fmt.Fprintf(os.Stderr, "warning: could not recover orphaned task %s: %v\n", name, err)
				continue
			}
			resolved, err := resolveOrphanCollision(src, dst)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not resolve orphan collision for %s: %v\n", name, err)
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
			fmt.Fprintf(os.Stderr, "warning: could not write failure record for %s: %v\n", name, err)
		}

		fmt.Fprintf(os.Stderr, "Recovered orphaned task %s back to backlog\n", name)
	}
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

	if equivalentOrphanContent(srcData, dstData) {
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

func equivalentOrphanContent(srcData, dstData []byte) bool {
	return bytes.Equal(normalizeOrphanContent(srcData), normalizeOrphanContent(dstData))
}

func normalizeOrphanContent(data []byte) []byte {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!-- claimed-by:") || strings.HasPrefix(trimmed, "<!-- branch:") {
			continue
		}
		filtered = append(filtered, line)
	}
	normalized := strings.TrimSpace(strings.Join(filtered, "\n"))
	if normalized == "" {
		return nil
	}
	return []byte(normalized)
}

func laterStateDuplicateDir(tasksDir, name string) string {
	for _, laterDir := range []string{DirReadyReview, DirReadyMerge, DirCompleted, DirFailed} {
		if _, err := os.Stat(filepath.Join(tasksDir, laterDir, name)); err == nil {
			return laterDir
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "warning: could not check %s for duplicate %s: %v\n", laterDir, name, err)
		}
	}
	return ""
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
	if err := os.Remove(src); err != nil {
		// The move is logically complete (dst exists), so warn but don't fail.
		fmt.Fprintf(os.Stderr, "warning: could not remove source %s after linking to %s: %v\n", src, dst, err)
	}
	return nil
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
		os.Remove(dst)
		return fmt.Errorf("atomic move %s → %s: write destination: %w", src, dst, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("atomic move %s → %s: close destination: %w", src, dst, err)
	}
	if err := os.Remove(src); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove source %s after copying to %s: %v\n", src, dst, err)
	}
	return nil
}
