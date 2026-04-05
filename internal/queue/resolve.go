package queue

import (
	"fmt"
	"sort"
	"strings"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
)

// TaskMatch is the result of a successful ResolveTask call.
type TaskMatch struct {
	Filename     string
	State        string
	Path         string
	Snapshot     *TaskSnapshot
	ParseFailure *ParseFailure
}

// ResolveTask finds a single task across all queue directories.
func ResolveTask(idx *PollIndex, taskRef string) (TaskMatch, error) {
	ref := strings.TrimSpace(taskRef)
	if ref == "" {
		return TaskMatch{}, fmt.Errorf("task reference must not be empty")
	}
	filenameRef := ref
	if !strings.HasSuffix(filenameRef, ".md") {
		filenameRef += ".md"
	}
	stemRef := strings.TrimSuffix(filenameRef, ".md")

	var matches []TaskMatch
	for _, dir := range dirs.All {
		for _, snap := range idx.TasksByState(dir) {
			match := TaskMatch{
				Filename: snap.Filename,
				State:    snap.State,
				Path:     snap.Path,
				Snapshot: snap,
			}
			if matchesTaskRef(match, ref, filenameRef, stemRef) {
				matches = append(matches, match)
			}
		}
	}
	for _, pf := range idx.ParseFailures() {
		pf := pf
		match := TaskMatch{
			Filename:     pf.Filename,
			State:        pf.State,
			Path:         pf.Path,
			ParseFailure: &pf,
		}
		if matchesParseFailureRef(match, ref, filenameRef, stemRef) {
			matches = append(matches, match)
		}
	}

	if len(matches) == 0 {
		return TaskMatch{}, fmt.Errorf("task not found: %s", ref)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].State != matches[j].State {
			return resolveStateOrder(matches[i].State) < resolveStateOrder(matches[j].State)
		}
		return matches[i].Filename < matches[j].Filename
	})
	if len(matches) > 1 {
		var b strings.Builder
		fmt.Fprintf(&b, "task reference %q is ambiguous:", ref)
		for _, m := range matches {
			fmt.Fprintf(&b, "\n- %s/%s (id: %s)", m.State, m.Filename, taskMatchID(m))
		}
		return TaskMatch{}, fmt.Errorf("%s", b.String())
	}
	return matches[0], nil
}

func matchesTaskRef(match TaskMatch, rawRef, filenameRef, stemRef string) bool {
	if match.Filename == filenameRef || match.Filename == rawRef {
		return true
	}
	stem := frontmatter.TaskFileStem(match.Filename)
	if stem == rawRef || stem == stemRef {
		return true
	}
	return match.Snapshot != nil && match.Snapshot.Meta.ID != "" && match.Snapshot.Meta.ID == rawRef
}

func matchesParseFailureRef(match TaskMatch, rawRef, filenameRef, stemRef string) bool {
	if match.Filename == filenameRef || match.Filename == rawRef {
		return true
	}
	stem := frontmatter.TaskFileStem(match.Filename)
	return stem == rawRef || stem == stemRef
}

func taskMatchID(match TaskMatch) string {
	if match.Snapshot != nil && match.Snapshot.Meta.ID != "" {
		return match.Snapshot.Meta.ID
	}
	return frontmatter.TaskFileStem(match.Filename)
}

func resolveStateOrder(state string) int {
	for i, dir := range dirs.All {
		if dir == state {
			return i
		}
	}
	return len(dirs.All)
}
