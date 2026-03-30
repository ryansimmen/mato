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
	"mato/internal/sessionmeta"
	"mato/internal/taskstate"
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
	if err := taskstate.Update(tasksDir, taskFile, func(state *taskstate.TaskState) {
		state.LastOutcome = "review-launched"
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
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load after approval: %v", err)
	}
	if state == nil || state.LastOutcome != "review-approved" {
		t.Fatalf("taskstate after approval = %+v, want LastOutcome=review-approved", state)
	}
	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}
	state, err = taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load after merge: %v", err)
	}
	if state != nil {
		t.Fatalf("taskstate should be removed after merge, got %+v", state)
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
	for _, kind := range []string{sessionmeta.KindWork, sessionmeta.KindReview} {
		if _, err := sessionmeta.LoadOrCreate(tasksDir, kind, taskFile, branch); err != nil {
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
	for _, kind := range []string{sessionmeta.KindWork, sessionmeta.KindReview} {
		session, err := sessionmeta.Load(tasksDir, kind, taskFile)
		if err != nil {
			t.Fatalf("Load %s session after merge: %v", kind, err)
		}
		if session != nil {
			t.Fatalf("%s session should be removed after merge, got %+v", kind, session)
		}
	}
}
