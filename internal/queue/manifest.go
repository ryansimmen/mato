package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/atomicwrite"
)

type queueEntry struct {
	name     string
	priority int
}

// ComputeQueueManifest returns the queue manifest content as a string without
// writing it to disk. This is the read-only equivalent of WriteQueueManifest.
// It returns an error if the backlog directory cannot be read.
//
// When idx is nil, the backlog directory is scanned and each task is parsed
// from disk (backward-compatible path). When idx is non-nil, the index is
// used for zero additional I/O.
func ComputeQueueManifest(tasksDir string, exclude map[string]struct{}, idx *PollIndex) (string, error) {
	var queueEntries []queueEntry

	if idx != nil {
		for _, warn := range idx.BuildWarnings() {
			fmt.Fprintf(os.Stderr, "warning: could not build queue index cleanly: read %s: %v\n", warn.Path, warn.Err)
			if warn.State == DirBacklog {
				return "", fmt.Errorf("read backlog dir: %w", warn.Err)
			}
		}
		view := ComputeRunnableBacklogView(tasksDir, idx)
		sorted := sortSnapshotsByPriority(view.Runnable, exclude)
		queueEntries = make([]queueEntry, 0, len(sorted))
		for _, snap := range sorted {
			queueEntries = append(queueEntries, queueEntry{name: snap.Filename, priority: snap.Meta.Priority})
		}
		for _, pf := range idx.BacklogParseFailures() {
			if exclude != nil {
				if _, excluded := exclude[pf.Filename]; excluded {
					continue
				}
			}
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for queue manifest: %v\n", pf.Filename, pf.Err)
		}
		sort.Slice(queueEntries, func(i, j int) bool {
			if queueEntries[i].priority != queueEntries[j].priority {
				return queueEntries[i].priority < queueEntries[j].priority
			}
			return queueEntries[i].name < queueEntries[j].name
		})
	} else {
		idx = BuildIndex(tasksDir)
		for _, warn := range idx.BuildWarnings() {
			fmt.Fprintf(os.Stderr, "warning: could not build queue index cleanly: read %s: %v\n", warn.Path, warn.Err)
			if warn.State == DirBacklog {
				return "", fmt.Errorf("read backlog dir: %w", warn.Err)
			}
		}
		view := ComputeRunnableBacklogView(tasksDir, idx)
		sorted := sortSnapshotsByPriority(view.Runnable, exclude)
		queueEntries = make([]queueEntry, 0, len(sorted))
		for _, snap := range sorted {
			queueEntries = append(queueEntries, queueEntry{name: snap.Filename, priority: snap.Meta.Priority})
		}
		for _, pf := range idx.BacklogParseFailures() {
			if exclude != nil {
				if _, excluded := exclude[pf.Filename]; excluded {
					continue
				}
			}
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for queue manifest: %v\n", pf.Filename, pf.Err)
		}
	}

	lines := make([]string, 0, len(queueEntries))
	for _, entry := range queueEntries {
		lines = append(lines, entry.name)
	}
	manifest := strings.Join(lines, "\n")
	if manifest != "" {
		manifest += "\n"
	}
	return manifest, nil
}

// WriteQueueManifest computes the queue manifest via ComputeQueueManifest
// and atomically writes it to the .queue file in tasksDir.
//
// When idx is nil, each backlog task is parsed from disk.
func WriteQueueManifest(tasksDir string, exclude map[string]struct{}, idx *PollIndex) error {
	manifest, err := ComputeQueueManifest(tasksDir, exclude, idx)
	if err != nil {
		return err
	}
	return atomicwrite.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest))
}
