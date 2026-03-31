package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withStatPathFn(t *testing.T, fn func(string) (os.FileInfo, error)) {
	t.Helper()
	orig := statPathFn
	statPathFn = fn
	t.Cleanup(func() { statPathFn = orig })
}

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

func TestBuildDockerArgs_CopilotCacheMount(t *testing.T) {
	env := envConfig{
		homeDir:         "/home/test",
		image:           "ubuntu:24.04",
		workdir:         "/workspace",
		copilotCacheDir: "/home/test/.cache/copilot",
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/home/test/.cache/copilot:/home/test/.cache/copilot") {
		t.Fatal("copilot cache directory should be bind-mounted")
	}
}

func TestBuildDockerArgs_SkipsMissingGoCacheMounts(t *testing.T) {
	withStatPathFn(t, func(path string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	})
	env := envConfig{
		homeDir:          "/home/test",
		image:            "ubuntu:24.04",
		workdir:          "/workspace",
		copilotConfigDir: "/home/test/.copilot",
		copilotCacheDir:  "/home/test/.cache/copilot",
	}
	run := runContext{prompt: "test"}

	_, stderr := captureStdoutStderr(t, func() {
		args := buildDockerArgs(env, run, nil, nil)
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "/home/test/go/pkg/mod:/home/test/go/pkg/mod") {
			t.Fatal("GOMODCACHE mount should be omitted when host cache path is missing")
		}
		if strings.Contains(joined, "/home/test/.cache/go-build:/home/test/.cache/go-build") {
			t.Fatal("GOCACHE mount should be omitted when host cache path is missing")
		}
	})
	if !strings.Contains(stderr, "skipping GOMODCACHE cache mount") {
		t.Fatalf("expected GOMODCACHE warning, got %q", stderr)
	}
	if !strings.Contains(stderr, "skipping GOCACHE cache mount") {
		t.Fatalf("expected GOCACHE warning, got %q", stderr)
	}
}

func TestBuildDockerArgs_IncludesExistingGoCacheMounts(t *testing.T) {
	homeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(homeDir, "go", "pkg", "mod"), 0o755); err != nil {
		t.Fatalf("MkdirAll GOMODCACHE: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(homeDir, ".cache", "go-build"), 0o755); err != nil {
		t.Fatalf("MkdirAll GOCACHE: %v", err)
	}
	env := envConfig{
		homeDir:          homeDir,
		image:            "ubuntu:24.04",
		workdir:          "/workspace",
		copilotConfigDir: filepath.Join(homeDir, ".copilot"),
		copilotCacheDir:  filepath.Join(homeDir, ".cache", "copilot"),
	}
	run := runContext{prompt: "test"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, filepath.Join(homeDir, "go", "pkg", "mod")+":"+homeDir+"/go/pkg/mod") {
		t.Fatal("expected GOMODCACHE mount when host cache path exists")
	}
	if !strings.Contains(joined, filepath.Join(homeDir, ".cache", "go-build")+":"+homeDir+"/.cache/go-build") {
		t.Fatal("expected GOCACHE mount when host cache path exists")
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
	if !strings.Contains(joined, "MATO_MESSAGES_DIR=/workspace/.mato/messages") {
		t.Fatal("MATO_MESSAGES_DIR should be set in docker args")
	}
}

func TestBuildDockerArgs_ModelAndReasoningEffort(t *testing.T) {
	env := envConfig{
		homeDir: "/home/test",
		image:   "ubuntu:24.04",
		workdir: "/workspace",
	}
	run := runContext{prompt: "test", model: "claude-opus-4.6", reasoningEffort: "high"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model claude-opus-4.6") {
		t.Fatalf("expected task model in docker args, got: %s", joined)
	}
	if !strings.Contains(joined, "--reasoning-effort high") {
		t.Fatalf("expected reasoning effort in docker args, got: %s", joined)
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

func TestBuildDockerArgs_DifferentModelValues(t *testing.T) {
	env := envConfig{homeDir: "/home/test", image: "ubuntu:24.04", workdir: "/workspace"}
	run := runContext{prompt: "test", model: "gpt-5.4", reasoningEffort: "xhigh"}

	args := buildDockerArgs(env, run, nil, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model gpt-5.4") {
		t.Fatalf("expected review model in docker args, got: %s", joined)
	}
	if !strings.Contains(joined, "--reasoning-effort xhigh") {
		t.Fatalf("expected xhigh reasoning effort in docker args, got: %s", joined)
	}
}

func TestBuildDockerArgs_AppendsResumeWhenSessionIDSet(t *testing.T) {
	env := envConfig{homeDir: "/home/test", image: "ubuntu:24.04", workdir: "/workspace"}
	run := runContext{prompt: "test", model: "gpt-5.4", reasoningEffort: "high", resumeSessionID: "session-123"}

	joined := strings.Join(buildDockerArgs(env, run, nil, nil), " ")
	if !strings.Contains(joined, "copilot --resume=session-123 -p test") {
		t.Fatalf("expected --resume in docker args, got: %s", joined)
	}
}

func TestBuildDockerArgs_OmitsResumeWhenSessionIDEmpty(t *testing.T) {
	env := envConfig{homeDir: "/home/test", image: "ubuntu:24.04", workdir: "/workspace"}
	run := runContext{prompt: "test", model: "gpt-5.4", reasoningEffort: "high"}

	joined := strings.Join(buildDockerArgs(env, run, nil, nil), " ")
	if strings.Contains(joined, "--resume=") {
		t.Fatalf("did not expect --resume in docker args, got: %s", joined)
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

// ---------- ensureDockerImage ----------

func TestEnsureDockerImage_Found(t *testing.T) {
	origInspect := dockerImageInspectFn
	t.Cleanup(func() { dockerImageInspectFn = origInspect })

	inspectCalled := false
	dockerImageInspectFn = func(image string) error {
		inspectCalled = true
		if image != "test:latest" {
			t.Errorf("expected image %q, got %q", "test:latest", image)
		}
		return nil
	}

	if err := ensureDockerImage("test:latest"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inspectCalled {
		t.Error("expected dockerImageInspectFn to be called")
	}
}

func TestEnsureDockerImage_NotFound_PullSucceeds(t *testing.T) {
	origInspect := dockerImageInspectFn
	origPull := dockerPullFn
	t.Cleanup(func() {
		dockerImageInspectFn = origInspect
		dockerPullFn = origPull
	})

	dockerImageInspectFn = func(image string) error {
		return fmt.Errorf("No such image: %s", image)
	}
	pullCalled := false
	dockerPullFn = func(image string) error {
		pullCalled = true
		if image != "myimage:v1" {
			t.Errorf("expected image %q, got %q", "myimage:v1", image)
		}
		return nil
	}

	stdout, _ := captureStdoutStderr(t, func() {
		if err := ensureDockerImage("myimage:v1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	if !pullCalled {
		t.Error("expected dockerPullFn to be called")
	}
	if !strings.Contains(stdout, "not found locally") {
		t.Errorf("expected 'not found locally' message, got: %s", stdout)
	}
	if !strings.Contains(stdout, "Pulling") {
		t.Errorf("expected 'Pulling' message, got: %s", stdout)
	}
}

func TestEnsureDockerImage_NotFound_PullFails(t *testing.T) {
	origInspect := dockerImageInspectFn
	origPull := dockerPullFn
	t.Cleanup(func() {
		dockerImageInspectFn = origInspect
		dockerPullFn = origPull
	})

	dockerImageInspectFn = func(image string) error {
		return fmt.Errorf("No such image: %s", image)
	}
	dockerPullFn = func(image string) error {
		return fmt.Errorf("network timeout")
	}

	_, _ = captureStdoutStderr(t, func() {
		err := ensureDockerImage("bad:image")
		if err == nil {
			t.Fatal("expected error when pull fails")
		}
		if !strings.Contains(err.Error(), "failed to pull Docker image bad:image") {
			t.Errorf("unexpected error message: %v", err)
		}
		if !strings.Contains(err.Error(), "verify the image name") {
			t.Errorf("expected actionable guidance in error: %v", err)
		}
	})
}
