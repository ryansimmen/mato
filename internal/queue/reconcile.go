package queue

import (
	"fmt"
	"os"
	"path/filepath"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
	"mato/internal/ui"
)

// promotableTask describes a waiting task whose dependencies are satisfied.
type promotableTask struct {
	name      string
	path      string
	meta      frontmatter.TaskMeta
	globError error
}

// resolvePromotableTasks determines which waiting tasks have all dependencies
// met and are not blocked by active overlap. It delegates to
// DiagnoseDependencies for dependency analysis.
//
// When idx is nil, a temporary index is built internally.
func resolvePromotableTasks(tasksDir string, idx *PollIndex) []promotableTask {
	idx = ensureIndex(tasksDir, idx)

	diag := DiagnoseDependencies(tasksDir, idx)
	satisfiedSet := make(map[string]struct{}, len(diag.Analysis.DepsSatisfied))
	for _, id := range diag.Analysis.DepsSatisfied {
		satisfiedSet[id] = struct{}{}
	}

	waitingTasks := idx.TasksByState(dirs.Waiting)
	var result []promotableTask
	for _, snap := range waitingTasks {
		// Only operate on retained files; skip duplicates.
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile != snap.Filename {
			continue
		}
		if _, ok := satisfiedSet[snap.Meta.ID]; !ok {
			continue
		}
		if idx.HasActiveOverlap(snap.Meta.Affects) {
			continue
		}
		result = append(result, promotableTask{name: snap.Filename, path: snap.Path, meta: snap.Meta, globError: snap.GlobError})
	}
	return result
}

// ReconcileReadyQueue promotes waiting tasks whose dependencies are satisfied
// to backlog/. It also moves unparseable waiting/backlog tasks to failed/ and
// moves cycle members to failed/ with cycle-failure markers.
//
// It returns true if any task was moved (promoted or failed), false otherwise.
// When idx is nil, a temporary index is built internally.
func ReconcileReadyQueue(tasksDir string, idx *PollIndex) bool {
	idx = ensureIndex(tasksDir, idx)

	moved := false

	if failUnparseableTasks(tasksDir, idx) {
		moved = true
	}

	if failInvalidGlobBacklog(tasksDir, idx) {
		moved = true
	}

	demoted, demoteMoved := demoteBlockedBacklog(tasksDir, idx)
	if demoteMoved {
		moved = true
	}
	if demoted > 0 {
		idx = ensureIndex(tasksDir, nil)
	}

	diag := DiagnoseDependencies(tasksDir, idx)

	emitDependencyWarnings(diag)

	if failDuplicateWaiting(tasksDir, idx, diag) {
		moved = true
	}

	quarantined, globMoved := failInvalidGlobWaiting(tasksDir, idx, diag)
	if globMoved {
		moved = true
	}

	if failCyclicWaiting(tasksDir, idx, diag, quarantined) {
		moved = true
	}

	if promoteReadyWaiting(tasksDir, idx, diag, quarantined) {
		moved = true
	}

	return moved
}

// failUnparseableTasks moves waiting and backlog tasks with frontmatter parse
// failures to failed/. Returns true if any task was moved.
func failUnparseableTasks(tasksDir string, idx *PollIndex) bool {
	moved := false
	for _, pf := range idx.WaitingParseFailures() {
		ui.Warnf("warning: moving unparseable waiting task %s to failed/: %v\n", pf.Filename, pf.Err)
		if err := taskfile.AppendTerminalFailureRecord(pf.Path, fmt.Sprintf("unparseable frontmatter: %v", pf.Err)); err != nil {
			ui.Warnf("warning: could not append terminal-failure to %s: %v\n", pf.Filename, err)
		}
		failedPath := filepath.Join(tasksDir, dirs.Failed, pf.Filename)
		if moveErr := AtomicMove(pf.Path, failedPath); moveErr != nil {
			ui.Warnf("warning: could not move %s to failed/: %v\n", pf.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, pf.Filename)
			moved = true
		}
	}
	for _, pf := range idx.BacklogParseFailures() {
		ui.Warnf("warning: moving unparseable backlog task %s to failed/: %v\n", pf.Filename, pf.Err)
		if err := taskfile.AppendTerminalFailureRecord(pf.Path, fmt.Sprintf("unparseable frontmatter: %v", pf.Err)); err != nil {
			ui.Warnf("warning: could not append terminal-failure to %s: %v\n", pf.Filename, err)
		}
		failedPath := filepath.Join(tasksDir, dirs.Failed, pf.Filename)
		if moveErr := AtomicMove(pf.Path, failedPath); moveErr != nil {
			ui.Warnf("warning: could not move %s to failed/: %v\n", pf.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, pf.Filename)
			moved = true
		}
	}
	return moved
}

// failInvalidGlobBacklog moves backlog tasks with invalid glob syntax in their
// affects field to failed/. Returns true if any task was moved.
func failInvalidGlobBacklog(tasksDir string, idx *PollIndex) bool {
	moved := false
	for _, snap := range idx.TasksByState(dirs.Backlog) {
		if snap.GlobError != nil {
			ui.Warnf("warning: moving backlog task %s with invalid glob to failed/: %v\n", snap.Filename, snap.GlobError)
			if appendErr := taskfile.AppendTerminalFailureRecord(snap.Path, fmt.Sprintf("invalid glob syntax: %v", snap.GlobError)); appendErr != nil {
				ui.Warnf("warning: could not append terminal-failure to %s: %v\n", snap.Filename, appendErr)
			}
			failedPath := filepath.Join(tasksDir, dirs.Failed, snap.Filename)
			if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
				ui.Warnf("warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
			} else {
				deleteTaskState(tasksDir, snap.Filename)
				moved = true
			}
		}
	}
	return moved
}

// demoteBlockedBacklog moves backlog tasks with unsatisfied dependencies back
// to waiting/. It returns the number of demoted tasks and whether any task was
// moved.
func demoteBlockedBacklog(tasksDir string, idx *PollIndex) (int, bool) {
	blockedBacklog := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	if len(blockedBacklog) == 0 {
		return 0, false
	}
	demoted := 0
	moved := false
	for _, snap := range idx.TasksByState(dirs.Backlog) {
		blocks, blocked := blockedBacklog[snap.Filename]
		if !blocked {
			continue
		}
		waitingPath := filepath.Join(tasksDir, dirs.Waiting, snap.Filename)
		if err := AtomicMove(snap.Path, waitingPath); err != nil {
			ui.Warnf("warning: could not move dependency-blocked backlog task %s back to waiting/: %v\n", snap.Filename, err)
			continue
		}
		ui.Warnf("warning: moved dependency-blocked backlog task %s back to waiting/ (blocked by %s)\n", snap.Filename, FormatDependencyBlocks(blocks))
		moved = true
		demoted++
	}
	return demoted, moved
}

// emitDependencyWarnings logs structured warnings from dependency diagnostics
// issues (ambiguous IDs, duplicates, self-cycles, cycles, unknown IDs).
func emitDependencyWarnings(diag DependencyDiagnostics) {
	for _, issue := range diag.Issues {
		switch issue.Kind {
		case DependencyAmbiguousID:
			ui.Warnf("warning: task ID %q exists in both completed and non-completed directories; dependency on it will not be satisfied\n", issue.TaskID)
		case DependencyDuplicateID:
			ui.Warnf("warning: duplicate waiting task ID %q: %s and %s\n", issue.TaskID, issue.DependsOn, issue.Filename)
		case DependencySelfCycle:
			ui.Warnf("warning: task %s depends on itself\n", issue.TaskID)
		case DependencyCycle:
			ui.Warnf("warning: task %s is part of a circular dependency\n", issue.TaskID)
		case DependencyUnknownID:
			ui.Warnf("warning: waiting task %s depends on unknown task ID %q (not found in any queue directory)\n", issue.Filename, issue.DependsOn)
		}
	}
}

// failDuplicateWaiting moves duplicate waiting files to failed/ with
// terminal-failure markers. A file is a duplicate if its task ID appears in
// RetainedFiles but its filename is not the retained copy. Returns true if
// any task was moved.
func failDuplicateWaiting(tasksDir string, idx *PollIndex, diag DependencyDiagnostics) bool {
	moved := false
	for _, snap := range idx.TasksByState(dirs.Waiting) {
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile == snap.Filename {
			continue // retained copy or unknown ID — skip
		}
		reason := fmt.Sprintf("duplicate waiting task ID %q (retained copy: %s)", snap.Meta.ID, retainedFile)
		ui.Warnf("warning: moving duplicate waiting task %s to failed/: %s\n", snap.Filename, reason)
		if err := taskfile.AppendTerminalFailureRecord(snap.Path, reason); err != nil {
			ui.Warnf("warning: could not append terminal-failure to %s: %v\n", snap.Filename, err)
		}
		failedPath := filepath.Join(tasksDir, dirs.Failed, snap.Filename)
		if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
			ui.Warnf("warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, snap.Filename)
			moved = true
		}
	}
	return moved
}

// failInvalidGlobWaiting moves retained waiting tasks with invalid glob syntax
// to failed/. It returns a set of quarantined filenames so later phases (cycle
// detection, promotion) skip already-moved tasks, and a boolean indicating
// whether any task was actually moved.
func failInvalidGlobWaiting(tasksDir string, idx *PollIndex, diag DependencyDiagnostics) (map[string]struct{}, bool) {
	quarantined := make(map[string]struct{})
	moved := false
	for _, snap := range idx.TasksByState(dirs.Waiting) {
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile != snap.Filename {
			continue
		}
		if snap.GlobError == nil {
			continue
		}
		ui.Warnf("warning: moving waiting task %s with invalid glob to failed/: %v\n", snap.Filename, snap.GlobError)
		if appendErr := taskfile.AppendTerminalFailureRecord(snap.Path, fmt.Sprintf("invalid glob syntax: %v", snap.GlobError)); appendErr != nil {
			ui.Warnf("warning: could not append terminal-failure to %s: %v\n", snap.Filename, appendErr)
		}
		failedPath := filepath.Join(tasksDir, dirs.Failed, snap.Filename)
		if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
			ui.Warnf("warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, snap.Filename)
			moved = true
		}
		// Always mark as quarantined so the promotion pass skips this
		// task even when the move to failed/ did not succeed.
		quarantined[snap.Filename] = struct{}{}
	}
	return quarantined, moved
}

// failCyclicWaiting moves waiting tasks that are part of dependency cycles to
// failed/ with cycle-failure markers. It skips quarantined and duplicate files.
// Returns true if any task was moved.
func failCyclicWaiting(tasksDir string, idx *PollIndex, diag DependencyDiagnostics, quarantined map[string]struct{}) bool {
	// Build a lookup from retained task ID to waiting snapshot.
	waitingByID := make(map[string]*TaskSnapshot)
	for _, snap := range idx.TasksByState(dirs.Waiting) {
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile != snap.Filename {
			continue
		}
		if _, ok := quarantined[snap.Filename]; ok {
			continue
		}
		waitingByID[snap.Meta.ID] = snap
	}

	moved := false
	for _, scc := range diag.Analysis.Cycles {
		for _, id := range scc {
			snap, ok := waitingByID[id]
			if !ok {
				continue
			}
			// Check for existing cycle-failure marker (idempotency).
			data, err := os.ReadFile(snap.Path)
			if err != nil {
				ui.Warnf("warning: could not read %s for cycle-failure check: %v\n", snap.Filename, err)
				continue
			}
			if !taskfile.ContainsCycleFailure(data) {
				if err := taskfile.AppendCycleFailureRecord(snap.Path); err != nil {
					ui.Warnf("warning: could not append cycle-failure record to %s: %v\n", snap.Filename, err)
					continue
				}
			}
			failedPath := filepath.Join(tasksDir, dirs.Failed, snap.Filename)
			if err := AtomicMove(snap.Path, failedPath); err != nil {
				// The idempotency check prevents duplicate records on the next pass.
				ui.Warnf("warning: could not move cycle member %s to failed/: %v\n", snap.Filename, err)
			} else {
				deleteTaskState(tasksDir, snap.Filename)
				moved = true
			}
		}
	}
	return moved
}

// promoteReadyWaiting moves waiting tasks whose dependencies are satisfied and
// have no active-affects overlap to backlog/. It skips quarantined and
// duplicate files. Returns true if any task was moved.
func promoteReadyWaiting(tasksDir string, idx *PollIndex, diag DependencyDiagnostics, quarantined map[string]struct{}) bool {
	satisfiedSet := make(map[string]struct{}, len(diag.Analysis.DepsSatisfied))
	for _, id := range diag.Analysis.DepsSatisfied {
		satisfiedSet[id] = struct{}{}
	}

	moved := false
	for _, snap := range idx.TasksByState(dirs.Waiting) {
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile != snap.Filename {
			continue
		}
		if _, ok := quarantined[snap.Filename]; ok {
			continue
		}
		if _, ok := satisfiedSet[snap.Meta.ID]; !ok {
			continue
		}
		if idx.HasActiveOverlap(snap.Meta.Affects) {
			continue
		}
		dst := filepath.Join(tasksDir, dirs.Backlog, snap.Filename)
		if err := AtomicMove(snap.Path, dst); err != nil {
			ui.Warnf("warning: could not promote waiting task %s: %v\n", snap.Filename, err)
			continue
		}
		moved = true
	}
	return moved
}

func deleteTaskState(tasksDir, filename string) {
	runtimedata.DeleteRuntimeArtifacts(tasksDir, filename)
}

// CountPromotableWaitingTasks is a read-only variant of ReconcileReadyQueue.
// It returns the number of waiting tasks whose dependencies are satisfied and
// would be promoted, without actually moving any files. Tasks with invalid
// glob syntax are excluded (they would be quarantined, not promoted).
//
// When idx is nil, a temporary index is built internally.
func CountPromotableWaitingTasks(tasksDir string, idx *PollIndex) int {
	promotable := resolvePromotableTasks(tasksDir, idx)
	count := 0
	for _, task := range promotable {
		if task.globError == nil {
			count++
		}
	}
	return count
}
