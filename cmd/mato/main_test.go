package main

import (
	"os"
	"path/filepath"
	"strings"
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
			name:     "worktree-repo backwards compat",
			args:     []string{"--worktree-repo=/tmp/repo"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{},
		},
		{
			name:     "with passthrough args",
			args:     []string{"--repo=/tmp/repo", "--", "--model", "gpt-5.2"},
			wantRepo: "/tmp/repo",
			wantArgs: []string{"--model", "gpt-5.2"},
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

func TestSanitizeBranchName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple name",
			input: "add-feature.md",
			want:  "add-feature",
		},
		{
			name:  "spaces and special chars",
			input: "fix the bug (urgent).md",
			want:  "fix-the-bug-urgent",
		},
		{
			name:  "already clean no extension",
			input: "my-task",
			want:  "my-task",
		},
		{
			name:  "consecutive special chars",
			input: "foo---bar___baz.md",
			want:  "foo-bar-baz",
		},
		{
			name:  "leading and trailing specials",
			input: "---hello---.md",
			want:  "hello",
		},
		{
			name:  "empty after strip",
			input: ".md",
			want:  "unnamed",
		},
		{
			name:  "unicode characters",
			input: "tâche-résumé.md",
			want:  "t-che-r-sum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeBranchName(tt.input); got != tt.want {
				t.Errorf("sanitizeBranchName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateAgentID(t *testing.T) {
	id, err := generateAgentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("expected 8 hex chars, got %q (len %d)", id, len(id))
	}
	// Verify uniqueness (two calls should differ).
	id2, err := generateAgentID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == id2 {
		t.Errorf("two consecutive IDs should differ: %q == %q", id, id2)
	}
}

func TestHasModelArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{
			name: "no model flag",
			args: []string{"--autopilot"},
			want: false,
		},
		{
			name: "model with value",
			args: []string{"--model", "gpt-5"},
			want: true,
		},
		{
			name: "model equals syntax",
			args: []string{"--model=gpt-5"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasModelArg(tt.args); got != tt.want {
				t.Fatalf("hasModelArg(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRecoverOrphanedTasks(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", "completed", "failed"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Place a task file in in-progress to simulate a crash.
	orphan := filepath.Join(tasksDir, "in-progress", "fix-bug.md")
	os.WriteFile(orphan, []byte("# Fix bug\nDo the thing.\n"), 0o644)

	recoverOrphanedTasks(tasksDir)

	// Should no longer be in in-progress.
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphaned task was not removed from in-progress/")
	}
	// Should now be in backlog.
	recovered := filepath.Join(tasksDir, "backlog", "fix-bug.md")
	data, err := os.ReadFile(recovered)
	if err != nil {
		t.Fatalf("recovered task not found in backlog/: %v", err)
	}
	// Should contain the original content plus a failure record.
	if !strings.Contains(string(data), "# Fix bug") {
		t.Error("recovered task lost original content")
	}
	if !strings.Contains(string(data), "<!-- failure: mato-recovery") {
		t.Error("recovered task missing failure record")
	}
}

func TestRecoverOrphanedTasks_IgnoresNonMd(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Place a non-.md file — should be left alone.
	other := filepath.Join(tasksDir, "in-progress", "notes.txt")
	os.WriteFile(other, []byte("hello"), 0o644)

	recoverOrphanedTasks(tasksDir)

	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-.md file should not be moved: %v", err)
	}
}

func TestCompletedTaskBranches(t *testing.T) {
	tasksDir := t.TempDir()
	os.MkdirAll(filepath.Join(tasksDir, "completed"), 0o755)

	// Create some completed task files.
	os.WriteFile(filepath.Join(tasksDir, "completed", "add-feature.md"), []byte("done"), 0o644)
	os.WriteFile(filepath.Join(tasksDir, "completed", "fix the bug.md"), []byte("done"), 0o644)

	branches := completedTaskBranches(tasksDir)

	if !branches["task/add-feature"] {
		t.Error("expected task/add-feature in completed branches")
	}
	if !branches["task/fix-the-bug"] {
		t.Error("expected task/fix-the-bug in completed branches")
	}
	if branches["task/nonexistent"] {
		t.Error("unexpected branch in completed set")
	}
}
