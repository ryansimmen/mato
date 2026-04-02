package main

import (
	"fmt"

	"mato/internal/config"
	"mato/internal/runner"

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
	var flags runFlags

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
			switch {
			case once:
				opts.Mode = runner.RunModeOnce
			case untilIdle:
				opts.Mode = runner.RunModeUntilIdle
			default:
				opts.Mode = runner.RunModeDaemon
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
	cmd.Flags().BoolVar(&once, "once", false, "Run exactly one poll iteration, then exit")
	cmd.Flags().BoolVar(&untilIdle, "until-idle", false, "Keep polling until no actionable work remains, then exit")
	cmd.Flags().StringVar(&flags.TaskModel, "task-model", "", "Copilot model for task agents (default: "+runner.DefaultTaskModel+")")
	cmd.Flags().StringVar(&flags.ReviewModel, "review-model", "", "Copilot model for review agents (default: "+runner.DefaultReviewModel+")")
	cmd.Flags().StringVar(&flags.TaskReasoningEffort, "task-reasoning-effort", "", "Reasoning effort for task agents (default: "+runner.DefaultReasoningEffort+")")
	cmd.Flags().StringVar(&flags.ReviewReasoningEffort, "review-reasoning-effort", "", "Reasoning effort for review agents (default: "+runner.DefaultReasoningEffort+")")
	return cmd
}
