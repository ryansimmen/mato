package main

import (
	"github.com/ryansimmen/mato/internal/graph"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

// graphShowFn is the function used to render dependency graphs.
// Tests replace it to verify CLI flag parsing, delegation, and writer errors.
var graphShowFn = graph.ShowTo

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
