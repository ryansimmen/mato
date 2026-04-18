package main

import (
	"fmt"

	"github.com/ryansimmen/mato/internal/history"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

// logShowFn is the function used to render durable task history.
// Tests replace it to verify CLI flag parsing and delegation.
var logShowFn = history.ShowTo

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
