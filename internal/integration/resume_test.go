package integration_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/git"
	"mato/internal/queue"
	"mato/internal/runner"
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
