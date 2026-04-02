package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mato/internal/atomicwrite"
)

// ComputeQueueManifest returns the queue manifest content as a string without
// writing it to disk. This is the read-only equivalent of WriteQueueManifest.
// It returns an error if the backlog directory cannot be read.
//
// When idx is nil, a temporary index and runnable backlog view are built
// internally. When idx is non-nil, the caller-provided index is reused.
func ComputeQueueManifest(tasksDir string, exclude map[string]struct{}, idx *PollIndex) (string, error) {
	idx = ensureIndex(tasksDir, idx)
	view := ComputeRunnableBacklogView(tasksDir, idx)
	return ComputeQueueManifestFromView(tasksDir, exclude, idx, view)
}

// ComputeQueueManifestFromView returns the queue manifest content using a
// caller-provided runnable backlog view. This lets hot paths reuse a single
// ComputeRunnableBacklogView result instead of recomputing it.
func ComputeQueueManifestFromView(tasksDir string, exclude map[string]struct{}, idx *PollIndex, view RunnableBacklogView) (string, error) {
	idx = ensureIndex(tasksDir, idx)

	for _, warn := range idx.BuildWarnings() {
		fmt.Fprintf(os.Stderr, "warning: could not build queue index cleanly: read %s: %v\n", warn.Path, warn.Err)
		if warn.State == DirBacklog && warn.DirLevel {
			return "", fmt.Errorf("read backlog dir: %w", warn.Err)
		}
	}
	lines := OrderedRunnableFilenames(view, exclude)
	for _, pf := range idx.BacklogParseFailures() {
		if exclude != nil {
			if _, excluded := exclude[pf.Filename]; excluded {
				continue
			}
		}
		fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for queue manifest: %v\n", pf.Filename, pf.Err)
	}

	manifest := strings.Join(lines, "\n")
	if manifest != "" {
		manifest += "\n"
	}
	return manifest, nil
}

// WriteQueueManifest computes the queue manifest via ComputeQueueManifest
// and atomically writes it to the .queue file in tasksDir.
func WriteQueueManifest(tasksDir string, exclude map[string]struct{}, idx *PollIndex) error {
	manifest, err := ComputeQueueManifest(tasksDir, exclude, idx)
	if err != nil {
		return err
	}
	return atomicwrite.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest))
}

// WriteQueueManifestFromView computes the queue manifest from a caller-provided
// runnable backlog view and atomically writes it to the .queue file in tasksDir.
func WriteQueueManifestFromView(tasksDir string, exclude map[string]struct{}, idx *PollIndex, view RunnableBacklogView) error {
	manifest, err := ComputeQueueManifestFromView(tasksDir, exclude, idx, view)
	if err != nil {
		return err
	}
	return atomicwrite.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest))
}
