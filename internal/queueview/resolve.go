package queueview

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
	ref, matches, err := CollectTaskMatches(idx, taskRef, dirs.All)
	if err != nil {
		return TaskMatch{}, err
	}
	if len(matches) == 0 {
		return TaskMatch{}, fmt.Errorf("task not found: %s", ref)
	}
	SortTaskMatches(matches)
	if len(matches) > 1 {
		return TaskMatch{}, fmt.Errorf("%s", FormatAmbiguousTaskMatches(ref, matches, "task reference %q is ambiguous:"))
	}
	return matches[0], nil
}

// CollectTaskMatches finds all tasks matching the given ref in the provided
// queue states. It returns the normalized ref used for matching.
func CollectTaskMatches(idx *PollIndex, taskRef string, states []string) (string, []TaskMatch, error) {
	ref := strings.TrimSpace(taskRef)
	if ref == "" {
		return "", nil, fmt.Errorf("task reference must not be empty")
	}
	filenameRef := ref
	if !strings.HasSuffix(filenameRef, ".md") {
		filenameRef += ".md"
	}
	stemRef := strings.TrimSuffix(filenameRef, ".md")

	allowed := make(map[string]struct{}, len(states))
	for _, state := range states {
		allowed[state] = struct{}{}
	}

	var matches []TaskMatch
	for _, dir := range states {
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
		if _, ok := allowed[pf.State]; !ok {
			continue
		}
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

	return ref, matches, nil
}

// SortTaskMatches applies the canonical deterministic ordering for task-match
// lists.
func SortTaskMatches(matches []TaskMatch) {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].State != matches[j].State {
			return resolveStateOrder(matches[i].State) < resolveStateOrder(matches[j].State)
		}
		return matches[i].Filename < matches[j].Filename
	})
}

// FormatAmbiguousTaskMatches renders a canonical ambiguity message body.
func FormatAmbiguousTaskMatches(ref string, matches []TaskMatch, headerFmt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, headerFmt, ref)
	for _, m := range matches {
		fmt.Fprintf(&b, "\n- %s/%s (id: %s)", m.State, m.Filename, taskMatchID(m))
	}
	return b.String()
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
