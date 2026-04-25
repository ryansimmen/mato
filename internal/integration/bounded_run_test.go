package integration_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/pause"
	"github.com/ryansimmen/mato/internal/runtimedata"
	"github.com/ryansimmen/mato/internal/taskfile"
	"github.com/ryansimmen/mato/internal/testutil"
)

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()

	got := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("command returned unexpected error type %T: %v", err, err)
		}
		got = exitErr.ExitCode()
	}
	if got != want {
		t.Fatalf("exit code = %d, want %d (err = %v)", got, want, err)
	}
}

func makeTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		mode := os.FileMode(0o644)
		if info.IsDir() {
			mode = 0o755
		}
		_ = os.Chmod(path, mode)
		return nil
	})
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	testutil.WriteFile(t, path, content)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func boundedRunTestEnv(t *testing.T) []string {
	t.Helper()

	toolsDir := t.TempDir()
	homeDir := t.TempDir()
	t.Cleanup(func() { makeTreeWritable(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".copilot"), 0o755); err != nil {
		t.Fatalf("mkdir .copilot: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homeDir, ".cache", "copilot"), 0o755); err != nil {
		t.Fatalf("mkdir copilot cache: %v", err)
	}

	writeExecutable(t, filepath.Join(toolsDir, "copilot"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(toolsDir, "gh"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(toolsDir, "docker"), strings.Join([]string{
		"#!/bin/sh",
		"case \"$1\" in",
		"  info)",
		"    exit 0",
		"    ;;",
		"  image)",
		"    if [ \"$2\" = \"inspect\" ]; then",
		"      exit 0",
		"    fi",
		"    exit 0",
		"    ;;",
		"  pull)",
		"    exit 0",
		"    ;;",
		"  run)",
		"    exit 0",
		"    ;;",
		"esac",
		"exit 0",
	}, "\n")+"\n")

	return []string{
		"HOME=" + homeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(homeDir, ".config"),
		"PATH=" + toolsDir + ":" + os.Getenv("PATH"),
	}
}

func boundedRunWorkEnv(t *testing.T) []string {
	t.Helper()

	toolsDir := t.TempDir()
	homeDir := t.TempDir()
	t.Cleanup(func() { makeTreeWritable(homeDir) })
	if err := os.MkdirAll(filepath.Join(homeDir, ".copilot"), 0o755); err != nil {
		t.Fatalf("mkdir .copilot: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homeDir, ".cache", "copilot"), 0o755); err != nil {
		t.Fatalf("mkdir copilot cache: %v", err)
	}

	writeExecutable(t, filepath.Join(toolsDir, "copilot"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(toolsDir, "gh"), "#!/bin/sh\nexit 0\n")
	writeExecutable(t, filepath.Join(toolsDir, "docker"), strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		"cmd=${1:-}",
		"if [ \"$cmd\" = info ] || [ \"$cmd\" = pull ]; then",
		"  exit 0",
		"fi",
		"if [ \"$cmd\" = image ] && [ \"${2:-}\" = inspect ]; then",
		"  exit 0",
		"fi",
		"if [ \"$cmd\" != run ]; then",
		"  exit 0",
		"fi",
		"if [ -n \"${MATO_DOCKER_ARGS_LOG:-}\" ]; then",
		"  printf '%s\n' \"$@\" > \"$MATO_DOCKER_ARGS_LOG\"",
		"fi",
		"clone=",
		"workdir=",
		"author_name=Test",
		"author_email=test@test.com",
		"shift",
		"while [ $# -gt 0 ]; do",
		"  case \"$1\" in",
		"    -v)",
		"      mount=$2",
		"      case \"$mount\" in",
		"        *:/workspace)",
		"          clone=${mount%:/workspace}",
		"          ;;",
		"      esac",
		"      shift 2",
		"      ;;",
		"    -w)",
		"      workdir=$2",
		"      shift 2",
		"      ;;",
		"    -e)",
		"      kv=$2",
		"      case \"$kv\" in",
		"        GIT_AUTHOR_NAME=*) author_name=${kv#GIT_AUTHOR_NAME=} ;;",
		"        GIT_AUTHOR_EMAIL=*) author_email=${kv#GIT_AUTHOR_EMAIL=} ;;",
		"        GIT_COMMITTER_NAME=*) author_name=${kv#GIT_COMMITTER_NAME=} ;;",
		"        GIT_COMMITTER_EMAIL=*) author_email=${kv#GIT_COMMITTER_EMAIL=} ;;",
		"      esac",
		"      shift 2",
		"      ;;",
		"    --*)",
		"      shift",
		"      ;;",
		"    *)",
		"      shift",
		"      ;;",
		"  esac",
		"done",
		"if [ -z \"$clone\" ]; then",
		"  exit 1",
		"fi",
		"git -C \"$clone\" config user.name \"$author_name\"",
		"git -C \"$clone\" config user.email \"$author_email\"",
		"printf 'bounded once\n' > \"$clone/once.txt\"",
		"git -C \"$clone\" add once.txt",
		"git -C \"$clone\" commit -m 'bounded once' >/dev/null",
		"exit 0",
	}, "\n")+"\n")

	return []string{
		"HOME=" + homeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(homeDir, ".config"),
		"PATH=" + toolsDir + ":" + os.Getenv("PATH"),
	}
}

func TestBoundedRun_OnceExitsOnEmptyQueue(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	_, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	queuePath := filepath.Join(tasksDir, ".queue")
	data, readErr := os.ReadFile(queuePath)
	if readErr != nil {
		t.Fatalf("read .queue: %v", readErr)
	}
	if string(data) != "" {
		t.Fatalf(".queue = %q, want empty string", string(data))
	}
}

func TestBoundedRun_OnceClaimsAndLeavesTaskReadyForReview(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, dirs.Backlog, "once.md", "# Once\nCreate once.txt\n")

	out, err := runMatoCommandWithEnv(t, boundedRunWorkEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, "once.md")
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready-for-review task missing: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, "once.md")); !os.IsNotExist(err) {
		t.Fatalf("backlog task should be gone, stat err = %v\n%s", err, out)
	}
	taskData := readFile(t, readyPath)
	if !strings.Contains(taskData, "<!-- branch: task/once -->") {
		t.Fatalf("task missing branch marker:\n%s", taskData)
	}
	if !strings.Contains(out, "Pushed task/once and moved once.md to ready-for-review/") {
		t.Fatalf("output = %q, want ready-for-review confirmation", out)
	}
	show, err := git.Output(repoRoot, "show", "task/once:once.txt")
	if err != nil {
		t.Fatalf("git show task/once:once.txt: %v", err)
	}
	if strings.TrimSpace(show) != "bounded once" {
		t.Fatalf("once.txt = %q, want %q", strings.TrimSpace(show), "bounded once")
	}
}

func TestBoundedRun_OnceRepairsMissingRecordedBranchAndStartsFreshSession(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	taskFile := "resume-repair.md"
	branch := "task/resume-repair"
	writeTask(t, tasksDir, dirs.Backlog, taskFile, strings.Join([]string{
		"<!-- branch: " + branch + " -->",
		"# Resume Repair",
		"Create once.txt on the repaired branch.",
		"",
	}, "\n"))

	oldSessionID := "stale-session-id"
	if err := runtimedata.UpdateSession(tasksDir, runtimedata.KindWork, taskFile, func(session *runtimedata.Session) {
		session.CopilotSessionID = oldSessionID
		session.TaskBranch = branch
	}); err != nil {
		t.Fatalf("seed work session: %v", err)
	}

	argsLog := filepath.Join(t.TempDir(), "docker-args.txt")
	env := append(boundedRunWorkEnv(t), "MATO_DOCKER_ARGS_LOG="+argsLog)
	out, err := runMatoCommandWithEnv(t, env, "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	readyPath := filepath.Join(tasksDir, dirs.ReadyReview, taskFile)
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready-for-review task missing: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, taskFile)); !os.IsNotExist(err) {
		t.Fatalf("backlog task should be gone after repair run, stat err = %v\n%s", err, out)
	}

	taskData := readFile(t, readyPath)
	if !strings.Contains(taskData, "<!-- branch: "+branch+" -->") {
		t.Fatalf("task missing original branch marker after repair:\n%s", taskData)
	}
	if !taskfile.ContainsBranchRepairMarker([]byte(taskData)) {
		t.Fatalf("task missing branch-repair marker after repair:\n%s", taskData)
	}
	if strings.Count(taskData, "<!-- branch-repair:") != 1 {
		t.Fatalf("expected exactly one branch-repair marker, got:\n%s", taskData)
	}
	if taskfile.CountFailureMarkers([]byte(taskData)) != 0 {
		t.Fatalf("branch repair should not consume retry budget, got %d failure markers", taskfile.CountFailureMarkers([]byte(taskData)))
	}

	if !strings.Contains(out, "starting a fresh work session") {
		t.Fatalf("output = %q, want repair warning", out)
	}
	if !strings.Contains(out, "Pushed "+branch+" and moved "+taskFile+" to ready-for-review/") {
		t.Fatalf("output = %q, want ready-for-review confirmation", out)
	}

	argsData, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read docker args log: %v", err)
	}
	args := string(argsData)
	if strings.Contains(args, "--resume=") {
		t.Fatalf("docker args should not reuse a stale session during branch repair:\n%s", args)
	}

	session, err := runtimedata.LoadSession(tasksDir, runtimedata.KindWork, taskFile)
	if err != nil {
		t.Fatalf("load work session: %v", err)
	}
	if session == nil {
		t.Fatal("expected work session after repair run")
	}
	if session.CopilotSessionID == oldSessionID {
		t.Fatal("expected repair run to rotate stale work session ID")
	}
	if session.TaskBranch != branch {
		t.Fatalf("session branch = %q, want %q", session.TaskBranch, branch)
	}

	show, err := git.Output(repoRoot, "show", branch+":once.txt")
	if err != nil {
		t.Fatalf("git show %s:once.txt: %v", branch, err)
	}
	if strings.TrimSpace(show) != "bounded once" {
		t.Fatalf("once.txt = %q, want %q", strings.TrimSpace(show), "bounded once")
	}
}

func TestBoundedRun_OnceUsesRestrictedContainerMounts(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, dirs.Backlog, "once.md", "# Once\nCreate once.txt\n")

	argsLog := filepath.Join(t.TempDir(), "docker-args.txt")
	env := append(boundedRunWorkEnv(t), "MATO_DOCKER_ARGS_LOG="+argsLog)
	_, err := runMatoCommandWithEnv(t, env, "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read docker args log: %v", err)
	}
	log := string(data)

	for _, want := range []string{
		repoRoot + ":/mato-host-repo:ro",
		filepath.Join(tasksDir, dirs.InProgress) + ":/workspace/.mato/in-progress:ro",
		filepath.Join(tasksDir, dirs.ReadyReview) + ":/workspace/.mato/ready-for-review:ro",
		filepath.Join(tasksDir, "messages") + ":/workspace/.mato/messages",
		"GIT_CONFIG_VALUE_0=/workspace",
		"--allow-all-tools",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("docker args missing %q:\n%s", want, log)
		}
	}
	for _, unwanted := range []string{
		tasksDir + ":/workspace/.mato",
		repoRoot + ":" + repoRoot,
		"GIT_CONFIG_VALUE_0=*",
	} {
		if strings.Contains(log, unwanted) {
			t.Fatalf("docker args unexpectedly contained %q:\n%s", unwanted, log)
		}
	}
	if strings.Contains("\n"+log+"\n", "\n--allow-all\n") {
		t.Fatalf("docker args unexpectedly contained %q:\n%s", "--allow-all", log)
	}
}

func TestBoundedRun_UntilIdleExitsWhenPausedAndQueueIsEmpty(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if _, err := pause.Pause(tasksDir, time.Now().UTC()); err != nil {
		t.Fatalf("pause repo: %v", err)
	}

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--until-idle")
	assertExitCode(t, err, 0)
	if !strings.Contains(out, "[mato] paused - run 'mato resume' to continue") {
		t.Fatalf("output = %q, want paused heartbeat", out)
	}
}

func TestBoundedRun_OnceExitsNonZeroOnParseFailure(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, dirs.Failed, "stale-broken.md", "---\npriority: nope\n---\n# Broken failed\n")
	writeTask(t, tasksDir, dirs.Backlog, "broken.md", "---\npriority: nope\n---\n# Broken\n")

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 1)

	if !strings.Contains(out, "bounded run encountered 1 poll cycle error") {
		t.Fatalf("output = %q, want bounded-run error summary", out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "broken.md")); err != nil {
		t.Fatalf("failed task missing after parse failure: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, "broken.md")); !os.IsNotExist(err) {
		t.Fatalf("parse-failed task should leave backlog, stat err = %v\n%s", err, out)
	}
}

func TestBoundedRun_OnceIgnoresMalformedFailedTask(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, dirs.Failed, "stale-broken.md", "---\npriority: nope\n---\n# Broken failed\n")

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	if strings.Contains(out, "bounded run encountered") {
		t.Fatalf("output = %q, want malformed failed/ task to be ignored", out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "stale-broken.md")); err != nil {
		t.Fatalf("malformed failed task should remain in failed/: %v\n%s", err, out)
	}
}

func TestBoundedRun_OnceExitsNonZeroOnMalformedReviewTask(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, dirs.ReadyReview, "broken-review.md", "---\npriority: [\n# Broken review\n")

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 1)

	if !strings.Contains(out, "bounded run encountered 1 poll cycle error") {
		t.Fatalf("output = %q, want bounded-run error summary", out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Failed, "broken-review.md")); err != nil {
		t.Fatalf("malformed review task should be quarantined to failed/: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, "broken-review.md")); !os.IsNotExist(err) {
		t.Fatalf("malformed review task should leave ready-for-review, stat err = %v\n%s", err, out)
	}
}

func TestBoundedRun_OnceExitsNonZeroOnMalformedReadyMergeTask(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, dirs.ReadyMerge, "broken-merge.md", "<!-- branch: task/broken-merge -->\n---\npriority: nope\n---\n# Broken merge\n")

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 1)

	if !strings.Contains(out, "bounded run encountered 1 poll cycle error") {
		t.Fatalf("output = %q, want bounded-run error summary", out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, "broken-merge.md")); err != nil {
		t.Fatalf("malformed ready-to-merge task should be requeued to backlog: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyMerge, "broken-merge.md")); !os.IsNotExist(err) {
		t.Fatalf("malformed ready-to-merge task should leave ready-to-merge, stat err = %v\n%s", err, out)
	}
}

func TestBoundedRun_OnceProcessesReviewOnlyQueueAndExitsSuccess(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	mustGitOutput(t, repoRoot, "checkout", "-b", "task/review-only", "mato")
	testutil.WriteFile(t, filepath.Join(repoRoot, "review.txt"), "reviewed\n")
	mustGitOutput(t, repoRoot, "add", "review.txt")
	mustGitOutput(t, repoRoot, "commit", "-m", "review only")
	mustGitOutput(t, repoRoot, "checkout", "mato")
	writeTask(t, tasksDir, dirs.ReadyReview, "review-only.md", strings.Join([]string{
		"<!-- claimed-by: task-agent  claimed-at: 2026-01-01T00:00:00Z -->",
		"<!-- branch: task/review-only -->",
		"# Review only",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(tasksDir, "messages", "verdict-review-only.md.json"), `{"verdict":"approve"}`)

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Completed, "review-only.md")); err != nil {
		t.Fatalf("completed review-only task missing: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, "review-only.md")); !os.IsNotExist(err) {
		t.Fatalf("review-only task should leave ready-for-review, stat err = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyMerge, "review-only.md")); !os.IsNotExist(err) {
		t.Fatalf("review-only task should not remain in ready-to-merge, stat err = %v\n%s", err, out)
	}
	contents, err := git.Output(repoRoot, "show", "mato:review.txt")
	if err != nil {
		t.Fatalf("git show mato:review.txt: %v", err)
	}
	if strings.TrimSpace(contents) != "reviewed" {
		t.Fatalf("review.txt = %q, want %q", strings.TrimSpace(contents), "reviewed")
	}
}

func TestBoundedRun_UntilIdleDrainsReadyToMergeAndExits(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	createTaskBranch(t, repoRoot, "task/add-bounded", map[string]string{"bounded.txt": "bounded\n"}, "add bounded")
	writeTask(t, tasksDir, dirs.ReadyMerge, "add-bounded.md", strings.Join([]string{
		"<!-- branch: task/add-bounded -->",
		"# Add bounded",
		"",
	}, "\n"))

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--until-idle")
	assertExitCode(t, err, 0)
	if !strings.Contains(out, "Merged 1 task(s) into mato") {
		t.Fatalf("output = %q, want merge confirmation", out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Completed, "add-bounded.md")); err != nil {
		t.Fatalf("completed task missing: %v", err)
	}
	contents, err := git.Output(repoRoot, "show", "mato:bounded.txt")
	if err != nil {
		t.Fatalf("git show mato:bounded.txt: %v", err)
	}
	if strings.TrimSpace(contents) != "bounded" {
		t.Fatalf("bounded.txt = %q, want %q", strings.TrimSpace(contents), "bounded")
	}
}

func TestBoundedRun_OnceLeavesDeferredBacklogTaskUnclaimed(t *testing.T) {
	t.Parallel()

	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	writeTask(t, tasksDir, dirs.InProgress, "active.md", strings.Join([]string{
		"<!-- claimed-by: overlap-agent  claimed-at: 2026-01-01T00:00:00Z -->",
		"---",
		"priority: 1",
		"affects:",
		"  - internal/runner/task.go",
		"---",
		"# Active",
		"",
	}, "\n"))
	writeTask(t, tasksDir, dirs.Backlog, "blocked.md", strings.Join([]string{
		"---",
		"priority: 10",
		"affects:",
		"  - internal/runner/*.go",
		"---",
		"# Blocked",
		"",
	}, "\n"))
	testutil.WriteFile(t, filepath.Join(tasksDir, dirs.Locks, "overlap-agent.pid"), strconv.Itoa(os.Getpid()))

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	assertExitCode(t, err, 0)

	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Backlog, "blocked.md")); err != nil {
		t.Fatalf("blocked task should remain in backlog: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.ReadyReview, "blocked.md")); !os.IsNotExist(err) {
		t.Fatalf("blocked task should not move to ready-for-review, stat err = %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, dirs.Completed, "blocked.md")); !os.IsNotExist(err) {
		t.Fatalf("blocked task should not complete, stat err = %v\n%s", err, out)
	}

	queuePath := filepath.Join(tasksDir, ".queue")
	data, readErr := os.ReadFile(queuePath)
	if readErr != nil {
		t.Fatalf("read .queue: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf(".queue = %q, want empty queue when only deferred backlog exists", string(data))
	}
}
