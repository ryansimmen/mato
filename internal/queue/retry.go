package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mato/internal/atomicwrite"
)

// failureMarkerPatterns matches all failure-related HTML comment markers that
// should be stripped when retrying a task.
var failureMarkerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?m)^\s*<!-- failure:.*-->\s*$`),
	regexp.MustCompile(`(?m)^\s*<!-- review-failure:.*-->\s*$`),
	regexp.MustCompile(`(?m)^\s*<!-- cycle-failure:.*-->\s*$`),
	regexp.MustCompile(`(?m)^\s*<!-- terminal-failure:.*-->\s*$`),
}

// stripFailureMarkers removes all failure/review-failure/cycle-failure/
// terminal-failure HTML comment lines from content,
// then collapses runs of 3+ consecutive newlines down to 2.
func stripFailureMarkers(content string) string {
	result := content
	for _, re := range failureMarkerPatterns {
		result = re.ReplaceAllString(result, "")
	}
	// Collapse runs of 3+ newlines down to 2.
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(result, "\n") + "\n"
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
