package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/queue"
	"mato/internal/runtimedata"
	"mato/internal/taskfile"
	"mato/internal/testutil"
	"mato/internal/ui"
)

func assertWorkLaunchTaskState(t *testing.T, tasksDir, taskFile, branch, targetBranch, lastHead string) {
	t.Helper()

	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to be written")
	}
	if state.TaskBranch != branch {
		t.Fatalf("TaskBranch = %q, want %q", state.TaskBranch, branch)
	}
	if state.TargetBranch != targetBranch {
		t.Fatalf("TargetBranch = %q, want %q", state.TargetBranch, targetBranch)
	}
	if state.LastHeadSHA != lastHead {
		t.Fatalf("LastHeadSHA = %q, want %q", state.LastHeadSHA, lastHead)
	}
	if state.LastOutcome != runtimedata.OutcomeWorkLaunched {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeWorkLaunched)
	}
}

func TestMoveTaskToReviewWithMarker_Success(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "marker-task.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Marker Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/marker-task",
		Title:    "Marker Task",
		TaskPath: inProgressPath,
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/marker-task")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Task should be in ready-for-review/.
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("task should be in ready-for-review/: %v", statErr)
	}

	// Task should not be in in-progress/.
	if _, statErr := os.Stat(inProgressPath); statErr == nil {
		t.Fatal("task should not remain in in-progress/")
	}

	// Branch marker should be written.
	data, _ := os.ReadFile(readyPath)
	if !strings.Contains(string(data), "<!-- branch: task/marker-task -->") {
		t.Fatalf("branch marker not found in moved file, got:\n%s", string(data))
	}
}

func TestMoveTaskToReviewWithMarker_SourceMissing(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)

	claimed := &queue.ClaimedTask{
		Filename: "missing.md",
		TaskPath: filepath.Join(tasksDir, dirs.InProgress, "missing.md"),
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/missing")
	if err == nil {
		t.Fatal("expected error when source file doesn't exist")
	}
}

func TestMoveTaskToReviewWithMarker_DestinationExists(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "dup.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Dup\n"), 0o644)

	// Pre-create at destination.
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(readyPath, []byte("# Existing\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		TaskPath: inProgressPath,
	}

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/dup")
	if err == nil {
		t.Fatal("expected error when destination already exists")
	}

	// Source should still exist.
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("source should still exist: %v", statErr)
	}
}

func TestMoveTaskToReviewWithMarker_AppendFailsRollback(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "rollback.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Rollback\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		TaskPath: inProgressPath,
	}

	setHook(t, &writeBranchMarkerFn, func(path, branch string) error {
		return fmt.Errorf("simulated write error")
	})

	err := moveTaskToReviewWithMarker(tasksDir, claimed, "task/rollback")
	if err == nil {
		t.Fatal("expected error when append fails")
	}
	if !strings.Contains(err.Error(), "rolled back to in-progress") {
		t.Fatalf("error should mention rollback, got: %v", err)
	}

	// File should be back in in-progress/.
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("file should be rolled back to in-progress/: %v", statErr)
	}
	// File should NOT be in ready-for-review/.
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatal("file should not remain in ready-for-review/ after rollback")
	}
}

func TestRecoverStuckTask_PushedTaskUsesRecordedBranchMarkerWhenTaskStateBranchMissing(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Backlog, dirs.InProgress, dirs.ReadyReview, "messages", "messages/events", "messages/completions", "messages/presence"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	taskFile := "branch-fallback.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/branch-fallback -->\n# Branch Fallback\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, taskFile, func(state *runtimedata.TaskState) {
		state.TargetBranch = "main"
		state.LastOutcome = runtimedata.OutcomeWorkBranchPushed
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/branch-fallback",
		Title:    "Branch Fallback",
		TaskPath: inProgressPath,
	}

	captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent1", claimed)
	})

	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	data, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatalf("task should be moved to ready-for-review: %v", err)
	}
	if !strings.Contains(string(data), "<!-- branch: task/branch-fallback -->") {
		t.Fatalf("ready-for-review task should keep recovered branch marker, got:\n%s", string(data))
	}
}

func TestRecoverStuckTask_PushedTaskRetryFailureMovesToFailed(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.Failed} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	taskFile := "retry-failure.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/retry-failure -->\n# Retry Failure\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := runtimedata.UpdateTaskState(tasksDir, taskFile, func(state *runtimedata.TaskState) {
		state.TaskBranch = "task/retry-failure"
		state.LastOutcome = runtimedata.OutcomeWorkBranchPushed
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/retry-failure",
		Title:    "Retry Failure",
		TaskPath: inProgressPath,
	}

	setHook(t, &writeBranchMarkerFn, func(path, branch string) error {
		return fmt.Errorf("simulated marker failure")
	})

	_, stderr := captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent1", claimed)
	})

	if !strings.Contains(stderr, "write branch marker") {
		t.Fatalf("expected branch marker warning, got:\n%s", stderr)
	}
	if _, err := os.Stat(inProgressPath); !os.IsNotExist(err) {
		t.Fatalf("task should leave in-progress after retry failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, taskFile)); !os.IsNotExist(err) {
		t.Fatalf("task should not remain in ready-for-review after retry failure: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, dirs.Failed, taskFile))
	if err != nil {
		t.Fatalf("task should be moved to failed/: %v", err)
	}
	if !taskfile.ContainsTerminalFailure(data) {
		t.Fatal("failed task should include terminal-failure marker")
	}
}

func TestMoveTaskToReviewWithMarker_ReplacesDuplicateMarkers(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "duplicate-markers.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent -->\n<!-- branch: task/old -->\n<!-- branch: task/legacy -->\n# Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/new",
		Title:    "Task",
		TaskPath: inProgressPath,
	}

	if err := moveTaskToReviewWithMarker(tasksDir, claimed, claimed.Branch); err != nil {
		t.Fatalf("moveTaskToReviewWithMarker: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, taskFile))
	if err != nil {
		t.Fatalf("read ready task: %v", err)
	}
	if branch := taskfile.ParseBranch(filepath.Join(tasksDir, dirs.ReadyReview, taskFile)); branch != "task/new" {
		t.Fatalf("ParseBranch = %q, want %q", branch, "task/new")
	}
	if strings.Contains(string(data), "<!-- branch: task/old -->") {
		t.Fatalf("old primary branch marker should be replaced:\n%s", string(data))
	}
}

func TestPostAgentPush_TaskAlreadyGone(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task file does NOT exist (already moved).
	claimed := &queue.ClaimedTask{
		Filename: "gone-task.md",
		Branch:   "task/gone-task",
		Title:    "Gone Task",
		TaskPath: filepath.Join(tasksDir, dirs.InProgress, "gone-task.md"),
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	err := postAgentPush(env, "agent1", claimed, t.TempDir(), "deadbeef")
	if err != nil {
		t.Fatalf("expected nil when task is already gone, got: %v", err)
	}
}

func TestPostAgentPush_NoCommits(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "no-commits.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# No Commits\n"), 0o644)

	// Set up git repo with no commits above main.
	cloneDir := t.TempDir()
	gitRun := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = cloneDir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	gitRun("git", "init", "-b", "main")
	gitRun("git", "config", "user.name", "test")
	gitRun("git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	gitRun("git", "add", ".")
	gitRun("git", "commit", "-m", "init")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/no-commits",
		Title:    "No Commits",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	startingTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	err = postAgentPush(env, "agent1", claimed, cloneDir, strings.TrimSpace(startingTip))
	if err != nil {
		t.Fatalf("expected nil when no commits exist, got: %v", err)
	}

	// Task should still be in in-progress/ (not moved).
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatal("task should remain in in-progress/ when no commits made")
	}
}

func TestPostAgentPush_RecordsWorkSessionHead(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	defer git.RemoveClone(cloneDir)

	taskFile := "session-work.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(inProgressPath, []byte("# Session Work\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/session-work",
		Title:    "Session Work",
		TaskPath: inProgressPath,
	}
	if _, err := git.Output(cloneDir, "checkout", "-b", claimed.Branch); err != nil {
		t.Fatalf("checkout branch: %v", err)
	}
	startingTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse starting tip: %v", err)
	}
	startingTip = strings.TrimSpace(startingTip)
	if err := runtimedata.UpdateSession(tasksDir, runtimedata.KindWork, taskFile, func(session *runtimedata.Session) {
		session.TaskBranch = claimed.Branch
	}); err != nil {
		t.Fatalf("seed work session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "work.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile clone change: %v", err)
	}
	if _, err := git.Output(cloneDir, "add", "work.txt"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := git.Output(cloneDir, "commit", "-m", "work session update"); err != nil {
		t.Fatalf("git commit: %v", err)
	}

	env := envConfig{repoRoot: repoRoot, tasksDir: tasksDir, targetBranch: "mato"}
	if err := postAgentPush(env, "agent1", claimed, cloneDir, startingTip); err != nil {
		t.Fatalf("postAgentPush: %v", err)
	}
	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindWork, taskFile)
	if err != nil {
		t.Fatalf("Load work session: %v", err)
	}
	if session == nil {
		t.Fatal("expected work session to exist")
	}
	currentTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse current tip: %v", err)
	}
	if session.LastHeadSHA != strings.TrimSpace(currentTip) {
		t.Fatalf("LastHeadSHA = %q, want %q", session.LastHeadSHA, strings.TrimSpace(currentTip))
	}
}

func TestRunOnce_UsesExistingWorkSessionResumeID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	setHook(t, &createCloneFn, func(string) (string, error) { return cloneDir, nil })
	setHook(t, &removeCloneFn, func(string) {})
	setHook(t, &ensureBranchFn, func(cloneDir, branch string) (git.EnsureBranchResult, error) {
		if _, err := git.Output(cloneDir, "checkout", "-b", branch); err != nil {
			return git.EnsureBranchResult{}, err
		}
		return git.EnsureBranchResult{Branch: branch, Source: git.BranchSourceLocal}, nil
	})
	if err := runtimedata.UpdateSession(tasksDir, runtimedata.KindWork, "task.md", func(session *runtimedata.Session) {
		session.CopilotSessionID = "work-session-123"
		session.TaskBranch = "task/task"
	}); err != nil {
		t.Fatalf("seed work session: %v", err)
	}

	var capturedArgs []string
	setHook(t, &execCommandContext, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		cmd := exec.CommandContext(ctx, "true")
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		return cmd
	})

	taskPath := filepath.Join(tasksDir, dirs.InProgress, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	env := envConfig{
		repoRoot:           repoRoot,
		tasksDir:           tasksDir,
		workdir:            "/workspace",
		copilotPath:        "/bin/true",
		gitPath:            "/usr/bin/git",
		gitUploadPackPath:  "/usr/bin/git-upload-pack",
		gitReceivePackPath: "/usr/bin/git-receive-pack",
		ghPath:             "/bin/true",
		homeDir:            "/home/test",
		copilotConfigDir:   t.TempDir(),
		copilotCacheDir:    t.TempDir(),
		image:              "ubuntu:24.04",
		targetBranch:       "mato",
	}
	run := runContext{agentID: "agent1", prompt: "test prompt", model: "claude-opus-4.6", reasoningEffort: "high", timeout: time.Second}
	claimed := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task", Title: "Task", TaskPath: taskPath}

	if err := runOnce(context.Background(), env, run, claimed); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	joined := strings.Join(capturedArgs, " ")
	if !strings.Contains(joined, "--resume=work-session-123") {
		t.Fatalf("expected work session resume in docker args, got %s", joined)
	}
}

func TestRunOnce_BranchChangeRotatesWorkSessionID(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	setHook(t, &createCloneFn, func(string) (string, error) { return cloneDir, nil })
	setHook(t, &removeCloneFn, func(string) {})
	setHook(t, &ensureBranchFn, func(cloneDir, branch string) (git.EnsureBranchResult, error) {
		if _, err := git.Output(cloneDir, "checkout", "-b", branch); err != nil {
			return git.EnsureBranchResult{}, err
		}
		return git.EnsureBranchResult{Branch: branch, Source: git.BranchSourceLocal}, nil
	})

	// Seed a work session on the OLD branch with a known session ID.
	oldSessionID := "stale-session-from-old-branch"
	if err := runtimedata.UpdateSession(tasksDir, runtimedata.KindWork, "task.md", func(session *runtimedata.Session) {
		session.CopilotSessionID = oldSessionID
		session.TaskBranch = "task/task-old"
	}); err != nil {
		t.Fatalf("seed work session: %v", err)
	}

	var capturedArgs []string
	setHook(t, &execCommandContext, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		capturedArgs = append([]string{name}, args...)
		cmd := exec.CommandContext(ctx, "true")
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		return cmd
	})

	taskPath := filepath.Join(tasksDir, dirs.InProgress, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	env := envConfig{
		repoRoot:           repoRoot,
		tasksDir:           tasksDir,
		workdir:            "/workspace",
		copilotPath:        "/bin/true",
		gitPath:            "/usr/bin/git",
		gitUploadPackPath:  "/usr/bin/git-upload-pack",
		gitReceivePackPath: "/usr/bin/git-receive-pack",
		ghPath:             "/bin/true",
		homeDir:            "/home/test",
		copilotConfigDir:   t.TempDir(),
		copilotCacheDir:    t.TempDir(),
		image:              "ubuntu:24.04",
		targetBranch:       "mato",
	}
	run := runContext{agentID: "agent1", prompt: "test prompt", model: "claude-opus-4.6", reasoningEffort: "high", timeout: time.Second}
	// The claimed task uses a DIFFERENT branch than the seeded session.
	claimed := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task-new", Title: "Task", TaskPath: taskPath}

	if err := runOnce(context.Background(), env, run, claimed); err != nil {
		t.Fatalf("runOnce: %v", err)
	}

	joined := strings.Join(capturedArgs, " ")
	// The resume session ID should NOT be the stale one from the old branch.
	if strings.Contains(joined, "--resume="+oldSessionID) {
		t.Fatalf("expected rotated session ID after branch change, but got stale ID %q in docker args: %s", oldSessionID, joined)
	}
	// It should still have a --resume arg (with the new rotated ID).
	if !strings.Contains(joined, "--resume=") {
		t.Fatalf("expected --resume with rotated session ID in docker args, got: %s", joined)
	}
	// Verify the persisted session metadata has the new branch and a new session ID.
	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindWork, "task.md")
	if err != nil {
		t.Fatalf("Load work session: %v", err)
	}
	if session == nil {
		t.Fatal("expected work session to exist after branch change")
	}
	if session.CopilotSessionID == oldSessionID {
		t.Fatal("persisted session ID should be rotated, not the stale one")
	}
	if session.TaskBranch != "task/task-new" {
		t.Fatalf("TaskBranch = %q, want %q", session.TaskBranch, "task/task-new")
	}
}

func TestRunOnce_BranchSetupFailureDoesNotCreateWorkSession(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	setHook(t, &createCloneFn, func(string) (string, error) { return cloneDir, nil })
	setHook(t, &removeCloneFn, func(string) {})
	setHook(t, &ensureBranchFn, func(string, string) (git.EnsureBranchResult, error) {
		return git.EnsureBranchResult{}, fmt.Errorf("simulated branch setup failure")
	})

	taskPath := filepath.Join(tasksDir, dirs.InProgress, "task.md")
	if err := os.WriteFile(taskPath, []byte("# Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	env := envConfig{repoRoot: repoRoot, tasksDir: tasksDir, targetBranch: "mato"}
	run := runContext{agentID: "agent1", timeout: time.Second}
	claimed := &queue.ClaimedTask{Filename: "task.md", Branch: "task/task", Title: "Task", TaskPath: taskPath}

	err = runOnce(context.Background(), env, run, claimed)
	if err == nil {
		t.Fatal("expected runOnce error")
	}
	assertWorkLaunchTaskState(t, tasksDir, "task.md", claimed.Branch, "mato", "")
	session, loadErr := runtimedata.LoadSession(tasksDir, runtimedata.KindWork, "task.md")
	if loadErr != nil {
		t.Fatalf("Load work session: %v", loadErr)
	}
	if session != nil {
		t.Fatalf("work session should not exist after pre-launch failure, got %+v", session)
	}
}

func TestRunOnce_CreateCloneFailureLeavesRecoverableTaskState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Backlog, dirs.InProgress} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	setHook(t, &createCloneFn, func(string) (string, error) {
		return "", fmt.Errorf("simulated clone failure")
	})

	taskFile := "clone-failure.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(taskPath, []byte("# Clone Failure\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	env := envConfig{repoRoot: t.TempDir(), tasksDir: tasksDir, targetBranch: "mato"}
	run := runContext{agentID: "agent1", timeout: time.Second}
	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/clone-failure", Title: "Clone Failure", TaskPath: taskPath}

	err := runOnce(context.Background(), env, run, claimed)
	if err == nil {
		t.Fatal("expected runOnce error")
	}
	if !strings.Contains(err.Error(), "create clone") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertWorkLaunchTaskState(t, tasksDir, taskFile, claimed.Branch, "mato", "")

	recoverStuckTask(tasksDir, "agent1", claimed)

	if _, statErr := os.Stat(filepath.Join(tasksDir, dirs.Backlog, taskFile)); statErr != nil {
		t.Fatalf("task should be recoverable to backlog after clone failure: %v", statErr)
	}
}

func TestRunOnce_StartingTipFailureLeavesRecoverableTaskState(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cloneDir := t.TempDir()
	setHook(t, &createCloneFn, func(string) (string, error) { return cloneDir, nil })
	setHook(t, &removeCloneFn, func(string) {})
	setHook(t, &ensureBranchFn, func(string, string) (git.EnsureBranchResult, error) {
		return git.EnsureBranchResult{Source: git.BranchSourceLocal}, nil
	})

	taskFile := "starting-tip-failure.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(taskPath, []byte("# Starting Tip Failure\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	env := envConfig{repoRoot: repoRoot, tasksDir: tasksDir, targetBranch: "mato"}
	run := runContext{agentID: "agent1", timeout: time.Second}
	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/starting-tip-failure", Title: "Starting Tip Failure", TaskPath: taskPath}

	err := runOnce(context.Background(), env, run, claimed)
	if err == nil {
		t.Fatal("expected runOnce error")
	}
	if !strings.Contains(err.Error(), "capture starting tip") {
		t.Fatalf("unexpected error: %v", err)
	}
	assertWorkLaunchTaskState(t, tasksDir, taskFile, claimed.Branch, "mato", "")

	recoverStuckTask(tasksDir, "agent1", claimed)

	if _, statErr := os.Stat(filepath.Join(tasksDir, dirs.Backlog, taskFile)); statErr != nil {
		t.Fatalf("task should be recoverable to backlog after starting-tip failure: %v", statErr)
	}
}

func TestPostAgentPush_LogProbeFailureReturnsError(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "probe-fail.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Probe Fail\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/probe-fail",
		Title:    "Probe Fail",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	err := postAgentPush(env, "agent1", claimed, t.TempDir(), "deadbeef")
	if err == nil {
		t.Fatal("expected error when git log probe fails")
	}
	if !strings.Contains(err.Error(), "determine current task branch tip") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("task should remain in in-progress/ after probe failure: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, taskFile)); !os.IsNotExist(statErr) {
		t.Fatal("task should not move to ready-for-review/ after probe failure")
	}
}

func TestRunOnce_PreservesCloneOnPostAgentPushFailure(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cloneRoot := t.TempDir()

	var createdClone string
	setHook(t, &createCloneFn, func(repo string) (string, error) {
		if repo != repoRoot {
			t.Fatalf("createCloneFn repo = %q, want %q", repo, repoRoot)
		}
		cloneDir := filepath.Join(cloneRoot, "preserved-clone")
		if err := os.MkdirAll(filepath.Dir(cloneDir), 0o755); err != nil {
			return "", err
		}
		if _, err := git.Output("", "clone", "--quiet", repoRoot, cloneDir); err != nil {
			return "", err
		}
		createdClone = cloneDir
		return cloneDir, nil
	})
	removeCalled := false
	setHook(t, &removeCloneFn, func(dir string) {
		removeCalled = true
		os.RemoveAll(dir)
	})
	setHook(t, &writeBranchMarkerFn, func(path, branch string) error {
		return fmt.Errorf("simulated branch marker failure")
	})
	setHook(t, &execCommandContext, func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sh", "-c", "printf 'agent work\n' > repo-change.txt && git add repo-change.txt && git commit -m 'agent work' >/dev/null 2>&1")
		cmd.Dir = createdClone
		cmd.Cancel = func() error { return nil }
		cmd.WaitDelay = gracefulShutdownDelay
		return cmd
	})

	taskFile := "preserve-clone.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(taskPath, []byte("---\npriority: 5\n---\n# Preserve Clone\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile task: %v", err)
	}
	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/preserve-clone",
		Title:    "Preserve Clone",
		TaskPath: taskPath,
	}
	env := envConfig{
		repoRoot:           repoRoot,
		tasksDir:           tasksDir,
		workdir:            repoRoot,
		copilotPath:        "/bin/true",
		gitPath:            "/usr/bin/git",
		gitUploadPackPath:  "/usr/bin/git-upload-pack",
		gitReceivePackPath: "/usr/bin/git-receive-pack",
		ghPath:             "/bin/true",
		goRoot:             "/usr",
		homeDir:            t.TempDir(),
		targetBranch:       "missing-target",
		image:              "test-image",
	}
	run := runContext{
		agentID: "agent1",
		prompt:  "test prompt",
		timeout: time.Second,
	}

	err := runOnce(context.Background(), env, run, claimed)
	if err == nil {
		t.Fatal("expected runOnce error when postAgentPush push fails")
	}
	if !strings.Contains(err.Error(), "post-agent push failed; preserving clone at") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "simulated branch marker failure") {
		t.Fatalf("error should include branch marker failure, got: %v", err)
	}
	if removeCalled {
		t.Fatal("clone should be preserved when postAgentPush fails")
	}
	if createdClone == "" {
		t.Fatal("expected createCloneFn to record clone path")
	}
	if _, statErr := os.Stat(createdClone); statErr != nil {
		t.Fatalf("preserved clone missing: %v", statErr)
	}
}

func TestAgentWroteFailureRecord_MultipleAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")

	content := "# Task\n" +
		"<!-- failure: agent-a at 2026-01-01T00:00:00Z step=WORK error=test1 -->\n" +
		"<!-- failure: agent-b at 2026-01-02T00:00:00Z step=WORK error=test2 -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	if !agentWroteFailureRecord(path, "agent-a") {
		t.Fatal("should find agent-a's failure record")
	}
	if !agentWroteFailureRecord(path, "agent-b") {
		t.Fatal("should find agent-b's failure record")
	}
	if agentWroteFailureRecord(path, "agent-c") {
		t.Fatal("should not find agent-c's failure record")
	}
}

func TestExtractFailureLines_WithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := "---\npriority: 10\n---\n# Task\nBody text\n" +
		"<!-- failure: agent1 at 2026-01-01T00:00:00Z step=WORK error=tests_failed files_changed=foo.go -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	result := extractFailureLines(path)
	if !strings.Contains(result, "failure: agent1") {
		t.Fatalf("expected failure line, got %q", result)
	}
}

func TestExtractFailureLines_MultipleDistinctFailures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	content := "# Task\n" +
		"<!-- failure: agent1 at 2026-01-01T00:00:00Z step=WORK error=build_failed files_changed=a.go -->\n" +
		"<!-- failure: agent2 at 2026-01-02T00:00:00Z step=COMMIT error=no_changes files_changed=none -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	result := extractFailureLines(path)
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 failure lines, got %d: %q", len(lines), result)
	}
}

// captureStderr redirects os.Stderr to a pipe and routes ui.Warnf
// through ui.SetWarningWriter, runs fn, and returns whatever was
// written. Tests that use this must not call t.Parallel because
// os.Stderr is process-global.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	prevWarn := ui.SetWarningWriter(w)

	fn()

	ui.SetWarningWriter(prevWarn)
	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	return buf.String()
}

func readDependencyContextDetails(t *testing.T, path string) []messaging.CompletionDetail {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dependency context file: %v", err)
	}

	var details []messaging.CompletionDetail
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("unmarshal dependency context file: %v", err)
	}
	return details
}

func TestWriteDependencyContextFile_ResolvesCompletedAliases(t *testing.T) {
	tests := []struct {
		name          string
		completedFile string
		completedBody string
		detail        *messaging.CompletionDetail
		dependsOn     string
		wantResult    bool
		wantTaskID    string
		wantTaskFile  string
	}{
		{
			name:          "filename stem resolves explicit id",
			completedFile: "foo.md",
			completedBody: "---\nid: canonical-id\n---\n# Foo\n",
			detail: &messaging.CompletionDetail{
				TaskID:    "canonical-id",
				TaskFile:  "foo.md",
				Branch:    "task/canonical-id",
				Title:     "Foo",
				CommitSHA: "abc123",
			},
			dependsOn:    "foo",
			wantResult:   true,
			wantTaskID:   "canonical-id",
			wantTaskFile: "foo.md",
		},
		{
			name:          "explicit id resolves different filename stem",
			completedFile: "foo.md",
			completedBody: "---\nid: canonical-id\n---\n# Foo\n",
			detail: &messaging.CompletionDetail{
				TaskID:    "canonical-id",
				TaskFile:  "foo.md",
				Branch:    "task/canonical-id",
				Title:     "Foo",
				CommitSHA: "def456",
			},
			dependsOn:    "canonical-id",
			wantResult:   true,
			wantTaskID:   "canonical-id",
			wantTaskFile: "foo.md",
		},
		{
			name:          "explicit id continues to work",
			completedFile: "dep-task.md",
			completedBody: "---\nid: dep-task\n---\n# Dep Task\n",
			detail: &messaging.CompletionDetail{
				TaskID:    "dep-task",
				TaskFile:  "dep-task.md",
				Branch:    "task/dep-task",
				Title:     "Dep Task",
				CommitSHA: "ghi789",
			},
			dependsOn:    "dep-task",
			wantResult:   true,
			wantTaskID:   "dep-task",
			wantTaskFile: "dep-task.md",
		},
		{
			name:          "missing completion detail is skipped cleanly",
			completedFile: "foo.md",
			completedBody: "---\nid: canonical-id\n---\n# Foo\n",
			detail:        nil,
			dependsOn:     "foo",
			wantResult:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tasksDir := t.TempDir()
			for _, sub := range []string{dirs.Completed, dirs.InProgress} {
				if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
					t.Fatalf("mkdir %s: %v", sub, err)
				}
			}
			if err := messaging.Init(tasksDir); err != nil {
				t.Fatalf("messaging.Init: %v", err)
			}

			completedPath := filepath.Join(tasksDir, dirs.Completed, tt.completedFile)
			if err := os.WriteFile(completedPath, []byte(tt.completedBody), 0o644); err != nil {
				t.Fatalf("write completed task: %v", err)
			}
			if tt.detail != nil {
				if err := messaging.WriteCompletionDetail(tasksDir, *tt.detail); err != nil {
					t.Fatalf("write completion detail: %v", err)
				}
			}

			taskFile := "consumer.md"
			taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
			taskContent := fmt.Sprintf("---\ndepends_on:\n  - %s\n---\n# Consumer\n", tt.dependsOn)
			if err := os.WriteFile(taskPath, []byte(taskContent), 0o644); err != nil {
				t.Fatalf("write in-progress task: %v", err)
			}

			claimed := &queue.ClaimedTask{
				Filename: taskFile,
				Branch:   "task/consumer",
				Title:    "Consumer",
				TaskPath: taskPath,
			}

			result := writeDependencyContextFile(tasksDir, claimed)
			if tt.wantResult {
				if result == "" {
					t.Fatal("expected dependency context file path")
				}
				details := readDependencyContextDetails(t, result)
				if len(details) != 1 {
					t.Fatalf("details count = %d, want 1", len(details))
				}
				if details[0].TaskID != tt.wantTaskID {
					t.Fatalf("TaskID = %q, want %q", details[0].TaskID, tt.wantTaskID)
				}
				if details[0].TaskFile != tt.wantTaskFile {
					t.Fatalf("TaskFile = %q, want %q", details[0].TaskFile, tt.wantTaskFile)
				}
				return
			}
			if result != "" {
				t.Fatalf("expected empty result when completion detail is unavailable, got %q", result)
			}
		})
	}
}

func TestWriteDependencyContextFile_InvalidFrontmatter(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.InProgress), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "bad-frontmatter.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	// Invalid YAML frontmatter.
	os.WriteFile(taskPath, []byte("---\n: invalid yaml\n---\n# Bad Frontmatter\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/bad",
		Title:    "Bad",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty string for invalid frontmatter, got %q", result)
	}
}

func TestWriteDependencyContextFile_DepsButNoCompletions(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.InProgress), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "with-deps.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - nonexistent-dep\n---\n# With Deps\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/with-deps",
		Title:    "With Deps",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty string when no completion files exist, got %q", result)
	}
}

func TestWriteDependencyContextFile_WithMatchingCompletion(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Completed, dirs.InProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "dep-task.md"), []byte("---\nid: dep-task\n---\n# Dep Task\n"), 0o644); err != nil {
		t.Fatalf("write completed task: %v", err)
	}

	// Create a completion detail file for the dependency.
	detail := messaging.CompletionDetail{
		TaskID:    "dep-task",
		TaskFile:  "dep-task.md",
		Branch:    "task/dep-task",
		Title:     "Dep Task",
		CommitSHA: "abc123",
	}
	if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("write completion detail: %v", err)
	}

	taskFile := "depends-on-dep.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - dep-task\n---\n# Depends On Dep\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/depends-on-dep",
		Title:    "Depends On Dep",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result == "" {
		t.Fatal("expected non-empty path for matching completion")
	}

	// Verify the file exists and contains expected data.
	fileData, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("could not read dependency context file: %v", err)
	}
	if !strings.Contains(string(fileData), "dep-task.md") {
		t.Fatalf("dependency context should contain dep task info, got:\n%s", string(fileData))
	}
}

func TestRemoveDependencyContextFile_ExistingFile(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "messages"), 0o755)

	filename := "task.md"
	depPath := filepath.Join(tasksDir, "messages", "dependency-context-"+filename+".json")
	os.WriteFile(depPath, []byte("[]"), 0o644)

	removeDependencyContextFile(tasksDir, filename)

	if _, err := os.Stat(depPath); !os.IsNotExist(err) {
		t.Fatal("dependency context file should be removed")
	}
}

func TestRemoveDependencyContextFile_NonexistentFile(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "messages"), 0o755)

	// Should not panic or error.
	removeDependencyContextFile(tasksDir, "nonexistent.md")
}

func TestWriteDependencyContextFile_MissingCompletionSkipped(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.InProgress), 0o755)
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "skip-missing.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - no-such-dep\n---\n# Skip Missing\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/skip-missing",
		Title:    "Skip Missing",
		TaskPath: taskPath,
	}

	// Missing completion detail file → silently skipped (no warning).
	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty string when completion detail does not exist, got %q", result)
	}
}

func TestWriteDependencyContextFile_MalformedCompletionWarns(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Completed, dirs.InProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "bad-dep.md"), []byte("---\nid: bad-dep\n---\n# Bad Dep\n"), 0o644); err != nil {
		t.Fatalf("write completed task: %v", err)
	}

	// Write a malformed (non-JSON) completion detail file.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	os.WriteFile(filepath.Join(completionsDir, "bad-dep.json"), []byte("not json{{{"), 0o644)

	taskFile := "bad-dep-task.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - bad-dep\n---\n# Bad Dep Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/bad-dep-task",
		Title:    "Bad Dep Task",
		TaskPath: taskPath,
	}

	// Malformed file is a non-not-found error; the function should still
	// return "" (no valid details) and emit a warning to stderr.
	var result string
	stderr := captureStderr(t, func() {
		result = writeDependencyContextFile(tasksDir, claimed)
	})
	if result != "" {
		t.Fatalf("expected empty string when completion detail is malformed, got %q", result)
	}
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected warning on stderr, got %q", stderr)
	}
	if !strings.Contains(stderr, "bad-dep") {
		t.Fatalf("expected stderr to mention dependency ID \"bad-dep\", got %q", stderr)
	}
	if !strings.Contains(stderr, taskFile) {
		t.Fatalf("expected stderr to mention task filename %q, got %q", taskFile, stderr)
	}
}

func TestWriteDependencyContextFile_MixedDeps(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Completed, dirs.InProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "good-dep.md"), []byte("---\nid: good-dep\n---\n# Good Dep\n"), 0o644); err != nil {
		t.Fatalf("write completed good-dep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "broken-dep.md"), []byte("---\nid: broken-dep\n---\n# Broken Dep\n"), 0o644); err != nil {
		t.Fatalf("write completed broken-dep: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, dirs.Completed, "missing-dep.md"), []byte("---\nid: missing-dep\n---\n# Missing Dep\n"), 0o644); err != nil {
		t.Fatalf("write completed missing-dep: %v", err)
	}

	// Valid completion detail for one dependency.
	detail := messaging.CompletionDetail{
		TaskID:    "good-dep",
		TaskFile:  "good-dep.md",
		Branch:    "task/good-dep",
		Title:     "Good Dep",
		CommitSHA: "def456",
	}
	if err := messaging.WriteCompletionDetail(tasksDir, detail); err != nil {
		t.Fatalf("write completion detail: %v", err)
	}

	// Write a malformed completion detail for another dependency.
	completionsDir := filepath.Join(tasksDir, "messages", "completions")
	os.WriteFile(filepath.Join(completionsDir, "broken-dep.json"), []byte("<<<invalid>>>"), 0o644)

	taskFile := "mixed-deps.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("---\ndepends_on:\n  - good-dep\n  - broken-dep\n  - missing-dep\n---\n# Mixed Deps\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/mixed-deps",
		Title:    "Mixed Deps",
		TaskPath: taskPath,
	}

	// Should return the valid dep's context despite one malformed and one missing,
	// and emit a warning to stderr for the malformed dependency only.
	var result string
	stderr := captureStderr(t, func() {
		result = writeDependencyContextFile(tasksDir, claimed)
	})
	if result == "" {
		t.Fatal("expected non-empty path when at least one valid completion exists")
	}

	// Verify stderr warning for the malformed dependency.
	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected warning on stderr for broken-dep, got %q", stderr)
	}
	if !strings.Contains(stderr, "broken-dep") {
		t.Fatalf("expected stderr to mention dependency ID \"broken-dep\", got %q", stderr)
	}
	if !strings.Contains(stderr, taskFile) {
		t.Fatalf("expected stderr to mention task filename %q, got %q", taskFile, stderr)
	}
	// missing-dep should not trigger a warning (os.ErrNotExist is silently skipped).
	if strings.Contains(stderr, "missing-dep") {
		t.Fatalf("missing-dep should not appear in stderr (ErrNotExist is skipped), got %q", stderr)
	}

	fileData, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("could not read dependency context file: %v", err)
	}
	if !strings.Contains(string(fileData), "good-dep.md") {
		t.Fatalf("dependency context should contain good-dep info, got:\n%s", string(fileData))
	}
	if strings.Contains(string(fileData), "broken-dep") {
		t.Fatalf("dependency context should not contain broken-dep info")
	}
	if strings.Contains(string(fileData), "missing-dep") {
		t.Fatalf("dependency context should not contain missing-dep info")
	}
}

func TestRecoverStuckTask_AppendsFailureWithTimestamp(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.Backlog, dirs.InProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "timestamp-task.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Timestamp Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/timestamp",
		Title:    "Timestamp Task",
		TaskPath: inProgressPath,
	}
	if err := runtimedata.UpdateTaskState(tasksDir, taskFile, func(state *runtimedata.TaskState) {
		state.TaskBranch = claimed.Branch
		state.LastOutcome = runtimedata.OutcomeWorkLaunched
	}); err != nil {
		t.Fatalf("seed work-launched taskstate: %v", err)
	}

	captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent-x", claimed)
	})

	backlogPath := filepath.Join(tasksDir, dirs.Backlog, taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task not found in backlog/: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "<!-- failure: agent-x at") {
		t.Fatal("failure record should contain agent ID and timestamp")
	}
	if !strings.Contains(content, "agent container exited without cleanup") {
		t.Fatal("failure record should contain generic message")
	}
}

func TestExtractReviewRejections_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte(""), 0o644)

	result := extractReviewRejections(path)
	if result != "" {
		t.Fatalf("expected empty string for empty file, got %q", result)
	}
}

func TestExtractReviewRejections_MissingFile(t *testing.T) {
	result := extractReviewRejections(filepath.Join(t.TempDir(), "nope.md"))
	if result != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", result)
	}
}

func TestSelectTaskForReview_ReturnsHighestPriority(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "low.md"),
		[]byte("<!-- branch: task/low -->\n---\npriority: 50\nmax_retries: 3\n---\n# Low\n"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "high.md"),
		[]byte("<!-- branch: task/high -->\n---\npriority: 5\nmax_retries: 3\n---\n# High\n"), 0o644)

	task := selectTaskForReview(tasksDir, nil)
	if task == nil {
		t.Fatal("expected a task to be selected")
	}
	if task.Filename != "high.md" {
		t.Fatalf("expected highest priority task (high.md), got %q", task.Filename)
	}
}

func TestSelectTaskForReview_NilIndex(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, dirs.ReadyReview), 0o755)
	os.MkdirAll(filepath.Join(tasksDir, dirs.Failed), 0o755)

	// No tasks.
	task := selectTaskForReview(tasksDir, nil)
	if task != nil {
		t.Fatal("expected nil when no tasks available")
	}
}

func TestReviewCandidates_FilesystemFallback_SkipsParseErrors(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Valid task.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "good.md"),
		[]byte("<!-- branch: task/good -->\n---\npriority: 10\nmax_retries: 3\n---\n# Good\n"), 0o644)
	// Invalid frontmatter task.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "bad.md"),
		[]byte("---\n: invalid\n---\n# Bad\n"), 0o644)

	_, stderr := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate (skipping bad parse), got %d", len(candidates))
		}
		if candidates[0].Filename != "good.md" {
			t.Fatalf("expected good.md, got %q", candidates[0].Filename)
		}
	})

	if !strings.Contains(stderr, "warning:") {
		t.Fatalf("expected parse warning in stderr, got:\n%s", stderr)
	}
}

// setupGitCloneWithCommits creates a bare repo as "remote", clones it, and adds
// a commit on the given task branch above the target branch. Returns the clone
// directory. The caller can use this to test postAgentPush end-to-end.
func setupGitCloneWithCommits(t *testing.T, targetBranch, taskBranch string) string {
	t.Helper()
	remoteDir := t.TempDir()
	cloneDir := t.TempDir()

	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}

	gitRun(remoteDir, "git", "init", "--bare", "-b", targetBranch)
	gitRun(cloneDir, "git", "init", "-b", targetBranch)
	gitRun(cloneDir, "git", "config", "user.name", "test")
	gitRun(cloneDir, "git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("initial"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "init")
	gitRun(cloneDir, "git", "remote", "add", "origin", remoteDir)
	gitRun(cloneDir, "git", "push", "origin", targetBranch)
	gitRun(cloneDir, "git", "checkout", "-b", taskBranch)
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "task work")

	return cloneDir
}

// readEventMessages reads all message JSON files from the messages/events/
// directory and returns them as parsed Message structs.
func readEventMessages(t *testing.T, tasksDir string) []messaging.Message {
	t.Helper()
	eventsDir := filepath.Join(tasksDir, "messages", "events")
	entries, err := os.ReadDir(eventsDir)
	if err != nil {
		t.Fatalf("read events dir: %v", err)
	}
	var msgs []messaging.Message
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(eventsDir, entry.Name()))
		if err != nil {
			t.Fatalf("read event file %s: %v", entry.Name(), err)
		}
		var msg messaging.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue // skip malformed
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

func TestPostAgentPush_HappyPath(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "happy-task.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Happy Task\n"), 0o644)

	cloneDir := setupGitCloneWithCommits(t, "main", "task/happy-task")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/happy-task",
		Title:    "Happy Task",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	stdout, _ := captureStdoutStderr(t, func() {
		err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Task should have moved to ready-for-review/.
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("task should be in ready-for-review/: %v", statErr)
	}

	// Task should no longer be in in-progress/.
	if _, statErr := os.Stat(inProgressPath); !os.IsNotExist(statErr) {
		t.Fatal("task should not remain in in-progress/ after successful push")
	}

	// Branch marker should be present in the moved file.
	data, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatalf("read ready file: %v", err)
	}
	if !strings.Contains(string(data), "<!-- branch: task/happy-task -->") {
		t.Fatalf("branch marker not found in moved file, got:\n%s", string(data))
	}

	// Stdout should report success.
	if !strings.Contains(stdout, "Pushed task/happy-task") {
		t.Fatalf("expected success message in stdout, got:\n%s", stdout)
	}

	// Verify conflict-warning and completion messages were emitted.
	msgs := readEventMessages(t, tasksDir)
	var hasConflictWarning, hasCompletion bool
	for _, msg := range msgs {
		if msg.Task != taskFile {
			continue
		}
		switch msg.Type {
		case "conflict-warning":
			hasConflictWarning = true
			if msg.From != "agent1" {
				t.Fatalf("conflict-warning from should be agent1, got %q", msg.From)
			}
			if msg.Branch != "task/happy-task" {
				t.Fatalf("conflict-warning branch should be task/happy-task, got %q", msg.Branch)
			}
			if len(msg.Files) == 0 {
				t.Fatal("conflict-warning should include changed files")
			}
		case "completion":
			hasCompletion = true
			if msg.From != "agent1" {
				t.Fatalf("completion from should be agent1, got %q", msg.From)
			}
			if msg.Branch != "task/happy-task" {
				t.Fatalf("completion branch should be task/happy-task, got %q", msg.Branch)
			}
		}
	}
	if !hasConflictWarning {
		t.Fatal("expected conflict-warning message to be written")
	}
	if !hasCompletion {
		t.Fatal("expected completion message to be written")
	}
}

func TestPostAgentPush_PushFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "push-fail.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Push Fail\n"), 0o644)

	// Set up a git repo with commits but NO remote, so push will fail.
	cloneDir := t.TempDir()
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = cloneDir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	gitRun("git", "init", "-b", "main")
	gitRun("git", "config", "user.name", "test")
	gitRun("git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	gitRun("git", "add", ".")
	gitRun("git", "commit", "-m", "init")
	gitRun("git", "checkout", "-b", "task/push-fail")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644)
	gitRun("git", "add", ".")
	gitRun("git", "commit", "-m", "task work")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/push-fail",
		Title:    "Push Fail",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")

	// Should return a push error.
	if err == nil {
		t.Fatal("expected error when push fails")
	}
	if !strings.Contains(err.Error(), "push task branch") {
		t.Fatalf("error should mention push failure, got: %v", err)
	}

	// Task should remain in in-progress/ (no premature move).
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("task should remain in in-progress/ after push failure: %v", statErr)
	}

	// Task should NOT be in ready-for-review/.
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatal("task should not appear in ready-for-review/ after push failure")
	}

	// No conflict-warning or completion messages should have been written.
	msgs := readEventMessages(t, tasksDir)
	for _, msg := range msgs {
		if msg.Task == taskFile && (msg.Type == "conflict-warning" || msg.Type == "completion") {
			t.Fatalf("no conflict-warning or completion messages should be written on push failure, found %q", msg.Type)
		}
	}
}

func TestPostAgentPush_MessagesContainChangedFiles(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "files-msg.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Files Msg\n"), 0o644)

	// Set up repo with multiple changed files to verify file list.
	remoteDir := t.TempDir()
	cloneDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	gitRun(remoteDir, "git", "init", "--bare", "-b", "main")
	gitRun(cloneDir, "git", "init", "-b", "main")
	gitRun(cloneDir, "git", "config", "user.name", "test")
	gitRun(cloneDir, "git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "a.go"), []byte("package a"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "init")
	gitRun(cloneDir, "git", "remote", "add", "origin", remoteDir)
	gitRun(cloneDir, "git", "push", "origin", "main")
	gitRun(cloneDir, "git", "checkout", "-b", "task/files-msg")
	os.WriteFile(filepath.Join(cloneDir, "a.go"), []byte("package a // changed"), 0o644)
	os.WriteFile(filepath.Join(cloneDir, "b.go"), []byte("package b"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "multi-file change")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/files-msg",
		Title:    "Files Msg",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	captureStdoutStderr(t, func() {
		if err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// Both conflict-warning and completion messages should list the changed files.
	msgs := readEventMessages(t, tasksDir)
	for _, msg := range msgs {
		if msg.Task != taskFile {
			continue
		}
		if msg.Type == "conflict-warning" || msg.Type == "completion" {
			foundA := false
			foundB := false
			for _, f := range msg.Files {
				if f == "a.go" {
					foundA = true
				}
				if f == "b.go" {
					foundB = true
				}
			}
			if !foundA || !foundB {
				t.Fatalf("%s message should list both a.go and b.go, got %v", msg.Type, msg.Files)
			}
		}
	}
}

func TestPostAgentPush_DestinationCollisionPreventsPush(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "collision.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("# Collision\n"), 0o644)

	// Pre-create the destination file to trigger the pre-check collision.
	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	os.WriteFile(readyPath, []byte("# Existing Review\n"), 0o644)

	cloneDir := setupGitCloneWithCommits(t, "main", "task/collision")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/collision",
		Title:    "Collision",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	_, stderr := captureStdoutStderr(t, func() {
		err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")
		if err == nil {
			t.Fatal("expected error when destination already exists")
		}
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("error should mention destination collision, got: %v", err)
		}
	})

	// Warning should be printed to stderr.
	if !strings.Contains(stderr, "already exists in ready-for-review") {
		t.Fatalf("expected collision warning in stderr, got:\n%s", stderr)
	}

	// The existing file in ready-for-review/ should be unchanged.
	data, _ := os.ReadFile(readyPath)
	if !strings.Contains(string(data), "Existing Review") {
		t.Fatal("existing file in ready-for-review/ should not be overwritten")
	}

	// Task should still be in in-progress/.
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("task should remain in in-progress/: %v", statErr)
	}
}

func TestPostAgentPush_NoOpResumedBranchSkipsPush(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "resume-noop.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/resume-noop -->\n# Resume Noop\n"), 0o644)

	cloneDir := setupGitCloneWithCommits(t, "main", "task/resume-noop")
	startingTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/resume-noop",
		Title:    "Resume Noop",
		TaskPath: inProgressPath,
	}
	env := envConfig{tasksDir: tasksDir, targetBranch: "main"}

	if err := postAgentPush(env, "agent1", claimed, cloneDir, strings.TrimSpace(startingTip)); err != nil {
		t.Fatalf("postAgentPush: %v", err)
	}
	if _, err := os.Stat(inProgressPath); err != nil {
		t.Fatalf("task should remain in in-progress/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, taskFile)); !os.IsNotExist(err) {
		t.Fatal("task should not move to ready-for-review/ when branch tip is unchanged")
	}
}

func TestPostAgentPush_RecordsWorkTaskState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "taskstate.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Taskstate\n"), 0o644)

	cloneDir := setupGitCloneWithCommits(t, "main", "task/taskstate")
	currentTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	currentTip = strings.TrimSpace(currentTip)

	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/taskstate", Title: "Taskstate", TaskPath: inProgressPath}
	env := envConfig{tasksDir: tasksDir, targetBranch: "main"}
	if err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef"); err != nil {
		t.Fatalf("postAgentPush: %v", err)
	}
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to be written")
	}
	if state.TaskBranch != claimed.Branch {
		t.Fatalf("TaskBranch = %q, want %q", state.TaskBranch, claimed.Branch)
	}
	if state.TargetBranch != "main" {
		t.Fatalf("TargetBranch = %q, want %q", state.TargetBranch, "main")
	}
	if state.LastHeadSHA != currentTip {
		t.Fatalf("LastHeadSHA = %q, want %q", state.LastHeadSHA, currentTip)
	}
	if state.LastOutcome != runtimedata.OutcomeWorkPushed {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeWorkPushed)
	}
}

func TestPostAgentPush_BranchMarkerWriteFailureLeavesPushedTaskState(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.InProgress, dirs.ReadyReview, "messages", "messages/events"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	taskFile := "marker-state.md"
	inProgressPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	if err := os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/marker-state -->\n# Marker State\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}

	cloneDir := t.TempDir()
	remoteDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	gitRun(remoteDir, "git", "init", "--bare", "-b", "main")
	gitRun(cloneDir, "git", "init", "-b", "main")
	gitRun(cloneDir, "git", "config", "user.name", "test")
	gitRun(cloneDir, "git", "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile init: %v", err)
	}
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "init")
	gitRun(cloneDir, "git", "remote", "add", "origin", remoteDir)
	gitRun(cloneDir, "git", "push", "origin", "main")
	gitRun(cloneDir, "git", "checkout", "-b", "task/marker-state")
	if err := os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("WriteFile branch change: %v", err)
	}
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "task work")
	currentTip, err := git.Output(cloneDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	currentTip = strings.TrimSpace(currentTip)

	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/marker-state", Title: "Marker State", TaskPath: inProgressPath}
	env := envConfig{tasksDir: tasksDir, targetBranch: "main"}
	setHook(t, &writeBranchMarkerFn, func(path, branch string) error {
		return fmt.Errorf("simulated disk full")
	})

	err = postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")
	if err == nil {
		t.Fatal("expected error when branch marker write fails")
	}
	state, err := runtimedata.LoadTaskState(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil {
		t.Fatal("expected taskstate to be written")
	}
	if state.LastOutcome != runtimedata.OutcomeWorkBranchPushed {
		t.Fatalf("LastOutcome = %q, want %q", state.LastOutcome, runtimedata.OutcomeWorkBranchPushed)
	}
	if state.LastHeadSHA != currentTip {
		t.Fatalf("LastHeadSHA = %q, want %q", state.LastHeadSHA, currentTip)
	}
}

func TestRunOnce_RecordedBranchResumeRequiresLocalOrRemoteSource(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "resume-guard.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/resume-guard -->\n# Resume Guard\n"), 0o644)
	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/resume-guard", Title: "Resume Guard", TaskPath: taskPath, HadRecordedBranchMark: true}

	setHook(t, &ensureBranchFn, func(repoRoot, branch string) (git.EnsureBranchResult, error) {
		return git.EnsureBranchResult{Branch: branch, Source: git.BranchSourceHeadRemoteMissing}, nil
	})

	dockerDir := t.TempDir()
	dockerScript := filepath.Join(dockerDir, "docker")
	if err := os.WriteFile(dockerScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	t.Setenv("PATH", dockerDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	env := envConfig{repoRoot: repoRoot, tasksDir: tasksDir, workdir: repoRoot, copilotPath: "/bin/true", gitPath: "/usr/bin/git", gitUploadPackPath: "/usr/bin/git-upload-pack", gitReceivePackPath: "/usr/bin/git-receive-pack", ghPath: "/bin/true", goRoot: "/usr", homeDir: t.TempDir(), targetBranch: "mato", image: "test-image"}
	run := runContext{agentID: "agent1", prompt: "test", timeout: time.Second}

	err := runOnce(context.Background(), env, run, claimed)
	if err == nil {
		t.Fatal("expected runOnce to fail closed for recorded branch fallback")
	}
	if !strings.Contains(err.Error(), "unsupported branch source") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunOnce_UsesEnsureBranchForRecordedResume(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "resume-ok.md"
	taskPath := filepath.Join(tasksDir, dirs.InProgress, taskFile)
	os.WriteFile(taskPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/resume-ok -->\n# Resume OK\n"), 0o644)
	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/resume-ok", Title: "Resume OK", TaskPath: taskPath, HadRecordedBranchMark: true}

	called := false
	setHook(t, &ensureBranchFn, func(repoRoot, branch string) (git.EnsureBranchResult, error) {
		called = true
		if _, err := git.Output(repoRoot, "checkout", "-b", branch); err != nil {
			return git.EnsureBranchResult{}, err
		}
		return git.EnsureBranchResult{Branch: branch, Source: git.BranchSourceRemote}, nil
	})

	dockerDir := t.TempDir()
	dockerLog := filepath.Join(dockerDir, "docker.log")
	dockerScript := filepath.Join(dockerDir, "docker")
	script := "#!/bin/sh\nprintf 'docker %s\\n' \"$*\" >> \"$DOCKER_LOG\"\nexit 0\n"
	if err := os.WriteFile(dockerScript, []byte(script), 0o755); err != nil {
		t.Fatalf("write docker stub: %v", err)
	}
	t.Setenv("PATH", dockerDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOCKER_LOG", dockerLog)

	env := envConfig{repoRoot: repoRoot, tasksDir: tasksDir, workdir: repoRoot, copilotPath: "/bin/true", gitPath: "/usr/bin/git", gitUploadPackPath: "/usr/bin/git-upload-pack", gitReceivePackPath: "/usr/bin/git-receive-pack", ghPath: "/bin/true", goRoot: "/usr", homeDir: t.TempDir(), targetBranch: "mato", image: "test-image"}
	run := runContext{agentID: "agent1", prompt: "test", timeout: time.Second}

	if err := runOnce(context.Background(), env, run, claimed); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if !called {
		t.Fatal("expected ensureBranchFn to be called")
	}
}

func TestReviewCandidates_FilesystemFallback_MissingBranchMarkerRecordsFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{dirs.ReadyReview, dirs.Failed} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Task without a branch marker in file.
	os.WriteFile(filepath.Join(tasksDir, dirs.ReadyReview, "no-branch.md"),
		[]byte("---\npriority: 10\nmax_retries: 3\n---\n# No Branch\n"), 0o644)

	stdout, _ := captureStdoutStderr(t, func() {
		candidates := reviewCandidates(tasksDir, nil)
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates, got %d", len(candidates))
		}
	})
	if !strings.Contains(stdout, "recorded review-failure for no-branch.md") {
		t.Fatalf("expected review-failure log, got:\n%s", stdout)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, dirs.ReadyReview, "no-branch.md"))
	if err != nil {
		t.Fatalf("ReadFile no-branch.md: %v", err)
	}
	if !strings.Contains(string(data), "missing required") || !strings.Contains(string(data), "ready-for-review") {
		t.Fatalf("expected review-failure marker, got:\n%s", string(data))
	}
}
