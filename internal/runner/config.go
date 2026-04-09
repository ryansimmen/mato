package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/git"
	"mato/internal/ui"

	"golang.org/x/term"
)

// statPathFn wraps os.Stat for test injection.
//
// NOTE: Package-level test seam — prevents t.Parallel(). Struct-based
// dependency injection would be needed for true parallel test safety.
var statPathFn = os.Stat

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
//
// NOTE: Package-level test seam — prevents t.Parallel(). See statPathFn.
var dockerImageInspectFn = func(image string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run()
}

// dockerPullTimeout is the maximum time allowed for a docker pull operation.
// Image pulls can be large and slow, so this is generous but prevents
// indefinite hangs from a stuck daemon or stalled network.
var dockerPullTimeout = 10 * time.Minute

// dockerPullFn pulls a Docker image. It accepts a context for cancellation
// and timeout support. It is a variable so tests can inject a stub without
// calling Docker.
//
// NOTE: Package-level test seam — prevents t.Parallel(). See statPathFn.
var dockerPullFn = func(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ensureDockerImage checks that the configured Docker image is available
// locally. If not, it prints a message and attempts to pull it with
// stdout/stderr forwarded to the user. Returns an error if the pull fails
// or the context is cancelled. The check is idempotent: once an image is
// pulled, subsequent calls return immediately.
func ensureDockerImage(ctx context.Context, image string) error {
	if err := dockerImageInspectFn(image); err == nil {
		return nil
	}
	fmt.Printf("Docker image %s not found locally. Pulling...\n", image)
	pullCtx, cancel := context.WithTimeout(ctx, dockerPullTimeout)
	defer cancel()
	if err := dockerPullFn(pullCtx, image); err != nil {
		if pullCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("docker pull %s timed out after %s: %w", image, dockerPullTimeout, err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("docker pull %s cancelled: %w", image, err)
		}
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
	gitReceivePackPath, ghPath, goplsPath   string
	goRoot                                  string
	copilotConfigDir, copilotCacheDir       string
	gitName, gitEmail, homeDir, ghConfigDir string
	hasGhConfig                             bool
	gitTemplatesDir                         string
	hasGitTemplates                         bool
	systemCertsDir                          string
	hasSystemCerts                          bool
	warnMissingGopls                        bool
	repoRoot, tasksDir                      string
	targetBranch, reviewModel               string
	reviewReasoningEffort                   string
	reviewSessionResumeEnabled              bool
	verbose                                 bool
	isTTY                                   bool
}

// runContext holds per-task execution state that varies between task runs.
// Each call to runOnce or runReview constructs its own runContext so that
// mutable fields like cloneDir are never shared across concurrent calls.
type runContext struct {
	cloneDir        string
	prompt          string
	agentID         string
	model           string
	reasoningEffort string
	resumeSessionID string
	timeout         time.Duration
}

// isTerminal reports whether f is connected to a terminal (not just any
// character device). Uses golang.org/x/term which performs the TCGETS
// ioctl safely without requiring unsafe.Pointer.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

const containerOriginRepoDir = "/mato-host-repo"

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
		"-v", fmt.Sprintf("%s:/usr/local/bin/copilot:ro", env.copilotPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git:ro", env.gitPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-upload-pack:ro", env.gitUploadPackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/git-receive-pack:ro", env.gitReceivePackPath),
		"-v", fmt.Sprintf("%s:/usr/local/bin/gh:ro", env.ghPath),
		"-v", fmt.Sprintf("%s:/usr/local/go:ro", env.goRoot),
		"-e", "GOROOT=/usr/local/go",
		"-e", "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"-e", "GIT_PAGER=cat",
		"-e", "PAGER=cat",
	}
	args = appendDockerBindMount(args, run.cloneDir, env.workdir, false)
	args = appendDockerBindMount(args, env.repoRoot, containerOriginRepoDir, true)
	if tasksDir := strings.TrimSpace(env.tasksDir); tasksDir != "" {
		args = appendDockerBindMount(args, filepath.Join(tasksDir, dirs.InProgress), env.workdir+"/"+dirs.Root+"/"+dirs.InProgress, true)
		args = appendDockerBindMount(args, filepath.Join(tasksDir, dirs.ReadyReview), env.workdir+"/"+dirs.Root+"/"+dirs.ReadyReview, true)
		args = appendDockerBindMount(args, filepath.Join(tasksDir, "messages"), env.workdir+"/"+dirs.Root+"/messages", false)
	}
	if goplsPath := strings.TrimSpace(env.goplsPath); goplsPath != "" {
		args = append(args, "-v", fmt.Sprintf("%s:/usr/local/bin/gopls:ro", goplsPath))
	} else if env.warnMissingGopls {
		ui.Warnf("warning: gopls not found on the host PATH; Go LSP features will be unavailable in Docker agent containers\n")
	}
	args = append(args,
		"-e", "MATO_AGENT_ID="+run.agentID,
		"-e", "MATO_MESSAGING_ENABLED=1",
		"-e", fmt.Sprintf("MATO_MESSAGES_DIR=%s/%s/messages", env.workdir, dirs.Root),
	)
	for _, e := range extraEnvs {
		args = append(args, "-e", e)
	}
	args = append(args,
		"-e", "GIT_CONFIG_COUNT=1",
		"-e", "GIT_CONFIG_KEY_0=safe.directory",
		"-e", "GIT_CONFIG_VALUE_0="+env.workdir,
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
		"-v", fmt.Sprintf("%s:%s/.cache/copilot", env.copilotCacheDir, containerHome),
		"-e", fmt.Sprintf("GOPATH=%s/go", containerHome),
		"-e", fmt.Sprintf("GOMODCACHE=%s/go/pkg/mod", containerHome),
		"-e", fmt.Sprintf("GOCACHE=%s/.cache/go-build", containerHome),
	)
	args = appendCacheMount(args, goModCache, fmt.Sprintf("%s/go/pkg/mod", containerHome), "GOMODCACHE")
	args = appendCacheMount(args, goBuildCache, fmt.Sprintf("%s/.cache/go-build", containerHome), "GOCACHE")
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
		"copilot",
	)
	if sessionID := strings.TrimSpace(run.resumeSessionID); sessionID != "" {
		args = append(args, "--resume="+sessionID)
	}
	args = append(args,
		"-p", run.prompt, "--autopilot", "--allow-all-tools",
		"--model", run.model,
		"--reasoning-effort", run.reasoningEffort,
	)
	return args
}

func appendDockerBindMount(args []string, hostPath, containerPath string, readOnly bool) []string {
	if strings.TrimSpace(hostPath) == "" || strings.TrimSpace(containerPath) == "" {
		return args
	}
	mount := fmt.Sprintf("%s:%s", hostPath, containerPath)
	if readOnly {
		mount += ":ro"
	}
	return append(args, "-v", mount)
}

func rewriteCloneOrigin(cloneDir, newOrigin string) (func() error, error) {
	if strings.TrimSpace(newOrigin) == "" {
		return func() error { return nil }, nil
	}
	originalOrigin, err := git.Output(cloneDir, "remote", "get-url", "origin")
	if err != nil {
		return nil, fmt.Errorf("read clone origin: %w", err)
	}
	originalOrigin = strings.TrimSpace(originalOrigin)
	if originalOrigin == newOrigin {
		return func() error { return nil }, nil
	}
	if _, err := git.Output(cloneDir, "remote", "set-url", "origin", newOrigin); err != nil {
		return nil, fmt.Errorf("set clone origin to %s: %w", newOrigin, err)
	}
	return func() error {
		if _, err := git.Output(cloneDir, "remote", "set-url", "origin", originalOrigin); err != nil {
			return fmt.Errorf("restore clone origin to %s: %w", originalOrigin, err)
		}
		return nil
	}, nil
}

func prepareCloneOriginForContainer(cloneDir string) (func() error, error) {
	return rewriteCloneOrigin(cloneDir, containerOriginRepoDir)
}

func appendCacheMount(args []string, hostPath, containerPath, label string) []string {
	if _, err := statPathFn(hostPath); err != nil {
		if os.IsNotExist(err) {
			ui.Warnf("warning: skipping %s cache mount; host path %s does not exist\n", label, hostPath)
			return args
		}
		ui.Warnf("warning: skipping %s cache mount; stat %s: %v\n", label, hostPath, err)
		return args
	}
	return append(args, "-v", fmt.Sprintf("%s:%s", hostPath, containerPath))
}

// configureReceiveDeny sets receive.denyCurrentBranch=updateInstead on the
// host repo so that Docker agent clones can push back into the checked-out
// target branch. Returns an error if the git config command fails.
func configureReceiveDeny(repoRoot string) error {
	_, err := git.Output(repoRoot, "config", "receive.denyCurrentBranch", "updateInstead")
	if err != nil {
		return fmt.Errorf("configure receive.denyCurrentBranch: %w", err)
	}
	return nil
}
