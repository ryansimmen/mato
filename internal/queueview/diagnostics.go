package queueview

import (
	"sort"

	"mato/internal/dag"
	"mato/internal/dirs"
)

// DependencyIssueKind classifies a dependency diagnostic issue.
type DependencyIssueKind string

const (
	DependencyAmbiguousID DependencyIssueKind = "ambiguous_id"
	DependencyDuplicateID DependencyIssueKind = "duplicate_id"
	DependencySelfCycle   DependencyIssueKind = "self_dependency"
	DependencyCycle       DependencyIssueKind = "cycle"
	DependencyUnknownID   DependencyIssueKind = "unknown_dependency"
)

// DependencyIssue describes a single dependency-related issue found during
// diagnostics. Issues are informational - the caller decides how to act on
// them (emit warnings, move tasks, etc.).
type DependencyIssue struct {
	Kind      DependencyIssueKind
	TaskID    string
	Filename  string
	DependsOn string // the problematic dependency reference
}

// DependencyDiagnostics is the result of DiagnoseDependencies. It wraps the
// underlying dag.Analysis so callers like ReconcileReadyQueue can act on
// DepsSatisfied and Cycles directly without a redundant dag.Analyze() call.
type DependencyDiagnostics struct {
	// Analysis is the underlying DAG result.
	Analysis dag.Analysis

	// Issues contains structured diagnostic issues sorted by
	// (Kind, TaskID, DependsOn).
	Issues []DependencyIssue

	// RetainedFiles maps each retained waiting task ID to its filename.
	// When duplicate waiting IDs exist, only the first file seen is
	// retained. Callers should use this map to filter
	// idx.TasksByState(dirs.Waiting) so duplicate files are not promoted
	// or cycle-failed.
	RetainedFiles map[string]string
}

// DiagnoseDependencies builds the inputs to dag.Analyze() from the PollIndex,
// runs the analysis, and produces structured diagnostic issues. It is a
// read-only function with no file I/O beyond what the index already captured.
func DiagnoseDependencies(tasksDir string, idx *PollIndex) DependencyDiagnostics {
	idx = ensureIndex(tasksDir, idx)

	completedIDs := idx.CompletedIDs()
	nonCompletedIDs := idx.NonCompletedIDs()
	knownIDs := idx.AllIDs()

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

	var issues []DependencyIssue

	sortedAmbiguous := sortedKeys(ambiguousIDs)
	for _, id := range sortedAmbiguous {
		issues = append(issues, DependencyIssue{
			Kind:   DependencyAmbiguousID,
			TaskID: id,
		})
	}

	waitingTasks := idx.TasksByState(dirs.Waiting)
	seenIDs := make(map[string]string, len(waitingTasks))
	var nodes []dag.Node
	nodeFilenames := make(map[string]string, len(waitingTasks))

	for _, snap := range waitingTasks {
		if first, exists := seenIDs[snap.Meta.ID]; exists {
			issues = append(issues, DependencyIssue{
				Kind:      DependencyDuplicateID,
				TaskID:    snap.Meta.ID,
				Filename:  snap.Filename,
				DependsOn: first,
			})
			continue
		}
		seenIDs[snap.Meta.ID] = snap.Filename
		nodeFilenames[snap.Meta.ID] = snap.Filename
		nodes = append(nodes, dag.Node{
			ID:        snap.Meta.ID,
			DependsOn: snap.Meta.DependsOn,
		})
	}

	analysis := dag.Analyze(nodes, safeCompleted, knownIDs, ambiguousIDs)

	for _, scc := range analysis.Cycles {
		if len(scc) == 1 {
			issues = append(issues, DependencyIssue{
				Kind:      DependencySelfCycle,
				TaskID:    scc[0],
				Filename:  nodeFilenames[scc[0]],
				DependsOn: scc[0],
			})
		} else {
			for _, id := range scc {
				issues = append(issues, DependencyIssue{
					Kind:     DependencyCycle,
					TaskID:   id,
					Filename: nodeFilenames[id],
				})
			}
		}
	}

	for taskID, details := range analysis.Blocked {
		for _, detail := range details {
			if detail.Reason == dag.BlockedByUnknown {
				issues = append(issues, DependencyIssue{
					Kind:      DependencyUnknownID,
					TaskID:    taskID,
					Filename:  nodeFilenames[taskID],
					DependsOn: detail.DependencyID,
				})
			}
		}
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		if issues[i].TaskID != issues[j].TaskID {
			return issues[i].TaskID < issues[j].TaskID
		}
		return issues[i].DependsOn < issues[j].DependsOn
	})

	return DependencyDiagnostics{
		Analysis:      analysis,
		Issues:        issues,
		RetainedFiles: nodeFilenames,
	}
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
