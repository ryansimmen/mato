package queue

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/frontmatter"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
)

// RetryTempInfix is the infix used in temporary file names created by
// RetryTask. Doctor uses this to detect leftover retry temp files.
const RetryTempInfix = ".retry-"

// RetryResult carries the outcome of a single RetryTask call.
type RetryResult struct {
	Filename          string   `json:"filename"`
	DependencyBlocked bool     `json:"dependency_blocked,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

var createRetryTempFileFn = func(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

// stripFailureMarkers delegates to the canonical taskfile.StripFailureMarkers
// implementation. Kept as a thin wrapper to avoid changing call sites.
func stripFailureMarkers(content string) string {
	return taskfile.StripFailureMarkers(content)
}

func normalizeRetryTaskRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("task name must not be empty")
	}
	if strings.Contains(ref, `\`) {
		return "", fmt.Errorf("invalid task name %q: path separators are not allowed", ref)
	}
	if strings.Contains(ref, "/") {
		cleaned := path.Clean(ref)
		if cleaned != ref || strings.HasPrefix(ref, "/") {
			return "", fmt.Errorf("invalid task name %q: path traversal is not allowed", ref)
		}
		for _, segment := range strings.Split(ref, "/") {
			if segment == "." || segment == ".." || segment == "" {
				return "", fmt.Errorf("invalid task name %q: path traversal is not allowed", ref)
			}
		}
	}
	return ref, nil
}

// RetryTask moves a task from failed/ back to backlog/ with all failure
// markers stripped. It writes the cleaned content directly to backlog/
// (never mutating the failed/ source) so that a destination collision or
// write error cannot destroy the original file.
func RetryTask(tasksDir, taskRef string) (RetryResult, error) {
	ref, err := normalizeRetryTaskRef(taskRef)
	if err != nil {
		return RetryResult{}, err
	}

	idx := BuildIndex(tasksDir)
	match, err := resolveFailedTask(idx, ref)
	if err != nil {
		return RetryResult{}, err
	}

	failedPath := match.Path
	data, err := os.ReadFile(failedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return RetryResult{}, fmt.Errorf("task %s not found in failed/", strings.TrimSuffix(ref, ".md"))
		}
		return RetryResult{}, fmt.Errorf("read failed task %s: %w", match.Filename, err)
	}

	cleaned := stripFailureMarkers(string(data))

	backlogDir := filepath.Join(tasksDir, DirBacklog)
	backlogPath := filepath.Join(backlogDir, match.Filename)

	// Write cleaned content to a temp file in backlog/, then atomically
	// move it to the final path. This ensures the backlog path is never
	// visible as an empty placeholder — scanners always see either
	// nothing or the complete task file.
	tmpFile, err := createRetryTempFileFn(backlogDir, "."+match.Filename+RetryTempInfix+"*")
	if err != nil {
		return RetryResult{}, fmt.Errorf("create temp file in backlog: %w", err)
	}
	tmpName := tmpFile.Name()

	if err := tmpFile.Chmod(0o644); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return RetryResult{}, fmt.Errorf("write task to temp file: %w", err)
	}
	if _, err := tmpFile.WriteString(cleaned); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return RetryResult{}, fmt.Errorf("write task to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return RetryResult{}, fmt.Errorf("write task to temp file: %w", err)
	}

	// AtomicMove uses os.Link + os.Remove and returns ErrDestinationExists
	// if the backlog path already exists, providing collision detection
	// without ever exposing an empty placeholder file.
	if err := AtomicMove(tmpName, backlogPath); err != nil {
		os.Remove(tmpName)
		if errors.Is(err, ErrDestinationExists) {
			return RetryResult{}, fmt.Errorf("task %s already exists in backlog/", frontmatter.TaskFileStem(match.Filename))
		}
		return RetryResult{}, fmt.Errorf("move task to backlog: %w", err)
	}

	// Only remove the failed/ copy after the backlog/ write succeeds.
	var removeWarning string
	if err := os.Remove(failedPath); err != nil {
		// The requeue is logically complete; warn but don't fail.
		removeWarning = fmt.Sprintf("could not remove %s after requeue: %v", failedPath, err)
	}

	// Clean up stale runtime state (taskstate, sessionmeta) from the
	// previous failed attempt so a fresh agent run starts clean.
	runtimedata.DeleteRuntimeArtifactsPreservingVerdict(tasksDir, match.Filename)

	result := RetryResult{Filename: match.Filename}
	if removeWarning != "" {
		result.Warnings = append(result.Warnings, removeWarning)
	}

	idx = BuildIndex(tasksDir)
	if blocks, ok := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)[match.Filename]; ok {
		result.DependencyBlocked = true
		result.Warnings = append(result.Warnings, fmt.Sprintf("retried task %s was placed in backlog/ but remains dependency-blocked; next reconcile will move it to waiting/ (blocked by %s)", match.Filename, FormatDependencyBlocks(blocks)))
	}

	return result, nil
}

func resolveFailedTask(idx *PollIndex, taskRef string) (TaskMatch, error) {
	ref := strings.TrimSpace(taskRef)
	filenameRef := ref
	if !strings.HasSuffix(filenameRef, ".md") {
		filenameRef += ".md"
	}
	stemRef := strings.TrimSuffix(filenameRef, ".md")

	var matches []TaskMatch
	for _, snap := range idx.TasksByState(DirFailed) {
		match := TaskMatch{Filename: snap.Filename, State: snap.State, Path: snap.Path, Snapshot: snap}
		if matchesTaskRef(match, ref, filenameRef, stemRef) {
			matches = append(matches, match)
		}
	}
	for _, pf := range idx.ParseFailures() {
		if pf.State != DirFailed {
			continue
		}
		pf := pf
		match := TaskMatch{Filename: pf.Filename, State: pf.State, Path: pf.Path, ParseFailure: &pf}
		if matchesParseFailureRef(match, ref, filenameRef, stemRef) {
			matches = append(matches, match)
		}
	}

	if len(matches) == 0 {
		return TaskMatch{}, fmt.Errorf("task %s not found in failed/", strings.TrimSuffix(ref, ".md"))
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Filename < matches[j].Filename
	})
	if len(matches) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "task reference %q is ambiguous in failed/:", ref)
		for _, m := range matches {
			fmt.Fprintf(&b, "\n- %s/%s (id: %s)", m.State, m.Filename, taskMatchID(m))
		}
		return TaskMatch{}, fmt.Errorf("%s", b.String())
	}
	return matches[0], nil
}
