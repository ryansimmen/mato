package main

import (
	"context"
	"fmt"
	"os"
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
	"mato/internal/setup"
	"mato/internal/status"
	"mato/internal/ui"

	"github.com/spf13/cobra"
)

// doctorRunFn is the function used to run health checks. It defaults to
// doctor.Run and can be replaced in tests to inject failures or exit codes.
var doctorRunFn = doctor.Run

// inspectShowFn is the function used to render task inspection results.
// Tests replace it to verify CLI flag parsing and delegation.
var inspectShowFn = inspect.Show

// logShowFn is the function used to render durable task history.
// Tests replace it to verify CLI flag parsing and delegation.
var logShowFn = history.Show

var cancelTaskFn = queue.CancelTask

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
	var verbose bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current state of the task queue",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
				return err
			}
			if format == "json" {
				if watch {
					return newUsageError(cmd, fmt.Errorf("--format json and --watch cannot be used together"))
				}
				if verbose {
					return newUsageError(cmd, fmt.Errorf("--verbose can only be used with text output"))
				}
				return status.ShowJSON(os.Stdout, repo)
			}
			if watch {
				if interval <= 0 {
					return newUsageError(cmd, fmt.Errorf("--interval must be a positive duration, got %s", interval))
				}
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stop()
				if verbose {
					return status.WatchVerbose(ctx, repo, interval)
				}
				return status.Watch(ctx, repo, interval)
			}
			if verbose {
				return status.ShowVerbose(repo)
			}
			return status.Show(repo)
		},
	}
	configureCommand(cmd)

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Continuously refresh the status display")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Refresh interval for watch mode")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show the expanded text status view")

	return cmd
}

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
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}

			repoInput, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repoInput); err != nil {
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
				if v := strings.TrimSpace(os.Getenv("MATO_DOCKER_IMAGE")); v != "" {
					dockerImage = v
					// Still validate repo config so a malformed committed
					// .mato.yaml is not hidden during a full doctor run
					// just because an env override supplies the image.
					if root, err := resolveRepoRoot(repoInput); err == nil {
						if _, err := config.Load(root); err != nil {
							return err
						}
					}
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
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			if limit < 0 {
				return newUsageError(cmd, fmt.Errorf("--limit must be >= 0, got %d", limit))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
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
			if err := ui.ValidateFormat(format, []string{"text", "dot", "json"}); err != nil {
				return newUsageError(cmd, fmt.Errorf("--format must be text, dot, or json, got %s", format))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
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
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, fmt.Errorf("--format must be text or json, got %s", format))
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
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
			if err := validateRepoPath(repo); err != nil {
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
			if err := validateRepoPath(repo); err != nil {
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
			if err := validateRepoPath(repo); err != nil {
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
			if err := validateRepoPath(repo); err != nil {
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
					ui.Warnf("warning: agent container for %s may still be running\n", stem)
				}
				if result.PriorState == queue.DirReadyReview {
					ui.Warnf("warning: task is in ready-for-review/ — a review agent may be running\n")
				}
				if result.PriorState == queue.DirReadyMerge {
					ui.Warnf("warning: merge queue may still merge %s's branch\n", stem)
				}
				if len(result.Warnings) > 0 {
					ui.Warnf("warning: %d task(s) depend on %s:\n", len(result.Warnings), stem)
					for _, warning := range result.Warnings {
						ui.Warnf("  %s\n", warning)
					}
					ui.Warnf("these tasks will remain blocked until %s is retried\n", stem)
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
