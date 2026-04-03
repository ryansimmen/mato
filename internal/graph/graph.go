// Package graph builds a structured dependency graph from a PollIndex.
// It combines DAG analysis results with queue metadata (state, priority,
// failure counts) to produce user-facing output. The package performs no
// filesystem I/O beyond what the index already captured.
package graph

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mato/internal/dag"
	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/queue"
	"mato/internal/ui"
)

// NodeState classifies a task's current queue position. Values match
// the queue directory constants from queue.DirWaiting etc.
type NodeState string

const (
	StateWaiting     NodeState = NodeState(queue.DirWaiting)
	StateBacklog     NodeState = NodeState(queue.DirBacklog)
	StateInProgress  NodeState = NodeState(queue.DirInProgress)
	StateReadyReview NodeState = NodeState(queue.DirReadyReview)
	StateReadyMerge  NodeState = NodeState(queue.DirReadyMerge)
	StateCompleted   NodeState = NodeState(queue.DirCompleted)
	StateFailed      NodeState = NodeState(queue.DirFailed)
)

// GraphNode represents a single task in the dependency graph.
type GraphNode struct {
	Key           string        `json:"key"`
	ID            string        `json:"id"`
	Filename      string        `json:"filename"`
	Title         string        `json:"title,omitempty"`
	State         NodeState     `json:"state"`
	Priority      int           `json:"priority"`
	DependsOn     []string      `json:"depends_on,omitempty"`
	FailureCount  int           `json:"failure_count,omitempty"`
	BlockDetails  []BlockDetail `json:"block_details,omitempty"`
	IsCycleMember bool          `json:"is_cycle_member,omitempty"`
	HiddenDeps    []HiddenDep   `json:"hidden_deps,omitempty"`
}

// BlockDetail describes why a specific dependency blocks a waiting task.
type BlockDetail struct {
	DependencyID string `json:"dependency_id"`
	Reason       string `json:"reason"`
}

// HiddenDep represents a dependency whose target task is not present as a
// node in the graph.
type HiddenDep struct {
	DependencyID string `json:"dependency_id"`
	Status       string `json:"status"`
}

// Edge represents a dependency relationship between two in-graph nodes.
type Edge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Satisfied bool   `json:"satisfied"`
}

// ParseFailure records a task file that could not be parsed.
type ParseFailure struct {
	Filename string `json:"filename"`
	State    string `json:"state"`
	Error    string `json:"error"`
}

// DuplicateWarning records a waiting-directory file that shares its
// meta.ID with another waiting file.
type DuplicateWarning struct {
	Filename    string `json:"filename"`
	DuplicateOf string `json:"duplicate_of"`
	SharedID    string `json:"shared_id"`
}

// GraphData is the complete graph structure ready for rendering.
type GraphData struct {
	Nodes             []GraphNode        `json:"nodes"`
	Edges             []Edge             `json:"edges"`
	Cycles            [][]string         `json:"cycles,omitempty"`
	ParseFailures     []ParseFailure     `json:"parse_failures,omitempty"`
	DuplicateWarnings []DuplicateWarning `json:"duplicate_warnings,omitempty"`
}

// blockReasonString converts dag.BlockReason to its string representation.
func blockReasonString(r dag.BlockReason) string {
	switch r {
	case dag.BlockedByWaiting:
		return "waiting"
	case dag.BlockedByUnknown:
		return "unknown"
	case dag.BlockedByExternal:
		return "external"
	case dag.BlockedByAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// classifyRef determines the status of a dependency reference using the
// same sets as DiagnoseDependencies.
func classifyRef(ref string, safeCompleted, ambiguousIDs, allIDs map[string]struct{}) string {
	if _, ok := safeCompleted[ref]; ok {
		return "satisfied"
	}
	if _, ok := ambiguousIDs[ref]; ok {
		return "ambiguous"
	}
	if _, ok := allIDs[ref]; ok {
		return "external"
	}
	return "unknown"
}

// Build constructs the dependency graph from a PollIndex.
func Build(tasksDir string, idx *queue.PollIndex, showAll bool) GraphData {
	diag := queue.DiagnoseDependencies(tasksDir, idx)

	// Build safeCompleted and ambiguousIDs (same derivation as DiagnoseDependencies).
	completedIDs := idx.CompletedIDs()
	nonCompletedIDs := idx.NonCompletedIDs()
	safeCompleted := copySet(completedIDs)
	ambiguousIDs := make(map[string]struct{})
	for id := range nonCompletedIDs {
		if _, dup := safeCompleted[id]; dup {
			delete(safeCompleted, id)
			ambiguousIDs[id] = struct{}{}
		}
	}
	allIDs := idx.AllIDs()

	// State ordering for deterministic sort.
	stateOrder := make(map[string]int, len(queue.AllDirs))
	for i, dir := range queue.AllDirs {
		stateOrder[dir] = i
	}

	var data GraphData
	aliasMap := make(map[string][]string) // ref → []nodeKey

	// Track which waiting IDs are retained (for duplicate detection).
	retainedWaitingIDs := diag.RetainedFiles // id → filename

	// Track duplicate waiting files for DuplicateWarning emission.
	// seenWaitingIDs maps meta.ID → retained filename for waiting tasks.
	seenWaitingIDs := make(map[string]string)

	// Step 3: Iterate all dirs, build nodes.
	for _, dir := range queue.AllDirs {
		if !showAll && (dir == queue.DirCompleted || dir == queue.DirFailed) {
			continue
		}
		for _, snap := range idx.TasksByState(dir) {
			key := dir + "/" + snap.Filename
			title := frontmatter.ExtractTitle(snap.Filename, snap.Body)

			node := GraphNode{
				Key:          key,
				ID:           snap.Meta.ID,
				Filename:     snap.Filename,
				Title:        title,
				State:        NodeState(dir),
				Priority:     snap.Meta.Priority,
				DependsOn:    snap.Meta.DependsOn,
				FailureCount: snap.FailureCount,
			}

			data.Nodes = append(data.Nodes, node)

			// Step 4: Build alias map.
			stem := frontmatter.TaskFileStem(snap.Filename)

			if dir == queue.DirWaiting {
				// For waiting tasks, alias map entries point to the
				// retained file only when duplicates exist.
				if retainedFilename, ok := retainedWaitingIDs[snap.Meta.ID]; ok && retainedFilename == snap.Filename {
					// This is the retained file for this ID.
					aliasMap[snap.Meta.ID] = appendUnique(aliasMap[snap.Meta.ID], key)
					if stem != snap.Meta.ID {
						aliasMap[stem] = appendUnique(aliasMap[stem], key)
					}
					seenWaitingIDs[snap.Meta.ID] = snap.Filename
				} else if _, exists := seenWaitingIDs[snap.Meta.ID]; exists {
					// Duplicate waiting file — emit warning.
					data.DuplicateWarnings = append(data.DuplicateWarnings, DuplicateWarning{
						Filename:    snap.Filename,
						DuplicateOf: seenWaitingIDs[snap.Meta.ID],
						SharedID:    snap.Meta.ID,
					})
					// Do NOT add to aliasMap — only retained file is
					// aliased. The duplicate is a node but should not be
					// a target for dependency resolution.
				} else {
					// First time seeing this ID in waiting (retained).
					aliasMap[snap.Meta.ID] = appendUnique(aliasMap[snap.Meta.ID], key)
					if stem != snap.Meta.ID {
						aliasMap[stem] = appendUnique(aliasMap[stem], key)
					}
					seenWaitingIDs[snap.Meta.ID] = snap.Filename
				}
			} else {
				// Non-waiting: standard alias map population.
				aliasMap[snap.Meta.ID] = appendUnique(aliasMap[snap.Meta.ID], key)
				if stem != snap.Meta.ID {
					aliasMap[stem] = appendUnique(aliasMap[stem], key)
				}
			}
		}
	}

	// Step 5: For each node with depends_on, resolve edges and hidden deps.
	// Deduplicate refs so repeated depends_on entries produce a single
	// edge or hidden dependency per unique reference.
	for i := range data.Nodes {
		node := &data.Nodes[i]
		seenRefs := make(map[string]struct{})
		for _, ref := range node.DependsOn {
			if ref == "" {
				continue
			}
			if _, dup := seenRefs[ref]; dup {
				continue
			}
			seenRefs[ref] = struct{}{}
			status := classifyRef(ref, safeCompleted, ambiguousIDs, allIDs)
			satisfied := status == "satisfied"

			targets := aliasMap[ref]
			if len(targets) > 0 {
				for _, target := range targets {
					if target == node.Key {
						// Self-edge handled by cycles; still create the edge.
					}
					data.Edges = append(data.Edges, Edge{
						From:      target,
						To:        node.Key,
						Satisfied: satisfied,
					})
				}
			} else {
				node.HiddenDeps = append(node.HiddenDeps, HiddenDep{
					DependencyID: ref,
					Status:       status,
				})
			}
		}
	}

	// Step 6: Attach BlockDetails from diag.Analysis.Blocked for waiting nodes.
	for i := range data.Nodes {
		node := &data.Nodes[i]
		if node.State != StateWaiting {
			continue
		}
		if details, ok := diag.Analysis.Blocked[node.ID]; ok {
			// Only attach to retained files (those that participated in DAG analysis).
			if retainedFilename, retained := retainedWaitingIDs[node.ID]; retained && retainedFilename == node.Filename {
				for _, d := range details {
					node.BlockDetails = append(node.BlockDetails, BlockDetail{
						DependencyID: d.DependencyID,
						Reason:       blockReasonString(d.Reason),
					})
				}
			}
		}
	}

	// Step 7: Map cycle IDs to node keys via waitingIDToKey.
	waitingIDToKey := make(map[string]string)
	for id, filename := range retainedWaitingIDs {
		waitingIDToKey[id] = queue.DirWaiting + "/" + filename
	}

	cycleMemberKeys := make(map[string]struct{})
	for _, scc := range diag.Analysis.Cycles {
		var keys []string
		for _, id := range scc {
			if key, ok := waitingIDToKey[id]; ok {
				keys = append(keys, key)
				cycleMemberKeys[key] = struct{}{}
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			data.Cycles = append(data.Cycles, keys)
		}
	}

	// Set IsCycleMember on corresponding nodes.
	for i := range data.Nodes {
		if _, ok := cycleMemberKeys[data.Nodes[i].Key]; ok {
			data.Nodes[i].IsCycleMember = true
		}
	}

	// Collect parse failures.
	for _, pf := range idx.ParseFailures() {
		if !showAll && (pf.State == queue.DirCompleted || pf.State == queue.DirFailed) {
			continue
		}
		data.ParseFailures = append(data.ParseFailures, ParseFailure{
			Filename: pf.Filename,
			State:    pf.State,
			Error:    pf.Err.Error(),
		})
	}

	// Step 8: Sort everything deterministically.
	sort.Slice(data.Nodes, func(i, j int) bool {
		si := stateOrder[string(data.Nodes[i].State)]
		sj := stateOrder[string(data.Nodes[j].State)]
		if si != sj {
			return si < sj
		}
		if data.Nodes[i].Priority != data.Nodes[j].Priority {
			return data.Nodes[i].Priority < data.Nodes[j].Priority
		}
		return data.Nodes[i].Filename < data.Nodes[j].Filename
	})

	sort.Slice(data.Edges, func(i, j int) bool {
		if data.Edges[i].From != data.Edges[j].From {
			return data.Edges[i].From < data.Edges[j].From
		}
		return data.Edges[i].To < data.Edges[j].To
	})

	sort.Slice(data.Cycles, func(i, j int) bool {
		return data.Cycles[i][0] < data.Cycles[j][0]
	})

	// Sort HiddenDeps within each node.
	for i := range data.Nodes {
		if len(data.Nodes[i].HiddenDeps) > 1 {
			sort.Slice(data.Nodes[i].HiddenDeps, func(a, b int) bool {
				return data.Nodes[i].HiddenDeps[a].DependencyID < data.Nodes[i].HiddenDeps[b].DependencyID
			})
		}
	}

	return data
}

// Show writes the dependency graph to os.Stdout.
func Show(repoRoot, format string, showAll bool) error {
	return ShowTo(os.Stdout, repoRoot, format, showAll)
}

// ShowTo resolves the tasks directory, builds the dependency graph, and
// writes it to w in the requested format.
func ShowTo(w io.Writer, repoRoot, format string, showAll bool) error {
	if err := ui.ValidateFormat(format, []string{"text", "dot", "json"}); err != nil {
		return err
	}

	repoRoot, err := git.ResolveRepoRoot(repoRoot)
	if err != nil {
		return err
	}
	tasksDir := filepath.Join(repoRoot, dirs.Root)

	if err := ui.RequireTasksDir(tasksDir); err != nil {
		return err
	}

	idx := queue.BuildIndex(tasksDir)

	// Fail on directory-level read errors only; skip glob warnings.
	for _, bw := range idx.BuildWarnings() {
		if !isGlobWarning(bw) {
			return fmt.Errorf("incomplete index: %s: %v", bw.State, bw.Err)
		}
	}

	data := Build(tasksDir, idx, showAll)

	switch format {
	case "dot":
		RenderDOT(w, data)
		return nil
	case "json":
		return RenderJSON(w, data)
	default:
		RenderText(w, data)
		return nil
	}
}

// isGlobWarning returns true if the build warning is a glob/affects
// validation warning rather than a directory-level read error.
func isGlobWarning(bw queue.BuildWarning) bool {
	msg := bw.Err.Error()
	return strings.Contains(msg, "glob") || strings.Contains(msg, "affects")
}

func copySet(m map[string]struct{}) map[string]struct{} {
	c := make(map[string]struct{}, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func appendUnique(slice []string, val string) []string {
	for _, s := range slice {
		if s == val {
			return slice
		}
	}
	return append(slice, val)
}
