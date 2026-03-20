package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/queue"
)

// substitutePromptPlaceholders replaces the 3 prompt placeholders with real paths.
func substitutePromptPlaceholders(script, tasksDir, branch string) string {
	s := strings.ReplaceAll(script, "TASKS_DIR_PLACEHOLDER", tasksDir)
	s = strings.ReplaceAll(s, "TARGET_BRANCH_PLACEHOLDER", branch)
	s = strings.ReplaceAll(s, "MESSAGES_DIR_PLACEHOLDER", filepath.Join(tasksDir, "messages"))
	return s
}

// runBash executes a bash script in the given directory with the given env vars.
// Returns combined stdout+stderr and any error.
func runBash(t *testing.T, dir string, env []string, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type promptEventMessage struct {
	ID     string   `json:"id"`
	From   string   `json:"from"`
	Type   string   `json:"type"`
	Task   string   `json:"task"`
	Branch string   `json:"branch"`
	Files  []string `json:"files"`
	Body   string   `json:"body"`
	SentAt string   `json:"sent_at"`
}

func taskInstructionsPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "runner", "task-instructions.md"))
}

func promptStateBlock(t *testing.T, state string) string {
	t.Helper()
	data, err := os.ReadFile(taskInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(task instructions): %v", err)
	}

	sectionStart := strings.Index(string(data), "## STATE: "+state)
	if sectionStart < 0 {
		t.Fatalf("state %q not found in task instructions", state)
	}
	section := string(data[sectionStart:])

	blockStart := strings.Index(section, "```bash\n")
	if blockStart < 0 {
		t.Fatalf("bash block for state %q not found", state)
	}
	section = section[blockStart+len("```bash\n"):]

	blockEnd := strings.Index(section, "\n```")
	if blockEnd < 0 {
		t.Fatalf("bash block terminator for state %q not found", state)
	}
	return strings.TrimSpace(section[:blockEnd])
}

// promptPreamble extracts the variable-initialization bash block that precedes
// all STATE sections. Tests that run individual state blocks must prepend this
// so variables like AGENT_ID are always defined under bash strict mode.
func promptPreamble(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(taskInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(task instructions): %v", err)
	}
	text := string(data)
	firstState := strings.Index(text, "## STATE:")
	if firstState < 0 {
		t.Fatal("no STATE sections found in task instructions")
	}
	preamble := text[:firstState]
	blockStart := strings.Index(preamble, "```bash\n")
	if blockStart < 0 {
		return ""
	}
	preamble = preamble[blockStart+len("```bash\n"):]
	blockEnd := strings.Index(preamble, "\n```")
	if blockEnd < 0 {
		t.Fatal("preamble bash block terminator not found")
	}
	return strings.TrimSpace(preamble[:blockEnd])
}

func TestPromptNoPushInstructions(t *testing.T) {
	// Verify the agent prompt does not contain push or file-move instructions.
	data, err := os.ReadFile(taskInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(task instructions): %v", err)
	}
	text := string(data)
	if strings.Contains(text, "git push") {
		t.Fatal("task instructions should not contain 'git push'; host handles pushing")
	}
	if strings.Contains(text, "PUSH_BRANCH") {
		t.Fatal("task instructions should not contain PUSH_BRANCH state")
	}
	if strings.Contains(text, "CREATE_BRANCH") {
		t.Fatal("task instructions should not contain CREATE_BRANCH state")
	}
	if strings.Contains(text, "MARK_READY") {
		t.Fatal("task instructions should not contain MARK_READY state")
	}
	// ON_FAILURE should not move files — the host handles that.
	onFailure := promptStateBlock(t, "ON_FAILURE")
	if strings.Contains(onFailure, "mv ") {
		t.Fatal("ON_FAILURE should not move files; host handles file moves")
	}
	if strings.Contains(onFailure, "git checkout") {
		t.Fatal("ON_FAILURE should not checkout branches; host handles cleanup")
	}
}

func createPromptClone(t *testing.T, repoRoot, tasksDir string) string {
	t.Helper()

	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		t.Fatalf("git.CreateClone: %v", err)
	}
	t.Cleanup(func() { git.RemoveClone(cloneDir) })

	configureCloneIdentity(t, cloneDir)
	appendGitExclude(t, cloneDir, "/.tasks", "/.tasks/")

	cloneTasksDir := filepath.Join(cloneDir, ".tasks")
	if err := os.Symlink(tasksDir, cloneTasksDir); err != nil {
		t.Fatalf("os.Symlink(%s, %s): %v", tasksDir, cloneTasksDir, err)
	}

	return cloneDir
}

func appendGitExclude(t *testing.T, cloneDir string, patterns ...string) {
	t.Helper()

	excludePath := filepath.Join(cloneDir, ".git", "info", "exclude")
	existing := ""
	if data, err := os.ReadFile(excludePath); err == nil {
		existing = string(data)
	} else if os.IsNotExist(err) {
		// Templates directory may not be available (e.g. inside Docker);
		// create .git/info/exclude so git excludes still work.
		if mkErr := os.MkdirAll(filepath.Dir(excludePath), 0o755); mkErr != nil {
			t.Fatalf("os.MkdirAll(%s): %v", filepath.Dir(excludePath), mkErr)
		}
	} else {
		t.Fatalf("os.ReadFile(%s): %v", excludePath, err)
	}

	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("os.OpenFile(%s): %v", excludePath, err)
	}
	defer f.Close()

	for _, pattern := range patterns {
		line := pattern + "\n"
		if strings.Contains(existing, line) {
			continue
		}
		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("write %s: %v", excludePath, err)
		}
		existing += line
	}
}

func readPromptEventMessages(t *testing.T, tasksDir string) []promptEventMessage {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(tasksDir, "messages", "events", "*.json"))
	if err != nil {
		t.Fatalf("filepath.Glob(events): %v", err)
	}
	sort.Strings(paths)

	messages := make([]promptEventMessage, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("os.ReadFile(%s): %v", path, err)
		}
		var msg promptEventMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("json.Unmarshal(%s): %v\ncontent: %s", path, err, string(data))
		}
		messages = append(messages, msg)
	}

	return messages
}

func findPromptEventMessage(t *testing.T, tasksDir, msgType string) promptEventMessage {
	t.Helper()

	for _, msg := range readPromptEventMessages(t, tasksDir) {
		if msg.Type == msgType {
			return msg
		}
	}
	t.Fatalf("message type %q not found", msgType)
	return promptEventMessage{}
}

func countFailureRecords(content string) int {
	return strings.Count(content, "<!-- failure:")
}

func quotedPath(path string) string {
	return fmt.Sprintf("%q", path)
}

func TestPromptVerifyClaim(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)

	claimed, err := queue.SelectAndClaimTask(tasksDir, "test-agent-1", nil)
	if claimed != nil {
		t.Fatalf("expected no task (backlog empty), got %+v", claimed)
	}

	writeTask(t, tasksDir, "backlog", "task-alpha.md", "# Task Alpha\nDo alpha.\n")
	writeTask(t, tasksDir, "backlog", "task-beta.md", "# Task Beta\nDo beta.\n")
	writeFile(t, filepath.Join(tasksDir, ".queue"), "task-alpha.md\ntask-beta.md\n")

	claimed, err = queue.SelectAndClaimTask(tasksDir, "test-agent-1", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if claimed == nil || claimed.Filename != "task-alpha.md" {
		t.Fatalf("expected task-alpha.md, got %+v", claimed)
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	script := strings.Join([]string{
		promptPreamble(t),
		promptStateBlock(t, "VERIFY_CLAIM"),
		`echo "FILENAME=$FILENAME"`,
		`echo "BRANCH=$BRANCH"`,
		`echo "TASK_TITLE=$TASK_TITLE"`,
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-agent-1",
		"MATO_TASK_FILE=" + claimed.Filename,
		"MATO_TASK_BRANCH=" + claimed.Branch,
		"MATO_TASK_TITLE=" + claimed.Title,
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, "in-progress", claimed.Filename),
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify claim: %v\noutput:\n%s", err, out)
	}

	alphaInProgress := filepath.Join(tasksDir, "in-progress", "task-alpha.md")
	mustExist(t, alphaInProgress)
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "task-alpha.md"))
	mustExist(t, filepath.Join(tasksDir, "backlog", "task-beta.md"))

	contents := readFile(t, alphaInProgress)
	if !strings.HasPrefix(contents, "<!-- claimed-by: test-agent-1  claimed-at: ") {
		t.Fatalf("claimed task missing claimed-by header: %q", contents)
	}
	if !strings.Contains(out, "FILENAME=task-alpha.md") {
		t.Fatalf("verify claim output missing filename: %s", out)
	}
	if !strings.Contains(out, "BRANCH=task/task-alpha") {
		t.Fatalf("verify claim output missing branch: %s", out)
	}
}

func TestPromptHostCreatesBranch(t *testing.T) {
	// The host creates the task branch before the agent runs.
	// Verify the agent can commit on the pre-created branch.
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-3  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)

	// Host creates the task branch (simulating runOnce pre-agent logic)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")

	script := strings.Join([]string{
		promptPreamble(t),
		`BRANCH="task/my-task"`,
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		`TASK_TITLE="My Task"`,
		`echo "hello world" > hello.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, nil, script)
	if err != nil {
		t.Fatalf("runBash commit on host-created branch: %v\noutput:\n%s", err, out)
	}

	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "branch", "--show-current")); got != "task/my-task" {
		t.Fatalf("current branch = %q, want %q", got, "task/my-task")
	}
	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "log", "--format=%s", "-1")); got != "My Task" {
		t.Fatalf("commit subject = %q, want %q", got, "My Task")
	}
	body := strings.TrimSpace(mustGitOutput(t, cloneDir, "log", "--format=%b", "-1"))
	if body == "" {
		t.Fatal("commit body is empty; expected a non-empty description")
	}
	if !strings.Contains(body, "hello.txt") {
		t.Fatalf("commit body should list changed files, got:\n%s", body)
	}
	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "show", "HEAD:hello.txt")); got != "hello world" {
		t.Fatalf("hello.txt contents = %q, want %q", got, "hello world")
	}
}

func TestPromptCommitIncludesDescription(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	// Host creates the task branch
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")

	script := strings.Join([]string{
		promptPreamble(t),
		`BRANCH="task/my-task"`,
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		`TASK_TITLE="My Task"`,
		`echo "aaa" > a.txt`,
		`echo "bbb" > b.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, nil, script)
	if err != nil {
		t.Fatalf("runBash: %v\noutput:\n%s", err, out)
	}

	subject := strings.TrimSpace(mustGitOutput(t, cloneDir, "log", "--format=%s", "-1"))
	if subject != "My Task" {
		t.Fatalf("commit subject = %q, want %q", subject, "My Task")
	}

	body := strings.TrimSpace(mustGitOutput(t, cloneDir, "log", "--format=%b", "-1"))
	if body == "" {
		t.Fatal("commit body is empty; expected a non-empty description")
	}
	if !strings.Contains(body, "Task: my-task.md") {
		t.Fatalf("commit body should reference the task filename, got:\n%s", body)
	}
	if !strings.Contains(body, "a.txt") || !strings.Contains(body, "b.txt") {
		t.Fatalf("commit body should list changed files (a.txt, b.txt), got:\n%s", body)
	}
}

func TestHostPushAndMarkReady(t *testing.T) {
	// Simulate the host post-agent push: push branch, write marker, move to ready-for-review.
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-4  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")
	writeFile(t, filepath.Join(cloneDir, "hello.txt"), "hello world\n")
	mustGitOutput(t, cloneDir, "add", "hello.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	// Host pushes the branch to the repo
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", "task/my-task")

	// Verify the branch exists in the host repo
	mustGitOutput(t, repoRoot, "rev-parse", "--verify", "refs/heads/task/my-task")

	// Host writes branch marker
	f, err := os.OpenFile(filepath.Join(tasksDir, "in-progress", "my-task.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open task file: %v", err)
	}
	fmt.Fprintf(f, "\n<!-- branch: task/my-task -->\n")
	f.Close()

	// Host moves to ready-for-review
	if err := os.Rename(
		filepath.Join(tasksDir, "in-progress", "my-task.md"),
		filepath.Join(tasksDir, "ready-for-review", "my-task.md"),
	); err != nil {
		t.Fatalf("move to ready-for-review: %v", err)
	}

	mustExist(t, filepath.Join(tasksDir, "ready-for-review", "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, "in-progress", "my-task.md"))

	contents := readFile(t, filepath.Join(tasksDir, "ready-for-review", "my-task.md"))
	if !strings.Contains(contents, "<!-- branch: task/my-task -->") {
		t.Fatalf("ready task missing branch metadata: %s", contents)
	}
}

func TestHostBranchMarkerWrittenAfterPush(t *testing.T) {
	// Verify branch marker is written to the task file after the host pushes.
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-branch  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")
	writeFile(t, filepath.Join(cloneDir, "branch.txt"), "branch metadata\n")
	mustGitOutput(t, cloneDir, "add", "branch.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	// Host pushes
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", "task/my-task")

	// Before marker: no branch comment
	contents := readFile(t, filepath.Join(tasksDir, "in-progress", "my-task.md"))
	if strings.Contains(contents, "<!-- branch:") {
		t.Fatalf("branch marker should not exist before host writes it: %s", contents)
	}

	// Host writes marker
	f, err := os.OpenFile(filepath.Join(tasksDir, "in-progress", "my-task.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open task file: %v", err)
	}
	fmt.Fprintf(f, "\n<!-- branch: task/my-task -->\n")
	f.Close()

	contents = readFile(t, filepath.Join(tasksDir, "in-progress", "my-task.md"))
	if !strings.Contains(contents, "<!-- branch: task/my-task -->") {
		t.Fatalf("task file missing branch metadata after host write: %s", contents)
	}
}

func TestHostReplacesExistingRemoteBranch(t *testing.T) {
	// When a task is retried, the host creates a fresh branch and force-pushes,
	// replacing the stale branch from the prior attempt.
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-stale  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	// Simulate a prior attempt that left a stale branch on the host repo.
	mustGitOutput(t, repoRoot, "checkout", "-b", "task/my-task", "mato")
	writeFile(t, filepath.Join(repoRoot, "stale.txt"), "stale branch\n")
	mustGitOutput(t, repoRoot, "add", "stale.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "stale branch work")
	mustGitOutput(t, repoRoot, "checkout", "mato")
	writeFile(t, filepath.Join(repoRoot, "base.txt"), "advanced target\n")
	mustGitOutput(t, repoRoot, "add", "base.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "advance mato")

	// Host creates a fresh clone and new task branch from the current target.
	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")

	// Agent makes changes and commits on the fresh branch.
	writeFile(t, filepath.Join(cloneDir, "fresh.txt"), "fresh branch\n")
	mustGitOutput(t, cloneDir, "add", "fresh.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	// Host force-pushes, replacing the stale branch.
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", "task/my-task")

	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "task/my-task:fresh.txt")); got != "fresh branch" {
		t.Fatalf("fresh.txt on remote branch = %q, want %q", got, "fresh branch")
	}
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "task/my-task:base.txt")); got != "advanced target" {
		t.Fatalf("base.txt on remote branch = %q, want %q", got, "advanced target")
	}
	if _, err := git.Output(repoRoot, "show", "task/my-task:stale.txt"); err == nil {
		t.Fatal("expected pre-existing remote branch content to be replaced by the newly pushed task branch")
	}
}

func TestPromptOnFailure(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", strings.Join([]string{
		"<!-- claimed-by: test-agent-5  claimed-at: 2026-01-01T00:00:00Z -->",
		"# My Task",
		"<!-- failure: prior -->",
		"",
	}, "\n"))

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task", "mato")

	script := strings.Join([]string{
		promptPreamble(t),
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		`FAIL_STEP="WORK"`,
		`FAIL_REASON="test failure"`,
		promptStateBlock(t, "ON_FAILURE"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-5"}, script)
	if err != nil {
		t.Fatalf("runBash on failure: %v\noutput:\n%s", err, out)
	}

	// ON_FAILURE writes the failure record but does NOT move the file.
	// The host handles the move to backlog/ via recoverStuckTask.
	inProgressTask := filepath.Join(tasksDir, "in-progress", "my-task.md")
	mustExist(t, inProgressTask)

	contents := readFile(t, inProgressTask)
	if got := countFailureRecords(contents); got != 2 {
		t.Fatalf("failure record count = %d, want 2\ncontents:\n%s", got, contents)
	}
	if !strings.Contains(contents, "step=WORK") || !strings.Contains(contents, "error=test failure") {
		t.Fatalf("failure metadata missing from task: %s", contents)
	}
}

func TestPromptOnFailureDoesNotMoveFile(t *testing.T) {
	// Even with many prior failures, ON_FAILURE only writes the failure record.
	// The host moves to backlog and handles retry budgets.
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", strings.Join([]string{
		"<!-- claimed-by: test-agent-6  claimed-at: 2026-01-01T00:00:00Z -->",
		"# My Task",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"",
	}, "\n"))

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task", "mato")

	script := strings.Join([]string{
		promptPreamble(t),
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		`FAIL_STEP="WORK"`,
		`FAIL_REASON="test failure"`,
		promptStateBlock(t, "ON_FAILURE"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-6"}, script)
	if err != nil {
		t.Fatalf("runBash on failure with many priors: %v\noutput:\n%s", err, out)
	}

	// Task stays in in-progress/, NOT moved to backlog/ — host handles that.
	inProgressTask := filepath.Join(tasksDir, "in-progress", "my-task.md")
	mustExist(t, inProgressTask)
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, "failed", "my-task.md"))

	contents := readFile(t, inProgressTask)
	if got := countFailureRecords(contents); got != 3 {
		t.Fatalf("failure record count = %d, want 3\ncontents:\n%s", got, contents)
	}
}

func TestPromptTwoAgentsParallelClaim(t *testing.T) {
	_, tasksDir := setupTestRepo(t)
	for _, name := range []string{"task-alpha.md", "task-beta.md", "task-gamma.md"} {
		writeTask(t, tasksDir, "backlog", name, "# "+strings.TrimSuffix(name, ".md")+"\n")
	}
	writeFile(t, filepath.Join(tasksDir, ".queue"), "task-alpha.md\ntask-beta.md\ntask-gamma.md\n")

	// Both agents claim via Go; each gets a different task.
	claimedA, err := queue.SelectAndClaimTask(tasksDir, "agent-a", nil)
	if err != nil {
		t.Fatalf("claim agent-a: %v", err)
	}
	if claimedA == nil {
		t.Fatal("agent-a got no task")
	}

	claimedB, err := queue.SelectAndClaimTask(tasksDir, "agent-b", nil)
	if err != nil {
		t.Fatalf("claim agent-b: %v", err)
	}
	if claimedB == nil {
		t.Fatal("agent-b got no task")
	}

	if claimedA.Filename == claimedB.Filename {
		t.Fatalf("both agents claimed the same task: %s", claimedA.Filename)
	}

	inProgress := markdownFileNames(t, filepath.Join(tasksDir, "in-progress"))
	if len(inProgress) != 2 {
		t.Fatalf("in-progress tasks = %v, want 2 claimed tasks", inProgress)
	}
	backlog := markdownFileNames(t, filepath.Join(tasksDir, "backlog"))
	if len(backlog) != 1 {
		t.Fatalf("backlog tasks = %v, want 1 unclaimed task", backlog)
	}
}

func TestPromptFullLifecycleWithMerge(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "backlog", "add-hello.md", "# Add hello\nCreate hello.txt with hello world.\n")
	writeFile(t, filepath.Join(tasksDir, ".queue"), "add-hello.md\n")

	claimed, err := queue.SelectAndClaimTask(tasksDir, "test-agent-8", nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a task to be claimed")
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	// Host creates the task branch before the agent runs.
	mustGitOutput(t, cloneDir, "checkout", "-b", claimed.Branch)

	script := strings.Join([]string{
		promptPreamble(t),
		promptStateBlock(t, "VERIFY_CLAIM"),
		promptStateBlock(t, "WORK"),
		`echo "hello world" > hello.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=test-agent-8",
		"MATO_TASK_FILE=" + claimed.Filename,
		"MATO_TASK_BRANCH=" + claimed.Branch,
		"MATO_TASK_TITLE=" + claimed.Title,
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, "in-progress", claimed.Filename),
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash full lifecycle: %v\noutput:\n%s", err, out)
	}

	// Host post-agent: push branch, write marker, move to ready-for-review.
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", claimed.Branch)

	taskFile := filepath.Join(tasksDir, "in-progress", "add-hello.md")
	f, fErr := os.OpenFile(taskFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if fErr != nil {
		t.Fatalf("open task file: %v", fErr)
	}
	fmt.Fprintf(f, "\n<!-- branch: %s -->\n", claimed.Branch)
	f.Close()

	readyTask := filepath.Join(tasksDir, "ready-for-review", "add-hello.md")
	if err := os.Rename(taskFile, readyTask); err != nil {
		t.Fatalf("move to ready-for-review: %v", err)
	}

	mustExist(t, readyTask)
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "add-hello.md"))

	// Simulate review approval: move task from ready-for-review/ to ready-to-merge/
	mergeTask := filepath.Join(tasksDir, "ready-to-merge", "add-hello.md")
	if err := os.Rename(readyTask, mergeTask); err != nil {
		t.Fatalf("move to ready-to-merge: %v", err)
	}

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, "completed", "add-hello.md"))
	mustNotExist(t, mergeTask)
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:hello.txt")); got != "hello world" {
		t.Fatalf("hello.txt on mato = %q, want %q", got, "hello world")
	}
}
