package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"mato/internal/doctor"

	"github.com/spf13/cobra"
)

func TestResolveRepo(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	tests := []struct {
		name     string
		input    string
		wantRepo string
	}{
		{"explicit path returned as-is", "/tmp/repo", "/tmp/repo"},
		{"empty defaults to cwd", "", wd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveRepo(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.wantRepo {
				t.Errorf("resolveRepo(%q) = %q, want %q", tt.input, got, tt.wantRepo)
			}
		})
	}
}

func TestResolveBranch(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"explicit branch", "develop", "develop"},
		{"empty defaults to mato", "", "mato"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBranch(tt.input)
			if got != tt.want {
				t.Errorf("resolveBranch(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestExtractKnownFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantRepo   string
		wantBranch string
		wantTasks  string
		wantDryRun bool
		wantExtra  []string
	}{
		{
			name:      "repo equals syntax",
			args:      []string{"--repo=/tmp/repo"},
			wantRepo:  "/tmp/repo",
			wantExtra: []string{},
		},
		{
			name:      "repo space syntax",
			args:      []string{"--repo", "/tmp/repo"},
			wantRepo:  "/tmp/repo",
			wantExtra: []string{},
		},
		{
			name:       "branch equals syntax",
			args:       []string{"--branch=develop"},
			wantBranch: "develop",
			wantExtra:  []string{},
		},
		{
			name:       "branch space syntax",
			args:       []string{"--branch", "develop"},
			wantBranch: "develop",
			wantExtra:  []string{},
		},
		{
			name:      "tasks-dir equals syntax",
			args:      []string{"--tasks-dir=/custom/tasks"},
			wantTasks: "/custom/tasks",
			wantExtra: []string{},
		},
		{
			name:      "tasks-dir space syntax",
			args:      []string{"--tasks-dir", "/custom/tasks"},
			wantTasks: "/custom/tasks",
			wantExtra: []string{},
		},
		{
			name:       "dry-run flag",
			args:       []string{"--dry-run"},
			wantDryRun: true,
			wantExtra:  []string{},
		},
		{
			name:       "dry-run equals true",
			args:       []string{"--dry-run=true"},
			wantDryRun: true,
			wantExtra:  []string{},
		},
		{
			name:       "dry-run equals false",
			args:       []string{"--dry-run=false"},
			wantDryRun: false,
			wantExtra:  []string{},
		},
		{
			name:       "dry-run equals 1",
			args:       []string{"--dry-run=1"},
			wantDryRun: true,
			wantExtra:  []string{},
		},
		{
			name:       "dry-run equals 0",
			args:       []string{"--dry-run=0"},
			wantDryRun: false,
			wantExtra:  []string{},
		},
		{
			name:       "dry-run=true with forwarded flags",
			args:       []string{"--dry-run=true", "--model", "gpt-5"},
			wantDryRun: true,
			wantExtra:  []string{"--model", "gpt-5"},
		},
		{
			name:       "dry-run=false with other known flags",
			args:       []string{"--repo=/tmp/repo", "--dry-run=false"},
			wantRepo:   "/tmp/repo",
			wantDryRun: false,
			wantExtra:  []string{},
		},
		{
			name:       "all flags combined",
			args:       []string{"--repo=/tmp/repo", "--branch=develop", "--tasks-dir=/tasks", "--dry-run"},
			wantRepo:   "/tmp/repo",
			wantBranch: "develop",
			wantTasks:  "/tasks",
			wantDryRun: true,
			wantExtra:  []string{},
		},
		{
			name:      "unknown flags forwarded as copilot args",
			args:      []string{"--repo=/tmp/repo", "--model", "gpt-5.2"},
			wantRepo:  "/tmp/repo",
			wantExtra: []string{"--model", "gpt-5.2"},
		},
		{
			name:      "double dash separator",
			args:      []string{"--repo=/tmp/repo", "--", "--model", "gpt-5.2"},
			wantRepo:  "/tmp/repo",
			wantExtra: []string{"--model", "gpt-5.2"},
		},
		{
			name:      "no args",
			args:      []string{},
			wantExtra: []string{},
		},
		{
			name:      "only unknown args",
			args:      []string{"--model", "gpt-5"},
			wantExtra: []string{"--model", "gpt-5"},
		},
		{
			name:      "flag followed by valid non-flag value",
			args:      []string{"--repo", "/tmp/foo", "--model", "gpt-5"},
			wantRepo:  "/tmp/foo",
			wantExtra: []string{"--model", "gpt-5"},
		},
		{
			name:      "equals form accepts flag-like value",
			args:      []string{"--repo=--model"},
			wantRepo:  "--model",
			wantExtra: []string{},
		},
		{
			name:       "values with internal spaces accepted",
			args:       []string{"--repo", "/path/with spaces", "--branch", "my branch"},
			wantRepo:   "/path/with spaces",
			wantBranch: "my branch",
			wantExtra:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := extractKnownFlags(tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", cfg.repo, tt.wantRepo)
			}
			if cfg.branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", cfg.branch, tt.wantBranch)
			}
			if cfg.tasksDir != tt.wantTasks {
				t.Errorf("tasksDir = %q, want %q", cfg.tasksDir, tt.wantTasks)
			}
			if cfg.dryRun != tt.wantDryRun {
				t.Errorf("dryRun = %v, want %v", cfg.dryRun, tt.wantDryRun)
			}
			if len(cfg.copilotArgs) != len(tt.wantExtra) {
				t.Fatalf("extra = %v, want %v", cfg.copilotArgs, tt.wantExtra)
			}
			for i := range cfg.copilotArgs {
				if cfg.copilotArgs[i] != tt.wantExtra[i] {
					t.Errorf("extra[%d] = %q, want %q", i, cfg.copilotArgs[i], tt.wantExtra[i])
				}
			}
		})
	}
}

func TestExtractKnownFlags_MissingValue(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "repo followed by another flag",
			args:    []string{"--repo", "--model", "gpt-5"},
			wantErr: "flag --repo requires a value, got flag --model",
		},
		{
			name:    "branch followed by another flag",
			args:    []string{"--branch", "--tasks-dir", ".tasks"},
			wantErr: "flag --branch requires a value, got flag --tasks-dir",
		},
		{
			name:    "tasks-dir at end of args",
			args:    []string{"--tasks-dir"},
			wantErr: "flag --tasks-dir requires a value",
		},
		{
			name:    "repo at end of args",
			args:    []string{"--repo"},
			wantErr: "flag --repo requires a value",
		},
		{
			name:    "branch at end of args",
			args:    []string{"--branch"},
			wantErr: "flag --branch requires a value",
		},
		{
			name:    "repo equals empty value",
			args:    []string{"--repo="},
			wantErr: "flag --repo requires a value",
		},
		{
			name:    "branch equals empty value",
			args:    []string{"--branch="},
			wantErr: "flag --branch requires a value",
		},
		{
			name:    "tasks-dir equals empty value",
			args:    []string{"--tasks-dir="},
			wantErr: "flag --tasks-dir requires a value",
		},
		{
			name:    "dry-run invalid boolean",
			args:    []string{"--dry-run=maybe"},
			wantErr: `invalid value "maybe" for flag --dry-run: must be a boolean`,
		},
		{
			name:    "dry-run empty equals value",
			args:    []string{"--dry-run="},
			wantErr: `invalid value "" for flag --dry-run: must be a boolean`,
		},
		{
			name:    "repo whitespace-only equals form",
			args:    []string{"--repo=   "},
			wantErr: "flag --repo requires a value",
		},
		{
			name:    "branch whitespace-only equals form",
			args:    []string{"--branch=\t "},
			wantErr: "flag --branch requires a value",
		},
		{
			name:    "tasks-dir whitespace-only equals form",
			args:    []string{"--tasks-dir=  "},
			wantErr: "flag --tasks-dir requires a value",
		},
		{
			name:    "repo whitespace-only space form",
			args:    []string{"--repo", "   "},
			wantErr: "flag --repo requires a value",
		},
		{
			name:    "branch whitespace-only space form",
			args:    []string{"--branch", " \t "},
			wantErr: "flag --branch requires a value",
		},
		{
			name:    "tasks-dir whitespace-only space form",
			args:    []string{"--tasks-dir", "  "},
			wantErr: "flag --tasks-dir requires a value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := extractKnownFlags(tt.args)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestRootCmd_Help(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"help flag", []string{"--help"}},
		{"short help flag", []string{"-h"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			// Help should not error
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRootCmd_HelpAfterDoubleDashForwarded(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantExtra []string
	}{
		{
			name:      "double dash then --help forwarded",
			args:      []string{"--", "--help"},
			wantExtra: []string{"--help"},
		},
		{
			name:      "double dash then -h forwarded",
			args:      []string{"--", "-h"},
			wantExtra: []string{"-h"},
		},
		{
			name:      "known flag then double dash then --help forwarded",
			args:      []string{"--repo=/tmp/repo", "--", "--help"},
			wantExtra: []string{"--help"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedArgs []string
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			cmd.RunE = func(cmd *cobra.Command, args []string) error {
				cfg, _ := extractKnownFlags(args)
				capturedArgs = cfg.copilotArgs
				return nil
			}
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(capturedArgs) != len(tt.wantExtra) {
				t.Fatalf("forwarded args = %v, want %v", capturedArgs, tt.wantExtra)
			}
			for i := range capturedArgs {
				if capturedArgs[i] != tt.wantExtra[i] {
					t.Errorf("forwarded args[%d] = %q, want %q", i, capturedArgs[i], tt.wantExtra[i])
				}
			}
		})
	}
}

func TestRootCmd_UnknownFlagsForwarded(t *testing.T) {
	var capturedArgs []string
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo=/tmp/repo", "--model", "gpt-5.2"})
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, _ := extractKnownFlags(args)
		capturedArgs = cfg.copilotArgs
		return nil
	}
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedArgs) != 2 {
		t.Fatalf("expected 2 forwarded args, got %v", capturedArgs)
	}
	if capturedArgs[0] != "--model" || capturedArgs[1] != "gpt-5.2" {
		t.Errorf("forwarded args = %v, want [--model gpt-5.2]", capturedArgs)
	}
}

func TestStatusCmd_NoExtraArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "extra-arg"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for extra args on status, got nil")
	}
}

func TestStatusCmd_WatchIntervalValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "zero interval rejected",
			args:    []string{"status", "--watch", "--interval=0s"},
			wantErr: "--interval must be a positive duration, got 0s",
		},
		{
			name:    "negative interval rejected",
			args:    []string{"status", "--watch", "--interval=-1s"},
			wantErr: "--interval must be a positive duration, got -1s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestStatusCmd_FormatJSONWithWatchRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "--format=json", "--watch"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format json with --watch, got nil")
	}
	want := "--format json and --watch cannot be used together"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestStatusCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "--format=yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestStatusCmd_WatchPositiveIntervalAccepted(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "--watch", "--interval=1s"})
	// Override status RunE to validate the interval check passes without
	// actually entering the Watch loop.
	for _, sub := range cmd.Commands() {
		if sub.Name() == "status" {
			sub.RunE = func(cmd *cobra.Command, args []string) error {
				// Re-read the watch and interval flags to exercise validation.
				w, _ := cmd.Flags().GetBool("watch")
				iv, _ := cmd.Flags().GetDuration("interval")
				if w && iv <= 0 {
					return fmt.Errorf("--interval must be a positive duration, got %s", iv)
				}
				return nil
			}
		}
	}
	if err := cmd.Execute(); err != nil {
		t.Fatalf("positive interval should be accepted, got: %v", err)
	}
}

func TestStatusCmd_NonWatchIgnoresInterval(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "--interval=0s"})
	// Override status RunE to avoid calling status.Show
	for _, sub := range cmd.Commands() {
		if sub.Name() == "status" {
			sub.RunE = func(cmd *cobra.Command, args []string) error { return nil }
		}
	}
	if err := cmd.Execute(); err != nil {
		t.Fatalf("non-watch status with zero interval should succeed, got: %v", err)
	}
}

func TestStatusCmd_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"status help", []string{"status", "--help"}},
		{"status with repo", []string{"status", "--repo=/tmp/repo"}},
		{"status with tasks-dir", []string{"status", "--tasks-dir=/tmp/tasks"}},
		{"status with repo and tasks-dir", []string{"status", "--repo=/tmp/repo", "--tasks-dir=/tmp/tasks"}},
		{"status with text format", []string{"status", "--format=text"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			// Override status RunE to avoid calling status.Show
			for _, sub := range cmd.Commands() {
				if sub.Name() == "status" {
					sub.RunE = func(cmd *cobra.Command, args []string) error { return nil }
				}
			}
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// --- Doctor subcommand tests ---

func TestExitError_Error(t *testing.T) {
	tests := []struct {
		name string
		code int
		want string
	}{
		{"exit 1", 1, "exit 1"},
		{"exit 2", 2, "exit 2"},
		{"exit 42", 42, "exit 42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ExitError{Code: tt.code}
			if got := e.Error(); got != tt.want {
				t.Errorf("ExitError{%d}.Error() = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestDoctorCmd_InvalidFormat(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--format", "bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format bogus, got nil")
	}
	want := "--format must be text or json, got bogus"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestDoctorCmd_NoExtraArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "extra-arg"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for extra positional args on doctor, got nil")
	}
}

func TestDoctorCmd_OnlyFlagsPassedThrough(t *testing.T) {
	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--only", "git", "--only", "docker,queue"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// cobra's StringSliceVar merges comma-separated and repeated flags.
	want := []string{"git", "docker", "queue"}
	if len(capturedOpts.Only) != len(want) {
		t.Fatalf("Only = %v, want %v", capturedOpts.Only, want)
	}
	for i := range want {
		if capturedOpts.Only[i] != want[i] {
			t.Errorf("Only[%d] = %q, want %q", i, capturedOpts.Only[i], want[i])
		}
	}
}

func TestDoctorCmd_ExitCodeBecomesExitError(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		wantCode int
	}{
		{"warnings produce exit 1", 1, 1},
		{"errors produce exit 2", 2, 2},
	}

	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doctorRunFn = func(_ context.Context, _, _ string, _ doctor.Options) (doctor.Report, error) {
				return doctor.Report{ExitCode: tt.exitCode}, nil
			}

			cmd := newRootCmd()
			cmd.SetArgs([]string{"doctor"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected ExitError, got nil")
			}

			var exitErr ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("expected ExitError, got %T: %v", err, err)
			}
			if exitErr.Code != tt.wantCode {
				t.Errorf("ExitError.Code = %d, want %d", exitErr.Code, tt.wantCode)
			}
		})
	}
}

func TestDoctorCmd_ExitCodeZeroNoError(t *testing.T) {
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _, _ string, _ doctor.Options) (doctor.Report, error) {
		return doctor.Report{ExitCode: 0}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected no error for exit code 0, got: %v", err)
	}
}

func TestDoctorCmd_HardFailurePropagated(t *testing.T) {
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	hardErr := fmt.Errorf("context canceled")
	doctorRunFn = func(_ context.Context, _, _ string, _ doctor.Options) (doctor.Report, error) {
		return doctor.Report{}, hardErr
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected hard failure error, got nil")
	}
	if err != hardErr {
		t.Errorf("error = %v, want %v", err, hardErr)
	}

	// Hard failure should NOT be an ExitError.
	var exitErr ExitError
	if errors.As(err, &exitErr) {
		t.Errorf("hard failure should not be ExitError, got code %d", exitErr.Code)
	}
}

func TestDoctorCmd_FlagParsing(t *testing.T) {
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _, _ string, _ doctor.Options) (doctor.Report, error) {
		return doctor.Report{}, nil
	}

	tests := []struct {
		name string
		args []string
	}{
		{"doctor help", []string{"doctor", "--help"}},
		{"doctor with repo", []string{"doctor", "--repo=/tmp/repo"}},
		{"doctor with tasks-dir", []string{"doctor", "--tasks-dir=/tmp/tasks"}},
		{"doctor with fix", []string{"doctor", "--fix"}},
		{"doctor with json format", []string{"doctor", "--format=json"}},
		{"doctor with text format", []string{"doctor", "--format=text"}},
		{"doctor with all flags", []string{"doctor", "--repo=/tmp/repo", "--tasks-dir=/tmp/tasks", "--fix", "--format=json", "--only=git"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
