package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/git"
	"mato/internal/pause"
	"mato/internal/queue"
	"mato/internal/testutil"
)

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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--once")
	if err != nil {
		t.Fatalf("mato run --once: %v\n%s", err, out)
	}

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
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	writeTask(t, tasksDir, queue.DirBacklog, "once.md", "# Once\nCreate once.txt\n")

	out, err := runMatoCommandWithEnv(t, boundedRunWorkEnv(t), "run", "--repo", repoRoot, "--once")
	if err != nil {
		t.Fatalf("mato run --once: %v\n%s", err, out)
	}

	readyPath := filepath.Join(tasksDir, queue.DirReadyReview, "once.md")
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready-for-review task missing: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "once.md")); !os.IsNotExist(err) {
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

func TestBoundedRun_UntilIdleExitsWhenPausedAndQueueIsEmpty(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if _, err := pause.Pause(tasksDir, time.Now().UTC()); err != nil {
		t.Fatalf("pause repo: %v", err)
	}

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--until-idle")
	if err != nil {
		t.Fatalf("mato run --until-idle: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[mato] paused - run 'mato resume' to continue") {
		t.Fatalf("output = %q, want paused heartbeat", out)
	}
}

func TestBoundedRun_UntilIdleDrainsReadyToMergeAndExits(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	createTaskBranch(t, repoRoot, "task/add-bounded", map[string]string{"bounded.txt": "bounded\n"}, "add bounded")
	writeTask(t, tasksDir, queue.DirReadyMerge, "add-bounded.md", strings.Join([]string{
		"<!-- branch: task/add-bounded -->",
		"# Add bounded",
		"",
	}, "\n"))

	out, err := runMatoCommandWithEnv(t, boundedRunTestEnv(t), "run", "--repo", repoRoot, "--until-idle")
	if err != nil {
		t.Fatalf("mato run --until-idle: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Merged 1 task(s) into mato") {
		t.Fatalf("output = %q, want merge confirmation", out)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirCompleted, "add-bounded.md")); err != nil {
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
