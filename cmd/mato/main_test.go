package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mato/internal/config"
	"mato/internal/doctor"
	"mato/internal/git"
	"mato/internal/runner"
	"mato/internal/setup"
	"mato/internal/testutil"

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

func TestResolveEnvBranch(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "")
		branch, ok, err := resolveEnvBranch()
		if err != nil {
			t.Fatalf("resolveEnvBranch: %v", err)
		}
		if ok || branch != "" {
			t.Fatalf("got (%q, %v), want (empty, false)", branch, ok)
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "main")
		branch, ok, err := resolveEnvBranch()
		if err != nil {
			t.Fatalf("resolveEnvBranch: %v", err)
		}
		if !ok || branch != "main" {
			t.Fatalf("got (%q, %v), want (%q, true)", branch, ok, "main")
		}
	})

	t.Run("empty treated as unset", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "")
		branch, ok, err := resolveEnvBranch()
		if err != nil {
			t.Fatalf("resolveEnvBranch: %v", err)
		}
		if ok || branch != "" {
			t.Fatalf("got (%q, %v), want (empty, false)", branch, ok)
		}
	})

	t.Run("whitespace rejected", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "   ")
		_, _, err := resolveEnvBranch()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestExtractKnownFlags(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantRepo   string
		wantBranch string
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
			name:    "repo whitespace-only space form",
			args:    []string{"--repo", "   "},
			wantErr: "flag --repo requires a value",
		},
		{
			name:    "branch whitespace-only space form",
			args:    []string{"--branch", " \t "},
			wantErr: "flag --branch requires a value",
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = origStdout
	}()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read pipe: %v", err)
	}
	return string(data)
}

func writeRepoConfig(t *testing.T, repoRoot, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoRoot, ".mato.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write .mato.yaml: %v", err)
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

func TestInitCmd_CreatesDirectoryStructure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot})

	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("init command failed: %v", err)
		}
	})

	for _, rel := range []string{
		".mato/backlog",
		".mato/waiting",
		".mato/in-progress",
		".mato/ready-for-review",
		".mato/ready-to-merge",
		".mato/completed",
		".mato/failed",
		".mato/.locks",
		".mato/messages/events",
		".mato/messages/presence",
		".mato/messages/completions",
	} {
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(data), "/.mato/") {
		t.Fatalf(".gitignore should contain /.mato/, got %q", string(data))
	}
	branchOut, err := runCmd("git", "-C", repoRoot, "branch", "--show-current")
	if err != nil {
		t.Fatalf("git branch --show-current: %v\n%s", err, branchOut)
	}
	if strings.TrimSpace(branchOut) != "mato" {
		t.Fatalf("current branch = %q, want %q", strings.TrimSpace(branchOut), "mato")
	}
	if !strings.Contains(output, "Ready to add tasks") {
		t.Fatalf("expected ready message in output, got %q", output)
	}
	if !strings.Contains(output, "Created branch: mato from current HEAD (origin unavailable)") {
		t.Fatalf("expected branch source message in output, got %q", output)
	}
}

func TestInitCmd_Idempotent(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("first init failed: %v", err)
	}

	cmd = newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("second init failed: %v", err)
		}
	})
	if !strings.Contains(output, "Nothing to do - already initialized.") {
		t.Fatalf("expected idempotent message, got %q", output)
	}
	if !strings.Contains(output, "Already on branch: mato (existing local branch)") {
		t.Fatalf("expected existing branch message, got %q", output)
	}
}

func TestPrintInitResult_RemoteCachedBranchMessage(t *testing.T) {
	output := captureStdout(t, func() {
		printInitResult(&setup.InitResult{
			BranchName:       "mato",
			BranchSource:     git.BranchSourceRemoteCached,
			IgnorePattern:    "/.mato/",
			TasksDir:         filepath.Join("repo", ".mato"),
			GitignoreUpdated: false,
		})
	})

	if !strings.Contains(output, "Switched to branch: mato (cached origin/mato (origin unavailable))") {
		t.Fatalf("expected cached origin branch message, got %q", output)
	}
}

func TestPrintInitResult_RemoteBranchMessage(t *testing.T) {
	output := captureStdout(t, func() {
		printInitResult(&setup.InitResult{
			BranchName:       "mato",
			BranchSource:     git.BranchSourceRemote,
			IgnorePattern:    "/.mato/",
			TasksDir:         filepath.Join("repo", ".mato"),
			GitignoreUpdated: false,
		})
	})

	if !strings.Contains(output, "Switched to branch: mato (live origin/mato)") {
		t.Fatalf("expected live origin branch message, got %q", output)
	}
}

func TestInitCmd_InvalidRepo(t *testing.T) {
	dir := t.TempDir()
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", dir})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-git repo")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitCmd_InvalidBranch(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot, "--branch", "foo..bar"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected invalid branch error")
	}
	if !strings.Contains(err.Error(), "invalid branch name") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitCmd_NoExtraArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "extra"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for extra positional arg")
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

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
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
			doctorRunFn = func(_ context.Context, _ string, _ doctor.Options) (doctor.Report, error) {
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

	doctorRunFn = func(_ context.Context, _ string, _ doctor.Options) (doctor.Report, error) {
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
	doctorRunFn = func(_ context.Context, _ string, _ doctor.Options) (doctor.Report, error) {
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

	doctorRunFn = func(_ context.Context, _ string, _ doctor.Options) (doctor.Report, error) {
		return doctor.Report{}, nil
	}

	tests := []struct {
		name string
		args []string
	}{
		{"doctor help", []string{"doctor", "--help"}},
		{"doctor with repo", []string{"doctor", "--repo=/tmp/repo"}},
		{"doctor with fix", []string{"doctor", "--fix"}},
		{"doctor with json format", []string{"doctor", "--format=json"}},
		{"doctor with text format", []string{"doctor", "--format=text"}},
		{"doctor with all flags", []string{"doctor", "--repo=/tmp/repo", "--fix", "--format=json", "--only=git"}},
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

func TestDoctorCmd_DockerImageFromConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: custom:latest\n")

	// Ensure the env var is NOT set so config file is used.
	t.Setenv("MATO_DOCKER_IMAGE", "")

	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedOpts.DockerImage != "custom:latest" {
		t.Errorf("DockerImage = %q, want %q", capturedOpts.DockerImage, "custom:latest")
	}
}

func TestDoctorCmd_DockerImageEnvOverridesConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: from-config:1.0\n")

	t.Setenv("MATO_DOCKER_IMAGE", "from-env:2.0")

	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedOpts.DockerImage != "from-env:2.0" {
		t.Errorf("DockerImage = %q, want %q", capturedOpts.DockerImage, "from-env:2.0")
	}
}

func TestDoctorCmd_MalformedConfigReturnsError(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	// Write invalid YAML that config.Load will reject.
	writeRepoConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	t.Setenv("MATO_DOCKER_IMAGE", "")

	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, _ doctor.Options) (doctor.Report, error) {
		t.Fatal("doctorRunFn should not be called when config is malformed")
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed .mato.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "parse config file") {
		t.Errorf("error = %q, want config parse error", err.Error())
	}
}

func TestDoctorCmd_EnvImageBypassesMalformedConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	t.Setenv("MATO_DOCKER_IMAGE", "env-override:3.0")

	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success when env var set, got: %v", err)
	}

	if capturedOpts.DockerImage != "env-override:3.0" {
		t.Errorf("DockerImage = %q, want %q", capturedOpts.DockerImage, "env-override:3.0")
	}
}

func TestDoctorNeedsDockerConfig(t *testing.T) {
	tests := []struct {
		name string
		only []string
		want bool
	}{
		{name: "all checks", only: nil, want: true},
		{name: "explicit docker", only: []string{"queue", "docker"}, want: true},
		{name: "queue only", only: []string{"queue", "tasks", "deps"}, want: false},
		{name: "git only", only: []string{"git"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doctorNeedsDockerConfig(tt.only); got != tt.want {
				t.Errorf("doctorNeedsDockerConfig(%v) = %v, want %v", tt.only, got, tt.want)
			}
		})
	}
}

func TestDoctorCmd_QueueOnlyBypassesMalformedConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	t.Setenv("MATO_DOCKER_IMAGE", "")

	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "queue,tasks,deps"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected queue-only doctor to bypass malformed config, got: %v", err)
	}

	if capturedOpts.DockerImage != "" {
		t.Errorf("DockerImage = %q, want empty for queue-only doctor run", capturedOpts.DockerImage)
	}

	wantOnly := []string{"queue", "tasks", "deps"}
	if len(capturedOpts.Only) != len(wantOnly) {
		t.Fatalf("Only = %v, want %v", capturedOpts.Only, wantOnly)
	}
	for i := range wantOnly {
		if capturedOpts.Only[i] != wantOnly[i] {
			t.Errorf("Only[%d] = %q, want %q", i, capturedOpts.Only[i], wantOnly[i])
		}
	}
}

// --- Graph subcommand tests ---

// runCmd executes an external command and returns its combined output.
func runCmd(args ...string) (string, error) {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	return string(out), err
}

func TestGraphCmd_InvalidFormat(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"graph", "--format", "invalid"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format invalid, got nil")
	}
	want := "--format must be text, dot, or json, got invalid"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestGraphCmd_NoExtraArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"graph", "extra-arg"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for extra positional args on graph, got nil")
	}
}

func TestGraphCmd_Help(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"graph", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error from graph --help: %v", err)
	}
}

func TestGraphCmd_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"graph with repo", []string{"graph", "--repo=/tmp/repo"}},
		{"graph with text format", []string{"graph", "--format=text"}},
		{"graph with dot format", []string{"graph", "--format=dot"}},
		{"graph with json format", []string{"graph", "--format=json"}},
		{"graph with all flag", []string{"graph", "--all"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			// Override graph RunE to avoid hitting real filesystem.
			for _, sub := range cmd.Commands() {
				if sub.Name() == "graph" {
					sub.RunE = func(cmd *cobra.Command, args []string) error { return nil }
				}
			}
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGraphCmd_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	// Initialize a git repo so graph.Show can resolve the repo root.
	for _, args := range [][]string{
		{"git", "init", dir},
		{"git", "-C", dir, "config", "user.name", "test"},
		{"git", "-C", dir, "config", "user.email", "test@test"},
		{"git", "-C", dir, "commit", "--allow-empty", "-m", "init"},
	} {
		out, err := runCmd(args...)
		if err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
	}

	// Create a minimal tasks directory with one task.
	tasksDir := filepath.Join(dir, ".mato")
	backlog := filepath.Join(tasksDir, "backlog")
	if err := os.MkdirAll(backlog, 0o755); err != nil {
		t.Fatalf("mkdir backlog: %v", err)
	}
	task := filepath.Join(backlog, "sample.md")
	if err := os.WriteFile(task, []byte("---\nid: sample\npriority: 10\n---\n# Sample task\n"), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"graph", "--repo", dir, "--format", "text"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("graph end-to-end failed: %v", err)
	}
}

// --- Branch validation tests ---

func TestValidateBranch_Valid(t *testing.T) {
	tests := []struct {
		name   string
		branch string
	}{
		{"simple name", "main"},
		{"with slash", "feature/add-tests"},
		{"default branch", "mato"},
		{"with dots", "release-1.2.3"},
		{"with hyphens", "my-branch"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateBranch(tt.branch); err != nil {
				t.Errorf("validateBranch(%q) returned error: %v", tt.branch, err)
			}
		})
	}
}

func TestValidateBranch_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		branch string
	}{
		{"double dots", "foo..bar"},
		{"ends with dot-lock", "foo.lock"},
		{"contains backslash", "foo\\bar"},
		{"contains space", "foo bar"},
		{"contains tilde", "foo~1"},
		{"contains caret", "foo^"},
		{"contains colon", "foo:bar"},
		{"starts with dash", "-branch"},
		{"contains question mark", "foo?bar"},
		{"contains asterisk", "foo*bar"},
		{"contains open bracket", "foo[bar"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranch(tt.branch)
			if err == nil {
				t.Errorf("validateBranch(%q) expected error, got nil", tt.branch)
			}
			if err != nil && !strings.Contains(err.Error(), "invalid branch name") {
				t.Errorf("validateBranch(%q) error = %q, want error containing 'invalid branch name'", tt.branch, err.Error())
			}
		})
	}
}

// --- Repo path validation tests ---

func TestValidateRepoPath_ValidRepo(t *testing.T) {
	dir := t.TempDir()
	out, err := runCmd("git", "init", dir)
	if err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	if err := validateRepoPath(dir); err != nil {
		t.Errorf("validateRepoPath(%q) returned error: %v", dir, err)
	}
}

func TestValidateRepoPath_NonexistentPath(t *testing.T) {
	err := validateRepoPath("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("expected error for nonexistent path, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want error containing 'does not exist'", err.Error())
	}
}

func TestValidateRepoPath_NotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	err := validateRepoPath(f)
	if err == nil {
		t.Fatal("expected error for file path, got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error = %q, want error containing 'not a directory'", err.Error())
	}
}

func TestValidateRepoPath_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()

	err := validateRepoPath(dir)
	if err == nil {
		t.Fatal("expected error for non-git directory, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %q, want error containing 'not a git repository'", err.Error())
	}
}

// --- Root command validation integration tests ---

func TestRootCmd_InvalidBranchRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--branch=foo..bar"})
	// Override RunE to add validation without calling runner.Run
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := extractKnownFlags(args)
		if err != nil {
			return err
		}
		resolved, err := resolveRepo(cfg.repo)
		if err != nil {
			return err
		}
		if err := validateRepoPath(resolved); err != nil {
			return err
		}
		br, err := resolveConfigBranch(configFixture(nil), cfg.branch)
		if err != nil {
			return err
		}
		return validateBranch(br)
	}
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid branch, got nil")
	}
	if !strings.Contains(err.Error(), "invalid branch name") {
		t.Errorf("error = %q, want error containing 'invalid branch name'", err.Error())
	}
}

func TestRootCmd_NonRepoPathRejected(t *testing.T) {
	dir := t.TempDir()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo=" + dir})
	// Override RunE to test validation without runner.Run
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		cfg, err := extractKnownFlags(args)
		if err != nil {
			return err
		}
		resolved, err := resolveRepo(cfg.repo)
		if err != nil {
			return err
		}
		if err := validateRepoPath(resolved); err != nil {
			return err
		}
		br, err := resolveConfigBranch(configFixture(nil), cfg.branch)
		if err != nil {
			return err
		}
		return validateBranch(br)
	}
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-git repo, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %q, want error containing 'not a git repository'", err.Error())
	}
}

func TestValidateBranch_GitCheckRefFormatHookable(t *testing.T) {
	orig := gitCheckRefFormat
	defer func() { gitCheckRefFormat = orig }()

	gitCheckRefFormat = func(name string) error {
		return fmt.Errorf("injected error for %s", name)
	}

	err := validateBranch("anything")
	if err == nil {
		t.Fatal("expected injected error, got nil")
	}
	if !strings.Contains(err.Error(), "injected error") {
		t.Errorf("error = %q, want injected error", err.Error())
	}
}

func TestValidateRepoPath_GitRevParseHookable(t *testing.T) {
	orig := gitRevParseGitDir
	defer func() { gitRevParseGitDir = orig }()

	dir := t.TempDir()
	gitRevParseGitDir = func(d string) error {
		return fmt.Errorf("injected repo error for %s", d)
	}

	err := validateRepoPath(dir)
	if err == nil {
		t.Fatal("expected injected error, got nil")
	}
	if !strings.Contains(err.Error(), "injected repo error") {
		t.Errorf("error = %q, want injected repo error", err.Error())
	}
}

func TestRetryCmd_NoArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error with no arguments")
	}
}

func TestRetryCmd_SuccessfulRetry(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	failedDir := filepath.Join(repoRoot, ".mato", "failed")
	backlogDir := filepath.Join(repoRoot, ".mato", "backlog")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := "# Fix bug\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "fix-bug.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("retry command failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backlogDir, "fix-bug.md")); err != nil {
		t.Fatal("task should be in backlog after retry")
	}
}

func TestRetryCmd_TaskNotFound(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	if err := os.MkdirAll(filepath.Join(repoRoot, ".mato", "failed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, ".mato", "backlog"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "nonexistent"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing task")
	}
	if !strings.Contains(err.Error(), "not found in failed/") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRetryCmd_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"repo equals form", []string{"retry", "--repo=/tmp/r", "foo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			// Override RunE to just validate flag parsing.
			for _, sub := range cmd.Commands() {
				if sub.Name() == "retry" {
					sub.RunE = func(cmd *cobra.Command, args []string) error {
						return nil
					}
				}
			}
			if err := cmd.Execute(); err != nil {
				t.Fatalf("flag parsing failed: %v", err)
			}
		})
	}
}

func TestRetryCmd_DefaultTasksDirUsesRepoRoot(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	subdir := filepath.Join(repoRoot, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	failedDir := filepath.Join(repoRoot, ".mato", "failed")
	backlogDir := filepath.Join(repoRoot, ".mato", "backlog")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(failedDir, "fix-bug.md"), []byte("# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subdir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("retry command failed from subdir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backlogDir, "fix-bug.md")); err != nil {
		t.Fatalf("task should be requeued to repo-root backlog: %v", err)
	}
}

func TestRetryCmd_PreservesReviewRejectionFeedback(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	failedDir := filepath.Join(repoRoot, ".mato", "failed")
	backlogDir := filepath.Join(repoRoot, ".mato", "backlog")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := "# Fix bug\n\n<!-- review-rejection: reviewer at 2026-01-01T00:00:00Z — add tests -->\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "fix-bug.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("retry command failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(backlogDir, "fix-bug.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "<!-- review-rejection:") {
		t.Fatal("review rejection feedback should be preserved")
	}
}

func TestResolveConfigBranch(t *testing.T) {
	branch := "main"
	tests := []struct {
		name string
		cfg  *string
		flag string
		env  string
		want string
	}{
		{name: "flag wins", cfg: &branch, flag: "feature", env: "env-branch", want: "feature"},
		{name: "env wins over config", cfg: &branch, env: "env-branch", want: "env-branch"},
		{name: "config used", cfg: &branch, want: "main"},
		{name: "default fallback", want: "mato"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MATO_BRANCH", tt.env)
			got, err := resolveConfigBranch(configFixture(tt.cfg), tt.flag)
			if err != nil {
				t.Fatalf("resolveConfigBranch: %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveConfigBranch(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveConfigBranch_WhitespaceEnvRejected(t *testing.T) {
	t.Setenv("MATO_BRANCH", "  ")
	_, err := resolveConfigBranch(configFixture(nil), "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestResolveRunOptions(t *testing.T) {
	stringPtr := func(v string) *string { return &v }

	t.Run("uses config values when env unset", func(t *testing.T) {
		opts, err := resolveRunOptions(configFixtureWithValues(stringPtr("custom:latest"), stringPtr("claude-sonnet-4"), stringPtr("45m"), stringPtr("5m")))
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		if opts.DockerImage != "custom:latest" || opts.DefaultModel != "claude-sonnet-4" || opts.AgentTimeout != 45*time.Minute || opts.RetryCooldown != 5*time.Minute {
			t.Fatalf("opts = %+v", opts)
		}
	})

	t.Run("env overrides invalid config", func(t *testing.T) {
		t.Setenv("MATO_AGENT_TIMEOUT", "1h")
		t.Setenv("MATO_RETRY_COOLDOWN", "90s")
		opts, err := resolveRunOptions(configFixtureWithValues(nil, nil, stringPtr("bad"), stringPtr("also-bad")))
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		if opts.AgentTimeout != time.Hour || opts.RetryCooldown != 90*time.Second {
			t.Fatalf("opts = %+v", opts)
		}
	})

	t.Run("invalid effective config errors", func(t *testing.T) {
		_, err := resolveRunOptions(configFixtureWithValues(nil, nil, stringPtr("bad"), nil))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("invalid effective env timeout errors", func(t *testing.T) {
		t.Setenv("MATO_AGENT_TIMEOUT", "bad")
		_, err := resolveRunOptions(configFixture(nil))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("invalid env cooldown rejected", func(t *testing.T) {
		t.Setenv("MATO_RETRY_COOLDOWN", "bad")
		_, err := resolveRunOptions(configFixture(nil))
		if err == nil {
			t.Fatal("expected error for invalid MATO_RETRY_COOLDOWN, got nil")
		}
		if !strings.Contains(err.Error(), "MATO_RETRY_COOLDOWN") {
			t.Fatalf("error should mention MATO_RETRY_COOLDOWN: %v", err)
		}
	})

	t.Run("non-positive env cooldown rejected", func(t *testing.T) {
		t.Setenv("MATO_RETRY_COOLDOWN", "-5m")
		_, err := resolveRunOptions(configFixture(nil))
		if err == nil {
			t.Fatal("expected error for non-positive MATO_RETRY_COOLDOWN, got nil")
		}
		if !strings.Contains(err.Error(), "positive") {
			t.Fatalf("error should mention positive: %v", err)
		}
	})
}

func TestConfigFile_BranchFromConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	writeRepoConfig(t, repoRoot, "branch: main\n")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	called := false
	runFn = func(repoRootArg, branch string, copilotArgs []string, opts runner.RunOptions) error {
		called = true
		if repoRootArg != repoRoot {
			t.Fatalf("repoRoot = %q, want %q", repoRootArg, repoRoot)
		}
		if branch != "main" {
			t.Fatalf("branch = %q, want %q", branch, "main")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !called {
		t.Fatal("expected runFn to be called")
	}
}

func TestConfigFile_BranchFromEnv(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "main")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, branch string, _ []string, _ runner.RunOptions) error {
		if branch != "main" {
			t.Fatalf("branch = %q, want %q", branch, "main")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_BranchFlagOverridesConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "branch: main\n")
	t.Setenv("MATO_BRANCH", "env-branch")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, branch string, _ []string, _ runner.RunOptions) error {
		if branch != "feature" {
			t.Fatalf("branch = %q, want %q", branch, "feature")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot, "--branch", "feature"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_BranchEnvOverridesConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "branch: config-branch\n")
	t.Setenv("MATO_BRANCH", "env-branch")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, branch string, _ []string, _ runner.RunOptions) error {
		if branch != "env-branch" {
			t.Fatalf("branch = %q, want %q", branch, "env-branch")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_MissingConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, branch string, _ []string, opts runner.RunOptions) error {
		if branch != "mato" {
			t.Fatalf("branch = %q, want %q", branch, "mato")
		}
		if opts != (runner.RunOptions{}) {
			t.Fatalf("opts = %+v, want zero value", opts)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_InvalidYAML(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "branch: [\n")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse config file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigFile_InvalidAgentTimeout_RunMode(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "agent_timeout: not-a-duration\n")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid agent_timeout") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigFile_InvalidTimeout_EnvOverride(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "agent_timeout: not-a-duration\n")
	t.Setenv("MATO_AGENT_TIMEOUT", "1h")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, _ []string, opts runner.RunOptions) error {
		if opts.AgentTimeout != time.Hour {
			t.Fatalf("AgentTimeout = %v, want %v", opts.AgentTimeout, time.Hour)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_InvalidCooldown_EnvOverride(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "retry_cooldown: not-a-duration\n")
	t.Setenv("MATO_RETRY_COOLDOWN", "90s")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, _ []string, opts runner.RunOptions) error {
		if opts.RetryCooldown != 90*time.Second {
			t.Fatalf("RetryCooldown = %v, want %v", opts.RetryCooldown, 90*time.Second)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_RunOptionsFromConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, strings.Join([]string{
		"docker_image: custom:latest",
		"default_model: claude-sonnet-4",
		"agent_timeout: 45m",
		"retry_cooldown: 5m",
		"",
	}, "\n"))

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, _ []string, opts runner.RunOptions) error {
		if opts.DockerImage != "custom:latest" || opts.DefaultModel != "claude-sonnet-4" || opts.AgentTimeout != 45*time.Minute || opts.RetryCooldown != 5*time.Minute {
			t.Fatalf("opts = %+v", opts)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_DryRunUsesConfigBranch(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	writeRepoConfig(t, repoRoot, "branch: main\n")

	origDryRunFn := dryRunFn
	defer func() { dryRunFn = origDryRunFn }()

	dryRunFn = func(_ string, branch string) error {
		if branch != "main" {
			t.Fatalf("branch = %q, want %q", branch, "main")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_WhitespaceEnvBranchRejected(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "   ")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MATO_BRANCH must not be whitespace-only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigFile_InitUsesConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	writeRepoConfig(t, repoRoot, "branch: main\n")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	branchOut, err := runCmd("git", "-C", repoRoot, "branch", "--show-current")
	if err != nil {
		t.Fatalf("git branch --show-current: %v\n%s", err, branchOut)
	}
	if strings.TrimSpace(branchOut) != "main" {
		t.Fatalf("current branch = %q, want %q", strings.TrimSpace(branchOut), "main")
	}
}

func TestConfigFile_InitUsesEnvBranch(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "main")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	branchOut, err := runCmd("git", "-C", repoRoot, "branch", "--show-current")
	if err != nil {
		t.Fatalf("git branch --show-current: %v\n%s", err, branchOut)
	}
	if strings.TrimSpace(branchOut) != "main" {
		t.Fatalf("current branch = %q, want %q", strings.TrimSpace(branchOut), "main")
	}
}

func configFixture(branch *string) config.Config {
	return config.Config{Branch: branch}
}

func configFixtureWithValues(dockerImage, defaultModel, agentTimeout, retryCooldown *string) config.Config {
	return config.Config{
		DockerImage:   dockerImage,
		DefaultModel:  defaultModel,
		AgentTimeout:  agentTimeout,
		RetryCooldown: retryCooldown,
	}
}
