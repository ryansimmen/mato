package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mato/internal/dirs"
	"mato/internal/queue"
	"mato/internal/ui"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var cancelTaskFn = queue.CancelTask

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

	cmd := &cobra.Command{
		Use:   "cancel <task-ref> [task-ref...]",
		Short: "Withdraw tasks from the queue by moving them to failed/",
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

			// Interactive confirmation when stdin is a TTY,
			// --yes is not set, and output is not JSON.
			if !yes && format != "json" && stdinIsTerminalFn() {
				idx := queue.BuildIndex(tasksDir)
				type taskInfo struct {
					stem  string
					state string
					agent string
				}
				var resolved []taskInfo
				for _, ref := range args {
					match, err := queue.ResolveTask(idx, ref)
					if err != nil {
						// Silently skip unresolved refs during prompt
						// preparation so errors are only reported once
						// by the cancel loop after confirmation.
						continue
					}
					stem := strings.TrimSuffix(match.Filename, ".md")
					agent := ""
					if match.Snapshot != nil {
						agent = match.Snapshot.ClaimedBy
					}
					resolved = append(resolved, taskInfo{stem: stem, state: match.State, agent: agent})
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

			type cancelItemResult struct {
				Task       string   `json:"task"`
				Cancelled  bool     `json:"cancelled"`
				PriorState string   `json:"prior_state,omitempty"`
				Warnings   []string `json:"warnings,omitempty"`
				Error      string   `json:"error,omitempty"`
			}

			var items []cancelItemResult
			var firstErr error
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
				stem := strings.TrimSuffix(result.Filename, ".md")
				if format == "json" {
					items = append(items, cancelItemResult{
						Task:       stem,
						Cancelled:  true,
						PriorState: result.PriorState,
						Warnings:   result.Warnings,
					})
				} else {
					if err := writef(out, "cancelled: %s (was in %s/)\n", result.Filename, result.PriorState); err != nil {
						return err
					}
					if result.PriorState == queue.DirInProgress {
						if err := ui.WarnTo(errOut, "warning: agent container for %s may still be running\n", stem); err != nil {
							return err
						}
					}
					if result.PriorState == queue.DirReadyReview {
						if err := ui.WarnTo(errOut, "warning: task is in ready-for-review/ — a review agent may be running\n"); err != nil {
							return err
						}
					}
					if result.PriorState == queue.DirReadyMerge {
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

	cancelDirs := []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirFailed}
	cmd.ValidArgsFunction = completeTaskNames(repoFlag, cancelDirs)
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
