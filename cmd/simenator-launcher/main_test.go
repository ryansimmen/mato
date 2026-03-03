package main

import (
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantRepo      string
		wantArgs      []string
		wantSeparator bool
		wantErr       bool
	}{
		{
			name:     "repo equals syntax",
			args:     []string{"--repo=/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:     "repo space syntax",
			args:     []string{"--repo", "/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:     "worktree-repo backwards compat equals",
			args:     []string{"--worktree-repo=/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:     "worktree-repo backwards compat space",
			args:     []string{"--worktree-repo", "/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:          "with passthrough args",
			args:          []string{"--repo=/tmp/repo", "--", "-model", "gpt-4"},
			wantRepo:      "/tmp/repo",
			wantArgs:      []string{"-model", "gpt-4"},
			wantSeparator: true,
		},
		{
			name:     "extra args before separator",
			args:     []string{"--repo=/tmp/repo", "extra"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{"extra"},
		},
		{
			name:    "missing required flag",
			args:    []string{"extra"},
			wantErr: true,
		},
		{
			name:    "empty args",
			args:    []string{},
			wantErr: true,
		},
		{
			name:    "flag without value",
			args:    []string{"--repo"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, args, hasSep, err := parseArgs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if hasSep != tt.wantSeparator {
				t.Errorf("hasSeparator = %v, want %v", hasSep, tt.wantSeparator)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
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
