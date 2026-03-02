package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRepo string
		wantArgs []string
		wantErr  bool
	}{
		{
			name:     "equals syntax",
			args:     []string{"--worktree-repo=/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:     "space syntax",
			args:     []string{"--worktree-repo", "/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:     "with passthrough args",
			args:     []string{"--worktree-repo=/tmp/repo", "--", "-model", "gpt-4"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{"-model", "gpt-4"},
		},
		{
			name:     "extra args before separator",
			args:     []string{"--worktree-repo=/tmp/repo", "extra"},
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
			args:    []string{"--worktree-repo"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, args, err := parseArgs(tt.args)
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

func TestReadCounter(t *testing.T) {
	tests := []struct {
		name    string
		content string // empty means file does not exist
		want    int
	}{
		{name: "missing file", content: "", want: 1},
		{name: "valid value", content: "5\n", want: 5},
		{name: "value 1", content: "1\n", want: 1},
		{name: "empty file", content: " \n", want: 1},
		{name: "invalid text", content: "abc\n", want: 1},
		{name: "negative", content: "-3\n", want: 1},
		{name: "zero", content: "0\n", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "counter")
			if tt.content != "" {
				if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readCounter(path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("readCounter() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNextAvailableAgentID(t *testing.T) {
	worktreesRoot := t.TempDir()

	// No existing directories or worktrees — should return start.
	got, err := nextAvailableAgentID(1, worktreesRoot, map[string]struct{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1", got)
	}

	// Create agent1 as a regular directory (not a worktree) — should skip to 2.
	agent1 := filepath.Join(worktreesRoot, "agent1")
	if err := os.Mkdir(agent1, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = nextAvailableAgentID(1, worktreesRoot, map[string]struct{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("got %d, want 2", got)
	}

	// If agent1 is a known worktree, it should be reused.
	abs1, _ := filepath.Abs(agent1)
	got, err = nextAvailableAgentID(1, worktreesRoot, map[string]struct{}{abs1: {}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 1 {
		t.Errorf("got %d, want 1 (known worktree)", got)
	}

	// Start from a higher number.
	got, err = nextAvailableAgentID(5, worktreesRoot, map[string]struct{}{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 5 {
		t.Errorf("got %d, want 5", got)
	}
}
