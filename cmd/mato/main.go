package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"mato/internal/runner"
	"mato/internal/status"
)

var errHelp = errors.New("help requested")

func usage() {
	fmt.Fprintf(os.Stderr, `Usage:
  mato [--repo <path>] [--branch <name>] [--tasks-dir <path>] [--dry-run] [copilot-args...]
  mato status [--repo <path>] [--tasks-dir <path>]

Runs autonomous Copilot agents against a task queue in Docker.

Options:
  --repo <path>       Path to the git repository (default: current directory)
  --branch <name>     Target branch for merging (default: mato)
  --tasks-dir <path>  Path to the tasks directory (default: <repo>/.tasks)
  --dry-run           Validate queue setup without launching Docker containers
  --help, -h          Show this help message

Any other flags are forwarded to the copilot CLI inside the container.
`)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "status" {
		repoRoot, _, tasksDir, extras, dryRun, err := parseArgs(os.Args[2:])
		if err == errHelp {
			fmt.Fprintf(os.Stderr, "Usage: mato status [--repo <path>] [--tasks-dir <path>]\n")
			os.Exit(0)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
			os.Exit(1)
		}
		if dryRun {
			fmt.Fprintf(os.Stderr, "mato error: --dry-run is not supported with the status subcommand\n")
			os.Exit(1)
		}
		if len(extras) > 0 {
			fmt.Fprintf(os.Stderr, "mato error: unexpected status arguments: %s\n", strings.Join(extras, " "))
			os.Exit(1)
		}
		if err := status.Show(repoRoot, tasksDir); err != nil {
			fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	repoRoot, branch, tasksDir, copilotArgs, dryRun, err := parseArgs(os.Args[1:])
	if err == errHelp {
		usage()
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
		os.Exit(1)
	}
	if dryRun {
		if err := runner.DryRun(repoRoot, branch, tasksDir); err != nil {
			fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := runner.Run(repoRoot, branch, tasksDir, copilotArgs); err != nil {
		fmt.Fprintf(os.Stderr, "mato error: %v\n", err)
		os.Exit(1)
	}
}

func parseArgs(args []string) (string, string, string, []string, bool, error) {
	var repoRoot string
	var branch string
	var tasksDir string
	var dryRun bool
	copilotArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			copilotArgs = append(copilotArgs, args[i+1:]...)
			break
		}
		if arg == "--help" || arg == "-h" {
			return "", "", "", nil, false, errHelp
		}
		if arg == "--dry-run" {
			dryRun = true
			continue
		}
		if strings.HasPrefix(arg, "--repo=") {
			repoRoot = strings.TrimSpace(strings.TrimPrefix(arg, "--repo="))
			continue
		}
		if arg == "--repo" {
			if i+1 >= len(args) {
				return "", "", "", nil, false, errors.New("--repo requires a value")
			}
			i++
			repoRoot = strings.TrimSpace(args[i])
			continue
		}
		if strings.HasPrefix(arg, "--branch=") {
			branch = strings.TrimSpace(strings.TrimPrefix(arg, "--branch="))
			continue
		}
		if arg == "--branch" {
			if i+1 >= len(args) {
				return "", "", "", nil, false, errors.New("--branch requires a value")
			}
			i++
			branch = strings.TrimSpace(args[i])
			continue
		}
		if strings.HasPrefix(arg, "--tasks-dir=") {
			tasksDir = strings.TrimSpace(strings.TrimPrefix(arg, "--tasks-dir="))
			continue
		}
		if arg == "--tasks-dir" {
			if i+1 >= len(args) {
				return "", "", "", nil, false, errors.New("--tasks-dir requires a value")
			}
			i++
			tasksDir = strings.TrimSpace(args[i])
			continue
		}
		copilotArgs = append(copilotArgs, arg)
	}
	if repoRoot == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", "", "", nil, false, fmt.Errorf("get working directory: %w", err)
		}
		repoRoot = wd
	}
	if branch == "" {
		branch = "mato"
	}
	return repoRoot, branch, tasksDir, copilotArgs, dryRun, nil
}
