package queue

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
	"mato/internal/taskfile"
)

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
func RetryTask(tasksDir, taskRef string) error {
	ref, err := normalizeRetryTaskRef(taskRef)
	if err != nil {
		return err
	}

	idx := BuildIndex(tasksDir)
	match, err := resolveFailedTask(idx, ref)
	if err != nil {
		return err
	}

	failedPath := match.Path
	data, err := os.ReadFile(failedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("task %s not found in failed/", strings.TrimSuffix(ref, ".md"))
		}
		return fmt.Errorf("read failed task %s: %w", match.Filename, err)
	}

	cleaned := stripFailureMarkers(string(data))

	backlogPath := filepath.Join(tasksDir, DirBacklog, match.Filename)

	// Check for destination collision before writing.
	if _, err := os.Stat(backlogPath); err == nil {
		return fmt.Errorf("task %s already exists in backlog/", frontmatter.TaskFileStem(match.Filename))
	}

	// Write cleaned content directly to backlog/ — the source in failed/
	// is never modified, so a write error here causes no data loss.
	if err := atomicwrite.WriteFile(backlogPath, []byte(cleaned)); err != nil {
		return fmt.Errorf("write task to backlog: %w", err)
	}

	// Only remove the failed/ copy after the backlog/ write succeeds.
	if err := os.Remove(failedPath); err != nil {
		// The requeue is logically complete; warn but don't fail.
		fmt.Fprintf(os.Stderr, "warning: could not remove %s after requeue: %v\n", failedPath, err)
	}

	idx = BuildIndex(tasksDir)
	if blocks, ok := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)[match.Filename]; ok {
		fmt.Fprintf(os.Stderr, "warning: retried task %s was placed in backlog/ but remains dependency-blocked; next reconcile will move it to waiting/ (blocked by %s)\n", match.Filename, FormatDependencyBlocks(blocks))
	}

	return nil
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
