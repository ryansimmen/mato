package main

import (
	"fmt"
	"strings"

	"mato/internal/config"
	"mato/internal/configresolve"
	"mato/internal/runner"
	"mato/internal/ui"

	"github.com/spf13/cobra"
)

// runFn is the function used to start the orchestrator loop. Defaults to
// runner.Run and can be replaced in tests to observe resolved values.
var runFn = runner.Run

// dryRunFn is the function used for dry-run validation. Defaults to
// runner.DryRun and can be replaced in tests.
var dryRunFn = runner.DryRun

func newRunCmd(repoFlag *string) *cobra.Command {
	var branch string
	var dryRun bool
	var once bool
	var untilIdle bool
	var verbose bool
	var flags configresolve.RunFlags

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the orchestrator loop",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun && once {
				return newUsageError(cmd, fmt.Errorf("--dry-run and --once are mutually exclusive"))
			}
			if dryRun && untilIdle {
				return newUsageError(cmd, fmt.Errorf("--dry-run and --until-idle are mutually exclusive"))
			}
			if once && untilIdle {
				return newUsageError(cmd, fmt.Errorf("--once and --until-idle are mutually exclusive"))
			}
			if branch != "" && strings.TrimSpace(branch) == "" {
				return newUsageError(cmd, ui.WithHint(fmt.Errorf("--branch must not be whitespace-only"), "pass --branch a valid git ref name such as mato or feature/my-change"))
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
			resolvedBranch, err := configresolve.ResolveBranch(load, branch)
			if err != nil {
				return err
			}
			if err := validateResolvedBranch(resolvedBranch); err != nil {
				return err
			}
			runCfg, err := configresolve.ResolveRunConfig(flags, load)
			if err != nil {
				return err
			}
			opts := runOptionsFromResolvedConfig(runCfg)
			opts.Verbose = verbose
			switch {
			case once:
				opts.Mode = runner.RunModeOnce
			case untilIdle:
				opts.Mode = runner.RunModeUntilIdle
			default:
				opts.Mode = runner.RunModeDaemon
			}
			if dryRun {
				return dryRunFn(cmd.OutOrStdout(), repoRoot, resolvedBranch.Value, opts)
			}
			return runFn(repoRoot, resolvedBranch.Value, opts)
		},
	}
	configureCommand(cmd)
	cmd.Flags().StringVar(&branch, "branch", "", "Target branch for merging (default: "+config.DefaultBranch+")")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate queue setup without launching Docker containers")
	cmd.Flags().BoolVar(&once, "once", false, "Run exactly one poll iteration, then exit")
	cmd.Flags().BoolVar(&untilIdle, "until-idle", false, "Keep polling until no actionable work remains, then exit")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "Write operator diagnostics to stderr")
	cmd.Flags().StringVar(&flags.TaskModel, "task-model", "", "Copilot model for task agents (default: "+config.DefaultTaskModel+")")
	cmd.Flags().StringVar(&flags.ReviewModel, "review-model", "", "Copilot model for review agents (default: "+config.DefaultReviewModel+")")
	cmd.Flags().StringVar(&flags.TaskReasoningEffort, "task-reasoning-effort", "", "Reasoning effort for task agents (default: "+config.DefaultReasoningEffort+")")
	cmd.Flags().StringVar(&flags.ReviewReasoningEffort, "review-reasoning-effort", "", "Reasoning effort for review agents (default: "+config.DefaultReasoningEffort+")")
	return cmd
}

func runOptionsFromResolvedConfig(runCfg configresolve.RunConfig) runner.RunOptions {
	return runner.RunOptions{
		DockerImage:                runCfg.DockerImage.Value,
		TaskModel:                  runCfg.TaskModel.Value,
		ReviewModel:                runCfg.ReviewModel.Value,
		ReviewSessionResumeEnabled: runCfg.ReviewSessionResumeEnabled.Value,
		TaskReasoningEffort:        runCfg.TaskReasoningEffort.Value,
		ReviewReasoningEffort:      runCfg.ReviewReasoningEffort.Value,
		AgentTimeout:               runCfg.AgentTimeout.Value,
		RetryCooldown:              runCfg.RetryCooldown.Value,
		Verbose:                    false,
	}
}
