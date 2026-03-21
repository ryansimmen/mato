package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/atomicwrite"
	"mato/internal/frontmatter"
)

type queueEntry struct {
	name     string
	priority int
}

// ComputeQueueManifest returns the queue manifest content as a string without
// writing it to disk. This is the read-only equivalent of WriteQueueManifest.
// It returns an error if the backlog directory cannot be read.
func ComputeQueueManifest(tasksDir string, exclude map[string]struct{}) (string, error) {
	names, err := ListTaskFiles(filepath.Join(tasksDir, DirBacklog))
	if err != nil {
		return "", err
	}

	queueEntries := make([]queueEntry, 0, len(names))
	for _, name := range names {
		if exclude != nil {
			if _, excluded := exclude[name]; excluded {
				continue
			}
		}
		meta, _, err := frontmatter.ParseTaskFile(filepath.Join(tasksDir, DirBacklog, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse backlog task %s for queue manifest: %v\n", name, err)
			continue
		}
		queueEntries = append(queueEntries, queueEntry{name: name, priority: meta.Priority})
	}

	sort.Slice(queueEntries, func(i, j int) bool {
		if queueEntries[i].priority != queueEntries[j].priority {
			return queueEntries[i].priority < queueEntries[j].priority
		}
		return queueEntries[i].name < queueEntries[j].name
	})

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
func WriteQueueManifest(tasksDir string, exclude map[string]struct{}) error {
	manifest, err := ComputeQueueManifest(tasksDir, exclude)
	if err != nil {
		return err
	}
	return atomicwrite.WriteFile(filepath.Join(tasksDir, ".queue"), []byte(manifest))
}
