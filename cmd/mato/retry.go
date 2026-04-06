package main

import (
	"path/filepath"
	"strings"

	"mato/internal/dirs"
	"mato/internal/queue"
	"mato/internal/ui"

	"github.com/spf13/cobra"
)

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
			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

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
						if err := writef(errOut, "mato error: %v\n", err); err != nil {
							return err
						}
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
					if err := writef(out, "Requeued %s to backlog\n", stem); err != nil {
						return err
					}
					for _, warning := range result.Warnings {
						if err := ui.WarnTo(errOut, "warning: %s\n", warning); err != nil {
							return err
						}
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

	cmd.ValidArgsFunction = completeTaskNames(repoFlag, []string{dirs.Failed})
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}
