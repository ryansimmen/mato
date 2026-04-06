package main

import (
	"context"
	"os"
	"os/signal"

	"mato/internal/configresolve"
	"mato/internal/doctor"
	"mato/internal/ui"

	"github.com/spf13/cobra"
)

// doctorRunFn is the function used to run health checks. It defaults to
// doctor.Run and can be replaced in tests to inject failures or exit codes.
var doctorRunFn = doctor.Run

func doctorShouldPreResolveDockerImage(only []string) bool {
	if len(only) == 0 {
		return false
	}
	hasDocker := false
	hasConfig := false
	for _, name := range only {
		if !doctor.IsValidCheckName(name) {
			return false
		}
		switch name {
		case "docker":
			hasDocker = true
		case "config":
			hasConfig = true
		}
	}
	return hasDocker && !hasConfig
}

func newDoctorCmd(repoFlag *string) *cobra.Command {
	var fix bool
	var format string
	var only []string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks on the repository and task queue",
		Args:  usageNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ui.ValidateFormat(format, []string{"text", "json"}); err != nil {
				return newUsageError(cmd, err)
			}

			repoInput, err := resolveRepo(*repoFlag)
			if err != nil {
				return err
			}

			var dockerImage string
			if doctorShouldPreResolveDockerImage(only) {
				repoRoot := ""
				if root, err := resolveRepoRoot(repoInput); err == nil {
					repoRoot = root
				}
				resolvedImage, err := configresolve.ResolveDoctorDockerImage(repoRoot)
				if err != nil {
					return err
				}
				dockerImage = resolvedImage.Value
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			report, err := doctorRunFn(ctx, repoInput, doctor.Options{
				Fix:         fix,
				Format:      format,
				Only:        only,
				DockerImage: dockerImage,
			})
			if err != nil {
				return err // hard failure -> "mato error: ..." + exit 1
			}

			if format == "json" {
				if renderErr := doctor.RenderJSON(cmd.OutOrStdout(), report); renderErr != nil {
					return renderErr
				}
			} else {
				if renderErr := doctor.RenderText(cmd.OutOrStdout(), report); renderErr != nil {
					return renderErr
				}
			}

			if report.ExitCode != 0 {
				return ExitError{Code: report.ExitCode} // health status -> silent exit 1 or 2
			}
			return nil // healthy -> exit 0
		},
	}
	configureCommand(cmd)

	cmd.Flags().BoolVar(&fix, "fix", false, "Auto-repair safe issues (stale locks, orphaned tasks, missing dirs, Docker image pulls, stale events, temp files)")
	cmd.Flags().StringVar(&format, "format", "text", "Output format: text or json")
	cmd.Flags().StringSliceVar(&only, "only", nil, "Run only specified checks (repeatable: git, tools, config, docker, queue, tasks, locks, hygiene, deps)")

	return cmd
}
