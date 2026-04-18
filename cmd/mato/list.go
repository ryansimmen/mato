package main

import (
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/ryansimmen/mato/internal/dirs"
	"github.com/ryansimmen/mato/internal/frontmatter"
	"github.com/ryansimmen/mato/internal/queueview"
	"github.com/ryansimmen/mato/internal/ui"

	"github.com/spf13/cobra"
)

var defaultListStates = []string{
	dirs.Waiting,
	dirs.Backlog,
	dirs.InProgress,
	dirs.ReadyReview,
	dirs.ReadyMerge,
}

var allListStates = append([]string(nil), dirs.All...)

type listTask struct {
	Filename   string `json:"filename"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	Priority   int    `json:"priority"`
	State      string `json:"state"`
	Path       string `json:"path"`
	ParseError string `json:"parse_error,omitempty"`
}

func newListCmd(repoFlag *string) *cobra.Command {
	var format string
	var stateFilter string
	var includeAll bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List queue tasks as a flat snapshot",
		Long: "List queue tasks in a simple text table or as JSON.\n\n" +
			"JSON output is an array of objects with stable fields: filename, id, title, priority, state, and path.",
		Example: "mato list\n" +
			"mato list --state failed\n" +
			"mato list --state backlog,waiting\n" +
			"mato list --all\n" +
			"mato list --format json",
		Args: usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}

			states, err := resolveListStates(stateFilter, includeAll)
			if err != nil {
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

			items := listTasksFromIndex(queueview.BuildIndex(tasksDir), states)
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), items)
			}
			return renderListText(cmd.OutOrStdout(), items)
		},
	}
	configureCommand(cmd)

	cmd.Flags().StringVar(&stateFilter, "state", "", "Comma-separated queue states to include")
	cmd.Flags().BoolVar(&includeAll, "all", false, "Include all queue states, including completed and failed")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	if err := cmd.RegisterFlagCompletionFunc("state", completeListStates); err != nil {
		panic(err)
	}

	return cmd
}

func resolveListStates(stateFilter string, includeAll bool) ([]string, error) {
	if strings.TrimSpace(stateFilter) == "" {
		if includeAll {
			return append([]string(nil), allListStates...), nil
		}
		return append([]string(nil), defaultListStates...), nil
	}

	valid := make(map[string]struct{}, len(allListStates))
	for _, state := range allListStates {
		valid[state] = struct{}{}
	}

	seen := make(map[string]struct{}, len(allListStates))
	var states []string
	for _, raw := range strings.Split(stateFilter, ",") {
		state := strings.TrimSpace(raw)
		if state == "" {
			return nil, ui.WithHint(
				errInvalidListStateFilter,
				"use --state with comma-separated queue states like backlog,failed",
			)
		}
		if _, ok := valid[state]; !ok {
			return nil, ui.WithHint(
				errUnknownListState(state),
				"valid states: "+strings.Join(allListStates, ", "),
			)
		}
		if _, ok := seen[state]; ok {
			continue
		}
		seen[state] = struct{}{}
		states = append(states, state)
	}
	return states, nil
}

func listTasksFromIndex(idx *queueview.PollIndex, states []string) []listTask {
	stateOrder := make(map[string]int, len(states))
	for i, state := range states {
		stateOrder[state] = i
	}

	items := make([]listTask, 0)
	for _, state := range states {
		for _, snap := range idx.TasksByState(state) {
			items = append(items, listTask{
				Filename: snap.Filename,
				ID:       snap.Meta.ID,
				Title:    frontmatter.ExtractTitle(snap.Filename, snap.Body),
				Priority: snap.Meta.Priority,
				State:    snap.State,
				Path:     snap.Path,
			})
		}
	}
	for _, pf := range idx.ParseFailures() {
		if _, ok := stateOrder[pf.State]; !ok {
			continue
		}
		stem := frontmatter.TaskFileStem(pf.Filename)
		items = append(items, listTask{
			Filename:   pf.Filename,
			ID:         stem,
			Title:      stem,
			Priority:   50,
			State:      pf.State,
			Path:       pf.Path,
			ParseError: pf.Err.Error(),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		leftOrder := stateOrder[items[i].State]
		rightOrder := stateOrder[items[j].State]
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		if items[i].Priority != items[j].Priority {
			return items[i].Priority < items[j].Priority
		}
		if items[i].Filename != items[j].Filename {
			return items[i].Filename < items[j].Filename
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		return items[i].Title < items[j].Title
	})

	return items
}

func renderListText(w io.Writer, items []listTask) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if err := writeln(tw, "STATE\tTASK\tPRIORITY\tID\tTITLE"); err != nil {
		return err
	}
	for _, item := range items {
		if err := writef(tw, "%s\t%s\t%d\t%s\t%s\n", item.State, item.Filename, item.Priority, item.ID, item.Title); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func completeListStates(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	selected := make(map[string]struct{}, len(allListStates))
	current := toComplete
	if idx := strings.LastIndex(toComplete, ","); idx >= 0 {
		for _, raw := range strings.Split(toComplete[:idx], ",") {
			state := strings.TrimSpace(raw)
			if state == "" {
				continue
			}
			selected[state] = struct{}{}
		}
		current = strings.TrimSpace(toComplete[idx+1:])
	}

	completions := make([]string, 0, len(allListStates))
	for _, state := range allListStates {
		if _, ok := selected[state]; ok {
			continue
		}
		if strings.HasPrefix(state, current) {
			completions = append(completions, state)
		}
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

type listStateError string

func (e listStateError) Error() string {
	return string(e)
}

var errInvalidListStateFilter = listStateError("--state contains an empty queue state")

func errUnknownListState(state string) error {
	return listStateError("--state contains unknown queue state " + `"` + state + `"`)
}
