package main

import (
	"path/filepath"
	"time"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/pause"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

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
			out := cmd.OutOrStdout()
			since := result.Since.Format(time.RFC3339)
			switch {
			case result.AlreadyPaused:
				return writef(out, "Already paused since %s\n", since)
			case result.Repaired:
				return writef(out, "Repaired pause sentinel. Paused since %s\n", since)
			default:
				return writef(out, "Paused since %s\n", since)
			}
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
			out := cmd.OutOrStdout()
			if result.WasActive {
				return writeln(out, "Resumed")
			}
			return writeln(out, "Not paused")
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}
