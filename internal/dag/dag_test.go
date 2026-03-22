package dag

import (
	"reflect"
	"testing"
)

func TestAnalyze_Empty(t *testing.T) {
	result := Analyze(nil, nil, nil, nil)
	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	if len(result.Blocked) != 0 {
		t.Fatalf("Blocked = %v, want empty", result.Blocked)
	}
	if len(result.Cycles) != 0 {
		t.Fatalf("Cycles = %v, want empty", result.Cycles)
	}
}

func TestAnalyze_NoDeps(t *testing.T) {
	waiting := []Node{
		{ID: "task-a"},
		{ID: "task-b"},
	}
	result := Analyze(waiting, nil, nil, nil)
	want := []string{"task-a", "task-b"}
	if !reflect.DeepEqual(result.DepsSatisfied, want) {
		t.Fatalf("DepsSatisfied = %v, want %v", result.DepsSatisfied, want)
	}
	if len(result.Blocked) != 0 {
		t.Fatalf("Blocked = %v, want empty", result.Blocked)
	}
	if len(result.Cycles) != 0 {
		t.Fatalf("Cycles = %v, want empty", result.Cycles)
	}
}

func TestAnalyze_Chain(t *testing.T) {
	// A is completed, B depends on A, C depends on B.
	// B should be deps-satisfied, C should be blocked (B is waiting).
	waiting := []Node{
		{ID: "B", DependsOn: []string{"A"}},
		{ID: "C", DependsOn: []string{"B"}},
	}
	completed := map[string]struct{}{"A": {}}
	known := map[string]struct{}{"A": {}, "B": {}, "C": {}}

	result := Analyze(waiting, completed, known, nil)

	if !reflect.DeepEqual(result.DepsSatisfied, []string{"B"}) {
		t.Fatalf("DepsSatisfied = %v, want [B]", result.DepsSatisfied)
	}
	wantBlocked := map[string][]BlockDetail{
		"C": {{DependencyID: "B", Reason: BlockedByWaiting}},
	}
	if !reflect.DeepEqual(result.Blocked, wantBlocked) {
		t.Fatalf("Blocked = %v, want %v", result.Blocked, wantBlocked)
	}
	if len(result.Cycles) != 0 {
		t.Fatalf("Cycles = %v, want empty", result.Cycles)
	}
}

func TestAnalyze_FanIn(t *testing.T) {
	// C depends on A (completed) and B (waiting). C should stay blocked.
	waiting := []Node{
		{ID: "B"},
		{ID: "C", DependsOn: []string{"A", "B"}},
	}
	completed := map[string]struct{}{"A": {}}
	known := map[string]struct{}{"A": {}, "B": {}, "C": {}}

	result := Analyze(waiting, completed, known, nil)

	if !reflect.DeepEqual(result.DepsSatisfied, []string{"B"}) {
		t.Fatalf("DepsSatisfied = %v, want [B]", result.DepsSatisfied)
	}
	wantBlocked := map[string][]BlockDetail{
		"C": {{DependencyID: "B", Reason: BlockedByWaiting}},
	}
	if !reflect.DeepEqual(result.Blocked, wantBlocked) {
		t.Fatalf("Blocked = %v, want %v", result.Blocked, wantBlocked)
	}
}

func TestAnalyze_SelfDependency(t *testing.T) {
	waiting := []Node{
		{ID: "self", DependsOn: []string{"self"}},
	}
	known := map[string]struct{}{"self": {}}

	result := Analyze(waiting, nil, known, nil)

	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	if len(result.Blocked) != 0 {
		t.Fatalf("Blocked = %v, want empty (cycle members not in Blocked)", result.Blocked)
	}
	wantCycles := [][]string{{"self"}}
	if !reflect.DeepEqual(result.Cycles, wantCycles) {
		t.Fatalf("Cycles = %v, want %v", result.Cycles, wantCycles)
	}
}

func TestAnalyze_UnknownDependency(t *testing.T) {
	waiting := []Node{
		{ID: "task-a", DependsOn: []string{"nonexistent"}},
	}
	known := map[string]struct{}{"task-a": {}}

	result := Analyze(waiting, nil, known, nil)

	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	wantBlocked := map[string][]BlockDetail{
		"task-a": {{DependencyID: "nonexistent", Reason: BlockedByUnknown}},
	}
	if !reflect.DeepEqual(result.Blocked, wantBlocked) {
		t.Fatalf("Blocked = %v, want %v", result.Blocked, wantBlocked)
	}
}

func TestAnalyze_ExternalDependency(t *testing.T) {
	// Dep exists in knownIDs but not in completedIDs or waiting.
	waiting := []Node{
		{ID: "task-a", DependsOn: []string{"failed-task"}},
	}
	known := map[string]struct{}{"task-a": {}, "failed-task": {}}

	result := Analyze(waiting, nil, known, nil)

	wantBlocked := map[string][]BlockDetail{
		"task-a": {{DependencyID: "failed-task", Reason: BlockedByExternal}},
	}
	if !reflect.DeepEqual(result.Blocked, wantBlocked) {
		t.Fatalf("Blocked = %v, want %v", result.Blocked, wantBlocked)
	}
}

func TestAnalyze_AmbiguousCompletedExcluded(t *testing.T) {
	// Ambiguous ID is excluded from completedIDs by the caller.
	// Task should remain blocked.
	waiting := []Node{
		{ID: "task-a", DependsOn: []string{"ambig"}},
	}
	// completedIDs does NOT contain "ambig" (caller excluded it).
	known := map[string]struct{}{"task-a": {}, "ambig": {}}
	ambiguous := map[string]struct{}{"ambig": {}}

	result := Analyze(waiting, nil, known, ambiguous)

	wantBlocked := map[string][]BlockDetail{
		"task-a": {{DependencyID: "ambig", Reason: BlockedByAmbiguous}},
	}
	if !reflect.DeepEqual(result.Blocked, wantBlocked) {
		t.Fatalf("Blocked = %v, want %v", result.Blocked, wantBlocked)
	}
}

func TestAnalyze_AmbiguousDependency(t *testing.T) {
	// Dep in ambiguousIDs blocks with BlockedByAmbiguous, not BlockedByExternal.
	waiting := []Node{
		{ID: "downstream", DependsOn: []string{"ambig-dep"}},
	}
	known := map[string]struct{}{"downstream": {}, "ambig-dep": {}}
	ambiguous := map[string]struct{}{"ambig-dep": {}}

	result := Analyze(waiting, nil, known, ambiguous)

	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	blocked := result.Blocked["downstream"]
	if len(blocked) != 1 || blocked[0].Reason != BlockedByAmbiguous {
		t.Fatalf("Blocked[downstream] = %v, want BlockedByAmbiguous for ambig-dep", blocked)
	}
}

func TestAnalyze_CycleMembersOnly(t *testing.T) {
	// A -> B -> A (cycle), D -> A (downstream, not cycle member).
	waiting := []Node{
		{ID: "A", DependsOn: []string{"B"}},
		{ID: "B", DependsOn: []string{"A"}},
		{ID: "D", DependsOn: []string{"A"}},
	}
	known := map[string]struct{}{"A": {}, "B": {}, "D": {}}

	result := Analyze(waiting, nil, known, nil)

	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	wantCycles := [][]string{{"A", "B"}}
	if !reflect.DeepEqual(result.Cycles, wantCycles) {
		t.Fatalf("Cycles = %v, want %v", result.Cycles, wantCycles)
	}
	// D should be blocked by A (waiting), not a cycle member.
	if _, isCycleMember := result.Blocked["A"]; isCycleMember {
		t.Fatal("cycle member A should not appear in Blocked")
	}
	if _, isCycleMember := result.Blocked["B"]; isCycleMember {
		t.Fatal("cycle member B should not appear in Blocked")
	}
	blockedD := result.Blocked["D"]
	if len(blockedD) != 1 || blockedD[0].DependencyID != "A" || blockedD[0].Reason != BlockedByWaiting {
		t.Fatalf("Blocked[D] = %v, want [{A BlockedByWaiting}]", blockedD)
	}
}

func TestAnalyze_LongCycle(t *testing.T) {
	// A -> B -> C -> A (3-node cycle), D -> C (downstream).
	waiting := []Node{
		{ID: "A", DependsOn: []string{"C"}},
		{ID: "B", DependsOn: []string{"A"}},
		{ID: "C", DependsOn: []string{"B"}},
		{ID: "D", DependsOn: []string{"C"}},
	}
	known := map[string]struct{}{"A": {}, "B": {}, "C": {}, "D": {}}

	result := Analyze(waiting, nil, known, nil)

	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	wantCycles := [][]string{{"A", "B", "C"}}
	if !reflect.DeepEqual(result.Cycles, wantCycles) {
		t.Fatalf("Cycles = %v, want %v", result.Cycles, wantCycles)
	}
	// D should be blocked, not a cycle member.
	blockedD := result.Blocked["D"]
	if len(blockedD) != 1 || blockedD[0].DependencyID != "C" || blockedD[0].Reason != BlockedByWaiting {
		t.Fatalf("Blocked[D] = %v, want [{C BlockedByWaiting}]", blockedD)
	}
	for _, member := range []string{"A", "B", "C"} {
		if _, ok := result.Blocked[member]; ok {
			t.Fatalf("cycle member %s should not appear in Blocked", member)
		}
	}
}

func TestAnalyze_Deterministic(t *testing.T) {
	// Run multiple times and verify stable output ordering.
	waiting := []Node{
		{ID: "Z"},
		{ID: "A"},
		{ID: "M", DependsOn: []string{"Z"}},
	}
	completed := map[string]struct{}{}
	known := map[string]struct{}{"A": {}, "Z": {}, "M": {}}

	var prev Analysis
	for i := 0; i < 20; i++ {
		result := Analyze(waiting, completed, known, nil)
		if i > 0 {
			if !reflect.DeepEqual(result.DepsSatisfied, prev.DepsSatisfied) {
				t.Fatalf("non-deterministic DepsSatisfied on iteration %d: %v vs %v", i, result.DepsSatisfied, prev.DepsSatisfied)
			}
			if !reflect.DeepEqual(result.Blocked, prev.Blocked) {
				t.Fatalf("non-deterministic Blocked on iteration %d", i)
			}
			if !reflect.DeepEqual(result.Cycles, prev.Cycles) {
				t.Fatalf("non-deterministic Cycles on iteration %d", i)
			}
		}
		prev = result
	}
}

func TestAnalyze_BlockReasons(t *testing.T) {
	// Task with multiple deps, each blocked for a different reason.
	waiting := []Node{
		{ID: "blocker"}, // another waiting task
		{ID: "task-a", DependsOn: []string{"blocker", "external", "ambig", "unknown"}},
	}
	completed := map[string]struct{}{} // ambig excluded from completedIDs by caller
	known := map[string]struct{}{"task-a": {}, "blocker": {}, "external": {}, "ambig": {}}
	ambiguous := map[string]struct{}{"ambig": {}}

	result := Analyze(waiting, completed, known, ambiguous)

	if !reflect.DeepEqual(result.DepsSatisfied, []string{"blocker"}) {
		t.Fatalf("DepsSatisfied = %v, want [blocker]", result.DepsSatisfied)
	}

	blocked := result.Blocked["task-a"]
	if len(blocked) != 4 {
		t.Fatalf("Blocked[task-a] has %d entries, want 4: %v", len(blocked), blocked)
	}

	// Sorted by DependencyID: ambig, blocker, external, unknown
	wantBlocked := []BlockDetail{
		{DependencyID: "ambig", Reason: BlockedByAmbiguous},
		{DependencyID: "blocker", Reason: BlockedByWaiting},
		{DependencyID: "external", Reason: BlockedByExternal},
		{DependencyID: "unknown", Reason: BlockedByUnknown},
	}
	if !reflect.DeepEqual(blocked, wantBlocked) {
		t.Fatalf("Blocked[task-a] = %v, want %v", blocked, wantBlocked)
	}
}

func TestAnalyze_MultipleCycles(t *testing.T) {
	// Two independent cycles: A <-> B and C <-> D.
	waiting := []Node{
		{ID: "A", DependsOn: []string{"B"}},
		{ID: "B", DependsOn: []string{"A"}},
		{ID: "C", DependsOn: []string{"D"}},
		{ID: "D", DependsOn: []string{"C"}},
	}
	known := map[string]struct{}{"A": {}, "B": {}, "C": {}, "D": {}}

	result := Analyze(waiting, nil, known, nil)

	wantCycles := [][]string{{"A", "B"}, {"C", "D"}}
	if !reflect.DeepEqual(result.Cycles, wantCycles) {
		t.Fatalf("Cycles = %v, want %v", result.Cycles, wantCycles)
	}
	if len(result.DepsSatisfied) != 0 {
		t.Fatalf("DepsSatisfied = %v, want empty", result.DepsSatisfied)
	}
	if len(result.Blocked) != 0 {
		t.Fatalf("Blocked = %v, want empty", result.Blocked)
	}
}

func TestAnalyze_MixedSatisfiedAndBlocked(t *testing.T) {
	// A has no deps (satisfied), B depends on completed C, D depends on unknown.
	waiting := []Node{
		{ID: "A"},
		{ID: "B", DependsOn: []string{"C"}},
		{ID: "D", DependsOn: []string{"missing"}},
	}
	completed := map[string]struct{}{"C": {}}
	known := map[string]struct{}{"A": {}, "B": {}, "C": {}, "D": {}}

	result := Analyze(waiting, completed, known, nil)

	wantSatisfied := []string{"A", "B"}
	if !reflect.DeepEqual(result.DepsSatisfied, wantSatisfied) {
		t.Fatalf("DepsSatisfied = %v, want %v", result.DepsSatisfied, wantSatisfied)
	}
	wantBlocked := map[string][]BlockDetail{
		"D": {{DependencyID: "missing", Reason: BlockedByUnknown}},
	}
	if !reflect.DeepEqual(result.Blocked, wantBlocked) {
		t.Fatalf("Blocked = %v, want %v", result.Blocked, wantBlocked)
	}
}

func TestAnalyze_CompletedDepSatisfies(t *testing.T) {
	waiting := []Node{
		{ID: "task-b", DependsOn: []string{"task-a"}},
	}
	completed := map[string]struct{}{"task-a": {}}
	known := map[string]struct{}{"task-a": {}, "task-b": {}}

	result := Analyze(waiting, completed, known, nil)

	if !reflect.DeepEqual(result.DepsSatisfied, []string{"task-b"}) {
		t.Fatalf("DepsSatisfied = %v, want [task-b]", result.DepsSatisfied)
	}
}

func TestAnalyze_CompletedAndWaitingOverlapIsAmbiguous(t *testing.T) {
	// When a dependency ID exists in both completed/ and waiting/
	// (and is therefore in ambiguousIDs), it should be classified as
	// BlockedByAmbiguous rather than BlockedByWaiting.
	waiting := []Node{
		{ID: "shared-id"}, // a waiting task with the same ID as a completed one
		{ID: "downstream", DependsOn: []string{"shared-id"}}, // depends on the ambiguous ID
	}
	// The caller (DiagnoseDependencies) removes ambiguous IDs from completedIDs.
	completed := map[string]struct{}{}
	known := map[string]struct{}{"shared-id": {}, "downstream": {}}
	ambiguous := map[string]struct{}{"shared-id": {}}

	result := Analyze(waiting, completed, known, ambiguous)

	// "shared-id" has no deps so it appears deps-satisfied (its own ambiguity
	// doesn't block itself — only dependents are blocked).
	if !reflect.DeepEqual(result.DepsSatisfied, []string{"shared-id"}) {
		t.Fatalf("DepsSatisfied = %v, want [shared-id]", result.DepsSatisfied)
	}

	// "downstream" should be blocked by ambiguous, NOT by waiting.
	blocked := result.Blocked["downstream"]
	if len(blocked) != 1 {
		t.Fatalf("Blocked[downstream] has %d entries, want 1: %v", len(blocked), blocked)
	}
	if blocked[0].Reason != BlockedByAmbiguous {
		t.Fatalf("Blocked[downstream][0].Reason = %v, want BlockedByAmbiguous", blocked[0].Reason)
	}
	if blocked[0].DependencyID != "shared-id" {
		t.Fatalf("Blocked[downstream][0].DependencyID = %q, want %q", blocked[0].DependencyID, "shared-id")
	}
}
