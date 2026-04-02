package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

// UsageError marks command-line misuse that should print the command usage.
type UsageError struct {
	Err   error
	Usage string
}

func (e *UsageError) Error() string {
	return e.Err.Error()
}

func (e *UsageError) Unwrap() error {
	return e.Err
}

// SilentError carries a non-zero exit code for failures that have already been
// reported to the user and should not be printed again by main.
type SilentError struct {
	Err  error
	Code int
}

func (e *SilentError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *SilentError) Unwrap() error {
	return e.Err
}

// ExitError carries a non-zero exit code without printing "mato error:".
type ExitError struct {
	Code int
}

func (e ExitError) Error() string {
	return fmt.Sprintf("exit %d", e.Code)
}

func newUsageError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err, Usage: cmd.UsageString()}
}

func usageNoArgs(cmd *cobra.Command, args []string) error {
	return newUsageError(cmd, cobra.NoArgs(cmd, args))
}

func usageExactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return newUsageError(cmd, cobra.ExactArgs(n)(cmd, args))
	}
}

func usageMinimumNArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return newUsageError(cmd, cobra.MinimumNArgs(n)(cmd, args))
	}
}

func configureCommand(cmd *cobra.Command) {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return newUsageError(c, err)
	})
}

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "mato %s\n", version)
	return err
}

func writeCommandError(w io.Writer, err error) int {
	var exitErr ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}

	var silentErr *SilentError
	if errors.As(err, &silentErr) {
		return silentErr.Code
	}

	var usageErr *UsageError
	if errors.As(err, &usageErr) {
		fmt.Fprintf(w, "mato error: %v\n\n", usageErr.Err)
		_, _ = io.WriteString(w, usageErr.Usage)
		return 1
	}

	fmt.Fprintf(w, "mato error: %v\n", err)
	return 1
}

func newRootCmd() *cobra.Command {
	var repoFlag string

	root := &cobra.Command{
		Use:     "mato",
		Short:   "Orchestrate autonomous Copilot agents against a task queue",
		Long:    "Mato orchestrates autonomous Copilot agents against a filesystem-backed task queue in Docker.",
		Example: "mato run\nmato status\nmato version",
		Args:    usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	configureCommand(root)
	root.Version = version
	root.SetVersionTemplate("mato {{.Version}}\n")
	root.PersistentFlags().StringVar(&repoFlag, "repo", "", "Path to the git repository (default: current directory)")

	root.AddCommand(newRunCmd(&repoFlag))
	root.AddCommand(newStatusCmd(&repoFlag))
	root.AddCommand(newLogCmd(&repoFlag))
	root.AddCommand(newDoctorCmd(&repoFlag))
	root.AddCommand(newGraphCmd(&repoFlag))
	root.AddCommand(newInitCmd(&repoFlag))
	root.AddCommand(newInspectCmd(&repoFlag))
	root.AddCommand(newCancelCmd(&repoFlag))
	root.AddCommand(newRetryCmd(&repoFlag))
	root.AddCommand(newPauseCmd(&repoFlag))
	root.AddCommand(newResumeCmd(&repoFlag))
	root.AddCommand(newVersionCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(writeCommandError(os.Stderr, err))
	}
}
