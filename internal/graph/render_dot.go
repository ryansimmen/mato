package graph

import (
	"fmt"
	"io"
	"strings"
)

// stateColor maps NodeState values to Graphviz fill colors.
var stateColor = map[NodeState]string{
	StateCompleted:   "#90EE90",
	StateBacklog:     "#ADD8E6",
	StateInProgress:  "#FFD700",
	StateReadyReview: "#FFDAB9",
	StateReadyMerge:  "#98FB98",
	StateWaiting:     "#D3D3D3",
	StateFailed:      "#FA8072",
}

// RenderDOT writes a Graphviz DOT representation of the graph.
func RenderDOT(w io.Writer, data GraphData) {
	fmt.Fprintln(w, "digraph mato {")
	fmt.Fprintln(w, "  rankdir=LR;")
	fmt.Fprintln(w, "  node [shape=box, style=filled];")

	// Build same-SCC lookup for cycle edge detection.
	// sccIndex maps node key → list of SCC indices it belongs to.
	sccByKey := make(map[string]map[int]struct{})
	for i, scc := range data.Cycles {
		for _, key := range scc {
			if sccByKey[key] == nil {
				sccByKey[key] = make(map[int]struct{})
			}
			sccByKey[key][i] = struct{}{}
		}
	}

	// Nodes.
	if len(data.Nodes) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  // Nodes")
		for _, node := range data.Nodes {
			label := dotEscape(node.ID) + "\\npriority: " + fmt.Sprintf("%d", node.Priority) + "\\n(" + dotEscape(string(node.State))
			if len(node.BlockDetails) > 0 {
				label += ", blocked"
			}
			label += ")"

			color := stateColor[node.State]
			if color == "" {
				color = "#FFFFFF"
			}
			fmt.Fprintf(w, "  %s [label=%s, fillcolor=%q];\n",
				dotQuote(node.Key), dotQuote(label), color)
		}
	}

	// Edges.
	if len(data.Edges) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  // Edges")
		for _, edge := range data.Edges {
			attrs := dotEdgeAttrs(edge, sccByKey)
			if attrs != "" {
				fmt.Fprintf(w, "  %s -> %s [%s];\n",
					dotQuote(edge.From), dotQuote(edge.To), attrs)
			} else {
				fmt.Fprintf(w, "  %s -> %s;\n",
					dotQuote(edge.From), dotQuote(edge.To))
			}
		}
	}

	fmt.Fprintln(w, "}")
}

// dotEdgeAttrs returns DOT edge attributes based on edge type.
func dotEdgeAttrs(edge Edge, sccByKey map[string]map[int]struct{}) string {
	// Check if both From and To are in the same SCC (cycle edge).
	if isSameSCC(edge.From, edge.To, sccByKey) {
		return "style=bold, color=red"
	}

	if edge.Satisfied {
		// Satisfied: solid black (default DOT style, no attrs needed).
		return ""
	}

	// Blocked: dashed red.
	return "style=dashed, color=red"
}

// isSameSCC returns true if both keys appear in at least one common SCC.
func isSameSCC(a, b string, sccByKey map[string]map[int]struct{}) bool {
	sccsA := sccByKey[a]
	sccsB := sccByKey[b]
	if len(sccsA) == 0 || len(sccsB) == 0 {
		return false
	}
	for idx := range sccsA {
		if _, ok := sccsB[idx]; ok {
			return true
		}
	}
	return false
}

// dotEscape escapes special characters for DOT labels.
func dotEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// dotQuote wraps a string in double quotes for DOT identifiers,
// escaping internal quotes and backslashes.
func dotQuote(s string) string {
	return `"` + dotEscape(s) + `"`
}
