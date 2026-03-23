package runner

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildDockerArgs_GhConfigMount(t *testing.T) {
	env := envConfig{
		homeDir:     "/home/test",
		image:       "ubuntu:24.04",
		workdir:     "/workspace",
		ghConfigDir: "/home/test/.config/gh",
		hasGhConfig: true,
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/home/test/.config/gh:/home/test/.config/gh:ro") {
		t.Fatal("gh config directory should be bind-mounted read-only when hasGhConfig is true")
	}
}

func TestBuildDockerArgs_GhConfigNotMounted(t *testing.T) {
	env := envConfig{
		homeDir:     "/home/test",
		image:       "ubuntu:24.04",
		workdir:     "/workspace",
		ghConfigDir: "/home/test/.config/gh",
		hasGhConfig: false,
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, ".config/gh") {
		t.Fatal("gh config directory should not be mounted when hasGhConfig is false")
	}
}

func TestBuildDockerArgs_GitTemplatesMount(t *testing.T) {
	env := envConfig{
		homeDir:         "/home/test",
		image:           "ubuntu:24.04",
		workdir:         "/workspace",
		gitTemplatesDir: "/usr/share/git-core/templates",
		hasGitTemplates: true,
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/usr/share/git-core/templates:/usr/share/git-core/templates:ro") {
		t.Fatal("git templates directory should be bind-mounted read-only when hasGitTemplates is true")
	}
}

func TestBuildDockerArgs_SystemCertsMount(t *testing.T) {
	env := envConfig{
		homeDir:        "/home/test",
		image:          "ubuntu:24.04",
		workdir:        "/workspace",
		systemCertsDir: "/etc/ssl/certs",
		hasSystemCerts: true,
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/etc/ssl/certs:/etc/ssl/certs:ro") {
		t.Fatal("system certs directory should be bind-mounted read-only when hasSystemCerts is true")
	}
}

func TestBuildDockerArgs_AllOptionalMounts(t *testing.T) {
	env := envConfig{
		homeDir:         "/home/test",
		image:           "ubuntu:24.04",
		workdir:         "/workspace",
		ghConfigDir:     "/home/test/.config/gh",
		hasGhConfig:     true,
		gitTemplatesDir: "/usr/share/git-core/templates",
		hasGitTemplates: true,
		systemCertsDir:  "/etc/ssl/certs",
		hasSystemCerts:  true,
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	for _, want := range []string{".config/gh", "git-core/templates", "/etc/ssl/certs"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in docker args when all optional mounts are enabled", want)
		}
	}
}

func TestBuildDockerArgs_ExtraVolumes(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{prompt: "test"}

	extras := []string{
		"/host/data:/container/data:ro",
		"/host/logs:/container/logs",
	}
	args := buildDockerArgs(env, run, nil, extras)
	joined := strings.Join(args, " ")
	for _, vol := range extras {
		if !strings.Contains(joined, vol) {
			t.Fatalf("extra volume %q should appear in docker args", vol)
		}
	}
}

func TestBuildDockerArgs_ExtraEnvs(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{prompt: "test", agentID: "test-agent"}

	extras := []string{
		"CUSTOM_VAR=hello",
		"ANOTHER_VAR=world",
	}
	args := buildDockerArgs(env, run, extras, nil)

	for _, want := range extras {
		found := false
		for i, a := range args {
			if a == "-e" && i+1 < len(args) && args[i+1] == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("extra env %q not found in docker args", want)
		}
	}
}

func TestBuildDockerArgs_GitIdentity(t *testing.T) {
	env := envConfig{
		homeDir:  "/home/test",
		image:    "ubuntu:24.04",
		workdir:  "/workspace",
		gitName:  "Test User",
		gitEmail: "test@example.com",
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"GIT_AUTHOR_NAME=Test User",
		"GIT_COMMITTER_NAME=Test User",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_EMAIL=test@example.com",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q in docker args when git identity is set", want)
		}
	}
}

func TestBuildDockerArgs_EmptyGitIdentity(t *testing.T) {
	env := envConfig{
		homeDir:  "/home/test",
		image:    "ubuntu:24.04",
		workdir:  "/workspace",
		gitName:  "",
		gitEmail: "",
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	for i, a := range args {
		if a == "-e" && i+1 < len(args) {
			val := args[i+1]
			if strings.HasPrefix(val, "GIT_AUTHOR_NAME=") || strings.HasPrefix(val, "GIT_COMMITTER_NAME=") {
				t.Fatalf("GIT name env var should not be set when gitName is empty, got %q", val)
			}
			if strings.HasPrefix(val, "GIT_AUTHOR_EMAIL=") || strings.HasPrefix(val, "GIT_COMMITTER_EMAIL=") {
				t.Fatalf("GIT email env var should not be set when gitEmail is empty, got %q", val)
			}
		}
	}
}

func TestBuildDockerArgs_WhitespaceOnlyGitIdentity(t *testing.T) {
	env := envConfig{
		homeDir:  "/home/test",
		image:    "ubuntu:24.04",
		workdir:  "/workspace",
		gitName:  "   ",
		gitEmail: "  \t  ",
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	for i, a := range args {
		if a == "-e" && i+1 < len(args) {
			val := args[i+1]
			if strings.HasPrefix(val, "GIT_AUTHOR_NAME=") || strings.HasPrefix(val, "GIT_COMMITTER_NAME=") {
				t.Fatalf("GIT name env var should not be set when gitName is whitespace-only, got %q", val)
			}
		}
	}
}

func TestBuildDockerArgs_AgentIDEnvVar(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{prompt: "test", agentID: "abc12345"}

	args := buildDockerArgs(env, run, nil, nil)
	found := false
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && args[i+1] == "MATO_AGENT_ID=abc12345" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("MATO_AGENT_ID should be set in docker args")
	}
}

func TestBuildDockerArgs_MessagingEnvVars(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "MATO_MESSAGING_ENABLED=1") {
		t.Fatal("MATO_MESSAGING_ENABLED should be set in docker args")
	}
	if !strings.Contains(joined, "MATO_MESSAGES_DIR=/workspace/.tasks/messages") {
		t.Fatal("MATO_MESSAGES_DIR should be set in docker args")
	}
}

func TestBuildDockerArgs_CopilotArgsPassthrough(t *testing.T) {
	t.Setenv("MATO_DEFAULT_MODEL", "")
	env := envConfig{
		homeDir:     "/home/test",
		image:       "ubuntu:24.04",
		workdir:     "/workspace",
		copilotArgs: []string{"--verbose", "--no-cache"},
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	// copilotArgs should appear after the image name.
	imageIdx := -1
	for i, a := range args {
		if a == "ubuntu:24.04" {
			imageIdx = i
			break
		}
	}
	if imageIdx == -1 {
		t.Fatal("image not found in docker args")
	}
	tail := strings.Join(args[imageIdx:], " ")
	if !strings.Contains(tail, "--verbose") || !strings.Contains(tail, "--no-cache") {
		t.Fatalf("copilotArgs should be passed through after the image, got tail: %s", tail)
	}
}

func TestBuildDockerArgs_PromptInArgs(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{prompt: "my custom prompt"}

	args := buildDockerArgs(env, run, nil, nil)
	found := false
	for i, a := range args {
		if a == "-p" && i+1 < len(args) && args[i+1] == "my custom prompt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("prompt should be passed with -p flag in docker args")
	}
}

func TestParseAgentTimeout_SubSecondDuration(t *testing.T) {
	got, err := parseAgentTimeout("500ms")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 500*time.Millisecond {
		t.Fatalf("expected 500ms, got %v", got)
	}
}

func TestParseAgentTimeout_LargeDuration(t *testing.T) {
	got, err := parseAgentTimeout("100h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 100*time.Hour {
		t.Fatalf("expected 100h, got %v", got)
	}
}

func TestDefaultModel_FallbackToHardcoded(t *testing.T) {
	t.Setenv("MATO_DEFAULT_MODEL", "")
	if m := defaultModel(); m != "claude-opus-4.6" {
		t.Fatalf("expected hardcoded default model, got %q", m)
	}
}

func TestDefaultModel_EnvVarOverride(t *testing.T) {
	t.Setenv("MATO_DEFAULT_MODEL", "custom-model-v2")
	if m := defaultModel(); m != "custom-model-v2" {
		t.Fatalf("expected env var model, got %q", m)
	}
}

func TestHasModelArg_EmptySlice(t *testing.T) {
	if hasModelArg(nil) {
		t.Fatal("hasModelArg(nil) should return false")
	}
	if hasModelArg([]string{}) {
		t.Fatal("hasModelArg([]) should return false")
	}
}

func TestHasModelArg_ModelWithWhitespace(t *testing.T) {
	// hasModelArg trims whitespace, so "  --model  " should match.
	if !hasModelArg([]string{"  --model  ", "gpt-5"}) {
		t.Fatal("hasModelArg should match --model with surrounding whitespace")
	}
}

func TestHasModelArg_ModelEqualsWithValue(t *testing.T) {
	if !hasModelArg([]string{"--model=claude-opus-4.6"}) {
		t.Fatal("hasModelArg should match --model= syntax")
	}
}

func TestHasModelArg_UnrelatedFlags(t *testing.T) {
	if hasModelArg([]string{"--modeler", "--model-path", "--verbose"}) {
		t.Fatal("hasModelArg should not match --modeler or --model-path")
	}
}

func TestIsTerminal_RegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "terminal-test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if isTerminal(f) {
		t.Fatal("regular file should not be detected as a terminal")
	}
}

func TestValidateTasksDir_SuccessCreatesAbsPath(t *testing.T) {
	dir := t.TempDir()
	got, err := validateTasksDir(dir + "/.tasks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != dir+"/.tasks" {
		t.Fatalf("expected %q, got %q", dir+"/.tasks", got)
	}
}
