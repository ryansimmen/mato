package queueview

import (
	"reflect"
	"testing"

	"github.com/ryansimmen/mato/internal/dirs"
)

func TestComputeRunnableBacklogView_ExcludesDeferredTasks(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "alpha.md", `---
priority: 10
affects:
  - shared.go
---
# Alpha
`)
	writeTask(t, tasksDir, dirs.Backlog, "bravo.md", `---
priority: 20
affects:
  - shared.go
---
# Bravo
`)
	writeTask(t, tasksDir, dirs.Backlog, "charlie.md", `---
priority: 30
affects:
  - unique.go
---
# Charlie
`)

	view := ComputeRunnableBacklogView(tasksDir, BuildIndex(tasksDir))

	if got, want := runnableBacklogFilenames(view), []string{"alpha.md", "charlie.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Runnable = %v, want %v", got, want)
	}

	if got, want := view.Deferred["bravo.md"], (DeferralInfo{
		BlockedBy:          "alpha.md",
		BlockedByDir:       dirs.Backlog,
		ConflictingAffects: []string{"shared.go"},
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Deferred[bravo.md] = %#v, want %#v", got, want)
	}

	if len(view.DependencyBlocked) != 0 {
		t.Fatalf("DependencyBlocked = %v, want empty", view.DependencyBlocked)
	}
}

func TestComputeRunnableBacklogView_ExcludesDependencyBlockedTasks(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Failed, "setup-db.md", `---
id: setup-db
priority: 5
---
# Setup DB
`)
	writeTask(t, tasksDir, dirs.Backlog, "blocked.md", `---
priority: 10
depends_on:
  - setup-db
---
# Blocked
`)
	writeTask(t, tasksDir, dirs.Backlog, "runnable.md", `---
priority: 20
affects:
  - runnable.go
---
# Runnable
`)

	view := ComputeRunnableBacklogView(tasksDir, BuildIndex(tasksDir))

	if got, want := runnableBacklogFilenames(view), []string{"runnable.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Runnable = %v, want %v", got, want)
	}

	if got, want := view.DependencyBlocked["blocked.md"], []DependencyBlock{{DependencyID: "setup-db", State: dirs.Failed}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DependencyBlocked[blocked.md] = %#v, want %#v", got, want)
	}

	if len(view.Deferred) != 0 {
		t.Fatalf("Deferred = %v, want empty", view.Deferred)
	}
}

func TestComputeRunnableBacklogView_OrdersRunnableByPriorityThenFilename(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Backlog, "zeta.md", `---
priority: 10
affects:
  - zeta.go
---
# Zeta
`)
	writeTask(t, tasksDir, dirs.Backlog, "alpha.md", `---
priority: 10
affects:
  - alpha.go
---
# Alpha
`)
	writeTask(t, tasksDir, dirs.Backlog, "beta.md", `---
priority: 5
affects:
  - beta.go
---
# Beta
`)

	view := ComputeRunnableBacklogView(tasksDir, BuildIndex(tasksDir))

	if got, want := runnableBacklogFilenames(view), []string{"beta.md", "alpha.md", "zeta.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Runnable = %v, want %v", got, want)
	}

	if len(view.Deferred) != 0 {
		t.Fatalf("Deferred = %v, want empty", view.Deferred)
	}
	if len(view.DependencyBlocked) != 0 {
		t.Fatalf("DependencyBlocked = %v, want empty", view.DependencyBlocked)
	}
}

func TestComputeRunnableBacklogView_AggregatesDependencyStates(t *testing.T) {
	tasksDir := setupIndexDirs(t)
	writeTask(t, tasksDir, dirs.Failed, "dep-failed.md", `---
id: shared-dep
priority: 5
---
# Failed dependency
`)
	writeTask(t, tasksDir, dirs.ReadyReview, "dep-review.md", `---
id: shared-dep
priority: 5
---
# Review dependency
`)
	writeTask(t, tasksDir, dirs.Backlog, "consumer.md", `---
priority: 20
depends_on:
  - shared-dep
---
# Consumer
`)

	view := ComputeRunnableBacklogView(tasksDir, BuildIndex(tasksDir))

	if got := runnableBacklogFilenames(view); len(got) != 0 {
		t.Fatalf("Runnable = %v, want empty", got)
	}

	if got, want := view.DependencyBlocked["consumer.md"], []DependencyBlock{{
		DependencyID: "shared-dep",
		State:        dirs.Failed + "," + dirs.ReadyReview,
	}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DependencyBlocked[consumer.md] = %#v, want %#v", got, want)
	}

	if len(view.Deferred) != 0 {
		t.Fatalf("Deferred = %v, want empty", view.Deferred)
	}
}

func runnableBacklogFilenames(view RunnableBacklogView) []string {
	names := make([]string, 0, len(view.Runnable))
	for _, snap := range view.Runnable {
		names = append(names, snap.Filename)
	}
	return names
}
