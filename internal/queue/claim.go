package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mato/internal/frontmatter"
)

// ClaimedTask holds the pre-resolved metadata for a task claimed on the host
// side, before the agent container is launched.
type ClaimedTask struct {
	Filename string // e.g. "add-hello.md"
	Branch   string // e.g. "task/add-hello"
	Title    string // first heading or filename stem
	TaskPath string // host-side path in in-progress/
}

var (
	branchUnsafe = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	branchMulti  = regexp.MustCompile(`-{2,}`)
)

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
		if parseErr == nil && meta.MaxRetries > 0 {
			maxRetries = meta.MaxRetries
		}
		failures := countFailureLines(src)

		if err := os.Rename(src, dst); err != nil {
			// Another agent may have claimed it; try next.
			continue
		}

		if failures >= maxRetries {
			if err := os.Rename(dst, filepath.Join(failedDir, name)); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move retry-exhausted task %s to failed: %v\n", name, err)
			}
			continue
		}

		claimedAt := time.Now().UTC().Format(time.RFC3339)
		if err := prependClaimedBy(dst, agentID, claimedAt); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write claimed-by header for %s: %v\n", name, err)
		}

		branch := "task/" + sanitizeForBranch(name)
		title := extractTitle(name, body)

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
		return err
	}
	header := fmt.Sprintf("<!-- claimed-by: %s  claimed-at: %s -->\n", agentID, claimedAt)
	return os.WriteFile(taskPath, append([]byte(header), existing...), 0o644)
}

func countFailureLines(taskPath string) int {
	data, err := os.ReadFile(taskPath)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "<!-- failure:")
}

func sanitizeForBranch(filename string) string {
	name := strings.TrimSuffix(filename, ".md")
	name = branchUnsafe.ReplaceAllString(name, "-")
	name = branchMulti.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "unnamed"
	}
	return name
}

func extractTitle(filename, body string) string {
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
	return frontmatter.TaskFileStem(filename)
}
