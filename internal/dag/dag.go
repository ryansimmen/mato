// Package dag provides deterministic dependency-graph analysis for waiting
// tasks. It accepts a snapshot of waiting tasks plus supporting state from the
// caller and produces a classification of each task as deps-satisfied, blocked,
// or part of a cycle. The package has no filesystem I/O; callers in
// internal/queue and internal/status translate filesystem state into graph
// inputs.
package dag

import (
	"container/heap"
	"sort"
)

// stringHeap implements heap.Interface for a min-heap of strings,
// used by Kahn's algorithm to maintain deterministic processing order.
type stringHeap []string

func (h stringHeap) Len() int           { return len(h) }
func (h stringHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h stringHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *stringHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *stringHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// Node represents a waiting task in the dependency graph.
type Node struct {
	ID        string
	DependsOn []string
}

// BlockReason classifies why a dependency prevents promotion.
type BlockReason int

const (
	BlockedByWaiting   BlockReason = iota // dependency is itself in waiting/
	BlockedByUnknown                      // dependency ID not found anywhere
	BlockedByExternal                     // dependency exists in a non-completed, non-waiting state (e.g. failed, in-progress)
	BlockedByAmbiguous                    // dependency ID exists in both completed/ and a non-completed directory
)

// String returns the stable user-facing label for a blocking reason.
func (r BlockReason) String() string {
	switch r {
	case BlockedByWaiting:
		return "waiting"
	case BlockedByUnknown:
		return "unknown"
	case BlockedByExternal:
		return "external"
	case BlockedByAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// BlockDetail describes a single blocking dependency.
type BlockDetail struct {
	DependencyID string
	Reason       BlockReason
}

// Analysis is the result of Analyze.
type Analysis struct {
	// DepsSatisfied lists task IDs whose dependencies are all in completedIDs.
	// This does NOT mean the task is promotable — the caller must still verify
	// there is no active-affects overlap (!idx.HasActiveOverlap()) before
	// promoting.
	DepsSatisfied []string

	// Blocked maps a task ID to the specific dependencies preventing promotion
	// and the reason each one blocks. A task that is a cycle member (appears in
	// Cycles) does NOT appear in Blocked — cycle members are handled separately
	// via cycle-to-failed. Tasks downstream of a cycle DO appear in Blocked
	// with BlockedByWaiting referencing the cycle member.
	Blocked map[string][]BlockDetail

	// Cycles contains the strongly connected components (size > 1, or size 1
	// with a self-edge) found in the waiting subgraph.
	Cycles [][]string
}

// Analyze determines which waiting tasks have all dependencies satisfied.
//
// completedIDs should be the caller's safeCompleted set (ambiguous IDs already
// removed). knownIDs is the full set of task IDs across all directories —
// needed to distinguish BlockedByUnknown (ID not found anywhere) from
// BlockedByExternal (ID exists in a non-completed, non-waiting state like
// failed/ or in-progress/). ambiguousIDs is the set of IDs that appear in both
// completed/ and a non-completed directory — these are excluded from
// completedIDs by the caller, and Analyze tags them as BlockedByAmbiguous
// rather than BlockedByExternal so the blocking reason is self-documenting.
func Analyze(waiting []Node, completedIDs, knownIDs, ambiguousIDs map[string]struct{}) Analysis {
	result := Analysis{
		Blocked: make(map[string][]BlockDetail),
	}

	if len(waiting) == 0 {
		return result
	}

	// Build waiting ID set for graph edge detection.
	waitingSet := make(map[string]struct{}, len(waiting))
	for _, n := range waiting {
		waitingSet[n.ID] = struct{}{}
	}

	// Deduplicate each node's DependsOn slice to avoid inflated in-degree
	// counts and duplicate BlockDetail entries.
	for i := range waiting {
		waiting[i].DependsOn = dedup(waiting[i].DependsOn)
	}

	adj, depEdges, inDeg := buildGraph(waiting, waitingSet)
	kahnResolved := kahnResolve(waiting, adj, inDeg)
	classifyResolved(waiting, kahnResolved, completedIDs, waitingSet, knownIDs, ambiguousIDs, &result)
	findCyclesAndBlocked(waiting, kahnResolved, depEdges, completedIDs, waitingSet, knownIDs, ambiguousIDs, &result)
	sortAnalysis(&result)

	return result
}

// buildGraph constructs the adjacency list, dependency edges, and in-degree
// map used by Kahn's algorithm. Only edges between waiting tasks are included.
func buildGraph(waiting []Node, waitingSet map[string]struct{}) (adj, depEdges map[string][]string, inDeg map[string]int) {
	adj = make(map[string][]string, len(waiting))      // from -> [to] (dependency direction: "to" depends on "from")
	inDeg = make(map[string]int, len(waiting))          // number of waiting-task dependencies
	depEdges = make(map[string][]string, len(waiting))  // task -> waiting deps (for SCC)

	for _, n := range waiting {
		if _, exists := inDeg[n.ID]; !exists {
			inDeg[n.ID] = 0
		}
		for _, dep := range n.DependsOn {
			if dep == "" {
				continue
			}
			if _, isWaiting := waitingSet[dep]; isWaiting {
				adj[dep] = append(adj[dep], n.ID)
				depEdges[n.ID] = append(depEdges[n.ID], dep)
				inDeg[n.ID]++
			}
		}
	}
	return adj, depEdges, inDeg
}

// kahnResolve runs Kahn's algorithm over the waiting-task graph and returns
// the set of node IDs that are not blocked by any waiting-task dependency edge.
func kahnResolve(waiting []Node, adj map[string][]string, inDeg map[string]int) map[string]struct{} {
	h := &stringHeap{}
	for _, n := range waiting {
		if inDeg[n.ID] == 0 {
			*h = append(*h, n.ID)
		}
	}
	heap.Init(h)

	resolved := make(map[string]struct{}, len(waiting))
	for h.Len() > 0 {
		id := heap.Pop(h).(string)
		resolved[id] = struct{}{}

		neighbors := adj[id]
		sorted := make([]string, len(neighbors))
		copy(sorted, neighbors)
		sort.Strings(sorted)
		for _, next := range sorted {
			inDeg[next]--
			if inDeg[next] == 0 {
				heap.Push(h, next)
			}
		}
	}
	return resolved
}

// classifyResolved checks each Kahn-resolved node's full dependency list
// against completedIDs and classifies it as deps-satisfied or blocked.
func classifyResolved(waiting []Node, kahnResolved map[string]struct{}, completedIDs, waitingSet, knownIDs, ambiguousIDs map[string]struct{}, result *Analysis) {
	for _, n := range waiting {
		if _, resolved := kahnResolved[n.ID]; !resolved {
			continue
		}
		satisfied := true
		var details []BlockDetail
		for _, dep := range n.DependsOn {
			if dep == "" {
				continue
			}
			if _, ok := completedIDs[dep]; ok {
				continue
			}
			satisfied = false
			details = append(details, classifyBlock(dep, waitingSet, knownIDs, ambiguousIDs))
		}
		if satisfied {
			result.DepsSatisfied = append(result.DepsSatisfied, n.ID)
		} else {
			result.Blocked[n.ID] = details
		}
	}
}

// findCyclesAndBlocked detects strongly connected components in the residual
// graph (nodes not resolved by Kahn) and classifies downstream nodes as blocked.
func findCyclesAndBlocked(waiting []Node, kahnResolved map[string]struct{}, depEdges map[string][]string, completedIDs, waitingSet, knownIDs, ambiguousIDs map[string]struct{}, result *Analysis) {
	residual := make([]string, 0)
	for _, n := range waiting {
		if _, resolved := kahnResolved[n.ID]; !resolved {
			residual = append(residual, n.ID)
		}
	}

	if len(residual) == 0 {
		return
	}

	// Build residual adjacency for Tarjan's.
	residualAdj := make(map[string][]string, len(residual))
	residualSet := make(map[string]struct{}, len(residual))
	for _, id := range residual {
		residualSet[id] = struct{}{}
	}
	for _, id := range residual {
		for _, dep := range depEdges[id] {
			if _, ok := residualSet[dep]; ok {
				residualAdj[id] = append(residualAdj[id], dep)
			}
		}
		sort.Strings(residualAdj[id])
	}

	// Determine self-edges for SCC classification.
	selfEdge := make(map[string]bool)
	for _, n := range waiting {
		for _, dep := range n.DependsOn {
			if dep == n.ID {
				selfEdge[n.ID] = true
			}
		}
	}

	sccs := tarjan(residual, residualAdj)

	cycleMembers := make(map[string]struct{})
	for _, scc := range sccs {
		isCycle := len(scc) > 1 || (len(scc) == 1 && selfEdge[scc[0]])
		if isCycle {
			sort.Strings(scc)
			result.Cycles = append(result.Cycles, scc)
			for _, id := range scc {
				cycleMembers[id] = struct{}{}
			}
		}
	}

	// Downstream of cycle: blocked by waiting.
	for _, id := range residual {
		if _, isCycleMember := cycleMembers[id]; isCycleMember {
			continue
		}
		var details []BlockDetail
		for _, dep := range depEdges[id] {
			if _, ok := residualSet[dep]; ok {
				details = append(details, BlockDetail{
					DependencyID: dep,
					Reason:       BlockedByWaiting,
				})
			}
		}
		// Also check non-waiting deps.
		nodeIdx := -1
		for i, n := range waiting {
			if n.ID == id {
				nodeIdx = i
				break
			}
		}
		if nodeIdx >= 0 {
			for _, dep := range waiting[nodeIdx].DependsOn {
				if dep == "" {
					continue
				}
				if _, isWaiting := waitingSet[dep]; isWaiting {
					continue
				}
				if _, ok := completedIDs[dep]; ok {
					continue
				}
				details = append(details, classifyBlock(dep, waitingSet, knownIDs, ambiguousIDs))
			}
		}
		if len(details) > 0 {
			sortBlockDetails(details)
			result.Blocked[id] = details
		}
	}
}

// sortAnalysis sorts all output fields for deterministic results.
func sortAnalysis(result *Analysis) {
	sort.Strings(result.DepsSatisfied)

	sort.Slice(result.Cycles, func(i, j int) bool {
		return result.Cycles[i][0] < result.Cycles[j][0]
	})

	for id, details := range result.Blocked {
		sortBlockDetails(details)
		result.Blocked[id] = details
	}
}

// classifyBlock determines the BlockReason for a dependency that is not
// satisfied by completedIDs and is not a waiting-task graph edge.
//
// ambiguousIDs is checked before waitingSet because an ID present in both
// completed/ and another directory is unsafe to treat as satisfied regardless
// of whether it also exists in waiting/.
func classifyBlock(dep string, waitingSet, knownIDs, ambiguousIDs map[string]struct{}) BlockDetail {
	if _, isAmbiguous := ambiguousIDs[dep]; isAmbiguous {
		return BlockDetail{DependencyID: dep, Reason: BlockedByAmbiguous}
	}
	if _, isWaiting := waitingSet[dep]; isWaiting {
		return BlockDetail{DependencyID: dep, Reason: BlockedByWaiting}
	}
	if _, known := knownIDs[dep]; known {
		return BlockDetail{DependencyID: dep, Reason: BlockedByExternal}
	}
	return BlockDetail{DependencyID: dep, Reason: BlockedByUnknown}
}

func sortBlockDetails(details []BlockDetail) {
	sort.Slice(details, func(i, j int) bool {
		return details[i].DependencyID < details[j].DependencyID
	})
}

// dedup returns a new slice with duplicate and empty strings removed,
// preserving the original order of first occurrences.
func dedup(ss []string) []string {
	if len(ss) <= 1 {
		return ss
	}
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// tarjan implements Tarjan's strongly connected components algorithm.
// It returns SCCs in reverse topological order. Each SCC is a slice of
// node IDs. The nodes slice must be sorted for deterministic output.
func tarjan(nodes []string, adj map[string][]string) [][]string {
	sort.Strings(nodes)

	index := 0
	nodeIndex := make(map[string]int, len(nodes))
	nodeLowlink := make(map[string]int, len(nodes))
	onStack := make(map[string]bool, len(nodes))
	visited := make(map[string]bool, len(nodes))
	var stack []string
	var sccs [][]string

	var strongconnect func(v string)
	strongconnect = func(v string) {
		nodeIndex[v] = index
		nodeLowlink[v] = index
		index++
		visited[v] = true
		stack = append(stack, v)
		onStack[v] = true

		for _, w := range adj[v] {
			if !visited[w] {
				strongconnect(w)
				if nodeLowlink[w] < nodeLowlink[v] {
					nodeLowlink[v] = nodeLowlink[w]
				}
			} else if onStack[w] {
				if nodeIndex[w] < nodeLowlink[v] {
					nodeLowlink[v] = nodeIndex[w]
				}
			}
		}

		if nodeLowlink[v] == nodeIndex[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}

	for _, v := range nodes {
		if !visited[v] {
			strongconnect(v)
		}
	}

	return sccs
}
