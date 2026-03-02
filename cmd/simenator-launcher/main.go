package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "launcher error: %v\n", err)
		os.Exit(1)
	}
}

func run(simenatorArgs []string) error {
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

	repoRoot := os.Getenv("SIMENATOR_WORKTREE_REPO")
	if repoRoot == "" {
		repoRoot = "/home/ryansimmen/staging-labs"
	}
	repoRoot, err = gitOutput(repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	worktreesRoot := repoRoot + ".worktrees"
	if err := os.MkdirAll(worktreesRoot, 0o755); err != nil {
		return fmt.Errorf("create worktrees root: %w", err)
	}

	counterPath := filepath.Join(worktreesRoot, ".agent-counter")
	counterLockPath := filepath.Join(worktreesRoot, ".agent-counter.lock")
	locksDir := filepath.Join(worktreesRoot, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		return fmt.Errorf("create locks dir: %w", err)
	}

	counterLockFile, err := os.OpenFile(counterLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open counter lock: %w", err)
	}
	defer counterLockFile.Close()

	if err := lockFile(counterLockFile); err != nil {
		return fmt.Errorf("lock counter: %w", err)
	}

	agentID, err := readCounter(counterPath)
	if err != nil {
		_ = unlockFile(counterLockFile)
		return err
	}
	worktrees, err := listWorktreePaths(repoRoot)
	if err != nil {
		_ = unlockFile(counterLockFile)
		return err
	}
	agentID, err = nextAvailableAgentID(agentID, worktreesRoot, worktrees)
	if err != nil {
		_ = unlockFile(counterLockFile)
		return err
	}
	if err := os.WriteFile(counterPath, []byte(fmt.Sprintf("%d\n", agentID+1)), 0o644); err != nil {
		_ = unlockFile(counterLockFile)
		return fmt.Errorf("write counter: %w", err)
	}
	if err := unlockFile(counterLockFile); err != nil {
		return fmt.Errorf("unlock counter: %w", err)
	}

	instanceLockPath := filepath.Join(locksDir, fmt.Sprintf("agent%d.lock", agentID))
	instanceLockFile, err := os.OpenFile(instanceLockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open instance lock: %w", err)
	}
	defer instanceLockFile.Close()
	if err := lockFile(instanceLockFile); err != nil {
		return fmt.Errorf("lock instance: %w", err)
	}
	defer unlockFile(instanceLockFile)

	agentDir := filepath.Join(worktreesRoot, fmt.Sprintf("agent%d", agentID))
	agentDir, err = filepath.Abs(agentDir)
	if err != nil {
		return fmt.Errorf("resolve agent path: %w", err)
	}

	if err := ensureWorktree(repoRoot, agentDir); err != nil {
		return err
	}

	image := os.Getenv("SIMENATOR_DOCKER_IMAGE")
	if image == "" {
		image = "ubuntu:24.04"
	}
	workdir := os.Getenv("SIMENATOR_CONTAINER_WORKDIR")
	if workdir == "" {
		workdir = "/workspace"
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
	copilotConfigDir := filepath.Join(homeDir, ".copilot")
	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasCopilotConfig := false
	hasGhConfig := false
	if info, statErr := os.Stat(copilotConfigDir); statErr == nil && info.IsDir() {
		hasCopilotConfig = true
	}
	if info, statErr := os.Stat(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	fmt.Printf("Launching agent%d from %s\n", agentID, agentDir)
	args := []string{
		"run", "--rm", "-it",
		"--name", fmt.Sprintf("simenator-agent%d", agentID),
		"-v", fmt.Sprintf("%s:%s", agentDir, workdir),
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
	}
	if hasCopilotConfig {
		args = append(args, "-v", fmt.Sprintf("%s:/root/.copilot", copilotConfigDir))
	}
	if hasGhConfig {
		args = append(args, "-v", fmt.Sprintf("%s:/root/.config/gh", ghConfigDir))
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

func ensureWorktree(repoRoot, agentDir string) error {
	worktrees, err := listWorktreePaths(repoRoot)
	if err != nil {
		return err
	}
	if _, ok := worktrees[agentDir]; ok {
		return nil
	}

	info, statErr := os.Stat(agentDir)
	if statErr == nil && info.IsDir() {
		return fmt.Errorf("agent directory exists but is not a worktree for this repo: %s", agentDir)
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("check agent directory: %w", statErr)
	}

	cmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "--detach", agentDir, "HEAD")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func readCounter(counterPath string) (int, error) {
	content, err := os.ReadFile(counterPath)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read counter: %w", err)
	}

	raw := strings.TrimSpace(string(content))
	if raw == "" {
		return 1, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1, nil
	}
	return n, nil
}

func nextAvailableAgentID(start int, worktreesRoot string, repoWorktrees map[string]struct{}) (int, error) {
	id := start
	for {
		agentDir := filepath.Join(worktreesRoot, fmt.Sprintf("agent%d", id))
		agentDir, err := filepath.Abs(agentDir)
		if err != nil {
			return 0, fmt.Errorf("resolve agent directory: %w", err)
		}
		if _, ok := repoWorktrees[agentDir]; ok {
			return id, nil
		}
		info, err := os.Stat(agentDir)
		if errors.Is(err, os.ErrNotExist) {
			return id, nil
		}
		if err != nil {
			return 0, fmt.Errorf("check agent directory: %w", err)
		}
		if !info.IsDir() {
			id++
			continue
		}
		id++
	}
}

func listWorktreePaths(repoRoot string) (map[string]struct{}, error) {
	listOut, err := gitOutput(repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	worktrees := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(listOut))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path := strings.TrimPrefix(line, "worktree ")
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve worktree path: %w", err)
		}
		worktrees[path] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read worktree list: %w", err)
	}
	return worktrees, nil
}

func gitOutput(repoRoot string, args ...string) (string, error) {
	cmdArgs := make([]string, 0, len(args)+2)
	if repoRoot != "" {
		cmdArgs = append(cmdArgs, "-C", repoRoot)
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

func lockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
