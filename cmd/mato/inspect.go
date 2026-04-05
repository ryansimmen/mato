package main

import (
	"mato/internal/inspect"
	"mato/internal/queue"
	"mato/internal/ui"

	"github.com/spf13/cobra"
)

// inspectShowFn is the function used to render task inspection results.
// Tests replace it to verify CLI flag parsing, delegation, and writer errors.
var inspectShowFn = inspect.ShowTo

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
