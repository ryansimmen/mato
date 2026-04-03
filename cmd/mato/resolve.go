package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"mato/internal/config"
	"mato/internal/git"
	"mato/internal/runner"
	"mato/internal/ui"
)

type runFlags struct {
	TaskModel             string
	ReviewModel           string
	TaskReasoningEffort   string
	ReviewReasoningEffort string
}

var validReasoningEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
}

func resolveRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return wd, nil
}

func resolveEnvBranch() (string, bool, error) {
	raw, ok := os.LookupEnv("MATO_BRANCH")
	if !ok || raw == "" {
		return "", false, nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, fmt.Errorf("MATO_BRANCH must not be whitespace-only")
	}
	return trimmed, true, nil
}

func resolveConfigBranch(cfg config.Config, flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if envBranch, ok, err := resolveEnvBranch(); err != nil {
		return "", err
	} else if ok {
		return envBranch, nil
	}
	if cfg.Branch != nil {
		return *cfg.Branch, nil
	}
	return "mato", nil
}

func resolveStringOption(flagVal, envKey string, configVal *string) string {
	if v := strings.TrimSpace(flagVal); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	if configVal != nil {
		return *configVal
	}
	return ""
}

func resolveBoolOption(envKey string, configVal *bool, defaultVal bool) (bool, error) {
	raw, ok := os.LookupEnv(envKey)
	if ok && strings.TrimSpace(raw) != "" {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "1", "true", "yes", "on":
			return true, nil
		case "0", "false", "no", "off":
			return false, nil
		default:
			return defaultVal, fmt.Errorf("parse %s %q: must be true or false", envKey, raw)
		}
	}
	if configVal != nil {
		return *configVal, nil
	}
	return defaultVal, nil
}

// resolveDurationOption resolves a duration from an environment variable or
// config value. It returns zero if neither source is set.
func resolveDurationOption(envKey string, configVal *string, name string) (time.Duration, error) {
	if v := os.Getenv(envKey); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("parse %s %q: %w", envKey, v, err)
		}
		if d <= 0 {
			return 0, fmt.Errorf("%s must be positive, got %v", envKey, d)
		}
		return d, nil
	}
	if configVal != nil {
		d, err := time.ParseDuration(*configVal)
		if err != nil {
			return 0, fmt.Errorf("invalid %s %q in .mato.yaml: %w", name, *configVal, err)
		}
		if d <= 0 {
			return 0, fmt.Errorf("%s in .mato.yaml must be positive, got %v", name, d)
		}
		return d, nil
	}
	return 0, nil
}

func validateReasoningEffort(value, flagName string) error {
	if !validReasoningEfforts[value] {
		return fmt.Errorf("invalid %s %q: must be one of low, medium, high, xhigh", flagName, value)
	}
	return nil
}

func resolveRunOptions(flags runFlags, cfg config.Config) (runner.RunOptions, error) {
	var opts runner.RunOptions

	if v := strings.TrimSpace(os.Getenv("MATO_DOCKER_IMAGE")); v != "" {
		opts.DockerImage = v
	} else if cfg.DockerImage != nil {
		opts.DockerImage = *cfg.DockerImage
	}

	opts.TaskModel = resolveStringOption(flags.TaskModel, "MATO_TASK_MODEL", cfg.TaskModel)
	if opts.TaskModel == "" {
		opts.TaskModel = runner.DefaultTaskModel
	}
	opts.ReviewModel = resolveStringOption(flags.ReviewModel, "MATO_REVIEW_MODEL", cfg.ReviewModel)
	if opts.ReviewModel == "" {
		opts.ReviewModel = runner.DefaultReviewModel
	}
	resumeEnabled, err := resolveBoolOption("MATO_REVIEW_SESSION_RESUME_ENABLED", cfg.ReviewSessionResume, true)
	if err != nil {
		return opts, err
	}
	opts.ReviewSessionResumeEnabled = resumeEnabled
	opts.TaskReasoningEffort = resolveStringOption(flags.TaskReasoningEffort, "MATO_TASK_REASONING_EFFORT", cfg.TaskReasoningEffort)
	if opts.TaskReasoningEffort == "" {
		opts.TaskReasoningEffort = runner.DefaultReasoningEffort
	}
	opts.ReviewReasoningEffort = resolveStringOption(flags.ReviewReasoningEffort, "MATO_REVIEW_REASONING_EFFORT", cfg.ReviewReasoningEffort)
	if opts.ReviewReasoningEffort == "" {
		opts.ReviewReasoningEffort = runner.DefaultReasoningEffort
	}
	if err := validateReasoningEffort(opts.TaskReasoningEffort, "task-reasoning-effort"); err != nil {
		return opts, err
	}
	if err := validateReasoningEffort(opts.ReviewReasoningEffort, "review-reasoning-effort"); err != nil {
		return opts, err
	}

	if d, err := resolveDurationOption("MATO_AGENT_TIMEOUT", cfg.AgentTimeout, "agent_timeout"); err != nil {
		return opts, err
	} else if d > 0 {
		opts.AgentTimeout = d
	}

	if d, err := resolveDurationOption("MATO_RETRY_COOLDOWN", cfg.RetryCooldown, "retry_cooldown"); err != nil {
		return opts, err
	} else if d > 0 {
		opts.RetryCooldown = d
	}

	return opts, nil
}

// gitResolveRepoRoot is the function used to resolve the repository root.
// It defaults to git.ResolveRepoRoot and can be replaced in tests.
var gitResolveRepoRoot = git.ResolveRepoRoot

func resolveRepoRoot(dir string) (string, error) {
	root, err := gitResolveRepoRoot(dir)
	if err != nil {
		return "", fmt.Errorf("resolve repo root for %q: %w", dir, err)
	}
	return root, nil
}

// gitCheckRefFormat is the function used to validate branch names. It
// defaults to running "git check-ref-format --branch" and can be replaced
// in tests.
var gitCheckRefFormat = func(name string) error {
	out, err := exec.Command("git", "check-ref-format", "--branch", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid branch name %q: git check-ref-format rejected it (%s)", name, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateBranch checks that the branch name is a legal git refname by
// delegating to "git check-ref-format --branch".
func validateBranch(branch string) error {
	return gitCheckRefFormat(branch)
}

// gitRevParseGitDir is the function used to verify a directory is a git
// repository. It defaults to running "git rev-parse --git-dir" and can
// be replaced in tests.
var gitRevParseGitDir = func(dir string) error {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--git-dir")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("repo path %q is not a git repository: %s", dir, strings.TrimSpace(string(out)))
	}
	return nil
}

// validateRepoPath checks that dir exists, is a directory, and is a git
// repository by running a lightweight git command.
func validateRepoPath(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("repo path %q does not exist: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repo path %q is not a directory", dir)
	}
	return gitRevParseGitDir(dir)
}

func requireTasksDir(tasksDir string) error {
	return ui.RequireTasksDir(tasksDir)
}
