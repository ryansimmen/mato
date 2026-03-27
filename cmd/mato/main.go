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
	"strconv"
	"strings"
	"time"

	"mato/internal/config"
	"mato/internal/dirs"
	"mato/internal/doctor"
	"mato/internal/git"
	"mato/internal/graph"
	"mato/internal/inspect"
	"mato/internal/queue"
	"mato/internal/runner"
	"mato/internal/setup"
	"mato/internal/status"

	"github.com/spf13/cobra"
)

var version = "dev"

// runConfig holds the parsed flags for the root command.
type runConfig struct {
	repo        string
	branch      string
	dryRun      bool
	version     bool
	copilotArgs []string
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

// assignFlag sets the target field on cfg for a known flag name. It returns
// true if the flag was recognised, false otherwise. This avoids duplicating
// the assignment switch for the --flag=value and --flag value parsing paths.
func assignFlag(name, val string, cfg *runConfig) bool {
	switch name {
	case "--repo":
		cfg.repo = val
	case "--branch":
		cfg.branch = val
	default:
		return false
	}
	return true
}

// extractKnownFlags separates mato's own flags from arguments that should be
// forwarded to the copilot CLI inside the Docker container. The root command
// uses DisableFlagParsing so that unknown flags (like --model) are not rejected
// by cobra and can be passed through.
func extractKnownFlags(args []string) (runConfig, error) {
	cfg := runConfig{copilotArgs: make([]string, 0, len(args))}
	known := map[string]bool{"--repo": true, "--branch": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			cfg.copilotArgs = append(cfg.copilotArgs, args[i+1:]...)
			break
		}
		if arg == "--dry-run" {
			cfg.dryRun = true
			continue
		}
		if arg == "--version" {
			cfg.version = true
			continue
		}
		if strings.HasPrefix(arg, "--dry-run=") {
			val := strings.TrimPrefix(arg, "--dry-run=")
			b, parseErr := strconv.ParseBool(val)
			if parseErr != nil {
				return runConfig{}, fmt.Errorf("invalid value %q for flag --dry-run: must be a boolean", val)
			}
			cfg.dryRun = b
			continue
		}
		if strings.HasPrefix(arg, "--version=") {
			val := strings.TrimPrefix(arg, "--version=")
			b, parseErr := strconv.ParseBool(val)
			if parseErr != nil {
				return runConfig{}, fmt.Errorf("invalid value %q for flag --version: must be a boolean", val)
			}
			cfg.version = b
			continue
		}
		// --flag=value form
		handled := false
		for flag := range known {
			if strings.HasPrefix(arg, flag+"=") {
				val := strings.TrimSpace(strings.TrimPrefix(arg, flag+"="))
				if val == "" {
					return runConfig{}, fmt.Errorf("flag %s requires a value", flag)
				}
				assignFlag(flag, val, &cfg)
				handled = true
				break
			}
		}
		if handled {
			continue
		}
		// --flag value form
		if known[arg] {
			if i+1 >= len(args) {
				return runConfig{}, fmt.Errorf("flag %s requires a value", arg)
			}
			next := args[i+1]
			if strings.HasPrefix(next, "--") {
				return runConfig{}, fmt.Errorf("flag %s requires a value, got flag %s", arg, next)
			}
			i++
			val := strings.TrimSpace(next)
			if val == "" {
				return runConfig{}, fmt.Errorf("flag %s requires a value", arg)
			}
			assignFlag(arg, val, &cfg)
			continue
		}
		cfg.copilotArgs = append(cfg.copilotArgs, arg)
	}
	return cfg, nil
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

func resolveRunOptions(cfg config.Config) (runner.RunOptions, error) {
	var opts runner.RunOptions

	if v := os.Getenv("MATO_DOCKER_IMAGE"); v != "" {
		opts.DockerImage = v
	} else if cfg.DockerImage != nil {
		opts.DockerImage = *cfg.DockerImage
	}

	if v := os.Getenv("MATO_DEFAULT_MODEL"); v != "" {
		opts.DefaultModel = v
	} else if cfg.DefaultModel != nil {
		opts.DefaultModel = *cfg.DefaultModel
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

// runFn is the function used to start the orchestrator loop. Defaults to
// runner.Run and can be replaced in tests to observe resolved values.
var runFn = runner.Run

// dryRunFn is the function used for dry-run validation. Defaults to
// runner.DryRun and can be replaced in tests.
var dryRunFn = runner.DryRun

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mato [copilot-args...]",
		Short: "Runs autonomous Copilot agents against a task queue in Docker",
		Long: `Runs autonomous Copilot agents against a task queue in Docker.

Any unrecognized flags are forwarded to the copilot CLI inside the container.
Pass a custom model directly; for example, "mato --model gpt-5.4".
Use "mato -- <copilot-args>" to force forwarding.`,
		Example:            "mato --model gpt-5.4\nmato status\nmato version",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "--" {
					break
				}
				if a == "--help" || a == "-h" {
					cmd.DisableFlagParsing = false
					return cmd.Help()
				}
			}
			cfg, err := extractKnownFlags(args)
			if err != nil {
				return newUsageError(cmd, err)
			}
			if cfg.version {
				return printVersion(cmd.OutOrStdout())
			}
			resolved, err := resolveRepo(cfg.repo)
			if err != nil {
				return err
			}
			if err := validateRepoPath(resolved); err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(resolved)
			if err != nil {
				return err
			}
			fileCfg, err := config.Load(repoRoot)
			if err != nil {
				return err
			}
			if cfg.dryRun {
				br, err := resolveConfigBranch(fileCfg, cfg.branch)
				if err != nil {
					return err
				}
				if err := validateBranch(br); err != nil {
					return err
				}
				return dryRunFn(repoRoot, br)
			}
			opts, err := resolveRunOptions(fileCfg)
			if err != nil {
				return err
			}
			br, err := resolveConfigBranch(fileCfg, cfg.branch)
			if err != nil {
				return err
			}
			if err := validateBranch(br); err != nil {
				return err
			}
			return runFn(repoRoot, br, cfg.copilotArgs, opts)
		},
	}
	// The root command keeps DisableFlagParsing enabled so unknown arguments can be
	// forwarded to Copilot. configureCommand is still useful here for shared
	// SilenceUsage/SilenceErrors behavior, even though Cobra will not call the
	// flag error hook on the root command.
	configureCommand(root)

	// Flags are defined for help/documentation only; actual parsing is manual
	// because DisableFlagParsing is true (required for copilot arg forwarding).
	root.Flags().String("repo", "", "Path to the git repository (default: current directory)")
	root.Flags().String("branch", "", "Target branch for merging (default: mato)")
	root.Flags().Bool("dry-run", false, "Validate queue setup without launching Docker containers")
	root.Flags().Bool("version", false, "Print mato version and exit")

	root.AddCommand(newStatusCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newGraphCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newInspectCmd())
	root.AddCommand(newCancelCmd())
	root.AddCommand(newRetryCmd())
	root.AddCommand(newVersionCmd())
	return root
}

func newInitCmd() *cobra.Command {
	var initRepo string
	var initBranch string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a repository for mato use",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(initRepo)
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

	cmd.Flags().StringVar(&initRepo, "repo", "", "Path to the git repository (default: current directory)")
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

func newStatusCmd() *cobra.Command {
	var statusRepo string
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
			repo, err := resolveRepo(statusRepo)
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

	cmd.Flags().StringVar(&statusRepo, "repo", "", "Path to the git repository (default: current directory)")
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

func newDoctorCmd() *cobra.Command {
	var doctorRepo string
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

			repoInput := doctorRepo
			if repoInput == "" {
				wd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				repoInput = wd
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

	cmd.Flags().StringVar(&doctorRepo, "repo", "", "Path to git repository (default: current directory)")
	cmd.Flags().BoolVar(&fix, "fix", false, "Auto-repair safe issues (stale locks, orphaned tasks, missing dirs)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().StringSliceVar(&only, "only", nil, "Run only specified checks (repeatable: git, tools, docker, queue, tasks, locks, hygiene, deps)")

	return cmd
}

func newGraphCmd() *cobra.Command {
	var graphRepo string
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
			repo, err := resolveRepo(graphRepo)
			if err != nil {
				return err
			}
			return graph.Show(repo, format, showAll)
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&graphRepo, "repo", "", "Path to the git repository (default: current directory)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text, dot, or json")
	cmd.Flags().BoolVar(&showAll, "all", false, "Include completed and failed tasks")

	return cmd
}

func newInspectCmd() *cobra.Command {
	var inspectRepo string
	var format string

	cmd := &cobra.Command{
		Use:   "inspect <task-ref>",
		Short: "Explain the current state of a single task",
		Args:  usageExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			repo, err := resolveRepo(inspectRepo)
			if err != nil {
				return err
			}
			return inspectShowFn(repo, args[0], format)
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&inspectRepo, "repo", "", "Path to the git repository (default: current directory)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newRetryCmd() *cobra.Command {
	var retryRepo string

	cmd := &cobra.Command{
		Use:   "retry <task-ref> [task-ref...]",
		Short: "Requeue failed tasks back to backlog",
		Args:  usageMinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(retryRepo)
			if err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			tasksDir := filepath.Join(repoRoot, dirs.Root)

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

	cmd.Flags().StringVar(&retryRepo, "repo", "", "Path to the git repository (default: current directory)")

	return cmd
}

var cancelTaskFn = queue.CancelTask

func newCancelCmd() *cobra.Command {
	var cancelRepo string

	cmd := &cobra.Command{
		Use:   "cancel <task-ref> [task-ref...]",
		Short: "Withdraw tasks from the queue by moving them to failed/",
		Args:  usageMinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(cancelRepo)
			if err != nil {
				return err
			}
			repoRoot, err := resolveRepoRoot(repo)
			if err != nil {
				return err
			}
			tasksDir := filepath.Join(repoRoot, dirs.Root)

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

	cmd.Flags().StringVar(&cancelRepo, "repo", "", "Path to the git repository (default: current directory)")

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
