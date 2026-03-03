package main

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed task-instructions.md
var taskInstructions string

func main() {
	repoRoot, copilotArgs, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher error: %v\n", err)
		os.Exit(1)
	}
	if err := run(repoRoot, copilotArgs); err != nil {
		fmt.Fprintf(os.Stderr, "launcher error: %v\n", err)
		os.Exit(1)
	}
}

func run(repoRoot string, copilotArgs []string) error {
	repoRoot, err := gitOutput(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	tasksDir := filepath.Join(repoRoot, "tasks")
	for _, sub := range []string{"backlog", "in-progress", "completed", ".locks"} {
		if err := os.MkdirAll(filepath.Join(tasksDir, sub), 0o755); err != nil {
			return fmt.Errorf("create tasks subdirectory %s: %w", sub, err)
		}
	}

	image := os.Getenv("SIMENATOR_DOCKER_IMAGE")
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
	prompt := strings.ReplaceAll(taskInstructions, "TASKS_DIR_PLACEHOLDER", workdir+"/tasks")

	fmt.Printf("Launching agent from %s\n", repoRoot)
	args := []string{
		"run", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", fmt.Sprintf("%s:%s", repoRoot, workdir),
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git:ro", gitPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", ghPath),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", goRoot),
		"-e", "GOROOT=/usr/local/go",
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if n := strings.TrimSpace(gitName); n != "" {
		args = append(args, "-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n)
	}
	if e := strings.TrimSpace(gitEmail); e != "" {
		args = append(args, "-e", "GIT_AUTHOR_EMAIL="+e, "-e", "GIT_COMMITTER_EMAIL="+e)
	}
	// Mount host .copilot dir so the copilot CLI can find auth + packages.
	containerHome := homeDir
	copilotDir := filepath.Join(homeDir, ".copilot")
	goModCache := filepath.Join(homeDir, "go", "pkg", "mod")
	goBuildCache := filepath.Join(homeDir, ".cache", "go-build")
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
		// Backwards compat
		if strings.HasPrefix(arg, "--worktree-repo=") {
			repoRoot = strings.TrimSpace(strings.TrimPrefix(arg, "--worktree-repo="))
			continue
		}
		if arg == "--worktree-repo" {
			if i+1 >= len(args) {
				return "", nil, errors.New("--worktree-repo requires a value")
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
