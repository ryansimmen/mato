package main

import (
	"io"

	"mato/internal/configresolve"
	"mato/internal/ui"

	"github.com/spf13/cobra"
)

// configShowFn is the function used to render resolved repository config.
// Tests replace it to verify CLI flag parsing and delegation.
var configShowFn = showConfig

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
	if err := validateResolvedBranch(resolved.Branch); err != nil {
		return err
	}
	if format == "json" {
		return configresolve.RenderJSON(w, resolved)
	}
	return configresolve.RenderText(w, resolved)
}
