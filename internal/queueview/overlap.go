package queueview

import (
	"sort"
	"strings"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"

	"github.com/bmatcuk/doublestar/v4"
)

// isGlob is an alias for frontmatter.IsGlob, kept local for readability.
var isGlob = frontmatter.IsGlob

// isInvalidGlob reports whether s is a glob entry with broken syntax.
// This covers entries that doublestar cannot compile and entries that
// combine glob metacharacters with a trailing "/" (ambiguous semantics).
// Callers treat invalid globs as conservative conflicts.
func isInvalidGlob(s string) bool {
	if !isGlob(s) {
		return false
	}
	if strings.HasSuffix(s, "/") {
		return true
	}
	_, err := doublestar.Match(s, "")
	return err != nil
}

// staticPrefix returns the longest directory path before the first glob
// metacharacter. Returns the full string if no metacharacters are present.
func staticPrefix(pattern string) string {
	for i, c := range pattern {
		if c == '*' || c == '?' || c == '[' || c == '{' {
			return pattern[:strings.LastIndex(pattern[:i], "/")+1]
		}
	}
	return pattern
}

type backlogTask struct {
	name     string
	dir      string
	path     string
	priority int
	affects  []string
}

// DeferralInfo describes why a task was excluded from the runnable queue.
type DeferralInfo struct {
	BlockedBy          string
	BlockedByDir       string
	ConflictingAffects []string
}

// DeferredOverlappingTasks returns the set of backlog task filenames that should
// be excluded from the queue because they conflict with higher-priority backlog
// tasks or active tasks in in-progress/ready-for-review/ready-to-merge.
func DeferredOverlappingTasks(tasksDir string, idx *PollIndex) map[string]struct{} {
	detailed := DeferredOverlappingTasksDetailed(tasksDir, idx)
	simple := make(map[string]struct{}, len(detailed))
	for name := range detailed {
		simple[name] = struct{}{}
	}
	return simple
}

// DeferredOverlappingTasksDetailed returns deferred tasks with the reason for deferral.
func DeferredOverlappingTasksDetailed(tasksDir string, idx *PollIndex) map[string]DeferralInfo {
	idx = ensureIndex(tasksDir, idx)
	view := ComputeRunnableBacklogView(tasksDir, idx)
	return view.Deferred
}

func deferredOverlappingTasksDetailedForSnapshots(idx *PollIndex, backlogSnaps []*TaskSnapshot) map[string]DeferralInfo {
	deferred := make(map[string]DeferralInfo)

	tasks := make([]backlogTask, 0, len(backlogSnaps))
	for _, snap := range backlogSnaps {
		tasks = append(tasks, backlogTask{
			name:     snap.Filename,
			path:     snap.Path,
			priority: snap.Meta.Priority,
			affects:  snap.Meta.Affects,
		})
	}

	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].priority != tasks[j].priority {
			return tasks[i].priority < tasks[j].priority
		}
		return tasks[i].name < tasks[j].name
	})

	active := idx.ActiveAffects()
	kept := make([]backlogTask, 0, len(tasks)+len(active))
	for _, at := range active {
		kept = append(kept, backlogTask{
			name:    at.Name,
			dir:     at.Dir,
			affects: at.Affects,
		})
	}
	var unknownBlocker *activeOverlapBlocker
	if len(idx.activeUnknownOverlapBlockers) > 0 {
		unknownBlocker = &idx.activeUnknownOverlapBlockers[0]
	}
	for _, task := range tasks {
		if unknownBlocker != nil && len(task.affects) > 0 {
			deferred[task.name] = DeferralInfo{
				BlockedBy:          unknownBlocker.name,
				BlockedByDir:       unknownBlocker.dir,
				ConflictingAffects: append([]string(nil), task.affects...),
			}
			continue
		}
		isDef := false
		for _, other := range kept {
			overlap := overlappingAffects(task.affects, other.affects)
			if len(overlap) > 0 {
				blockedByDir := other.dir
				if blockedByDir == "" {
					blockedByDir = dirs.Backlog
				}
				deferred[task.name] = DeferralInfo{
					BlockedBy:          other.name,
					BlockedByDir:       blockedByDir,
					ConflictingAffects: overlap,
				}
				isDef = true
				break
			}
		}
		if !isDef {
			task.dir = dirs.Backlog
			kept = append(kept, task)
		}
	}

	return deferred
}

func affectsMatch(a, b string) bool {
	if a == b {
		return true
	}

	aInvalid, bInvalid := isInvalidGlob(a), isInvalidGlob(b)
	if aInvalid || bInvalid {
		pa, pb := a, b
		if aInvalid || isGlob(a) {
			pa = staticPrefix(a)
		}
		if bInvalid || isGlob(b) {
			pb = staticPrefix(b)
		}
		if pa == "" || pb == "" {
			return true
		}
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	}

	if strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) {
		return true
	}
	if strings.HasSuffix(b, "/") && strings.HasPrefix(a, b) {
		return true
	}

	aGlob, bGlob := isGlob(a), isGlob(b)

	if aGlob && isDirPrefix(b) {
		pa := staticPrefix(a)
		if pa == "" {
			return true
		}
		return strings.HasPrefix(pa, b) || strings.HasPrefix(b, pa)
	}
	if bGlob && isDirPrefix(a) {
		pb := staticPrefix(b)
		if pb == "" {
			return true
		}
		return strings.HasPrefix(pb, a) || strings.HasPrefix(a, pb)
	}

	if aGlob && bGlob {
		pa, pb := staticPrefix(a), staticPrefix(b)
		if pa == "" || pb == "" {
			return true
		}
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	}
	if aGlob {
		matched, err := doublestar.Match(a, b)
		if err != nil {
			return true
		}
		return matched
	}
	if bGlob {
		matched, err := doublestar.Match(b, a)
		if err != nil {
			return true
		}
		return matched
	}

	return false
}

func isDirPrefix(s string) bool {
	return strings.HasSuffix(s, "/")
}

func overlappingAffects(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}

	aClean := make([]string, 0, len(a))
	hasPrefixA, hasGlobA := false, false
	for _, item := range a {
		if item == "" {
			continue
		}
		aClean = append(aClean, item)
		if isDirPrefix(item) {
			hasPrefixA = true
		}
		if isGlob(item) {
			hasGlobA = true
		}
	}
	bClean := make([]string, 0, len(b))
	hasPrefixB, hasGlobB := false, false
	for _, item := range b {
		if item == "" {
			continue
		}
		bClean = append(bClean, item)
		if isDirPrefix(item) {
			hasPrefixB = true
		}
		if isGlob(item) {
			hasGlobB = true
		}
	}

	if len(aClean) == 0 || len(bClean) == 0 {
		return nil
	}

	if !hasPrefixA && !hasPrefixB && !hasGlobA && !hasGlobB {
		seen := make(map[string]struct{}, len(aClean))
		for _, item := range aClean {
			seen[item] = struct{}{}
		}
		overlap := make([]string, 0)
		added := make(map[string]struct{})
		for _, item := range bClean {
			if _, ok := seen[item]; !ok {
				continue
			}
			if _, ok := added[item]; ok {
				continue
			}
			added[item] = struct{}{}
			overlap = append(overlap, item)
		}
		sort.Strings(overlap)
		return overlap
	}

	overlap := make([]string, 0)
	added := make(map[string]struct{})
	for _, ai := range aClean {
		for _, bi := range bClean {
			if affectsMatch(ai, bi) {
				if _, ok := added[ai]; !ok {
					added[ai] = struct{}{}
					overlap = append(overlap, ai)
				}
				if _, ok := added[bi]; !ok {
					added[bi] = struct{}{}
					overlap = append(overlap, bi)
				}
			}
		}
	}
	sort.Strings(overlap)
	return overlap
}
