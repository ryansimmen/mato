package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"mato/internal/config"
	"mato/internal/configresolve"
	"mato/internal/dirs"
	"mato/internal/doctor"
	"mato/internal/frontmatter"
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
	"golang.org/x/term"
)

// doctorRunFn is the function used to run health checks. It defaults to
// doctor.Run and can be replaced in tests to inject failures or exit codes.
var doctorRunFn = doctor.Run

// inspectShowFn is the function used to render task inspection results.
// Tests replace it to verify CLI flag parsing, delegation, and writer errors.
var inspectShowFn = inspect.ShowTo

// logShowFn is the function used to render durable task history.
// Tests replace it to verify CLI flag parsing and delegation.
var logShowFn = history.ShowTo

// graphShowFn is the function used to render dependency graphs.
// Tests replace it to verify CLI flag parsing, delegation, and writer errors.
var graphShowFn = graph.ShowTo

// configShowFn is the function used to render resolved repository config.
// Tests replace it to verify CLI flag parsing and delegation.
var configShowFn = showConfig

var cancelTaskFn = queue.CancelTask

// confirmCancelFn asks the user to confirm cancellation. It receives an
// io.Reader (normally os.Stdin) and returns true if the user confirmed.
// Tests replace it to simulate user input.
var confirmCancelFn = confirmCancel

// stdinIsTerminalFn returns true when stdin is a TTY. Tests replace it
// to exercise the interactive confirmation path.
var stdinIsTerminalFn = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func newInitCmd(repoFlag *string) *cobra.Command {
	var initBranch string
	var format string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a repository for mato use",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
			if initBranch != "" && strings.TrimSpace(initBranch) == "" {
				return newUsageError(cmd, fmt.Errorf("--branch must not be whitespace-only"))
			}
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
			load, err := config.Load(repoRoot)
			if err != nil {
				return err
			}
			branch, err := configresolve.ResolveBranch(load, initBranch)
			if err != nil {
				return err
			}
			if err := validateBranch(branch.Value); err != nil {
				return err
			}

			result, err := setup.InitRepo(repoRoot, branch.Value)
			if err != nil {
				return err
			}
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			printInitResult(result)
			return nil
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&initBranch, "branch", "", "Target branch name (default: mato)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

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
				return newUsageError(cmd, err)
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
				return status.ShowJSON(cmd.OutOrStdout(), repo)
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
				return status.ShowVerboseTo(cmd.OutOrStdout(), repo)
			}
			return status.ShowTo(cmd.OutOrStdout(), repo)
		},
	}
	configureCommand(cmd)

	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Continuously refresh the status display")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Refresh interval for watch mode")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Show the expanded text status view")

	return cmd
}

func doctorShouldPreResolveDockerImage(only []string) bool {
	if len(only) == 0 {
		return false
	}
	hasDocker := false
	hasConfig := false
	for _, name := range only {
		if !doctor.IsValidCheckName(name) {
			return false
		}
		switch name {
		case "docker":
			hasDocker = true
		case "config":
			hasConfig = true
		}
	}
	return hasDocker && !hasConfig
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
				return newUsageError(cmd, err)
			}

			repoInput, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}

			var dockerImage string
			if doctorShouldPreResolveDockerImage(only) {
				repoRoot := ""
				if root, err := resolveRepoRoot(repoInput); err == nil {
					repoRoot = root
				}
				resolvedImage, err := configresolve.ResolveDoctorDockerImage(repoRoot)
				if err != nil {
					return err
				}
				dockerImage = resolvedImage.Value
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
				if renderErr := doctor.RenderJSON(cmd.OutOrStdout(), report); renderErr != nil {
					return renderErr
				}
			} else {
				doctor.RenderText(cmd.OutOrStdout(), report)
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
	cmd.Flags().StringSliceVar(&only, "only", nil, "Run only specified checks (repeatable: git, tools, config, docker, queue, tasks, locks, hygiene, deps)")

	return cmd
}

func newConfigCmd(repoFlag *string) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show effective repository configuration",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
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
			return configShowFn(cmd.OutOrStdout(), repoRoot, format)
		},
	}
	configureCommand(cmd)
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	return cmd
}

func showConfig(w io.Writer, repoRoot, format string) error {
	resolved, err := configresolve.ResolveRepoDefaults(repoRoot)
	if err != nil {
		return err
	}
	if err := validateBranch(resolved.Branch.Value); err != nil {
		return err
	}
	if format == "json" {
		return configresolve.RenderJSON(w, resolved)
	}
	return configresolve.RenderText(w, resolved)
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
				return newUsageError(cmd, err)
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
			return logShowFn(cmd.OutOrStdout(), repo, limit, format)
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
				return newUsageError(cmd, err)
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
				return err
			}
			return graphShowFn(cmd.OutOrStdout(), repo, format, showAll)
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
				return newUsageError(cmd, err)
			}
			repo, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}
			if err := validateRepoPath(repo); err != nil {
				return err
			}
			return inspectShowFn(cmd.OutOrStdout(), repo, args[0], format)
		},
	}
	configureCommand(cmd)

	cmd.ValidArgsFunction = completeTaskNames(repoFlag, queue.AllDirs)
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newRetryCmd(repoFlag *string) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "retry <task-ref> [task-ref...]",
		Short: "Requeue failed tasks back to backlog",
		Args:  usageMinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
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

			type retryItemResult struct {
				Task              string   `json:"task"`
				Requeued          bool     `json:"requeued"`
				Error             string   `json:"error,omitempty"`
				DependencyBlocked bool     `json:"dependency_blocked,omitempty"`
				Warnings          []string `json:"warnings,omitempty"`
			}

			var items []retryItemResult
			var firstErr error
			for _, name := range args {
				result, err := queue.RetryTask(tasksDir, name)
				if err != nil {
					if format == "json" {
						items = append(items, retryItemResult{
							Task:  strings.TrimSuffix(name, ".md"),
							Error: err.Error(),
						})
					} else {
						fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
					}
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				stem := strings.TrimSuffix(result.Filename, ".md")
				if format == "json" {
					items = append(items, retryItemResult{
						Task:              stem,
						Requeued:          true,
						DependencyBlocked: result.DependencyBlocked,
						Warnings:          result.Warnings,
					})
				} else {
					fmt.Printf("Requeued %s to backlog\n", stem)
					for _, w := range result.Warnings {
						ui.Warnf("warning: %s\n", w)
					}
				}
			}
			if format == "json" {
				if err := writeJSON(cmd.OutOrStdout(), items); err != nil {
					return err
				}
			}
			if firstErr != nil {
				return &SilentError{Err: firstErr, Code: 1}
			}
			return nil
		},
	}
	configureCommand(cmd)

	cmd.ValidArgsFunction = completeTaskNames(repoFlag, []string{queue.DirFailed})
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newPauseCmd(repoFlag *string) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Pause new task claims and review launches",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
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
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), result)
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

	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newResumeCmd(repoFlag *string) *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume task claims and review launches",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
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
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), result)
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

	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func newCancelCmd(repoFlag *string) *cobra.Command {
	var format string
	var yes bool

	cmd := &cobra.Command{
		Use:   "cancel <task-ref> [task-ref...]",
		Short: "Withdraw tasks from the queue by moving them to failed/",
		Args:  usageMinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
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

			// Interactive confirmation when stdin is a TTY,
			// --yes is not set, and output is not JSON.
			if !yes && format != "json" && stdinIsTerminalFn() {
				idx := queue.BuildIndex(tasksDir)
				type taskInfo struct {
					stem  string
					state string
					agent string
				}
				var resolved []taskInfo
				for _, ref := range args {
					match, err := queue.ResolveTask(idx, ref)
					if err != nil {
						// Silently skip unresolved refs during prompt
						// preparation so errors are only reported once
						// by the cancel loop after confirmation.
						continue
					}
					stem := strings.TrimSuffix(match.Filename, ".md")
					agent := ""
					if match.Snapshot != nil {
						agent = match.Snapshot.ClaimedBy
					}
					resolved = append(resolved, taskInfo{stem: stem, state: match.State, agent: agent})
				}

				if len(resolved) > 0 {
					fmt.Println("The following tasks will be cancelled:")
					for _, ti := range resolved {
						if ti.agent != "" {
							fmt.Printf("  %s (%s, agent %s)\n", ti.stem, ti.state, ti.agent)
						} else {
							fmt.Printf("  %s (%s)\n", ti.stem, ti.state)
						}
					}
					fmt.Println()
					fmt.Printf("Cancel %d task(s)? [y/N]: ", len(resolved))
					if !confirmCancelFn(os.Stdin) {
						fmt.Println("Cancelled. No tasks were modified.")
						return nil
					}
				}
			}

			type cancelItemResult struct {
				Task       string   `json:"task"`
				Cancelled  bool     `json:"cancelled"`
				PriorState string   `json:"prior_state,omitempty"`
				Warnings   []string `json:"warnings,omitempty"`
				Error      string   `json:"error,omitempty"`
			}

			var items []cancelItemResult
			var firstErr error
			for _, ref := range args {
				result, err := cancelTaskFn(tasksDir, ref)
				if err != nil {
					if format == "json" {
						items = append(items, cancelItemResult{
							Task:  strings.TrimSuffix(ref, ".md"),
							Error: err.Error(),
						})
					} else {
						fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
					}
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				stem := strings.TrimSuffix(result.Filename, ".md")
				if format == "json" {
					items = append(items, cancelItemResult{
						Task:       stem,
						Cancelled:  true,
						PriorState: result.PriorState,
						Warnings:   result.Warnings,
					})
				} else {
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
			}
			if format == "json" {
				if err := writeJSON(cmd.OutOrStdout(), items); err != nil {
					return err
				}
			}
			if firstErr != nil {
				return &SilentError{Err: firstErr, Code: 1}
			}
			return nil
		},
	}
	configureCommand(cmd)

	cancelDirs := []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirFailed}
	cmd.ValidArgsFunction = completeTaskNames(repoFlag, cancelDirs)
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// completeTaskNames returns a cobra ValidArgsFunction that completes task
// names from the given queue directories. Both filename stems and explicit
// frontmatter IDs are offered for successfully parsed tasks. For parse-failure
// entries, the filename stem and full filename are offered as completions
// since those are the valid refs for resolution (frontmatter IDs are omitted
// because malformed files may not have trustworthy metadata).
func completeTaskNames(repoFlag *string, queueDirs []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		repo, err := resolveRepo(*repoFlag)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		repoRoot, err := resolveRepoRoot(repo)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		tasksDir := filepath.Join(repoRoot, dirs.Root)
		if _, err := os.Stat(tasksDir); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		idx := queue.BuildIndex(tasksDir)

		dirSet := make(map[string]struct{}, len(queueDirs))
		for _, d := range queueDirs {
			dirSet[d] = struct{}{}
		}

		seen := make(map[string]struct{})
		var completions []string
		for _, dir := range queueDirs {
			for _, snap := range idx.TasksByState(dir) {
				stem := frontmatter.TaskFileStem(snap.Filename)
				if _, ok := seen[stem]; !ok && strings.HasPrefix(stem, toComplete) {
					seen[stem] = struct{}{}
					completions = append(completions, stem)
				}
				if id := snap.Meta.ID; id != "" && id != stem {
					if _, ok := seen[id]; !ok && strings.HasPrefix(id, toComplete) {
						seen[id] = struct{}{}
						completions = append(completions, id)
					}
				}
			}
		}
		for _, pf := range idx.ParseFailures() {
			if _, ok := dirSet[pf.State]; !ok {
				continue
			}
			stem := frontmatter.TaskFileStem(pf.Filename)
			if _, ok := seen[stem]; !ok && strings.HasPrefix(stem, toComplete) {
				seen[stem] = struct{}{}
				completions = append(completions, stem)
			}
			if _, ok := seen[pf.Filename]; !ok && strings.HasPrefix(pf.Filename, toComplete) {
				seen[pf.Filename] = struct{}{}
				completions = append(completions, pf.Filename)
			}
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}
}

// confirmCancel reads a line from r and returns true if the user confirmed
// with y, Y, yes, or YES.
func confirmCancel(r io.Reader) bool {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(scanner.Text())
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// writeJSON encodes v as indented JSON to w.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newVersionCmd() *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print mato version",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"version": version})
			}
			return printVersion(cmd.OutOrStdout())
		},
	}
	configureCommand(cmd)
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	return cmd
}
