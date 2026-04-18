package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/ryansimmen/mato/internal/status"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

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
					return newUsageError(cmd, ui.WithHint(fmt.Errorf("--interval must be a positive duration, got %s", interval), "pass --interval a positive duration like 2s, 30s, or 1m"))
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
