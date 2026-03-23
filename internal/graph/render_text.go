package graph

import (
	"fmt"
	"io"
	"sort"
)

// RenderText writes a human-readable indented view of the graph grouped
// by state. Tasks with dependencies show inline dependency trees with
// status annotations.
func RenderText(w io.Writer, data GraphData) {
	nodeCount := len(data.Nodes)
	edgeCount := len(data.Edges)
	cycleCount := len(data.Cycles)

	fmt.Fprintf(w, "mato graph — %d tasks, %d edges, %d cycles\n", nodeCount, edgeCount, cycleCount)

	// Build lookup maps.
	nodeByKey := make(map[string]*GraphNode, len(data.Nodes))
	for i := range data.Nodes {
		nodeByKey[data.Nodes[i].Key] = &data.Nodes[i]
	}

	// Build a set of nodes that are targets of edges (dependents exist).
	dependentTargets := make(map[string]struct{})
	for _, e := range data.Edges {
		dependentTargets[e.To] = struct{}{}
	}

	// Build edge lookup: to-key → list of from-keys.
	edgesByTo := make(map[string][]Edge)
	for _, e := range data.Edges {
		edgesByTo[e.To] = append(edgesByTo[e.To], e)
	}

	// Build cycle membership: key → set of SCC indices.
	cycleSCCs := make(map[string][]int)
	for i, scc := range data.Cycles {
		for _, key := range scc {
			cycleSCCs[key] = append(cycleSCCs[key], i)
		}
	}

	// Group nodes by state, preserving the sorted order from Build.
	type stateGroup struct {
		state string
		nodes []*GraphNode
	}
	var groups []stateGroup
	groupIdx := make(map[string]int)
	for i := range data.Nodes {
		n := &data.Nodes[i]
		s := string(n.State)
		if idx, ok := groupIdx[s]; ok {
			groups[idx].nodes = append(groups[idx].nodes, n)
		} else {
			groupIdx[s] = len(groups)
			groups = append(groups, stateGroup{state: s, nodes: []*GraphNode{n}})
		}
	}

	for _, g := range groups {
		fmt.Fprintf(w, "\n%s/\n", g.state)
		for _, node := range g.nodes {
			renderTextNode(w, node, nodeByKey, edgesByTo, cycleSCCs)
		}
	}

	// DuplicateWarnings.
	for _, dw := range data.DuplicateWarnings {
		fmt.Fprintf(w, "\nwarning: %s is a duplicate of %s (shared ID: %s)\n", dw.Filename, dw.DuplicateOf, dw.SharedID)
	}

	// ParseFailures.
	if len(data.ParseFailures) > 0 {
		fmt.Fprintln(w)
		for _, pf := range data.ParseFailures {
			fmt.Fprintf(w, "warning: failed to parse %s/%s: %s\n", pf.State, pf.Filename, pf.Error)
		}
	}
}

// renderTextNode renders a single primary node with its dependency tree.
func renderTextNode(w io.Writer, node *GraphNode, nodeByKey map[string]*GraphNode, edgesByTo map[string][]Edge, cycleSCCs map[string][]int) {
	// Build annotation.
	annotation := fmt.Sprintf("priority: %d", node.Priority)
	if len(node.BlockDetails) > 0 {
		annotation += ", blocked"
	}
	if node.IsCycleMember {
		annotation += ", cycle ⚠"
	}

	fmt.Fprintf(w, "  %s (%s)\n", node.ID, annotation)

	// Collect dependency entries: edges + hidden deps.
	type depEntry struct {
		label string
		sort  string
	}
	var deps []depEntry

	// Edges pointing to this node (these are the node's dependencies).
	edges := edgesByTo[node.Key]
	sort.Slice(edges, func(i, j int) bool {
		return edges[i].From < edges[j].From
	})

	for _, e := range edges {
		fromNode := nodeByKey[e.From]
		if fromNode == nil {
			continue
		}
		// Check if this is a self-dependency.
		if e.From == node.Key {
			deps = append(deps, depEntry{
				label: fmt.Sprintf("%s (self-dependency)", fromNode.ID),
				sort:  fromNode.ID,
			})
			continue
		}
		deps = append(deps, depEntry{
			label: fmt.Sprintf("%s (%s)", fromNode.ID, depStateAnnotation(fromNode, e.Satisfied, cycleSCCs)),
			sort:  fromNode.ID,
		})
	}

	// Hidden deps.
	for _, hd := range node.HiddenDeps {
		deps = append(deps, depEntry{
			label: fmt.Sprintf("%s (%s)", hd.DependencyID, hiddenDepAnnotation(hd.Status)),
			sort:  hd.DependencyID,
		})
	}

	// Render the dependency tree.
	for i, d := range deps {
		isLast := i == len(deps)-1
		prefix := "├── "
		if isLast {
			prefix = "└── "
		}
		fmt.Fprintf(w, "    %s%s\n", prefix, d.label)
	}
}

// depStateAnnotation returns a short annotation for a dependency node
// used in the dependency tree under a primary node.
func depStateAnnotation(node *GraphNode, satisfied bool, cycleSCCs map[string][]int) string {
	state := string(node.State)
	if satisfied {
		return state + " ✓"
	}
	if node.IsCycleMember {
		return state + " ⚠"
	}
	switch state {
	case "in-progress":
		return state + " ⟳"
	case "completed":
		return "completed ✓"
	default:
		if len(node.BlockDetails) > 0 {
			return state + ", blocked"
		}
		return state
	}
}

// hiddenDepAnnotation returns an annotation string for a hidden dependency
// based on its status.
func hiddenDepAnnotation(status string) string {
	switch status {
	case "satisfied":
		return "completed ✓"
	case "external":
		return "external ✗"
	case "ambiguous":
		return "ambiguous ⚠"
	default:
		return "unknown ?"
	}
}
