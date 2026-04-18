package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/queue"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

var retryTaskFn = queue.RetryTask

type retryItemResult struct {
	Task              string   `json:"task"`
	Requeued          bool     `json:"requeued"`
	Error             string   `json:"error,omitempty"`
	DependencyBlocked bool     `json:"dependency_blocked,omitempty"`
	Warnings          []string `json:"warnings,omitempty"`
}

func newRetryCmd(repoFlag *string) *cobra.Command {
	var format string
	var retryAll bool

	cmd := &cobra.Command{
		Use:   "retry [--all | <task-ref> [task-ref...]]",
		Short: "Requeue failed tasks back to backlog",
		Long: "Requeue failed tasks back to backlog.\n\n" +
			"`mato retry --all` retries every task currently in failed/.",
		Example: "mato retry fix-login-bug\n" +
			"mato retry fix-login-bug add-dark-mode\n" +
			"mato retry --all\n" +
			"mato retry --all --format=json",
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case retryAll && len(args) > 0:
				return newUsageError(cmd, fmt.Errorf("--all cannot be used with explicit task refs"))
			case !retryAll:
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

			taskRefs := args
			if retryAll {
				taskRefs = listRetryAllTaskRefs(tasksDir)
			}

			items := make([]retryItemResult, 0, len(taskRefs))
			var firstErr error
			for _, name := range taskRefs {
				result, err := retryTaskFn(tasksDir, name)
				if err != nil {
					if format == "json" {
						items = append(items, retryItemResult{
							Task:  strings.TrimSuffix(name, ".md"),
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
				if err := writeRetryResult(out, errOut, format, &items, result); err != nil {
					return err
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

	cmd.ValidArgsFunction = completeTaskNames(repoFlag, []string{dirs.Failed})
	cmd.Flags().BoolVar(&retryAll, "all", false, "Retry every task currently in failed/")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func listRetryAllTaskRefs(tasksDir string) []string {
	idx := queue.BuildIndex(tasksDir)
	refs := make([]string, 0)
	seen := make(map[string]struct{})

	for _, snap := range idx.TasksByState(dirs.Failed) {
		if _, ok := seen[snap.Filename]; ok {
			continue
		}
		seen[snap.Filename] = struct{}{}
		refs = append(refs, snap.Filename)
	}
	for _, pf := range idx.ParseFailures() {
		if pf.State != dirs.Failed {
			continue
		}
		if _, ok := seen[pf.Filename]; ok {
			continue
		}
		seen[pf.Filename] = struct{}{}
		refs = append(refs, pf.Filename)
	}

	sort.Strings(refs)
	return refs
}

func writeRetryResult(out, errOut io.Writer, format string, items *[]retryItemResult, result queue.RetryResult) error {
	stem := strings.TrimSuffix(result.Filename, ".md")
	if format == "json" {
		*items = append(*items, retryItemResult{
			Task:              stem,
			Requeued:          true,
			DependencyBlocked: result.DependencyBlocked,
			Warnings:          result.Warnings,
		})
		return nil
	}
	if err := writef(out, "Requeued %s to backlog\n", stem); err != nil {
		return err
	}
	for _, warning := range result.Warnings {
		if err := ui.WarnTo(errOut, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}
