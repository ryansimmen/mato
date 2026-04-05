package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"mato/internal/configresolve"
	"mato/internal/doctor"
	"mato/internal/git"
	"mato/internal/queue"
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

func TestResolveBranch_EnvOnly(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "")
		resolved, err := configresolve.ResolveBranch(config.LoadResult{}, "")
		if err != nil {
			t.Fatalf("ResolveBranch: %v", err)
		}
		if resolved.Source != configresolve.SourceDefault || resolved.Value != "mato" {
			t.Fatalf("resolved = %+v, want default mato", resolved)
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "main")
		resolved, err := configresolve.ResolveBranch(config.LoadResult{}, "")
		if err != nil {
			t.Fatalf("ResolveBranch: %v", err)
		}
		if resolved.Source != configresolve.SourceEnv || resolved.Value != "main" || resolved.EnvVar != "MATO_BRANCH" {
			t.Fatalf("resolved = %+v, want env main", resolved)
		}
	})

	t.Run("empty treated as unset", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "")
		resolved, err := configresolve.ResolveBranch(config.LoadResult{}, "")
		if err != nil {
			t.Fatalf("ResolveBranch: %v", err)
		}
		if resolved.Source != configresolve.SourceDefault || resolved.Value != "mato" {
			t.Fatalf("resolved = %+v, want default mato", resolved)
		}
	})

	t.Run("whitespace rejected", func(t *testing.T) {
		t.Setenv("MATO_BRANCH", "   ")
		_, err := configresolve.ResolveBranch(config.LoadResult{}, "")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestConfigCmd_SubcommandRegistered(t *testing.T) {
	cmd := newRootCmd()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "config" {
			return
		}
	}
	t.Fatal("config subcommand not registered")
}

func TestConfigCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"config", "--format=yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	if err.Error() != "--format must be text or json, got yaml" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestConfigCmd_DelegatesToConfigShow(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	var gotRepo, gotFormat string
	orig := configShowFn
	defer func() { configShowFn = orig }()
	configShowFn = func(w io.Writer, repoRootArg, format string) error {
		gotRepo = repoRootArg
		gotFormat = format
		_, _ = io.WriteString(w, "ok")
		return nil
	}
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"config", "--repo", repoRoot, "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotRepo != repoRoot || gotFormat != "json" {
		t.Fatalf("delegation = repo %q format %q", gotRepo, gotFormat)
	}
	if out.String() != "ok" {
		t.Fatalf("output = %q, want ok", out.String())
	}
}

func TestConfigCmd_JSONOutput(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	writeRepoConfig(t, repoRoot, "branch: main\n")
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"config", "--repo", repoRoot, "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result struct {
		RepoRoot string `json:"repo_root"`
		Branch   struct {
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"branch"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("json.Unmarshal: %v\n%s", err, out.String())
	}
	if result.RepoRoot != repoRoot || result.Branch.Value != "main" || result.Branch.Source != "config" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestConfigCmd_ResolverErrorPropagated(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "branch: [\n")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"config", "--repo", repoRoot})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "parse config file") {
		t.Fatalf("err = %v, want parse config file error", err)
	}
}

func TestConfigCmd_InvalidEffectiveBranchRejected(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	writeRepoConfig(t, repoRoot, "branch: foo..bar\n")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"config", "--repo", repoRoot})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid branch name") {
		t.Fatalf("err = %v, want invalid branch name", err)
	}
}

func TestConfigCmd_WorksWithoutMatoDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"config", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.String(), "Config file: none") {
		t.Fatalf("output = %q, want config file none", out.String())
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

func TestRootCmd_PositionalArgsRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command \"foo\" for \"mato\"") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRootCmd_DoubleDashRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--", "--model", "gpt-5.4"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command \"--model\" for \"mato\"") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRootCmd_UnknownRootFlagRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--model", "gpt-5.4"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown flag: --model") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCmd_DoubleDashRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--", "--model", "gpt-5.4"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command \"--model\" for \"mato run\"") {
		t.Fatalf("unexpected error: %v", err)
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

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = origStderr
	}()

	fn()
	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close read pipe: %v", err)
	}
	return string(data)
}

type failAfterNWriter struct {
	n      int
	err    error
	writes int
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.n {
		return 0, w.err
	}
	return len(p), nil
}

func renderCommandError(t *testing.T, err error) (string, int) {
	t.Helper()
	var buf bytes.Buffer
	code := writeCommandError(&buf, err)
	return buf.String(), code
}

func writeRepoConfig(t *testing.T, repoRoot, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repoRoot, ".mato.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write .mato.yaml: %v", err)
	}
}

func TestRootCmd_HelpListsCompletionCommand(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "completion") {
		t.Fatalf("expected help to mention completion command, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "run") {
		t.Fatalf("expected help to mention run subcommand, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Path to the git repository (default: current directory)") {
		t.Fatalf("expected help to mention repo default, got:\n%s", out.String())
	}
}

func TestRunCmd_HelpDocumentsResolvedDefaults(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"run", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	help := out.String()
	for _, want := range []string{
		"Target branch for merging (default: mato)",
		"Copilot model for task agents (default: " + config.DefaultTaskModel + ")",
		"Copilot model for review agents (default: " + config.DefaultReviewModel + ")",
		"Reasoning effort for task agents (default: " + config.DefaultReasoningEffort + ")",
		"Reasoning effort for review agents (default: " + config.DefaultReasoningEffort + ")",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("expected run help to contain %q, got:\n%s", want, help)
		}
	}
}

func TestVersionCmd_Output(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "subcommand", args: []string{"version"}, want: "mato 1.2.3\n"},
		{name: "root flag", args: []string{"--version"}, want: "mato 1.2.3\n"},
	}

	version = "1.2.3"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.String() != tt.want {
				t.Fatalf("output = %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestVersionCmd_ShortFlag(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "1.2.3"

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-v"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "mato 1.2.3\n" {
		t.Fatalf("output = %q, want %q", out.String(), "mato 1.2.3\n")
	}
}

func TestVersionCmd_DefaultFallback(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "dev"

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "mato dev\n" {
		t.Fatalf("output = %q, want %q", out.String(), "mato dev\n")
	}
}

func TestVersionCmd_FormatJSON(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "1.2.3"

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version", "--format=json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, out.String())
	}
	if result["version"] != "1.2.3" {
		t.Errorf("version = %q, want %q", result["version"], "1.2.3")
	}
}

func TestVersionCmd_FormatText(t *testing.T) {
	origVersion := version
	defer func() { version = origVersion }()
	version = "1.2.3"

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version", "--format=text"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "mato 1.2.3\n" {
		t.Fatalf("output = %q, want %q", out.String(), "mato 1.2.3\n")
	}
}

func TestVersionCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"version", "--format=yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestWriteCommandError_UsageErrorIncludesUsage(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "extra"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	out, code := renderCommandError(t, err)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(out, "mato error:") {
		t.Fatalf("expected mato error prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "Usage:\n  mato status") {
		t.Fatalf("expected status usage in output, got:\n%s", out)
	}
}

func TestWriteCommandError_RuntimeErrorOmitsUsage(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", t.TempDir()})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	out, code := renderCommandError(t, err)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(out, "Usage:") {
		t.Fatalf("runtime error should not include usage, got:\n%s", out)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Fatalf("expected git repo error, got:\n%s", out)
	}
}

func TestWriteCommandError_SilentErrorSuppressesOutput(t *testing.T) {
	out, code := renderCommandError(t, &SilentError{Err: fmt.Errorf("already printed"), Code: 1})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if out != "" {
		t.Fatalf("expected no output, got %q", out)
	}
}

func TestWriteCommandError_ExitErrorSuppressesOutput(t *testing.T) {
	out, code := renderCommandError(t, ExitError{Code: 2})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if out != "" {
		t.Fatalf("expected no output, got %q", out)
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
	var out bytes.Buffer
	if err := printInitResult(&out, &setup.InitResult{
		BranchName:       "mato",
		BranchSource:     git.BranchSourceRemoteCached,
		IgnorePattern:    "/.mato/",
		TasksDir:         filepath.Join("repo", ".mato"),
		GitignoreUpdated: false,
	}); err != nil {
		t.Fatalf("printInitResult: %v", err)
	}
	output := out.String()

	if !strings.Contains(output, "Switched to branch: mato (cached origin/mato (origin unavailable))") {
		t.Fatalf("expected cached origin branch message, got %q", output)
	}
}

func TestPrintInitResult_RemoteBranchMessage(t *testing.T) {
	var out bytes.Buffer
	if err := printInitResult(&out, &setup.InitResult{
		BranchName:       "mato",
		BranchSource:     git.BranchSourceRemote,
		IgnorePattern:    "/.mato/",
		TasksDir:         filepath.Join("repo", ".mato"),
		GitignoreUpdated: false,
	}); err != nil {
		t.Fatalf("printInitResult: %v", err)
	}
	output := out.String()

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

func TestStatusCmd_FormatJSONWithVerboseRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"status", "--format=json", "--verbose"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format json with --verbose, got nil")
	}
	want := "--verbose can only be used with text output"
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
		{"status with verbose", []string{"status", "--verbose"}},
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

func TestStatusCmd_PersistentRepoFlagBothPositions(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "root before subcommand",
			args: []string{"--repo", repoRoot, "status"},
		},
		{
			name: "subcommand local position",
			args: []string{"status", "--repo", repoRoot},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
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

	repoRoot := testutil.SetupRepo(t)

	tests := []struct {
		name string
		args []string
	}{
		{"doctor help", []string{"doctor", "--help"}},
		{"doctor with repo", []string{"doctor", "--repo=" + repoRoot}},
		{"doctor with fix", []string{"doctor", "--fix"}},
		{"doctor with json format", []string{"doctor", "--format=json"}},
		{"doctor with text format", []string{"doctor", "--format=text"}},
		{"doctor with all flags", []string{"doctor", "--repo=" + repoRoot, "--fix", "--format=json", "--only=git"}},
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
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
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
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
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
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed .mato.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "parse config file") {
		t.Errorf("error = %q, want config parse error", err.Error())
	}
}

func TestDoctorCmd_EnvImageStillValidatesConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	t.Setenv("MATO_DOCKER_IMAGE", "env-override:3.0")

	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		t.Fatal("doctorRunFn should not be called when config is malformed")
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed .mato.yaml even with MATO_DOCKER_IMAGE set, got nil")
	}
	if !strings.Contains(err.Error(), "parse config file") {
		t.Errorf("error = %q, want config parse error", err.Error())
	}
}

func TestDoctorCmd_EnvImageWithValidConfigSucceeds(t *testing.T) {
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
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected success with env var and valid config, got: %v", err)
	}

	if capturedOpts.DockerImage != "from-env:2.0" {
		t.Errorf("DockerImage = %q, want %q", capturedOpts.DockerImage, "from-env:2.0")
	}
}

func TestDoctorCmd_IgnoresUnrelatedInvalidRunSettings(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "task_reasoning_effort: nope\nagent_timeout: bad\n")

	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedOpts.DockerImage != config.DefaultDockerImage {
		t.Fatalf("DockerImage = %q, want %q", capturedOpts.DockerImage, config.DefaultDockerImage)
	}
}

func TestDoctorCmd_EnvImageWithMultiDocConfigFails(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: img:1\n---\ndocker_image: img:2\n")

	t.Setenv("MATO_DOCKER_IMAGE", "from-env:3.0")

	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, _ doctor.Options) (doctor.Report, error) {
		t.Fatal("doctorRunFn should not be called when config has multiple YAML documents")
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for multi-document .mato.yaml even with MATO_DOCKER_IMAGE set")
	}
	if !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Errorf("error = %q, want multiple YAML documents error", err.Error())
	}
}

func TestDoctorCmd_WhitespaceOnlyEnvImageFallsBackToConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: from-config:1.0\n")

	t.Setenv("MATO_DOCKER_IMAGE", "   ")

	var capturedOpts doctor.Options
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		capturedOpts = opts
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedOpts.DockerImage != "from-config:1.0" {
		t.Errorf("DockerImage = %q, want %q (whitespace-only env should fall back to config)", capturedOpts.DockerImage, "from-config:1.0")
	}
}

func TestDoctorShouldPreResolveDockerImage(t *testing.T) {
	tests := []struct {
		name string
		only []string
		want bool
	}{
		{name: "all checks", only: nil, want: false},
		{name: "explicit docker", only: []string{"queue", "docker"}, want: true},
		{name: "docker only", only: []string{"docker"}, want: true},
		{name: "config only", only: []string{"config"}, want: false},
		{name: "docker and config", only: []string{"docker", "config"}, want: false},
		{name: "queue only", only: []string{"queue", "tasks", "deps"}, want: false},
		{name: "git only", only: []string{"git"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doctorShouldPreResolveDockerImage(tt.only); got != tt.want {
				t.Errorf("doctorShouldPreResolveDockerImage(%v) = %v, want %v", tt.only, got, tt.want)
			}
		})
	}
}

func TestDoctorCmd_DefaultRunDoesNotPreResolveDockerImage(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: custom:latest\n")

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

	if capturedOpts.DockerImage != "" {
		t.Fatalf("DockerImage = %q, want empty for default full doctor run", capturedOpts.DockerImage)
	}
}

func TestDoctorCmd_DefaultRunWithMalformedConfigDelegatesToDoctor(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	called := false
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
		called = true
		if opts.DockerImage != "" {
			t.Fatalf("DockerImage = %q, want empty when config check owns validation", opts.DockerImage)
		}
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected default doctor run to delegate malformed config to doctor.Run, got: %v", err)
	}
	if !called {
		t.Fatal("expected doctorRunFn to be called")
	}
}

func TestDoctorCmd_ConfigAndDockerOrderDoesNotPreResolveDockerImage(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: custom:latest\n")

	tests := [][]string{
		{"doctor", "--repo", repoRoot, "--only", "docker,config"},
		{"doctor", "--repo", repoRoot, "--only", "config,docker"},
	}

	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	for _, args := range tests {
		var capturedOpts doctor.Options
		doctorRunFn = func(_ context.Context, _ string, opts doctor.Options) (doctor.Report, error) {
			capturedOpts = opts
			return doctor.Report{}, nil
		}

		cmd := newRootCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("Execute(%v): %v", args, err)
		}
		if capturedOpts.DockerImage != "" {
			t.Fatalf("DockerImage = %q, want empty for %v", capturedOpts.DockerImage, args)
		}
	}
}

func TestDoctorCmd_NonRepoNotRejectedBeforeDoctorRun(t *testing.T) {
	dir := t.TempDir()
	called := false
	orig := doctorRunFn
	defer func() { doctorRunFn = orig }()

	doctorRunFn = func(_ context.Context, repo string, _ doctor.Options) (doctor.Report, error) {
		called = true
		if repo != dir {
			t.Fatalf("repo = %q, want %q", repo, dir)
		}
		return doctor.Report{}, nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected non-repo path to reach doctor.Run, got: %v", err)
	}
	if !called {
		t.Fatal("expected doctorRunFn to be called")
	}
}

func TestDoctorCmd_InvalidOnlySkipsDockerPreResolution(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, ":\n  bad yaml: [unbalanced\n")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"doctor", "--repo", repoRoot, "--only", "docker,bogus"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected doctor.invalid_only exit error")
	}
	var exitErr ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.Code != 2 {
		t.Fatalf("exit code = %d, want 2", exitErr.Code)
	}
	if strings.Contains(err.Error(), "parse config file") {
		t.Fatalf("expected invalid --only handling before config pre-resolution, got: %v", err)
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

func TestGraphCmd_DelegatesToGraphShow(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	var gotWriter io.Writer
	var gotRepo, gotFormat string
	var gotShowAll bool

	orig := graphShowFn
	defer func() { graphShowFn = orig }()
	graphShowFn = func(w io.Writer, repoRootArg, format string, showAll bool) error {
		gotWriter = w
		gotRepo = repoRootArg
		gotFormat = format
		gotShowAll = showAll
		_, _ = io.WriteString(w, "ok")
		return nil
	}

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"graph", "--repo", repoRoot, "--format", "json", "--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotWriter != &out {
		t.Fatalf("writer = %T, want command output writer", gotWriter)
	}
	if gotRepo != repoRoot {
		t.Errorf("repo = %q, want %q", gotRepo, repoRoot)
	}
	if gotFormat != "json" {
		t.Errorf("format = %q, want %q", gotFormat, "json")
	}
	if !gotShowAll {
		t.Error("showAll = false, want true")
	}
	if out.String() != "ok" {
		t.Fatalf("output = %q, want %q", out.String(), "ok")
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

func TestGraphCmd_MissingMatoDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	// Do NOT create .mato/ — the repo is uninitialized.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"graph", "--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing .mato directory, got nil")
	}
	want := ".mato/ directory not found - run 'mato init' first"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want containing %q", err.Error(), want)
	}
}

func TestInspectCmd_MissingMatoDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	// Do NOT create .mato/ — the repo is uninitialized.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"inspect", "some-task", "--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing .mato directory, got nil")
	}
	want := ".mato/ directory not found - run 'mato init' first"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want containing %q", err.Error(), want)
	}
}

func TestInspectCmd_InvalidFormat(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"inspect", "sample", "--format", "yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid inspect format, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestInspectCmd_ExactArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing ref", args: []string{"inspect"}},
		{name: "extra ref", args: []string{"inspect", "one", "two"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected arg validation error, got nil")
			}
		})
	}
}

func TestInspectCmd_DelegatesToInspectShow(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	var gotRepo, gotRef, gotFormat string
	var gotWriter io.Writer

	orig := inspectShowFn
	defer func() { inspectShowFn = orig }()

	inspectShowFn = func(w io.Writer, repoRootArg, taskRef, format string) error {
		gotWriter = w
		gotRepo = repoRootArg
		gotRef = taskRef
		gotFormat = format
		return nil
	}

	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"inspect", "sample-task", "--repo", repoRoot, "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotRepo != repoRoot {
		t.Errorf("repo = %q, want %q", gotRepo, repoRoot)
	}
	if gotRef != "sample-task" {
		t.Errorf("taskRef = %q, want %q", gotRef, "sample-task")
	}
	if gotFormat != "json" {
		t.Errorf("format = %q, want %q", gotFormat, "json")
	}
	if gotWriter != &out {
		t.Fatalf("writer = %T %p, want command output writer %p", gotWriter, gotWriter, &out)
	}
}

func TestInspectCmd_TextOutputUsesCommandWriters(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirBacklog, "sample.md"), "---\nid: sample\npriority: 1\n---\n# Sample task\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"inspect", "sample", "--repo", repoRoot, "--format", "text"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("inspect command failed: %v", err)
	}

	if got := stdout.String(); !strings.Contains(got, "Task: sample") || !strings.Contains(got, "File: backlog/sample.md") {
		t.Fatalf("stdout = %q, want rendered inspect text on the command writer", got)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestInspectCmd_TextWriterErrorPropagates(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	testutil.WriteFile(t, filepath.Join(tasksDir, queue.DirBacklog, "sample.md"), "---\nid: sample\npriority: 1\n---\n# Sample task\n")

	writeErr := errors.New("broken pipe")
	fw := &failAfterNWriter{n: 1, err: writeErr}

	cmd := newRootCmd()
	cmd.SetOut(fw)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"inspect", "sample", "--repo", repoRoot, "--format", "text"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected writer error, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want wrapped %v", err, writeErr)
	}
}

func TestInspectCmd_SubcommandRegistered(t *testing.T) {
	cmd := newRootCmd()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "inspect" {
			return
		}
	}
	t.Fatal("inspect subcommand not registered")
}

func TestLogCmd_InvalidFormat(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"log", "--format", "yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid log format, got nil")
	}
	if err.Error() != "--format must be text or json, got yaml" {
		t.Fatalf("error = %q, want %q", err.Error(), "--format must be text or json, got yaml")
	}
}

func TestLogCmd_NegativeLimitRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"log", "--limit", "-1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for negative limit, got nil")
	}
	if err.Error() != "--limit must be >= 0, got -1" {
		t.Fatalf("error = %q, want %q", err.Error(), "--limit must be >= 0, got -1")
	}
}

func TestLogCmd_DelegatesToLogShow(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	var gotRepo, gotFormat string
	var gotLimit int

	orig := logShowFn
	defer func() { logShowFn = orig }()

	logShowFn = func(w io.Writer, repo string, limit int, format string) error {
		gotRepo = repo
		gotLimit = limit
		gotFormat = format
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"log", "--repo", repoRoot, "--limit", "7", "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotRepo != repoRoot {
		t.Fatalf("repo = %q, want %q", gotRepo, repoRoot)
	}
	if gotLimit != 7 {
		t.Fatalf("limit = %d, want 7", gotLimit)
	}
	if gotFormat != "json" {
		t.Fatalf("format = %q, want json", gotFormat)
	}
}

func TestLogCmd_SubcommandRegistered(t *testing.T) {
	cmd := newRootCmd()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "log" {
			return
		}
	}
	t.Fatal("log subcommand not registered")
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
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--branch=foo..bar"})
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
	cmd.SetArgs([]string{"run", "--repo=" + dir})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-git repo, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error = %q, want error containing 'not a git repository'", err.Error())
	}
}

func TestValidateBranch_DelegatesToSharedValidator(t *testing.T) {
	if err := validateBranch("feature/shared-validator"); err != nil {
		t.Fatalf("validateBranch(valid): %v", err)
	}

	err := validateBranch("foo..bar")
	if err == nil {
		t.Fatal("expected invalid branch error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid branch name") {
		t.Errorf("error = %q, want invalid branch name", err.Error())
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
	var execErr error
	stderr := captureStderr(t, func() {
		execErr = cmd.Execute()
	})
	if !strings.Contains(stderr, "mato error: task nonexistent not found in failed/") {
		t.Fatalf("expected prefixed retry error, got %q", stderr)
	}
	if execErr == nil {
		t.Fatal("expected error for missing task")
	}
	var silentErr *SilentError
	if !errors.As(execErr, &silentErr) {
		t.Fatalf("expected SilentError, got %T: %v", execErr, execErr)
	}
	if !strings.Contains(execErr.Error(), "not found in failed/") {
		t.Errorf("unexpected error: %v", execErr)
	}
}

func TestRetryCmd_MissingMatoDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "missing"})
	var execErr error
	stderr := captureStderr(t, func() {
		execErr = cmd.Execute()
	})
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	if execErr == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(execErr.Error(), ".mato/ directory not found - run 'mato init' first") {
		t.Fatalf("unexpected error: %v", execErr)
	}
	var silentErr *SilentError
	if errors.As(execErr, &silentErr) {
		t.Fatalf("unexpected SilentError: %v", execErr)
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

func TestPauseCmd_Registered(t *testing.T) {
	cmd := newRootCmd()
	if got, _, err := cmd.Find([]string{"pause"}); err != nil || got == nil || got.Name() != "pause" {
		t.Fatalf("pause command not registered: cmd=%v err=%v", got, err)
	}
}

func TestResumeCmd_Registered(t *testing.T) {
	cmd := newRootCmd()
	if got, _, err := cmd.Find([]string{"resume"}); err != nil || got == nil || got.Name() != "resume" {
		t.Fatalf("resume command not registered: cmd=%v err=%v", got, err)
	}
}

func TestPauseCmd_CreatesSentinel(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"pause", "--repo", repoRoot})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pause command failed: %v", err)
		}
	})
	if !strings.HasPrefix(output, "Paused since ") {
		t.Fatalf("unexpected output: %q", output)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, ".paused")); err != nil {
		t.Fatalf("expected pause sentinel: %v", err)
	}
}

func TestPauseCmd_AlreadyPaused(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("2026-03-23T10:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"pause", "--repo", repoRoot})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pause command failed: %v", err)
		}
	})
	if !strings.HasPrefix(output, "Already paused since ") {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestPauseCmd_RepairsMalformedSentinel(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"pause", "--repo", repoRoot})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pause command failed: %v", err)
		}
	})
	if !strings.HasPrefix(output, "Repaired pause sentinel. Paused since ") {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestResumeCmd_RemovesSentinel(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("2026-03-23T10:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"resume", "--repo", repoRoot})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("resume command failed: %v", err)
		}
	})
	if strings.TrimSpace(output) != "Resumed" {
		t.Fatalf("unexpected output: %q", output)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, ".paused")); !os.IsNotExist(err) {
		t.Fatalf("expected sentinel removed, got err %v", err)
	}
}

func TestResumeCmd_NotPaused(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"resume", "--repo", repoRoot})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("resume command failed: %v", err)
		}
	})
	if strings.TrimSpace(output) != "Not paused" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestPauseResumeCmd_MissingTasksDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	for _, subcmd := range []string{"pause", "resume"} {
		t.Run(subcmd, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs([]string{subcmd, "--repo", repoRoot})
			out, code := renderCommandError(t, cmd.Execute())
			if code == 0 {
				t.Fatal("expected non-zero exit code")
			}
			if !strings.Contains(out, ".mato/ directory not found - run 'mato init' first") {
				t.Fatalf("unexpected output: %q", out)
			}
		})
	}
}

func TestPauseResumeCmd_RejectExtraArgs(t *testing.T) {
	for _, subcmd := range []string{"pause", "resume"} {
		t.Run(subcmd, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs([]string{subcmd, "extra"})
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected error with extra positional args")
			}
		})
	}
}

func TestPauseResumeCmd_InvalidRepo(t *testing.T) {
	for _, subcmd := range []string{"pause", "resume"} {
		t.Run(subcmd, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs([]string{subcmd, "--repo", "/nonexistent/repo"})
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected invalid repo error")
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

func TestRetryCmd_ExplicitID(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	failedDir := filepath.Join(repoRoot, ".mato", "failed")
	backlogDir := filepath.Join(repoRoot, ".mato", "backlog")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := "---\nid: explicit-id\n---\n# Fix bug\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "fix-bug.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "explicit-id"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("retry command failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backlogDir, "fix-bug.md")); err != nil {
		t.Fatal("task should be in backlog after retry by explicit id")
	}
}

func TestRetryCmd_ExplicitIDWithSlash(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	failedDir := filepath.Join(repoRoot, ".mato", "failed")
	backlogDir := filepath.Join(repoRoot, ".mato", "backlog")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := "---\nid: group/explicit-id\n---\n# Fix bug\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "fix-bug.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "group/explicit-id"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("retry command failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(backlogDir, "fix-bug.md")); err != nil {
		t.Fatal("task should be in backlog after retry by slash id")
	}
}

func TestCancelCmd_Registered(t *testing.T) {
	cmd := newRootCmd()
	if got, _, err := cmd.Find([]string{"cancel"}); err != nil || got == nil || got.Name() != "cancel" {
		t.Fatalf("cancel command not registered: cmd=%v err=%v", got, err)
	}
}

func TestCancelCmd_NoArgs(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error with no arguments")
	}
}

func TestCancelCmd_SingleTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel command failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "fix-bug.md")); err != nil {
		t.Fatalf("task should be in failed after cancel: %v", err)
	}
}

func TestCancelCmd_MultiTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	for _, name := range []string{"one.md", "two.md"} {
		if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, name), []byte("---\nid: "+strings.TrimSuffix(name, ".md")+"\n---\n# Task\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "one", "two"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel command failed: %v", err)
	}
	for _, name := range []string{"one.md", "two.md"} {
		if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, name)); err != nil {
			t.Fatalf("task %s should be in failed after cancel: %v", name, err)
		}
	}
}

func TestCancelCmd_PartialFailure(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "good.md"), []byte("---\nid: good\n---\n# Good\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "good", "missing"})
	var execErr error
	stderr := captureStderr(t, func() {
		execErr = cmd.Execute()
	})
	if !strings.Contains(stderr, "mato error: task not found: missing") {
		t.Fatalf("expected prefixed cancel error, got %q", stderr)
	}
	var silentErr *SilentError
	if !errors.As(execErr, &silentErr) {
		t.Fatalf("expected SilentError, got %T: %v", execErr, execErr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "good.md")); err != nil {
		t.Fatalf("successful cancel should still move task: %v", err)
	}
}

func TestCancelCmd_CompletedRefusal(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirCompleted, "done.md"), []byte("---\nid: done\n---\n# Done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "done"})
	var execErr error
	stderr := captureStderr(t, func() { execErr = cmd.Execute() })
	if !strings.Contains(stderr, "mato error: cannot cancel done: task has already been merged") {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	if execErr == nil {
		t.Fatal("expected error")
	}
}

func TestCancelCmd_MissingMatoDir(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "missing"})
	var execErr error
	stderr := captureStderr(t, func() { execErr = cmd.Execute() })
	if stderr != "" {
		t.Fatalf("unexpected stderr: %q", stderr)
	}
	if execErr == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(execErr.Error(), ".mato/ directory not found - run 'mato init' first") {
		t.Fatalf("unexpected error: %v", execErr)
	}
}

func TestCancelCmd_InProgressWarning(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirInProgress, "running.md"), []byte("---\nid: running\n---\n# Running\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "running"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel command failed: %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "cancelled: running.md (was in in-progress/)") {
		t.Fatalf("stdout = %q, want cancellation message on stdout", got)
	}
	if strings.Contains(stdout.String(), "warning:") {
		t.Fatalf("stdout leaked warning output: %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "warning: agent container for running may still be running") {
		t.Fatalf("missing in-progress warning on stderr: %q", got)
	}
}

func TestCancelCmd_ReadyToMergeWarning(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyMerge, "merge-me.md"), []byte("---\nid: merge-me\n---\n# Merge\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "merge-me"})
	stderr := captureStderr(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cancel command failed: %v", err)
		}
	})
	if !strings.Contains(stderr, "warning: merge queue may still merge merge-me's branch") {
		t.Fatalf("missing ready-to-merge warning: %q", stderr)
	}
}

func TestCancelCmd_ReadyForReviewWarning(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirReadyReview, "reviewing.md"), []byte("---\nid: reviewing\n---\n# Reviewing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "reviewing"})
	stderr := captureStderr(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cancel command failed: %v", err)
		}
	})
	if !strings.Contains(stderr, "warning: task is in ready-for-review/ — a review agent may be running") {
		t.Fatalf("missing ready-for-review warning: %q", stderr)
	}
}

func TestCancelCmd_DownstreamWarnings(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "dep.md"), []byte("---\nid: dep\n---\n# Dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "waiter.md"), []byte("---\nid: waiter\ndepends_on: [dep]\n---\n# Waiter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "dep"})
	stderr := captureStderr(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cancel command failed: %v", err)
		}
	})
	if !strings.Contains(stderr, "warning: 1 task(s) depend on dep:") || !strings.Contains(stderr, "waiting/waiter.md") {
		t.Fatalf("missing downstream warning output on stderr:\n%s", stderr)
	}
}

func TestCancelCmd_UsesRepoRootFromSubdir(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	subdir := filepath.Join(repoRoot, "nested")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(subdir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel command failed from subdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "fix-bug.md")); err != nil {
		t.Fatalf("task should be cancelled into repo-root failed/: %v", err)
	}
}

func TestStatusCmd_InvalidRepoPath(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantErr string
	}{
		{
			name:    "nonexistent path",
			repo:    "/nonexistent/path/that/does/not/exist",
			wantErr: "does not exist",
		},
		{
			name:    "not a git repo",
			repo:    t.TempDir(),
			wantErr: "not a git repository",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs([]string{"status", "--repo", tt.repo})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error for invalid repo path, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestCancelCmd_InvalidRepoPath(t *testing.T) {
	tests := []struct {
		name    string
		repo    string
		wantErr string
	}{
		{
			name:    "nonexistent path",
			repo:    "/nonexistent/path/that/does/not/exist",
			wantErr: "does not exist",
		},
		{
			name:    "not a git repo",
			repo:    t.TempDir(),
			wantErr: "not a git repository",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			cmd.SetArgs([]string{"cancel", "--repo", tt.repo, "some-task"})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error for invalid repo path, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
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
			got, err := configresolve.ResolveBranch(config.LoadResult{Config: configFixture(tt.cfg)}, tt.flag)
			if err != nil {
				t.Fatalf("resolveConfigBranch: %v", err)
			}
			if got.Value != tt.want {
				t.Fatalf("resolveConfigBranch(...) = %q, want %q", got.Value, tt.want)
			}
		})
	}
}

func TestResolveConfigBranch_WhitespaceEnvRejected(t *testing.T) {
	t.Setenv("MATO_BRANCH", "  ")
	_, err := configresolve.ResolveBranch(config.LoadResult{Config: configFixture(nil)}, "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestResolveRunOptions(t *testing.T) {
	stringPtr := func(v string) *string { return &v }

	t.Run("uses config values when env unset", func(t *testing.T) {
		resumeDisabled := false
		resolved, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixtureWithValues(stringPtr("custom:latest"), stringPtr("claude-sonnet-4"), stringPtr("gpt-5.4"), &resumeDisabled, stringPtr("medium"), stringPtr("xhigh"), stringPtr("45m"), stringPtr("5m"))})
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		opts := runOptionsFromResolvedConfig(resolved)
		if opts.DockerImage != "custom:latest" || opts.TaskModel != "claude-sonnet-4" || opts.ReviewModel != "gpt-5.4" || opts.ReviewSessionResumeEnabled || opts.TaskReasoningEffort != "medium" || opts.ReviewReasoningEffort != "xhigh" || opts.AgentTimeout != 45*time.Minute || opts.RetryCooldown != 5*time.Minute {
			t.Fatalf("opts = %+v", opts)
		}
	})

	t.Run("whitespace-only docker image env falls back to config", func(t *testing.T) {
		t.Setenv("MATO_DOCKER_IMAGE", "  \t ")
		resolved, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixtureWithValues(stringPtr("from-config:1.0"), nil, nil, nil, nil, nil, nil, nil)})
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		opts := runOptionsFromResolvedConfig(resolved)
		if opts.DockerImage != "from-config:1.0" {
			t.Errorf("DockerImage = %q, want %q", opts.DockerImage, "from-config:1.0")
		}
	})

	t.Run("real docker image env still overrides config", func(t *testing.T) {
		t.Setenv("MATO_DOCKER_IMAGE", "from-env:2.0")
		resolved, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixtureWithValues(stringPtr("from-config:1.0"), nil, nil, nil, nil, nil, nil, nil)})
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		opts := runOptionsFromResolvedConfig(resolved)
		if opts.DockerImage != "from-env:2.0" {
			t.Errorf("DockerImage = %q, want %q", opts.DockerImage, "from-env:2.0")
		}
	})

	t.Run("env overrides invalid config", func(t *testing.T) {
		t.Setenv("MATO_AGENT_TIMEOUT", "1h")
		t.Setenv("MATO_RETRY_COOLDOWN", "90s")
		resolved, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixtureWithValues(nil, nil, nil, nil, nil, nil, stringPtr("bad"), stringPtr("also-bad"))})
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		opts := runOptionsFromResolvedConfig(resolved)
		if opts.AgentTimeout != time.Hour || opts.RetryCooldown != 90*time.Second {
			t.Fatalf("opts = %+v", opts)
		}
	})

	t.Run("invalid effective config errors", func(t *testing.T) {
		_, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixtureWithValues(nil, nil, nil, nil, nil, nil, stringPtr("bad"), nil)})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("review session resume defaults to enabled", func(t *testing.T) {
		resolved, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixture(nil)})
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		opts := runOptionsFromResolvedConfig(resolved)
		if !opts.ReviewSessionResumeEnabled {
			t.Fatal("ReviewSessionResumeEnabled should default to true")
		}
	})

	t.Run("review session resume env overrides config", func(t *testing.T) {
		resumeEnabled := true
		t.Setenv("MATO_REVIEW_SESSION_RESUME_ENABLED", "false")
		resolved, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixtureWithValues(nil, nil, nil, &resumeEnabled, nil, nil, nil, nil)})
		if err != nil {
			t.Fatalf("resolveRunOptions: %v", err)
		}
		opts := runOptionsFromResolvedConfig(resolved)
		if opts.ReviewSessionResumeEnabled {
			t.Fatal("ReviewSessionResumeEnabled should respect env override")
		}
	})

	t.Run("invalid review session resume env errors", func(t *testing.T) {
		t.Setenv("MATO_REVIEW_SESSION_RESUME_ENABLED", "maybe")
		_, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixture(nil)})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "MATO_REVIEW_SESSION_RESUME_ENABLED") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("invalid effective env timeout errors", func(t *testing.T) {
		t.Setenv("MATO_AGENT_TIMEOUT", "bad")
		_, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixture(nil)})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("invalid env cooldown rejected", func(t *testing.T) {
		t.Setenv("MATO_RETRY_COOLDOWN", "bad")
		_, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixture(nil)})
		if err == nil {
			t.Fatal("expected error for invalid MATO_RETRY_COOLDOWN, got nil")
		}
		if !strings.Contains(err.Error(), "MATO_RETRY_COOLDOWN") {
			t.Fatalf("error should mention MATO_RETRY_COOLDOWN: %v", err)
		}
	})

	t.Run("non-positive env cooldown rejected", func(t *testing.T) {
		t.Setenv("MATO_RETRY_COOLDOWN", "-5m")
		_, err := configresolve.ResolveRunConfig(configresolve.RunFlags{}, config.LoadResult{Config: configFixture(nil)})
		if err == nil {
			t.Fatal("expected error for non-positive MATO_RETRY_COOLDOWN, got nil")
		}
		if !strings.Contains(err.Error(), "positive") {
			t.Fatalf("error should mention positive: %v", err)
		}
	})

	t.Run("reasoning effort validation", func(t *testing.T) {
		_, err := configresolve.ResolveRunConfig(configresolve.RunFlags{TaskReasoningEffort: "invalid"}, config.LoadResult{Config: configFixture(nil)})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "task-reasoning-effort") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestResolveRunConfig_StringPrecedence(t *testing.T) {
	configVal := "from-config"
	t.Setenv("MATO_TASK_MODEL", "from-env")
	resolved, err := configresolve.ResolveRunConfig(
		configresolve.RunFlags{TaskModel: " from-flag "},
		config.LoadResult{Config: config.Config{TaskModel: &configVal}},
	)
	if err != nil {
		t.Fatalf("ResolveRunConfig: %v", err)
	}
	if resolved.TaskModel.Value != "from-flag" || resolved.TaskModel.Source != configresolve.SourceFlag {
		t.Fatalf("TaskModel = %+v, want flag precedence", resolved.TaskModel)
	}

	resolved, err = configresolve.ResolveRunConfig(
		configresolve.RunFlags{TaskModel: "  "},
		config.LoadResult{Config: config.Config{TaskModel: &configVal}},
	)
	if err != nil {
		t.Fatalf("ResolveRunConfig: %v", err)
	}
	if resolved.TaskModel.Value != "from-env" || resolved.TaskModel.Source != configresolve.SourceEnv {
		t.Fatalf("TaskModel = %+v, want env precedence", resolved.TaskModel)
	}

	t.Setenv("MATO_TASK_MODEL", "  ")
	resolved, err = configresolve.ResolveRunConfig(
		configresolve.RunFlags{},
		config.LoadResult{Config: config.Config{TaskModel: &configVal}},
	)
	if err != nil {
		t.Fatalf("ResolveRunConfig: %v", err)
	}
	if resolved.TaskModel.Value != "from-config" || resolved.TaskModel.Source != configresolve.SourceConfig {
		t.Fatalf("TaskModel = %+v, want config precedence", resolved.TaskModel)
	}
}

func TestConfigFile_BranchFromConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	writeRepoConfig(t, repoRoot, "branch: main\n")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	called := false
	runFn = func(repoRootArg, branch string, opts runner.RunOptions) error {
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
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
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

	runFn = func(_ string, branch string, _ runner.RunOptions) error {
		if branch != "main" {
			t.Fatalf("branch = %q, want %q", branch, "main")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
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

	runFn = func(_ string, branch string, _ runner.RunOptions) error {
		if branch != "feature" {
			t.Fatalf("branch = %q, want %q", branch, "feature")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--branch", "feature"})
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

	runFn = func(_ string, branch string, _ runner.RunOptions) error {
		if branch != "env-branch" {
			t.Fatalf("branch = %q, want %q", branch, "env-branch")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_MissingConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, branch string, opts runner.RunOptions) error {
		if branch != "mato" {
			t.Fatalf("branch = %q, want %q", branch, "mato")
		}
		if opts != defaultResolvedRunOptions() {
			t.Fatalf("opts = %+v, want zero value", opts)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_InvalidYAML(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "branch: [\n")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
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
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
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

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.AgentTimeout != time.Hour {
			t.Fatalf("AgentTimeout = %v, want %v", opts.AgentTimeout, time.Hour)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
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

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.RetryCooldown != 90*time.Second {
			t.Fatalf("RetryCooldown = %v, want %v", opts.RetryCooldown, 90*time.Second)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestConfigFile_RunOptionsFromConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, strings.Join([]string{
		"docker_image: custom:latest",
		"task_model: claude-sonnet-4",
		"review_model: gpt-5.4",
		"task_reasoning_effort: medium",
		"review_reasoning_effort: high",
		"agent_timeout: 45m",
		"retry_cooldown: 5m",
		"",
	}, "\n"))

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.DockerImage != "custom:latest" || opts.TaskModel != "claude-sonnet-4" || opts.ReviewModel != "gpt-5.4" || !opts.ReviewSessionResumeEnabled || opts.TaskReasoningEffort != "medium" || opts.ReviewReasoningEffort != "high" || opts.AgentTimeout != 45*time.Minute || opts.RetryCooldown != 5*time.Minute {
			t.Fatalf("opts = %+v", opts)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestRunCmd_WhitespaceOnlyDockerImageEnvFallsBackToConfig(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	writeRepoConfig(t, repoRoot, "docker_image: from-config:1.0\n")

	t.Setenv("MATO_DOCKER_IMAGE", "   ")

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.DockerImage != "from-config:1.0" {
			t.Fatalf("DockerImage = %q, want %q (whitespace-only env should fall back to config)", opts.DockerImage, "from-config:1.0")
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestRunCmd_TaskModelFlagOverridesResolvedOptions(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.TaskModel != "claude-sonnet-4" {
			t.Fatalf("TaskModel = %q, want %q", opts.TaskModel, "claude-sonnet-4")
		}
		if opts.ReviewModel != config.DefaultReviewModel {
			t.Fatalf("ReviewModel = %q, want %q", opts.ReviewModel, config.DefaultReviewModel)
		}
		if opts.TaskReasoningEffort != config.DefaultReasoningEffort {
			t.Fatalf("TaskReasoningEffort = %q, want %q", opts.TaskReasoningEffort, config.DefaultReasoningEffort)
		}
		if opts.ReviewReasoningEffort != config.DefaultReasoningEffort {
			t.Fatalf("ReviewReasoningEffort = %q, want %q", opts.ReviewReasoningEffort, config.DefaultReasoningEffort)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--task-model", "claude-sonnet-4"})
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

	var gotWriter io.Writer
	dryRunFn = func(w io.Writer, _ string, branch string, opts runner.RunOptions) error {
		gotWriter = w
		if branch != "main" {
			t.Fatalf("branch = %q, want %q", branch, "main")
		}
		if opts != defaultResolvedRunOptions() {
			t.Fatalf("opts = %+v", opts)
		}
		return nil
	}

	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotWriter != &out {
		t.Fatalf("dryRun writer = %T %p, want command output writer %p", gotWriter, gotWriter, &out)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func TestRunCmd_OnceSetsRunMode(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.Mode != runner.RunModeOnce {
			t.Fatalf("Mode = %v, want %v", opts.Mode, runner.RunModeOnce)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--once"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestRunCmd_UntilIdleSetsRunMode(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	origRunFn := runFn
	defer func() { runFn = origRunFn }()

	runFn = func(_ string, _ string, opts runner.RunOptions) error {
		if opts.Mode != runner.RunModeUntilIdle {
			t.Fatalf("Mode = %v, want %v", opts.Mode, runner.RunModeUntilIdle)
		}
		return nil
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--until-idle"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestRunCmd_BoundedFlagsMutuallyExclusive(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "dry-run and once",
			args: []string{"run", "--repo", repoRoot, "--dry-run", "--once"},
			want: "--dry-run and --once are mutually exclusive",
		},
		{
			name: "dry-run and until-idle",
			args: []string{"run", "--repo", repoRoot, "--dry-run", "--until-idle"},
			want: "--dry-run and --until-idle are mutually exclusive",
		},
		{
			name: "once and until-idle",
			args: []string{"run", "--repo", repoRoot, "--once", "--until-idle"},
			want: "--once and --until-idle are mutually exclusive",
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
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestConfigFile_WhitespaceEnvBranchRejected(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "   ")

	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "MATO_BRANCH must not be whitespace-only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunCmd_WhitespaceOnlyBranchFlagRejected(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"run", "--repo", repoRoot, "--branch", "   "})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--branch must not be whitespace-only") {
		t.Fatalf("err = %v, want whitespace-only branch error", err)
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

func TestInitCmd_WhitespaceOnlyBranchFlagRejected(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot, "--branch", "   "})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--branch must not be whitespace-only") {
		t.Fatalf("err = %v, want whitespace-only branch error", err)
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

func defaultResolvedRunOptions() runner.RunOptions {
	return runner.RunOptions{
		DockerImage:                config.DefaultDockerImage,
		TaskModel:                  config.DefaultTaskModel,
		ReviewModel:                config.DefaultReviewModel,
		ReviewSessionResumeEnabled: true,
		TaskReasoningEffort:        config.DefaultReasoningEffort,
		ReviewReasoningEffort:      config.DefaultReasoningEffort,
		AgentTimeout:               config.DefaultAgentTimeout,
		RetryCooldown:              config.DefaultRetryCooldown,
	}
}

func resolvedRunConfigFixture(dockerImage, taskModel, reviewModel string, reviewSessionResumeEnabled bool, taskReasoningEffort, reviewReasoningEffort string, agentTimeout, retryCooldown time.Duration) configresolve.RunConfig {
	return configresolve.RunConfig{
		DockerImage:                configresolve.Resolved[string]{Value: dockerImage},
		TaskModel:                  configresolve.Resolved[string]{Value: taskModel},
		ReviewModel:                configresolve.Resolved[string]{Value: reviewModel},
		ReviewSessionResumeEnabled: configresolve.Resolved[bool]{Value: reviewSessionResumeEnabled},
		TaskReasoningEffort:        configresolve.Resolved[string]{Value: taskReasoningEffort},
		ReviewReasoningEffort:      configresolve.Resolved[string]{Value: reviewReasoningEffort},
		AgentTimeout:               configresolve.Resolved[time.Duration]{Value: agentTimeout},
		RetryCooldown:              configresolve.Resolved[time.Duration]{Value: retryCooldown},
	}
}

func TestRunOptionsFromResolvedConfig(t *testing.T) {
	runCfg := resolvedRunConfigFixture("custom:latest", "claude-sonnet-4", "gpt-5.4", false, "medium", "xhigh", 45*time.Minute, 90*time.Second)

	got := runOptionsFromResolvedConfig(runCfg)
	want := runner.RunOptions{
		DockerImage:                "custom:latest",
		TaskModel:                  "claude-sonnet-4",
		ReviewModel:                "gpt-5.4",
		ReviewSessionResumeEnabled: false,
		TaskReasoningEffort:        "medium",
		ReviewReasoningEffort:      "xhigh",
		AgentTimeout:               45 * time.Minute,
		RetryCooldown:              90 * time.Second,
	}
	if got != want {
		t.Fatalf("runOptionsFromResolvedConfig() = %+v, want %+v", got, want)
	}
}

func configFixtureWithValues(dockerImage, taskModel, reviewModel *string, reviewSessionResume *bool, taskReasoningEffort, reviewReasoningEffort, agentTimeout, retryCooldown *string) config.Config {
	return config.Config{
		DockerImage:           dockerImage,
		TaskModel:             taskModel,
		ReviewModel:           reviewModel,
		ReviewSessionResume:   reviewSessionResume,
		TaskReasoningEffort:   taskReasoningEffort,
		ReviewReasoningEffort: reviewReasoningEffort,
		AgentTimeout:          agentTimeout,
		RetryCooldown:         retryCooldown,
	}
}

// --- JSON format tests for mutating commands ---

func TestInitCmd_FormatJSON(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv("MATO_BRANCH", "")
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--repo", repoRoot, "--format=json"})

	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("init --format=json failed: %v", err)
		}
	})

	var result setup.InitResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if result.BranchName != "mato" {
		t.Errorf("branch_name = %q, want %q", result.BranchName, "mato")
	}
	if len(result.DirsCreated) == 0 {
		t.Error("expected dirs_created to be non-empty on first init")
	}
}

func TestInitCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"init", "--format=yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestPauseCmd_FormatJSON(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"pause", "--repo", repoRoot, "--format=json"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pause --format=json failed: %v", err)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if _, ok := result["since"]; !ok {
		t.Error("expected 'since' field in JSON output")
	}
	if result["already_paused"] != false {
		t.Errorf("already_paused = %v, want false", result["already_paused"])
	}
}

func TestPauseCmd_FormatJSON_AlreadyPaused(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("2026-03-23T10:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"pause", "--repo", repoRoot, "--format=json"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("pause --format=json failed: %v", err)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if result["already_paused"] != true {
		t.Errorf("already_paused = %v, want true", result["already_paused"])
	}
}

func TestPauseCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"pause", "--format=yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestResumeCmd_FormatJSON(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("2026-03-23T10:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"resume", "--repo", repoRoot, "--format=json"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("resume --format=json failed: %v", err)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if result["was_active"] != true {
		t.Errorf("was_active = %v, want true", result["was_active"])
	}
}

func TestResumeCmd_FormatJSON_NotPaused(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	cmd := newRootCmd()
	cmd.SetArgs([]string{"resume", "--repo", repoRoot, "--format=json"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("resume --format=json failed: %v", err)
		}
	})

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if result["was_active"] != false {
		t.Errorf("was_active = %v, want false", result["was_active"])
	}
}

func TestResumeCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"resume", "--format=yaml"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestRetryCmd_FormatJSON_Success(t *testing.T) {
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
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "--format=json", "fix-bug"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("retry --format=json failed: %v", err)
		}
	})

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["task"] != "fix-bug" {
		t.Errorf("task = %v, want %q", items[0]["task"], "fix-bug")
	}
	if items[0]["requeued"] != true {
		t.Errorf("requeued = %v, want true", items[0]["requeued"])
	}
}

func TestRetryCmd_FormatJSON_PartialFailure(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	failedDir := filepath.Join(repoRoot, ".mato", "failed")
	backlogDir := filepath.Join(repoRoot, ".mato", "backlog")
	if err := os.MkdirAll(failedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backlogDir, 0o755); err != nil {
		t.Fatal(err)
	}

	content := "# Good\n\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(failedDir, "good.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "--format=json", "good", "missing"})
	var execErr error
	output := captureStdout(t, func() {
		execErr = cmd.Execute()
	})
	if execErr == nil {
		t.Fatal("expected error for partial failure")
	}

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0]["requeued"] != true {
		t.Errorf("first item requeued = %v, want true", items[0]["requeued"])
	}
	if items[1]["error"] == nil || items[1]["error"] == "" {
		t.Error("second item should have an error")
	}
}

func TestRetryCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--format=yaml", "foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestCancelCmd_FormatJSON_SingleTask(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "--format=json", "fix-bug"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cancel --format=json failed: %v", err)
		}
	})

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["task"] != "fix-bug" {
		t.Errorf("task = %v, want %q", items[0]["task"], "fix-bug")
	}
	if items[0]["cancelled"] != true {
		t.Errorf("cancelled = %v, want true", items[0]["cancelled"])
	}
	if items[0]["prior_state"] != queue.DirBacklog {
		t.Errorf("prior_state = %v, want %q", items[0]["prior_state"], queue.DirBacklog)
	}
}

func TestCancelCmd_FormatJSON_PartialFailure(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "good.md"), []byte("---\nid: good\n---\n# Good\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "--format=json", "good", "missing"})
	var execErr error
	output := captureStdout(t, func() {
		execErr = cmd.Execute()
	})
	if execErr == nil {
		t.Fatal("expected error for partial failure")
	}

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if items[0]["cancelled"] != true {
		t.Errorf("first item cancelled = %v, want true", items[0]["cancelled"])
	}
	if items[1]["error"] == nil || items[1]["error"] == "" {
		t.Error("second item should have an error")
	}
}

func TestCancelCmd_FormatJSON_DownstreamWarnings(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "dep.md"), []byte("---\nid: dep\n---\n# Dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirWaiting, "waiter.md"), []byte("---\nid: waiter\ndepends_on: [dep]\n---\n# Waiter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "--format=json", "dep"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cancel --format=json failed: %v", err)
		}
	})

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	warnings, ok := items[0]["warnings"].([]interface{})
	if !ok || len(warnings) == 0 {
		t.Error("expected warnings in JSON output for downstream dependencies")
	}
}

func TestCancelCmd_InvalidFormatRejected(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--format=yaml", "foo"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --format yaml, got nil")
	}
	want := "--format must be text or json, got yaml"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestCancelCmd_YesSkipsPrompt(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "--yes", "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel --yes failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "fix-bug.md")); err != nil {
		t.Fatalf("task should be in failed after cancel --yes: %v", err)
	}
}

func TestCancelCmd_NonTTYSkipsPrompt(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// In go test, stdin is not a TTY, so the prompt should be skipped.
	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel command failed (non-TTY): %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "fix-bug.md")); err != nil {
		t.Fatalf("task should be in failed after cancel (non-TTY): %v", err)
	}
}

func TestCancelCmd_ConfirmAbort(t *testing.T) {
	// confirmCancel is tested directly since in go test stdin is not a TTY,
	// meaning the interactive prompt path is not exercised via the command.
	if confirmCancel(strings.NewReader("n\n")) {
		t.Error("confirmCancel should return false for 'n'")
	}
	if confirmCancel(strings.NewReader("\n")) {
		t.Error("confirmCancel should return false for empty input")
	}
	if confirmCancel(strings.NewReader("no\n")) {
		t.Error("confirmCancel should return false for 'no'")
	}
}

func TestCancelCmd_ConfirmAccept(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"y", "y\n", true},
		{"Y", "Y\n", true},
		{"yes", "yes\n", true},
		{"YES", "YES\n", true},
		{"Yes", "Yes\n", true},
		{"n", "n\n", false},
		{"empty", "\n", false},
		{"no", "no\n", false},
		{"random", "maybe\n", false},
		{"eof", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := confirmCancel(strings.NewReader(tt.input))
			if got != tt.want {
				t.Errorf("confirmCancel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestCancelCmd_FormatJSONSkipsPrompt(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "--format=json", "fix-bug"})
	output := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cancel --format=json failed: %v", err)
		}
	})

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["cancelled"] != true {
		t.Errorf("cancelled = %v, want true", items[0]["cancelled"])
	}
}

func TestCancelCmd_YesShortFlag(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "fix-bug.md"), []byte("---\nid: fix-bug\n---\n# Fix bug\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "-y", "fix-bug"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel -y failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "fix-bug.md")); err != nil {
		t.Fatalf("task should be in failed after cancel -y: %v", err)
	}
}

func TestCancelCmd_InteractiveMixedRefs(t *testing.T) {
	// Simulate interactive TTY mode with mixed valid and invalid refs.
	// The valid task should still be cancelled (partial-failure semantics)
	// even though an invalid ref also appears in the batch.
	// Errors for unresolved refs must appear exactly once — from the cancel
	// loop after confirmation, not during prompt preparation.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "good.md"), []byte("---\nid: good\n---\n# Good\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origTermFn := stdinIsTerminalFn
	stdinIsTerminalFn = func() bool { return true }
	defer func() { stdinIsTerminalFn = origTermFn }()

	origConfirmFn := confirmCancelFn
	confirmCancelFn = func(_ io.Reader) bool { return true }
	defer func() { confirmCancelFn = origConfirmFn }()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "good", "missing"})

	var execErr error
	stderr := captureStderr(t, func() {
		execErr = cmd.Execute()
	})

	if !strings.Contains(stderr, "mato error: task not found: missing") {
		t.Fatalf("expected error about missing ref on stderr, got %q", stderr)
	}
	if strings.Count(stderr, "task not found: missing") != 1 {
		t.Fatalf("expected exactly one 'task not found' error, got stderr: %q", stderr)
	}

	var silentErr *SilentError
	if !errors.As(execErr, &silentErr) {
		t.Fatalf("expected SilentError for partial failure, got %T: %v", execErr, execErr)
	}

	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirFailed, "good.md")); err != nil {
		t.Fatalf("valid task should be in failed/ after interactive partial cancel: %v", err)
	}
}

func TestCancelCmd_InteractiveRejectMixedRefs(t *testing.T) {
	// When the user rejects the confirmation prompt with mixed refs,
	// no task-not-found errors should be emitted and no tasks cancelled.
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	if err := os.WriteFile(filepath.Join(tasksDir, queue.DirBacklog, "good.md"), []byte("---\nid: good\n---\n# Good\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origTermFn := stdinIsTerminalFn
	stdinIsTerminalFn = func() bool { return true }
	defer func() { stdinIsTerminalFn = origTermFn }()

	origConfirmFn := confirmCancelFn
	confirmCancelFn = func(_ io.Reader) bool { return false }
	defer func() { confirmCancelFn = origConfirmFn }()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"cancel", "--repo", repoRoot, "good", "missing"})

	var execErr error
	stderr := captureStderr(t, func() {
		execErr = cmd.Execute()
	})

	if execErr != nil {
		t.Fatalf("expected no error when prompt rejected, got %v", execErr)
	}
	if strings.Contains(stderr, "task not found") {
		t.Fatalf("rejecting prompt should not emit task-not-found errors, got stderr %q", stderr)
	}
	if _, err := os.Stat(filepath.Join(tasksDir, queue.DirBacklog, "good.md")); err != nil {
		t.Fatalf("task should remain in backlog after rejected prompt: %v", err)
	}
}

func TestRetryCmd_FormatJSON_DependencyBlockedWarnings(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	content := "---\nid: blocked\ndepends_on: [missing-dep]\n---\n# Blocked\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tasksDir, "failed", "blocked.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "--format=json", "blocked"})
	var stderr string
	output := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatalf("retry --format=json failed: %v", err)
			}
		})
	})

	// JSON mode should not leak warnings as prose to stderr.
	if strings.Contains(stderr, "warning:") {
		t.Errorf("stderr should be clean in JSON mode, got %q", stderr)
	}

	var items []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &items); err != nil {
		t.Fatalf("invalid JSON output: %v\noutput: %s", err, output)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0]["dependency_blocked"] != true {
		t.Errorf("dependency_blocked = %v, want true", items[0]["dependency_blocked"])
	}
	warnings, ok := items[0]["warnings"].([]interface{})
	if !ok || len(warnings) == 0 {
		t.Error("expected warnings in JSON output for dependency-blocked retry")
	}
	if ok && len(warnings) > 0 {
		w, _ := warnings[0].(string)
		if !strings.Contains(w, "dependency-blocked") {
			t.Errorf("warning = %q, want substring 'dependency-blocked'", w)
		}
	}
}

func TestRetryCmd_TextMode_DependencyBlockedWarningOnStderr(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)
	content := "---\nid: blocked\ndepends_on: [missing-dep]\n---\n# Blocked\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n"
	if err := os.WriteFile(filepath.Join(tasksDir, "failed", "blocked.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"retry", "--repo", repoRoot, "blocked"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("retry failed: %v", err)
	}

	if got := stdout.String(); !strings.Contains(got, "Requeued blocked to backlog") {
		t.Fatalf("stdout = %q, want requeue message on stdout", got)
	}
	if strings.Contains(stdout.String(), "warning:") {
		t.Fatalf("stdout leaked warning output: %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "dependency-blocked") {
		t.Errorf("stderr = %q, want warning about dependency block", got)
	}
}

func TestCompleteTaskNames_InspectAllDirs(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Place tasks in various directories.
	tasks := map[string]string{
		"backlog/add-feature.md":        "---\nid: add-feature\n---\n# Add feature\n",
		"in-progress/fix-login.md":      "---\nid: fix-login\n---\n# Fix login\n",
		"completed/old-task.md":         "---\nid: old-task\n---\n# Old task\n",
		"failed/broken-build.md":        "---\nid: broken-build\n---\n# Broken build\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n",
		"ready-for-review/review-me.md": "---\nid: review-me\n---\n# Review me\n",
		"waiting/blocked-task.md":       "---\nid: blocked-task\ndepends_on: [add-feature]\n---\n# Blocked task\n",
		"ready-to-merge/merge-ready.md": "---\nid: merge-ready\n---\n# Merge ready\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo := repoRoot
	fn := completeTaskNames(&repo, queue.AllDirs)
	completions, directive := fn(nil, nil, "")

	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
	// Should include tasks from all directories.
	want := []string{"add-feature", "fix-login", "old-task", "broken-build", "review-me", "blocked-task", "merge-ready"}
	for _, w := range want {
		found := false
		for _, c := range completions {
			if c == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("completions missing %q, got %v", w, completions)
		}
	}
}

func TestCompleteTaskNames_CancelExcludesCompletedAndFailed(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	tasks := map[string]string{
		"backlog/cancel-me.md":  "---\nid: cancel-me\n---\n# Cancel me\n",
		"completed/done.md":     "---\nid: done\n---\n# Done\n",
		"failed/already-bad.md": "---\nid: already-bad\n---\n# Already bad\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n",
		"in-progress/active.md": "---\nid: active\n---\n# Active\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cancelDirs := []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirFailed}
	repo := repoRoot
	fn := completeTaskNames(&repo, cancelDirs)
	completions, _ := fn(nil, nil, "")

	for _, c := range completions {
		if c == "done" {
			t.Errorf("cancel completions should not include %q from completed/", c)
		}
	}
	wantPresent := map[string]bool{"cancel-me": false, "active": false, "already-bad": false}
	for _, c := range completions {
		if _, ok := wantPresent[c]; ok {
			wantPresent[c] = true
		}
	}
	for name, found := range wantPresent {
		if !found {
			t.Errorf("completions missing %q, got %v", name, completions)
		}
	}
}

func TestCompleteTaskNames_RetryOnlyFailed(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	tasks := map[string]string{
		"backlog/not-failed.md": "---\nid: not-failed\n---\n# Not failed\n",
		"failed/retry-me.md":    "---\nid: retry-me\n---\n# Retry me\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo := repoRoot
	fn := completeTaskNames(&repo, []string{queue.DirFailed})
	completions, _ := fn(nil, nil, "")

	if len(completions) != 1 || completions[0] != "retry-me" {
		t.Errorf("retry completions = %v, want [retry-me]", completions)
	}
}

func TestCompleteTaskNames_PrefixFiltering(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	tasks := map[string]string{
		"backlog/add-dark-mode.md": "---\nid: add-dark-mode\n---\n# Add dark mode\n",
		"backlog/add-search.md":    "---\nid: add-search\n---\n# Add search\n",
		"backlog/fix-login.md":     "---\nid: fix-login\n---\n# Fix login\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo := repoRoot
	fn := completeTaskNames(&repo, queue.AllDirs)
	completions, _ := fn(nil, nil, "add-")

	for _, c := range completions {
		if !strings.HasPrefix(c, "add-") {
			t.Errorf("completion %q does not have prefix add-", c)
		}
	}
	if len(completions) != 2 {
		t.Errorf("expected 2 completions with prefix add-, got %v", completions)
	}
}

func TestCompleteTaskNames_FrontmatterIDDiffersFromStem(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	// Frontmatter id differs from filename stem.
	content := "---\nid: my-custom-id\n---\n# Custom ID task\n"
	if err := os.WriteFile(filepath.Join(tasksDir, "backlog", "different-name.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := repoRoot
	fn := completeTaskNames(&repo, queue.AllDirs)
	completions, _ := fn(nil, nil, "")

	hasStem := false
	hasID := false
	for _, c := range completions {
		if c == "different-name" {
			hasStem = true
		}
		if c == "my-custom-id" {
			hasID = true
		}
	}
	if !hasStem {
		t.Errorf("completions missing filename stem different-name, got %v", completions)
	}
	if !hasID {
		t.Errorf("completions missing frontmatter id my-custom-id, got %v", completions)
	}
}

func TestCompleteTaskNames_EmptyQueue(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)

	repo := repoRoot
	fn := completeTaskNames(&repo, queue.AllDirs)
	completions, directive := fn(nil, nil, "")

	if len(completions) != 0 {
		t.Errorf("expected no completions for empty queue, got %v", completions)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
}

func TestCompleteTaskNames_InvalidRepo(t *testing.T) {
	badPath := t.TempDir()
	fn := completeTaskNames(&badPath, queue.AllDirs)
	completions, directive := fn(nil, nil, "")

	if len(completions) != 0 {
		t.Errorf("expected no completions for invalid repo, got %v", completions)
	}
	if directive != cobra.ShellCompDirectiveError {
		t.Errorf("directive = %v, want ShellCompDirectiveError", directive)
	}
}

func TestCompleteTaskNames_GitRepoWithoutMato(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)

	repo := repoRoot
	fn := completeTaskNames(&repo, queue.AllDirs)
	completions, directive := fn(nil, nil, "")

	if len(completions) != 0 {
		t.Errorf("expected no completions for repo without .mato, got %v", completions)
	}
	if directive != cobra.ShellCompDirectiveError {
		t.Errorf("directive = %v, want ShellCompDirectiveError", directive)
	}
}

func TestCompleteTaskNames_ParseFailureInAllDirs(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	tasks := map[string]string{
		"backlog/good-task.md":  "---\nid: good-task\n---\n# Good task\n",
		"backlog/malformed.md":  "---\nbad yaml: [unmatched\n---\n# Malformed\n",
		"failed/broken-yaml.md": "---\nalso bad: {{{\n---\n# Broken\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo := repoRoot
	fn := completeTaskNames(&repo, queue.AllDirs)
	completions, directive := fn(nil, nil, "")

	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}

	// Stems and filenames for parse-failure entries should both appear.
	want := []string{"good-task", "malformed", "malformed.md", "broken-yaml", "broken-yaml.md"}
	for _, w := range want {
		found := false
		for _, c := range completions {
			if c == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("completions missing %q, got %v", w, completions)
		}
	}
}

func TestCompleteTaskNames_RetryParseFailureInFailed(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	tasks := map[string]string{
		"backlog/not-failed.md":     "---\nid: not-failed\n---\n# Not failed\n",
		"failed/retry-me.md":        "---\nid: retry-me\n---\n# Retry me\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n",
		"failed/bad-frontmatter.md": "---\nbad: {{{\n---\n# Bad\n<!-- failure: abc at 2026-01-01T00:00:00Z step=WORK error=oops -->\n",
		"backlog/also-malformed.md": "---\nbad: [[[[\n---\n# Also bad\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	repo := repoRoot
	fn := completeTaskNames(&repo, []string{queue.DirFailed})
	completions, _ := fn(nil, nil, "")

	// Both stem and filename should be offered for the parse-failure entry.
	wantPresent := map[string]bool{
		"retry-me":           false,
		"bad-frontmatter":    false,
		"bad-frontmatter.md": false,
	}
	for _, c := range completions {
		if _, ok := wantPresent[c]; ok {
			wantPresent[c] = true
		}
	}
	for name, found := range wantPresent {
		if !found {
			t.Errorf("completions missing %q, got %v", name, completions)
		}
	}

	// Parse failure in backlog and its filename should NOT appear in retry.
	for _, c := range completions {
		if c == "also-malformed" || c == "also-malformed.md" || c == "not-failed" {
			t.Errorf("retry completions should not include %q, got %v", c, completions)
		}
	}
}

func TestCompleteTaskNames_CancelExcludesParseFailureInTerminalStates(t *testing.T) {
	repoRoot, tasksDir := testutil.SetupRepoWithTasks(t)

	tasks := map[string]string{
		"backlog/cancel-me.md":       "---\nid: cancel-me\n---\n# Cancel me\n",
		"backlog/bad-backlog.md":     "---\nbad: [unmatched\n---\n# Bad backlog\n",
		"failed/bad-failed.md":       "---\nbad: {{{\n---\n# Bad failed\n",
		"completed/bad-completed.md": "---\nbad: [[[[\n---\n# Bad completed\n",
	}
	for relPath, content := range tasks {
		if err := os.WriteFile(filepath.Join(tasksDir, relPath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cancelDirs := []string{queue.DirWaiting, queue.DirBacklog, queue.DirInProgress, queue.DirReadyReview, queue.DirReadyMerge, queue.DirFailed}
	repo := repoRoot
	fn := completeTaskNames(&repo, cancelDirs)
	completions, _ := fn(nil, nil, "")

	// Parse failures in backlog and failed should appear with both stem and filename.
	wantPresent := []string{"bad-backlog", "bad-backlog.md", "bad-failed", "bad-failed.md"}
	for _, w := range wantPresent {
		found := false
		for _, c := range completions {
			if c == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("completions missing %q (parse failure in cancellable state), got %v", w, completions)
		}
	}

	// Parse failures in completed/ should NOT appear (stem or filename).
	for _, c := range completions {
		if c == "bad-completed" || c == "bad-completed.md" {
			t.Errorf("cancel completions should not include %q from completed/, got %v", c, completions)
		}
	}
}
