package dirs

import "testing"

func TestAllContainsExpectedDirs(t *testing.T) {
	expected := map[string]bool{
		Waiting:     false,
		Backlog:     false,
		InProgress:  false,
		ReadyReview: false,
		ReadyMerge:  false,
		Completed:   false,
		Failed:      false,
	}

	for _, d := range All {
		if _, ok := expected[d]; !ok {
			t.Errorf("unexpected directory in All: %s", d)
		}
		expected[d] = true
	}
	for name, seen := range expected {
		if !seen {
			t.Errorf("expected directory %s missing from All", name)
		}
	}
}

func TestLocksConstant(t *testing.T) {
	if Locks != ".locks" {
		t.Errorf("Locks = %q, want %q", Locks, ".locks")
	}
}
