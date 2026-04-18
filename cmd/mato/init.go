package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ryansimmen/mato/internal/config"
	"github.com/ryansimmen/mato/internal/configresolve"
	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/git"
	"github.com/ryansimmen/mato/internal/setup"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

func newInitCmd(repoFlag *string) *cobra.Command {
	var initBranch string
	var format string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a repository for mato use",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}
			if initBranch != "" && strings.TrimSpace(initBranch) == "" {
				return newUsageError(cmd, ui.WithHint(fmt.Errorf("--branch must not be whitespace-only"), "pass --branch a valid git ref name such as mato or feature/my-change"))
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
			load, err := config.Load(repoRoot)
			if err != nil {
				return err
			}
			branch, err := configresolve.ResolveBranch(load, initBranch)
			if err != nil {
				return err
			}
			if err := validateResolvedBranch(branch); err != nil {
				return err
			}

			result, err := setup.InitRepo(repoRoot, branch.Value)
			if err != nil {
				return err
			}
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), result)
			}
			return printInitResult(cmd.OutOrStdout(), result)
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&initBranch, "branch", "", "Target branch name (default: "+config.DefaultBranch+")")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")

	return cmd
}

func printInitResult(w io.Writer, result *setup.InitResult) error {
	if len(result.DirsCreated) > 0 {
		for _, rel := range result.DirsCreated {
			if err := writef(w, "Created %s/\n", rel); err != nil {
				return err
			}
		}
	} else {
		if err := writef(w, "%s/ directory structure already exists\n", dirs.Root); err != nil {
			return err
		}
	}

	if result.GitignoreUpdated {
		if err := writef(w, "Added %s to .gitignore\n", result.IgnorePattern); err != nil {
			return err
		}
	} else {
		if err := writef(w, ".gitignore already contains %s\n", result.IgnorePattern); err != nil {
			return err
		}
	}

	switch {
	case result.AlreadyOnBranch:
		if err := writef(w, "Already on branch: %s (%s)\n", result.BranchName, branchSourceDescription(result)); err != nil {
			return err
		}
	case result.LocalBranchExisted || result.BranchSource == git.BranchSourceRemote || result.BranchSource == git.BranchSourceRemoteCached:
		if err := writef(w, "Switched to branch: %s (%s)\n", result.BranchName, branchSourceDescription(result)); err != nil {
			return err
		}
	default:
		if err := writef(w, "Created branch: %s from %s\n", result.BranchName, branchSourceDescription(result)); err != nil {
			return err
		}
	}

	if len(result.DirsCreated) == 0 && !result.GitignoreUpdated && result.AlreadyOnBranch {
		return writeln(w, "Nothing to do - already initialized.")
	}
	return writef(w, "Ready to add tasks to %s\n", filepath.Join(result.TasksDir, "backlog")+string(filepath.Separator))
}

func branchSourceDescription(result *setup.InitResult) string {
	return git.DescribeBranchSource(result.BranchName, result.BranchSource)
}
