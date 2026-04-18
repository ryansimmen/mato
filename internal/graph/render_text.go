package graph

import (
	"fmt"
	"io"
	"sort"

	"github.com/ryansimmen/mato/internal/ui"
)

var colors = ui.NewColorSet()

type textRenderWriter struct {
	w   io.Writer
	err error
}

func (tw *textRenderWriter) printf(format string, args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintf(tw.w, format, args...)
}

func (tw *textRenderWriter) println(args ...any) {
	if tw.err != nil {
		return
	}
	_, tw.err = fmt.Fprintln(tw.w, args...)
}

// RenderText writes a human-readable indented view of the graph grouped
// by state. Tasks with dependencies show inline dependency trees with
// status annotations. It returns any write error from w.
func RenderText(w io.Writer, data GraphData) error {
	tw := textRenderWriter{w: w}

	nodeCount := len(data.Nodes)
	edgeCount := len(data.Edges)
	cycleCount := len(data.Cycles)

	tw.printf("%s — %d tasks, %d edges, %d cycles\n", colors.Bold("mato graph"), nodeCount, edgeCount, cycleCount)

	// Build lookup maps.
	nodeByKey := make(map[string]*GraphNode, len(data.Nodes))
	for i := range data.Nodes {
		nodeByKey[data.Nodes[i].Key] = &data.Nodes[i]
	}

	// Build edge lookup: to-key → list of from-keys.
	edgesByTo := make(map[string][]Edge)
	for _, e := range data.Edges {
		edgesByTo[e.To] = append(edgesByTo[e.To], e)
	}

	// Build cycle membership: key → set of SCC indices.
	// (Used only by the DOT renderer; text renderer uses IsCycleMember.)

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
		tw.printf("\n%s\n", colors.Bold(g.state+"/"))
		for _, node := range g.nodes {
			renderTextNode(&tw, node, nodeByKey, edgesByTo)
		}
	}

	// DuplicateWarnings.
	for _, dw := range data.DuplicateWarnings {
		tw.printf("\nwarning: %s is a duplicate of %s (shared ID: %s)\n", dw.Filename, dw.DuplicateOf, dw.SharedID)
	}

	// ParseFailures.
	if len(data.ParseFailures) > 0 {
		tw.println()
		for _, pf := range data.ParseFailures {
			tw.printf("warning: failed to parse %s/%s: %s\n", pf.State, pf.Filename, pf.Error)
		}
	}

	return tw.err
}

// renderTextNode renders a single primary node with its recursive
// dependency tree.
func renderTextNode(w *textRenderWriter, node *GraphNode, nodeByKey map[string]*GraphNode, edgesByTo map[string][]Edge) {
	// Build annotation.
	annotation := fmt.Sprintf("priority: %d", node.Priority)
	if len(node.BlockDetails) > 0 {
		annotation += ", " + colors.Yellow("blocked")
	}
	if node.IsCycleMember {
		annotation += ", " + colors.Yellow("cycle ⚠")
	}

	w.printf("  %s (%s)\n", node.ID, annotation)

	// Render the recursive dependency tree starting from this node.
	visited := map[string]struct{}{node.Key: {}}
	renderDepTree(w, node.Key, "    ", nodeByKey, edgesByTo, visited)
}

// renderDepTree recursively renders the dependency tree for a node.
// prefix is the indentation string for the current nesting level.
// visited prevents infinite recursion in the presence of cycles.
func renderDepTree(w *textRenderWriter, nodeKey, prefix string, nodeByKey map[string]*GraphNode, edgesByTo map[string][]Edge, visited map[string]struct{}) {
	node := nodeByKey[nodeKey]
	if node == nil {
		return
	}

	// Collect dependency entries: edges + hidden deps.
	type depEntry struct {
		label   string
		fromKey string // empty for hidden deps
	}
	var deps []depEntry

	// Edges pointing to this node (these are the node's dependencies).
	edges := edgesByTo[nodeKey]
	sorted := make([]Edge, len(edges))
	copy(sorted, edges)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].From < sorted[j].From
	})

	for _, e := range sorted {
		fromNode := nodeByKey[e.From]
		if fromNode == nil {
			continue
		}
		if e.From == nodeKey {
			deps = append(deps, depEntry{
				label:   fmt.Sprintf("%s (self-dependency)", fromNode.ID),
				fromKey: "",
			})
			continue
		}
		ann := depStateAnnotation(fromNode, e.Satisfied)
		deps = append(deps, depEntry{
			label:   fmt.Sprintf("%s (%s)", fromNode.ID, ann),
			fromKey: e.From,
		})
	}

	// Hidden deps.
	for _, hd := range node.HiddenDeps {
		deps = append(deps, depEntry{
			label:   fmt.Sprintf("%s (%s)", hd.DependencyID, hiddenDepAnnotation(hd.Status)),
			fromKey: "",
		})
	}

	for i, d := range deps {
		isLast := i == len(deps)-1
		connector := "├── "
		childPrefix := prefix + "│   "
		if isLast {
			connector = "└── "
			childPrefix = prefix + "    "
		}
		w.printf("%s%s%s\n", prefix, connector, d.label)

		// Recurse into the dependency's own dependencies if it has an
		// in-graph node and hasn't been visited yet (short-form dedup).
		if d.fromKey != "" {
			if _, seen := visited[d.fromKey]; !seen {
				visited[d.fromKey] = struct{}{}
				renderDepTree(w, d.fromKey, childPrefix, nodeByKey, edgesByTo, visited)
			}
		}
	}
}

// depStateAnnotation returns a short annotation for a dependency node
// used in the dependency tree under a primary node.
func depStateAnnotation(node *GraphNode, satisfied bool) string {
	state := string(node.State)
	if satisfied {
		return state + " " + colors.Green("✓")
	}
	if node.IsCycleMember {
		return state + " " + colors.Yellow("⚠")
	}
	switch state {
	case "in-progress":
		return state + " " + colors.Cyan("⟳")
	case "completed":
		return "completed " + colors.Green("✓")
	default:
		if len(node.BlockDetails) > 0 {
			return state + ", " + colors.Yellow("blocked")
		}
		return state
	}
}

// hiddenDepAnnotation returns an annotation string for a hidden dependency
// based on its status.
func hiddenDepAnnotation(status string) string {
	switch status {
	case "satisfied":
		return "completed " + colors.Green("✓")
	case "external":
		return "external " + colors.Red("✗")
	case "ambiguous":
		return "ambiguous " + colors.Yellow("⚠")
	default:
		return "unknown ?"
	}
}
