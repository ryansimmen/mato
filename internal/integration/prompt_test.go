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
	"sync"
	"testing"

	"mato/internal/git"
	"mato/internal/merge"
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
	} else {
		t.Fatalf("os.ReadFile(%s): %v", excludePath, err)
	}

	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_WRONLY, 0o644)
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

func TestPromptClaimTask(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "backlog", "task-alpha.md", "# Task Alpha\nDo alpha.\n")
	writeTask(t, tasksDir, "backlog", "task-beta.md", "# Task Beta\nDo beta.\n")
	writeFile(t, filepath.Join(tasksDir, ".queue"), "task-alpha.md\ntask-beta.md\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, ".tasks")

	script := strings.Join([]string{
		promptStateBlock(t, "SELECT_TASK"),
		promptStateBlock(t, "CLAIM_TASK"),
		`echo "FILENAME=$FILENAME"`,
		`echo "BRANCH=$BRANCH"`,
		`echo "SAFE_NAME=$SAFE_NAME"`,
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-1"}, script)
	if err != nil {
		t.Fatalf("runBash claim task: %v\noutput:\n%s", err, out)
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
		t.Fatalf("claim output missing filename: %s", out)
	}
	if !strings.Contains(out, "BRANCH=task/task-alpha") {
		t.Fatalf("claim output missing branch: %s", out)
	}
	if !strings.Contains(out, "SAFE_NAME=task-alpha") {
		t.Fatalf("claim output missing safe name: %s", out)
	}

	intent := findPromptEventMessage(t, tasksDir, "intent")
	if intent.Task != "task-alpha.md" || intent.Branch != "task/task-alpha" || intent.From != "test-agent-1" {
		t.Fatalf("intent message = %+v, want task-alpha/task/task-alpha/test-agent-1", intent)
	}
}

func TestPromptClaimTaskRetryExhausted(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "backlog", "task-retry.md", strings.Join([]string{
		"# Retry task",
		"<!-- failure: one -->",
		"<!-- failure: two -->",
		"<!-- failure: three -->",
		"",
	}, "\n"))
	writeFile(t, filepath.Join(tasksDir, ".queue"), "task-retry.md\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	script := substitutePromptPlaceholders(strings.Join([]string{
		promptStateBlock(t, "SELECT_TASK"),
		promptStateBlock(t, "CLAIM_TASK"),
	}, "\n\n"), filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-2"}, script)
	if err != nil {
		t.Fatalf("runBash retry exhausted: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "Task exceeded retry budget and was moved to failed/.") {
		t.Fatalf("expected retry exhaustion output, got: %s", out)
	}

	mustExist(t, filepath.Join(tasksDir, "failed", "task-retry.md"))
	mustNotExist(t, filepath.Join(tasksDir, "in-progress", "task-retry.md"))
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "task-retry.md"))
}

func TestPromptCreateBranchAndCommit(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-3  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	script := strings.Join([]string{
		`BRANCH="task/my-task"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		`TASK_TITLE="My Task"`,
		promptStateBlock(t, "CREATE_BRANCH"),
		`echo "hello world" > hello.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, nil, script)
	if err != nil {
		t.Fatalf("runBash create branch and commit: %v\noutput:\n%s", err, out)
	}

	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "branch", "--show-current")); got != "task/my-task" {
		t.Fatalf("current branch = %q, want %q", got, "task/my-task")
	}
	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "log", "--format=%s", "-1")); got != "My Task" {
		t.Fatalf("commit subject = %q, want %q", got, "My Task")
	}
	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "show", "HEAD:hello.txt")); got != "hello world" {
		t.Fatalf("hello.txt contents = %q, want %q", got, "hello world")
	}
}

func TestPromptPushAndMarkReady(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-4  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task", "mato")
	writeFile(t, filepath.Join(cloneDir, "hello.txt"), "hello world\n")
	mustGitOutput(t, cloneDir, "add", "hello.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	script := strings.Join([]string{
		`AGENT_ID="${MATO_AGENT_ID:-unknown}"`,
		`FILENAME="my-task.md"`,
		`BRANCH="task/my-task"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		promptStateBlock(t, "PUSH_BRANCH"),
		promptStateBlock(t, "MARK_READY"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-4"}, script)
	if err != nil {
		t.Fatalf("runBash push and mark ready: %v\noutput:\n%s", err, out)
	}

	mustGitOutput(t, repoRoot, "rev-parse", "--verify", "refs/heads/task/my-task")
	mustExist(t, filepath.Join(tasksDir, "ready-to-merge", "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, "in-progress", "my-task.md"))

	warning := findPromptEventMessage(t, tasksDir, "conflict-warning")
	if warning.Task != "my-task.md" || warning.Branch != "task/my-task" || warning.From != "test-agent-4" {
		t.Fatalf("warning message = %+v, want my-task/task/my-task/test-agent-4", warning)
	}
	completion := findPromptEventMessage(t, tasksDir, "completion")
	if completion.Task != "my-task.md" || completion.Branch != "task/my-task" || completion.From != "test-agent-4" {
		t.Fatalf("completion message = %+v, want my-task/task/my-task/test-agent-4", completion)
	}
	if !strings.Contains(out, "Completed my-task.md on task/my-task and moved it to ready-to-merge/.") {
		t.Fatalf("mark ready output missing completion line: %s", out)
	}
}

func TestPromptRecordsBranchInTaskFile(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-branch  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task", "mato")
	writeFile(t, filepath.Join(cloneDir, "branch.txt"), "branch metadata\n")
	mustGitOutput(t, cloneDir, "add", "branch.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	script := strings.Join([]string{
		`AGENT_ID="${MATO_AGENT_ID:-unknown}"`,
		`FILENAME="my-task.md"`,
		`BRANCH="task/my-task"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		promptStateBlock(t, "PUSH_BRANCH"),
		promptStateBlock(t, "MARK_READY"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-branch"}, script)
	if err != nil {
		t.Fatalf("runBash record branch metadata: %v\noutput:\n%s", err, out)
	}

	readyTask := filepath.Join(tasksDir, "ready-to-merge", "my-task.md")
	contents := readFile(t, readyTask)
	if !strings.Contains(contents, "<!-- branch: task/my-task -->") {
		t.Fatalf("ready task missing branch metadata: %s", contents)
	}
}

func TestStaleRemoteBranchCleanedBeforeNewBranch(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "in-progress", "my-task.md", "<!-- claimed-by: test-agent-stale  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	mustGitOutput(t, repoRoot, "checkout", "-b", "task/my-task", "mato")
	writeFile(t, filepath.Join(repoRoot, "stale.txt"), "stale branch\n")
	mustGitOutput(t, repoRoot, "add", "stale.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "stale branch work")
	mustGitOutput(t, repoRoot, "checkout", "mato")
	writeFile(t, filepath.Join(repoRoot, "base.txt"), "advanced target\n")
	mustGitOutput(t, repoRoot, "add", "base.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "advance mato")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	script := strings.Join([]string{
		`AGENT_ID="${MATO_AGENT_ID:-unknown}"`,
		`FILENAME="my-task.md"`,
		`BRANCH="task/my-task"`,
		`TASK_TITLE="My Task"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		promptStateBlock(t, "CREATE_BRANCH"),
		`echo "fresh branch" > fresh.txt`,
		promptStateBlock(t, "COMMIT"),
		promptStateBlock(t, "PUSH_BRANCH"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-stale"}, script)
	if err != nil {
		t.Fatalf("runBash stale branch cleanup: %v\noutput:\n%s", err, out)
	}

	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "task/my-task:fresh.txt")); got != "fresh branch" {
		t.Fatalf("fresh.txt on remote branch = %q, want %q", got, "fresh branch")
	}
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "task/my-task:base.txt")); got != "advanced target" {
		t.Fatalf("base.txt on remote branch = %q, want %q", got, "advanced target")
	}
	if _, err := git.Output(repoRoot, "show", "task/my-task:stale.txt"); err == nil {
		t.Fatal("expected stale remote branch content to be removed before pushing fresh branch")
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
		`AGENT_ID="${MATO_AGENT_ID:-unknown}"`,
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

	backlogTask := filepath.Join(tasksDir, "backlog", "my-task.md")
	mustExist(t, backlogTask)
	mustNotExist(t, filepath.Join(tasksDir, "in-progress", "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, "failed", "my-task.md"))

	contents := readFile(t, backlogTask)
	if got := countFailureRecords(contents); got != 2 {
		t.Fatalf("failure record count = %d, want 2\ncontents:\n%s", got, contents)
	}
	if !strings.Contains(contents, "step=WORK") || !strings.Contains(contents, "error=test failure") {
		t.Fatalf("failure metadata missing from task: %s", contents)
	}
	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "branch", "--show-current")); got != "mato" {
		t.Fatalf("current branch after failure = %q, want %q", got, "mato")
	}
}

func TestPromptOnFailureExhaustsRetries(t *testing.T) {
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
		`AGENT_ID="${MATO_AGENT_ID:-unknown}"`,
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, ".tasks", "in-progress", "my-task.md")),
		`FAIL_STEP="WORK"`,
		`FAIL_REASON="test failure"`,
		promptStateBlock(t, "ON_FAILURE"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-6"}, script)
	if err != nil {
		t.Fatalf("runBash on failure exhausted: %v\noutput:\n%s", err, out)
	}

	failedTask := filepath.Join(tasksDir, "failed", "my-task.md")
	mustExist(t, failedTask)
	mustNotExist(t, filepath.Join(tasksDir, "in-progress", "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "my-task.md"))

	contents := readFile(t, failedTask)
	if got := countFailureRecords(contents); got != 3 {
		t.Fatalf("failure record count = %d, want 3\ncontents:\n%s", got, contents)
	}
	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "branch", "--show-current")); got != "mato" {
		t.Fatalf("current branch after failure = %q, want %q", got, "mato")
	}
}

func TestPromptTwoAgentsParallelClaim(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	for _, name := range []string{"task-alpha.md", "task-beta.md", "task-gamma.md"} {
		writeTask(t, tasksDir, "backlog", name, "# "+strings.TrimSuffix(name, ".md")+"\n")
	}
	writeFile(t, filepath.Join(tasksDir, ".queue"), "task-alpha.md\ntask-beta.md\ntask-gamma.md\n")

	cloneA := createPromptClone(t, repoRoot, tasksDir)
	cloneB := createPromptClone(t, repoRoot, tasksDir)
	scriptA := substitutePromptPlaceholders(strings.Join([]string{
		promptStateBlock(t, "SELECT_TASK"),
		promptStateBlock(t, "CLAIM_TASK"),
		`echo "FILENAME=$FILENAME"`,
	}, "\n\n"), filepath.Join(cloneA, ".tasks"), "mato")
	scriptB := substitutePromptPlaceholders(strings.Join([]string{
		promptStateBlock(t, "SELECT_TASK"),
		promptStateBlock(t, "CLAIM_TASK"),
		`echo "FILENAME=$FILENAME"`,
	}, "\n\n"), filepath.Join(cloneB, ".tasks"), "mato")

	type result struct {
		out string
		err error
	}
	results := make([]result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		results[0].out, results[0].err = runBash(t, cloneA, []string{"MATO_AGENT_ID=agent-a"}, scriptA)
	}()
	go func() {
		defer wg.Done()
		<-start
		results[1].out, results[1].err = runBash(t, cloneB, []string{"MATO_AGENT_ID=agent-b"}, scriptB)
	}()
	close(start)
	wg.Wait()

	for i, result := range results {
		if result.err != nil {
			t.Fatalf("parallel claim %d failed: %v\noutput:\n%s", i, result.err, result.out)
		}
	}

	inProgress := markdownFileNames(t, filepath.Join(tasksDir, "in-progress"))
	if len(inProgress) != 2 {
		t.Fatalf("in-progress tasks = %v, want 2 claimed tasks", inProgress)
	}
	backlog := markdownFileNames(t, filepath.Join(tasksDir, "backlog"))
	if len(backlog) != 1 {
		t.Fatalf("backlog tasks = %v, want 1 unclaimed task", backlog)
	}

	claimedByTask := map[string]string{}
	for _, name := range inProgress {
		contents := readFile(t, filepath.Join(tasksDir, "in-progress", name))
		switch {
		case strings.Contains(contents, "claimed-by: agent-a"):
			claimedByTask[name] = "agent-a"
		case strings.Contains(contents, "claimed-by: agent-b"):
			claimedByTask[name] = "agent-b"
		default:
			t.Fatalf("task %s missing expected claimant: %s", name, contents)
		}
	}
	if len(claimedByTask) != 2 {
		t.Fatalf("claimed tasks = %+v, want 2 unique claims", claimedByTask)
	}
	if claimedByTask[inProgress[0]] == claimedByTask[inProgress[1]] {
		t.Fatalf("same agent claimed both tasks: %+v", claimedByTask)
	}

	for _, result := range results {
		if strings.Count(result.out, "FILENAME=") != 1 {
			t.Fatalf("expected one claim per output, got: %s", result.out)
		}
	}
}

func TestPromptFullLifecycleWithMerge(t *testing.T) {
	repoRoot, tasksDir := setupTestRepo(t)
	writeTask(t, tasksDir, "backlog", "add-hello.md", "# Add hello\nCreate hello.txt with hello world.\n")
	writeFile(t, filepath.Join(tasksDir, ".queue"), "add-hello.md\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	script := strings.Join([]string{
		promptStateBlock(t, "SELECT_TASK"),
		promptStateBlock(t, "CLAIM_TASK"),
		promptStateBlock(t, "CREATE_BRANCH"),
		promptStateBlock(t, "WORK"),
		`echo "hello world" > hello.txt`,
		promptStateBlock(t, "COMMIT"),
		promptStateBlock(t, "PUSH_BRANCH"),
		promptStateBlock(t, "MARK_READY"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, ".tasks"), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-8"}, script)
	if err != nil {
		t.Fatalf("runBash full lifecycle: %v\noutput:\n%s", err, out)
	}

	readyTask := filepath.Join(tasksDir, "ready-to-merge", "add-hello.md")
	mustExist(t, readyTask)
	mustNotExist(t, filepath.Join(tasksDir, "backlog", "add-hello.md"))

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, "completed", "add-hello.md"))
	mustNotExist(t, readyTask)
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:hello.txt")); got != "hello world" {
		t.Fatalf("hello.txt on mato = %q, want %q", got, "hello world")
	}

	messages := readPromptEventMessages(t, tasksDir)
	if len(messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(messages))
	}
	types := []string{messages[0].Type, messages[1].Type, messages[2].Type}
	sort.Strings(types)
	if strings.Join(types, ",") != "completion,conflict-warning,intent" {
		t.Fatalf("message types = %v, want intent/conflict-warning/completion", types)
	}
}
