package integration_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"mato/internal/dirs"
	"mato/internal/graph"
	"mato/internal/testutil"
)

func TestGraph_LinearDependencyChain(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// A is completed, B depends on A (waiting), C depends on B (waiting).
	writeTask(t, tasksDir, dirs.Completed, "task-a.md", "---\nid: task-a\npriority: 1\n---\n# Task A\n")
	writeTask(t, tasksDir, dirs.Waiting, "task-b.md", "---\nid: task-b\npriority: 2\ndepends_on: [task-a]\n---\n# Task B\n")
	writeTask(t, tasksDir, dirs.Backlog, "task-c.md", "---\nid: task-c\npriority: 3\ndepends_on: [task-b]\n---\n# Task C\n")

	tests := []struct {
		name    string
		format  string
		showAll bool
		check   func(t *testing.T, output string)
	}{
		{
			name:    "text default hides completed",
			format:  "text",
			showAll: false,
			check: func(t *testing.T, output string) {
				t.Helper()
				if strings.Contains(output, "completed/") {
					t.Fatalf("text output with showAll=false should not contain completed section, got:\n%s", output)
				}
				if !strings.Contains(output, "waiting/") {
					t.Fatalf("text output should contain waiting/ section, got:\n%s", output)
				}
				if !strings.Contains(output, "task-b") {
					t.Fatalf("text output should contain task-b, got:\n%s", output)
				}
			},
		},
		{
			name:    "text showAll includes completed",
			format:  "text",
			showAll: true,
			check: func(t *testing.T, output string) {
				t.Helper()
				if !strings.Contains(output, "completed/") {
					t.Fatalf("text output with showAll=true should contain completed/ section, got:\n%s", output)
				}
				if !strings.Contains(output, "task-a") {
					t.Fatalf("text output with showAll=true should contain task-a, got:\n%s", output)
				}
			},
		},
		{
			name:    "dot format",
			format:  "dot",
			showAll: false,
			check: func(t *testing.T, output string) {
				t.Helper()
				if !strings.HasPrefix(output, "digraph") {
					t.Fatalf("dot output should start with 'digraph', got:\n%s", output)
				}
				if !strings.Contains(output, "}") {
					t.Fatalf("dot output should contain closing brace, got:\n%s", output)
				}
			},
		},
		{
			name:    "json format",
			format:  "json",
			showAll: false,
			check: func(t *testing.T, output string) {
				t.Helper()
				var data graph.GraphData
				if err := json.Unmarshal([]byte(output), &data); err != nil {
					t.Fatalf("json output should be valid JSON: %v\noutput:\n%s", err, output)
				}
				// showAll=false: completed tasks are excluded.
				for _, n := range data.Nodes {
					if n.State == graph.StateCompleted {
						t.Fatalf("json with showAll=false should not include completed nodes, got node %q", n.ID)
					}
				}
			},
		},
		{
			name:    "json showAll includes all states",
			format:  "json",
			showAll: true,
			check: func(t *testing.T, output string) {
				t.Helper()
				var data graph.GraphData
				if err := json.Unmarshal([]byte(output), &data); err != nil {
					t.Fatalf("json output should be valid JSON: %v", err)
				}
				found := false
				for _, n := range data.Nodes {
					if n.ID == "task-a" && n.State == graph.StateCompleted {
						found = true
					}
				}
				if !found {
					t.Fatal("json with showAll=true should include completed task-a")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := graph.ShowTo(&buf, repoRoot, tt.format, tt.showAll); err != nil {
				t.Fatalf("graph.ShowTo(%s, showAll=%v): %v", tt.format, tt.showAll, err)
			}
			tt.check(t, buf.String())
		})
	}
}

func TestGraph_AliasResolution(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Task with explicit ID different from stem.
	writeTask(t, tasksDir, dirs.Completed, "setup-database.md", "---\nid: db-setup\npriority: 1\n---\n# Setup Database\n")
	// Task that depends on stem alias.
	writeTask(t, tasksDir, dirs.Waiting, "use-stem.md", "---\nid: use-stem\npriority: 2\ndepends_on: [setup-database]\n---\n# Use stem alias\n")
	// Task that depends on meta.ID alias.
	writeTask(t, tasksDir, dirs.Waiting, "use-id.md", "---\nid: use-id\npriority: 3\ndepends_on: [db-setup]\n---\n# Use meta ID\n")

	var buf bytes.Buffer
	if err := graph.ShowTo(&buf, repoRoot, "json", true); err != nil {
		t.Fatalf("graph.ShowTo: %v", err)
	}

	var data graph.GraphData
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("unmarshal graph JSON: %v", err)
	}

	// Both use-stem and use-id should have edges pointing from setup-database.
	edgeTargets := make(map[string][]string)
	for _, e := range data.Edges {
		edgeTargets[e.To] = append(edgeTargets[e.To], e.From)
	}

	stemKey := "waiting/use-stem.md"
	if froms, ok := edgeTargets[stemKey]; !ok || len(froms) == 0 {
		t.Fatalf("expected edge to %s, got none; edges = %+v", stemKey, data.Edges)
	}

	idKey := "waiting/use-id.md"
	if froms, ok := edgeTargets[idKey]; !ok || len(froms) == 0 {
		t.Fatalf("expected edge to %s, got none; edges = %+v", idKey, data.Edges)
	}
}

func TestGraph_AmbiguousIDs(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Same ID in both completed/ and waiting/ — ambiguous.
	writeTask(t, tasksDir, dirs.Completed, "shared-done.md", "---\nid: shared\n---\n# Shared done\n")
	writeTask(t, tasksDir, dirs.Waiting, "shared-waiting.md", "---\nid: shared\n---\n# Shared waiting\n")
	// Task depending on the ambiguous ID.
	writeTask(t, tasksDir, dirs.Waiting, "dependent.md", "---\nid: dependent\ndepends_on: [shared]\n---\n# Dependent\n")

	var buf bytes.Buffer
	if err := graph.ShowTo(&buf, repoRoot, "json", true); err != nil {
		t.Fatalf("graph.ShowTo: %v", err)
	}

	var data graph.GraphData
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Find the dependent node and check for ambiguous block detail.
	var depNode *graph.GraphNode
	for i := range data.Nodes {
		if data.Nodes[i].ID == "dependent" {
			depNode = &data.Nodes[i]
			break
		}
	}
	if depNode == nil {
		t.Fatal("dependent node not found in graph")
	}

	foundAmbiguous := false
	for _, bd := range depNode.BlockDetails {
		if bd.DependencyID == "shared" && bd.Reason == "ambiguous" {
			foundAmbiguous = true
		}
	}
	if !foundAmbiguous {
		t.Fatalf("expected ambiguous block detail for 'shared', got block details: %+v, hidden deps: %+v",
			depNode.BlockDetails, depNode.HiddenDeps)
	}
}

func TestGraph_AllFormatsValid(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.Waiting, "alpha.md", "---\nid: alpha\npriority: 1\ndepends_on: [beta]\n---\n# Alpha\n")
	writeTask(t, tasksDir, dirs.Waiting, "beta.md", "---\nid: beta\npriority: 2\ndepends_on: [alpha]\n---\n# Beta (cycle)\n")
	writeTask(t, tasksDir, dirs.Backlog, "gamma.md", "---\nid: gamma\npriority: 3\n---\n# Gamma\n")
	writeTask(t, tasksDir, dirs.InProgress, "delta.md", "<!-- claimed-by: test  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: delta\npriority: 4\n---\n# Delta\n")

	for _, format := range []string{"text", "dot", "json"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			if err := graph.ShowTo(&buf, repoRoot, format, false); err != nil {
				t.Fatalf("ShowTo(%s): %v", format, err)
			}
			output := buf.String()
			if output == "" {
				t.Fatalf("ShowTo(%s) produced empty output", format)
			}

			switch format {
			case "json":
				var data graph.GraphData
				if err := json.Unmarshal([]byte(output), &data); err != nil {
					t.Fatalf("invalid JSON: %v", err)
				}
			case "dot":
				if !strings.HasPrefix(output, "digraph") {
					t.Fatalf("DOT output should start with 'digraph'")
				}
			case "text":
				if !strings.Contains(output, "mato graph") {
					t.Fatalf("text output should contain header")
				}
			}
		})
	}
}

func TestGraph_ShowAllDifference(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.Backlog, "active.md", "---\nid: active\n---\n# Active\n")
	writeTask(t, tasksDir, dirs.Completed, "done.md", "---\nid: done\n---\n# Done\n")
	writeTask(t, tasksDir, dirs.Failed, "broken.md", "---\nid: broken\n---\n# Broken\n")

	// showAll=false: only active tasks.
	var bufDefault bytes.Buffer
	if err := graph.ShowTo(&bufDefault, repoRoot, "json", false); err != nil {
		t.Fatalf("ShowTo(showAll=false): %v", err)
	}
	var dataDefault graph.GraphData
	if err := json.Unmarshal(bufDefault.Bytes(), &dataDefault); err != nil {
		t.Fatalf("unmarshal default: %v", err)
	}

	// showAll=true: all tasks.
	var bufAll bytes.Buffer
	if err := graph.ShowTo(&bufAll, repoRoot, "json", true); err != nil {
		t.Fatalf("ShowTo(showAll=true): %v", err)
	}
	var dataAll graph.GraphData
	if err := json.Unmarshal(bufAll.Bytes(), &dataAll); err != nil {
		t.Fatalf("unmarshal all: %v", err)
	}

	if len(dataAll.Nodes) <= len(dataDefault.Nodes) {
		t.Fatalf("showAll=true should have more nodes than showAll=false: all=%d, default=%d",
			len(dataAll.Nodes), len(dataDefault.Nodes))
	}

	// Default should not include completed or failed.
	for _, n := range dataDefault.Nodes {
		if n.State == graph.StateCompleted || n.State == graph.StateFailed {
			t.Fatalf("showAll=false should not include %s state, got node %q", n.State, n.ID)
		}
	}
}

func TestGraph_EmptyRepo(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)

	for _, format := range []string{"text", "dot", "json"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			err := graph.ShowTo(&buf, repoRoot, format, false)
			if err != nil {
				t.Fatalf("ShowTo(%s) on empty repo should succeed, got: %v", format, err)
			}
			output := buf.String()
			if output == "" {
				t.Fatalf("ShowTo(%s) on empty repo produced empty output", format)
			}

			switch format {
			case "json":
				var data graph.GraphData
				if err := json.Unmarshal([]byte(output), &data); err != nil {
					t.Fatalf("invalid JSON on empty repo: %v", err)
				}
				if len(data.Nodes) != 0 {
					t.Fatalf("empty repo should have 0 nodes, got %d", len(data.Nodes))
				}
			case "text":
				if !strings.Contains(output, "0 tasks") {
					t.Fatalf("text output for empty repo should show 0 tasks, got:\n%s", output)
				}
			case "dot":
				if !strings.HasPrefix(output, "digraph") {
					t.Fatalf("DOT output should start with 'digraph'")
				}
			}
		})
	}
}
