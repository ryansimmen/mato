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

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/queue"
	"mato/internal/testutil"
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

func TestPromptFileClaimsMentionDirectoryPrefixes(t *testing.T) {
	data, err := os.ReadFile(taskInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(task instructions): %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "directory prefixes ending with `/`") {
		t.Fatal("task instructions should explain that file claims may include directory prefixes")
	}
	if !strings.Contains(text, "falls under a claimed directory prefix") {
		t.Fatal("task instructions should explain how directory-prefix claims affect planned edits")
	}
}

func TestPromptFileClaimsMentionGlobPatterns(t *testing.T) {
	data, err := os.ReadFile(taskInstructionsPath(t))
	if err != nil {
		t.Fatalf("os.ReadFile(task instructions): %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "glob pattern") {
		t.Fatal("task instructions should explain that file claims may include glob patterns")
	}
	if !strings.Contains(text, "matches a glob") {
		t.Fatal("task instructions should explain how glob-pattern claims affect planned edits")
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
	appendGitExclude(t, cloneDir, "/"+dirs.Root, "/"+dirs.Root+"/")

	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)
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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	claimed, err := queue.SelectAndClaimTask(tasksDir, "test-agent-1", nil, 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask empty: %v", err)
	}
	if claimed != nil {
		t.Fatalf("expected no task (backlog empty), got %+v", claimed)
	}

	writeTask(t, tasksDir, queue.DirBacklog, "task-alpha.md", "# Task Alpha\nDo alpha.\n")
	writeTask(t, tasksDir, queue.DirBacklog, "task-beta.md", "# Task Beta\nDo beta.\n")

	claimed, err = queue.SelectAndClaimTask(tasksDir, "test-agent-1", []string{"task-alpha.md", "task-beta.md"}, 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if claimed == nil || claimed.Filename != "task-alpha.md" {
		t.Fatalf("expected task-alpha.md, got %+v", claimed)
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, claimed.Filename),
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify claim: %v\noutput:\n%s", err, out)
	}

	alphaInProgress := filepath.Join(tasksDir, queue.DirInProgress, "task-alpha.md")
	mustExist(t, alphaInProgress)
	mustNotExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-alpha.md"))
	mustExist(t, filepath.Join(tasksDir, queue.DirBacklog, "task-beta.md"))

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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", "<!-- claimed-by: test-agent-3  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)

	// Host creates the task branch (simulating runOnce pre-agent logic)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")

	script := strings.Join([]string{
		promptPreamble(t),
		`BRANCH="task/my-task"`,
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, dirs.Root, queue.DirInProgress, "my-task.md")),
		`TASK_TITLE="My Task"`,
		`echo "hello world" > hello.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, dirs.Root), "mato")

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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", "<!-- claimed-by: test-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	// Host creates the task branch
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")

	script := strings.Join([]string{
		promptPreamble(t),
		`BRANCH="task/my-task"`,
		`FILENAME="my-task.md"`,
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, dirs.Root, queue.DirInProgress, "my-task.md")),
		`TASK_TITLE="My Task"`,
		`echo "aaa" > a.txt`,
		`echo "bbb" > b.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, dirs.Root), "mato")

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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", "<!-- claimed-by: test-agent-4  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")
	testutil.WriteFile(t, filepath.Join(cloneDir, "hello.txt"), "hello world\n")
	mustGitOutput(t, cloneDir, "add", "hello.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	// Host pushes the branch to the repo
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", "task/my-task")

	// Verify the branch exists in the host repo
	mustGitOutput(t, repoRoot, "rev-parse", "--verify", "refs/heads/task/my-task")

	// Host writes branch marker
	f, err := os.OpenFile(filepath.Join(tasksDir, queue.DirInProgress, "my-task.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open task file: %v", err)
	}
	fmt.Fprintf(f, "\n<!-- branch: task/my-task -->\n")
	f.Close()

	// Host moves to ready-for-review
	if err := os.Rename(
		filepath.Join(tasksDir, queue.DirInProgress, "my-task.md"),
		filepath.Join(tasksDir, queue.DirReadyReview, "my-task.md"),
	); err != nil {
		t.Fatalf("move to ready-for-review: %v", err)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirReadyReview, "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirInProgress, "my-task.md"))

	contents := readFile(t, filepath.Join(tasksDir, queue.DirReadyReview, "my-task.md"))
	if !strings.Contains(contents, "<!-- branch: task/my-task -->") {
		t.Fatalf("ready task missing branch metadata: %s", contents)
	}
}

func TestHostBranchMarkerWrittenAfterPush(t *testing.T) {
	// Verify branch marker is written to the task file after the host pushes.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", "<!-- claimed-by: test-agent-branch  claimed-at: 2026-01-01T00:00:00Z -->\n# My Task\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	mustGitOutput(t, cloneDir, "checkout", "-b", "task/my-task")
	testutil.WriteFile(t, filepath.Join(cloneDir, "branch.txt"), "branch metadata\n")
	mustGitOutput(t, cloneDir, "add", "branch.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	// Host pushes
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", "task/my-task")

	// Before marker: no branch comment
	contents := readFile(t, filepath.Join(tasksDir, queue.DirInProgress, "my-task.md"))
	if strings.Contains(contents, "<!-- branch:") {
		t.Fatalf("branch marker should not exist before host writes it: %s", contents)
	}

	// Host writes marker
	f, err := os.OpenFile(filepath.Join(tasksDir, queue.DirInProgress, "my-task.md"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open task file: %v", err)
	}
	fmt.Fprintf(f, "\n<!-- branch: task/my-task -->\n")
	f.Close()

	contents = readFile(t, filepath.Join(tasksDir, queue.DirInProgress, "my-task.md"))
	if !strings.Contains(contents, "<!-- branch: task/my-task -->") {
		t.Fatalf("task file missing branch metadata after host write: %s", contents)
	}
}

func TestHostRetryResumesExistingRemoteBranch(t *testing.T) {
	// When a task is retried with a recorded branch, the host should resume from
	// the existing task branch tip instead of recreating the branch from target.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", "<!-- claimed-by: test-agent-stale  claimed-at: 2026-01-01T00:00:00Z -->\n<!-- branch: task/my-task -->\n# My Task\n")

	// Simulate a prior attempt that already pushed work to the task branch.
	mustGitOutput(t, repoRoot, "checkout", "-b", "task/my-task", "mato")
	testutil.WriteFile(t, filepath.Join(repoRoot, "resume.txt"), "previous branch work\n")
	mustGitOutput(t, repoRoot, "add", "resume.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "previous branch work")
	mustGitOutput(t, repoRoot, "checkout", "mato")
	testutil.WriteFile(t, filepath.Join(repoRoot, "base.txt"), "advanced target\n")
	mustGitOutput(t, repoRoot, "add", "base.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "advance mato")

	// A retry clone should resume from the existing remote task branch.
	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	result, err := git.EnsureBranch(cloneDir, "task/my-task")
	if err != nil {
		t.Fatalf("git.EnsureBranch: %v", err)
	}
	if result.Source != git.BranchSourceRemote {
		t.Fatalf("EnsureBranch source = %q, want %q", result.Source, git.BranchSourceRemote)
	}

	if got := strings.TrimSpace(mustGitOutput(t, cloneDir, "show", "HEAD:resume.txt")); got != "previous branch work" {
		t.Fatalf("resume.txt in resumed clone = %q, want %q", got, "previous branch work")
	}
	if _, err := git.Output(cloneDir, "show", "HEAD:base.txt"); err == nil {
		t.Fatal("did not expect resumed branch tip to silently restart from advanced target branch")
	}

	// Agent makes follow-up changes on the resumed branch.
	testutil.WriteFile(t, filepath.Join(cloneDir, "followup.txt"), "follow-up work\n")
	mustGitOutput(t, cloneDir, "add", "followup.txt")
	mustGitOutput(t, cloneDir, "commit", "-m", "My Task")

	// Host pushes the updated resumed branch.
	mustGitOutput(t, cloneDir, "push", "origin", "task/my-task")

	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "task/my-task:resume.txt")); got != "previous branch work" {
		t.Fatalf("resume.txt on remote branch = %q, want %q", got, "previous branch work")
	}
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "task/my-task:followup.txt")); got != "follow-up work" {
		t.Fatalf("followup.txt on remote branch = %q, want %q", got, "follow-up work")
	}
	if _, err := git.Output(repoRoot, "show", "task/my-task:base.txt"); err == nil {
		t.Fatal("did not expect advanced target-only content to appear on resumed branch without merge")
	}
}

func TestPromptOnFailure(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", strings.Join([]string{
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
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, dirs.Root, queue.DirInProgress, "my-task.md")),
		`FAIL_STEP="WORK"`,
		`FAIL_REASON="test failure"`,
		promptStateBlock(t, "ON_FAILURE"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, dirs.Root), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-5"}, script)
	if err != nil {
		t.Fatalf("runBash on failure: %v\noutput:\n%s", err, out)
	}

	// ON_FAILURE writes the failure record but does NOT move the file.
	// The host handles the move to backlog/ via recoverStuckTask.
	inProgressTask := filepath.Join(tasksDir, queue.DirInProgress, "my-task.md")
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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "my-task.md", strings.Join([]string{
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
		"TASK_PATH=" + quotedPath(filepath.Join(cloneDir, dirs.Root, queue.DirInProgress, "my-task.md")),
		`FAIL_STEP="WORK"`,
		`FAIL_REASON="test failure"`,
		promptStateBlock(t, "ON_FAILURE"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, filepath.Join(cloneDir, dirs.Root), "mato")

	out, err := runBash(t, cloneDir, []string{"MATO_AGENT_ID=test-agent-6"}, script)
	if err != nil {
		t.Fatalf("runBash on failure with many priors: %v\noutput:\n%s", err, out)
	}

	// Task stays in in-progress/, NOT moved to backlog/ — host handles that.
	inProgressTask := filepath.Join(tasksDir, queue.DirInProgress, "my-task.md")
	mustExist(t, inProgressTask)
	mustNotExist(t, filepath.Join(tasksDir, queue.DirBacklog, "my-task.md"))
	mustNotExist(t, filepath.Join(tasksDir, queue.DirFailed, "my-task.md"))

	contents := readFile(t, inProgressTask)
	if got := countFailureRecords(contents); got != 3 {
		t.Fatalf("failure record count = %d, want 3\ncontents:\n%s", got, contents)
	}
}

func TestPromptTwoAgentsParallelClaim(t *testing.T) {
	_, tasksDir := testutil.SetupRepoWithTasks(t)
	for _, name := range []string{"task-alpha.md", "task-beta.md", "task-gamma.md"} {
		writeTask(t, tasksDir, queue.DirBacklog, name, "# "+strings.TrimSuffix(name, ".md")+"\n")
	}

	// Both agents claim via Go; each gets a different task.
	claimedA, err := queue.SelectAndClaimTask(tasksDir, "agent-a", []string{"task-alpha.md", "task-beta.md", "task-gamma.md"}, 0, nil)
	if err != nil {
		t.Fatalf("claim agent-a: %v", err)
	}
	if claimedA == nil {
		t.Fatal("agent-a got no task")
	}

	claimedB, err := queue.SelectAndClaimTask(tasksDir, "agent-b", []string{"task-alpha.md", "task-beta.md", "task-gamma.md"}, 0, nil)
	if err != nil {
		t.Fatalf("claim agent-b: %v", err)
	}
	if claimedB == nil {
		t.Fatal("agent-b got no task")
	}

	if claimedA.Filename == claimedB.Filename {
		t.Fatalf("both agents claimed the same task: %s", claimedA.Filename)
	}

	inProgress := markdownFileNames(t, filepath.Join(tasksDir, queue.DirInProgress))
	if len(inProgress) != 2 {
		t.Fatalf("in-progress tasks = %v, want 2 claimed tasks", inProgress)
	}
	backlog := markdownFileNames(t, filepath.Join(tasksDir, queue.DirBacklog))
	if len(backlog) != 1 {
		t.Fatalf("backlog tasks = %v, want 1 unclaimed task", backlog)
	}
}

func TestPromptVerifyClaimDependencyContext(t *testing.T) {
	// MATO_DEPENDENCY_CONTEXT points to a JSON file written by the host with
	// details about completed prerequisite tasks. The VERIFY_CLAIM block should
	// read and echo its contents.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "dep-task.md",
		"<!-- claimed-by: dep-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Dep Task\nDo dep work.\n")

	// Write a dependency-context JSON file.
	depCtx := filepath.Join(t.TempDir(), "dependency-context.json")
	depJSON := `[{"task":"setup-db.md","title":"Setup database","commit":"abc1234","files":["db/schema.sql"]}]`
	if err := os.WriteFile(depCtx, []byte(depJSON), 0o644); err != nil {
		t.Fatalf("write dependency context: %v", err)
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	script := strings.Join([]string{
		promptPreamble(t),
		promptStateBlock(t, "VERIFY_CLAIM"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=dep-agent",
		"MATO_TASK_FILE=dep-task.md",
		"MATO_TASK_BRANCH=task/dep-task",
		"MATO_TASK_TITLE=Dep Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, "dep-task.md"),
		"MATO_DEPENDENCY_CONTEXT=" + depCtx,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify claim with dependency context: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Dependency context") {
		t.Fatalf("output should contain dependency context header, got:\n%s", out)
	}
	if !strings.Contains(out, "setup-db.md") {
		t.Fatalf("output should contain dependency task filename, got:\n%s", out)
	}
	if !strings.Contains(out, "abc1234") {
		t.Fatalf("output should contain dependency commit SHA, got:\n%s", out)
	}
}

func TestPromptVerifyClaimFileClaims(t *testing.T) {
	// MATO_FILE_CLAIMS points to a JSON file listing files claimed by other
	// in-progress tasks. VERIFY_CLAIM should read and echo the contents.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "claims-task.md",
		"<!-- claimed-by: claims-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Claims Task\nDo claims work.\n")

	// Write a file-claims JSON file.
	claimsFile := filepath.Join(t.TempDir(), "file-claims.json")
	claimsJSON := `{"internal/runner/runner.go":{"task":"other-task.md","status":"in-progress"},"docs/":{"task":"doc-task.md","status":"in-progress"}}`
	if err := os.WriteFile(claimsFile, []byte(claimsJSON), 0o644); err != nil {
		t.Fatalf("write file claims: %v", err)
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	script := strings.Join([]string{
		promptPreamble(t),
		promptStateBlock(t, "VERIFY_CLAIM"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	env := []string{
		"MATO_AGENT_ID=claims-agent",
		"MATO_TASK_FILE=claims-task.md",
		"MATO_TASK_BRANCH=task/claims-task",
		"MATO_TASK_TITLE=Claims Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, "claims-task.md"),
		"MATO_FILE_CLAIMS=" + claimsFile,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify claim with file claims: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Files and directory prefixes currently claimed") {
		t.Fatalf("output should contain file claims header, got:\n%s", out)
	}
	if !strings.Contains(out, "internal/runner/runner.go") {
		t.Fatalf("output should contain claimed file path, got:\n%s", out)
	}
	if !strings.Contains(out, "other-task.md") {
		t.Fatalf("output should contain claiming task name, got:\n%s", out)
	}
}

func TestPromptVerifyClaimPreviousFailures(t *testing.T) {
	// MATO_PREVIOUS_FAILURES is a string env var containing prior failure
	// records. VERIFY_CLAIM should echo the content.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "fail-task.md",
		"<!-- claimed-by: fail-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Fail Task\nRetry work.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	script := strings.Join([]string{
		promptPreamble(t),
		promptStateBlock(t, "VERIFY_CLAIM"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	failureLines := "<!-- failure: agent-x at 2026-03-20T10:00:00Z step=WORK error=build_failed files_changed=main.go -->"
	env := []string{
		"MATO_AGENT_ID=fail-agent",
		"MATO_TASK_FILE=fail-task.md",
		"MATO_TASK_BRANCH=task/fail-task",
		"MATO_TASK_TITLE=Fail Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, "fail-task.md"),
		"MATO_PREVIOUS_FAILURES=" + failureLines,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify claim with previous failures: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Previous failure records") {
		t.Fatalf("output should contain previous failures header, got:\n%s", out)
	}
	if !strings.Contains(out, "step=WORK") {
		t.Fatalf("output should contain failure step, got:\n%s", out)
	}
	if !strings.Contains(out, "build_failed") {
		t.Fatalf("output should contain failure error, got:\n%s", out)
	}
}

func TestPromptVerifyClaimReviewFeedback(t *testing.T) {
	// MATO_REVIEW_FEEDBACK is a string env var containing prior review
	// rejection feedback. VERIFY_CLAIM should echo the content.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirInProgress, "review-task.md",
		"<!-- claimed-by: review-agent  claimed-at: 2026-01-01T00:00:00Z -->\n# Review Task\nAddress review.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	script := strings.Join([]string{
		promptPreamble(t),
		promptStateBlock(t, "VERIFY_CLAIM"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	feedback := "Reviewer rejected: missing error handling in parseConfig; add nil check before dereferencing pointer"
	env := []string{
		"MATO_AGENT_ID=review-agent",
		"MATO_TASK_FILE=review-task.md",
		"MATO_TASK_BRANCH=task/review-task",
		"MATO_TASK_TITLE=Review Task",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, "review-task.md"),
		"MATO_REVIEW_FEEDBACK=" + feedback,
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash verify claim with review feedback: %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(out, "Previous review rejection feedback") {
		t.Fatalf("output should contain review feedback header, got:\n%s", out)
	}
	if !strings.Contains(out, "missing error handling in parseConfig") {
		t.Fatalf("output should contain review feedback details, got:\n%s", out)
	}
}

func TestPromptFullLifecycleWithMerge(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirBacklog, "add-hello.md", "# Add hello\nCreate hello.txt with hello world.\n")

	claimed, err := queue.SelectAndClaimTask(tasksDir, "test-agent-8", []string{"add-hello.md"}, 0, nil)
	if err != nil {
		t.Fatalf("SelectAndClaimTask: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected a task to be claimed")
	}

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

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
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, claimed.Filename),
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash full lifecycle: %v\noutput:\n%s", err, out)
	}

	// Host post-agent: push branch, write marker, move to ready-for-review.
	mustGitOutput(t, cloneDir, "push", "--force-with-lease", "origin", claimed.Branch)

	taskFile := filepath.Join(tasksDir, queue.DirInProgress, "add-hello.md")
	f, fErr := os.OpenFile(taskFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if fErr != nil {
		t.Fatalf("open task file: %v", fErr)
	}
	fmt.Fprintf(f, "\n<!-- branch: %s -->\n", claimed.Branch)
	f.Close()

	readyTask := filepath.Join(tasksDir, queue.DirReadyReview, "add-hello.md")
	if err := os.Rename(taskFile, readyTask); err != nil {
		t.Fatalf("move to ready-for-review: %v", err)
	}

	mustExist(t, readyTask)
	mustNotExist(t, filepath.Join(tasksDir, queue.DirBacklog, "add-hello.md"))

	// Simulate review approval: move task from ready-for-review/ to ready-to-merge/
	mergeTask := filepath.Join(tasksDir, queue.DirReadyMerge, "add-hello.md")
	if err := os.Rename(readyTask, mergeTask); err != nil {
		t.Fatalf("move to ready-to-merge: %v", err)
	}

	if got := merge.ProcessQueue(repoRoot, tasksDir, "mato"); got != 1 {
		t.Fatalf("merge.ProcessQueue() = %d, want 1", got)
	}

	mustExist(t, filepath.Join(tasksDir, queue.DirCompleted, "add-hello.md"))
	mustNotExist(t, mergeTask)
	if got := strings.TrimSpace(mustGitOutput(t, repoRoot, "show", "mato:hello.txt")); got != "hello world" {
		t.Fatalf("hello.txt on mato = %q, want %q", got, "hello world")
	}
}

// TestPromptProgressMessagesAreDistinct verifies that the VERIFY_CLAIM, WORK,
// and COMMIT state blocks each produce a progress message with a distinct
// filename so that messages are never overwritten even when all three states
// run in the same second.
func TestPromptProgressMessagesAreDistinct(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, queue.DirInProgress, "progress-test.md",
		"<!-- claimed-by: test-progress  claimed-at: 2026-01-01T00:00:00Z -->\n"+
			"# Progress Test\nTest progress messages.\n")

	cloneDir := createPromptClone(t, repoRoot, tasksDir)
	cloneTasksDir := filepath.Join(cloneDir, dirs.Root)

	// Run the messaging snippets from all three state blocks back-to-back.
	// Extract just the message-writing portions of each state block.
	script := strings.Join([]string{
		promptPreamble(t),
		`FILENAME="progress-test.md"`,
		`BRANCH="task/progress-test"`,
		`TASK_TITLE="Progress Test"`,
		`TASK_PATH="` + filepath.Join(cloneTasksDir, queue.DirInProgress, "progress-test.md") + `"`,
		promptStateBlock(t, "VERIFY_CLAIM"),
		promptStateBlock(t, "WORK"),
		// Create a file to stage so the COMMIT block's git commit succeeds.
		`echo "progress test" > progress-file.txt`,
		promptStateBlock(t, "COMMIT"),
	}, "\n\n")
	script = substitutePromptPlaceholders(script, cloneTasksDir, "mato")

	mustGitOutput(t, cloneDir, "checkout", "-b", "task/progress-test")

	env := []string{
		"MATO_AGENT_ID=test-progress",
		"MATO_TASK_FILE=progress-test.md",
		"MATO_TASK_BRANCH=task/progress-test",
		"MATO_TASK_TITLE=Progress Test",
		"MATO_TASK_PATH=" + filepath.Join(cloneTasksDir, queue.DirInProgress, "progress-test.md"),
	}
	out, err := runBash(t, cloneDir, env, script)
	if err != nil {
		t.Fatalf("runBash progress: %v\noutput:\n%s", err, out)
	}

	// Read all messages from the agent and verify we got exactly 3 distinct
	// progress messages — one for each state.
	msgs := readPromptEventMessages(t, tasksDir)
	progressBodies := make(map[string]string) // body → filename
	for _, msg := range msgs {
		if msg.Type != "progress" || msg.From != "test-progress" {
			continue
		}
		if prev, ok := progressBodies[msg.Body]; ok {
			t.Fatalf("duplicate progress body %q: %s and %s", msg.Body, prev, msg.ID)
		}
		progressBodies[msg.Body] = msg.ID
	}

	for _, expected := range []string{"VERIFY_CLAIM", "WORK", "COMMIT"} {
		key := "Step: " + expected
		if _, ok := progressBodies[key]; !ok {
			t.Fatalf("missing progress message for %s; got bodies: %v", expected, progressBodies)
		}
	}

	if len(progressBodies) != 3 {
		t.Fatalf("expected 3 distinct progress messages, got %d: %v", len(progressBodies), progressBodies)
	}
}
