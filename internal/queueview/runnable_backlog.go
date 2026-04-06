package queueview

import (
	"fmt"
	"sort"
	"strings"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
)

// DependencyBlock describes why a task dependency is not currently satisfied.
type DependencyBlock struct {
	DependencyID string
	State        string
}

// RunnableBacklogView describes the effective backlog state after excluding
// dependency-blocked and conflict-deferred tasks.
type RunnableBacklogView struct {
	Runnable          []*TaskSnapshot
	Deferred          map[string]DeferralInfo
	DependencyBlocked map[string][]DependencyBlock
}

type dependencyLookup struct {
	safeCompleted map[string]struct{}
	ambiguousIDs  map[string]struct{}
	statesByID    map[string][]string
}

// FormatDependencyBlocks formats dependency blockers for user-facing warnings.
func FormatDependencyBlocks(blocks []DependencyBlock) string {
	if len(blocks) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, fmt.Sprintf("%s (%s)", block.DependencyID, block.State))
	}
	return strings.Join(parts, ", ")
}

// DependencyBlockedBacklogTasksDetailed returns backlog tasks whose
// dependencies are not currently satisfied.
func DependencyBlockedBacklogTasksDetailed(tasksDir string, idx *PollIndex) map[string][]DependencyBlock {
	idx = ensureIndex(tasksDir, idx)
	lookup := newDependencyLookup(idx)
	blocked := make(map[string][]DependencyBlock)
	for _, snap := range idx.TasksByState(dirs.Backlog) {
		blocks := lookup.blockedDependencies(snap.Meta.DependsOn)
		if len(blocks) == 0 {
			continue
		}
		blocked[snap.Filename] = blocks
	}
	return blocked
}

// ComputeRunnableBacklogView derives the effective runnable backlog from the
// current queue snapshot. Dependency-blocked backlog tasks are excluded before
// affects-conflict deferral is applied.
func ComputeRunnableBacklogView(tasksDir string, idx *PollIndex) RunnableBacklogView {
	idx = ensureIndex(tasksDir, idx)

	dependencyBlocked := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	candidates := make([]*TaskSnapshot, 0, len(idx.TasksByState(dirs.Backlog)))
	for _, snap := range idx.TasksByState(dirs.Backlog) {
		if _, blocked := dependencyBlocked[snap.Filename]; blocked {
			continue
		}
		candidates = append(candidates, snap)
	}

	deferred := deferredOverlappingTasksDetailedForSnapshots(idx, candidates)
	deferredNames := make(map[string]struct{}, len(deferred))
	for name := range deferred {
		deferredNames[name] = struct{}{}
	}
	runnable := sortSnapshotsByPriority(candidates, deferredNames)

	return RunnableBacklogView{
		Runnable:          runnable,
		Deferred:          deferred,
		DependencyBlocked: dependencyBlocked,
	}
}

// OrderedRunnableFilenames returns the runnable backlog filenames in claim/
// manifest order after applying any extra caller-provided exclusions.
func OrderedRunnableFilenames(view RunnableBacklogView, exclude map[string]struct{}) []string {
	sorted := sortSnapshotsByPriority(view.Runnable, exclude)
	names := make([]string, 0, len(sorted))
	for _, snap := range sorted {
		names = append(names, snap.Filename)
	}
	return names
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

func sortSnapshotsByPriority(snaps []*TaskSnapshot, exclude map[string]struct{}) []*TaskSnapshot {
	result := make([]*TaskSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		if exclude != nil {
			if _, skipped := exclude[snap.Filename]; skipped {
				continue
			}
		}
		result = append(result, snap)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Meta.Priority != result[j].Meta.Priority {
			return result[i].Meta.Priority < result[j].Meta.Priority
		}
		return result[i].Filename < result[j].Filename
	})
	return result
}
