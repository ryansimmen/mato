package graph

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// --- helpers ---

func emptyGraph() GraphData {
	return GraphData{}
}

func simpleGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: "backlog/add-retry-logic.md", ID: "add-retry-logic",
				Filename: "add-retry-logic.md", Title: "Add retry logic",
				State: StateBacklog, Priority: 10,
				DependsOn: []string{"setup-http-client"},
				HiddenDeps: []HiddenDep{
					{DependencyID: "setup-http-client", Status: "satisfied"},
				},
			},
			{
				Key: "in-progress/setup-test-fixtures.md", ID: "setup-test-fixtures",
				Filename: "setup-test-fixtures.md", Title: "Setup test fixtures",
				State: StateInProgress, Priority: 15,
			},
		},
		Edges: []Edge{
			{From: "in-progress/setup-test-fixtures.md", To: "backlog/add-retry-logic.md", Satisfied: false},
		},
	}
}

func cycleGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: "waiting/task-a.md", ID: "task-a",
				Filename: "task-a.md", Title: "Task A",
				State: StateWaiting, Priority: 10,
				DependsOn:     []string{"task-b"},
				IsCycleMember: true,
			},
			{
				Key: "waiting/task-b.md", ID: "task-b",
				Filename: "task-b.md", Title: "Task B",
				State: StateWaiting, Priority: 20,
				DependsOn:     []string{"task-a"},
				IsCycleMember: true,
			},
		},
		Edges: []Edge{
			{From: "waiting/task-a.md", To: "waiting/task-b.md", Satisfied: false},
			{From: "waiting/task-b.md", To: "waiting/task-a.md", Satisfied: false},
		},
		Cycles: [][]string{
			{"waiting/task-a.md", "waiting/task-b.md"},
		},
	}
}

func blockedGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: "waiting/blocked-task.md", ID: "blocked-task",
				Filename: "blocked-task.md", Title: "Blocked task",
				State: StateWaiting, Priority: 10,
				DependsOn: []string{"dep-task"},
				BlockDetails: []BlockDetail{
					{DependencyID: "dep-task", Reason: "waiting"},
				},
			},
			{
				Key: "waiting/dep-task.md", ID: "dep-task",
				Filename: "dep-task.md", Title: "Dep task",
				State: StateWaiting, Priority: 5,
			},
		},
		Edges: []Edge{
			{From: "waiting/dep-task.md", To: "waiting/blocked-task.md", Satisfied: false},
		},
	}
}

func hiddenDepsGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: "backlog/my-task.md", ID: "my-task",
				Filename: "my-task.md", Title: "My task",
				State: StateBacklog, Priority: 10,
				DependsOn: []string{"completed-dep", "external-dep", "ambiguous-dep", "unknown-dep"},
				HiddenDeps: []HiddenDep{
					{DependencyID: "ambiguous-dep", Status: "ambiguous"},
					{DependencyID: "completed-dep", Status: "satisfied"},
					{DependencyID: "external-dep", Status: "external"},
					{DependencyID: "unknown-dep", Status: "unknown"},
				},
			},
		},
	}
}

func specialCharsGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: `backlog/task-with-"quotes".md`, ID: `task-with-"quotes"`,
				Filename: `task-with-"quotes".md`, Title: "Task with quotes",
				State: StateBacklog, Priority: 10,
			},
			{
				Key: `backlog/task-with-\backslash.md`, ID: `task-with-\backslash`,
				Filename: `task-with-\backslash.md`, Title: "Task with backslash",
				State: StateBacklog, Priority: 20,
				DependsOn: []string{`task-with-"quotes"`},
			},
		},
		Edges: []Edge{
			{From: `backlog/task-with-"quotes".md`, To: `backlog/task-with-\backslash.md`, Satisfied: true},
		},
	}
}

func duplicateWarningGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: "backlog/my-task.md", ID: "my-task",
				Filename: "my-task.md", Title: "My task",
				State: StateBacklog, Priority: 10,
			},
		},
		DuplicateWarnings: []DuplicateWarning{
			{Filename: "dup.md", DuplicateOf: "my-task.md", SharedID: "my-task"},
		},
	}
}

func parseFailureGraph() GraphData {
	return GraphData{
		Nodes: []GraphNode{
			{
				Key: "backlog/good-task.md", ID: "good-task",
				Filename: "good-task.md", Title: "Good task",
				State: StateBacklog, Priority: 10,
			},
		},
		ParseFailures: []ParseFailure{
			{Filename: "bad-task.md", State: "backlog", Error: "invalid frontmatter"},
		},
	}
}

// --- Text renderer tests ---

func TestRenderText_EmptyGraph(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, emptyGraph())
	got := buf.String()

	expected := "mato graph — 0 tasks, 0 edges, 0 cycles\n"
	if got != expected {
		t.Errorf("empty graph text:\ngot:  %q\nwant: %q", got, expected)
	}
}

func TestRenderText_SimpleGraph(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, simpleGraph())
	got := buf.String()

	if !strings.Contains(got, "2 tasks, 1 edges, 0 cycles") {
		t.Errorf("header mismatch:\n%s", got)
	}
	if !strings.Contains(got, "backlog/") {
		t.Errorf("missing backlog group:\n%s", got)
	}
	if !strings.Contains(got, "in-progress/") {
		t.Errorf("missing in-progress group:\n%s", got)
	}
	if !strings.Contains(got, "add-retry-logic") {
		t.Errorf("missing task ID:\n%s", got)
	}
	if !strings.Contains(got, "completed ✓") {
		t.Errorf("missing hidden dep annotation:\n%s", got)
	}
}

func TestRenderText_CycleGraph(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, cycleGraph())
	got := buf.String()

	if !strings.Contains(got, "1 cycle") {
		t.Errorf("missing cycle count:\n%s", got)
	}
	if !strings.Contains(got, "cycle ⚠") {
		t.Errorf("missing cycle annotation:\n%s", got)
	}
}

func TestRenderText_BlockedTasks(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, blockedGraph())
	got := buf.String()

	if !strings.Contains(got, "blocked") {
		t.Errorf("missing blocked annotation:\n%s", got)
	}
}

func TestRenderText_HiddenDeps(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, hiddenDepsGraph())
	got := buf.String()

	checks := []string{
		"completed ✓",
		"external ✗",
		"ambiguous ⚠",
		"unknown ?",
	}
	for _, c := range checks {
		if !strings.Contains(got, c) {
			t.Errorf("missing hidden dep annotation %q:\n%s", c, got)
		}
	}
}

func TestRenderText_DuplicateWarning(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, duplicateWarningGraph())
	got := buf.String()

	if !strings.Contains(got, "warning: dup.md is a duplicate of my-task.md") {
		t.Errorf("missing duplicate warning:\n%s", got)
	}
}

func TestRenderText_ParseFailure(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, parseFailureGraph())
	got := buf.String()

	if !strings.Contains(got, "warning: failed to parse backlog/bad-task.md: invalid frontmatter") {
		t.Errorf("missing parse failure warning:\n%s", got)
	}
}

func TestRenderText_RecursiveDepTree(t *testing.T) {
	// C depends on B, B depends on A. Text output should nest A under B
	// under C, not just show B under C.
	g := GraphData{
		Nodes: []GraphNode{
			{
				Key: "waiting/task-c.md", ID: "task-c",
				Filename: "task-c.md", Title: "Task C",
				State: StateWaiting, Priority: 30,
				DependsOn: []string{"task-b"},
			},
			{
				Key: "waiting/task-b.md", ID: "task-b",
				Filename: "task-b.md", Title: "Task B",
				State: StateWaiting, Priority: 20,
				DependsOn: []string{"task-a"},
			},
			{
				Key: "backlog/task-a.md", ID: "task-a",
				Filename: "task-a.md", Title: "Task A",
				State: StateBacklog, Priority: 10,
			},
		},
		Edges: []Edge{
			{From: "backlog/task-a.md", To: "waiting/task-b.md", Satisfied: false},
			{From: "waiting/task-b.md", To: "waiting/task-c.md", Satisfied: false},
		},
	}

	var buf bytes.Buffer
	RenderText(&buf, g)
	got := buf.String()

	// Under task-c, task-b should appear, and under task-b, task-a should
	// appear nested one level deeper.
	lines := strings.Split(got, "\n")
	foundB := -1
	foundAUnderB := false
	for i, line := range lines {
		// Find task-b as a dependency of task-c (indented).
		if strings.Contains(line, "└── task-b") || strings.Contains(line, "├── task-b") {
			foundB = i
		}
		// After finding task-b, look for task-a nested deeper.
		if foundB >= 0 && i > foundB && strings.Contains(line, "task-a") {
			// task-a should be indented further than task-b.
			foundAUnderB = true
		}
	}
	if foundB < 0 {
		t.Errorf("task-b not found as dependency of task-c:\n%s", got)
	}
	if !foundAUnderB {
		t.Errorf("task-a not found nested under task-b (recursive dep tree missing):\n%s", got)
	}
}

// --- DOT renderer tests ---

func TestRenderDOT_EmptyGraph(t *testing.T) {
	var buf bytes.Buffer
	RenderDOT(&buf, emptyGraph())
	got := buf.String()

	if !strings.Contains(got, "digraph mato {") {
		t.Errorf("missing digraph declaration:\n%s", got)
	}
	if !strings.Contains(got, "rankdir=LR") {
		t.Errorf("missing rankdir:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "}") {
		t.Errorf("missing closing brace:\n%s", got)
	}
	// Should have no node or edge lines.
	if strings.Contains(got, "// Nodes") {
		t.Errorf("empty graph should not have node section:\n%s", got)
	}
}

func TestRenderDOT_CycleEdges_SameSCC(t *testing.T) {
	var buf bytes.Buffer
	RenderDOT(&buf, cycleGraph())
	got := buf.String()

	// Both edges should be bold red (same SCC).
	if !strings.Contains(got, "style=bold, color=red") {
		t.Errorf("missing bold red cycle edge:\n%s", got)
	}

	// Count occurrences of bold red — should be 2 (one for each direction).
	count := strings.Count(got, "style=bold, color=red")
	if count != 2 {
		t.Errorf("expected 2 bold red edges, got %d:\n%s", count, got)
	}
}

func TestRenderDOT_CycleEdges_DifferentSCC(t *testing.T) {
	// Two nodes in different SCCs should NOT get bold red edges between them.
	data := GraphData{
		Nodes: []GraphNode{
			{Key: "waiting/a.md", ID: "a", Filename: "a.md", State: StateWaiting, Priority: 1, IsCycleMember: true},
			{Key: "waiting/b.md", ID: "b", Filename: "b.md", State: StateWaiting, Priority: 2, IsCycleMember: true},
			{Key: "waiting/c.md", ID: "c", Filename: "c.md", State: StateWaiting, Priority: 3, IsCycleMember: true},
		},
		Edges: []Edge{
			// a→b is NOT a cycle edge (different SCCs).
			{From: "waiting/a.md", To: "waiting/b.md", Satisfied: false},
		},
		Cycles: [][]string{
			{"waiting/a.md", "waiting/c.md"}, // SCC 1: a, c
			{"waiting/b.md"},                 // SCC 2: b only (self-loop)
		},
	}

	var buf bytes.Buffer
	RenderDOT(&buf, data)
	got := buf.String()

	// The a→b edge should be dashed red (blocked), NOT bold red.
	if strings.Contains(got, "style=bold, color=red") {
		t.Errorf("edge between different SCCs should not be bold red:\n%s", got)
	}
	if !strings.Contains(got, "style=dashed, color=red") {
		t.Errorf("blocked edge should be dashed red:\n%s", got)
	}
}

func TestRenderDOT_BlockedEdges(t *testing.T) {
	var buf bytes.Buffer
	RenderDOT(&buf, blockedGraph())
	got := buf.String()

	if !strings.Contains(got, "style=dashed, color=red") {
		t.Errorf("missing dashed red edge:\n%s", got)
	}
}

func TestRenderDOT_SatisfiedEdges(t *testing.T) {
	data := GraphData{
		Nodes: []GraphNode{
			{Key: "completed/dep.md", ID: "dep", Filename: "dep.md", State: StateCompleted, Priority: 1},
			{Key: "backlog/task.md", ID: "task", Filename: "task.md", State: StateBacklog, Priority: 10},
		},
		Edges: []Edge{
			{From: "completed/dep.md", To: "backlog/task.md", Satisfied: true},
		},
	}

	var buf bytes.Buffer
	RenderDOT(&buf, data)
	got := buf.String()

	// Satisfied edges have no style attributes (default solid black).
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		if strings.Contains(line, "->") {
			if strings.Contains(line, "style=") {
				t.Errorf("satisfied edge should have no style attrs:\n%s", line)
			}
		}
	}
}

func TestRenderDOT_SpecialCharacters(t *testing.T) {
	var buf bytes.Buffer
	RenderDOT(&buf, specialCharsGraph())
	got := buf.String()

	// Quotes should be escaped in node IDs and labels.
	if !strings.Contains(got, `\"quotes\"`) {
		t.Errorf("missing escaped quotes:\n%s", got)
	}
	// Backslashes should be escaped.
	if !strings.Contains(got, `\\backslash`) {
		t.Errorf("missing escaped backslash:\n%s", got)
	}
}

func TestRenderDOT_LabelNewlines(t *testing.T) {
	data := GraphData{
		Nodes: []GraphNode{
			{Key: "waiting/task.md", ID: "task", Filename: "task.md", State: StateWaiting, Priority: 7},
		},
	}

	var buf bytes.Buffer
	RenderDOT(&buf, data)
	got := buf.String()

	if !strings.Contains(got, `label="task\npriority: 7\n(waiting)"`) {
		t.Fatalf("missing DOT newline escapes in label:\n%s", got)
	}
	if strings.Contains(got, `label="task\\npriority: 7\\n(waiting)"`) {
		t.Fatalf("label newlines were double-escaped:\n%s", got)
	}
}

func TestRenderDOT_NodeColors(t *testing.T) {
	tests := []struct {
		state NodeState
		color string
	}{
		{StateCompleted, "#90EE90"},
		{StateBacklog, "#ADD8E6"},
		{StateInProgress, "#FFD700"},
		{StateReadyReview, "#FFDAB9"},
		{StateReadyMerge, "#98FB98"},
		{StateWaiting, "#D3D3D3"},
		{StateFailed, "#FA8072"},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			data := GraphData{
				Nodes: []GraphNode{
					{Key: string(tt.state) + "/t.md", ID: "t", Filename: "t.md", State: tt.state, Priority: 1},
				},
			}
			var buf bytes.Buffer
			RenderDOT(&buf, data)
			got := buf.String()
			if !strings.Contains(got, tt.color) {
				t.Errorf("state %s: missing color %s:\n%s", tt.state, tt.color, got)
			}
		})
	}
}

// --- JSON renderer tests ---

func TestRenderJSON_EmptyGraph(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, emptyGraph()); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	var got GraphData
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if got.Nodes != nil {
		t.Errorf("expected nil nodes, got %v", got.Nodes)
	}
}

func TestRenderJSON_RoundTrip_AllFields(t *testing.T) {
	original := simpleGraph()
	var buf bytes.Buffer
	if err := RenderJSON(&buf, original); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	var decoded GraphData
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(decoded.Nodes) != len(original.Nodes) {
		t.Errorf("node count: got %d, want %d", len(decoded.Nodes), len(original.Nodes))
	}
	if len(decoded.Edges) != len(original.Edges) {
		t.Errorf("edge count: got %d, want %d", len(decoded.Edges), len(original.Edges))
	}
	for i, n := range decoded.Nodes {
		if n.Key != original.Nodes[i].Key {
			t.Errorf("node[%d].Key: got %q, want %q", i, n.Key, original.Nodes[i].Key)
		}
		if n.ID != original.Nodes[i].ID {
			t.Errorf("node[%d].ID: got %q, want %q", i, n.ID, original.Nodes[i].ID)
		}
		if n.State != original.Nodes[i].State {
			t.Errorf("node[%d].State: got %q, want %q", i, n.State, original.Nodes[i].State)
		}
	}
}

func TestRenderJSON_CycleGraph(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, cycleGraph()); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	var got GraphData
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(got.Cycles) != 1 {
		t.Errorf("expected 1 cycle, got %d", len(got.Cycles))
	}
	if len(got.Cycles[0]) != 2 {
		t.Errorf("expected 2 members in cycle, got %d", len(got.Cycles[0]))
	}
}

func TestRenderJSON_HiddenDeps(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, hiddenDepsGraph()); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	var got GraphData
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if len(got.Nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(got.Nodes))
	}
	if len(got.Nodes[0].HiddenDeps) != 4 {
		t.Errorf("expected 4 hidden deps, got %d", len(got.Nodes[0].HiddenDeps))
	}

	statusMap := make(map[string]string)
	for _, hd := range got.Nodes[0].HiddenDeps {
		statusMap[hd.DependencyID] = hd.Status
	}
	if statusMap["completed-dep"] != "satisfied" {
		t.Errorf("completed-dep status: got %q, want %q", statusMap["completed-dep"], "satisfied")
	}
	if statusMap["external-dep"] != "external" {
		t.Errorf("external-dep status: got %q, want %q", statusMap["external-dep"], "external")
	}
}

func TestRenderJSON_Indented(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, simpleGraph()); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	// JSON should be indented (contain newlines and spaces).
	if !strings.Contains(buf.String(), "\n  ") {
		t.Errorf("JSON output should be indented:\n%s", buf.String())
	}
}

// --- Deterministic output tests ---

func TestRenderText_Deterministic(t *testing.T) {
	data := simpleGraph()
	var buf1, buf2 bytes.Buffer
	RenderText(&buf1, data)
	RenderText(&buf2, data)
	if buf1.String() != buf2.String() {
		t.Errorf("text output is not deterministic:\nfirst:  %q\nsecond: %q", buf1.String(), buf2.String())
	}
}

func TestRenderDOT_Deterministic(t *testing.T) {
	data := simpleGraph()
	var buf1, buf2 bytes.Buffer
	RenderDOT(&buf1, data)
	RenderDOT(&buf2, data)
	if buf1.String() != buf2.String() {
		t.Errorf("DOT output is not deterministic:\nfirst:  %q\nsecond: %q", buf1.String(), buf2.String())
	}
}

func TestRenderJSON_Deterministic(t *testing.T) {
	data := simpleGraph()
	var buf1, buf2 bytes.Buffer
	if err := RenderJSON(&buf1, data); err != nil {
		t.Fatalf("first RenderJSON: %v", err)
	}
	if err := RenderJSON(&buf2, data); err != nil {
		t.Fatalf("second RenderJSON: %v", err)
	}
	if buf1.String() != buf2.String() {
		t.Errorf("JSON output is not deterministic:\nfirst:  %q\nsecond: %q", buf1.String(), buf2.String())
	}
}

// --- Text self-dependency test ---

func TestRenderText_SelfDependency(t *testing.T) {
	data := GraphData{
		Nodes: []GraphNode{
			{
				Key: "waiting/self-dep.md", ID: "self-dep",
				Filename: "self-dep.md", Title: "Self dep",
				State: StateWaiting, Priority: 10,
				DependsOn:     []string{"self-dep"},
				IsCycleMember: true,
			},
		},
		Edges: []Edge{
			{From: "waiting/self-dep.md", To: "waiting/self-dep.md", Satisfied: false},
		},
		Cycles: [][]string{
			{"waiting/self-dep.md"},
		},
	}

	var buf bytes.Buffer
	RenderText(&buf, data)
	got := buf.String()

	if !strings.Contains(got, "self-dependency") {
		t.Errorf("missing self-dependency annotation:\n%s", got)
	}
}

// --- Multi-state graph test ---

func TestRenderText_AllStates(t *testing.T) {
	data := GraphData{
		Nodes: []GraphNode{
			{Key: "waiting/w.md", ID: "w", Filename: "w.md", State: StateWaiting, Priority: 1},
			{Key: "backlog/b.md", ID: "b", Filename: "b.md", State: StateBacklog, Priority: 2},
			{Key: "in-progress/ip.md", ID: "ip", Filename: "ip.md", State: StateInProgress, Priority: 3},
			{Key: "ready-for-review/rfr.md", ID: "rfr", Filename: "rfr.md", State: StateReadyReview, Priority: 4},
			{Key: "ready-to-merge/rtm.md", ID: "rtm", Filename: "rtm.md", State: StateReadyMerge, Priority: 5},
		},
	}

	var buf bytes.Buffer
	RenderText(&buf, data)
	got := buf.String()

	states := []string{"waiting/", "backlog/", "in-progress/", "ready-for-review/", "ready-to-merge/"}
	for _, s := range states {
		if !strings.Contains(got, s) {
			t.Errorf("missing state group %q:\n%s", s, got)
		}
	}

	// Verify ordering: waiting should come before backlog.
	wIdx := strings.Index(got, "waiting/")
	bIdx := strings.Index(got, "backlog/")
	if wIdx > bIdx {
		t.Errorf("waiting should appear before backlog:\n%s", got)
	}
}

func TestRenderText_NoColorFallback(t *testing.T) {
	tests := []struct {
		name string
		data GraphData
		want []string
	}{
		{
			name: "header and state groups readable",
			data: simpleGraph(),
			want: []string{"mato graph", "2 tasks, 1 edges, 0 cycles", "backlog/", "in-progress/"},
		},
		{
			name: "cycle annotation readable",
			data: cycleGraph(),
			want: []string{"cycle ⚠", "1 cycles"},
		},
		{
			name: "blocked annotation readable",
			data: blockedGraph(),
			want: []string{"blocked"},
		},
		{
			name: "hidden dep symbols readable",
			data: hiddenDepsGraph(),
			want: []string{"completed ✓", "external ✗", "ambiguous ⚠", "unknown ?"},
		},
		{
			name: "duplicate warning readable",
			data: duplicateWarningGraph(),
			want: []string{"warning: dup.md is a duplicate of my-task.md"},
		},
		{
			name: "parse failure warning readable",
			data: parseFailureGraph(),
			want: []string{"warning: failed to parse backlog/bad-task.md: invalid frontmatter"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			RenderText(&buf, tt.data)
			got := buf.String()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("output missing %q:\n%s", w, got)
				}
			}
		})
	}
}
