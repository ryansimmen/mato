package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mato/internal/dirs"
	"mato/internal/doctor"
	"mato/internal/git"
	"mato/internal/graph"
	"mato/internal/queue"
	"mato/internal/runner"
	"mato/internal/setup"
	"mato/internal/status"

	"github.com/spf13/cobra"
)

// runConfig holds the parsed flags for the root command.
type runConfig struct {
	repo        string
	branch      string
	dryRun      bool
	copilotArgs []string
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
		if strings.HasPrefix(arg, "--dry-run=") {
			val := strings.TrimPrefix(arg, "--dry-run=")
			b, parseErr := strconv.ParseBool(val)
			if parseErr != nil {
				return runConfig{}, fmt.Errorf("invalid value %q for flag --dry-run: must be a boolean", val)
			}
			cfg.dryRun = b
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

func resolveBranch(b string) string {
	if b != "" {
		return b
	}
	return "mato"
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

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mato [copilot-args...]",
		Short: "Runs autonomous Copilot agents against a task queue in Docker",
		Long: `Runs autonomous Copilot agents against a task queue in Docker.

Any unrecognized flags are forwarded to the copilot CLI inside the container.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
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
				return err
			}
			resolved, err := resolveRepo(cfg.repo)
			if err != nil {
				return err
			}
			if err := validateRepoPath(resolved); err != nil {
				return err
			}
			br := resolveBranch(cfg.branch)
			if err := validateBranch(br); err != nil {
				return err
			}
			if cfg.dryRun {
				return runner.DryRun(resolved, br)
			}
			return runner.Run(resolved, br, cfg.copilotArgs)
		},
	}

	// Flags are defined for help/documentation only; actual parsing is manual
	// because DisableFlagParsing is true (required for copilot arg forwarding).
	root.Flags().String("repo", "", "Path to the git repository (default: current directory)")
	root.Flags().String("branch", "", "Target branch for merging (default: mato)")
	root.Flags().Bool("dry-run", false, "Validate queue setup without launching Docker containers")

	root.AddCommand(newStatusCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newGraphCmd())
	root.AddCommand(newInitCmd())
	root.AddCommand(newRetryCmd())
	return root
}

func newInitCmd() *cobra.Command {
	var initRepo string
	var initBranch string

	cmd := &cobra.Command{
		Use:           "init",
		Short:         "Initialize a repository for mato use",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
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
			branch := resolveBranch(initBranch)
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
		fmt.Printf("Already on branch: %s\n", result.BranchName)
	case result.BranchExisted:
		fmt.Printf("Switched to branch: %s\n", result.BranchName)
	default:
		fmt.Printf("Created branch: %s\n", result.BranchName)
	}

	if len(result.DirsCreated) == 0 && !result.GitignoreUpdated && result.AlreadyOnBranch {
		fmt.Println("Nothing to do - already initialized.")
		return
	}
	fmt.Printf("Ready to add tasks to %s\n", filepath.Join(result.TasksDir, "backlog")+string(filepath.Separator))
}

func newStatusCmd() *cobra.Command {
	var statusRepo string
	var watch bool
	var interval time.Duration
	var format string

	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Show the current state of the task queue",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json, got %s", format)
			}
			repo, err := resolveRepo(statusRepo)
			if err != nil {
				return err
			}
			if format == "json" {
				if watch {
					return fmt.Errorf("--format json and --watch cannot be used together")
				}
				return status.ShowJSON(os.Stdout, repo)
			}
			if watch {
				if interval <= 0 {
					return fmt.Errorf("--interval must be a positive duration, got %s", interval)
				}
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stop()
				return status.Watch(ctx, repo, interval)
			}
			return status.Show(repo)
		},
	}

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

func newDoctorCmd() *cobra.Command {
	var doctorRepo string
	var fix bool
	var format string
	var only []string

	cmd := &cobra.Command{
		Use:           "doctor",
		Short:         "Run health checks on the repository and task queue",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json, got %s", format)
			}

			repoInput := doctorRepo
			if repoInput == "" {
				wd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("get working directory: %w", err)
				}
				repoInput = wd
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			report, err := doctorRunFn(ctx, repoInput, doctor.Options{
				Fix:    fix,
				Format: format,
				Only:   only,
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
		Use:           "graph",
		Short:         "Visualize task dependency topology",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "dot" && format != "json" {
				return fmt.Errorf("--format must be text, dot, or json, got %s", format)
			}
			repo, err := resolveRepo(graphRepo)
			if err != nil {
				return err
			}
			return graph.Show(repo, format, showAll)
		},
	}

	cmd.Flags().StringVar(&graphRepo, "repo", "", "Path to the git repository (default: current directory)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text, dot, or json")
	cmd.Flags().BoolVar(&showAll, "all", false, "Include completed and failed tasks")

	return cmd
}

func newRetryCmd() *cobra.Command {
	var retryRepo string

	cmd := &cobra.Command{
		Use:           "retry <task-name> [task-name...]",
		Short:         "Requeue failed tasks back to backlog",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MinimumNArgs(1),
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
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				stem := strings.TrimSuffix(name, ".md")
				fmt.Printf("Requeued %s to backlog\n", stem)
			}
			return firstErr
		},
	}

	cmd.Flags().StringVar(&retryRepo, "repo", "", "Path to the git repository (default: current directory)")

	return cmd
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var exitErr ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.Code)
		}
		fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
		os.Exit(1)
	}
}
