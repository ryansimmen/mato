// Package graph builds a structured dependency graph from a PollIndex.
// It combines DAG analysis results with queue metadata (state, priority,
// failure counts) to produce user-facing output. The package performs no
// filesystem I/O beyond what the index already captured.
package graph

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/queueview"
	"github.com/ryansimmen/mato/internal/ui"
)

// NodeState classifies a task's current queue position. Values match
// the queue directory constants from dirs.Waiting etc.
type NodeState string

const (
	StateWaiting     NodeState = NodeState(dirs.Waiting)
	StateBacklog     NodeState = NodeState(dirs.Backlog)
	StateInProgress  NodeState = NodeState(dirs.InProgress)
	StateReadyReview NodeState = NodeState(dirs.ReadyReview)
	StateReadyMerge  NodeState = NodeState(dirs.ReadyMerge)
	StateCompleted   NodeState = NodeState(dirs.Completed)
	StateFailed      NodeState = NodeState(dirs.Failed)
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
func Build(tasksDir string, idx *queueview.PollIndex, showAll bool) GraphData {
	diag := queueview.DiagnoseDependencies(tasksDir, idx)

	// Derive safeCompleted and ambiguousIDs (same logic as DiagnoseDependencies).
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
	stateOrder := make(map[string]int, len(dirs.All))
	for i, dir := range dirs.All {
		stateOrder[dir] = i
	}

	var data GraphData
	aliasMap := buildNodes(&data, idx, &diag, showAll)
	resolveEdges(&data, aliasMap, safeCompleted, ambiguousIDs, allIDs)
	attachBlockDetails(&data, &diag)
	annotateCycles(&data, &diag)
	appendParseFailures(&data, idx, showAll)
	sortGraphData(&data, stateOrder)

	return data
}

// buildNodes iterates queue directories, constructs GraphNode entries, populates
// the alias map used for edge resolution, and emits duplicate warnings for
// waiting tasks that share a meta.ID.
func buildNodes(data *GraphData, idx *queueview.PollIndex, diag *queueview.DependencyDiagnostics, showAll bool) map[string][]string {
	aliasMap := make(map[string][]string) // ref → []nodeKey
	retainedWaitingIDs := diag.RetainedFiles
	seenWaitingIDs := make(map[string]string) // meta.ID → retained filename

	for _, dir := range dirs.All {
		if !showAll && (dir == dirs.Completed || dir == dirs.Failed) {
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

			stem := frontmatter.TaskFileStem(snap.Filename)

			if dir == dirs.Waiting {
				// For waiting tasks, only use meta.ID as the alias key —
				// NOT the filename stem. This matches the runtime DAG
				// which resolves waiting-to-waiting dependencies by
				// meta.ID only (see dag.Analyze).
				if retainedFilename, ok := retainedWaitingIDs[snap.Meta.ID]; ok && retainedFilename == snap.Filename {
					aliasMap[snap.Meta.ID] = appendUnique(aliasMap[snap.Meta.ID], key)
					seenWaitingIDs[snap.Meta.ID] = snap.Filename
				} else if _, exists := seenWaitingIDs[snap.Meta.ID]; exists {
					// Duplicate waiting file — emit warning.
					data.DuplicateWarnings = append(data.DuplicateWarnings, DuplicateWarning{
						Filename:    snap.Filename,
						DuplicateOf: seenWaitingIDs[snap.Meta.ID],
						SharedID:    snap.Meta.ID,
					})
				} else {
					// First time seeing this ID in waiting (retained).
					aliasMap[snap.Meta.ID] = appendUnique(aliasMap[snap.Meta.ID], key)
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

	return aliasMap
}

// resolveEdges deduplicates depends_on refs for each node and creates Edge
// entries for in-graph targets or HiddenDep entries for out-of-graph refs.
func resolveEdges(data *GraphData, aliasMap map[string][]string, safeCompleted, ambiguousIDs, allIDs map[string]struct{}) {
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
				// Self-edges are still emitted so cycle rendering remains intact.
				for _, target := range targets {
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
}

// attachBlockDetails populates BlockDetail entries on waiting nodes from
// the dependency diagnosis results.
func attachBlockDetails(data *GraphData, diag *queueview.DependencyDiagnostics) {
	retainedWaitingIDs := diag.RetainedFiles
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
						Reason:       d.Reason.String(),
					})
				}
			}
		}
	}
}

// annotateCycles maps cycle member IDs to node keys, records cycle groups,
// and sets IsCycleMember on the corresponding nodes.
func annotateCycles(data *GraphData, diag *queueview.DependencyDiagnostics) {
	retainedWaitingIDs := diag.RetainedFiles
	waitingIDToKey := make(map[string]string)
	for id, filename := range retainedWaitingIDs {
		waitingIDToKey[id] = dirs.Waiting + "/" + filename
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

	for i := range data.Nodes {
		if _, ok := cycleMemberKeys[data.Nodes[i].Key]; ok {
			data.Nodes[i].IsCycleMember = true
		}
	}
}

// appendParseFailures collects task files that could not be parsed from
// the index.
func appendParseFailures(data *GraphData, idx *queueview.PollIndex, showAll bool) {
	for _, pf := range idx.ParseFailures() {
		if !showAll && (pf.State == dirs.Completed || pf.State == dirs.Failed) {
			continue
		}
		data.ParseFailures = append(data.ParseFailures, ParseFailure{
			Filename: pf.Filename,
			State:    pf.State,
			Error:    pf.Err.Error(),
		})
	}
}

// sortGraphData applies deterministic ordering to nodes, edges, cycles,
// and hidden deps.
func sortGraphData(data *GraphData, stateOrder map[string]int) {
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

	for i := range data.Nodes {
		if len(data.Nodes[i].HiddenDeps) > 1 {
			sort.Slice(data.Nodes[i].HiddenDeps, func(a, b int) bool {
				return data.Nodes[i].HiddenDeps[a].DependencyID < data.Nodes[i].HiddenDeps[b].DependencyID
			})
		}
	}
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

	idx := queueview.BuildIndex(tasksDir)

	// Fail on directory-level read errors only; skip glob warnings.
	for _, bw := range idx.BuildWarnings() {
		if !isGlobWarning(bw) {
			return fmt.Errorf("incomplete index: %s: %w", bw.State, bw.Err)
		}
	}

	data := Build(tasksDir, idx, showAll)

	switch format {
	case "dot":
		if err := RenderDOT(w, data); err != nil {
			return fmt.Errorf("render dot graph: %w", err)
		}
		return nil
	case "json":
		return RenderJSON(w, data)
	default:
		if err := RenderText(w, data); err != nil {
			return fmt.Errorf("render text graph: %w", err)
		}
		return nil
	}
}

// isGlobWarning returns true if the build warning is a glob/affects
// validation warning rather than a directory-level read error.
func isGlobWarning(bw queueview.BuildWarning) bool {
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
