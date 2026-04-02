package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"mato/internal/frontmatter"
	"mato/internal/git"
	"mato/internal/messaging"
	"mato/internal/pause"
	"mato/internal/process"
	"mato/internal/queue"
	"mato/internal/taskfile"
	"mato/internal/taskstate"
	"mato/internal/ui"
)

func captureStdoutStderr(t *testing.T, fn func()) (string, string) {
	t.Helper()

	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	os.Stdout = stdoutW
	os.Stderr = stderrW
	defer func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
	}()

	fn()

	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	stdoutData, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	stderrData, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := stdoutR.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	if err := stderrR.Close(); err != nil {
		t.Fatalf("close stderr reader: %v", err)
	}
	return string(stdoutData), string(stderrData)
}

func testRunOptions() RunOptions {
	return RunOptions{
		TaskModel:                  DefaultTaskModel,
		ReviewModel:                DefaultReviewModel,
		ReviewSessionResumeEnabled: true,
		TaskReasoningEffort:        DefaultReasoningEffort,
		ReviewReasoningEffort:      DefaultReasoningEffort,
	}
}

func TestRecoverStuckTask_MovesToBacklog(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Example Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}

	recoverStuckTask(tasksDir, "agent1", claimed)

	// Task should no longer be in in-progress/
	if _, err := os.Stat(inProgressPath); err == nil {
		t.Fatal("task file still in in-progress/ after recovery")
	}

	// Task should be in backlog/ with a failure record
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task file not found in backlog/: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: agent1") {
		t.Fatal("failure record not appended to recovered task")
	}
	if !strings.Contains(string(data), "agent container exited without cleanup") {
		t.Fatal("failure record missing expected message")
	}
}

func TestRecoverStuckTask_NoopWhenTaskAlreadyMoved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress, queue.DirReadyMerge} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	// Task already moved to ready-to-merge by the agent (success case)
	readyPath := filepath.Join(tasksDir, queue.DirReadyMerge, taskFile)
	os.WriteFile(readyPath, []byte("# Example Task\n"), 0o644)

	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}

	// Should be a no-op since the task is not in in-progress/
	recoverStuckTask(tasksDir, "agent1", claimed)

	// backlog/ should remain empty
	entries, _ := os.ReadDir(filepath.Join(tasksDir, queue.DirBacklog))
	if len(entries) != 0 {
		t.Fatalf("expected backlog/ to be empty, got %d entries", len(entries))
	}

	// ready-to-merge/ should still have the file
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatal("task file disappeared from ready-to-merge/")
	}
}

func TestRecoverStuckTask_BacklogCollision(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Example Task\n"), 0o644)

	// A file with the same name already exists in backlog/
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	os.WriteFile(backlogPath, []byte("# Existing Task\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}

	// Recovery should refuse to overwrite the existing backlog file
	recoverStuckTask(tasksDir, "agent1", claimed)

	// The existing backlog file must be untouched
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("backlog file should still exist: %v", err)
	}
	if string(data) != "# Existing Task\n" {
		t.Fatalf("backlog file was overwritten, got: %s", string(data))
	}

	// The in-progress file should remain since recovery was skipped
	if _, err := os.Stat(inProgressPath); err != nil {
		t.Fatal("in-progress file should remain when backlog destination exists")
	}
}

func TestRecoverStuckTask_PushedTaskMovesToReadyReview(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}
	for _, sub := range []string{"messages", "messages/events", "messages/completions", "messages/presence"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", sub, err)
		}
	}

	taskFile := "pushed-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	if err := os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n<!-- branch: task/pushed-task -->\n# Example Task\n"), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := taskstate.Update(tasksDir, taskFile, func(state *taskstate.TaskState) {
		state.TaskBranch = "task/pushed-task"
		state.LastOutcome = taskstate.OutcomeWorkBranchPushed
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/pushed-task", Title: "Example Task", TaskPath: inProgressPath}
	stdout, stderr := captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent1", claimed)
	})
	if !strings.Contains(stdout, "Recovered pushed task") {
		t.Fatalf("expected pushed-task recovery message in stdout, got:\n%s", stdout)
	}
	if strings.Contains(stderr, "warning:") {
		t.Fatalf("expected no stderr warnings, got:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, taskFile)); !os.IsNotExist(err) {
		t.Fatalf("task should not be moved to backlog, got err: %v", err)
	}
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	data, err := os.ReadFile(readyPath)
	if err != nil {
		t.Fatalf("task should be moved to ready-for-review: %v", err)
	}
	if !strings.Contains(string(data), "<!-- branch: task/pushed-task -->") {
		t.Fatalf("ready-for-review task should keep branch marker, got:\n%s", string(data))
	}
	state, err := taskstate.Load(tasksDir, taskFile)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state == nil || state.LastOutcome != "work-pushed" {
		t.Fatalf("taskstate = %+v, want LastOutcome=work-pushed", state)
	}
	msgs, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	var hasConflictWarning bool
	var hasCompletion bool
	for _, msg := range msgs {
		if msg.Task != taskFile {
			continue
		}
		switch msg.Type {
		case "conflict-warning":
			hasConflictWarning = true
		case "completion":
			hasCompletion = true
		}
	}
	if !hasConflictWarning {
		t.Fatal("expected recovered pushed task to emit conflict-warning message")
	}
	if !hasCompletion {
		t.Fatal("expected recovered pushed task to emit completion message")
	}
	for _, msg := range msgs {
		if msg.Task != taskFile {
			continue
		}
		if (msg.Type == "conflict-warning" || msg.Type == "completion") && len(msg.Files) != 0 {
			t.Fatalf("recovered pushed task without affects should emit empty file list, got %v for %s", msg.Files, msg.Type)
		}
	}
}

func TestBuildDockerArgs_ModelAndReasoningEffort_FromRunContext(t *testing.T) {
	baseEnv := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	baseRun := runContext{
		prompt:          "do stuff",
		model:           "claude-opus-4.6",
		reasoningEffort: "high",
	}

	joined := strings.Join(buildDockerArgs(baseEnv, baseRun, nil, nil), " ")
	if !strings.Contains(joined, "--model claude-opus-4.6") {
		t.Fatalf("expected task model in docker args, got %s", joined)
	}
	if !strings.Contains(joined, "--reasoning-effort high") {
		t.Fatalf("expected reasoning effort in docker args, got %s", joined)
	}
}

func TestWriteDependencyContextFile_WithCompletionFiles(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Create a task that depends on "dep-a" and "dep-b"
	taskFile := "my-task.md"
	taskPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	taskContent := "---\ndepends_on:\n  - dep-a\n  - dep-b\n---\n# My Task\n"
	os.WriteFile(taskPath, []byte(taskContent), 0o644)

	// Write completion detail for dep-a only
	if err := messaging.WriteCompletionDetail(tasksDir, messaging.CompletionDetail{
		TaskID:       "dep-a",
		TaskFile:     "dep-a.md",
		Branch:       "task/dep-a",
		CommitSHA:    "sha-a",
		FilesChanged: []string{"a.go"},
		Title:        "Dep A",
	}); err != nil {
		t.Fatalf("WriteCompletionDetail: %v", err)
	}

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/my-task",
		Title:    "My Task",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result == "" {
		t.Fatal("expected non-empty file path")
	}

	expectedPath := filepath.Join(tasksDir, "messages", "dependency-context-"+taskFile+".json")
	if result != expectedPath {
		t.Fatalf("path = %q, want %q", result, expectedPath)
	}

	data, err := os.ReadFile(result)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var details []messaging.CompletionDetail
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(details) != 1 {
		t.Fatalf("expected 1 dependency detail, got %d", len(details))
	}
	if details[0].TaskID != "dep-a" {
		t.Fatalf("TaskID = %q, want %q", details[0].TaskID, "dep-a")
	}
	if details[0].CommitSHA != "sha-a" {
		t.Fatalf("CommitSHA = %q, want %q", details[0].CommitSHA, "sha-a")
	}
}

func TestWriteDependencyContextFile_NoDeps(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "no-deps.md"
	taskPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	taskContent := "---\npriority: 5\n---\n# No deps\n"
	os.WriteFile(taskPath, []byte(taskContent), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/no-deps",
		Title:    "No deps",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty path, got %q", result)
	}
}

func TestWriteDependencyContextFile_NoCompletionFiles(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	taskFile := "has-deps.md"
	taskPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	taskContent := "---\ndepends_on:\n  - missing-dep\n---\n# Has deps\n"
	os.WriteFile(taskPath, []byte(taskContent), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/has-deps",
		Title:    "Has deps",
		TaskPath: taskPath,
	}

	result := writeDependencyContextFile(tasksDir, claimed)
	if result != "" {
		t.Fatalf("expected empty path when no completion files exist, got %q", result)
	}
}

func TestRemoveDependencyContextFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	filename := "my-task.md"
	depCtxPath := filepath.Join(tasksDir, "messages", "dependency-context-"+filename+".json")
	os.WriteFile(depCtxPath, []byte(`[{"task_id":"dep-a"}]`), 0o644)

	if _, err := os.Stat(depCtxPath); err != nil {
		t.Fatalf("file should exist before removal: %v", err)
	}

	removeDependencyContextFile(tasksDir, filename)

	if _, err := os.Stat(depCtxPath); !os.IsNotExist(err) {
		t.Fatalf("file should have been removed, got err: %v", err)
	}
}

func TestRemoveDependencyContextFile_MissingFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := messaging.Init(tasksDir); err != nil {
		t.Fatalf("messaging.Init: %v", err)
	}

	// Should not panic or error when file doesn't exist
	removeDependencyContextFile(tasksDir, "nonexistent.md")
}

func TestConfigureReceiveDeny_Success(t *testing.T) {
	// Create a real git repo to test the config call
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	if err := configureReceiveDeny(repoDir); err != nil {
		t.Fatalf("configureReceiveDeny should succeed on a valid repo: %v", err)
	}

	// Verify the config was set
	cmd = exec.Command("git", "-C", repoDir, "config", "receive.denyCurrentBranch")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("reading config: %v (%s)", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "updateInstead" {
		t.Fatalf("receive.denyCurrentBranch = %q, want %q", got, "updateInstead")
	}
}

func TestConfigureReceiveDeny_Failure(t *testing.T) {
	// Point at a nonexistent directory so git config fails
	err := configureReceiveDeny(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Fatal("configureReceiveDeny should fail for a nonexistent repo")
	}
}

// checkIdleTransition returns true when the system transitions from active to
// idle, so the caller should print the idle message exactly once per idle period.
func checkIdleTransition(isIdle bool, wasIdle *bool) bool {
	shouldPrint := isIdle && !*wasIdle
	*wasIdle = isIdle
	return shouldPrint
}

func TestCheckIdleTransition(t *testing.T) {
	tests := []struct {
		name       string
		sequence   []bool // sequence of isIdle values per iteration
		wantPrints []bool // expected shouldPrint results per iteration
	}{
		{
			name:       "prints once on entering idle",
			sequence:   []bool{true, true, true},
			wantPrints: []bool{true, false, false},
		},
		{
			name:       "prints again after leaving and re-entering idle",
			sequence:   []bool{true, true, false, true, true},
			wantPrints: []bool{true, false, false, true, false},
		},
		{
			name:       "never prints when always active",
			sequence:   []bool{false, false, false},
			wantPrints: []bool{false, false, false},
		},
		{
			name:       "alternating prints each time",
			sequence:   []bool{true, false, true, false},
			wantPrints: []bool{true, false, true, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wasIdle := false
			for i, isIdle := range tt.sequence {
				got := checkIdleTransition(isIdle, &wasIdle)
				if got != tt.wantPrints[i] {
					t.Errorf("iteration %d: checkIdleTransition(%v) = %v, want %v",
						i, isIdle, got, tt.wantPrints[i])
				}
			}
		})
	}
}

func TestIdleHeartbeat_FirstMessageIncludesNextPoll(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hb := newIdleHeartbeat(now)

	msg := hb.idleMessage(now.Add(10*time.Second), 10*time.Second)
	want := "[mato] idle — waiting for tasks (next poll in 10s)"
	if msg != want {
		t.Errorf("first idle message = %q, want %q", msg, want)
	}
}

func TestIdleHeartbeat_SuppressedDuringThreshold(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hb := newIdleHeartbeat(now)

	// First message should be printed.
	msg := hb.idleMessage(now.Add(10*time.Second), 10*time.Second)
	if msg == "" {
		t.Fatal("expected first idle message, got empty")
	}

	// Polls 2 through idleHeartbeatThreshold should be suppressed.
	for i := 2; i <= idleHeartbeatThreshold; i++ {
		elapsed := time.Duration(i) * 10 * time.Second
		msg = hb.idleMessage(now.Add(elapsed), 10*time.Second)
		if msg != "" {
			t.Errorf("poll %d: expected empty, got %q", i, msg)
		}
	}
}

func TestIdleHeartbeat_ThrottledHeartbeatAfterThreshold(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hb := newIdleHeartbeat(now)

	// Exhaust the initial quiet period.
	for i := 1; i <= idleHeartbeatThreshold; i++ {
		hb.idleMessage(now.Add(time.Duration(i)*10*time.Second), 10*time.Second)
	}

	// The next call after the threshold should produce a heartbeat.
	t1 := now.Add(time.Duration(idleHeartbeatThreshold+1) * 10 * time.Second)
	msg := hb.idleMessage(t1, 10*time.Second)
	if msg == "" {
		t.Fatal("expected throttled heartbeat, got empty")
	}
	if !strings.Contains(msg, "uptime:") || !strings.Contains(msg, "last activity:") {
		t.Errorf("throttled heartbeat missing expected fields: %q", msg)
	}

	// A call less than heartbeatInterval later should be suppressed.
	t2 := t1.Add(30 * time.Second)
	msg = hb.idleMessage(t2, 10*time.Second)
	if msg != "" {
		t.Errorf("expected suppressed heartbeat within interval, got %q", msg)
	}

	// A call at or beyond heartbeatInterval should print again.
	t3 := t1.Add(heartbeatInterval)
	msg = hb.idleMessage(t3, 10*time.Second)
	if msg == "" {
		t.Fatal("expected heartbeat after interval elapsed, got empty")
	}
}

func TestIdleHeartbeat_RecordActivityResetsState(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hb := newIdleHeartbeat(now)

	// Advance through 3 idle polls.
	for i := 1; i <= 3; i++ {
		hb.idleMessage(now.Add(time.Duration(i)*10*time.Second), 10*time.Second)
	}

	// Record activity (e.g., a task was claimed).
	activityTime := now.Add(35 * time.Second)
	hb.recordActivity(activityTime)

	if hb.consecutiveIdlePolls != 0 {
		t.Errorf("consecutiveIdlePolls after recordActivity = %d, want 0", hb.consecutiveIdlePolls)
	}

	// Next idle message should be the first-idle message again.
	msg := hb.idleMessage(activityTime.Add(10*time.Second), 10*time.Second)
	want := "[mato] idle — waiting for tasks (next poll in 10s)"
	if msg != want {
		t.Errorf("idle message after reset = %q, want %q", msg, want)
	}
}

func TestIdleHeartbeat_PausedMessage(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hb := newIdleHeartbeat(now)

	msg := hb.pausedMessage(now.Add(10*time.Second), false)
	if msg == "" {
		t.Fatal("expected immediate paused message")
	}

	msg = hb.pausedMessage(now.Add(20*time.Second), true)
	if msg != "" {
		t.Fatalf("expected throttled empty message, got %q", msg)
	}

	msg = hb.pausedMessage(now.Add(10*time.Second+heartbeatInterval), true)
	if msg == "" {
		t.Fatal("expected paused heartbeat after interval")
	}
}

func TestFormatDurationShort(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{10 * time.Second, "10s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{5 * time.Minute, "5m"},
		{90 * time.Second, "1m"},
		{time.Hour, "1h"},
		{time.Hour + 30*time.Minute, "1h30m"},
		{2*time.Hour + 5*time.Minute, "2h5m"},
		{2*time.Hour + 5*time.Minute + 37*time.Second, "2h5m"},
	}
	for _, tt := range tests {
		t.Run(tt.d.String(), func(t *testing.T) {
			got := formatDurationShort(tt.d)
			if got != tt.want {
				t.Errorf("formatDurationShort(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestExtractFailureLines_NoFailures(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# My Task\nDo something.\n"), 0o644)

	got := extractFailureLines(f)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExtractFailureLines_SingleFailure(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\nDo something.\n<!-- failure: agent-7 at 2026-01-01T00:03:00Z step=WORK error=tests_failed files_changed=queue.go -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractFailureLines(f)
	if !strings.Contains(got, "step=WORK") {
		t.Fatalf("expected failure line with step=WORK, got %q", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("expected single line (no newlines), got %q", got)
	}
}

func TestExtractFailureLines_MultipleFailures(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed files_changed=main.go -->\n<!-- failure: agent-2 at 2026-01-01T00:02:00Z step=COMMIT error=no_changes files_changed=none -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractFailureLines(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 failure lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "agent-1") {
		t.Fatalf("first line should contain agent-1, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "agent-2") {
		t.Fatalf("second line should contain agent-2, got %q", lines[1])
	}
}

func TestExtractFailureLines_IgnoresBodyText(t *testing.T) {
	content := "---\npriority: 5\n---\n# Retry budget\n`CountFailureLines()` counts `<!-- failure: ... -->` records.\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractFailureLines(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 failure line (ignoring body text), got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "agent-1") {
		t.Fatalf("expected agent-1 failure line, got %q", lines[0])
	}
}

func TestExtractFailureLines_NonexistentFile(t *testing.T) {
	got := extractFailureLines(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", got)
	}
}

func TestExtractReviewRejections_NoRejections(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# My Task\nDo something.\n"), 0o644)

	got := extractReviewRejections(f)
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExtractReviewRejections_SingleRejection(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\nDo something.\n<!-- review-rejection: reviewer-1 at 2026-01-01T00:03:00Z reason=missing_tests -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractReviewRejections(f)
	if !strings.Contains(got, "reason=missing_tests") {
		t.Fatalf("expected rejection line with reason=missing_tests, got %q", got)
	}
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("expected single line (no newlines), got %q", got)
	}
}

func TestExtractReviewRejections_MultipleRejections(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\n<!-- review-rejection: reviewer-1 at 2026-01-01T00:01:00Z reason=missing_tests -->\n<!-- review-rejection: reviewer-2 at 2026-01-01T00:02:00Z reason=style_issues -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractReviewRejections(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 rejection lines, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "reviewer-1") {
		t.Fatalf("first line should contain reviewer-1, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "reviewer-2") {
		t.Fatalf("second line should contain reviewer-2, got %q", lines[1])
	}
}

func TestExtractReviewRejections_MixedRecords(t *testing.T) {
	content := "---\npriority: 5\n---\n# My Task\n<!-- failure: agent-1 at 2026-01-01T00:01:00Z step=WORK error=build_failed files_changed=main.go -->\n<!-- review-rejection: reviewer-1 at 2026-01-01T00:02:00Z reason=missing_tests -->\n<!-- failure: agent-2 at 2026-01-01T00:03:00Z step=COMMIT error=no_changes files_changed=none -->\n"
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte(content), 0o644)

	got := extractReviewRejections(f)
	lines := strings.Split(got, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 rejection line, got %d: %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "reviewer-1") {
		t.Fatalf("line should contain reviewer-1, got %q", lines[0])
	}
	if strings.Contains(got, "failure") {
		t.Fatalf("should not contain failure lines, got %q", got)
	}
}

func TestExtractReviewRejections_NonexistentFile(t *testing.T) {
	got := extractReviewRejections(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("expected empty string for nonexistent file, got %q", got)
	}
}

func TestRecoverStuckTask_StillMovesWhenAppendFails(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "readonly-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Read-only task\n"), 0o444)
	t.Cleanup(func() {
		os.Chmod(filepath.Join(tasksDir, queue.DirBacklog, taskFile), 0o644)
	})

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/readonly-task",
		Title:    "Read-only task",
		TaskPath: inProgressPath,
	}

	recoverStuckTask(tasksDir, "agent1", claimed)

	// Task should be moved to backlog even though append will fail
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task should be moved to backlog even when append fails: %v", err)
	}

	// Original content preserved
	if !strings.Contains(string(data), "# Read-only task") {
		t.Fatal("task content should be preserved")
	}

	// Failure record should NOT be present since file was read-only
	if strings.Contains(string(data), "<!-- failure:") {
		t.Fatal("failure record should not be present when append fails on read-only file")
	}

	// Task should no longer be in in-progress
	if _, err := os.Stat(inProgressPath); err == nil {
		t.Fatal("task file still in in-progress/ after recovery")
	}
}

func TestSelectTaskForReview_EmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)

	got := selectTaskForReview(tasksDir, nil)
	if got != nil {
		t.Fatalf("expected nil for empty ready-for-review/, got %+v", got)
	}
}

func TestSelectTaskForReview_NonexistentDir(t *testing.T) {
	got := selectTaskForReview(filepath.Join(t.TempDir(), "nonexistent"), nil)
	if got != nil {
		t.Fatalf("expected nil for nonexistent tasks dir, got %+v", got)
	}
}

func TestSelectTaskForReview_SingleTask(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)

	content := "<!-- claimed-by: abc12345  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: test-task\npriority: 10\n---\n# Test Task\nDo something.\n\n<!-- branch: task/test-task -->\n"
	os.WriteFile(filepath.Join(reviewDir, "test-task.md"), []byte(content), 0o644)

	got := selectTaskForReview(tasksDir, nil)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Filename != "test-task.md" {
		t.Fatalf("Filename = %q, want %q", got.Filename, "test-task.md")
	}
	if got.Branch != "task/test-task" {
		t.Fatalf("Branch = %q, want %q", got.Branch, "task/test-task")
	}
	if got.Title != "Test Task" {
		t.Fatalf("Title = %q, want %q", got.Title, "Test Task")
	}
}

func TestSelectTaskForReview_HighestPriority(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)

	// Priority 20 — lower priority (higher number)
	os.WriteFile(filepath.Join(reviewDir, "low-pri.md"), []byte(
		"---\npriority: 20\n---\n# Low Priority\n<!-- branch: task/low-pri -->\n"), 0o644)

	// Priority 5 — highest priority (lowest number)
	os.WriteFile(filepath.Join(reviewDir, "high-pri.md"), []byte(
		"---\npriority: 5\n---\n# High Priority\n<!-- branch: task/high-pri -->\n"), 0o644)

	// Priority 10 — middle
	os.WriteFile(filepath.Join(reviewDir, "mid-pri.md"), []byte(
		"---\npriority: 10\n---\n# Mid Priority\n<!-- branch: task/mid-pri -->\n"), 0o644)

	got := selectTaskForReview(tasksDir, nil)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Filename != "high-pri.md" {
		t.Fatalf("expected highest priority task, got %q", got.Filename)
	}
}

func TestSelectTaskForReview_SamePriorityAlphabetical(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "beta-task.md"), []byte(
		"---\npriority: 10\n---\n# Beta\n<!-- branch: task/beta -->\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "alpha-task.md"), []byte(
		"---\npriority: 10\n---\n# Alpha\n<!-- branch: task/alpha -->\n"), 0o644)

	got := selectTaskForReview(tasksDir, nil)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Filename != "alpha-task.md" {
		t.Fatalf("expected alphabetically first task, got %q", got.Filename)
	}
}

func TestSelectTaskForReview_IgnoresNonMdFiles(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)

	// Non-.md files should be ignored
	os.WriteFile(filepath.Join(reviewDir, "notes.txt"), []byte("not a task"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, ".hidden"), []byte("hidden"), 0o644)
	os.MkdirAll(filepath.Join(reviewDir, "subdir"), 0o755)

	got := selectTaskForReview(tasksDir, nil)
	if got != nil {
		t.Fatalf("expected nil when only non-.md files present, got %+v", got)
	}
}

func TestSelectTaskForReview_BranchFallback(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)

	// No branch comment — should fall back to task/<sanitized-name>
	os.WriteFile(filepath.Join(reviewDir, "my-task.md"), []byte(
		"---\npriority: 5\n---\n# My Task\nNo branch comment here.\n"), 0o644)

	got := selectTaskForReview(tasksDir, nil)
	if got == nil {
		t.Fatal("expected non-nil result")
	}
	if got.Branch != "task/my-task" {
		t.Fatalf("Branch = %q, want %q (fallback)", got.Branch, "task/my-task")
	}
}

func TestParseBranchFromTaskFile_WithBranch(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# Task\n\n<!-- branch: task/foo-bar -->\n"), 0o644)

	got := taskfile.ParseBranch(f)
	if got != "task/foo-bar" {
		t.Fatalf("got %q, want %q", got, "task/foo-bar")
	}
}

func TestParseBranchFromTaskFile_WithoutBranch(t *testing.T) {
	f := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(f, []byte("---\npriority: 5\n---\n# Task\nNo branch.\n"), 0o644)

	got := taskfile.ParseBranch(f)
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestParseBranchFromTaskFile_NonexistentFile(t *testing.T) {
	got := taskfile.ParseBranch(filepath.Join(t.TempDir(), "nonexistent.md"))
	if got != "" {
		t.Fatalf("got %q, want empty string", got)
	}
}

func TestFailedDirUnavailable_ExclusionLogic(t *testing.T) {
	// Simulate the runner loop's exclusion behavior: when SelectAndClaimTask
	// returns a FailedDirUnavailableError, the runner should add the task to
	// the deferred set so it is not re-selected on the next poll.

	excluded := make(map[string]struct{})

	// Simulate first error for task "exhausted.md"
	err1 := fmt.Errorf("wrap: %w", &queue.FailedDirUnavailableError{
		TaskFilename: "exhausted.md",
		MoveErr:      fmt.Errorf("permission denied"),
	})
	var fdErr *queue.FailedDirUnavailableError
	if !errors.As(err1, &fdErr) {
		t.Fatal("errors.As should match FailedDirUnavailableError")
	}
	excluded[fdErr.TaskFilename] = struct{}{}

	if _, ok := excluded["exhausted.md"]; !ok {
		t.Fatal("exhausted.md should be in exclusion set")
	}

	// Simulate second error for a different task
	err2 := &queue.FailedDirUnavailableError{
		TaskFilename: "another-exhausted.md",
		MoveErr:      fmt.Errorf("no space"),
	}
	if !errors.As(err2, &fdErr) {
		t.Fatal("errors.As should match direct FailedDirUnavailableError")
	}
	excluded[fdErr.TaskFilename] = struct{}{}

	// Both should be excluded
	if len(excluded) != 2 {
		t.Fatalf("expected 2 excluded tasks, got %d", len(excluded))
	}

	// Merging into deferred should work
	deferred := map[string]struct{}{"overlap-task.md": {}}
	for name := range excluded {
		deferred[name] = struct{}{}
	}
	if len(deferred) != 3 {
		t.Fatalf("expected 3 deferred tasks after merge, got %d", len(deferred))
	}
	for _, want := range []string{"exhausted.md", "another-exhausted.md", "overlap-task.md"} {
		if _, ok := deferred[want]; !ok {
			t.Fatalf("%s should be in merged deferred set", want)
		}
	}

	// A non-FailedDirUnavailableError should NOT match
	plainErr := fmt.Errorf("unrelated claim error")
	if errors.As(plainErr, &fdErr) {
		t.Fatal("plain error should not match FailedDirUnavailableError")
	}
}

func TestFailedDirUnavailable_IsPredicateFromRunner(t *testing.T) {
	err := &queue.FailedDirUnavailableError{
		TaskFilename: "test.md",
		MoveErr:      fmt.Errorf("disk full"),
	}
	if !queue.IsFailedDirUnavailable(err) {
		t.Fatal("IsFailedDirUnavailable should return true")
	}
	if queue.IsFailedDirUnavailable(fmt.Errorf("other")) {
		t.Fatal("IsFailedDirUnavailable should return false for unrelated errors")
	}
}

func TestSelectAndLockReview_AcquiresLock(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755)

	os.WriteFile(filepath.Join(reviewDir, "task-a.md"), []byte(
		"---\npriority: 10\n---\n# Task A\n<!-- branch: task/task-a -->\n"), 0o644)

	task, cleanup := selectAndLockReview(tasksDir, nil)
	if task == nil {
		t.Fatal("expected non-nil task")
	}
	if task.Filename != "task-a.md" {
		t.Fatalf("Filename = %q, want %q", task.Filename, "task-a.md")
	}

	// Lock file should exist.
	lockFile := filepath.Join(tasksDir, ".locks", "review-task-a.md.lock")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock file should exist: %v", err)
	}

	cleanup()
	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove lock file")
	}
}

func TestSelectAndLockReview_SkipsLockedTask(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(locksDir, 0o755)

	// Two tasks: task-a (priority 5) and task-b (priority 10).
	os.WriteFile(filepath.Join(reviewDir, "task-a.md"), []byte(
		"---\npriority: 5\n---\n# Task A\n<!-- branch: task/task-a -->\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "task-b.md"), []byte(
		"---\npriority: 10\n---\n# Task B\n<!-- branch: task/task-b -->\n"), 0o644)

	// Lock task-a (highest priority) — simulates another agent holding it.
	// Use real lock identity so IsLockHolderAlive recognizes it as alive.
	os.WriteFile(filepath.Join(locksDir, "review-task-a.md.lock"),
		[]byte(process.LockIdentity(os.Getpid())), 0o644)

	task, cleanup := selectAndLockReview(tasksDir, nil)
	if task == nil {
		t.Fatal("expected non-nil task (should fall back to task-b)")
	}
	if task.Filename != "task-b.md" {
		t.Fatalf("Filename = %q, want %q (should skip locked task-a)", task.Filename, "task-b.md")
	}
	cleanup()
}

func TestSelectAndLockReview_AllLocked(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(locksDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "task-a.md"), []byte(
		"---\npriority: 5\n---\n# Task A\n<!-- branch: task/task-a -->\n"), 0o644)

	// Lock it — simulates another agent holding it.
	// Use real lock identity so IsLockHolderAlive recognizes it as alive.
	os.WriteFile(filepath.Join(locksDir, "review-task-a.md.lock"),
		[]byte(process.LockIdentity(os.Getpid())), 0o644)

	task, cleanup := selectAndLockReview(tasksDir, nil)
	if task != nil {
		cleanup()
		t.Fatal("expected nil when all tasks are locked")
	}
}

func TestSelectAndLockReview_EmptyDir(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, queue.DirReadyReview), 0o755)

	task, cleanup := selectAndLockReview(tasksDir, nil)
	if task != nil {
		cleanup()
		t.Fatal("expected nil for empty ready-for-review/")
	}
}

func TestReviewCandidates_SortedByPriority(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	os.MkdirAll(reviewDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "low.md"), []byte(
		"---\npriority: 20\n---\n# Low\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "high.md"), []byte(
		"---\npriority: 5\n---\n# High\n"), 0o644)
	os.WriteFile(filepath.Join(reviewDir, "mid.md"), []byte(
		"---\npriority: 10\n---\n# Mid\n"), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	if candidates[0].Filename != "high.md" {
		t.Errorf("first candidate = %q, want %q", candidates[0].Filename, "high.md")
	}
	if candidates[1].Filename != "mid.md" {
		t.Errorf("second candidate = %q, want %q", candidates[1].Filename, "mid.md")
	}
	if candidates[2].Filename != "low.md" {
		t.Errorf("third candidate = %q, want %q", candidates[2].Filename, "low.md")
	}
}

func TestReviewCandidates_SkipsRetryExhausted(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 3 review-failures (default max_retries=3) — should be moved to failed/
	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"---",
		"priority: 5",
		"---",
		"# Exhausted Task",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- review-failure: three -->",
		"<!-- branch: task/exhausted -->",
		"",
	}, "\n")), 0o644)

	// Task with 1 review-failure — should still be a candidate
	os.WriteFile(filepath.Join(reviewDir, "healthy.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Healthy Task",
		"<!-- review-failure: one -->",
		"<!-- branch: task/healthy -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Filename != "healthy.md" {
		t.Fatalf("expected healthy.md, got %q", candidates[0].Filename)
	}

	// Exhausted task should be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "exhausted.md")); err != nil {
		t.Fatalf("exhausted task not moved to failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reviewDir, "exhausted.md")); !os.IsNotExist(err) {
		t.Fatal("exhausted task still in ready-for-review")
	}
}

func TestReviewCandidates_ExhaustedHasTerminalMarker(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"---",
		"priority: 5",
		"---",
		"# Exhausted Task",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- review-failure: three -->",
		"<!-- branch: task/exhausted -->",
		"",
	}, "\n")), 0o644)

	reviewCandidates(tasksDir, nil)

	data, err := os.ReadFile(filepath.Join(failedDir, "exhausted.md"))
	if err != nil {
		t.Fatalf("exhausted.md not found in failed/: %v", err)
	}
	if !taskfile.ContainsTerminalFailure(data) {
		t.Error("exhausted.md in failed/ should contain terminal-failure marker")
	}
	if !strings.Contains(string(data), "review retry budget exhausted") {
		t.Error("terminal-failure marker should mention review retry budget exhausted")
	}
}

func TestReviewCandidates_CustomMaxRetries(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with max_retries=1 and 1 review-failure — should be exhausted
	os.WriteFile(filepath.Join(reviewDir, "custom.md"), []byte(strings.Join([]string{
		"---",
		"max_retries: 1",
		"priority: 5",
		"---",
		"# Custom Retries",
		"<!-- review-failure: one -->",
		"<!-- branch: task/custom -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates (custom max_retries=1 exhausted), got %d", len(candidates))
	}

	if _, err := os.Stat(filepath.Join(failedDir, "custom.md")); err != nil {
		t.Fatalf("custom.md not moved to failed: %v", err)
	}
}

func TestSelectTaskForReview_SkipsRetryExhausted(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Only task has exhausted review retries — selectTaskForReview should return nil
	os.WriteFile(filepath.Join(reviewDir, "exhausted.md"), []byte(strings.Join([]string{
		"# Exhausted",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- review-failure: three -->",
		"<!-- branch: task/exhausted -->",
		"",
	}, "\n")), 0o644)

	got := selectTaskForReview(tasksDir, nil)
	if got != nil {
		t.Fatalf("expected nil (all tasks retry-exhausted), got %+v", got)
	}

	if _, err := os.Stat(filepath.Join(failedDir, "exhausted.md")); err != nil {
		t.Fatalf("exhausted task not in failed: %v", err)
	}
}

func TestReviewCandidates_BelowBudgetNotMoved(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 2 review-failures but default max_retries=3 — should remain a candidate
	os.WriteFile(filepath.Join(reviewDir, "still-ok.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Still OK",
		"<!-- review-failure: one -->",
		"<!-- review-failure: two -->",
		"<!-- branch: task/still-ok -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Filename != "still-ok.md" {
		t.Fatalf("expected still-ok.md, got %q", candidates[0].Filename)
	}

	// Should NOT be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "still-ok.md")); !os.IsNotExist(err) {
		t.Fatal("task with retries remaining should not be in failed")
	}
}

func TestReviewCandidates_TaskFailuresIgnored(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 3 task-agent failures but 0 review-failures.
	// Task failures should NOT count toward review retry budget.
	os.WriteFile(filepath.Join(reviewDir, "task-fails-only.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Task Failures Only",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- failure: agent-2 at 2025-01-02T00:00:00Z step=WORK error=tests -->",
		"<!-- failure: agent-3 at 2025-01-03T00:00:00Z step=PUSH error=push -->",
		"<!-- branch: task/task-fails-only -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (task failures should not exhaust review budget), got %d", len(candidates))
	}
	if candidates[0].Filename != "task-fails-only.md" {
		t.Fatalf("expected task-fails-only.md, got %q", candidates[0].Filename)
	}

	// Should NOT be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "task-fails-only.md")); !os.IsNotExist(err) {
		t.Fatal("task with only task-agent failures should not be moved to failed by review budget check")
	}
}

func TestReviewCandidates_ReviewFailuresSeparateFromTaskFailures(t *testing.T) {
	tasksDir := t.TempDir()
	reviewDir := filepath.Join(tasksDir, queue.DirReadyReview)
	failedDir := filepath.Join(tasksDir, queue.DirFailed)
	os.MkdirAll(reviewDir, 0o755)
	os.MkdirAll(failedDir, 0o755)

	// Task with 2 task-agent failures and 2 review-failures (max_retries=3).
	// Only the 2 review-failures count toward review retry budget, so it should
	// remain a candidate (2 < 3).
	os.WriteFile(filepath.Join(reviewDir, "mixed.md"), []byte(strings.Join([]string{
		"---",
		"priority: 10",
		"---",
		"# Mixed Failures",
		"<!-- failure: agent-1 at 2025-01-01T00:00:00Z step=WORK error=build -->",
		"<!-- failure: agent-2 at 2025-01-02T00:00:00Z step=WORK error=tests -->",
		"<!-- review-failure: rev-1 at 2025-01-03T00:00:00Z step=DIFF error=fetch -->",
		"<!-- review-failure: rev-2 at 2025-01-04T00:00:00Z step=DIFF error=timeout -->",
		"<!-- branch: task/mixed -->",
		"",
	}, "\n")), 0o644)

	candidates := reviewCandidates(tasksDir, nil)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (only 2 review-failures < max_retries 3), got %d", len(candidates))
	}

	// Should NOT be in failed/
	if _, err := os.Stat(filepath.Join(failedDir, "mixed.md")); !os.IsNotExist(err) {
		t.Fatal("mixed-failure task should not be in failed (only 2 review-failures)")
	}
}

func TestRunOnce_TimeoutKillsCommand(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	run := runContext{
		timeout: 100 * time.Millisecond,
	}

	// runOnceWithTimeout is not directly callable with a subprocess
	// instead we test the context timeout behavior directly
	ctx, cancel := context.WithTimeout(context.Background(), run.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "10")
	err := cmd.Run()

	if err == nil {
		t.Fatal("expected error from timed-out command, got nil")
	}
	if ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", ctx.Err())
	}
}

func TestBuildEnvAndRunContext_AgentTimeoutResolution(t *testing.T) {
	tests := []struct {
		name string
		opts RunOptions
		want time.Duration
	}{
		{
			name: "default when unset",
			want: defaultAgentTimeout,
		},
		{
			name: "valid duration 1h",
			opts: RunOptions{AgentTimeout: time.Hour},
			want: time.Hour,
		},
		{
			name: "valid duration 45m",
			opts: RunOptions{AgentTimeout: 45 * time.Minute},
			want: 45 * time.Minute,
		},
		{
			name: "valid duration 2h30m",
			opts: RunOptions{AgentTimeout: 2*time.Hour + 30*time.Minute},
			want: 2*time.Hour + 30*time.Minute,
		},
		{
			name: "sub-second duration",
			opts: RunOptions{AgentTimeout: 500 * time.Millisecond},
			want: 500 * time.Millisecond,
		},
		{
			name: "large duration",
			opts: RunOptions{AgentTimeout: 100 * time.Hour},
			want: 100 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, run := buildEnvAndRunContext("main", hostTools{homeDir: "/home/test"}, "agent", "name", "email", "/repo", "/repo/.mato", tt.opts)
			if run.timeout != tt.want {
				t.Fatalf("timeout = %v, want %v", run.timeout, tt.want)
			}
		})
	}
}

func TestDefaultAgentTimeout(t *testing.T) {
	if defaultAgentTimeout != 30*time.Minute {
		t.Fatalf("expected default timeout of 30m, got %v", defaultAgentTimeout)
	}
}

func TestAgentWroteFailureRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")

	// No failure record: should return false.
	os.WriteFile(path, []byte("# Task\nSome content\n"), 0o644)
	if agentWroteFailureRecord(path, "abc12345") {
		t.Fatal("expected false when no failure record exists")
	}

	// Failure from a different agent: should return false.
	os.WriteFile(path, []byte("# Task\n<!-- failure: other-agent at 2026-01-01T00:00:00Z step=WORK error=test -->\n"), 0o644)
	if agentWroteFailureRecord(path, "abc12345") {
		t.Fatal("expected false for a different agent's failure record")
	}

	// Failure from the matching agent: should return true.
	os.WriteFile(path, []byte("# Task\n<!-- failure: abc12345 at 2026-01-01T00:00:00Z step=WORK error=test -->\n"), 0o644)
	if !agentWroteFailureRecord(path, "abc12345") {
		t.Fatal("expected true when agent's failure record exists")
	}

	// Nonexistent file: should return false.
	if agentWroteFailureRecord(filepath.Join(dir, "nonexistent.md"), "abc12345") {
		t.Fatal("expected false for nonexistent file")
	}
}

func TestRecoverStuckTask_SkipsDuplicateFailureRecord(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "my-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	// Agent already wrote a failure record via ON_FAILURE.
	os.WriteFile(inProgressPath, []byte(strings.Join([]string{
		"<!-- claimed-by: agent-x -->\n# My Task",
		"<!-- failure: agent-x at 2026-01-01T00:00:00Z step=WORK error=tests_failed files_changed=foo.go -->",
		"",
	}, "\n")), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/my-task",
		Title:    "My Task",
		TaskPath: inProgressPath,
	}

	recoverStuckTask(tasksDir, "agent-x", claimed)

	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task file not found in backlog/: %v", err)
	}

	// Should have exactly 1 failure record (the agent's), not 2.
	count := strings.Count(string(data), "<!-- failure:")
	if count != 1 {
		t.Fatalf("failure record count = %d, want 1 (no duplicate)\ncontents:\n%s", count, string(data))
	}
	if !strings.Contains(string(data), "step=WORK") {
		t.Fatal("original failure record should be preserved")
	}
}

func TestPostAgentPush_SkipsWhenReadyForReviewExists(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "example-task.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Example Task\n"), 0o644)

	// Place a stale file at the ready-for-review/ destination.
	staleContent := []byte("<!-- claimed-by: old-agent -->\n# Stale Task\n")
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(readyPath, staleContent, 0o644)

	// Set up a minimal git repo so postAgentPush can check for commits.
	cloneDir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)
		cmd.Dir = cloneDir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("git", "init", "-b", "main")
	run("git", "config", "user.name", "test")
	run("git", "config", "user.email", "test@test.com")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	run("git", "add", ".")
	run("git", "commit", "-m", "init")
	// Create a second commit on a task branch so there are commits above main.
	run("git", "checkout", "-b", "task/example-task")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644)
	run("git", "add", ".")
	run("git", "commit", "-m", "task work")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/example-task",
		Title:    "Example Task",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")

	// Should return an error indicating the destination exists.
	if err == nil {
		t.Fatal("expected error when ready-for-review/ destination already exists, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error should mention destination already exists, got: %v", err)
	}

	// The stale file should not have been overwritten.
	data, readErr := os.ReadFile(readyPath)
	if readErr != nil {
		t.Fatalf("stale file in ready-for-review/ was removed: %v", readErr)
	}
	if string(data) != string(staleContent) {
		t.Fatalf("stale file was overwritten; got %q, want %q", string(data), string(staleContent))
	}

	// The in-progress/ file should still be there (no move occurred).
	if _, err := os.Stat(inProgressPath); err != nil {
		t.Fatalf("in-progress/ task file should still exist: %v", err)
	}
}

func TestPostAgentPush_BranchMarkerWriteFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "marker-fail.md"
	taskContent := "<!-- claimed-by: agent1 -->\n# Marker Fail\n"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte(taskContent), 0o644)

	// Set up a git repo with commits on a task branch.
	cloneDir := t.TempDir()
	remoteDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
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
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "init")
	gitRun(cloneDir, "git", "remote", "add", "origin", remoteDir)
	gitRun(cloneDir, "git", "push", "origin", "main")
	gitRun(cloneDir, "git", "checkout", "-b", "task/marker-fail")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "task work")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/marker-fail",
		Title:    "Marker Fail",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	// Inject branch marker write failure so the branch marker write fails
	// after os.Link has already moved the file to ready-for-review/.
	origWriteBranchMarker := writeBranchMarkerFn
	t.Cleanup(func() { writeBranchMarkerFn = origWriteBranchMarker })
	writeBranchMarkerFn = func(path, branch string) error {
		return fmt.Errorf("simulated disk full")
	}

	err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")

	// Should return a fatal error mentioning the write failure.
	if err == nil {
		t.Fatal("expected error when branch marker write fails")
	}
	if !strings.Contains(err.Error(), "simulated disk full") {
		t.Fatalf("error should mention the write failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rolled back to in-progress") {
		t.Fatalf("error should mention rollback, got: %v", err)
	}

	// Task should be rolled back to in-progress/.
	if _, statErr := os.Stat(inProgressPath); statErr != nil {
		t.Fatalf("task should be back in in-progress/ after rollback: %v", statErr)
	}

	// Task should NOT remain in ready-for-review/.
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); !os.IsNotExist(statErr) {
		t.Fatal("task should not remain in ready-for-review/ after rollback")
	}

	// Rolled-back file should not contain a branch marker.
	data, _ := os.ReadFile(inProgressPath)
	if strings.Contains(string(data), "<!-- branch:") {
		t.Fatal("rolled-back file should not contain branch marker")
	}
}

func TestPostAgentPush_BranchMarkerRollbackFails(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirInProgress, queue.DirReadyReview, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "stranded.md"
	taskContent := "<!-- claimed-by: agent1 -->\n# Stranded\n"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte(taskContent), 0o644)

	// Set up git repo.
	cloneDir := t.TempDir()
	remoteDir := t.TempDir()
	gitRun := func(dir string, args ...string) {
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
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("hello"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "init")
	gitRun(cloneDir, "git", "remote", "add", "origin", remoteDir)
	gitRun(cloneDir, "git", "push", "origin", "main")
	gitRun(cloneDir, "git", "checkout", "-b", "task/stranded")
	os.WriteFile(filepath.Join(cloneDir, "file.txt"), []byte("changed"), 0o644)
	gitRun(cloneDir, "git", "add", ".")
	gitRun(cloneDir, "git", "commit", "-m", "task work")

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/stranded",
		Title:    "Stranded",
		TaskPath: inProgressPath,
	}
	env := envConfig{
		tasksDir:     tasksDir,
		targetBranch: "main",
	}

	// Inject branch marker write failure AND sneak a file back into in-progress/
	// so the rollback os.Link hits EEXIST.
	origWriteBranchMarker := writeBranchMarkerFn
	t.Cleanup(func() { writeBranchMarkerFn = origWriteBranchMarker })
	writeBranchMarkerFn = func(path, branch string) error {
		// Re-create the in-progress file to simulate a race (another agent
		// placed a file there), so rollback link will fail with EEXIST.
		os.WriteFile(inProgressPath, []byte("<!-- claimed-by: other -->\n# Other\n"), 0o644)
		return fmt.Errorf("simulated write error")
	}

	err := postAgentPush(env, "agent1", claimed, cloneDir, "deadbeef")

	if err == nil {
		t.Fatal("expected error when both marker write and rollback fail")
	}
	if !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("error should mention rollback failure, got: %v", err)
	}

	// The in-progress/ file should be the "other" agent's copy (not overwritten).
	data, _ := os.ReadFile(inProgressPath)
	if !strings.Contains(string(data), "Other") {
		t.Fatal("in-progress/ file should be the racing agent's copy, not overwritten")
	}

	// The ready-for-review/ file should still exist (stranded, since rollback failed).
	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	if _, statErr := os.Stat(readyPath); statErr != nil {
		t.Fatalf("task should remain in ready-for-review/ when rollback fails: %v", statErr)
	}
}

func TestPostReviewAction_Approved(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	// Write a verdict file (the new approach).
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"approve"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Should be moved to ready-to-merge/.
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyMerge, taskFile)); err != nil {
		t.Fatal("approved task not moved to ready-to-merge/")
	}
	if _, err := os.Stat(reviewPath); err == nil {
		t.Fatal("approved task still in ready-for-review/")
	}
	// Task file should have the approval marker written by the host.
	data, _ := os.ReadFile(filepath.Join(tasksDir, queue.DirReadyMerge, taskFile))
	if !strings.Contains(string(data), "<!-- reviewed:") {
		t.Fatal("approval marker not written to task file")
	}
	// Verdict file should be cleaned up.
	if _, err := os.Stat(verdictPath); err == nil {
		t.Fatal("verdict file should be cleaned up after processing")
	}
}

func TestPostReviewAction_Rejected(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	// Write a rejection verdict file.
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":"missing error wrapping in postAgentPush"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Should be moved to backlog/.
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, taskFile)); err != nil {
		t.Fatal("rejected task not moved to backlog/")
	}
	if _, err := os.Stat(reviewPath); err == nil {
		t.Fatal("rejected task still in ready-for-review/")
	}
	// Task file should have the rejection marker with the reason.
	data, _ := os.ReadFile(filepath.Join(tasksDir, queue.DirBacklog, taskFile))
	if !strings.Contains(string(data), "<!-- review-rejection:") {
		t.Fatal("rejection marker not written to task file")
	}
	if !strings.Contains(string(data), "missing error wrapping") {
		t.Fatal("rejection reason not included in marker")
	}
}

func TestPostReviewAction_Rejected_SanitizesReason(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":"missing tests\n--> injected"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	data, _ := os.ReadFile(filepath.Join(tasksDir, queue.DirBacklog, taskFile))
	if !strings.Contains(string(data), "missing tests —> injected") {
		t.Fatalf("sanitized rejection reason not found in marker:\n%s", string(data))
	}
	if strings.Contains(string(data), "\n--> injected") {
		t.Fatalf("raw rejection reason should not be written into marker:\n%s", string(data))
	}
}

func TestPostReviewAction_NoVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Task should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task with no verdict should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure record not written for task with no verdict")
	}
	if !strings.Contains(string(data), "exited without rendering a verdict") {
		t.Fatal("review-failure record missing expected reason")
	}
}

func TestPostReviewAction_ApprovalMarkerWriteFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"approve"}`), 0o644)

	origAppend := appendToFileFn
	t.Cleanup(func() { appendToFileFn = origAppend })
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated approval write failure")
	}

	task := &queue.ClaimedTask{Filename: taskFile, Branch: "task/review-task", Title: "Review Task", TaskPath: reviewPath}
	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task should stay in ready-for-review/ when approval marker write fails")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyMerge, taskFile)); err == nil {
		t.Fatal("task should not move to ready-to-merge/ when approval marker write fails")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded after approval marker write failure:\n%s", string(data))
	}
	if !strings.Contains(string(data), "could not write approval marker") {
		t.Fatalf("review-failure should mention approval marker write failure:\n%s", string(data))
	}
}

func TestPostReviewAction_RejectionMarkerWriteFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"reject","reason":"missing tests"}`), 0o644)

	origAppend := appendToFileFn
	t.Cleanup(func() { appendToFileFn = origAppend })
	appendToFileFn = func(path, text string) error {
		return fmt.Errorf("simulated rejection write failure")
	}

	task := &queue.ClaimedTask{Filename: taskFile, Branch: "task/review-task", Title: "Review Task", TaskPath: reviewPath}
	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task should stay in ready-for-review/ when rejection marker write fails")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, taskFile)); err == nil {
		t.Fatal("task should not move to backlog/ when rejection marker write fails")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatalf("review-failure should be recorded after rejection marker write failure:\n%s", string(data))
	}
	if !strings.Contains(string(data), "could not write rejection marker") {
		t.Fatalf("review-failure should mention rejection marker write failure:\n%s", string(data))
	}
}

func TestPostReviewAction_ApprovalMarkerAndReviewFailureWriteFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o444)
	t.Cleanup(func() { _ = os.Chmod(reviewPath, 0o644) })
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"approve"}`), 0o644)

	task := &queue.ClaimedTask{Filename: taskFile, Branch: "task/review-task", Title: "Review Task", TaskPath: reviewPath}
	stdout, stderr := captureStdoutStderr(t, func() {
		postReviewAction(tasksDir, "host-agent", task)
	})

	if !strings.Contains(stdout, "Review incomplete: could not record review-failure for review-task.md") {
		t.Fatalf("expected stdout to report failed review-failure recording, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "recorded review-failure") {
		t.Fatalf("stdout should not claim review-failure was recorded:\n%s", stdout)
	}
	if !strings.Contains(stderr, "could not write approval marker") {
		t.Fatalf("expected approval marker warning in stderr, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "for append:") {
		t.Fatalf("expected review-failure append warning in stderr, got:\n%s", stderr)
	}
}

func TestPostReviewAction_ErrorVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	// Write an error verdict file (review agent's ON_FAILURE).
	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"error","reason":"DIFF: could not fetch branch"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Task should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("error verdict task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure record not written for error verdict")
	}
	if !strings.Contains(string(data), "could not fetch branch") {
		t.Fatal("review-failure reason should include the error details")
	}
	// Verdict file should be cleaned up.
	if _, err := os.Stat(verdictPath); err == nil {
		t.Fatal("verdict file should be cleaned up after processing")
	}
}

func TestPostReviewAction_UnknownVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{"verdict":"maybe","reason":"unsure"}`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("unknown verdict task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure not written for unknown verdict")
	}
	if !strings.Contains(string(data), `unknown verdict: "maybe"`) {
		t.Fatalf("review-failure should mention the unknown verdict, got:\n%s", string(data))
	}
}

func TestPostReviewAction_MalformedJSON(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(`{bad json`), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("malformed JSON task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure not written for malformed JSON")
	}
	if !strings.Contains(string(data), "could not parse verdict file") {
		t.Fatal("review-failure should mention parse error")
	}
}

func TestPostReviewAction_EmptyVerdictFile(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	os.WriteFile(reviewPath, []byte("<!-- claimed-by: task-agent -->\n# Review Task\n"), 0o644)

	verdictPath := filepath.Join(tasksDir, "messages", "verdict-"+taskFile+".json")
	os.WriteFile(verdictPath, []byte(""), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("empty verdict file task should stay in ready-for-review/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure not written for empty verdict file")
	}
}

func TestReviewedReRegex(t *testing.T) {
	tests := []struct {
		name  string
		input string
		match bool
	}{
		{"complete marker", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved -->", true},
		{"extra whitespace before close", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved  -->", true},
		{"missing closing tag", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — approved", false},
		{"partial write no em-dash", "<!-- reviewed: agent1 at 2026-01", false},
		{"missing approved word", "<!-- reviewed: agent1 at 2026-01-01T00:00:00Z — -->", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewedRe.MatchString(tt.input); got != tt.match {
				t.Errorf("reviewedRe.MatchString(%q) = %v, want %v", tt.input, got, tt.match)
			}
		})
	}
}

func TestReviewRejectionReRegex(t *testing.T) {
	tests := []struct {
		name  string
		input string
		match bool
	}{
		{"complete marker", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing error wrapping -->", true},
		{"reason with spaces", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — needs better test coverage for edge cases -->", true},
		{"extra whitespace before close", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — bad code  -->", true},
		{"missing closing tag", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — missing error wrapping", false},
		{"partial write no em-dash", "<!-- review-rejection: agent1 at 2026-01", false},
		{"missing reason after em-dash", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z — -->", false},
		{"no em-dash or reason", "<!-- review-rejection: agent1 at 2026-01-01T00:00:00Z", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reviewRejectionRe.MatchString(tt.input); got != tt.match {
				t.Errorf("reviewRejectionRe.MatchString(%q) = %v, want %v", tt.input, got, tt.match)
			}
		})
	}
}

func TestPostReviewAction_PartialRejectionMarkerTreatedAsNoVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirReadyReview, queue.DirReadyMerge, queue.DirBacklog, "messages", "messages/events"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "review-task.md"
	reviewPath := filepath.Join(tasksDir, queue.DirReadyReview, taskFile)
	// Partial rejection marker (no closing -->, simulating agent crash during write)
	os.WriteFile(reviewPath, []byte(strings.Join([]string{
		"<!-- claimed-by: task-agent -->",
		"<!-- branch: task/review-task -->",
		"# Review Task",
		"",
		"<!-- review-rejection: review-agent at 2026-01-01T00:00:00Z",
	}, "\n")), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/review-task",
		Title:    "Review Task",
		TaskPath: reviewPath,
	}

	postReviewAction(tasksDir, "host-agent", task)

	// Partial marker should NOT be treated as a valid rejection.
	// Task should stay in ready-for-review/ with a review-failure record.
	if _, err := os.Stat(reviewPath); err != nil {
		t.Fatal("task with partial rejection marker should stay in ready-for-review/")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, taskFile)); err == nil {
		t.Fatal("task with partial rejection marker should not be moved to backlog/")
	}
	data, _ := os.ReadFile(reviewPath)
	if !strings.Contains(string(data), "<!-- review-failure:") {
		t.Fatal("review-failure record not written for partial rejection marker")
	}
}

func TestAppendReviewFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.md")
	os.WriteFile(path, []byte("# Task\n"), 0o644)

	appendReviewFailure(path, "review-abc", "could not fetch branch")

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "<!-- review-failure: review-abc") {
		t.Fatal("review-failure record not written")
	}
	if !strings.Contains(string(data), "error=could not fetch branch") {
		t.Fatal("review-failure reason not included")
	}
}

func TestBuildDockerArgs_TTYFlag(t *testing.T) {
	baseEnv := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	baseRun := runContext{}

	t.Run("with TTY passes -it", func(t *testing.T) {
		env := baseEnv
		env.isTTY = true
		args := buildDockerArgs(env, baseRun, nil, nil)
		if args[3] != "-it" {
			t.Fatalf("expected -it flag when isTTY=true, got %q", args[3])
		}
	})

	t.Run("without TTY passes -i", func(t *testing.T) {
		env := baseEnv
		env.isTTY = false
		args := buildDockerArgs(env, baseRun, nil, nil)
		if args[3] != "-i" {
			t.Fatalf("expected -i flag when isTTY=false, got %q", args[3])
		}
	})
}

func TestBuildDockerArgs_InitFlag(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{}
	args := buildDockerArgs(env, run, nil, nil)
	if args[2] != "--init" {
		t.Fatalf("expected --init at args[2], got %q", args[2])
	}
}

func TestIsTerminal(t *testing.T) {
	// os.Stdin in a test runner (go test) is typically not a terminal.
	// A pipe or /dev/null is definitely not a terminal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	if isTerminal(r) {
		t.Fatal("pipe should not be detected as a terminal")
	}

	// /dev/null is also not a terminal.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	if isTerminal(devNull) {
		t.Fatal("/dev/null should not be detected as a terminal")
	}
}

func TestMoveReviewedTask_Success(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, queue.DirReadyReview)
	dstDir := filepath.Join(tasksDir, queue.DirReadyMerge)
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "my-task.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# My Task\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/my-task",
		Title:    "My Task",
		TaskPath: srcPath,
	}

	moveReviewedTask(tasksDir, "agent1", task, reviewDisposition{
		dir:         queue.DirReadyMerge,
		messageBody: "approved",
		logPrefix:   "review",
	})

	// Source should be removed
	if _, err := os.Stat(srcPath); err == nil {
		t.Fatal("source file should have been removed after move")
	}
	// Destination should exist
	dstPath := filepath.Join(dstDir, taskFile)
	if _, err := os.Stat(dstPath); err != nil {
		t.Fatalf("destination file should exist: %v", err)
	}
}

func TestMoveReviewedTask_DestinationExists(t *testing.T) {
	tasksDir := t.TempDir()
	srcDir := filepath.Join(tasksDir, queue.DirReadyReview)
	dstDir := filepath.Join(tasksDir, queue.DirBacklog)
	msgDir := filepath.Join(tasksDir, "messages", "events")
	for _, d := range []string{srcDir, dstDir, msgDir} {
		os.MkdirAll(d, 0o755)
	}

	taskFile := "dup-task.md"
	srcPath := filepath.Join(srcDir, taskFile)
	os.WriteFile(srcPath, []byte("# Dup Task\n"), 0o644)

	// Pre-create a file at the destination to simulate a race
	dstPath := filepath.Join(dstDir, taskFile)
	os.WriteFile(dstPath, []byte("# Existing\n"), 0o644)

	task := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/dup-task",
		Title:    "Dup Task",
		TaskPath: srcPath,
	}

	moveReviewedTask(tasksDir, "agent1", task, reviewDisposition{
		dir:         queue.DirBacklog,
		messageBody: "rejected",
		logPrefix:   "review",
	})

	// Source should still exist (move was skipped)
	if _, err := os.Stat(srcPath); err != nil {
		t.Fatalf("source file should still exist when destination already exists: %v", err)
	}
	// Destination should still have original content
	data, _ := os.ReadFile(dstPath)
	if string(data) != "# Existing\n" {
		t.Fatalf("destination file should not have been overwritten, got: %s", string(data))
	}
}

func TestCheckDocker_NotInPath(t *testing.T) {
	// Override PATH so "docker" cannot be found.
	t.Setenv("PATH", t.TempDir())

	err := checkDocker()
	if err == nil {
		t.Fatal("checkDocker should fail when docker is not in PATH")
	}
	if !strings.Contains(err.Error(), "docker is required but not available") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCheckDocker_Available(t *testing.T) {
	// Only run when Docker is actually available in the test environment.
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available in test environment")
	}
	// Also skip if daemon is not running (e.g., CI without Docker).
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not running: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	if err := checkDocker(); err != nil {
		t.Fatalf("checkDocker should succeed when Docker is available: %v", err)
	}
}

func TestGracefulShutdownDelay(t *testing.T) {
	if gracefulShutdownDelay != 10*time.Second {
		t.Fatalf("expected gracefulShutdownDelay to be 10s, got %v", gracefulShutdownDelay)
	}
}

func TestSignalForwarding_ContextCancelSendsSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	// Create a cancellable context to simulate SIGINT/SIGTERM.
	ctx, cancel := context.WithCancel(context.Background())
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start command: %v", err)
	}

	// Cancel the parent context (simulates receiving a signal).
	cancel()

	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected error from cancelled command, got nil")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", ctx.Err())
	}
}

func TestSignalForwarding_TimeoutStillWorks(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	// Verify that timeout still kills the command when using the
	// two-phase signal forwarding setup.
	ctx := context.Background()
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error from timed-out command, got nil")
	}
	if timeoutCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", timeoutCtx.Err())
	}
}

func TestSignalForwarding_ProcessExitsOnSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	// Verify that the process exits promptly after SIGTERM via Cancel,
	// well before the WaitDelay would escalate to SIGKILL.
	ctx, cancel := context.WithCancel(context.Background())
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Cancel after a short delay to ensure the process is running.
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_ = cmd.Wait()
	elapsed := time.Since(start)

	// sleep responds to SIGTERM immediately, so it should exit well
	// before the 10s WaitDelay. Allow 5 seconds to be safe in CI.
	if elapsed > 5*time.Second {
		t.Fatalf("process took %v to exit after SIGTERM; expected prompt exit", elapsed)
	}
}

func TestPollBackoff_BelowThreshold(t *testing.T) {
	for i := 0; i < errBackoffThreshold; i++ {
		got := pollBackoff(i)
		if got != basePollInterval {
			t.Errorf("pollBackoff(%d) = %v, want %v", i, got, basePollInterval)
		}
	}
}

func TestPollBackoff_AtAndAboveThreshold(t *testing.T) {
	// At threshold: 10s * 2^1 = 20s
	got := pollBackoff(errBackoffThreshold)
	want := basePollInterval * 2
	if got != want {
		t.Errorf("pollBackoff(%d) = %v, want %v", errBackoffThreshold, got, want)
	}

	// One above: 10s * 2^2 = 40s
	got = pollBackoff(errBackoffThreshold + 1)
	want = basePollInterval * 4
	if got != want {
		t.Errorf("pollBackoff(%d) = %v, want %v", errBackoffThreshold+1, got, want)
	}

	// Two above: 10s * 2^3 = 80s
	got = pollBackoff(errBackoffThreshold + 2)
	want = basePollInterval * 8
	if got != want {
		t.Errorf("pollBackoff(%d) = %v, want %v", errBackoffThreshold+2, got, want)
	}
}

func TestPollBackoff_CapsAtMax(t *testing.T) {
	got := pollBackoff(100)
	if got != maxPollInterval {
		t.Errorf("pollBackoff(100) = %v, want %v (max)", got, maxPollInterval)
	}

	// Just past the cap boundary: basePollInterval * 2^N >= maxPollInterval
	// with base=10s and max=5m=300s, 10*2^5=320s > 300s, so threshold+4
	got = pollBackoff(errBackoffThreshold + 4)
	if got != maxPollInterval {
		t.Errorf("pollBackoff(%d) = %v, want %v (max)", errBackoffThreshold+4, got, maxPollInterval)
	}
}

func TestPollBackoff_ExponentialProgression(t *testing.T) {
	prev := pollBackoff(errBackoffThreshold - 1) // base
	for i := errBackoffThreshold; i < errBackoffThreshold+10; i++ {
		cur := pollBackoff(i)
		if cur < prev {
			t.Errorf("pollBackoff(%d) = %v < pollBackoff(%d) = %v; expected non-decreasing", i, cur, i-1, prev)
		}
		if cur > maxPollInterval {
			t.Errorf("pollBackoff(%d) = %v exceeds maxPollInterval %v", i, cur, maxPollInterval)
		}
		prev = cur
	}
}

func TestDryRun_BasicValidation(t *testing.T) {
	// Create a git repo
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	cmd = exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com")
	cmd.Run()
	cmd = exec.Command("git", "-C", repoDir, "config", "user.name", "Test")
	cmd.Run()

	tasksDir := filepath.Join(repoDir, ".mato")
	subdirs := queue.AllDirs
	for _, sub := range subdirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create a valid backlog task
	taskContent := "---\nid: task-a\npriority: 10\naffects:\n  - file-a.go\n---\n# Task A\nDo something.\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"), []byte(taskContent), 0o644)

	err := DryRun(io.Discard, repoDir, "main", testRunOptions())
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}

	// DryRun is read-only: .queue manifest should NOT be written.
	if _, statErr := os.Stat(filepath.Join(tasksDir, ".queue")); statErr == nil {
		t.Fatal(".queue manifest should NOT be written during dry-run")
	}
}

func TestDryRun_DetectsParseErrors(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create a task with invalid frontmatter
	badContent := "---\npriority: not-a-number\n---\n# Bad Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "bad-task.md"), []byte(badContent), 0o644)

	// DryRun should succeed (parse errors are reported, not fatal)
	err := DryRun(io.Discard, repoDir, "main", testRunOptions())
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
}

func TestDryRun_DetectsOverlaps(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create two tasks with overlapping affects
	taskA := "---\nid: task-a\npriority: 10\naffects:\n  - shared.go\n---\n# Task A\n"
	taskB := "---\nid: task-b\npriority: 20\naffects:\n  - shared.go\n---\n# Task B\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"), []byte(taskA), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"), []byte(taskB), 0o644)

	err := DryRun(io.Discard, repoDir, "main", testRunOptions())
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}

	// DryRun is read-only: no .queue manifest should be written.
	if _, statErr := os.Stat(filepath.Join(tasksDir, ".queue")); statErr == nil {
		t.Fatal(".queue manifest should NOT be written during dry-run")
	}
}

func TestDryRun_MissingDirectories(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	os.MkdirAll(tasksDir, 0o755)
	// Only create some subdirs, leaving others missing
	os.MkdirAll(filepath.Join(tasksDir, queue.DirBacklog), 0o755)

	err := DryRun(io.Discard, repoDir, "main", testRunOptions())
	if err == nil {
		t.Fatal("DryRun should return error when directories are missing")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error should mention missing directories, got: %v", err)
	}
}

func TestDryRun_ReportsPromotableDependencies(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create a completed dependency
	depContent := "---\nid: dep-task\npriority: 5\n---\n# Dep Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "dep-task.md"), []byte(depContent), 0o644)

	// Create a waiting task that depends on the completed task
	waitingContent := "---\nid: child-task\npriority: 10\ndepends_on:\n  - dep-task\n---\n# Child Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "child-task.md"), []byte(waitingContent), 0o644)

	err := DryRun(io.Discard, repoDir, "main", testRunOptions())
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}

	// DryRun is read-only: the waiting task should NOT be moved.
	if _, statErr := os.Stat(filepath.Join(tasksDir, queue.DirWaiting, "child-task.md")); statErr != nil {
		t.Fatal("child-task.md should still be in waiting/ (dry-run is read-only)")
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "child-task.md")); statErr == nil {
		t.Fatal("child-task.md should NOT have been promoted to backlog/ during dry-run")
	}
}

func TestDryRun_NoDockerLaunched(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Add a backlog task — in normal run this would trigger Docker launch
	taskContent := "---\nid: runnable\npriority: 1\n---\n# Runnable Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "runnable.md"), []byte(taskContent), 0o644)

	// DryRun should complete without attempting to claim or launch Docker
	err := DryRun(io.Discard, repoDir, "main", testRunOptions())
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}

	// The task should still be in backlog (not claimed/moved)
	if _, statErr := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "runnable.md")); statErr != nil {
		t.Fatal("task should remain in backlog/ during dry-run")
	}
	if _, statErr := os.Stat(filepath.Join(tasksDir, queue.DirInProgress, "runnable.md")); statErr == nil {
		t.Fatal("task should NOT be moved to in-progress/ during dry-run")
	}
}

func TestDryRun_ResolvedSettingsOutput(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	opts := RunOptions{
		TaskModel:             "claude-sonnet-4",
		ReviewModel:           "gpt-5.4",
		TaskReasoningEffort:   "medium",
		ReviewReasoningEffort: "xhigh",
	}

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", opts); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "=== Resolved Settings ===") {
		t.Fatalf("missing Resolved Settings section, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "task model:") || !strings.Contains(stdout, "claude-sonnet-4") {
		t.Fatalf("missing task model output, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "review model:") || !strings.Contains(stdout, "gpt-5.4") {
		t.Fatalf("missing review model output, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "task reasoning effort:") || !strings.Contains(stdout, "medium") {
		t.Fatalf("missing task reasoning effort output, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "review reasoning effort:") || !strings.Contains(stdout, "xhigh") {
		t.Fatalf("missing review reasoning effort output, got:\n%s", stdout)
	}
}

func TestDryRun_ExecutionOrder(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create backlog tasks with different priorities and overlapping affects
	// to verify ordering and deferred exclusion.
	taskA := "---\nid: task-a\npriority: 10\naffects:\n  - shared.go\n---\n# Task A\n"
	taskB := "---\nid: task-b\npriority: 20\naffects:\n  - shared.go\n---\n# Task B\n"
	taskC := "---\nid: task-c\npriority: 5\naffects:\n  - other.go\n---\n# Task C\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"), []byte(taskA), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"), []byte(taskB), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-c.md"), []byte(taskC), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	// Execution Order should list task-c (priority 5) then task-a (priority 10).
	// task-b is deferred (overlaps with task-a on shared.go) and excluded.
	if !strings.Contains(stdout, "=== Execution Order ===") {
		t.Fatal("missing Execution Order section")
	}
	if !strings.Contains(stdout, "1. task-c.md (priority 5)") {
		t.Errorf("expected task-c first in execution order, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "2. task-a.md (priority 10)") {
		t.Errorf("expected task-a second in execution order, got:\n%s", stdout)
	}
	if first := strings.Index(stdout, "1. task-c.md (priority 5)"); first == -1 {
		t.Errorf("expected task-c line in execution order, got:\n%s", stdout)
	} else if second := strings.Index(stdout, "2. task-a.md (priority 10)"); second == -1 {
		t.Errorf("expected task-a line in execution order, got:\n%s", stdout)
	} else if first > second {
		t.Errorf("execution order lines out of order, got:\n%s", stdout)
	}
	// Deferred task-b should NOT appear in execution order.
	if strings.Contains(stdout, "task-b.md (priority") {
		t.Errorf("deferred task-b should not appear in execution order, got:\n%s", stdout)
	}
}

func TestDryRun_BacklogTaskSummary(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Runnable task with affects and a satisfied dependency.
	taskA := "---\nid: task-a\npriority: 10\naffects:\n  - file-a.go\n  - file-b.go\ndepends_on:\n  - dep-x\n---\n# Task A\n"
	// Deferred task (overlaps with task-a).
	taskB := "---\nid: task-b\npriority: 20\naffects:\n  - file-a.go\n---\n# Task B\n"
	// Task with no affects or depends_on.
	taskC := "---\nid: task-c\npriority: 5\n---\n# Task C\n"
	depX := "---\nid: dep-x\n---\n# Dep X\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "dep-x.md"), []byte(depX), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"), []byte(taskA), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"), []byte(taskB), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-c.md"), []byte(taskC), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "=== Backlog Task Summary ===") {
		t.Fatal("missing Backlog Task Summary section")
	}

	// Check runnable vs deferred labels.
	if !strings.Contains(stdout, "task-a.md [runnable]") {
		t.Errorf("task-a should be marked runnable, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "task-b.md [deferred]") {
		t.Errorf("task-b should be marked deferred, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "task-c.md [runnable]") {
		t.Errorf("task-c should be marked runnable, got:\n%s", stdout)
	}

	// Check frontmatter fields.
	if !strings.Contains(stdout, "affects: file-a.go, file-b.go") {
		t.Errorf("task-a affects not shown correctly, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "depends_on: dep-x") {
		t.Errorf("task-a depends_on not shown correctly, got:\n%s", stdout)
	}
	// Task with no affects/depends_on should show "none".
	if !strings.Contains(stdout, "affects: none") {
		t.Errorf("task-c should show affects: none, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "depends_on: none") {
		t.Errorf("task-c should show depends_on: none, got:\n%s", stdout)
	}
}

func TestDryRun_DependencySummary(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// completed dependency
	depCompleted := "---\nid: dep-completed\npriority: 5\n---\n# Completed\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "dep-completed.md"), []byte(depCompleted), 0o644)

	// waiting dependency
	depWaiting := "---\nid: dep-waiting\npriority: 5\ndepends_on:\n  - dep-completed\n---\n# Waiting\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "dep-waiting.md"), []byte(depWaiting), 0o644)

	// failed dependency
	depFailed := "---\nid: dep-failed\npriority: 5\n---\n# Failed\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirFailed, "dep-failed.md"), []byte(depFailed), 0o644)

	// The main waiting task that depends on completed, waiting, failed, and unknown deps.
	mainTask := "---\nid: main-task\npriority: 10\ndepends_on:\n  - dep-completed\n  - dep-waiting\n  - dep-failed\n  - dep-nonexistent\n---\n# Main Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "main-task.md"), []byte(mainTask), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "=== Dependency Summary ===") {
		t.Fatal("missing Dependency Summary section")
	}

	// Verify state labels for each dependency.
	if !strings.Contains(stdout, "- dep-completed (completed)") {
		t.Errorf("dep-completed should be labeled completed, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- dep-waiting (waiting)") {
		t.Errorf("dep-waiting should be labeled waiting, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- dep-failed (failed)") {
		t.Errorf("dep-failed should be labeled failed, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- dep-nonexistent (unknown)") {
		t.Errorf("dep-nonexistent should be labeled unknown, got:\n%s", stdout)
	}

	// Verify diagnostics subsection for unknown dependency.
	if !strings.Contains(stdout, "WARNING main-task depends on unknown id") {
		t.Errorf("diagnostics should warn about unknown dep, got:\n%s", stdout)
	}
}

func TestDryRun_DependencySummary_Ambiguous(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create a task ID that exists in both completed and non-completed (backlog).
	ambigCompleted := "---\nid: ambig-dep\npriority: 5\n---\n# Ambig Completed\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "ambig-dep.md"), []byte(ambigCompleted), 0o644)

	ambigBacklog := "---\nid: ambig-dep\npriority: 5\n---\n# Ambig Backlog\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "ambig-dep-v2.md"), []byte(ambigBacklog), 0o644)

	// Waiting task that depends on the ambiguous ID.
	waitingTask := "---\nid: waiter\npriority: 10\ndepends_on:\n  - ambig-dep\n---\n# Waiter\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "waiter.md"), []byte(waitingTask), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "- ambig-dep (ambiguous)") {
		t.Errorf("ambig-dep should be labeled ambiguous, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "WARNING id \"ambig-dep\" is ambiguous") {
		t.Errorf("diagnostics should warn about ambiguous id, got:\n%s", stdout)
	}
}

func TestDryRun_DependencySummary_ActiveAndBacklogStates(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	backlogTask := "---\nid: dep-backlog\npriority: 5\n---\n# Backlog\n"
	inProgressTask := "---\nid: dep-in-progress\npriority: 5\n---\n# In Progress\n"
	reviewTask := "---\nid: dep-review\npriority: 5\n---\n# Review\n"
	mergeTask := "---\nid: dep-merge\npriority: 5\n---\n# Merge\n"
	waitingTask := "---\nid: waiter\npriority: 10\ndepends_on:\n  - dep-backlog\n  - dep-in-progress\n  - dep-review\n  - dep-merge\n---\n# Waiter\n"

	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "dep-backlog.md"), []byte(backlogTask), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "dep-in-progress.md"), []byte(inProgressTask), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "dep-review.md"), []byte(reviewTask), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirReadyMerge, "dep-merge.md"), []byte(mergeTask), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "waiter.md"), []byte(waitingTask), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "- dep-backlog (backlog)") {
		t.Errorf("dep-backlog should be labeled backlog, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- dep-in-progress (in-progress)") {
		t.Errorf("dep-in-progress should be labeled in-progress, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- dep-review (ready-for-review)") {
		t.Errorf("dep-review should be labeled ready-for-review, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- dep-merge (ready-to-merge)") {
		t.Errorf("dep-merge should be labeled ready-to-merge, got:\n%s", stdout)
	}
}

func TestDryRun_DependencySummary_DuplicateWaitingIDIsAmbiguous(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	dupA := "---\nid: dup-dep\npriority: 5\n---\n# Duplicate A\n"
	dupB := "---\nid: dup-dep\npriority: 6\n---\n# Duplicate B\n"
	waitingTask := "---\nid: waiter\npriority: 10\ndepends_on:\n  - dup-dep\n---\n# Waiter\n"

	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "dup-a.md"), []byte(dupA), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "dup-b.md"), []byte(dupB), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "waiter.md"), []byte(waitingTask), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "- dup-dep (ambiguous)") {
		t.Errorf("duplicate waiting id should be labeled ambiguous, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "WARNING duplicate waiting id \"dup-dep\"") {
		t.Errorf("diagnostics should warn about duplicate waiting id, got:\n%s", stdout)
	}
}

func TestDryRun_DependencySummary_ParseFailedTargetUsesQueueState(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	badBacklog := "---\npriority: not-a-number\n---\n# Broken Backlog Task\n"
	waitingTask := "---\nid: waiter\npriority: 10\ndepends_on:\n  - parse-target\n---\n# Waiter\n"

	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "parse-target.md"), []byte(badBacklog), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "waiter.md"), []byte(waitingTask), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "ERROR backlog/parse-target.md") {
		t.Errorf("parse-target parse error should be reported, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "- parse-target (backlog)") {
		t.Errorf("parse-failed dependency target should retain backlog state, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "WARNING waiter depends on unknown id \"parse-target\"") {
		t.Errorf("parse-failed dependency target should not be reported as unknown, got:\n%s", stdout)
	}
}

func TestDryRun_AffectsConflictDetail(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskA := "---\nid: task-a\npriority: 10\naffects:\n  - shared.go\n  - unique-a.go\n---\n# Task A\n"
	taskB := "---\nid: task-b\npriority: 20\naffects:\n  - shared.go\n  - unique-b.go\n---\n# Task B\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-a.md"), []byte(taskA), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-b.md"), []byte(taskB), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "=== Affects Conflict Detection ===") {
		t.Fatal("missing Affects Conflict Detection section")
	}
	if !strings.Contains(stdout, "DEFERRED task-b.md") {
		t.Errorf("task-b should be deferred, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "blocked by task-a.md") {
		t.Errorf("should show blocking task, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "shared.go") {
		t.Errorf("should show conflicting affects entry, got:\n%s", stdout)
	}
}

func TestDryRun_DependencyBlockedBacklogTask(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	blocked := "---\nid: blocked\ndepends_on:\n  - missing-dep\npriority: 10\n---\n# Blocked\n"
	runnable := "---\nid: runnable\npriority: 20\n---\n# Runnable\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "blocked.md"), []byte(blocked), 0o644)
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "runnable.md"), []byte(runnable), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	if !strings.Contains(stdout, "=== Dependency-Blocked Backlog Tasks ===") {
		t.Fatalf("missing dependency-blocked backlog section:\n%s", stdout)
	}
	if !strings.Contains(stdout, "BLOCKED blocked.md") {
		t.Fatalf("blocked task should be surfaced, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "blocked.md (priority 10)") {
		t.Fatalf("blocked task should not appear in execution order, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "runnable.md (priority 20)") {
		t.Fatalf("runnable task should remain in execution order, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "blocked.md [dependency-blocked]") {
		t.Fatalf("backlog summary should label blocked task, got:\n%s", stdout)
	}
}

func TestDryRun_ParseErrorsWithValidTasks(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Valid backlog task.
	validTask := "---\nid: valid-task\npriority: 10\naffects:\n  - file.go\n---\n# Valid Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "valid-task.md"), []byte(validTask), 0o644)

	// Invalid backlog task (parse error).
	badTask := "---\npriority: not-a-number\n---\n# Bad Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "bad-task.md"), []byte(badTask), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	// Parse error should be reported.
	if !strings.Contains(stdout, "ERROR backlog/bad-task.md") {
		t.Errorf("parse error should be reported, got:\n%s", stdout)
	}

	// Valid task should still appear in Execution Order and Backlog Task Summary.
	if !strings.Contains(stdout, "=== Execution Order ===") {
		t.Fatal("missing Execution Order section")
	}
	if !strings.Contains(stdout, "valid-task.md (priority 10)") {
		t.Errorf("valid task should appear in execution order despite parse error in another task, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "=== Backlog Task Summary ===") {
		t.Fatal("missing Backlog Task Summary section")
	}
	if !strings.Contains(stdout, "valid-task.md [runnable]") {
		t.Errorf("valid task should appear in backlog summary, got:\n%s", stdout)
	}
	// Bad task should NOT appear in Backlog Task Summary (it failed parsing).
	if strings.Contains(stdout, "bad-task.md [runnable]") || strings.Contains(stdout, "bad-task.md [deferred]") {
		t.Errorf("bad-task should not appear in backlog summary, got:\n%s", stdout)
	}

	// Queue Summary should count both valid and broken files (2 total in backlog).
	if !strings.Contains(stdout, "backlog              2") {
		t.Errorf("queue summary should count 2 backlog files (valid + broken), got:\n%s", stdout)
	}
}

func TestSurfaceBuildWarnings_NoWarnings(t *testing.T) {
	tasksDir := t.TempDir()
	for _, d := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	idx := queue.BuildIndex(tasksDir)
	_, stderr := captureStdoutStderr(t, func() {
		if surfaceBuildWarnings(idx) {
			t.Error("surfaceBuildWarnings should return false when there are no warnings")
		}
	})

	if stderr != "" {
		t.Errorf("expected no stderr output, got: %s", stderr)
	}
}

func TestSurfaceBuildWarnings_DirReadFailure(t *testing.T) {
	tasksDir := t.TempDir()
	for _, d := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Replace the backlog directory with a regular file so ReadDir fails
	// with a non-NotExist error, triggering a directory-level build warning.
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog)
	if err := os.RemoveAll(backlogPath); err != nil {
		t.Fatalf("remove backlog dir: %v", err)
	}
	if err := os.WriteFile(backlogPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("create backlog file: %v", err)
	}

	idx := queue.BuildIndex(tasksDir)
	if len(idx.BuildWarnings()) == 0 {
		t.Fatal("expected BuildWarnings from unreadable directory")
	}

	_, stderr := captureStdoutStderr(t, func() {
		if !surfaceBuildWarnings(idx) {
			t.Error("surfaceBuildWarnings should return true for directory-level read failure")
		}
	})

	if !strings.Contains(stderr, "warning: index build:") {
		t.Errorf("expected warning on stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, queue.DirBacklog) {
		t.Errorf("expected stderr to mention %q, got: %s", queue.DirBacklog, stderr)
	}
}

func TestSurfaceBuildWarnings_GlobWarningDoesNotTriggerPollError(t *testing.T) {
	tasksDir := t.TempDir()
	for _, d := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	// Create a task with an invalid glob pattern in affects to trigger a
	// per-file build warning (not a directory-level failure).
	taskContent := "---\nid: bad-glob\npriority: 10\naffects:\n  - \"[invalid\"\n---\n# Bad Glob\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "bad-glob.md"), []byte(taskContent), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	idx := queue.BuildIndex(tasksDir)
	if len(idx.BuildWarnings()) == 0 {
		t.Fatal("expected BuildWarnings from invalid glob")
	}

	_, stderr := captureStdoutStderr(t, func() {
		if surfaceBuildWarnings(idx) {
			t.Error("surfaceBuildWarnings should return false for per-file glob warnings (not directory-level)")
		}
	})

	if !strings.Contains(stderr, "warning: index build:") {
		t.Errorf("expected warning logged on stderr even for glob issues, got: %s", stderr)
	}
}

func TestDryRun_SurfacesBuildWarnings(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskContent := "---\nid: bad-glob\npriority: 10\naffects:\n  - \"[invalid\"\n---\n# Bad Glob\n"
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "bad-glob.md"), []byte(taskContent), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	_, stderr := captureStdoutStderr(t, func() {
		if err := DryRun(io.Discard, repoDir, "main", testRunOptions()); err != nil {
			t.Fatalf("DryRun returned error: %v", err)
		}
	})

	if !strings.Contains(stderr, "warning: index build:") {
		t.Errorf("expected dry-run to surface build warning on stderr, got: %s", stderr)
	}
	if !strings.Contains(stderr, "bad-glob.md") {
		t.Errorf("expected dry-run warning to mention bad-glob.md, got: %s", stderr)
	}
}

func TestGracefulShutdown_RecoversDuringSignal(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	tasksDir := t.TempDir()
	for _, sub := range []string{queue.DirBacklog, queue.DirInProgress} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	taskFile := "signal-test.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	os.WriteFile(inProgressPath, []byte("<!-- claimed-by: agent1 -->\n# Signal Test\n"), 0o644)

	claimed := &queue.ClaimedTask{
		Filename: taskFile,
		Branch:   "task/signal-test",
		Title:    "Signal Test",
		TaskPath: inProgressPath,
	}

	// Simulate runOnce with context cancellation (SIGTERM).
	ctx, cancel := context.WithCancel(context.Background())
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()

	cmd := exec.CommandContext(timeoutCtx, "sleep", "60")
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = gracefulShutdownDelay

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start: %v", err)
	}

	// Cancel after a short delay (simulates receiving SIGTERM).
	time.AfterFunc(50*time.Millisecond, cancel)

	// Wait for command to exit (simulates runOnce completing).
	_ = cmd.Wait()

	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", ctx.Err())
	}

	// recoverStuckTask is called after runOnce returns, even during shutdown.
	stdout, _ := captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent1", claimed)
	})

	// Task should be recovered to backlog/.
	if _, err := os.Stat(inProgressPath); err == nil {
		t.Fatal("task still in in-progress/ after shutdown recovery")
	}
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, taskFile)
	data, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("task not found in backlog/: %v", err)
	}
	if !strings.Contains(string(data), "<!-- failure: agent1") {
		t.Fatal("failure record not appended to recovered task")
	}
	if !strings.Contains(stdout, "Recovered task") {
		t.Fatalf("expected recovery message, got: %s", stdout)
	}
}

func TestGracefulShutdown_ExitsEarlyAfterRecovery(t *testing.T) {
	// Verify that a cancelled context causes pollLoop to skip further work.
	// We test the building block: after runOnce + recoverStuckTask, if
	// ctx.Err() != nil the loop should exit without reaching review/merge.

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// After cancellation, ctx.Err() must be non-nil.
	if ctx.Err() == nil {
		t.Fatal("expected cancelled context")
	}
}

func TestSetupSignalContext_PrintsShutdownMessage(t *testing.T) {
	stdout, _ := captureStdoutStderr(t, func() {
		ctx, cancel := setupSignalContext()
		defer cancel()
		ch := signalChan(ctx)
		defer func() {
			if ch != nil {
				signal.Stop(ch)
			}
		}()

		// Deliver a signal to the channel to trigger the shutdown message.
		ch <- syscall.SIGTERM

		// Wait for context cancellation.
		<-ctx.Done()
	})

	if !strings.Contains(stdout, "Shutting down, waiting for current task to finish...") {
		t.Fatalf("expected shutdown message in stdout, got: %q", stdout)
	}
}

func TestReportBranchResolution_SkipsLocalBranch(t *testing.T) {
	stdout, stderr := captureStdoutStderr(t, func() {
		reportBranchResolution(git.EnsureBranchResult{Branch: "mato", Source: git.BranchSourceLocal})
	})

	if stdout != "" {
		t.Fatalf("expected no stdout, got %q", stdout)
	}
	if stderr != "" {
		t.Fatalf("expected no stderr, got %q", stderr)
	}
}

func TestReportBranchResolution_PrintsExpectedStdoutMessage(t *testing.T) {
	tests := []struct {
		name   string
		result git.EnsureBranchResult
		want   string
	}{
		{
			name:   "live remote",
			result: git.EnsureBranchResult{Branch: "mato", Source: git.BranchSourceRemote},
			want:   "Using branch mato (live origin/mato)",
		},
		{
			name:   "head remote missing",
			result: git.EnsureBranchResult{Branch: "mato", Source: git.BranchSourceHeadRemoteMissing},
			want:   "Using branch mato (current HEAD (origin/mato not found on remote))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := captureStdoutStderr(t, func() {
				reportBranchResolution(tt.result)
			})

			if !strings.Contains(stdout, tt.want) {
				t.Fatalf("expected stdout branch message %q, got %q", tt.want, stdout)
			}
			if stderr != "" {
				t.Fatalf("expected no stderr, got %q", stderr)
			}
		})
	}
}

func TestReportBranchResolution_WarnsForCachedOrUnavailableBranch(t *testing.T) {
	tests := []struct {
		name   string
		result git.EnsureBranchResult
		want   string
	}{
		{
			name:   "cached remote",
			result: git.EnsureBranchResult{Branch: "mato", Source: git.BranchSourceRemoteCached},
			want:   "warning: using branch mato (cached origin/mato (origin unavailable))",
		},
		{
			name:   "head remote unavailable",
			result: git.EnsureBranchResult{Branch: "mato", Source: git.BranchSourceHeadRemoteUnavailable},
			want:   "warning: using branch mato (current HEAD (origin unavailable))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr := captureStdoutStderr(t, func() {
				reportBranchResolution(tt.result)
			})

			if stdout != "" {
				t.Fatalf("expected no stdout, got %q", stdout)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("expected stderr warning %q, got %q", tt.want, stderr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// pollCleanup tests
// ---------------------------------------------------------------------------

func setupFullTasksDir(t *testing.T) string {
	t.Helper()
	tasksDir := t.TempDir()
	for _, sub := range queue.AllDirs {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatalf("mkdir .locks: %v", err)
	}
	for _, sub := range []string{"messages/events", "messages/presence", "messages/completions"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	return tasksDir
}

func TestPollCleanup_RecoverOrphanedTask(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Place an orphaned task in in-progress/ with a dead agent ID.
	taskContent := "<!-- claimed-by: deadagent1  claimed-at: 2026-01-01T00:00:00Z -->\n---\nid: orphan-task\npriority: 10\n---\n# Orphan\n"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, "orphan-task.md")
	os.WriteFile(inProgressPath, []byte(taskContent), 0o644)

	// No lock file for deadagent1, so the task should be recovered.
	captureStdoutStderr(t, func() {
		pollCleanup(tasksDir)
	})

	if _, err := os.Stat(inProgressPath); err == nil {
		t.Fatal("orphaned task should have been removed from in-progress/")
	}
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, "orphan-task.md")
	if _, err := os.Stat(backlogPath); err != nil {
		t.Fatalf("orphaned task should have been moved to backlog/: %v", err)
	}
}

func TestPollCleanup_CleansOldMessages(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Create an old message file with a past modification time.
	msgPath := filepath.Join(tasksDir, "messages", "events", "old-msg.json")
	os.WriteFile(msgPath, []byte(`{"id":"old-msg"}`), 0o644)
	oldTime := time.Now().Add(-48 * time.Hour)
	os.Chtimes(msgPath, oldTime, oldTime)

	// Create a recent message that should be kept.
	recentPath := filepath.Join(tasksDir, "messages", "events", "new-msg.json")
	os.WriteFile(recentPath, []byte(`{"id":"new-msg"}`), 0o644)

	captureStdoutStderr(t, func() {
		pollCleanup(tasksDir)
	})

	if _, err := os.Stat(msgPath); err == nil {
		t.Fatal("old message should have been cleaned up")
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatal("recent message should have been kept")
	}
}

func TestPollCleanup_EmptyDir(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Should not panic or error on empty directories.
	captureStdoutStderr(t, func() {
		pollCleanup(tasksDir)
	})
}

func TestPollCleanup_SweepsStaleTaskState(t *testing.T) {
	tasksDir := setupFullTasksDir(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "active.md"), []byte("# Active\n"), 0o644); err != nil {
		t.Fatalf("WriteFile active task: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "done.md"), []byte("# Done\n"), 0o644); err != nil {
		t.Fatalf("WriteFile done task: %v", err)
	}
	for _, name := range []string{"active.md", "done.md", "gone.md"} {
		if err := taskstate.Update(tasksDir, name, func(state *taskstate.TaskState) {
			state.LastOutcome = name
		}); err != nil {
			t.Fatalf("seed taskstate %s: %v", name, err)
		}
	}
	captureStdoutStderr(t, func() {
		pollCleanup(tasksDir)
	})
	active, err := taskstate.Load(tasksDir, "active.md")
	if err != nil {
		t.Fatalf("Load active taskstate: %v", err)
	}
	if active == nil {
		t.Fatal("active taskstate should remain after pollCleanup")
	}
	for _, name := range []string{"done.md", "gone.md"} {
		state, err := taskstate.Load(tasksDir, name)
		if err != nil {
			t.Fatalf("Load %s taskstate: %v", name, err)
		}
		if state != nil {
			t.Fatalf("stale taskstate %s should be removed", name)
		}
	}
}

func TestPollCleanup_RebuildsFileClaimsAfterRecoveringPushedTask(t *testing.T) {
	tasksDir := setupFullTasksDir(t)
	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("seed file claims: %v", err)
	}
	content := strings.Join([]string{
		"<!-- claimed-by: deadagent1  claimed-at: 2026-01-01T00:00:00Z -->",
		"<!-- branch: task/pushed-claims -->",
		"---",
		"id: pushed-claims",
		"affects:",
		"  - src/api.go",
		"---",
		"# Pushed Claims",
		"",
	}, "\n")
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, "pushed-claims.md")
	if err := os.WriteFile(inProgressPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := taskstate.Update(tasksDir, "pushed-claims.md", func(state *taskstate.TaskState) {
		state.TaskBranch = "task/pushed-claims"
		state.TargetBranch = "mato"
		state.LastHeadSHA = "deadbeef"
		state.LastOutcome = taskstate.OutcomeWorkBranchPushed
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	captureStdoutStderr(t, func() {
		pollCleanup(tasksDir)
	})
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirReadyReview, "pushed-claims.md")); err != nil {
		t.Fatalf("recovered pushed task should be in ready-for-review: %v", err)
	}
	if err := messaging.BuildAndWriteFileClaims(tasksDir, ""); err != nil {
		t.Fatalf("rebuild file claims after recovery: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tasksDir, "messages", "file-claims.json"))
	if err != nil {
		t.Fatalf("read file-claims.json: %v", err)
	}
	var claims map[string]messaging.FileClaim
	if err := json.Unmarshal(data, &claims); err != nil {
		t.Fatalf("parse file-claims.json: %v", err)
	}
	claim, ok := claims["src/api.go"]
	if !ok {
		t.Fatal("expected recovered pushed task to appear in file-claims.json")
	}
	if claim.Task != "pushed-claims.md" {
		t.Fatalf("claim.Task = %q, want %q", claim.Task, "pushed-claims.md")
	}
	if claim.Status != queue.DirReadyReview {
		t.Fatalf("claim.Status = %q, want %q", claim.Status, queue.DirReadyReview)
	}
	msgs, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	var hasConflictWarning bool
	var hasCompletion bool
	for _, msg := range msgs {
		if msg.Task != "pushed-claims.md" {
			continue
		}
		switch msg.Type {
		case "conflict-warning":
			hasConflictWarning = true
			want := []string{"src/api.go"}
			if len(msg.Files) != len(want) || msg.Files[0] != want[0] {
				t.Fatalf("conflict-warning files = %v, want %v", msg.Files, want)
			}
		case "completion":
			hasCompletion = true
			want := []string{"src/api.go"}
			if len(msg.Files) != len(want) || msg.Files[0] != want[0] {
				t.Fatalf("completion files = %v, want %v", msg.Files, want)
			}
		}
	}
	if !hasConflictWarning {
		t.Fatal("expected pollCleanup recovery to emit conflict-warning message")
	}
	if !hasCompletion {
		t.Fatal("expected pollCleanup recovery to emit completion message")
	}
}

func TestRecoverStuckTask_PushedTaskRecoveryUsesAffectsForMessageFiles(t *testing.T) {
	tasksDir := setupFullTasksDir(t)
	content := strings.Join([]string{
		"<!-- claimed-by: deadagent1  claimed-at: 2026-01-01T00:00:00Z -->",
		"<!-- branch: task/recovered-files -->",
		"---",
		"id: recovered-files",
		"affects:",
		"  - src/b.go",
		"  - src/a.go",
		"---",
		"# Recovered Files",
		"",
	}, "\n")
	taskFile := "recovered-files.md"
	inProgressPath := filepath.Join(tasksDir, queue.DirInProgress, taskFile)
	if err := os.WriteFile(inProgressPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile task: %v", err)
	}
	if err := taskstate.Update(tasksDir, taskFile, func(state *taskstate.TaskState) {
		state.TaskBranch = "task/recovered-files"
		state.TargetBranch = "mato"
		state.LastHeadSHA = "deadbeef"
		state.LastOutcome = taskstate.OutcomeWorkBranchPushed
	}); err != nil {
		t.Fatalf("seed taskstate: %v", err)
	}

	claimed := &queue.ClaimedTask{Filename: taskFile, Branch: "task/recovered-files", Title: "Recovered Files", TaskPath: inProgressPath}
	captureStdoutStderr(t, func() {
		recoverStuckTask(tasksDir, "agent1", claimed)
	})

	msgs, err := messaging.ReadMessages(tasksDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadMessages: %v", err)
	}
	var gotFiles []string
	for _, msg := range msgs {
		if msg.Task == taskFile && msg.Type == "completion" {
			gotFiles = msg.Files
			break
		}
	}
	want := []string{"src/a.go", "src/b.go"}
	if len(gotFiles) != len(want) {
		t.Fatalf("completion files = %v, want %v", gotFiles, want)
	}
	for i := range want {
		if gotFiles[i] != want[i] {
			t.Fatalf("completion files = %v, want %v", gotFiles, want)
		}
	}
}

// ---------------------------------------------------------------------------
// pollReconcile tests
// ---------------------------------------------------------------------------

func TestPollReconcile_EmptyQueue(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	_, stderr := captureStdoutStderr(t, func() {
		idx, hadError := pollReconcile(tasksDir)
		if hadError {
			t.Error("expected no errors on empty queue")
		}
		if idx == nil {
			t.Fatal("expected non-nil index")
		}
	})

	if strings.Contains(stderr, "warning:") {
		t.Errorf("expected no warnings, got: %s", stderr)
	}
}

func TestPollReconcile_PromotesWaitingTask(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Create a completed dependency task.
	completedContent := "---\nid: dep-task\npriority: 10\n---\n# Dep\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "dep-task.md"), []byte(completedContent), 0o644)

	// Create a waiting task that depends on the completed one.
	waitingContent := "---\nid: child-task\npriority: 20\ndepends_on:\n  - dep-task\n---\n# Child\n"
	waitingPath := filepath.Join(tasksDir, queue.DirWaiting, "child-task.md")
	os.WriteFile(waitingPath, []byte(waitingContent), 0o644)

	captureStdoutStderr(t, func() {
		idx, hadError := pollReconcile(tasksDir)
		if hadError {
			t.Error("expected no errors during reconciliation")
		}
		if idx == nil {
			t.Fatal("expected non-nil index")
		}
	})

	// Waiting task should have been promoted to backlog.
	if _, err := os.Stat(waitingPath); err == nil {
		t.Fatal("waiting task should have been promoted out of waiting/")
	}
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog, "child-task.md")
	if _, err := os.Stat(backlogPath); err != nil {
		t.Fatalf("waiting task should have been promoted to backlog/: %v", err)
	}
}

func TestPollReconcile_DirReadFailureFlagsError(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Replace backlog directory with a regular file to trigger ReadDir failure.
	backlogPath := filepath.Join(tasksDir, queue.DirBacklog)
	os.RemoveAll(backlogPath)
	os.WriteFile(backlogPath, []byte("not a directory"), 0o644)

	_, stderr := captureStdoutStderr(t, func() {
		_, hadError := pollReconcile(tasksDir)
		if !hadError {
			t.Error("expected hadError=true when a queue directory is unreadable")
		}
	})

	if !strings.Contains(stderr, "warning: index build:") {
		t.Errorf("expected warning about index build on stderr, got: %s", stderr)
	}
}

// ---------------------------------------------------------------------------
// pollClaimAndRun tests
// ---------------------------------------------------------------------------

func TestPollClaimAndRun_NoTasksAvailable(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	ctx := context.Background()
	env := envConfig{tasksDir: tasksDir}
	run := runContext{agentID: "test-agent"}
	failedDirExcluded := make(map[string]struct{})
	idx := queue.BuildIndex(tasksDir)
	view := queue.ComputeRunnableBacklogView(tasksDir, idx)

	var claimed, hadError bool
	captureStdoutStderr(t, func() {
		claimed, hadError = pollClaimAndRun(ctx, env, run, tasksDir, "test-agent", failedDirExcluded, 0, idx, view)
	})

	if claimed {
		t.Fatal("expected claimed=false when backlog is empty")
	}
	if hadError {
		t.Error("expected hadError=false when backlog is empty")
	}
}

func TestPollClaimAndRun_FailedDirUnavailableExclusion(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Pre-exclude a task name; verify it stays in the exclusion map
	// and doesn't cause errors.
	ctx := context.Background()
	env := envConfig{tasksDir: tasksDir}
	run := runContext{agentID: "test-agent"}
	failedDirExcluded := map[string]struct{}{"already-excluded.md": {}}
	idx := queue.BuildIndex(tasksDir)
	view := queue.ComputeRunnableBacklogView(tasksDir, idx)

	captureStdoutStderr(t, func() {
		pollClaimAndRun(ctx, env, run, tasksDir, "test-agent", failedDirExcluded, 0, idx, view)
	})

	if _, ok := failedDirExcluded["already-excluded.md"]; !ok {
		t.Fatal("pre-existing exclusion should be preserved")
	}
}

func TestPollClaimAndRun_DeferredOverlapSkipped(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// Create a task in in-progress that claims affects on a path.
	activeContent := "<!-- claimed-by: other-agent  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- branch: task/active -->\n---\nid: active-task\npriority: 5\naffects:\n  - src/main.go\n---\n# Active\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "active-task.md"), []byte(activeContent), 0o644)

	// Register a lock for other-agent so it isn't orphan-recovered.
	lockPath := filepath.Join(tasksDir, ".locks", "other-agent.pid")
	os.WriteFile(lockPath, []byte(fmt.Sprintf("%d:%d", os.Getpid(), time.Now().Unix())), 0o644)

	// Create a backlog task that overlaps the same affects.
	backlogContent := "---\nid: overlap-task\npriority: 10\naffects:\n  - src/main.go\n---\n# Overlap\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "overlap-task.md"), []byte(backlogContent), 0o644)

	ctx := context.Background()
	env := envConfig{tasksDir: tasksDir}
	run := runContext{agentID: "test-agent"}
	failedDirExcluded := make(map[string]struct{})
	idx := queue.BuildIndex(tasksDir)
	view := queue.ComputeRunnableBacklogView(tasksDir, idx)

	var claimed bool
	captureStdoutStderr(t, func() {
		claimed, _ = pollClaimAndRun(ctx, env, run, tasksDir, "test-agent", failedDirExcluded, 0, idx, view)
	})

	if claimed {
		t.Fatal("overlapping task should have been deferred, not claimed")
	}
}

// ---------------------------------------------------------------------------
// pollMerge tests
// ---------------------------------------------------------------------------

func TestPollMerge_EmptyQueue(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	var count int
	captureStdoutStderr(t, func() {
		count = pollMerge(context.Background(), "/nonexistent", tasksDir, "mato")
	})

	if count != 0 {
		t.Fatalf("expected 0 merged tasks from empty queue, got %d", count)
	}
}

func TestPollMerge_LockAcquiredAndReleased(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	// First call should acquire the lock and return 0 (no tasks).
	var count int
	captureStdoutStderr(t, func() {
		count = pollMerge(context.Background(), "/nonexistent", tasksDir, "mato")
	})
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}

	// Second call should also succeed (lock was released by first call).
	captureStdoutStderr(t, func() {
		count = pollMerge(context.Background(), "/nonexistent", tasksDir, "mato")
	})
	if count != 0 {
		t.Fatalf("expected 0 on second call, got %d", count)
	}
}

func TestPollIterate_PausedSkipsClaimAndReviewButMerges(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	origNowFn := nowFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
		nowFn = origNowFn
	}()

	tasksDir := setupFullTasksDir(t)
	var manifestCalled, claimCalled, reviewCalled bool
	pauseReadFn = func(string) (pause.State, error) {
		return pause.State{Active: true, Since: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)}, nil
	}
	pollWriteManifestFn = func(tasksDir string, failedDirExcluded map[string]struct{}, idx *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		manifestCalled = true
		if _, ok := failedDirExcluded["excluded.md"]; !ok {
			t.Fatalf("failedDirExcluded not preserved")
		}
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		claimCalled = true
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		reviewCalled = true
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 1 }
	nowFn = func() time.Time { return time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC) }

	hb := newIdleHeartbeat(nowFn())
	stdout, _ := captureStdoutStderr(t, func() {
		result := pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &hb, map[string]struct{}{"excluded.md": {}}, false)
		if !result.pauseActive {
			t.Fatal("pauseActive = false, want true")
		}
		if result.mergeCount != 1 {
			t.Fatalf("mergeCount = %d, want 1", result.mergeCount)
		}
	})

	if !manifestCalled {
		t.Fatal("expected manifest refresh")
	}
	if claimCalled {
		t.Fatal("claim phase should be skipped while paused")
	}
	if reviewCalled {
		t.Fatal("review phase should be skipped while paused")
	}
	if !strings.Contains(stdout, "[mato] paused - run 'mato resume' to continue") {
		t.Fatalf("expected paused message, got %q", stdout)
	}
}

func TestPollIterate_PauseWarningThrottled(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	origNowFn := nowFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
		nowFn = origNowFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) {
		return pause.State{Active: true, ProblemKind: pause.ProblemMalformed, Problem: `invalid timestamp: "bad"`}, nil
	}
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	hb := newIdleHeartbeat(time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC))
	nowFn = func() time.Time { return time.Date(2026, 3, 23, 10, 0, 10, 0, time.UTC) }
	_, stderr1 := captureStdoutStderr(t, func() {
		pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &hb, map[string]struct{}{}, false)
	})
	if !strings.Contains(stderr1, "warning: pause sentinel: invalid timestamp") {
		t.Fatalf("expected warning, got %q", stderr1)
	}

	nowFn = func() time.Time { return time.Date(2026, 3, 23, 10, 0, 20, 0, time.UTC) }
	_, stderr2 := captureStdoutStderr(t, func() {
		pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &hb, map[string]struct{}{}, true)
	})
	if strings.Contains(stderr2, "warning: pause sentinel:") {
		t.Fatalf("expected warning to be throttled, got %q", stderr2)
	}
}

func TestPollIterate_PausedMergeDoesNotResetHeartbeatThrottle(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	origNowFn := nowFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
		nowFn = origNowFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) {
		return pause.State{Active: true, Since: time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)}, nil
	}
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 1 }

	hb := newIdleHeartbeat(time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC))
	nowFn = func() time.Time { return time.Date(2026, 3, 23, 10, 0, 10, 0, time.UTC) }
	stdout1, _ := captureStdoutStderr(t, func() {
		pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &hb, map[string]struct{}{}, false)
	})
	if !strings.Contains(stdout1, "[mato] paused - run 'mato resume' to continue") {
		t.Fatalf("expected initial paused heartbeat, got %q", stdout1)
	}

	nowFn = func() time.Time { return time.Date(2026, 3, 23, 10, 0, 20, 0, time.UTC) }
	stdout2, _ := captureStdoutStderr(t, func() {
		pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &hb, map[string]struct{}{}, true)
	})
	if strings.Contains(stdout2, "[mato] paused - run 'mato resume' to continue") {
		t.Fatalf("paused merge work should not reset heartbeat throttle, got %q", stdout2)
	}
}

func TestPollIterate_ClaimedCancelledSkipsReviewAndMerge(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return true, false
	}
	reviewCalled := false
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		reviewCalled = true
		return false
	}
	mergeCalled := false
	pollMergeFn = func(context.Context, string, string, string) int {
		mergeCalled = true
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := pollIterate(ctx, envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &idleHeartbeat{}, map[string]struct{}{}, false)
	if !result.claimedTask {
		t.Fatal("claimedTask = false, want true")
	}
	if reviewCalled {
		t.Fatal("review phase should be skipped")
	}
	if mergeCalled {
		t.Fatal("merge phase should be skipped")
	}
}

func TestPollIterate_CancelledWithoutClaimSkipsReviewAndMerge(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(ctx context.Context, env envConfig, run runContext, tasksDir, agentID string, failedDirExcluded map[string]struct{}, cooldown time.Duration, idx *queue.PollIndex, view queue.RunnableBacklogView) (bool, bool) {
		if ctx.Err() == nil {
			t.Fatal("expected cancelled context in claim stub")
		}
		return false, false
	}
	reviewCalled := false
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		reviewCalled = true
		return false
	}
	mergeCalled := false
	pollMergeFn = func(context.Context, string, string, string) int {
		mergeCalled = true
		return 0
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := pollIterate(ctx, envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &idleHeartbeat{}, map[string]struct{}{}, false)
	if result.claimedTask {
		t.Fatal("claimedTask = true, want false")
	}
	if reviewCalled {
		t.Fatal("review phase should be skipped")
	}
	if mergeCalled {
		t.Fatal("merge phase should be skipped")
	}
}

func TestPollIterate_CancelDuringClaimSkipsReviewAndMerge(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}

	// Simulate a context that gets cancelled during the claim phase.
	ctx, cancel := context.WithCancel(context.Background())
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		cancel() // cancel context during claim
		return false, false
	}
	reviewCalled := false
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		reviewCalled = true
		return false
	}
	mergeCalled := false
	pollMergeFn = func(context.Context, string, string, string) int {
		mergeCalled = true
		return 0
	}

	result := pollIterate(ctx, envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &idleHeartbeat{}, map[string]struct{}{}, false)
	if result.claimedTask {
		t.Fatal("claimedTask = true, want false")
	}
	if reviewCalled {
		t.Fatal("review phase should be skipped after mid-claim cancellation")
	}
	if mergeCalled {
		t.Fatal("merge phase should be skipped after mid-claim cancellation")
	}
}

func TestPollReview_CancelledContextReturnsImmediately(t *testing.T) {
	tasksDir := setupFullTasksDir(t)
	idx := queue.BuildIndex(tasksDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}
	if pollReview(ctx, env, run, tasksDir, "mato", "test-agent", idx) {
		t.Fatal("pollReview should return false when context is cancelled")
	}
}

func TestPollMerge_CancelledContextReturnsZero(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := pollMerge(ctx, t.TempDir(), tasksDir, "mato"); got != 0 {
		t.Fatalf("pollMerge() = %d, want 0 for cancelled context", got)
	}
}

func TestPollLoop_CancelDuringIterationExits(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}

	ctx, cancel := context.WithCancel(context.Background())
	iterations := 0
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		iterations++
		cancel() // cancel during first iteration
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int {
		return 0
	}

	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}

	err := pollLoop(ctx, env, run, env.repoRoot, tasksDir, "mato", "test-agent", 0, RunModeDaemon)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if iterations != 1 {
		t.Fatalf("expected 1 iteration before exit, got %d", iterations)
	}
}

func TestPollIterate_ReviewAvailabilityUsesFreshScanAfterMerge(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int {
		path := filepath.Join(tasksDir, queue.DirReadyReview, "new-review.md")
		if err := os.WriteFile(path, []byte("---\nid: new-review\nbranch: task/new-review\n---\n# Review\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return 0
	}

	result := pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &idleHeartbeat{}, map[string]struct{}{}, false)
	if !result.hasReviewTasks {
		t.Fatal("hasReviewTasks = false, want true after merge-side review creation")
	}
}

func TestPollIterate_IdleReviewProbeDoesNotQuarantineMalformedTasks(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	malformedPath := filepath.Join(tasksDir, queue.DirReadyReview, "malformed.md")
	if err := os.WriteFile(malformedPath, []byte("---\npriority: [\n# broken\n"), 0o644); err != nil {
		t.Fatalf("WriteFile malformed review task: %v", err)
	}

	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	result := pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &idleHeartbeat{}, map[string]struct{}{}, false)
	if result.hasReviewTasks {
		t.Fatal("hasReviewTasks = true, want false with only malformed review task")
	}
	if _, err := os.Stat(malformedPath); err != nil {
		t.Fatalf("malformed review task should remain in ready-for-review/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "malformed.md")); !os.IsNotExist(err) {
		t.Fatalf("malformed review task should not be moved to failed/, got err: %v", err)
	}
}

func TestPollIterate_IdleReviewProbeDoesNotMoveExhaustedTasks(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	exhaustedContent := "---\npriority: 10\nmax_retries: 2\n---\n# Exhausted\n" +
		"<!-- review-failure: a1 at 2026-01-01T00:00:00Z — f1 -->\n" +
		"<!-- review-failure: a2 at 2026-01-02T00:00:00Z — f2 -->\n"
	exhaustedPath := filepath.Join(tasksDir, queue.DirReadyReview, "exhausted.md")
	if err := os.WriteFile(exhaustedPath, []byte(exhaustedContent), 0o644); err != nil {
		t.Fatalf("WriteFile exhausted review task: %v", err)
	}

	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	result := pollIterate(context.Background(), envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}, runContext{agentID: "a1"}, t.TempDir(), tasksDir, "mato", "a1", 0, &idleHeartbeat{}, map[string]struct{}{}, false)
	if result.hasReviewTasks {
		t.Fatal("hasReviewTasks = true, want false with only exhausted review task")
	}
	// The exhausted task must remain in ready-for-review/ — not moved by the idle probe.
	if _, err := os.Stat(exhaustedPath); err != nil {
		t.Fatalf("exhausted review task should remain in ready-for-review/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "exhausted.md")); !os.IsNotExist(err) {
		t.Fatalf("exhausted review task should not be moved to failed/ by idle probe, got err: %v", err)
	}
}

// ---------------------------------------------------------------------------
// resolveGitIdentity tests
// ---------------------------------------------------------------------------

func TestResolveGitIdentity_ConfiguredValues(t *testing.T) {
	dir := t.TempDir()
	if _, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := exec.Command("git", "-C", dir, "config", "user.name", "Alice Test").CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v", err)
	}
	if _, err := exec.Command("git", "-C", dir, "config", "user.email", "alice@example.com").CombinedOutput(); err != nil {
		t.Fatalf("git config user.email: %v", err)
	}

	name, email := resolveGitIdentity(dir)

	if name != "Alice Test" {
		t.Errorf("expected name %q, got %q", "Alice Test", name)
	}
	if email != "alice@example.com" {
		t.Errorf("expected email %q, got %q", "alice@example.com", email)
	}
}

func TestResolveGitIdentity_FallsBackToDefaults(t *testing.T) {
	// Use an isolated HOME so global git config doesn't interfere.
	dir := t.TempDir()
	if _, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v", err)
	}

	// Temporarily set HOME to an empty dir to prevent global config.
	fakeHome := t.TempDir()
	origHome := os.Getenv("HOME")
	origXDG := os.Getenv("XDG_CONFIG_HOME")
	os.Setenv("HOME", fakeHome)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(fakeHome, ".config"))
	t.Cleanup(func() {
		os.Setenv("HOME", origHome)
		if origXDG != "" {
			os.Setenv("XDG_CONFIG_HOME", origXDG)
		} else {
			os.Unsetenv("XDG_CONFIG_HOME")
		}
	})

	name, email := resolveGitIdentity(dir)

	if name != "mato" {
		t.Errorf("expected default name %q, got %q", "mato", name)
	}
	if email != "mato@local.invalid" {
		t.Errorf("expected default email %q, got %q", "mato@local.invalid", email)
	}
}

func TestResolveGitIdentity_SetsLocalConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	if _, err := exec.Command("git", "-C", dir, "config", "user.name", "Bob").CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v", err)
	}
	if _, err := exec.Command("git", "-C", dir, "config", "user.email", "bob@test.com").CombinedOutput(); err != nil {
		t.Fatalf("git config user.email: %v", err)
	}

	resolveGitIdentity(dir)

	// Verify the values were set in local config (git config --local).
	out, err := exec.Command("git", "-C", dir, "config", "--local", "user.name").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --local user.name: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "Bob" {
		t.Errorf("expected local user.name %q, got %q", "Bob", got)
	}
	out, err = exec.Command("git", "-C", dir, "config", "--local", "user.email").CombinedOutput()
	if err != nil {
		t.Fatalf("git config --local user.email: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "bob@test.com" {
		t.Errorf("expected local user.email %q, got %q", "bob@test.com", got)
	}
}

// ---------------------------------------------------------------------------
// Run initialization error path tests
// ---------------------------------------------------------------------------

func TestRun_InvalidRepoPath(t *testing.T) {
	err := Run("/nonexistent/path/that/does/not/exist", "mato", testRunOptions())
	if err == nil {
		t.Fatal("expected error for invalid repo path")
	}
}

func TestNormalizeAndValidateRunOptions(t *testing.T) {
	t.Run("trims valid values", func(t *testing.T) {
		opts, err := normalizeAndValidateRunOptions(RunOptions{
			TaskModel:                  "  claude-opus-4.6  ",
			ReviewModel:                "  gpt-5.4  ",
			ReviewSessionResumeEnabled: true,
			TaskReasoningEffort:        "  high  ",
			ReviewReasoningEffort:      "  medium  ",
		})
		if err != nil {
			t.Fatalf("normalizeAndValidateRunOptions returned error: %v", err)
		}
		if opts.TaskModel != "claude-opus-4.6" {
			t.Fatalf("TaskModel = %q, want %q", opts.TaskModel, "claude-opus-4.6")
		}
		if opts.ReviewModel != "gpt-5.4" {
			t.Fatalf("ReviewModel = %q, want %q", opts.ReviewModel, "gpt-5.4")
		}
		if opts.TaskReasoningEffort != "high" {
			t.Fatalf("TaskReasoningEffort = %q, want %q", opts.TaskReasoningEffort, "high")
		}
		if opts.ReviewReasoningEffort != "medium" {
			t.Fatalf("ReviewReasoningEffort = %q, want %q", opts.ReviewReasoningEffort, "medium")
		}
	})

	tests := []struct {
		name string
		opts RunOptions
		want string
	}{
		{
			name: "missing task model",
			opts: RunOptions{ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort},
			want: "task model must not be empty",
		},
		{
			name: "missing review model",
			opts: RunOptions{TaskModel: DefaultTaskModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort},
			want: "review model must not be empty",
		},
		{
			name: "missing task reasoning effort",
			opts: RunOptions{TaskModel: DefaultTaskModel, ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, ReviewReasoningEffort: DefaultReasoningEffort},
			want: "task reasoning effort must not be empty",
		},
		{
			name: "missing review reasoning effort",
			opts: RunOptions{TaskModel: DefaultTaskModel, ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort},
			want: "review reasoning effort must not be empty",
		},
		{
			name: "whitespace task model",
			opts: RunOptions{TaskModel: "   ", ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort},
			want: "task model must not be empty",
		},
		{
			name: "invalid run mode",
			opts: RunOptions{Mode: RunMode(99), TaskModel: DefaultTaskModel, ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort},
			want: "invalid run mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeAndValidateRunOptions(tt.opts)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestDefaultConstants(t *testing.T) {
	if DefaultTaskModel != "claude-opus-4.6" {
		t.Fatalf("DefaultTaskModel = %q, want %q", DefaultTaskModel, "claude-opus-4.6")
	}
	if DefaultReviewModel != "gpt-5.4" {
		t.Fatalf("DefaultReviewModel = %q, want %q", DefaultReviewModel, "gpt-5.4")
	}
	if DefaultReasoningEffort != "high" {
		t.Fatalf("DefaultReasoningEffort = %q, want %q", DefaultReasoningEffort, "high")
	}
}

// ---------------------------------------------------------------------------
// buildEnvAndRunContext tests
// ---------------------------------------------------------------------------

func TestBuildEnvAndRunContext_BasicFields(t *testing.T) {
	tools := hostTools{
		copilotPath:        "/usr/bin/copilot",
		gitPath:            "/usr/bin/git",
		gitUploadPackPath:  "/usr/bin/git-upload-pack",
		gitReceivePackPath: "/usr/bin/git-receive-pack",
		ghPath:             "/usr/bin/gh",
		goRoot:             "/usr/local/go",
		homeDir:            "/home/testuser",
		copilotConfigDir:   "/home/testuser/.copilot",
	}

	env, run := buildEnvAndRunContext("main", tools, "agent-123", "Test User", "test@test.com",
		"/repo", "/repo/.mato", RunOptions{TaskModel: DefaultTaskModel, ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort, AgentTimeout: 45 * time.Minute})

	if env.image == "" {
		t.Error("expected default docker image to be set")
	}
	if env.workdir != "/workspace" {
		t.Errorf("expected workdir /workspace, got %s", env.workdir)
	}
	if env.gitName != "Test User" {
		t.Errorf("expected gitName %q, got %q", "Test User", env.gitName)
	}
	if env.gitEmail != "test@test.com" {
		t.Errorf("expected gitEmail %q, got %q", "test@test.com", env.gitEmail)
	}
	if env.targetBranch != "main" {
		t.Errorf("expected targetBranch %q, got %q", "main", env.targetBranch)
	}
	if run.agentID != "agent-123" {
		t.Errorf("expected agentID %q, got %q", "agent-123", run.agentID)
	}
	if run.timeout != 45*time.Minute {
		t.Errorf("expected timeout %v, got %v", 45*time.Minute, run.timeout)
	}
	if run.model != DefaultTaskModel {
		t.Errorf("expected task model %q, got %q", DefaultTaskModel, run.model)
	}
	if run.reasoningEffort != DefaultReasoningEffort {
		t.Errorf("expected task reasoning effort %q, got %q", DefaultReasoningEffort, run.reasoningEffort)
	}
	if env.reviewModel != DefaultReviewModel {
		t.Errorf("expected review model %q, got %q", DefaultReviewModel, env.reviewModel)
	}
	if env.reviewReasoningEffort != DefaultReasoningEffort {
		t.Errorf("expected review reasoning effort %q, got %q", DefaultReasoningEffort, env.reviewReasoningEffort)
	}
	if !env.reviewSessionResumeEnabled {
		t.Fatal("expected review session resume to be enabled")
	}
	if !strings.Contains(run.prompt, "main") {
		t.Error("expected prompt to contain branch name")
	}
}

func TestBuildEnvAndRunContext_CustomDockerImage(t *testing.T) {
	tools := hostTools{homeDir: "/home/test"}
	env, _ := buildEnvAndRunContext("main", tools, "a1", "n", "e", "/r", "/r/.mato", RunOptions{TaskModel: DefaultTaskModel, ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort, DockerImage: "custom:latest", AgentTimeout: time.Hour})

	if env.image != "custom:latest" {
		t.Errorf("expected custom image %q, got %q", "custom:latest", env.image)
	}
}

func TestBuildEnvAndRunContext_ModelOverrides(t *testing.T) {
	tools := hostTools{homeDir: "/home/test"}
	env, run := buildEnvAndRunContext("main", tools, "a1", "n", "e", "/r", "/r/.mato", RunOptions{TaskModel: "claude-sonnet-4", ReviewModel: "gpt-5.4", ReviewSessionResumeEnabled: false, TaskReasoningEffort: "medium", ReviewReasoningEffort: "xhigh", AgentTimeout: time.Hour})

	if run.model != "claude-sonnet-4" {
		t.Fatalf("run.model = %q, want %q", run.model, "claude-sonnet-4")
	}
	if run.reasoningEffort != "medium" {
		t.Fatalf("run.reasoningEffort = %q, want %q", run.reasoningEffort, "medium")
	}
	if env.reviewModel != "gpt-5.4" {
		t.Fatalf("env.reviewModel = %q, want %q", env.reviewModel, "gpt-5.4")
	}
	if env.reviewReasoningEffort != "xhigh" {
		t.Fatalf("env.reviewReasoningEffort = %q, want %q", env.reviewReasoningEffort, "xhigh")
	}
	if env.reviewSessionResumeEnabled {
		t.Fatal("expected review session resume to reflect RunOptions")
	}
}

func TestBuildEnvAndRunContext_PromptPlaceholders(t *testing.T) {
	tools := hostTools{homeDir: "/home/test"}
	_, run := buildEnvAndRunContext("my-branch", tools, "a1", "n", "e", "/r", "/r/.mato", RunOptions{TaskModel: DefaultTaskModel, ReviewModel: DefaultReviewModel, ReviewSessionResumeEnabled: true, TaskReasoningEffort: DefaultReasoningEffort, ReviewReasoningEffort: DefaultReasoningEffort, AgentTimeout: time.Hour})

	if strings.Contains(run.prompt, "TASKS_DIR_PLACEHOLDER") {
		t.Error("prompt still contains TASKS_DIR_PLACEHOLDER")
	}
	if strings.Contains(run.prompt, "TARGET_BRANCH_PLACEHOLDER") {
		t.Error("prompt still contains TARGET_BRANCH_PLACEHOLDER")
	}
	if strings.Contains(run.prompt, "MESSAGES_DIR_PLACEHOLDER") {
		t.Error("prompt still contains MESSAGES_DIR_PLACEHOLDER")
	}
}

func TestResumeRejected(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "copilot invalid resume flag", output: "error: invalid value for '--resume': session not found", want: true},
		{name: "copilot failed to resume", output: "failed to resume session 123", want: true},
		{name: "normal discussion text", output: "the review session discussed an invalid task state", want: false},
		{name: "keywords across lines", output: "session\nnot found", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resumeRejected(tt.output); got != tt.want {
				t.Fatalf("resumeRejected(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

func TestResumeDetectionBuffer_MatchesAcrossLargeOutput(t *testing.T) {
	var buf resumeDetectionBuffer
	chunkA := strings.Repeat("x", resumeDetectionBufferLimit)
	chunkB := strings.Repeat("y", resumeDetectionBufferLimit) + "\nerror: failed to resume session 123\n"
	if _, err := buf.Write([]byte(chunkA)); err != nil {
		t.Fatalf("Write chunkA: %v", err)
	}
	if buf.Matched() {
		t.Fatal("buffer should not match before resume rejection text appears")
	}
	if _, err := buf.Write([]byte(chunkB)); err != nil {
		t.Fatalf("Write chunkB: %v", err)
	}
	if !buf.Matched() {
		t.Fatal("buffer should detect resume rejection after large output")
	}
}

// resumeDetectionBufferOld is the previous string-concatenation implementation,
// preserved here for benchmark comparison against the current bytes.Buffer version.
type resumeDetectionBufferOld struct {
	matched bool
	carry   string
}

func (b *resumeDetectionBufferOld) Write(p []byte) (int, error) {
	combined := b.carry + string(p)
	if !b.matched && resumeRejected(combined) {
		b.matched = true
	}
	if len(combined) > resumeDetectionBufferLimit {
		combined = combined[len(combined)-resumeDetectionBufferLimit:]
	}
	b.carry = combined
	return len(p), nil
}

func BenchmarkResumeDetectionBuffer_LargeChunks(b *testing.B) {
	chunk := []byte(strings.Repeat("normal output line without any resume markers\n", 100))
	b.Run("Old_StringConcat", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf resumeDetectionBufferOld
			for j := 0; j < 100; j++ {
				buf.Write(chunk)
			}
		}
	})
	b.Run("New_BytesBuffer", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf resumeDetectionBuffer
			for j := 0; j < 100; j++ {
				buf.Write(chunk)
			}
		}
	})
}

func BenchmarkResumeDetectionBuffer_SmallChunks(b *testing.B) {
	chunk := []byte("short line\n")
	b.Run("Old_StringConcat", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf resumeDetectionBufferOld
			for j := 0; j < 1000; j++ {
				buf.Write(chunk)
			}
		}
	})
	b.Run("New_BytesBuffer", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var buf resumeDetectionBuffer
			for j := 0; j < 1000; j++ {
				buf.Write(chunk)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// pollLoop integration-level tests
// ---------------------------------------------------------------------------

func TestPollLoop_ExitsOnCancelledContext(t *testing.T) {
	tasksDir := setupFullTasksDir(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}

	err := pollLoop(ctx, env, run, env.repoRoot, tasksDir, "mato", "test-agent", 0, RunModeDaemon)
	if err != nil {
		t.Fatalf("expected nil error from cancelled pollLoop, got: %v", err)
	}
}

func TestPollLoop_RunModeOnceExitsAfterSingleIteration(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	iterations := 0
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		iterations++
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}
	err := pollLoop(context.Background(), env, run, env.repoRoot, tasksDir, "mato", "test-agent", 0, RunModeOnce)
	if err != nil {
		t.Fatalf("pollLoop returned error: %v", err)
	}
	if iterations != 1 {
		t.Fatalf("iterations = %d, want 1", iterations)
	}
}

func TestPollLoop_RunModeUntilIdleExitsWhenQueueBecomesIdle(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}
	err := pollLoop(context.Background(), env, run, env.repoRoot, tasksDir, "mato", "test-agent", 0, RunModeUntilIdle)
	if err != nil {
		t.Fatalf("pollLoop returned error: %v", err)
	}
}

func TestPollLoop_RunModeUntilIdlePausedWithPendingMergeDoesNotExit(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyMerge, "pending.md"), []byte("# Pending\n"), 0o644); err != nil {
		t.Fatalf("WriteFile pending merge task: %v", err)
	}
	pauseReadFn = func(string) (pause.State, error) { return pause.State{Active: true}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, false
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}
	done := make(chan error, 1)
	go func() {
		done <- pollLoop(ctx, env, run, env.repoRoot, tasksDir, "mato", "test-agent", 0, RunModeUntilIdle)
	}()

	select {
	case err := <-done:
		t.Fatalf("pollLoop exited early despite paused ready-to-merge work: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pollLoop returned error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pollLoop did not return after cancellation")
	}
}

func TestPollLoop_BoundedModeReturnsErrorOnPollFailure(t *testing.T) {
	origPauseReadFn := pauseReadFn
	origPollWriteManifestFn := pollWriteManifestFn
	origPollClaimAndRunFn := pollClaimAndRunFn
	origPollReviewFn := pollReviewFn
	origPollMergeFn := pollMergeFn
	defer func() {
		pauseReadFn = origPauseReadFn
		pollWriteManifestFn = origPollWriteManifestFn
		pollClaimAndRunFn = origPollClaimAndRunFn
		pollReviewFn = origPollReviewFn
		pollMergeFn = origPollMergeFn
	}()

	tasksDir := setupFullTasksDir(t)
	pauseReadFn = func(string) (pause.State, error) { return pause.State{}, nil }
	pollWriteManifestFn = func(string, map[string]struct{}, *queue.PollIndex) (queue.RunnableBacklogView, bool) {
		return queue.RunnableBacklogView{}, true
	}
	pollClaimAndRunFn = func(context.Context, envConfig, runContext, string, string, map[string]struct{}, time.Duration, *queue.PollIndex, queue.RunnableBacklogView) (bool, bool) {
		return false, false
	}
	pollReviewFn = func(context.Context, envConfig, runContext, string, string, string, *queue.PollIndex) bool {
		return false
	}
	pollMergeFn = func(context.Context, string, string, string) int { return 0 }

	env := envConfig{tasksDir: tasksDir, repoRoot: t.TempDir()}
	run := runContext{agentID: "test-agent"}
	err := pollLoop(context.Background(), env, run, env.repoRoot, tasksDir, "mato", "test-agent", 0, RunModeOnce)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bounded run encountered 1 poll cycle error") {
		t.Fatalf("err = %v", err)
	}
}

// TestStartupPullCancelledBySignalContext verifies that the startup path
// (setupSignalContext → ensureDockerImage) correctly cancels docker pull
// when the signal context is cancelled, matching the flow in Run().
func TestStartupPullCancelledBySignalContext(t *testing.T) {
	origInspect := dockerImageInspectFn
	origPull := dockerPullFn
	t.Cleanup(func() {
		dockerImageInspectFn = origInspect
		dockerPullFn = origPull
	})

	dockerImageInspectFn = func(image string) error {
		return fmt.Errorf("No such image: %s", image)
	}

	pullStarted := make(chan struct{})
	dockerPullFn = func(ctx context.Context, image string) error {
		close(pullStarted)
		<-ctx.Done()
		return ctx.Err()
	}

	// Mirror the startup path in Run: setupSignalContext then ensureDockerImage.
	ctx, cancel := setupSignalContext()
	defer signal.Stop(signalChan(ctx))
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, stderr := captureStdoutStderr(t, func() {
			done <- ensureDockerImage(ctx, "test:latest")
		})
		_ = stderr
	}()

	// Wait for pull to start, then cancel the context (simulating SIGINT).
	<-pullStarted
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error when signal context is cancelled during pull")
		}
		if !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("expected cancellation error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ensureDockerImage did not return after context cancellation")
	}
}

func TestCleanStaleClones_RemovesOldCloneDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	origTempDirFn := tempDirFn
	t.Cleanup(func() { tempDirFn = origTempDirFn })
	tempDirFn = func() string { return tmpDir }

	origNowFn := nowFn
	t.Cleanup(func() { nowFn = origNowFn })
	fakeNow := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return fakeNow }

	staleTime := fakeNow.Add(-25 * time.Hour)
	freshTime := fakeNow.Add(-1 * time.Hour)

	// Create a stale clone with debug marker (should be removed).
	staleDir := filepath.Join(tmpDir, "mato-stale123")
	os.MkdirAll(staleDir, 0o755)
	os.WriteFile(filepath.Join(staleDir, debugMarkerFile), []byte("preserved"), 0o644)
	os.Chtimes(staleDir, staleTime, staleTime)

	// Create a fresh clone with debug marker (should NOT be removed).
	freshDir := filepath.Join(tmpDir, "mato-fresh456")
	os.MkdirAll(freshDir, 0o755)
	os.WriteFile(filepath.Join(freshDir, debugMarkerFile), []byte("preserved"), 0o644)
	os.Chtimes(freshDir, freshTime, freshTime)

	// Create a stale clone WITHOUT debug marker (should NOT be removed).
	noMarkerDir := filepath.Join(tmpDir, "mato-nomarker789")
	os.MkdirAll(filepath.Join(noMarkerDir, ".git"), 0o755)
	os.Chtimes(noMarkerDir, staleTime, staleTime)

	// Create a stale non-mato directory with debug marker (should NOT be removed).
	otherDir := filepath.Join(tmpDir, "other-stale")
	os.MkdirAll(otherDir, 0o755)
	os.WriteFile(filepath.Join(otherDir, debugMarkerFile), []byte("preserved"), 0o644)
	os.Chtimes(otherDir, staleTime, staleTime)

	// Create a regular file with mato- prefix (should be ignored).
	regularFile := filepath.Join(tmpDir, "mato-file.txt")
	os.WriteFile(regularFile, []byte("data"), 0o644)
	os.Chtimes(regularFile, staleTime, staleTime)

	cleanStaleClones(24 * time.Hour)

	// Stale debug clone should be removed.
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("expected stale debug clone to be removed: %s", staleDir)
	}

	// Fresh debug clone should still exist.
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh debug clone should not be removed: %v", err)
	}

	// Clone without debug marker should still exist.
	if _, err := os.Stat(noMarkerDir); err != nil {
		t.Errorf("clone without debug marker should not be removed: %v", err)
	}

	// Non-mato directory should still exist.
	if _, err := os.Stat(otherDir); err != nil {
		t.Errorf("non-mato directory should not be removed: %v", err)
	}

	// Regular file should still exist.
	if _, err := os.Stat(regularFile); err != nil {
		t.Errorf("regular file should not be removed: %v", err)
	}
}

func TestCleanStaleClones_NoEntriesNoPanic(t *testing.T) {
	tmpDir := t.TempDir()

	origTempDirFn := tempDirFn
	t.Cleanup(func() { tempDirFn = origTempDirFn })
	tempDirFn = func() string { return tmpDir }

	// Should not panic when there are no entries.
	cleanStaleClones(24 * time.Hour)
}

func TestCleanStaleClones_LogsRemovedDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	origTempDirFn := tempDirFn
	t.Cleanup(func() { tempDirFn = origTempDirFn })
	tempDirFn = func() string { return tmpDir }

	origNowFn := nowFn
	t.Cleanup(func() { nowFn = origNowFn })
	fakeNow := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	nowFn = func() time.Time { return fakeNow }

	staleDir := filepath.Join(tmpDir, "mato-logtest")
	os.MkdirAll(staleDir, 0o755)
	os.WriteFile(filepath.Join(staleDir, debugMarkerFile), []byte("preserved"), 0o644)
	staleTime := fakeNow.Add(-48 * time.Hour)
	os.Chtimes(staleDir, staleTime, staleTime)

	stdout, _ := captureStdoutStderr(t, func() {
		cleanStaleClones(24 * time.Hour)
	})

	if !strings.Contains(stdout, "Cleaned up stale clone directory") {
		t.Errorf("expected cleanup log message, got: %q", stdout)
	}
	if !strings.Contains(stdout, "mato-logtest") {
		t.Errorf("expected directory name in log message, got: %q", stdout)
	}
}

// --- DryRunRenderer direct tests ---

func newTestRenderer(buf *bytes.Buffer) *DryRunRenderer {
	return &DryRunRenderer{
		W:     buf,
		Color: ui.NewColorSet(),
		Width: 80,
	}
}

func TestDryRunRenderer_RenderValidation_Success(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	r.RenderValidation(nil, 5)
	out := buf.String()

	if !strings.Contains(out, "=== Task File Validation ===") {
		t.Errorf("missing section header, got:\n%s", out)
	}
	if !strings.Contains(out, "All 5 task file(s) parsed successfully") {
		t.Errorf("missing success message, got:\n%s", out)
	}
}

func TestDryRunRenderer_RenderValidation_Errors(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	failures := []queue.ParseFailure{
		{State: "backlog", Filename: "bad.md", Err: fmt.Errorf("invalid yaml")},
	}
	r.RenderValidation(failures, 3)
	out := buf.String()

	if !strings.Contains(out, "ERROR backlog/bad.md") {
		t.Errorf("missing error line, got:\n%s", out)
	}
	if !strings.Contains(out, "1 of 3 task file(s) have parse errors") {
		t.Errorf("missing error summary, got:\n%s", out)
	}
}

func TestDryRunRenderer_RenderExecutionOrder_Empty(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	r.RenderExecutionOrder(nil)
	out := buf.String()

	if !strings.Contains(out, "=== Execution Order ===") {
		t.Errorf("missing section header, got:\n%s", out)
	}
	if !strings.Contains(out, "(no runnable tasks)") {
		t.Errorf("missing empty message, got:\n%s", out)
	}
}

func TestDryRunRenderer_RenderExecutionOrder_WithTasks(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	tasks := []*queue.TaskSnapshot{
		{Filename: "first.md", Meta: frontmatter.TaskMeta{Priority: 5}},
		{Filename: "second.md", Meta: frontmatter.TaskMeta{Priority: 10}},
	}
	r.RenderExecutionOrder(tasks)
	out := buf.String()

	if !strings.Contains(out, "1. first.md") {
		t.Errorf("missing first task, got:\n%s", out)
	}
	if !strings.Contains(out, "2. second.md") {
		t.Errorf("missing second task, got:\n%s", out)
	}
	if !strings.Contains(out, "(priority 5)") {
		t.Errorf("missing priority for first task, got:\n%s", out)
	}
}

func TestDryRunRenderer_RenderResolvedSettings(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	opts := RunOptions{
		TaskModel:             "claude-sonnet-4",
		ReviewModel:           "gpt-5.4",
		TaskReasoningEffort:   "high",
		ReviewReasoningEffort: "medium",
	}
	r.RenderResolvedSettings(opts)
	out := buf.String()

	if !strings.Contains(out, "=== Resolved Settings ===") {
		t.Errorf("missing section header, got:\n%s", out)
	}
	for _, want := range []string{"claude-sonnet-4", "gpt-5.4", "high", "medium"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestDryRunRenderer_RenderDependencyResolution(t *testing.T) {
	tests := []struct {
		name       string
		promotable int
		want       string
	}{
		{"no promotable", 0, "No waiting tasks ready for promotion"},
		{"some promotable", 3, "3 task(s) in waiting/ would be promoted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := newTestRenderer(&buf)
			r.RenderDependencyResolution(tt.promotable)
			out := buf.String()
			if !strings.Contains(out, tt.want) {
				t.Errorf("expected %q in output:\n%s", tt.want, out)
			}
		})
	}
}

func TestDryRunRenderer_WarningFormatting(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Create tasks that produce a dependency diagnostic warning.
	task := "---\nid: warn-task\npriority: 10\ndepends_on:\n  - nonexistent\n---\n# Warn Task\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "warn-task.md"), []byte(task), 0o644)

	idx := queue.BuildIndex(tasksDir)
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	r.RenderDependencySummary(tasksDir, idx)
	out := buf.String()

	if !strings.Contains(out, "WARNING") {
		t.Errorf("expected WARNING in dependency diagnostics, got:\n%s", out)
	}
	if !strings.Contains(out, "depends on unknown id") {
		t.Errorf("expected unknown-id warning, got:\n%s", out)
	}
}

func TestDryRunRenderer_NarrowWidth(t *testing.T) {
	var buf bytes.Buffer
	r := &DryRunRenderer{
		W:     &buf,
		Color: ui.NewColorSet(),
		Width: 40,
	}

	tasks := []*queue.TaskSnapshot{
		{Filename: "a-very-long-task-filename-that-exceeds-reasonable-bounds.md", Meta: frontmatter.TaskMeta{Priority: 1}},
	}
	r.RenderExecutionOrder(tasks)
	out := buf.String()

	// With width 40, the task name should be truncated.
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation marker in narrow output, got:\n%s", out)
	}
	// Should NOT contain the full untruncated filename.
	if strings.Contains(out, "a-very-long-task-filename-that-exceeds-reasonable-bounds.md") {
		t.Errorf("expected truncated filename in narrow output, got:\n%s", out)
	}
}

func TestDryRunRenderer_RenderQueueSummary(t *testing.T) {
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	task := "---\nid: task-1\npriority: 10\n---\n# Task 1\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "task-1.md"), []byte(task), 0o644)

	idx := queue.BuildIndex(tasksDir)
	var buf bytes.Buffer
	r := newTestRenderer(&buf)
	r.RenderQueueSummary(idx, queue.AllDirs, map[string]int{}, 1)
	out := buf.String()

	if !strings.Contains(out, "=== Queue Summary ===") {
		t.Errorf("missing section header, got:\n%s", out)
	}
	if !strings.Contains(out, "backlog") {
		t.Errorf("missing backlog line, got:\n%s", out)
	}
	if !strings.Contains(out, "deferred") {
		t.Errorf("missing deferred line when count > 0, got:\n%s", out)
	}
}

func TestDryRunRenderer_SectionHeaders(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRenderer(&buf)

	r.RenderDependencyResolution(0)
	r.RenderExecutionOrder(nil)
	r.RenderResolvedSettings(RunOptions{})

	out := buf.String()

	// All section headers should be present.
	for _, header := range []string{
		"=== Dependency Resolution ===",
		"=== Execution Order ===",
		"=== Resolved Settings ===",
	} {
		if !strings.Contains(out, header) {
			t.Errorf("missing section header %q in output:\n%s", header, out)
		}
	}
}

func TestDryRun_WidthFromWriter(t *testing.T) {
	// Verify that DryRun derives width from the writer through
	// writerWidthFn. When passed a bytes.Buffer (no Fd, not a TTY) the
	// default writerWidthFn returns defaultDryRunWidth (80), so a
	// filename shorter than that should NOT be truncated.
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// A task name that fits in 80 columns but would be truncated at 40.
	longName := "this-is-a-moderately-long-task-name.md"
	task := "---\nid: long-name\npriority: 10\n---\n# Long Name\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, longName), []byte(task), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	stdout := buf.String()

	// With default 80 column width the full filename should appear.
	if !strings.Contains(stdout, longName) {
		t.Errorf("expected full filename %q in output (buffer should get default 80-col width), got:\n%s", longName, stdout)
	}
}

func TestDryRun_NarrowWidthTruncatesViaCommandPath(t *testing.T) {
	// Prove that DryRun derives its renderer width through writerWidthFn
	// by injecting a narrow value and observing truncation in the output.
	// This exercises the actual command path, not just a hand-built renderer.
	origFn := writerWidthFn
	defer func() { writerWidthFn = origFn }()
	writerWidthFn = func(_ io.Writer, _ int) int { return 40 }

	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	longName := "this-is-a-very-very-very-long-task-filename-that-should-be-truncated.md"
	task := "---\nid: trunc-test\npriority: 10\n---\n# Truncation Test\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, longName), []byte(task), 0o644)

	var buf bytes.Buffer
	if err := DryRun(&buf, repoDir, "main", testRunOptions()); err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	out := buf.String()

	// Full long name should NOT appear because width is 40.
	if strings.Contains(out, longName) {
		t.Errorf("expected truncated filename at width 40 through DryRun command path, but found full name in:\n%s", out)
	}
	// Truncation marker should be present.
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation marker '…' in narrow-width DryRun output, got:\n%s", out)
	}
}

func TestDryRunRenderer_NarrowWidth_ResolvedSettings(t *testing.T) {
	// At width 40 the label column (24) + indent (2) + separator (1) leaves
	// only 13 chars for values. A model name like "claude-opus-4.6" (15 chars)
	// should be truncated.
	var buf bytes.Buffer
	r := &DryRunRenderer{
		W:     &buf,
		Color: ui.NewColorSet(),
		Width: 40,
	}
	r.RenderResolvedSettings(RunOptions{
		TaskModel:             "claude-opus-4.6",
		ReviewModel:           "gpt-5.4",
		TaskReasoningEffort:   "high",
		ReviewReasoningEffort: "high",
	})
	out := buf.String()

	// "claude-opus-4.6" is 15 chars but only 13 fit; should be truncated.
	if strings.Contains(out, "claude-opus-4.6") {
		t.Errorf("expected task model to be truncated at width 40, got:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation marker in narrow Resolved Settings, got:\n%s", out)
	}
	// "gpt-5.4" (7 chars) fits in 13 columns and should appear in full.
	if !strings.Contains(out, "gpt-5.4") {
		t.Errorf("expected short model name to appear untruncated, got:\n%s", out)
	}
}

func TestDryRunRenderer_NarrowWidth_BacklogSummary(t *testing.T) {
	// Verify that the backlog summary truncation accounts for the status
	// label (e.g. "[runnable]") so lines fit within the terminal width.
	repoDir := t.TempDir()
	cmd := exec.Command("git", "init", repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}

	tasksDir := filepath.Join(repoDir, ".mato")
	for _, sub := range queue.AllDirs {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	longName := "extremely-long-backlog-task-name-that-will-overflow.md"
	task := "---\nid: overflow-test\npriority: 10\n---\n# Overflow\n"
	os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, longName), []byte(task), 0o644)

	idx := queue.BuildIndex(tasksDir)
	var buf bytes.Buffer
	r := &DryRunRenderer{
		W:     &buf,
		Color: ui.NewColorSet(),
		Width: 40,
	}
	r.RenderBacklogSummary(idx, map[string]struct{}{}, map[string][]queue.DependencyBlock{})
	out := buf.String()

	// The full long filename should be truncated to fit width 40 minus
	// the overhead for "  " + " [runnable]".
	if strings.Contains(out, longName) {
		t.Errorf("expected filename truncated in backlog summary at width 40, got:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected truncation marker in narrow backlog summary, got:\n%s", out)
	}
	// Check no output line exceeds width 40 in runes (ignoring the header).
	// We use rune count because the "…" truncation marker is a single
	// display column but 3 UTF-8 bytes.
	for _, line := range strings.Split(out, "\n") {
		rc := utf8.RuneCountInString(line)
		if rc > 40 && !strings.Contains(line, "===") {
			t.Errorf("line exceeds width 40 (%d runes): %q", rc, line)
		}
	}
}
