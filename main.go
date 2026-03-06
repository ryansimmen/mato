package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed task-instructions.md
var taskInstructions string

func main() {
	repoRoot, copilotArgs, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
		os.Exit(1)
	}
	if err := run(repoRoot, copilotArgs); err != nil {
		fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
		os.Exit(1)
	}
}

func run(repoRoot string, copilotArgs []string) error {
	repoRoot, err := gitOutput(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	tasksDir := filepath.Join(repoRoot, ".tasks")
	for _, sub := range []string{"backlog", "in-progress", "completed", "failed"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create .tasks subdirectory %s: %w", sub, err)
		}
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

		if !hasAvailableTasks(tasksDir) {
			fmt.Println("No tasks found in backlog. Waiting...")
			select {
			case <-sigCh:
				fmt.Println("\nInterrupted. Exiting.")
				return nil
			case <-time.After(10 * time.Second):
				continue
			}
		}

		if err := runOnce(repoRoot, tasksDir, agentID, copilotArgs, image, workdir, prompt,
			copilotPath, gitPath, gitUploadPackPath, gitReceivePackPath, ghPath, goRoot,
			gitName, gitEmail, homeDir, ghConfigDir, hasGhConfig,
			systemCertsDir, hasSystemCerts); err != nil {
			fmt.Fprintf(os.Stderr, "warning: agent run failed: %v\n", err)
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
	args = append(args, "-e", "MATO_AGENT_ID="+agentID)
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

func parseArgs(args []string) (string, []string, error) {
	var repoRoot string
	copilotArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			copilotArgs = append(copilotArgs, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "--repo=") {
			repoRoot = strings.TrimSpace(strings.TrimPrefix(arg, "--repo="))
			continue
		}
		if arg == "--repo" {
			if i+1 >= len(args) {
				return "", nil, errors.New("--repo requires a value")
			}
			i++
			repoRoot = strings.TrimSpace(args[i])
			continue
		}
		copilotArgs = append(copilotArgs, arg)
	}
	if repoRoot == "" {
		return "", nil, errors.New("--repo is required")
	}
	return repoRoot, copilotArgs, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if dir != "" {
		cmdArgs = append(cmdArgs, "-C", dir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func generateAgentID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var nonAlphanumDash = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
var multiDash = regexp.MustCompile(`-{2,}`)

func sanitizeBranchName(name string) string {
	// Strip the .md extension if present.
	name = strings.TrimSuffix(name, ".md")
	// Replace non-alphanumeric chars (except dash) with dashes.
	name = nonAlphanumDash.ReplaceAllString(name, "-")
	// Collapse consecutive dashes.
	name = multiDash.ReplaceAllString(name, "-")
	// Trim leading/trailing dashes.
	name = strings.Trim(name, "-")
	if name == "" {
		name = "unnamed"
	}
	return name
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

func createClone(repoRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "mato-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	if _, err := gitOutput("", "clone", "--quiet", repoRoot, dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("clone repo: %w", err)
	}
	return dir, nil
}

func removeClone(dir string) {
	os.RemoveAll(dir)
}

// ensureGitignored appends pattern to the repo's .gitignore if not already present.
func ensureGitignored(repoRoot, pattern string) error {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
	}
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	// Add a newline before the pattern if the file doesn't end with one.
	if len(data) > 0 && data[len(data)-1] != '\n' {
		fmt.Fprintln(f)
	}
	fmt.Fprintln(f, pattern)
	f.Close()
	if _, err := gitOutput(repoRoot, "add", ".gitignore"); err != nil {
		return fmt.Errorf("git add .gitignore: %w", err)
	}
	if _, err := gitOutput(repoRoot, "commit", "-m", "chore: add "+pattern+" to .gitignore"); err != nil {
		return fmt.Errorf("git commit .gitignore: %w", err)
	}
	return nil
}

// hasAvailableTasks reports whether there is at least one .md task file
// in backlog/. After orphan recovery, any task still in in-progress/
// belongs to an active agent, so only backlog/ matters for new agents.
func hasAvailableTasks(tasksDir string) bool {
	entries, err := os.ReadDir(filepath.Join(tasksDir, "backlog"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			return true
		}
	}
	return false
}

var claimedByRe = regexp.MustCompile(`<!-- claimed-by:\s*(\S+)`)

// registerAgent writes a PID lock file so concurrent mato instances
// know this agent is still alive. Returns a cleanup function.
func registerAgent(tasksDir, agentID string) (func(), error) {
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return nil, fmt.Errorf("create locks dir: %w", err)
	}
	lockFile := filepath.Join(locksDir, agentID+".pid")
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, fmt.Errorf("write agent lock: %w", err)
	}
	return func() { os.Remove(lockFile) }, nil
}

// isAgentActive checks whether the agent that wrote a lock file is still running.
func isAgentActive(tasksDir, agentID string) bool {
	if agentID == "" {
		return false
	}
	lockFile := filepath.Join(tasksDir, ".locks", agentID+".pid")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// parseClaimedBy extracts the agent ID from a task file's claimed-by metadata.
func parseClaimedBy(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := claimedByRe.FindStringSubmatch(string(data))
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// cleanStaleLocks removes lock files for agents that are no longer running.
func cleanStaleLocks(tasksDir string) {
	locksDir := filepath.Join(tasksDir, ".locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pid") {
			continue
		}
		agentID := strings.TrimSuffix(e.Name(), ".pid")
		if !isAgentActive(tasksDir, agentID) {
			os.Remove(filepath.Join(locksDir, e.Name()))
		}
	}
}

// recoverOrphanedTasks moves any files in in-progress/ back to backlog/.
// This handles the case where a previous run was killed (e.g. Ctrl+C)
// before the agent could clean up. A failure record is appended so the
// retry-count logic can eventually move it to failed/.
// Tasks claimed by a still-active agent are skipped.
func recoverOrphanedTasks(tasksDir string) {
	inProgress := filepath.Join(tasksDir, "in-progress")
	entries, err := os.ReadDir(inProgress)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		src := filepath.Join(inProgress, e.Name())

		// If the task is claimed by an agent that's still running, skip it.
		if agent := parseClaimedBy(src); agent != "" && isAgentActive(tasksDir, agent) {
			fmt.Printf("Skipping in-progress task %s (agent %s still active)\n", e.Name(), agent)
			continue
		}

		dst := filepath.Join(tasksDir, "backlog", e.Name())

		// Append a failure record so the retry count increments.
		f, err := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "\n<!-- failure: mato-recovery at %s — agent was interrupted -->\n",
				time.Now().UTC().Format(time.RFC3339))
			f.Close()
		}

		if err := os.Rename(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not recover orphaned task %s: %v\n", e.Name(), err)
			continue
		}
		fmt.Printf("Recovered orphaned task %s back to backlog\n", e.Name())
	}
}
