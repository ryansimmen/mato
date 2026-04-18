package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/queue"
	"github.com/ryansimmen/mato/internal/queueview"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var cancelTaskFn = queue.CancelTask
var cancelTaskMatchFn = queue.CancelTaskMatch
var listCancellableTasksFn = queue.ListCancellableTasks

type cancelItemResult struct {
	Task       string   `json:"task"`
	Cancelled  bool     `json:"cancelled"`
	PriorState string   `json:"prior_state,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// confirmCancelFn asks the user to confirm cancellation. It receives an
// io.Reader (normally os.Stdin) and returns true if the user confirmed.
// Tests replace it to simulate user input.
var confirmCancelFn = confirmCancel

// stdinIsTerminalFn returns true when stdin is a TTY. Tests replace it
// to exercise the interactive confirmation path.
var stdinIsTerminalFn = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func newCancelCmd(repoFlag *string) *cobra.Command {
	var format string
	var yes bool
	var cancelAll bool

	cmd := &cobra.Command{
		Use:   "cancel [--all | <task-ref> [task-ref...]]",
		Short: "Withdraw tasks from the queue by moving them to failed/",
		Long: "Withdraw tasks from the queue by moving them to failed/.\n\n" +
			"`mato cancel --all` cancels every task currently in waiting/, backlog/, " +
			"in-progress/, ready-for-review/, ready-to-merge/, and failed/. " +
			"It never cancels completed/ tasks.",
		Example: "mato cancel fix-login-bug\n" +
			"mato cancel fix-login-bug add-dark-mode\n" +
			"mato cancel --all",
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case cancelAll && len(args) > 0:
				return newUsageError(cmd, fmt.Errorf("--all cannot be used with explicit task refs"))
			case !cancelAll:
				return usageMinimumNArgs(1)(cmd, args)
			default:
				return nil
			}
		},
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

			var allMatches []queue.TaskMatch
			if cancelAll {
				allMatches = listCancellableTasksFn(tasksDir)
			}

			// Interactive confirmation when stdin is a TTY,
			// --yes is not set, and output is not JSON.
			if !yes && format != "json" && stdinIsTerminalFn() {
				idx := queueview.BuildIndex(tasksDir)
				type taskInfo struct {
					stem  string
					state string
					agent string
				}
				var resolved []taskInfo
				if cancelAll {
					resolved = make([]taskInfo, 0, len(allMatches))
					for _, match := range allMatches {
						resolved = append(resolved, taskInfo{
							stem:  strings.TrimSuffix(match.Filename, ".md"),
							state: match.State,
							agent: cancelTaskAgent(match),
						})
					}
				} else {
					for _, ref := range args {
						match, err := queueview.ResolveTask(idx, ref)
						if err != nil {
							// Silently skip unresolved refs during prompt
							// preparation so errors are only reported once
							// by the cancel loop after confirmation.
							continue
						}
						stem := strings.TrimSuffix(match.Filename, ".md")
						resolved = append(resolved, taskInfo{
							stem:  stem,
							state: match.State,
							agent: cancelTaskAgent(match),
						})
					}
				}

				if len(resolved) > 0 {
					if err := writeln(out, "The following tasks will be cancelled:"); err != nil {
						return err
					}
					for _, ti := range resolved {
						if ti.agent != "" {
							if err := writef(out, "  %s (%s, agent %s)\n", ti.stem, ti.state, ti.agent); err != nil {
								return err
							}
						} else {
							if err := writef(out, "  %s (%s)\n", ti.stem, ti.state); err != nil {
								return err
							}
						}
					}
					if err := writeln(out); err != nil {
						return err
					}
					if err := writef(out, "Cancel %d task(s)? [y/N]: ", len(resolved)); err != nil {
						return err
					}
					if !confirmCancelFn(os.Stdin) {
						return writeln(out, "Cancelled. No tasks were modified.")
					}
				}
			}

			items := make([]cancelItemResult, 0)
			var firstErr error
			if cancelAll {
				for _, match := range allMatches {
					taskName := strings.TrimSuffix(match.Filename, ".md")
					result, err := cancelTaskMatchFn(tasksDir, match)
					if err != nil {
						if format == "json" {
							items = append(items, cancelItemResult{
								Task:  taskName,
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
					if err := writeCancelResult(out, errOut, format, &items, result); err != nil {
						return err
					}
				}
			} else {
				for _, ref := range args {
					result, err := cancelTaskFn(tasksDir, ref)
					if err != nil {
						if format == "json" {
							items = append(items, cancelItemResult{
								Task:  strings.TrimSuffix(ref, ".md"),
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
					if err := writeCancelResult(out, errOut, format, &items, result); err != nil {
						return err
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

	cancelDirs := []string{dirs.Waiting, dirs.Backlog, dirs.InProgress, dirs.ReadyReview, dirs.ReadyMerge, dirs.Failed}
	cmd.ValidArgsFunction = completeTaskNames(repoFlag, cancelDirs)
	cmd.Flags().BoolVar(&cancelAll, "all", false, "Cancel every task in waiting/, backlog/, in-progress/, ready-for-review/, ready-to-merge/, and failed/ (never completed/)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// confirmCancel reads a line from r and returns true if the user confirmed
// with y, Y, yes, or YES.
func confirmCancel(r io.Reader) bool {
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(scanner.Text())
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func cancelTaskAgent(match queue.TaskMatch) string {
	if match.Snapshot != nil {
		return match.Snapshot.ClaimedBy
	}
	if match.ParseFailure != nil {
		return match.ParseFailure.ClaimedBy
	}
	return ""
}

func writeCancelResult(out, errOut io.Writer, format string, items *[]cancelItemResult, result queue.CancelResult) error {
	stem := strings.TrimSuffix(result.Filename, ".md")
	if format == "json" {
		*items = append(*items, cancelItemResult{
			Task:       stem,
			Cancelled:  true,
			PriorState: result.PriorState,
			Warnings:   result.Warnings,
		})
		return nil
	}
	if err := writef(out, "cancelled: %s (was in %s/)\n", result.Filename, result.PriorState); err != nil {
		return err
	}
	if result.PriorState == dirs.InProgress {
		if err := ui.WarnTo(errOut, "warning: agent container for %s may still be running\n", stem); err != nil {
			return err
		}
	}
	if result.PriorState == dirs.ReadyReview {
		if err := ui.WarnTo(errOut, "warning: task is in ready-for-review/ — a review agent may be running\n"); err != nil {
			return err
		}
	}
	if result.PriorState == dirs.ReadyMerge {
		if err := ui.WarnTo(errOut, "warning: merge queue may still merge %s's branch\n", stem); err != nil {
			return err
		}
	}
	if len(result.Warnings) > 0 {
		if err := ui.WarnTo(errOut, "warning: %d task(s) depend on %s:\n", len(result.Warnings), stem); err != nil {
			return err
		}
		for _, warning := range result.Warnings {
			if err := ui.WarnTo(errOut, "  %s\n", warning); err != nil {
				return err
			}
		}
		if err := ui.WarnTo(errOut, "these tasks will remain blocked until %s is retried\n", stem); err != nil {
			return err
		}
	}
	return nil
}
