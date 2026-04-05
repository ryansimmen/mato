package integration_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/queue"
	"mato/internal/runner"
	"mato/internal/runtimedata"
	"mato/internal/testutil"
)

func TestResumeWorkAfterReviewRejection_ReusesBranchAndBranchContents(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "resume-retry.md"
	branch := "task/resume-retry"
	taskContent := strings.Join([]string{
		"<!-- branch: " + branch + " -->",
		"# Resume Retry",
		"Implement the follow-up changes.",
		"",
	}, "\n")
	writeTask(t, tasksDir, queue.DirBacklog, taskFile, taskContent)

	createTaskBranch(t, repoRoot, branch, map[string]string{"resume.txt": "from previous attempt\n"}, "previous attempt")

	firstClaim, err := queue.SelectAndClaimTask(tasksDir, "work-agent-1", []string{taskFile}, 0, nil)
	if err != nil {
		t.Fatalf("first SelectAndClaimTask: %v", err)
	}
	if firstClaim == nil {
		t.Fatal("expected first claimed task, got nil")
	}
	if firstClaim.Branch != branch {
		t.Fatalf("first claim branch = %q, want %q", firstClaim.Branch, branch)
	}
	if !firstClaim.HadRecordedBranchMark {
		t.Fatal("first claim should recognize pre-recorded branch marker")
	}

	readyReviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if err := queue.WriteBranchMarker(firstClaim.TaskPath, branch); err != nil {
		t.Fatalf("WriteBranchMarker: %v", err)
	}
	mustRename(t, firstClaim.TaskPath, readyReviewPath)

	writeVerdict(t, tasksDir, taskFile, map[string]string{
		"verdict": "reject",
		"reason":  "please keep prior branch work and add tests",
	})
	runner.PostReviewAction(tasksDir, "review-host", &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   branch,
		Title:    "Resume Retry",
		TaskPath: readyReviewPath,
	})

	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	mustExist(t, backlogPath)
	backlogData := readFile(t, backlogPath)
	if !strings.Contains(backlogData, "<!-- branch: "+branch+" -->") {
		t.Fatalf("backlog task should retain branch marker after rejection:\n%s", backlogData)
	}
	if !strings.Contains(backlogData, "<!-- review-rejection:") {
		t.Fatalf("backlog task should include review rejection marker:\n%s", backlogData)
	}

	secondClaim, err := queue.SelectAndClaimTask(tasksDir, "work-agent-2", []string{taskFile}, time.Second, nil)
	if err != nil {
		t.Fatalf("second SelectAndClaimTask: %v", err)
	}
	if secondClaim == nil {
		t.Fatal("expected second claimed task, got nil")
	}
	if secondClaim.Branch != branch {
		t.Fatalf("second claim branch = %q, want %q", secondClaim.Branch, branch)
	}
	if !secondClaim.HadRecordedBranchMark {
		t.Fatal("second claim should preserve recorded branch marker state")
	}

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	defer git.RemoveClone(cloneDir)

	result, err := git.EnsureBranch(cloneDir, secondClaim.Branch)
	if err != nil {
		t.Fatalf("git.EnsureBranch: %v", err)
	}
	if result.Source != git.BranchSourceRemote {
		t.Fatalf("EnsureBranch source = %q, want %q", result.Source, git.BranchSourceRemote)
	}
	contents := strings.TrimSpace(mustGitOutput(t, cloneDir, "show", "HEAD:resume.txt"))
	if contents != "from previous attempt" {
		t.Fatalf("resumed branch contents = %q, want %q", contents, "from previous attempt")
	}
	branchMarker := readFile(t, secondClaim.TaskPath)
	if strings.Count(branchMarker, "<!-- branch:") != 1 {
		t.Fatalf("expected one branch marker after reclaim, got:\n%s", branchMarker)
	}
}

func TestReviewApprovalThenMerge_CleansTaskState(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "cleanup-taskstate.md"
	branch := "task/cleanup-taskstate"
	writeTask(t, tasksDir, queue.DirReadyReview, taskFile, strings.Join([]string{
		"<!-- branch: " + branch + " -->",
		"# Cleanup Taskstate",
		"Review and merge this task.",
		"",
	}, "\n"))
	createTaskBranch(t, repoRoot, branch, map[string]string{"cleanup.txt": "hello\n"}, "cleanup taskstate")
	if err := runtimedata.UpdateTaskState(tasksDir, taskFile, func(state *runtimedata.TaskState) {
		state.LastOutcome = runtimedata.OutcomeReviewLaunched
		state.TaskBranch = branch
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	writeVerdict(t, tasksDir, taskFile, map[string]string{"verdict": "approve"})
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	runner.PostReviewAction(tasksDir, "review-host", &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   branch,
		Title:    "Cleanup Taskstate",
		TaskPath: reviewPath,
	})
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load after approval: %v", err)
	}
	if state == nil || state.LastOutcome != runtimedata.OutcomeReviewApproved {
		t.Fatalf("taskstate after approval = %+v, want LastOutcome=%s", state, runtimedata.OutcomeReviewApproved)
	}
	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}
	state, err = runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load after merge: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should be removed after merge, got %+v", state)
	}
}

// TestSessionIDRotation_BranchDisambiguationRotatesSessionID exercises the
// full task lifecycle through claim → work-session → push → review-session →
// reject → branch-collision → reclaim-with-disambiguation, verifying that both
// work and review Copilot session IDs are rotated when the queue produces a
// disambiguated branch name. This complements the runner-level tests
// TestRunOnce_BranchChangeRotatesWorkSessionID and
// TestRunReview_BranchChangeRotatesReviewSessionID which verify the --resume
// docker arg directly.
func TestSessionIDRotation_BranchDisambiguationRotatesSessionID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "branch-rotate.md"

	writeTask(t, tasksDir, queue.DirBacklog, taskFile, strings.Join([]string{
		"# Branch Rotate",
		"Test session ID rotation on branch disambiguation.",
		"",
	}, "\n"))

	// --- First work cycle: claim produces branch A ---
	firstClaim, err := queue.SelectAndClaimTask(tasksDir, "agent-1", []string{taskFile}, 0, nil)
	if err != nil {
		t.Fatalf("first SelectAndClaimTask: %v", err)
	}
	if firstClaim == nil {
		t.Fatal("expected first claimed task, got nil")
	}
	branchA := firstClaim.Branch // branch produced by the actual claim flow

	// Runner creates a work session on branch A (as loadOrCreateSession does
	// inside runOnce).
	workA, err := runtimedata.LoadOrCreateSession(tasksDir, runtimedata.KindWork, taskFile, branchA)
	if err != nil {
		t.Fatalf("LoadOrCreate work branchA: %v", err)
	}
	// Runner records head SHA after the agent commits (as recordSessionUpdate does).
	if err := runtimedata.UpdateSession(tasksDir, runtimedata.KindWork, taskFile, func(s *runtimedata.Session) {
		s.TaskBranch = branchA
		s.LastHeadSHA = "abc123"
	}); err != nil {
		t.Fatalf("Update work session: %v", err)
	}

	// Agent pushes work on branch A; host moves task to ready-for-review.
	createTaskBranch(t, repoRoot, branchA, map[string]string{"rotate.txt": "v1\n"}, "first attempt")
	if err := queue.WriteBranchMarker(firstClaim.TaskPath, branchA); err != nil {
		t.Fatalf("WriteBranchMarker: %v", err)
	}
	readyReviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	mustRename(t, firstClaim.TaskPath, readyReviewPath)

	// --- Review cycle on branch A ---
	// Runner creates a review session on branch A (as loadOrCreateSession does
	// inside runReview with reviewSessionResumeEnabled).
	reviewA, err := runtimedata.LoadOrCreateSession(tasksDir, runtimedata.KindReview, taskFile, branchA)
	if err != nil {
		t.Fatalf("LoadOrCreate review branchA: %v", err)
	}
	if reviewA.CopilotSessionID == workA.CopilotSessionID {
		t.Fatal("work and review sessions should have independent IDs")
	}

	// Reviewer rejects; PostReviewAction moves the task back to backlog.
	writeVerdict(t, tasksDir, taskFile, map[string]string{
		"verdict": "reject",
		"reason":  "needs more tests",
	})
	runner.PostReviewAction(tasksDir, "review-host", &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   branchA,
		Title:    "Branch Rotate",
		TaskPath: readyReviewPath,
	})
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, taskFile))

	// --- Force a branch collision ---
	// Place a different task in in-progress/ that occupies branch A. When
	// branch-rotate.md is reclaimed, chooseClaimBranch sees the recorded
	// branch marker (branch A) is taken and falls through to a disambiguated
	// branch name.
	collidingTask := "collider.md"
	writeTask(t, tasksDir, queue.DirInProgress, collidingTask, strings.Join([]string{
		"<!-- branch: " + branchA + " -->",
		"<!-- claimed-by: agent-collider  claimed-at: 2026-01-01T00:00:00Z -->",
		"# Collider",
		"This task occupies branch A to force disambiguation.",
		"",
	}, "\n"))

	// --- Second work cycle: claim triggers disambiguation ---
	secondClaim, err := queue.SelectAndClaimTask(tasksDir, "agent-2", []string{taskFile}, time.Second, nil)
	if err != nil {
		t.Fatalf("second SelectAndClaimTask: %v", err)
	}
	if secondClaim == nil {
		t.Fatal("expected second claimed task, got nil")
	}

	// The queue must have produced a different branch due to disambiguation.
	if secondClaim.Branch == branchA {
		t.Fatalf("expected disambiguated branch, got same branch %q", branchA)
	}
	if !strings.HasPrefix(secondClaim.Branch, "task/branch-rotate") {
		t.Fatalf("disambiguated branch should have task/branch-rotate prefix, got %q", secondClaim.Branch)
	}

	// Runner calls LoadOrCreate with the disambiguated branch (as
	// loadOrCreateSession does inside runOnce). The returned
	// CopilotSessionID is what the runner would pass via --resume.
	workB, err := runtimedata.LoadOrCreateSession(tasksDir, runtimedata.KindWork, taskFile, secondClaim.Branch)
	if err != nil {
		t.Fatalf("LoadOrCreate work disambiguated branch: %v", err)
	}

	// Work session ID must have been rotated — a stale ID here would cause
	// --resume to resume the wrong conversation context on the new branch.
	if workB.CopilotSessionID == workA.CopilotSessionID {
		t.Fatalf("work session ID should rotate after branch disambiguation, got same ID %q", workA.CopilotSessionID)
	}
	if workB.TaskBranch != secondClaim.Branch {
		t.Fatalf("TaskBranch = %q, want %q", workB.TaskBranch, secondClaim.Branch)
	}
	// Durable metadata should survive the branch change.
	if workB.LastHeadSHA != "abc123" {
		t.Fatalf("LastHeadSHA = %q, want preserved %q", workB.LastHeadSHA, "abc123")
	}

	// Same-branch reload must reuse the rotated session ID (no spurious rotation).
	workB2, err := runtimedata.LoadOrCreateSession(tasksDir, runtimedata.KindWork, taskFile, secondClaim.Branch)
	if err != nil {
		t.Fatalf("LoadOrCreate work disambiguated branch again: %v", err)
	}
	if workB2.CopilotSessionID != workB.CopilotSessionID {
		t.Fatalf("same-branch session ID changed: %q → %q", workB.CopilotSessionID, workB2.CopilotSessionID)
	}

	// Review session ID must also rotate when loaded for the disambiguated branch.
	reviewB, err := runtimedata.LoadOrCreateSession(tasksDir, runtimedata.KindReview, taskFile, secondClaim.Branch)
	if err != nil {
		t.Fatalf("LoadOrCreate review disambiguated branch: %v", err)
	}
	if reviewB.CopilotSessionID == reviewA.CopilotSessionID {
		t.Fatalf("review session ID should rotate after branch disambiguation, got same ID %q", reviewA.CopilotSessionID)
	}
	if reviewB.TaskBranch != secondClaim.Branch {
		t.Fatalf("review TaskBranch = %q, want %q", reviewB.TaskBranch, secondClaim.Branch)
	}
}

func TestTerminalCleanup_RemovesSessionMetadata(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "cleanup-sessionmeta.md"
	branch := "task/cleanup-sessionmeta"
	writeTask(t, tasksDir, queue.DirReadyReview, taskFile, strings.Join([]string{
		"<!-- branch: " + branch + " -->",
		"# Cleanup Sessionmeta",
		"Review and merge this task.",
		"",
	}, "\n"))
	createTaskBranch(t, repoRoot, branch, map[string]string{"cleanup.txt": "hello\n"}, "cleanup sessionmeta")
	for _, kind := range []string{runtimedata.KindWork, runtimedata.KindReview} {
		if _, err := runtimedata.LoadOrCreateSession(tasksDir, kind, taskFile, branch); err != nil {
			t.Fatalf("seed %s session: %v", kind, err)
		}
	}

	writeVerdict(t, tasksDir, taskFile, map[string]string{"verdict": "approve"})
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	runner.PostReviewAction(tasksDir, "review-host", &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   branch,
		Title:    "Cleanup Sessionmeta",
		TaskPath: reviewPath,
	})
	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}
	for _, kind := range []string{runtimedata.KindWork, runtimedata.KindReview} {
		session, err := runtimedata.LoadSession(tasksDir, kind, taskFile)
		if err != nil {
			t.Fatalf("Load %s session after merge: %v", kind, err)
		}
		if session != nil {
			t.Fatalf("%s session should be removed after merge, got %+v", kind, session)
		}
	}
}
