package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mato/internal/atomicwrite"
	"mato/internal/taskfile"
)

// stripFailureMarkers delegates to the canonical taskfile.StripFailureMarkers
// implementation. Kept as a thin wrapper to avoid changing call sites.
func stripFailureMarkers(content string) string {
	return taskfile.StripFailureMarkers(content)
}

func normalizeRetryTaskName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("task name must not be empty")
	}
	if strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("invalid task name %q: path separators are not allowed", name)
	}
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	if name == ".md" || name == "..md" || filepath.Base(name) != name {
		return "", fmt.Errorf("invalid task name %q", strings.TrimSuffix(name, ".md"))
	}
	return name, nil
}

// RetryTask moves a task from failed/ back to backlog/ with all failure
// markers stripped. It writes the cleaned content directly to backlog/
// (never mutating the failed/ source) so that a destination collision or
// write error cannot destroy the original file.
func RetryTask(tasksDir, name string) error {
	var err error
	name, err = normalizeRetryTaskName(name)
	if err != nil {
		return err
	}

	failedPath := filepath.Join(tasksDir, DirFailed, name)
	data, err := os.ReadFile(failedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("task %s not found in failed/", strings.TrimSuffix(name, ".md"))
		}
		return fmt.Errorf("read failed task %s: %w", name, err)
	}

	cleaned := stripFailureMarkers(string(data))

	backlogPath := filepath.Join(tasksDir, DirBacklog, name)

	// Check for destination collision before writing.
	if _, err := os.Stat(backlogPath); err == nil {
		return fmt.Errorf("task %s already exists in backlog/", strings.TrimSuffix(name, ".md"))
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

	return nil
}
