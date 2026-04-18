package main

import (
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

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
