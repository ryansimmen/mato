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
func RenderDOT(w io.Writer, data GraphData) error {
	dw := dotRenderWriter{w: w}

	dw.println("digraph mato {")
	dw.println("  rankdir=LR;")
	dw.println("  node [shape=box, style=filled];")

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
		dw.println()
		dw.println("  // Nodes")
		for _, node := range data.Nodes {
			label := node.ID + "\npriority: " + fmt.Sprintf("%d", node.Priority) + "\n(" + string(node.State)
			if len(node.BlockDetails) > 0 {
				label += ", blocked"
			}
			label += ")"

			color := stateColor[node.State]
			if color == "" {
				color = "#FFFFFF"
			}
			dw.printf("  %s [label=%s, fillcolor=%q];\n",
				dotQuote(node.Key), dotQuoteLabel(label), color)
		}
	}

	// Edges.
	if len(data.Edges) > 0 {
		dw.println()
		dw.println("  // Edges")
		for _, edge := range data.Edges {
			attrs := dotEdgeAttrs(edge, sccByKey)
			if attrs != "" {
				dw.printf("  %s -> %s [%s];\n",
					dotQuote(edge.From), dotQuote(edge.To), attrs)
			} else {
				dw.printf("  %s -> %s;\n",
					dotQuote(edge.From), dotQuote(edge.To))
			}
		}
	}

	dw.println("}")
	return dw.err
}

type dotRenderWriter struct {
	w   io.Writer
	err error
}

func (dw *dotRenderWriter) printf(format string, args ...any) {
	if dw.err != nil {
		return
	}
	_, dw.err = fmt.Fprintf(dw.w, format, args...)
}

func (dw *dotRenderWriter) println(args ...any) {
	if dw.err != nil {
		return
	}
	_, dw.err = fmt.Fprintln(dw.w, args...)
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

// dotQuoteLabel wraps a DOT label in double quotes, preserving newline escapes.
func dotQuoteLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}
