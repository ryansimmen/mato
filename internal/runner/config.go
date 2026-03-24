package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/git"

	"golang.org/x/term"
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

// dockerImageInspectFn checks whether a Docker image is available locally.
// It is a variable so tests can inject a stub without calling Docker.
var dockerImageInspectFn = func(image string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run()
}

// dockerPullFn pulls a Docker image. It is a variable so tests can inject
// a stub without calling Docker.
var dockerPullFn = func(image string) error {
	cmd := exec.Command("docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ensureDockerImage checks that the configured Docker image is available
// locally. If not, it prints a message and attempts to pull it with
// stdout/stderr forwarded to the user. Returns an error if the pull fails.
// The check is idempotent: once an image is pulled, subsequent calls
// return immediately.
func ensureDockerImage(image string) error {
	if err := dockerImageInspectFn(image); err == nil {
		return nil
	}
	fmt.Printf("Docker image %s not found locally. Pulling...\n", image)
	if err := dockerPullFn(image); err != nil {
		return fmt.Errorf("failed to pull Docker image %s: verify the image name and your network connection: %w", image, err)
	}
	return nil
}

// envConfig holds immutable environment configuration populated once during
// initialization. It contains tool paths, Docker image settings, git identity,
// feature flags, and filesystem paths that do not change between task runs.
type envConfig struct {
	image, workdir                          string
	copilotPath, gitPath, gitUploadPackPath string
	gitReceivePackPath, ghPath, goRoot      string
	copilotConfigDir                        string
	gitName, gitEmail, homeDir, ghConfigDir string
	hasGhConfig                             bool
	gitTemplatesDir                         string
	hasGitTemplates                         bool
	systemCertsDir                          string
	hasSystemCerts                          bool
	copilotArgs                             []string
	repoRoot, tasksDir                      string
	targetBranch                            string
	isTTY                                   bool
}

// runContext holds per-task execution state that varies between task runs.
// Each call to runOnce or runReview constructs its own runContext so that
// mutable fields like cloneDir are never shared across concurrent calls.
type runContext struct {
	cloneDir string
	prompt   string
	agentID  string
	timeout  time.Duration
}

// isTerminal reports whether f is connected to a terminal (not just any
// character device). Uses golang.org/x/term which performs the TCGETS
// ioctl safely without requiring unsafe.Pointer.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func buildDockerArgs(env envConfig, run runContext, extraEnvs []string, extraVolumes []string) []string {
	containerHome := env.homeDir
	goModCache := filepath.Join(env.homeDir, "go", "pkg", "mod")
	goBuildCache := filepath.Join(env.homeDir, ".cache", "go-build")

	runFlags := "-i"
	if env.isTTY {
		runFlags = "-it"
	}
	args := []string{
		"run", "--rm", "--init", runFlags,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", fmt.Sprintf("%s:%s", run.cloneDir, env.workdir),
		"-v", fmt.Sprintf("%s:%s/.tasks", env.tasksDir, env.workdir),
		"-v", fmt.Sprintf("%s:%s", env.repoRoot, env.repoRoot),
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", env.copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git:ro", env.gitPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-upload-pack:ro", env.gitUploadPackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-receive-pack:ro", env.gitReceivePackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", env.ghPath),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", env.goRoot),
		"-e", "GOROOT=/usr/local/go",
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	args = append(args,
		"-e", "MATO_AGENT_ID="+run.agentID,
		"-e", "MATO_MESSAGING_ENABLED=1",
		"-e", fmt.Sprintf("MATO_MESSAGES_DIR=%s/.tasks/messages", env.workdir),
	)
	for _, e := range extraEnvs {
		args = append(args, "-e", e)
	}
	args = append(args,
		"-e", "GIT_CONFIG_COUNT=1",
		"-e", "GIT_CONFIG_KEY_0=safe.directory",
		"-e", "GIT_CONFIG_VALUE_0=*",
	)
	if n := strings.TrimSpace(env.gitName); n != "" {
		args = append(args, "-e", "GIT_AUTHOR_NAME="+n, "-e", "GIT_COMMITTER_NAME="+n)
	}
	if e := strings.TrimSpace(env.gitEmail); e != "" {
		args = append(args, "-e", "GIT_AUTHOR_EMAIL="+e, "-e", "GIT_COMMITTER_EMAIL="+e)
	}
	args = append(args,
		"-e", fmt.Sprintf("HOME=%s", containerHome),
		"-v", fmt.Sprintf("%s:%s/.copilot", env.copilotConfigDir, containerHome),
		"-e", fmt.Sprintf("GOPATH=%s/go", containerHome),
		"-e", fmt.Sprintf("GOMODCACHE=%s/go/pkg/mod", containerHome),
		"-e", fmt.Sprintf("GOCACHE=%s/.cache/go-build", containerHome),
		"-v", fmt.Sprintf("%s:%s/go/pkg/mod", goModCache, containerHome),
		"-v", fmt.Sprintf("%s:%s/.cache/go-build", goBuildCache, containerHome),
	)
	if env.hasGhConfig {
		args = append(args, "-v", fmt.Sprintf("%s:%s/.config/gh:ro", env.ghConfigDir, containerHome))
	}
	if env.hasGitTemplates {
		args = append(args, "-v", fmt.Sprintf("%s:%s:ro", env.gitTemplatesDir, env.gitTemplatesDir))
	}
	if env.hasSystemCerts {
		args = append(args, "-v", fmt.Sprintf("%s:/etc/ssl/certs:ro", env.systemCertsDir))
	}
	for _, vol := range extraVolumes {
		args = append(args, "-v", vol)
	}
	args = append(args,
		"-w", env.workdir,
		env.image,
		"copilot", "-p", run.prompt, "--autopilot", "--allow-all",
	)
	if !hasModelArg(env.copilotArgs) {
		args = append(args, "--model", defaultModel())
	}
	args = append(args, env.copilotArgs...)
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
