package merge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/git"
	"mato/internal/testutil"
)

func TestParseAgentCommitLog(t *testing.T) {
	tests := []struct {
		name        string
		log         string
		wantSubject string
		wantBody    string
	}{
		{
			name:        "empty input",
			log:         "",
			wantSubject: "",
			wantBody:    "",
		},
		{
			name:        "whitespace only",
			log:         "   \n\n  \n",
			wantSubject: "",
			wantBody:    "",
		},
		{
			name:        "subject only",
			log:         "feat: add dark mode",
			wantSubject: "feat: add dark mode",
			wantBody:    "",
		},
		{
			name:        "subject with trailing newline",
			log:         "fix: correct typo\n",
			wantSubject: "fix: correct typo",
			wantBody:    "",
		},
		{
			name:        "subject and body",
			log:         "feat: add caching\n\nAdds Redis-based caching for API responses.",
			wantSubject: "feat: add caching",
			wantBody:    "Adds Redis-based caching for API responses.",
		},
		{
			name:        "subject with leading blank lines",
			log:         "\n\nfeat: add caching\n\nSome body text.",
			wantSubject: "feat: add caching",
			wantBody:    "Some body text.",
		},
		{
			name:        "body with multiple lines",
			log:         "fix: handle edge case\n\nLine one.\nLine two.\nLine three.",
			wantSubject: "fix: handle edge case",
			wantBody:    "Line one.\nLine two.\nLine three.",
		},
		{
			name: "filters Task: line from body",
			log:  "feat: implement search\n\nAdds full-text search.\n\nTask: implement-search.md",
			wantSubject: "feat: implement search",
			wantBody:    "Adds full-text search.",
		},
		{
			name: "filters Changed files: section",
			log: "feat: add auth\n\nJWT-based authentication.\n\nChanged files:\nsrc/auth.go\nsrc/auth_test.go\n",
			wantSubject: "feat: add auth",
			wantBody:    "JWT-based authentication.",
		},
		{
			name: "filters both Task: and Changed files:",
			log: "fix: race condition\n\nFixed locking issue.\n\nTask: fix-race.md\n\nChanged files:\nqueue.go\n",
			wantSubject: "fix: race condition",
			wantBody:    "Fixed locking issue.",
		},
		{
			name:        "no body, just subject and blank line",
			log:         "docs: update readme\n\n",
			wantSubject: "docs: update readme",
			wantBody:    "",
		},
		{
			name: "multi-commit log uses only first commit",
			log:  "feat: primary change\n\nPrimary body.\n\nChanged files:\nfile1.go\n\nfeat: secondary change\n\nSecondary body.",
			wantSubject: "feat: primary change",
			wantBody:    "Primary body.",
		},
		{
			name:        "body with trailing blank lines trimmed",
			log:         "fix: cleanup\n\nSome explanation.\n\n\n",
			wantSubject: "fix: cleanup",
			wantBody:    "Some explanation.",
		},
		{
			name: "body with Co-authored-by trailer preserved",
			log:  "feat: add feature\n\nImplementation details.\n\nCo-authored-by: Bot <bot@example.com>",
			wantSubject: "feat: add feature",
			wantBody:    "Implementation details.\n\nCo-authored-by: Bot <bot@example.com>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSubject, gotBody := parseAgentCommitLog(tt.log)
			if gotSubject != tt.wantSubject {
				t.Errorf("subject = %q, want %q", gotSubject, tt.wantSubject)
			}
			if gotBody != tt.wantBody {
				t.Errorf("body = %q, want %q", gotBody, tt.wantBody)
			}
		})
	}
}

func TestFormatSquashCommitMessage(t *testing.T) {
	tests := []struct {
		name     string
		task     mergeQueueTask
		agentLog string
		want     string
	}{
		{
			name:     "agent log with subject only, no trailers",
			task:     mergeQueueTask{title: "Add dark mode"},
			agentLog: "feat: add dark mode support",
			want:     "feat: add dark mode support",
		},
		{
			name:     "empty agent log falls back to task title",
			task:     mergeQueueTask{title: "Add dark mode"},
			agentLog: "",
			want:     "Add dark mode",
		},
		{
			name:     "agent log with body and no trailers",
			task:     mergeQueueTask{title: "Fix bug"},
			agentLog: "fix: correct null pointer\n\nHandles nil receiver in Process().",
			want:     "fix: correct null pointer\n\nHandles nil receiver in Process().",
		},
		{
			name: "task ID trailer appended",
			task: mergeQueueTask{
				title: "Add caching",
				id:    "add-caching",
			},
			agentLog: "feat: add caching",
			want:     "feat: add caching\n\nTask-ID: add-caching",
		},
		{
			name: "affects trailer appended",
			task: mergeQueueTask{
				title:   "Fix auth",
				affects: []string{"internal/auth/auth.go", "internal/auth/auth_test.go"},
			},
			agentLog: "fix: auth token expiry",
			want:     "fix: auth token expiry\n\nAffects: internal/auth/auth.go, internal/auth/auth_test.go",
		},
		{
			name: "both trailers appended",
			task: mergeQueueTask{
				title:   "Refactor queue",
				id:      "refactor-queue",
				affects: []string{"internal/queue/queue.go"},
			},
			agentLog: "refactor: simplify queue logic",
			want:     "refactor: simplify queue logic\n\nTask-ID: refactor-queue\nAffects: internal/queue/queue.go",
		},
		{
			name: "agent log with body and trailers",
			task: mergeQueueTask{
				title: "Add feature",
				id:    "add-feature",
			},
			agentLog: "feat: new feature\n\nDetailed explanation of the change.",
			want:     "feat: new feature\n\nDetailed explanation of the change.\n\nTask-ID: add-feature",
		},
		{
			name: "whitespace-only agent log falls back to title with trailers",
			task: mergeQueueTask{
				title: "Fix tests",
				id:    "fix-tests",
			},
			agentLog: "  \n\n  ",
			want:     "Fix tests\n\nTask-ID: fix-tests",
		},
		{
			name: "agent log with Task: line filtered before formatting",
			task: mergeQueueTask{
				title: "Update docs",
				id:    "update-docs",
			},
			agentLog: "docs: update architecture\n\nUpdated diagrams.\n\nTask: update-docs.md",
			want:     "docs: update architecture\n\nUpdated diagrams.\n\nTask-ID: update-docs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSquashCommitMessage(tt.task, tt.agentLog)
			if got != tt.want {
				t.Errorf("got:\n%s\n\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestTaskBranchName(t *testing.T) {
	tests := []struct {
		name string
		task mergeQueueTask
		want string
	}{
		{
			name: "uses branch field when set",
			task: mergeQueueTask{branch: "task/my-feature", name: "my-feature.md"},
			want: "task/my-feature",
		},
		{
			name: "falls back to sanitized name",
			task: mergeQueueTask{name: "add dark mode.md"},
			want: "task/add-dark-mode",
		},
		{
			name: "empty branch uses name",
			task: mergeQueueTask{name: "fix-bug.md"},
			want: "task/fix-bug",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := taskBranchName(tt.task)
			if !strings.HasPrefix(got, "task/") {
				t.Errorf("expected branch to start with 'task/', got %q", got)
			}
			if tt.task.branch != "" && got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// setupMergeRepo creates a temporary git repo with a "mato" target branch,
// configures it to accept pushes (receive.denyCurrentBranch=updateInstead),
// and returns the repo root. This repo acts as both the local and the "origin"
// for mergeReadyTask's internal clone.
func setupMergeRepo(t *testing.T) string {
	t.Helper()
	repoRoot := testutil.SetupRepo(t)
	if _, err := git.Output(repoRoot, "checkout", "-b", "mato"); err != nil {
		t.Fatalf("git checkout -b mato: %v", err)
	}
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch: %v", err)
	}
	return repoRoot
}

// createTaskBranch creates a task branch from the target branch with a single
// file commit. Returns the branch name used.
func createTaskBranch(t *testing.T, repoRoot, branchName, fileName, content, commitMsg string) {
	t.Helper()
	if _, err := git.Output(repoRoot, "checkout", "-b", branchName, "mato"); err != nil {
		t.Fatalf("git checkout -b %s: %v", branchName, err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, fileName), []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile %s: %v", fileName, err)
	}
	if _, err := git.Output(repoRoot, "add", fileName); err != nil {
		t.Fatalf("git add %s: %v", fileName, err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", commitMsg); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}
}

func TestMergeReadyTask_CleanMerge(t *testing.T) {
	repoRoot := setupMergeRepo(t)
	createTaskBranch(t, repoRoot, "task/clean-merge", "feature.txt", "new feature\n", "feat: add feature")

	task := mergeQueueTask{
		name:   "clean-merge.md",
		branch: "task/clean-merge",
		title:  "Clean merge task",
		id:     "clean-merge",
		affects: []string{"feature.txt"},
	}

	result, err := mergeReadyTask(repoRoot, "mato", task)
	if err != nil {
		t.Fatalf("mergeReadyTask() error = %v", err)
	}
	if result == nil {
		t.Fatal("mergeReadyTask() returned nil result for clean merge")
	}
	if result.commitSHA == "" {
		t.Error("mergeReadyTask() commitSHA is empty")
	}
	if len(result.filesChanged) == 0 {
		t.Error("mergeReadyTask() filesChanged is empty")
	}

	foundFeature := false
	for _, f := range result.filesChanged {
		if f == "feature.txt" {
			foundFeature = true
			break
		}
	}
	if !foundFeature {
		t.Errorf("filesChanged = %v, want to contain feature.txt", result.filesChanged)
	}

	// Verify the merge actually landed on the target branch.
	if _, err := os.Stat(filepath.Join(repoRoot, "feature.txt")); err != nil {
		t.Errorf("feature.txt should exist on target branch after merge: %v", err)
	}
}

func TestMergeReadyTask_MergeConflict(t *testing.T) {
	repoRoot := setupMergeRepo(t)

	// Create a task branch that modifies README.md.
	createTaskBranch(t, repoRoot, "task/conflict", "README.md", "task version\n", "feat: task changes")

	// Create a conflicting change on the target branch.
	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("mato version\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile README.md: %v", err)
	}
	if _, err := git.Output(repoRoot, "add", "README.md"); err != nil {
		t.Fatalf("git add README.md: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "conflicting change on mato"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	task := mergeQueueTask{
		name:   "conflict.md",
		branch: "task/conflict",
		title:  "Conflicting task",
	}

	result, err := mergeReadyTask(repoRoot, "mato", task)
	if err == nil {
		t.Fatal("mergeReadyTask() expected error for merge conflict")
	}
	if !errors.Is(err, errSquashMergeConflict) {
		t.Errorf("error = %v, want errSquashMergeConflict", err)
	}
	if result != nil {
		t.Errorf("result should be nil on conflict, got %+v", result)
	}
}

func TestMergeReadyTask_AlreadyMerged(t *testing.T) {
	repoRoot := setupMergeRepo(t)
	createTaskBranch(t, repoRoot, "task/already-merged", "already.txt", "content\n", "feat: already merged work")

	task := mergeQueueTask{
		name:   "already-merged.md",
		branch: "task/already-merged",
		title:  "Already merged task",
		id:     "already-merged",
	}

	// First merge succeeds normally.
	result1, err := mergeReadyTask(repoRoot, "mato", task)
	if err != nil {
		t.Fatalf("first mergeReadyTask() error = %v", err)
	}
	if result1 == nil {
		t.Fatal("first mergeReadyTask() returned nil result")
	}

	// Second merge of the same branch should detect already-merged state.
	// The squash produces no staged changes → returns recovery metadata.
	result2, err := mergeReadyTask(repoRoot, "mato", task)
	if err != nil {
		t.Fatalf("second mergeReadyTask() error = %v, want nil for already-merged", err)
	}
	if result2 == nil {
		t.Fatal("second mergeReadyTask() returned nil result for already-merged")
	}
	if result2.commitSHA == "" {
		t.Error("already-merged result should have commitSHA")
	}
}

func TestMergeReadyTask_MissingTaskBranch(t *testing.T) {
	repoRoot := setupMergeRepo(t)

	task := mergeQueueTask{
		name:   "missing-branch.md",
		branch: "task/nonexistent",
		title:  "Missing branch task",
	}

	result, err := mergeReadyTask(repoRoot, "mato", task)
	if err == nil {
		t.Fatal("mergeReadyTask() expected error for missing task branch")
	}
	if !errors.Is(err, errTaskBranchNotPushed) {
		t.Errorf("error = %v, want errTaskBranchNotPushed", err)
	}
	if result != nil {
		t.Errorf("result should be nil for missing branch, got %+v", result)
	}
}

func TestMergeReadyTask_PushFailure(t *testing.T) {
	repoRoot := setupMergeRepo(t)
	createTaskBranch(t, repoRoot, "task/push-fail", "pushfail.txt", "content\n", "feat: push fail test")

	// Reconfigure the origin to refuse pushes.
	if _, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "refuse"); err != nil {
		t.Fatalf("git config receive.denyCurrentBranch refuse: %v", err)
	}

	task := mergeQueueTask{
		name:   "push-fail.md",
		branch: "task/push-fail",
		title:  "Push failure task",
	}

	result, err := mergeReadyTask(repoRoot, "mato", task)
	if err == nil {
		t.Fatal("mergeReadyTask() expected error for push failure")
	}
	if !errors.Is(err, errPushAfterSquashFailed) {
		t.Errorf("error = %v, want errPushAfterSquashFailed", err)
	}
	if result != nil {
		t.Errorf("result should be nil on push failure, got %+v", result)
	}

	// Target branch should not have the file since push was refused.
	if _, err := os.Stat(filepath.Join(repoRoot, "pushfail.txt")); !os.IsNotExist(err) {
		t.Error("pushfail.txt should not exist on target branch after refused push")
	}
}

func TestMergeReadyTask_SquashCommitMessageTrailers(t *testing.T) {
	repoRoot := setupMergeRepo(t)
	createTaskBranch(t, repoRoot, "task/with-trailers", "trailer.txt", "content\n", "feat: add trailers feature")

	task := mergeQueueTask{
		name:    "with-trailers.md",
		branch:  "task/with-trailers",
		title:   "With trailers task",
		id:      "with-trailers",
		affects: []string{"trailer.txt", "other.go"},
	}

	result, err := mergeReadyTask(repoRoot, "mato", task)
	if err != nil {
		t.Fatalf("mergeReadyTask() error = %v", err)
	}
	if result == nil {
		t.Fatal("mergeReadyTask() returned nil result")
	}

	// Read the squash commit message from the target branch.
	commitMsg, err := git.Output(repoRoot, "log", "-1", "--format=%B", "mato")
	if err != nil {
		t.Fatalf("git log: %v", err)
	}

	if !strings.Contains(commitMsg, "Task-ID: with-trailers") {
		t.Errorf("commit message missing Task-ID trailer:\n%s", commitMsg)
	}
	if !strings.Contains(commitMsg, "Affects: trailer.txt, other.go") {
		t.Errorf("commit message missing Affects trailer:\n%s", commitMsg)
	}
	// The subject should come from the agent's commit, not the task title.
	if !strings.Contains(commitMsg, "feat: add trailers feature") {
		t.Errorf("commit message should use agent commit subject:\n%s", commitMsg)
	}
}

func TestMergeReadyTask_MultipleFilesChanged(t *testing.T) {
	repoRoot := setupMergeRepo(t)

	// Create a task branch with multiple file changes.
	if _, err := git.Output(repoRoot, "checkout", "-b", "task/multi-file", "mato"); err != nil {
		t.Fatalf("git checkout: %v", err)
	}
	for _, name := range []string{"file1.txt", "file2.txt", "file3.txt"} {
		if err := os.WriteFile(filepath.Join(repoRoot, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("os.WriteFile %s: %v", name, err)
		}
	}
	if _, err := git.Output(repoRoot, "add", "-A"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(repoRoot, "commit", "-m", "feat: add multiple files"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	if _, err := git.Output(repoRoot, "checkout", "mato"); err != nil {
		t.Fatalf("git checkout mato: %v", err)
	}

	task := mergeQueueTask{
		name:   "multi-file.md",
		branch: "task/multi-file",
		title:  "Multi file task",
	}

	result, err := mergeReadyTask(repoRoot, "mato", task)
	if err != nil {
		t.Fatalf("mergeReadyTask() error = %v", err)
	}
	if result == nil {
		t.Fatal("mergeReadyTask() returned nil result")
	}

	if len(result.filesChanged) != 3 {
		t.Errorf("filesChanged = %v, want 3 files", result.filesChanged)
	}

	expected := map[string]bool{"file1.txt": false, "file2.txt": false, "file3.txt": false}
	for _, f := range result.filesChanged {
		expected[f] = true
	}
	for name, found := range expected {
		if !found {
			t.Errorf("filesChanged missing %s", name)
		}
	}
}
