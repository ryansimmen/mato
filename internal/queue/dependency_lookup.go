package queue

import (
	"sort"
	"strings"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
)

type dependencyLookup struct {
	safeCompleted map[string]struct{}
	ambiguousIDs  map[string]struct{}
	statesByID    map[string][]string
}

func newDependencyLookup(idx *PollIndex) dependencyLookup {
	completedIDs := idx.CompletedIDs()
	nonCompletedIDs := idx.NonCompletedIDs()

	safeCompleted := make(map[string]struct{}, len(completedIDs))
	for id := range completedIDs {
		safeCompleted[id] = struct{}{}
	}
	ambiguousIDs := make(map[string]struct{})
	for id := range nonCompletedIDs {
		if _, dup := safeCompleted[id]; dup {
			delete(safeCompleted, id)
			ambiguousIDs[id] = struct{}{}
		}
	}

	statesByID := make(map[string]map[string]struct{})
	for _, dir := range dirs.All {
		for _, snap := range idx.TasksByState(dir) {
			registerState(statesByID, frontmatter.TaskFileStem(snap.Filename), dir)
			if snap.Meta.ID != "" {
				registerState(statesByID, snap.Meta.ID, dir)
			}
		}
	}
	for _, pf := range idx.ParseFailures() {
		registerState(statesByID, frontmatter.TaskFileStem(pf.Filename), pf.State)
	}

	collapsed := make(map[string][]string, len(statesByID))
	for id, states := range statesByID {
		collapsed[id] = sortedKeys(states)
	}

	return dependencyLookup{
		safeCompleted: safeCompleted,
		ambiguousIDs:  ambiguousIDs,
		statesByID:    collapsed,
	}
}

func registerState(statesByID map[string]map[string]struct{}, id, state string) {
	if id == "" || state == "" {
		return
	}
	states, ok := statesByID[id]
	if !ok {
		states = make(map[string]struct{})
		statesByID[id] = states
	}
	states[state] = struct{}{}
}

func (l dependencyLookup) blockedDependencies(dependsOn []string) []DependencyBlock {
	if len(dependsOn) == 0 {
		return nil
	}
	blocks := make([]DependencyBlock, 0, len(dependsOn))
	for _, dep := range dependsOn {
		if dep == "" {
			continue
		}
		if _, ok := l.safeCompleted[dep]; ok {
			continue
		}
		if _, ok := l.ambiguousIDs[dep]; ok {
			blocks = append(blocks, DependencyBlock{DependencyID: dep, State: "ambiguous"})
			continue
		}
		states := l.statesByID[dep]
		if len(states) == 0 {
			blocks = append(blocks, DependencyBlock{DependencyID: dep, State: "unknown"})
			continue
		}
		blocks = append(blocks, DependencyBlock{DependencyID: dep, State: strings.Join(states, ",")})
	}
	sort.Slice(blocks, func(i, j int) bool {
		return blocks[i].DependencyID < blocks[j].DependencyID
	})
	return blocks
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
