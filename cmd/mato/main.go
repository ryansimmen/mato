package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/config"
	"mato/internal/dirs"
	"mato/internal/doctor"
	"mato/internal/git"
	"mato/internal/graph"
	"mato/internal/history"
	"mato/internal/inspect"
	"mato/internal/pause"
	"mato/internal/queue"
	"mato/internal/runner"
	"mato/internal/setup"
	"mato/internal/status"

	"github.com/spf13/cobra"
)

var version = "dev"

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

// UsageError marks command-line misuse that should print the command usage.
type UsageError struct {
	Err   error
	Usage string
}

func (e *UsageError) Error() string {
	return e.Err.Error()
}

func (e *UsageError) Unwrap() error {
	return e.Err
}

// SilentError carries a non-zero exit code for failures that have already been
// reported to the user and should not be printed again by main.
type SilentError struct {
	Err  error
	Code int
}

func (e *SilentError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *SilentError) Unwrap() error {
	return e.Err
}

func newUsageError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err, Usage: cmd.UsageString()}
}

func usageNoArgs(cmd *cobra.Command, args []string) error {
	return newUsageError(cmd, cobra.NoArgs(cmd, args))
}

func usageExactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return newUsageError(cmd, cobra.ExactArgs(n)(cmd, args))
	}
}

func usageMinimumNArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return newUsageError(cmd, cobra.MinimumNArgs(n)(cmd, args))
	}
}

func configureCommand(cmd *cobra.Command) {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return newUsageError(c, err)
	})
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "mato %s\n", version)
	return err
}

func writeCommandError(w io.Writer, err error) int {
	var exitErr ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}

	var silentErr *SilentError
	if errors.As(err, &silentErr) {
		return silentErr.Code
	}

	var usageErr *UsageError
	if errors.As(err, &usageErr) {
		fmt.Fprintf(w, "mato error: %v\n\n", usageErr.Err)
		_, _ = io.WriteString(w, usageErr.Usage)
		return 1
	}

	fmt.Fprintf(w, "mato error: %v\n", err)
	return 1
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

func validateReasoningEffort(value, flagName string) error {
	if !validReasoningEfforts[value] {
		return fmt.Errorf("invalid %s %q: must be one of low, medium, high, xhigh", flagName, value)
	}
	return nil
}

func resolveRunOptions(flags runFlags, cfg config.Config) (runner.RunOptions, error) {
	var opts runner.RunOptions

	if v := os.Getenv("MATO_DOCKER_IMAGE"); v != "" {
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

	if v := os.Getenv("MATO_AGENT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return opts, fmt.Errorf("parse MATO_AGENT_TIMEOUT %q: %w", v, err)
		}
		if d <= 0 {
			return opts, fmt.Errorf("MATO_AGENT_TIMEOUT must be positive, got %v", d)
		}
		opts.AgentTimeout = d
	} else if cfg.AgentTimeout != nil {
		d, err := time.ParseDuration(*cfg.AgentTimeout)
		if err != nil {
			return opts, fmt.Errorf("invalid agent_timeout %q in .mato.yaml: %w", *cfg.AgentTimeout, err)
		}
		if d <= 0 {
			return opts, fmt.Errorf("agent_timeout in .mato.yaml must be positive, got %v", d)
		}
		opts.AgentTimeout = d
	}

	if v := os.Getenv("MATO_RETRY_COOLDOWN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return opts, fmt.Errorf("parse MATO_RETRY_COOLDOWN %q: %w", v, err)
		}
		if d <= 0 {
			return opts, fmt.Errorf("MATO_RETRY_COOLDOWN must be positive, got %v", d)
		}
		opts.RetryCooldown = d
	} else if cfg.RetryCooldown != nil {
		d, err := time.ParseDuration(*cfg.RetryCooldown)
		if err != nil {
			return opts, fmt.Errorf("invalid retry_cooldown %q in .mato.yaml: %w", *cfg.RetryCooldown, err)
		}
		if d <= 0 {
			return opts, fmt.Errorf("retry_cooldown in .mato.yaml must be positive, got %v", d)
		}
		opts.RetryCooldown = d
	}

	return opts, nil
}

var gitShowTopLevel = func(dir string) (string, error) {
	return git.Output(dir, "rev-parse", "--show-toplevel")
}

func resolveRepoRoot(dir string) (string, error) {
	out, err := gitShowTopLevel(dir)
	if err != nil {
		return "", fmt.Errorf("resolve repo root for %q: %w", dir, err)
	}
	return strings.TrimSpace(out), nil
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
	info, err := os.Stat(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf(".mato/ directory not found - run 'mato init' first")
		}
		return fmt.Errorf("stat %s: %w", tasksDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", tasksDir)
	}
	return nil
}

// runFn is the function used to start the orchestrator loop. Defaults to
// runner.Run and can be replaced in tests to observe resolved values.
var runFn = runner.Run

// dryRunFn is the function used for dry-run validation. Defaults to
// runner.DryRun and can be replaced in tests.
var dryRunFn = runner.DryRun

func newRootCmd() *cobra.Command {
	var repoFlag string

	root := &cobra.Command{
		Use:     "mato",
		Short:   "Orchestrate autonomous Copilot agents against a task queue",
		Long:    "Mato orchestrates autonomous Copilot agents against a filesystem-backed task queue in Docker.",
		Example: "mato run\nmato status\nmato version",
		Args:    usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	configureCommand(root)
	root.Version = version
	root.SetVersionTemplate("mato {{.Version}}\n")
	root.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to the git repository (default: current directory)")

	root.AddCommand(newRunCmd(&repoFlag))
	root.AddCommand(newStatusCmd(&repoFlag))
	root.AddCommand(newLogCmd(&repoFlag))
	root.AddCommand(newDoctorCmd(&repoFlag))
	root.AddCommand(newGraphCmd(&repoFlag))
	root.AddCommand(newInitCmd(&repoFlag))
	root.AddCommand(newInspectCmd(&repoFlag))
	root.AddCommand(newCancelCmd(&repoFlag))
	root.AddCommand(newRetryCmd(&repoFlag))
	root.AddCommand(newPauseCmd(&repoFlag))
	root.AddCommand(newResumeCmd(&repoFlag))
	root.AddCommand(newVersionCmd())
	return root
}

func newRunCmd(repoFlag *string) *cobra.Command {
	var branch string
	var dryRun bool
	var flags runFlags

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the orchestrator loop",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			fileCfg, err := config.Load(repoRoot)
			if err != nil {
				return err
			}
			resolvedBranch, err := resolveConfigBranch(fileCfg, branch)
			if err != nil {
				return err
			}
			if err := validateBranch(resolvedBranch); err != nil {
				return err
			}
			opts, err := resolveRunOptions(flags, fileCfg)
			if err != nil {
				return err
			}
			if dryRun {
				return dryRunFn(repoRoot, resolvedBranch, opts)
			}
			return runFn(repoRoot, resolvedBranch, opts)
		},
	}
	configureCommand(cmd)
	cmd.Flags().StringVar(&branch, "branch", "", "Target branch for merging (default: mato)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate queue setup without launching Docker containers")
	cmd.Flags().StringVar(&flags.TaskModel, "task-model", "", "Copilot model for task agents")
	cmd.Flags().StringVar(&flags.ReviewModel, "review-model", "", "Copilot model for review agents")
	cmd.Flags().StringVar(&flags.TaskReasoningEffort, "task-reasoning-effort", "", "Reasoning effort for task agents")
	cmd.Flags().StringVar(&flags.ReviewReasoningEffort, "review-reasoning-effort", "", "Reasoning effort for review agents")
	return cmd
}

func newInitCmd(repoFlag *string) *cobra.Command {
	var initBranch string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a repository for mato use",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			fileCfg, err := config.Load(repoRoot)
			if err != nil {
				return err
			}
			branch, err := resolveConfigBranch(fileCfg, initBranch)
			if err != nil {
				return err
			}
			if err := validateBranch(branch); err != nil {
				return err
			}

			result, err := setup.InitRepo(repoRoot, branch)
			if err != nil {
				return err
			}
			printInitResult(result)
			return nil
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&initBranch, "branch", "", "Target branch name (default: mato)")

	return cmd
}

func printInitResult(result *setup.InitResult) {
	if len(result.DirsCreated) > 0 {
		for _, rel := range result.DirsCreated {
			fmt.Printf("Created %s/\n", rel)
		}
	} else {
		fmt.Printf("%s/ directory structure already exists\n", dirs.Root)
	}

	if result.GitignoreUpdated {
		fmt.Printf("Added %s to .gitignore\n", result.IgnorePattern)
	} else {
		fmt.Printf(".gitignore already contains %s\n", result.IgnorePattern)
	}

	switch {
	case result.AlreadyOnBranch:
		fmt.Printf("Already on branch: %s (%s)\n", result.BranchName, branchSourceDescription(result))
	case result.LocalBranchExisted || result.BranchSource == git.BranchSourceRemote || result.BranchSource == git.BranchSourceRemoteCached:
		fmt.Printf("Switched to branch: %s (%s)\n", result.BranchName, branchSourceDescription(result))
	default:
		fmt.Printf("Created branch: %s from %s\n", result.BranchName, branchSourceDescription(result))
	}

	if len(result.DirsCreated) == 0 && !result.GitignoreUpdated && result.AlreadyOnBranch {
		fmt.Println("Nothing to do - already initialized.")
		return
	}
	fmt.Printf("Ready to add tasks to %s\n", filepath.Join(result.TasksDir, "backlog")+string(filepath.Separator))
}

func branchSourceDescription(result *setup.InitResult) string {
	return git.DescribeBranchSource(result.BranchName, result.BranchSource)
}

func newStatusCmd(repoFlag *string) *cobra.Command {
	var watch bool
	var interval time.Duration
	var format string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current state of the task queue",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if format == "json" {
				if watch {
					return newUsageError(cmd, fmt.Errorf("--format json and --watch cannot be used together"))
				}
				return status.ShowJSON(os.Stdout, repo)
			}
			if watch {
				if interval <= 0 {
					return newUsageError(cmd, fmt.Errorf("--interval must be a positive duration, got %s", interval))
				}
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stop()
				return status.Watch(ctx, repo, interval)
			}
			return status.Show(repo)
		},
	}
	configureCommand(cmd)

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Continuously refresh the status display")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Refresh interval for watch mode")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

// ExitError carries a non-zero exit code without printing "mato error:".
type ExitError struct {
	Code int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit %d", e.Code)
}

// doctorRunFn is the function used to run health checks. It defaults to
// doctor.Run and can be replaced in tests to inject failures or exit codes.
var doctorRunFn = doctor.Run

// inspectShowFn is the function used to render task inspection results.
// Tests replace it to verify CLI flag parsing and delegation.
var inspectShowFn = inspect.Show

// logShowFn is the function used to render durable task history.
// Tests replace it to verify CLI flag parsing and delegation.
var logShowFn = history.Show

func doctorNeedsDockerConfig(only []string) bool {
	if len(only) == 0 {
		return true
	}
	for _, name := range only {
		if name == "docker" {
			return true
		}
	}
	return false
}

func newDoctorCmd(repoFlag *string) *cobra.Command {
	var fix bool
	var format string
	var only []string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks on the repository and task queue",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}

			repoInput, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}

			var dockerImage string
			if doctorNeedsDockerConfig(only) {
				// Resolve docker image the same way as the run command:
				// env var > .mato.yaml > default. If the repo root cannot
				// be determined, fall back to env/default and let the git
				// check report the problem. Config load errors are fatal
				// so doctor does not silently produce results based on
				// the wrong image when .mato.yaml is malformed.
				if v := os.Getenv("MATO_DOCKER_IMAGE"); v != "" {
					dockerImage = v
				} else if root, err := resolveRepoRoot(repoInput); err == nil {
					fileCfg, err := config.Load(root)
					if err != nil {
						return err
					}
					if fileCfg.DockerImage != nil {
						dockerImage = *fileCfg.DockerImage
					}
				}
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			report, err := doctorRunFn(ctx, repoInput, doctor.Options{
				Fix:         fix,
				Format:      format,
				Only:        only,
				DockerImage: dockerImage,
			})
			if err != nil {
				return err // hard failure -> "mato error: ..." + exit 1
			}

			if format == "json" {
				if renderErr := doctor.RenderJSON(os.Stdout, report); renderErr != nil {
					return renderErr
				}
			} else {
				doctor.RenderText(os.Stdout, report)
			}

			if report.ExitCode != 0 {
				return ExitError{Code: report.ExitCode} // health status -> silent exit 1 or 2
			}
			return nil // healthy -> exit 0
		},
	}
	configureCommand(cmd)

	cmd.Flags().BoolVar(&fix, "fix", false, "Auto-repair safe issues (stale locks, orphaned tasks, missing dirs, Docker image pulls, stale events, temp files)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().StringSliceVar(&only, "only", nil, "Run only specified checks (repeatable: git, tools, docker, queue, tasks, locks, hygiene, deps)")

	return cmd
}

func newLogCmd(repoFlag *string) *cobra.Command {
	var limit int
	var format string

	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show recent durable task outcomes",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			if limit < 0 {
				return newUsageError(cmd, fmt.Errorf("--limit must be >= 0, got %d", limit))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			return logShowFn(repo, limit, format)
		},
	}
	configureCommand(cmd)

	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of events to show (0 means unlimited)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newGraphCmd(repoFlag *string) *cobra.Command {
	var format string
	var showAll bool

	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Visualize task dependency topology",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "dot" && format != "json" {
				return newUsageError(cmd, fmt.Errorf("--format must be text, dot, or json, got %s", format))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			return graph.Show(repo, format, showAll)
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&format, "format", "text", "Output format: text, dot, or json")
	cmd.Flags().BoolVar(&showAll, "all", false, "Include completed and failed tasks")

	return cmd
}

func newInspectCmd(repoFlag *string) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "inspect <task-ref>",
		Short: "Explain the current state of a single task",
		Args:  usageExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			return inspectShowFn(repo, args[0], format)
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newRetryCmd(repoFlag *string) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "retry <task-ref> [task-ref...]",
		Short: "Requeue failed tasks back to backlog",
		Args:  usageMinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			tasksDir := filepath.Join(repoRoot, dirs.Root)
			if err := requireTasksDir(tasksDir); err != nil {
				return err
			}

			var firstErr error
			for _, name := range args {
				if err := queue.RetryTask(tasksDir, name); err != nil {
					fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				stem := strings.TrimSuffix(name, ".md")
				fmt.Printf("Requeued %s to backlog\n", stem)
			}
			if firstErr != nil {
				return &SilentError{Err: firstErr, Code: 1}
			}
			return nil
		},
	}
	configureCommand(cmd)

	return cmd
}

func newPauseCmd(repoFlag *string) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Pause new task claims and review launches",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			tasksDir := filepath.Join(repoRoot, dirs.Root)
			if err := requireTasksDir(tasksDir); err != nil {
				return err
			}
			result, err := pause.Pause(tasksDir, time.Now().UTC())
			if err != nil {
				return err
			}
			since := result.Since.Format(time.RFC3339)
			switch {
			case result.AlreadyPaused:
				fmt.Printf("Already paused since %s\n", since)
			case result.Repaired:
				fmt.Printf("Repaired pause sentinel. Paused since %s\n", since)
			default:
				fmt.Printf("Paused since %s\n", since)
			}
			return nil
		},
	}
	configureCommand(cmd)
	return cmd
}

func newResumeCmd(repoFlag *string) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume task claims and review launches",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			tasksDir := filepath.Join(repoRoot, dirs.Root)
			if err := requireTasksDir(tasksDir); err != nil {
				return err
			}
			result, err := pause.Resume(tasksDir)
			if err != nil {
				return err
			}
			if result.WasActive {
				fmt.Println("Resumed")
			} else {
				fmt.Println("Not paused")
			}
			return nil
		},
	}
	configureCommand(cmd)
	return cmd
}

var cancelTaskFn = queue.CancelTask

func newCancelCmd(repoFlag *string) *cobra.Command {

	cmd := &cobra.Command{
		Use:   "cancel <task-ref> [task-ref...]",
		Short: "Withdraw tasks from the queue by moving them to failed/",
		Args:  usageMinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			tasksDir := filepath.Join(repoRoot, dirs.Root)
			if err := requireTasksDir(tasksDir); err != nil {
				return err
			}

			var firstErr error
			for _, ref := range args {
				result, err := cancelTaskFn(tasksDir, ref)
				if err != nil {
					fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				stem := strings.TrimSuffix(result.Filename, ".md")
				fmt.Printf("cancelled: %s (was in %s/)\n", result.Filename, result.PriorState)
				if result.PriorState == queue.DirInProgress {
					fmt.Fprintf(os.Stderr, "warning: agent container for %s may still be running\n", stem)
				}
				if result.PriorState == queue.DirReadyMerge {
					fmt.Fprintf(os.Stderr, "warning: merge queue may still merge %s's branch\n", stem)
				}
				if len(result.Warnings) > 0 {
					fmt.Printf("  warning: %d task(s) depend on %s:\n", len(result.Warnings), stem)
					for _, warning := range result.Warnings {
						fmt.Printf("    %s\n", warning)
					}
					fmt.Printf("  these tasks will remain blocked until %s is retried\n", stem)
				}
			}
			if firstErr != nil {
				return &SilentError{Err: firstErr, Code: 1}
			}
			return nil
		},
	}
	configureCommand(cmd)

	return cmd
}

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print mato version",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return printVersion(cmd.OutOrStdout())
		},
	}
	configureCommand(cmd)
	return cmd
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(writeCommandError(os.Stderr, err))
	}
}
