package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func TestParseClaimedBy(t *testing.T) {
	dir := t.TempDir()

	// File with claimed-by metadata.
	withClaim := filepath.Join(dir, "task.md")
	os.WriteFile(withClaim, []byte("<!-- claimed-by: abc123  claimed-at: 2026-01-01T00:00:00Z -->\n# Do stuff\n"), 0o644)
	if got := parseClaimedBy(withClaim); got != "abc123" {
		t.Errorf("parseClaimedBy = %q, want %q", got, "abc123")
	}

	// File without claimed-by.
	noClaim := filepath.Join(dir, "plain.md")
	os.WriteFile(noClaim, []byte("# Just a task\n"), 0o644)
	if got := parseClaimedBy(noClaim); got != "" {
		t.Errorf("parseClaimedBy = %q, want empty", got)
	}

	// Non-existent file.
	if got := parseClaimedBy(filepath.Join(dir, "missing.md")); got != "" {
		t.Errorf("parseClaimedBy = %q, want empty for missing file", got)
	}
}

func TestIsAgentActive(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	// Empty agent ID is never active.
	if isAgentActive(tasksDir, "") {
		t.Error("empty agent ID should not be active")
	}

	// No lock file means not active.
	if isAgentActive(tasksDir, "no-such-agent") {
		t.Error("agent without lock file should not be active")
	}

	// Lock file with our own PID should be active.
	os.WriteFile(filepath.Join(locksDir, "live.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if !isAgentActive(tasksDir, "live") {
		t.Error("agent with current PID should be active")
	}

	// Lock file with a definitely-dead PID should not be active.
	// PID 2147483647 is almost certainly not running.
	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647"), 0o644)
	if isAgentActive(tasksDir, "dead") {
		t.Error("agent with dead PID should not be active")
	}
}

func TestRegisterAgent(t *testing.T) {
	tasksDir := t.TempDir()

	cleanup, err := registerAgent(tasksDir, "test-agent")
	if err != nil {
		t.Fatalf("registerAgent: %v", err)
	}

	lockFile := filepath.Join(tasksDir, ".locks", "test-agent.pid")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file PID = %q, want %q", got, strconv.Itoa(os.Getpid()))
	}

	cleanup()

	if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
		t.Error("cleanup should remove lock file")
	}
}

func TestRecoverOrphanedTasks_SkipsActiveAgent(t *testing.T) {
	tasksDir := t.TempDir()
	for _, sub := range []string{"backlog", "in-progress", ".locks"} {
		os.MkdirAll(filepath.Join(tasksDir, sub), 0o755)
	}

	// Place a task claimed by an "active" agent (our own PID).
	agentID := "active-agent"
	task := filepath.Join(tasksDir, "in-progress", "active-task.md")
	content := fmt.Sprintf("<!-- claimed-by: %s  claimed-at: 2026-01-01T00:00:00Z -->\n# Active task\n", agentID)
	os.WriteFile(task, []byte(content), 0o644)

	// Write a lock file with our PID so it looks alive.
	os.WriteFile(filepath.Join(tasksDir, ".locks", agentID+".pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)

	recoverOrphanedTasks(tasksDir)

	// Task should still be in in-progress (not recovered).
	if _, err := os.Stat(task); err != nil {
		t.Fatal("task claimed by active agent should NOT be recovered")
	}
	// Should NOT be in backlog.
	if _, err := os.Stat(filepath.Join(tasksDir, "backlog", "active-task.md")); err == nil {
		t.Fatal("task claimed by active agent should NOT appear in backlog")
	}
}

func TestMergeToMain_ConflictReturnsError(t *testing.T) {
	// Create a bare "origin" repo to act as the push target.
	origin := t.TempDir()
	if _, err := gitOutput(origin, "init", "--bare"); err != nil {
		t.Fatalf("init bare origin: %v", err)
	}
	gitOutput(origin, "symbolic-ref", "HEAD", "refs/heads/main")

	// Create a working repo, add initial commit, push to origin.
	work := t.TempDir()
	if _, err := gitOutput(work, "init", "-b", "main"); err != nil {
		t.Fatalf("init work: %v", err)
	}
	gitOutput(work, "config", "user.name", "test")
	gitOutput(work, "config", "user.email", "test@test")
	gitOutput(work, "remote", "add", "origin", origin)
	os.WriteFile(filepath.Join(work, "README.md"), []byte("# repo\n"), 0o644)
	gitOutput(work, "add", "-A")
	gitOutput(work, "commit", "-m", "init")
	gitOutput(work, "push", "origin", "main")

	// Branch A: adds main.go with "Hello World".
	gitOutput(work, "checkout", "-b", "task/hello-world", "main")
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\nfunc main() { println(\"Hello World\") }\n"), 0o644)
	gitOutput(work, "add", "-A")
	gitOutput(work, "commit", "-m", "hello world")

	// Branch B: adds main.go with "Hello America" (conflicts with A).
	gitOutput(work, "checkout", "-b", "task/hello-america", "main")
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\nfunc main() { println(\"Hello America\") }\n"), 0o644)
	gitOutput(work, "add", "-A")
	gitOutput(work, "commit", "-m", "hello america")

	// Allow pushing to the checked-out branch.
	gitOutput(origin, "config", "receive.denyCurrentBranch", "updateInstead")

	// Merge branch A — should succeed cleanly.
	if err := mergeToMain(work, origin, "task/hello-world"); err != nil {
		t.Fatalf("mergeToMain(hello-world): %v", err)
	}

	// Merge branch B — conflicts with A. The agent should have resolved
	// this before marking complete; mergeToMain must return an error.
	err := mergeToMain(work, origin, "task/hello-america")
	if err == nil {
		t.Fatal("expected mergeToMain(hello-america) to return an error on conflict, got nil")
	}
	if !strings.Contains(err.Error(), "merge conflict") {
		t.Errorf("expected error to mention 'merge conflict', got: %v", err)
	}
}

func TestCleanStaleLocks(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	os.MkdirAll(locksDir, 0o755)

	// Live lock (our PID).
	os.WriteFile(filepath.Join(locksDir, "alive.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	// Stale lock (dead PID).
	os.WriteFile(filepath.Join(locksDir, "dead.pid"), []byte("2147483647"), 0o644)

	cleanStaleLocks(tasksDir)

	if _, err := os.Stat(filepath.Join(locksDir, "alive.pid")); err != nil {
		t.Error("live lock should not be removed")
	}
	if _, err := os.Stat(filepath.Join(locksDir, "dead.pid")); !os.IsNotExist(err) {
		t.Error("stale lock should be removed")
	}
}
