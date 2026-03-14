package main

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
)

//go:embed task-instructions.md
var taskInstructions string

func run(repoRoot, branch, tasksDirOverride string, copilotArgs []string) error {
	repoRoot, err := gitOutput(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	// Create the target branch if it doesn't exist yet.
	if err := ensureBranch(repoRoot, branch); err != nil {
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
	if err := initMessaging(tasksDir); err != nil {
		return fmt.Errorf("init messaging: %w", err)
	}

	// Generate agent identity early so the lock can be registered
	// before recovering orphaned tasks.
	agentID, err := generateAgentID()
	if err != nil {
		return fmt.Errorf("generate agent ID: %w", err)
	}

	// Register this agent so concurrent instances know we're alive.
	cleanupLock, err := registerAgent(tasksDir, agentID)
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}
	defer cleanupLock()

	// Ensure .tasks is gitignored so it never pollutes commits or status.
	if err := ensureGitignored(repoRoot, "/.tasks/"); err != nil {
		return err
	}

	image := os.Getenv("MATO_DOCKER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	workdir := "/workspace"

	// Read host git identity for commits made inside the container.
	gitName, _ := gitOutput(repoRoot, "config", "user.name")
	gitEmail, _ := gitOutput(repoRoot, "config", "user.email")
	if strings.TrimSpace(gitName) == "" {
		gitName, _ = gitOutput("", "config", "--global", "user.name")
	}
	if strings.TrimSpace(gitEmail) == "" {
		gitEmail, _ = gitOutput("", "config", "--global", "user.email")
	}

	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		return fmt.Errorf("find copilot CLI: %w", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("find git CLI: %w", err)
	}
	// git-upload-pack is spawned by git when fetching from a local-path remote.
	// Find it via LookPath, falling back to git's exec-path directory.
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
	// git-receive-pack is spawned by git when pushing to a local-path remote.
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

	// Build the prompt from embedded task instructions.
	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/.tasks")
	prompt = strings.ReplaceAll(prompt, "TARGET_BRANCH_PLACEHOLDER", branch)
	prompt = strings.ReplaceAll(prompt, "MESSAGES_DIR_PLACEHOLDER", workdir+"/.tasks/messages")

	// Listen for interrupt signals to stop the loop gracefully.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		// Recover any tasks left in in-progress by a previous crashed run.
		// Tasks claimed by a still-active agent are left alone.
		recoverOrphanedTasks(tasksDir)

		// Remove lock files from agents that are no longer running.
		cleanStaleLocks(tasksDir)
		cleanStalePresence(tasksDir)
		cleanOldMessages(tasksDir, 24*time.Hour)

		reconcileReadyQueue(tasksDir)
		if err := writeQueueManifest(tasksDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write queue manifest: %v\n", err)
		}
		removeOverlappingTasks(tasksDir)
		if err := writeQueueManifest(tasksDir); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write queue manifest after overlap cleanup: %v\n", err)
		}

		hasBacklogTasks := hasAvailableTasks(tasksDir)
		if hasBacklogTasks {
			if err := runOnce(repoRoot, tasksDir, agentID, copilotArgs, image, workdir, prompt,
				copilotPath, gitPath, gitUploadPackPath, gitReceivePackPath, ghPath, goRoot,
				gitName, gitEmail, homeDir, ghConfigDir, hasGhConfig,
				systemCertsDir, hasSystemCerts); err != nil {
				fmt.Fprintf(os.Stderr, "warning: agent run failed: %v\n", err)
			}
		}

		// Process any tasks ready to merge.
		if cleanup, ok := acquireMergeLock(tasksDir); ok {
			merged := processMergeQueue(repoRoot, tasksDir, branch)
			cleanup()
			if merged > 0 {
				fmt.Printf("Merged %d task(s) into %s\n", merged, branch)
			}
		}

		if !hasBacklogTasks && !hasReadyToMergeTasks(tasksDir) {
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
	// Create a temporary local clone so multiple instances can run in
	// parallel without conflicting on branch checkouts or index state.
	cloneDir, err := createClone(repoRoot)
	if err != nil {
		return err
	}
	defer removeClone(cloneDir)

	// Allow the LLM agent to push to the checked-out branch in the origin repo.
	// With .tasks/ gitignored the working tree stays clean, so updateInstead
	// can safely update it on push.
	gitOutput(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead")

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
		// Mount repoRoot so the clone's "origin" remote is reachable
		// for git fetch/push inside the container.
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
	// Trust all directories inside the container — it's ephemeral and the
	// repoRoot mount may appear owned by a different UID with userns-remap.
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
	// Mount host .copilot dir so the copilot CLI can find auth + packages.
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
