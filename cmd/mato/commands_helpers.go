package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mato/internal/dirs"
	"mato/internal/frontmatter"
	"mato/internal/queue"

	"github.com/spf13/cobra"
)

// completeTaskNames returns a cobra ValidArgsFunction that completes task
// names from the given queue directories. Both filename stems and explicit
// frontmatter IDs are offered for successfully parsed tasks. For parse-failure
// entries, the filename stem and full filename are offered as completions
// since those are the valid refs for resolution (frontmatter IDs are omitted
// because malformed files may not have trustworthy metadata).
func completeTaskNames(repoFlag *string, queueDirs []string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		repo, err := resolveRepo(*repoFlag)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		repoRoot, err := resolveRepoRoot(repo)
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		tasksDir := filepath.Join(repoRoot, dirs.Root)
		if _, err := os.Stat(tasksDir); err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		idx := queue.BuildIndex(tasksDir)

		dirSet := make(map[string]struct{}, len(queueDirs))
		for _, d := range queueDirs {
			dirSet[d] = struct{}{}
		}

		seen := make(map[string]struct{})
		var completions []string
		for _, dir := range queueDirs {
			for _, snap := range idx.TasksByState(dir) {
				stem := frontmatter.TaskFileStem(snap.Filename)
				if _, ok := seen[stem]; !ok && strings.HasPrefix(stem, toComplete) {
					seen[stem] = struct{}{}
					completions = append(completions, stem)
				}
				if id := snap.Meta.ID; id != "" && id != stem {
					if _, ok := seen[id]; !ok && strings.HasPrefix(id, toComplete) {
						seen[id] = struct{}{}
						completions = append(completions, id)
					}
				}
			}
		}
		for _, pf := range idx.ParseFailures() {
			if _, ok := dirSet[pf.State]; !ok {
				continue
			}
			stem := frontmatter.TaskFileStem(pf.Filename)
			if _, ok := seen[stem]; !ok && strings.HasPrefix(stem, toComplete) {
				seen[stem] = struct{}{}
				completions = append(completions, stem)
			}
			if _, ok := seen[pf.Filename]; !ok && strings.HasPrefix(pf.Filename, toComplete) {
				seen[pf.Filename] = struct{}{}
				completions = append(completions, pf.Filename)
			}
		}
		return completions, cobra.ShellCompDirectiveNoFileComp
	}
}

// writeJSON encodes v as indented JSON to w.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writef(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

func writeln(w io.Writer, args ...any) error {
	_, err := fmt.Fprintln(w, args...)
	return err
}
