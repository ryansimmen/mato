package queue

import (
	"fmt"
	"os"
	"path/filepath"

	"mato/internal/frontmatter"
	"mato/internal/taskfile"
	"mato/internal/taskstate"
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

	waitingTasks := idx.TasksByState(DirWaiting)
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

	// Move unparseable waiting/backlog tasks to failed/ using index parse failures.
	for _, pf := range idx.WaitingParseFailures() {
		fmt.Fprintf(os.Stderr, "warning: moving unparseable waiting task %s to failed/: %v\n", pf.Filename, pf.Err)
		if err := taskfile.AppendTerminalFailureRecord(pf.Path, fmt.Sprintf("unparseable frontmatter: %v", pf.Err)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", pf.Filename, err)
		}
		failedPath := filepath.Join(tasksDir, DirFailed, pf.Filename)
		if moveErr := AtomicMove(pf.Path, failedPath); moveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", pf.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, pf.Filename)
			moved = true
		}
	}
	for _, pf := range idx.BacklogParseFailures() {
		fmt.Fprintf(os.Stderr, "warning: moving unparseable backlog task %s to failed/: %v\n", pf.Filename, pf.Err)
		if err := taskfile.AppendTerminalFailureRecord(pf.Path, fmt.Sprintf("unparseable frontmatter: %v", pf.Err)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", pf.Filename, err)
		}
		failedPath := filepath.Join(tasksDir, DirFailed, pf.Filename)
		if moveErr := AtomicMove(pf.Path, failedPath); moveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", pf.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, pf.Filename)
			moved = true
		}
	}

	// Move backlog tasks with invalid glob syntax to failed/.
	for _, snap := range idx.TasksByState(DirBacklog) {
		if snap.GlobError != nil {
			fmt.Fprintf(os.Stderr, "warning: moving backlog task %s with invalid glob to failed/: %v\n", snap.Filename, snap.GlobError)
			if appendErr := taskfile.AppendTerminalFailureRecord(snap.Path, fmt.Sprintf("invalid glob syntax: %v", snap.GlobError)); appendErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", snap.Filename, appendErr)
			}
			failedPath := filepath.Join(tasksDir, DirFailed, snap.Filename)
			if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
			} else {
				deleteTaskState(tasksDir, snap.Filename)
				moved = true
			}
		}
	}

	blockedBacklog := DependencyBlockedBacklogTasksDetailed(tasksDir, idx)
	if len(blockedBacklog) > 0 {
		demoted := 0
		for _, snap := range idx.TasksByState(DirBacklog) {
			blocks, blocked := blockedBacklog[snap.Filename]
			if !blocked {
				continue
			}
			waitingPath := filepath.Join(tasksDir, DirWaiting, snap.Filename)
			if err := AtomicMove(snap.Path, waitingPath); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move dependency-blocked backlog task %s back to waiting/: %v\n", snap.Filename, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "warning: moved dependency-blocked backlog task %s back to waiting/ (blocked by %s)\n", snap.Filename, FormatDependencyBlocks(blocks))
			moved = true
			demoted++
		}
		if demoted > 0 {
			idx = ensureIndex(tasksDir, nil)
		}
	}

	// Run DAG-based dependency analysis.
	diag := DiagnoseDependencies(tasksDir, idx)

	// Emit structured warnings from diagnostics issues.
	for _, issue := range diag.Issues {
		switch issue.Kind {
		case DependencyAmbiguousID:
			fmt.Fprintf(os.Stderr, "warning: task ID %q exists in both completed and non-completed directories; dependency on it will not be satisfied\n", issue.TaskID)
		case DependencyDuplicateID:
			fmt.Fprintf(os.Stderr, "warning: duplicate waiting task ID %q: %s and %s\n", issue.TaskID, issue.DependsOn, issue.Filename)
		case DependencySelfCycle:
			fmt.Fprintf(os.Stderr, "warning: task %s depends on itself\n", issue.TaskID)
		case DependencyCycle:
			fmt.Fprintf(os.Stderr, "warning: task %s is part of a circular dependency\n", issue.TaskID)
		case DependencyUnknownID:
			fmt.Fprintf(os.Stderr, "warning: waiting task %s depends on unknown task ID %q (not found in any queue directory)\n", issue.Filename, issue.DependsOn)
		}
	}

	// Move duplicate waiting files to failed/ with terminal-failure markers.
	// A file is a duplicate if its task ID appears in RetainedFiles but
	// its filename is not the retained copy. The first file (by filename
	// sort) is kept; every subsequent copy is failed.
	for _, snap := range idx.TasksByState(DirWaiting) {
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile == snap.Filename {
			continue // retained copy or unknown ID — skip
		}
		reason := fmt.Sprintf("duplicate waiting task ID %q (retained copy: %s)", snap.Meta.ID, retainedFile)
		fmt.Fprintf(os.Stderr, "warning: moving duplicate waiting task %s to failed/: %s\n", snap.Filename, reason)
		if err := taskfile.AppendTerminalFailureRecord(snap.Path, reason); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", snap.Filename, err)
		}
		failedPath := filepath.Join(tasksDir, DirFailed, snap.Filename)
		if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
		} else {
			deleteTaskState(tasksDir, snap.Filename)
			moved = true
		}
	}

	// Move cycle members to failed/ using the cycle-to-failed sequence.
	// Build a lookup from retained task ID to waiting snapshot for cycle processing.
	// Only retained files (from deduped diagnostics) are eligible; duplicates are skipped.
	waitingByID := make(map[string]*TaskSnapshot)
	for _, snap := range idx.TasksByState(DirWaiting) {
		retainedFile, ok := diag.RetainedFiles[snap.Meta.ID]
		if !ok || retainedFile != snap.Filename {
			continue
		}
		waitingByID[snap.Meta.ID] = snap
	}

	for _, scc := range diag.Analysis.Cycles {
		for _, id := range scc {
			snap, ok := waitingByID[id]
			if !ok {
				continue
			}
			// Step 1: Check for existing cycle-failure marker (idempotency).
			data, err := os.ReadFile(snap.Path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not read %s for cycle-failure check: %v\n", snap.Filename, err)
				continue
			}
			if !taskfile.ContainsCycleFailure(data) {
				// Step 2: Append cycle-failure record before moving.
				if err := taskfile.AppendCycleFailureRecord(snap.Path); err != nil {
					fmt.Fprintf(os.Stderr, "warning: could not append cycle-failure record to %s: %v\n", snap.Filename, err)
					continue
				}
			}
			// Step 3: Move to failed/.
			failedPath := filepath.Join(tasksDir, DirFailed, snap.Filename)
			if err := AtomicMove(snap.Path, failedPath); err != nil {
				// Step 4: If move fails, warn and continue. The idempotency
				// check in step 1 prevents duplicate records on the next pass.
				fmt.Fprintf(os.Stderr, "warning: could not move cycle member %s to failed/: %v\n", snap.Filename, err)
			} else {
				deleteTaskState(tasksDir, snap.Filename)
				moved = true
			}
		}
	}

	// Promote deps-satisfied tasks to backlog/.
	satisfiedSet := make(map[string]struct{}, len(diag.Analysis.DepsSatisfied))
	for _, id := range diag.Analysis.DepsSatisfied {
		satisfiedSet[id] = struct{}{}
	}

	for _, snap := range idx.TasksByState(DirWaiting) {
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
		// Quarantine tasks with invalid glob syntax instead of promoting.
		if snap.GlobError != nil {
			fmt.Fprintf(os.Stderr, "warning: moving waiting task %s with invalid glob to failed/: %v\n", snap.Filename, snap.GlobError)
			if appendErr := taskfile.AppendTerminalFailureRecord(snap.Path, fmt.Sprintf("invalid glob syntax: %v", snap.GlobError)); appendErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not append terminal-failure to %s: %v\n", snap.Filename, appendErr)
			}
			failedPath := filepath.Join(tasksDir, DirFailed, snap.Filename)
			if moveErr := AtomicMove(snap.Path, failedPath); moveErr != nil {
				fmt.Fprintf(os.Stderr, "warning: could not move %s to failed/: %v\n", snap.Filename, moveErr)
			} else {
				deleteTaskState(tasksDir, snap.Filename)
				moved = true
			}
			continue
		}
		dst := filepath.Join(tasksDir, DirBacklog, snap.Filename)
		if err := AtomicMove(snap.Path, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not promote waiting task %s: %v\n", snap.Filename, err)
			continue
		}
		moved = true
	}

	return moved
}

func deleteTaskState(tasksDir, filename string) {
	if err := taskstate.Delete(tasksDir, filename); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not delete taskstate for %s: %v\n", filename, err)
	}
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
