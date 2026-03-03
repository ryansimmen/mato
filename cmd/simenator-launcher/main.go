package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	repoRoot, simenatorArgs, hasSeparator, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "launcher error: %v\n", err)
		os.Exit(1)
	}
	if err := run(repoRoot, simenatorArgs, hasSeparator); err != nil {
		fmt.Fprintf(os.Stderr, "launcher error: %v\n", err)
		os.Exit(1)
	}
}

func run(repoRoot string, simenatorArgs []string, hasSeparator bool) error {
	appRepo := os.Getenv("SIMENATOR_APP_REPO")
	if appRepo == "" {
		var err error
		appRepo, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("get current directory: %w", err)
		}
	}
	appRepo, err := gitOutput(appRepo, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("resolve app repo: %w", err)
	}
	appRepo = strings.TrimSpace(appRepo)

	repoRoot, err = gitOutput(repoRoot, "rev-parse", "--show-toplevel")
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

	if !hasSeparator && len(simenatorArgs) == 0 {
		simenatorArgs = append(simenatorArgs, "-task")
	}

	image := os.Getenv("SIMENATOR_DOCKER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	workdir := "/workspace"

	// Read host git identity for commits made inside the container.
	gitName, _ := gitOutput(repoRoot, "config", "user.name")
	gitEmail, _ := gitOutput(repoRoot, "config", "user.email")
	if n := strings.TrimSpace(gitName); n == "" {
		gitName, _ = gitOutput("", "config", "--global", "user.name")
	}
	if e := strings.TrimSpace(gitEmail); e == "" {
		gitEmail, _ = gitOutput("", "config", "--global", "user.email")
	}

	goroot := runtime.GOROOT()
	if goroot == "" {
		return errors.New("unable to determine host GOROOT")
	}
	goModCache, err := goEnv("GOMODCACHE")
	if err != nil {
		return err
	}
	goBuildCache, err := goEnv("GOCACHE")
	if err != nil {
		return err
	}
	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		return fmt.Errorf("find copilot CLI: %w", err)
	}
	ghPath := "/usr/bin/gh"
	if info, statErr := os.Stat(ghPath); statErr != nil || info.IsDir() {
		ghPath, err = exec.LookPath("gh")
		if err != nil {
			return fmt.Errorf("find gh CLI: %w", err)
		}
	}
	systemCertsDir := "/etc/ssl/certs"
	hasSystemCerts := false
	if info, statErr := os.Stat(systemCertsDir); statErr == nil && info.IsDir() {
		hasSystemCerts = true
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	copilotSkillsDir := filepath.Join(homeDir, ".copilot", "skills")
	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasCopilotSkills := false
	hasGhConfig := false
	if info, statErr := os.Stat(copilotSkillsDir); statErr == nil && info.IsDir() {
		hasCopilotSkills = true
	}
	if info, statErr := os.Stat(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	fmt.Printf("Launching agent from %s\n", repoRoot)
	args := []string{
		"run", "--rm", "-it",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", fmt.Sprintf("%s:%s", repoRoot, workdir),
		"-v", fmt.Sprintf("%s:/simenator:ro", appRepo),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", goroot),
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", ghPath),
		"-v", fmt.Sprintf("%s:/go/pkg/mod", goModCache),
		"-v", fmt.Sprintf("%s:/tmp/go-build", goBuildCache),
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"-e", "GOPATH=/go",
		"-e", "GOMODCACHE=/go/pkg/mod",
		"-e", "GOCACHE=/tmp/go-build",
		"-e", fmt.Sprintf("SIMENATOR_SESSION_CWD=%s", workdir),
		"-e", fmt.Sprintf("SIMENATOR_TASKS_DIR=%s/tasks", workdir),
	}
	if n := strings.TrimSpace(gitName); n != "" {
		args = append(args, "-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n)
	}
	if e := strings.TrimSpace(gitEmail); e != "" {
		args = append(args, "-e", "GIT_AUTHOR_EMAIL="+e, "-e", "GIT_COMMITTER_EMAIL="+e)
	}
	// Set HOME for the container user and mount the host .copilot dir
	// so the copilot CLI can extract its bundled package.
	containerHome := homeDir
	copilotDir := filepath.Join(homeDir, ".copilot")
	args = append(args,
		"-e", fmt.Sprintf("HOME=%s", containerHome),
		"-v", fmt.Sprintf("%s:%s/.copilot", copilotDir, containerHome),
	)
	if hasCopilotSkills {
		args = append(args, "-v", fmt.Sprintf("%s:%s/.copilot/skills:ro", copilotSkillsDir, containerHome))
	}
	if hasGhConfig {
		args = append(args, "-v", fmt.Sprintf("%s:%s/.config/gh:ro", ghConfigDir, containerHome))
	}
	if hasSystemCerts {
		args = append(args, "-v", fmt.Sprintf("%s:/etc/ssl/certs:ro", systemCertsDir))
	}
	args = append(args,
		"-w", workdir,
		image,
		"go", "-C", "/simenator", "run", "./cmd/simenator",
	)
	args = append(args, simenatorArgs...)

	cmd := exec.Command("docker", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func parseArgs(args []string) (string, []string, bool, error) {
	var repoRoot string
	hasSeparator := false
	simenatorArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			hasSeparator = true
			simenatorArgs = append(simenatorArgs, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "--repo=") {
			repoRoot = strings.TrimSpace(strings.TrimPrefix(arg, "--repo="))
			continue
		}
		if arg == "--repo" {
			if i+1 >= len(args) {
				return "", nil, false, errors.New("--repo requires a value")
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
				return "", nil, false, errors.New("--worktree-repo requires a value")
			}
			i++
			repoRoot = strings.TrimSpace(args[i])
			continue
		}
		simenatorArgs = append(simenatorArgs, arg)
	}
	if repoRoot == "" {
		return "", nil, false, errors.New("--repo is required")
	}
	return repoRoot, simenatorArgs, hasSeparator, nil
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

func goEnv(name string) (string, error) {
	out, err := exec.Command("go", "env", name).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w (%s)", name, err, strings.TrimSpace(string(out)))
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("go env %s returned empty value", name)
	}
	return value, nil
}
