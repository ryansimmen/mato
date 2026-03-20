package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"mato/internal/git"
)

// parseAgentTimeout parses a duration string for the agent timeout.
// Returns defaultAgentTimeout if envVal is empty.
func parseAgentTimeout(envVal string) (time.Duration, error) {
	if envVal == "" {
		return defaultAgentTimeout, nil
	}
	parsed, err := time.ParseDuration(envVal)
	if err != nil {
		return 0, fmt.Errorf("parse MATO_AGENT_TIMEOUT %q: %w", envVal, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("MATO_AGENT_TIMEOUT must be positive, got %v", parsed)
	}
	return parsed, nil
}

// checkDocker verifies that Docker is installed and the daemon is running
// by executing "docker info". This runs before any queue setup so that a
// missing or stopped Docker installation fails fast with a clear message
// instead of producing an opaque exec error deep in the polling loop.
// Uses a 10-second timeout to prevent hanging if the daemon is stuck.
func checkDocker() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("docker is required but not available: timed out after 10s waiting for docker daemon to respond")
	}
	if err != nil {
		// Provide a clear, actionable message that identifies Docker as the problem.
		return fmt.Errorf("docker is required but not available: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

type dockerConfig struct {
	image, workdir, prompt                  string
	copilotPath, gitPath, gitUploadPackPath string
	gitReceivePackPath, ghPath, goRoot      string
	gitName, gitEmail, homeDir, ghConfigDir string
	hasGhConfig                             bool
	gitTemplatesDir                         string
	hasGitTemplates                         bool
	systemCertsDir                          string
	hasSystemCerts                          bool
	agentID                                 string
	copilotArgs                             []string
	repoRoot, cloneDir, tasksDir            string
	targetBranch                            string
	timeout                                 time.Duration
	isTTY                                   bool
}

// isTerminal reports whether f is connected to a terminal (not just any
// character device). Uses the TCGETS ioctl which succeeds only on real
// terminal file descriptors.
func isTerminal(f *os.File) bool {
	// syscall.Termios is the Linux termios struct; TCGETS retrieves it and
	// fails with ENOTTY on non-terminal fds (including /dev/null).
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func buildDockerArgs(cfg dockerConfig, extraEnvs []string, extraVolumes []string) []string {
	containerHome := cfg.homeDir
	copilotDir := filepath.Join(cfg.homeDir, ".copilot")
	goModCache := filepath.Join(cfg.homeDir, "go", "pkg", "mod")
	goBuildCache := filepath.Join(cfg.homeDir, ".cache", "go-build")

	runFlags := "-i"
	if cfg.isTTY {
		runFlags = "-it"
	}
	args := []string{
		"run", "--rm", "--init", runFlags,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", fmt.Sprintf("%s:%s", cfg.cloneDir, cfg.workdir),
		"-v", fmt.Sprintf("%s:%s/.tasks", cfg.tasksDir, cfg.workdir),
		"-v", fmt.Sprintf("%s:%s", cfg.repoRoot, cfg.repoRoot),
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", cfg.copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git:ro", cfg.gitPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-upload-pack:ro", cfg.gitUploadPackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-receive-pack:ro", cfg.gitReceivePackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", cfg.ghPath),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", cfg.goRoot),
		"-e", "GOROOT=/usr/local/go",
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	args = append(args,
		"-e", "MATO_AGENT_ID="+cfg.agentID,
		"-e", "MATO_MESSAGING_ENABLED=1",
		"-e", fmt.Sprintf("MATO_MESSAGES_DIR=%s/.tasks/messages", cfg.workdir),
	)
	for _, env := range extraEnvs {
		args = append(args, "-e", env)
	}
	args = append(args,
		"-e", "GIT_CONFIG_COUNT=1",
		"-e", "GIT_CONFIG_KEY_0=safe.directory",
		"-e", "GIT_CONFIG_VALUE_0=*",
	)
	if n := strings.TrimSpace(cfg.gitName); n != "" {
		args = append(args, "-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n)
	}
	if e := strings.TrimSpace(cfg.gitEmail); e != "" {
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
	if cfg.hasGhConfig {
		args = append(args, "-v", fmt.Sprintf("%s:%s/.config/gh:ro", cfg.ghConfigDir, containerHome))
	}
	if cfg.hasGitTemplates {
		args = append(args, "-v", fmt.Sprintf("%s:%s:ro", cfg.gitTemplatesDir, cfg.gitTemplatesDir))
	}
	if cfg.hasSystemCerts {
		args = append(args, "-v", fmt.Sprintf("%s:/etc/ssl/certs:ro", cfg.systemCertsDir))
	}
	for _, vol := range extraVolumes {
		args = append(args, "-v", vol)
	}
	args = append(args,
		"-w", cfg.workdir,
		cfg.image,
		"copilot", "-p", cfg.prompt, "--autopilot", "--allow-all",
	)
	if !hasModelArg(cfg.copilotArgs) {
		args = append(args, "--model", defaultModel())
	}
	args = append(args, cfg.copilotArgs...)
	return args
}

// validateTasksDir resolves tasksDir to an absolute path and verifies
// that its parent directory exists. Returns the resolved absolute path.
func validateTasksDir(tasksDir string) (string, error) {
	abs, err := filepath.Abs(tasksDir)
	if err != nil {
		return "", fmt.Errorf("resolve tasks directory to absolute path: %w", err)
	}
	parent := filepath.Dir(abs)
	info, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("tasks directory parent %s does not exist: %w", parent, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("tasks directory parent %s is not a directory", parent)
	}
	return abs, nil
}

// defaultModel returns the Copilot model to use when --model is not
// explicitly passed. It checks MATO_DEFAULT_MODEL first, then falls
// back to the hardcoded default.
func defaultModel() string {
	if m := os.Getenv("MATO_DEFAULT_MODEL"); m != "" {
		return m
	}
	return "claude-opus-4.6"
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

// configureReceiveDeny sets receive.denyCurrentBranch=updateInstead on the
// host repo so that Docker agent clones can push back into the checked-out
// target branch. Returns an error if the git config command fails.
func configureReceiveDeny(repoRoot string) error {
	_, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead")
	return err
}
