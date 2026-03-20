package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"mato/internal/runner"
	"mato/internal/status"

	"github.com/spf13/cobra"
)

// extractKnownFlags separates mato's own flags from arguments that should be
// forwarded to the copilot CLI inside the Docker container. The root command
// uses DisableFlagParsing so that unknown flags (like --model) are not rejected
// by cobra and can be passed through.
func extractKnownFlags(args []string) (repo, branch, tasksDir string, dryRun bool, copilotArgs []string, err error) {
	copilotArgs = make([]string, 0, len(args))
	known := map[string]bool{"--repo": true, "--branch": true, "--tasks-dir": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			copilotArgs = append(copilotArgs, args[i+1:]...)
			break
		}
		if arg == "--dry-run" {
			dryRun = true
			continue
		}
		if strings.HasPrefix(arg, "--dry-run=") {
			val := strings.TrimPrefix(arg, "--dry-run=")
			b, parseErr := strconv.ParseBool(val)
			if parseErr != nil {
				err = fmt.Errorf("invalid value %q for flag --dry-run: must be a boolean", val)
				return
			}
			dryRun = b
			continue
		}
		// --flag=value form
		handled := false
		for flag := range known {
			if strings.HasPrefix(arg, flag+"=") {
				val := strings.TrimSpace(strings.TrimPrefix(arg, flag+"="))
				if val == "" {
					err = fmt.Errorf("flag %s requires a value", flag)
					return
				}
				switch flag {
				case "--repo":
					repo = val
				case "--branch":
					branch = val
				case "--tasks-dir":
					tasksDir = val
				}
				handled = true
				break
			}
		}
		if handled {
			continue
		}
		// --flag value form
		if known[arg] {
			if i+1 >= len(args) {
				err = fmt.Errorf("flag %s requires a value", arg)
				return
			}
			next := args[i+1]
			if strings.HasPrefix(next, "--") {
				err = fmt.Errorf("flag %s requires a value, got flag %s", arg, next)
				return
			}
			i++
			val := strings.TrimSpace(next)
			switch arg {
			case "--repo":
				repo = val
			case "--branch":
				branch = val
			case "--tasks-dir":
				tasksDir = val
			}
			continue
		}
		copilotArgs = append(copilotArgs, arg)
	}
	return
}

func resolveRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return wd, nil
}

func resolveBranch(b string) string {
	if b != "" {
		return b
	}
	return "mato"
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "mato [copilot-args...]",
		Short: "Runs autonomous Copilot agents against a task queue in Docker",
		Long: `Runs autonomous Copilot agents against a task queue in Docker.

Any unrecognized flags are forwarded to the copilot CLI inside the container.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "--" {
					break
				}
				if a == "--help" || a == "-h" {
					cmd.DisableFlagParsing = false
					return cmd.Help()
				}
			}
			repo, branch, tasksDir, dryRun, copilotArgs, err := extractKnownFlags(args)
			if err != nil {
				return err
			}
			resolved, err := resolveRepo(repo)
			if err != nil {
				return err
			}
			br := resolveBranch(branch)
			if dryRun {
				return runner.DryRun(resolved, br, tasksDir)
			}
			return runner.Run(resolved, br, tasksDir, copilotArgs)
		},
	}

	// Flags are defined for help/documentation only; actual parsing is manual
	// because DisableFlagParsing is true (required for copilot arg forwarding).
	root.Flags().String("repo", "", "Path to the git repository (default: current directory)")
	root.Flags().String("branch", "", "Target branch for merging (default: mato)")
	root.Flags().String("tasks-dir", "", "Path to the tasks directory (default: <repo>/.tasks)")
	root.Flags().Bool("dry-run", false, "Validate queue setup without launching Docker containers")

	root.AddCommand(newStatusCmd())
	return root
}

func newStatusCmd() *cobra.Command {
	var statusRepo string
	var statusTasksDir string
	var watch bool
	var interval time.Duration

	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Show the current state of the task queue",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := resolveRepo(statusRepo)
			if err != nil {
				return err
			}
			if watch {
				ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
				defer stop()
				return status.Watch(ctx, repo, statusTasksDir, interval)
			}
			return status.Show(repo, statusTasksDir)
		},
	}

	cmd.Flags().StringVar(&statusRepo, "repo", "", "Path to the git repository (default: current directory)")
	cmd.Flags().StringVar(&statusTasksDir, "tasks-dir", "", "Path to the tasks directory (default: <repo>/.tasks)")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Continuously refresh the status display")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Refresh interval for watch mode")

	return cmd
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
		os.Exit(1)
	}
}
