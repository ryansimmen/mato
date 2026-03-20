package main

import (
	"os"
	"testing"

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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, branch, tasks, dryRun, extra := extractKnownFlags(tt.args)
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", branch, tt.wantBranch)
			}
			if tasks != tt.wantTasks {
				t.Errorf("tasksDir = %q, want %q", tasks, tt.wantTasks)
			}
			if dryRun != tt.wantDryRun {
				t.Errorf("dryRun = %v, want %v", dryRun, tt.wantDryRun)
			}
			if len(extra) != len(tt.wantExtra) {
				t.Fatalf("extra = %v, want %v", extra, tt.wantExtra)
			}
			for i := range extra {
				if extra[i] != tt.wantExtra[i] {
					t.Errorf("extra[%d] = %q, want %q", i, extra[i], tt.wantExtra[i])
				}
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

func TestRootCmd_UnknownFlagsForwarded(t *testing.T) {
	var capturedArgs []string
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo=/tmp/repo", "--model", "gpt-5.2"})
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		_, _, _, _, copilotArgs := extractKnownFlags(args)
		capturedArgs = copilotArgs
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

func TestStatusCmd_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"status help", []string{"status", "--help"}},
		{"status with repo", []string{"status", "--repo=/tmp/repo"}},
		{"status with tasks-dir", []string{"status", "--tasks-dir=/tmp/tasks"}},
		{"status with repo and tasks-dir", []string{"status", "--repo=/tmp/repo", "--tasks-dir=/tmp/tasks"}},
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
