package runner

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"mato/internal/git"
	"mato/internal/merge"
	"mato/internal/messaging"
	"mato/internal/queue"
)

//go:embed task-instructions.md
var taskInstructions string

func Run(repoRoot, branch, tasksDirOverride string, copilotArgs []string) error {
	repoRoot, err := git.Output(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	if err := git.EnsureBranch(repoRoot, branch); err != nil {
		return err
	}

	tasksDir := tasksDirOverride
	if tasksDir == "" {
		tasksDir = filepath.Join(repoRoot, ".tasks")
	}
	for _, sub := range []string{"waiting", "backlog", "in-progress", "ready-to-merge", "completed", "failed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create .tasks subdirectory %s: %w", sub, err)
		}
	}
	if err := messaging.Init(tasksDir); err != nil {
		return fmt.Errorf("init messaging: %w", err)
	}

	agentID, err := queue.GenerateAgentID()
	if err != nil {
		return fmt.Errorf("generate agent ID: %w", err)
	}

	cleanupLock, err := queue.RegisterAgent(tasksDir, agentID)
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	defer cleanupLock()

	gitName, _ := git.Output(repoRoot, "config", "user.name")
	gitEmail, _ := git.Output(repoRoot, "config", "user.email")
	if strings.TrimSpace(gitName) == "" {
		gitName, _ = git.Output("", "config", "--global", "user.name")
	}
	if strings.TrimSpace(gitEmail) == "" {
		gitEmail, _ = git.Output("", "config", "--global", "user.email")
	}
	if n := strings.TrimSpace(gitName); n != "" {
		git.Output(repoRoot, "config", "user.name", n)
	}
	if e := strings.TrimSpace(gitEmail); e != "" {
		git.Output(repoRoot, "config", "user.email", e)
	}

	if err := git.EnsureGitignored(repoRoot, "/.tasks/"); err != nil {
		return err
	}

	image := os.Getenv("MATO_DOCKER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	workdir := "/workspace"

	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		return fmt.Errorf("find copilot CLI: %w", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("find git CLI: %w", err)
	}
	gitUploadPackPath, err := exec.LookPath("git-upload-pack")
	if err != nil {
		out, execErr := exec.Command("git", "--exec-path").Output()
		if execErr != nil {
			return fmt.Errorf("find git-upload-pack: %w", err)
		}
		candidate := filepath.Join(strings.TrimSpace(string(out)), "git-upload-pack")
		if _, statErr := os.Stat(candidate); statErr != nil {
			return fmt.Errorf("find git-upload-pack: %w", err)
		}
		gitUploadPackPath = candidate
	}
	gitReceivePackPath, err := exec.LookPath("git-receive-pack")
	if err != nil {
		out, execErr := exec.Command("git", "--exec-path").Output()
		if execErr != nil {
			return fmt.Errorf("find git-receive-pack: %w", err)
		}
		candidate := filepath.Join(strings.TrimSpace(string(out)), "git-receive-pack")
		if _, statErr := os.Stat(candidate); statErr != nil {
			return fmt.Errorf("find git-receive-pack: %w", err)
		}
		gitReceivePackPath = candidate
	}
	ghPath := "/usr/bin/gh"
	if info, statErr := os.Stat(ghPath); statErr != nil || info.IsDir() {
		ghPath, err = exec.LookPath("gh")
		if err != nil {
			return fmt.Errorf("find gh CLI: %w", err)
		}
	}
	goRoot := runtime.GOROOT()

	systemCertsDir := "/etc/ssl/certs"
	hasSystemCerts := false
	if info, statErr := os.Stat(systemCertsDir); statErr == nil && info.IsDir() {
		hasSystemCerts = true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasGhConfig := false
	if info, statErr := os.Stat(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/.tasks")
	prompt = strings.ReplaceAll(prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	prompt = strings.ReplaceAll(prompt, "MESSAGES_DIR_PLACEHOLDER", workdir+"/.tasks/messages")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		queue.RecoverOrphanedTasks(tasksDir)
		queue.CleanStaleLocks(tasksDir)
		messaging.CleanStalePresence(tasksDir)
		messaging.CleanOldMessages(tasksDir, 24*time.Hour)

		queue.ReconcileReadyQueue(tasksDir)
		deferred := queue.DeferredOverlappingTasks(tasksDir)
		if err := queue.WriteQueueManifest(tasksDir, deferred); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write queue manifest: %v\n", err)
		}

		hasBacklogTasks := queue.HasAvailableTasks(tasksDir)
		if hasBacklogTasks {
			if err := runOnce(repoRoot, tasksDir, agentID, copilotArgs, image, workdir, prompt,
				copilotPath, gitPath, gitUploadPackPath, gitReceivePackPath, ghPath, goRoot,
				gitName, gitEmail, homeDir, ghConfigDir, hasGhConfig,
				systemCertsDir, hasSystemCerts); err != nil {
				fmt.Fprintf(os.Stderr, "warning: agent run failed: %v\n", err)
			}
		}

		if cleanup, ok := merge.AcquireLock(tasksDir); ok {
			merged := merge.ProcessQueue(repoRoot, tasksDir, branch)
			cleanup()
			if merged > 0 {
				fmt.Printf("Merged %d task(s) into %s\n", merged, branch)
			}
		}

		if !hasBacklogTasks && !merge.HasReadyTasks(tasksDir) {
			fmt.Println("No tasks found in backlog or ready-to-merge. Waiting...")
		}

		select {
		case <-sigCh:
			fmt.Println("\nInterrupted. Exiting.")
			return nil
		case <-time.After(10 * time.Second):
		}
	}
}

func runOnce(repoRoot, tasksDir, agentID string, copilotArgs []string,
	image, workdir, prompt string,
	copilotPath, gitPath, gitUploadPackPath, gitReceivePackPath, ghPath, goRoot string,
	gitName, gitEmail, homeDir, ghConfigDir string, hasGhConfig bool,
	systemCertsDir string, hasSystemCerts bool,
) error {
	cloneDir, err := git.CreateClone(repoRoot)
	if err != nil {
		return err
	}
	defer git.RemoveClone(cloneDir)

	git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead")

	containerHome := homeDir
	copilotDir := filepath.Join(homeDir, ".copilot")
	goModCache := filepath.Join(homeDir, "go", "pkg", "mod")
	goBuildCache := filepath.Join(homeDir, ".cache", "go-build")

	fmt.Printf("Launching agent from %s (clone: %s)\n", repoRoot, cloneDir)
	args := []string{
		"run", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", fmt.Sprintf("%s:%s", cloneDir, workdir),
		"-v", fmt.Sprintf("%s:%s/.tasks", tasksDir, workdir),
		"-v", fmt.Sprintf("%s:%s", repoRoot, repoRoot),
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git:ro", gitPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-upload-pack:ro", gitUploadPackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-receive-pack:ro", gitReceivePackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", ghPath),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", goRoot),
		"-e", "GOROOT=/usr/local/go",
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	args = append(args,
		"-e", "MATO_AGENT_ID="+agentID,
		"-e", "MATO_MESSAGING_ENABLED=1",
		"-e", fmt.Sprintf("MATO_MESSAGES_DIR=%s/.tasks/messages", workdir),
	)
	args = append(args,
		"-e", "GIT_CONFIG_COUNT=1",
		"-e", "GIT_CONFIG_KEY_0=safe.directory",
		"-e", "GIT_CONFIG_VALUE_0=*",
	)
	if n := strings.TrimSpace(gitName); n != "" {
		args = append(args, "-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n)
	}
	if e := strings.TrimSpace(gitEmail); e != "" {
		args = append(args, "-e", "GIT_AUTHOR_EMAIL="+e, "-e", "GIT_COMMITTER_EMAIL="+e)
	}
	args = append(args,
		"-e", fmt.Sprintf("HOME=%s", containerHome),
		"-v", fmt.Sprintf("%s:%s/.copilot", copilotDir, containerHome),
		"-e", fmt.Sprintf("GOPATH=%s/go", containerHome),
		"-e", fmt.Sprintf("GOMODCACHE=%s/go/pkg/mod", containerHome),
		"-e", fmt.Sprintf("GOCACHE=%s/.cache/go-build", containerHome),
		"-v", fmt.Sprintf("%s:%s/go/pkg/mod", goModCache, containerHome),
		"-v", fmt.Sprintf("%s:%s/.cache/go-build", goBuildCache, containerHome),
	)
	if hasGhConfig {
		args = append(args, "-v", fmt.Sprintf("%s:%s/.config/gh:ro", ghConfigDir, containerHome))
	}
	if hasSystemCerts {
		args = append(args, "-v", fmt.Sprintf("%s:/etc/ssl/certs:ro", systemCertsDir))
	}
	args = append(args,
		"-w", workdir,
		image,
		"copilot", "-p", prompt, "--autopilot", "--allow-all",
	)
	if !hasModelArg(copilotArgs) {
		args = append(args, "--model", "claude-opus-4.6")
	}
	args = append(args, copilotArgs...)

	cmd := exec.Command("docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func hasModelArg(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if arg == "--model" {
			return true
		}
		if strings.HasPrefix(arg, "--model=") {
			return true
		}
	}
	return false
}
