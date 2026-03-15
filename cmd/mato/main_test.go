package main

import (
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
