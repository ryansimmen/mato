package main

import (
	"errors"
	"os"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantRepo   string
		wantBranch string
		wantArgs   []string
		wantErr    bool
	}{
		{
			name:       "repo equals syntax",
			args:       []string{"--repo=/tmp/repo"},
			wantRepo:   "/tmp/repo",
			wantBranch: "mato",
			wantArgs:   []string{},
		},
		{
			name:       "repo space syntax",
			args:       []string{"--repo", "/tmp/repo"},
			wantRepo:   "/tmp/repo",
			wantBranch: "mato",
			wantArgs:   []string{},
		},
		{
			name:       "with passthrough args",
			args:       []string{"--repo=/tmp/repo", "--", "--model", "gpt-5.2"},
			wantRepo:   "/tmp/repo",
			wantBranch: "mato",
			wantArgs:   []string{"--model", "gpt-5.2"},
		},
		{
			name:       "branch equals syntax",
			args:       []string{"--repo=/tmp/repo", "--branch=develop"},
			wantRepo:   "/tmp/repo",
			wantBranch: "develop",
			wantArgs:   []string{},
		},
		{
			name:       "branch space syntax",
			args:       []string{"--repo=/tmp/repo", "--branch", "develop"},
			wantRepo:   "/tmp/repo",
			wantBranch: "develop",
			wantArgs:   []string{},
		},
		{
			name:    "flag without value",
			args:    []string{"--repo"},
			wantErr: true,
		},
		{
			name:    "branch flag without value",
			args:    []string{"--branch"},
			wantErr: true,
		},
		{
			name:    "tasks-dir flag without value",
			args:    []string{"--tasks-dir"},
			wantErr: true,
		},
		{
			name:    "help flag",
			args:    []string{"--help"},
			wantErr: true,
		},
		{
			name:    "short help flag",
			args:    []string{"-h"},
			wantErr: true,
		},
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	defaultTests := []struct {
		name       string
		args       []string
		wantRepo   string
		wantBranch string
		wantArgs   []string
	}{
		{
			name:       "no args defaults to cwd",
			args:       []string{},
			wantRepo:   wd,
			wantBranch: "mato",
			wantArgs:   []string{},
		},
		{
			name:       "extra args without repo defaults to cwd",
			args:       []string{"--model", "gpt-5"},
			wantRepo:   wd,
			wantBranch: "mato",
			wantArgs:   []string{"--model", "gpt-5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, branch, _, args, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", branch, tt.wantBranch)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tt.wantArgs)
			}
			for i := range args {
				if args[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, args[i], tt.wantArgs[i])
				}
			}
		})
	}

	for _, tt := range defaultTests {
		t.Run(tt.name, func(t *testing.T) {
			repo, branch, _, args, err := parseArgs(tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", branch, tt.wantBranch)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("args = %v, want %v", args, tt.wantArgs)
			}
			for i := range args {
				if args[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] = %q, want %q", i, args[i], tt.wantArgs[i])
				}
			}
		})
	}
}

// TestStatusSubcommandArgs verifies argument parsing behavior specific to the
// status subcommand: no extra positional args allowed, --repo/--tasks-dir accepted,
// --branch silently accepted (but ignored by status), and --help/-h work.
func TestStatusSubcommandArgs(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	tests := []struct {
		name         string
		args         []string // args after "status", as passed to parseArgs
		wantRepo     string
		wantTasksDir string
		wantExtras   int // number of extra args; status rejects >0
		wantErr      bool
	}{
		// 1. mato status (no extra args) — success
		{
			name:     "no args succeeds",
			args:     []string{},
			wantRepo: wd,
		},
		// 2. mato status extra-arg — extras returned, status rejects
		{
			name:       "single extra positional arg",
			args:       []string{"extra-arg"},
			wantRepo:   wd,
			wantExtras: 1,
		},
		{
			name:       "multiple extra positional args",
			args:       []string{"arg1", "arg2"},
			wantRepo:   wd,
			wantExtras: 2,
		},
		// 3. mato status --repo /path --tasks-dir /path — valid flags
		{
			name:         "repo and tasks-dir space syntax",
			args:         []string{"--repo", "/tmp/repo", "--tasks-dir", "/tmp/tasks"},
			wantRepo:     "/tmp/repo",
			wantTasksDir: "/tmp/tasks",
		},
		{
			name:         "repo and tasks-dir equals syntax",
			args:         []string{"--repo=/tmp/repo", "--tasks-dir=/tmp/tasks"},
			wantRepo:     "/tmp/repo",
			wantTasksDir: "/tmp/tasks",
		},
		{
			name:         "tasks-dir only",
			args:         []string{"--tasks-dir", "/custom/tasks"},
			wantRepo:     wd,
			wantTasksDir: "/custom/tasks",
		},
		// 4. mato status --branch main — accepted but ignored by status
		{
			name:     "branch space syntax silently accepted",
			args:     []string{"--branch", "main"},
			wantRepo: wd,
		},
		{
			name:     "branch equals syntax silently accepted",
			args:     []string{"--branch=develop"},
			wantRepo: wd,
		},
		{
			name:         "branch with repo and tasks-dir",
			args:         []string{"--repo=/tmp/repo", "--branch=main", "--tasks-dir=/tmp/tasks"},
			wantRepo:     "/tmp/repo",
			wantTasksDir: "/tmp/tasks",
		},
		// 5. --help / -h for status subcommand
		{
			name:    "help flag",
			args:    []string{"--help"},
			wantErr: true,
		},
		{
			name:    "short help flag",
			args:    []string{"-h"},
			wantErr: true,
		},
		{
			name:    "help after other flags",
			args:    []string{"--repo=/tmp/repo", "--help"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _, tasksDir, extras, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, errHelp) {
					t.Errorf("expected errHelp, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
			if tasksDir != tt.wantTasksDir {
				t.Errorf("tasksDir = %q, want %q", tasksDir, tt.wantTasksDir)
			}
			if len(extras) != tt.wantExtras {
				t.Errorf("extras count = %d, want %d (extras: %v)", len(extras), tt.wantExtras, extras)
			}
			// Status subcommand rejects any extra positional args
			if tt.wantExtras > 0 && len(extras) == 0 {
				t.Error("expected extras to be non-empty for status rejection")
			}
		})
	}
}
