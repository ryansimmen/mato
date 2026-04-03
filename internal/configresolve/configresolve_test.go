package configresolve

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"mato/internal/config"
	"mato/internal/queue"
	"mato/internal/runner"
	"mato/internal/testutil"
)

func TestResolveRepoDefaults_DefaultsOnly(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv(envBranch.Name, "")
	resolved, err := ResolveRepoDefaults(repoRoot)
	if err != nil {
		t.Fatalf("ResolveRepoDefaults: %v", err)
	}
	if resolved.Branch.Value != "mato" || resolved.Branch.Source != SourceDefault {
		t.Fatalf("Branch = %+v, want default mato", resolved.Branch)
	}
	if resolved.DockerImage.Value != runner.DefaultDockerImage || resolved.DockerImage.Source != SourceDefault {
		t.Fatalf("DockerImage = %+v", resolved.DockerImage)
	}
	if resolved.AgentTimeout.Value != runner.DefaultAgentTimeout.String() || resolved.AgentTimeout.Source != SourceDefault {
		t.Fatalf("AgentTimeout = %+v", resolved.AgentTimeout)
	}
	if resolved.RetryCooldown.Value != queue.DefaultRetryCooldown.String() || resolved.RetryCooldown.Source != SourceDefault {
		t.Fatalf("RetryCooldown = %+v", resolved.RetryCooldown)
	}
	if resolved.ConfigExists || resolved.ConfigPath != "" {
		t.Fatalf("config metadata = exists=%v path=%q, want no config", resolved.ConfigExists, resolved.ConfigPath)
	}
}

func TestResolveBranch_Sources(t *testing.T) {
	branch := "main"
	tests := []struct {
		name    string
		flag    string
		env     string
		config  *string
		want    string
		source  Source
		envVar  string
		wantErr string
	}{
		{name: "flag", flag: "feature", env: "env-branch", config: &branch, want: "feature", source: SourceFlag},
		{name: "env", env: "env-branch", config: &branch, want: "env-branch", source: SourceEnv, envVar: envBranch.Name},
		{name: "config", config: &branch, want: "main", source: SourceConfig},
		{name: "default", want: "mato", source: SourceDefault},
		{name: "whitespace env rejected", env: "   ", wantErr: "MATO_BRANCH must not be whitespace-only"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envBranch.Name, tt.env)
			got, err := ResolveBranch(config.LoadResult{Config: config.Config{Branch: tt.config}}, tt.flag)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveBranch: %v", err)
			}
			if got.Value != tt.want || got.Source != tt.source || got.EnvVar != tt.envVar {
				t.Fatalf("resolved = %+v, want value=%q source=%q env=%q", got, tt.want, tt.source, tt.envVar)
			}
		})
	}
}

func TestResolveRunConfig(t *testing.T) {
	stringPtr := func(v string) *string { return &v }
	boolPtr := func(v bool) *bool { return &v }

	t.Run("config values", func(t *testing.T) {
		resolved, err := ResolveRunConfig(RunFlags{}, config.LoadResult{Config: config.Config{
			DockerImage:           stringPtr("custom:latest"),
			TaskModel:             stringPtr("claude-sonnet-4"),
			ReviewModel:           stringPtr("gpt-5.4"),
			ReviewSessionResume:   boolPtr(false),
			TaskReasoningEffort:   stringPtr("medium"),
			ReviewReasoningEffort: stringPtr("xhigh"),
			AgentTimeout:          stringPtr("45m"),
			RetryCooldown:         stringPtr("5m"),
		}})
		if err != nil {
			t.Fatalf("ResolveRunConfig: %v", err)
		}
		if resolved.DockerImage.Source != SourceConfig || resolved.TaskModel.Source != SourceConfig || resolved.AgentTimeout.Source != SourceConfig {
			t.Fatalf("unexpected sources: %+v", resolved)
		}
		if got := resolved.RunOptions(); got.DockerImage != "custom:latest" || got.TaskModel != "claude-sonnet-4" || got.AgentTimeout != 45*time.Minute || got.RetryCooldown != 5*time.Minute {
			t.Fatalf("RunOptions = %+v", got)
		}
	})

	t.Run("env overrides include env var attribution", func(t *testing.T) {
		t.Setenv(envDockerImage.Name, "from-env:2.0")
		t.Setenv(envReviewSessionResumeEnabled.Name, "false")
		resolved, err := ResolveRunConfig(RunFlags{}, config.LoadResult{Config: config.Config{DockerImage: stringPtr("from-config:1.0")}})
		if err != nil {
			t.Fatalf("ResolveRunConfig: %v", err)
		}
		if resolved.DockerImage.Value != "from-env:2.0" || resolved.DockerImage.Source != SourceEnv || resolved.DockerImage.EnvVar != envDockerImage.Name {
			t.Fatalf("DockerImage = %+v", resolved.DockerImage)
		}
		if resolved.ReviewSessionResumeEnabled.Value || resolved.ReviewSessionResumeEnabled.EnvVar != envReviewSessionResumeEnabled.Name {
			t.Fatalf("ReviewSessionResumeEnabled = %+v", resolved.ReviewSessionResumeEnabled)
		}
	})

	t.Run("flag overrides only real run flags", func(t *testing.T) {
		resolved, err := ResolveRunConfig(RunFlags{TaskModel: "claude-sonnet-4"}, config.LoadResult{})
		if err != nil {
			t.Fatalf("ResolveRunConfig: %v", err)
		}
		if resolved.TaskModel.Source != SourceFlag || resolved.TaskModel.Value != "claude-sonnet-4" {
			t.Fatalf("TaskModel = %+v", resolved.TaskModel)
		}
		if resolved.DockerImage.Source != SourceDefault {
			t.Fatalf("DockerImage source = %q, want default", resolved.DockerImage.Source)
		}
	})

	t.Run("invalid env bool", func(t *testing.T) {
		t.Setenv(envReviewSessionResumeEnabled.Name, "maybe")
		_, err := ResolveRunConfig(RunFlags{}, config.LoadResult{})
		if err == nil || !strings.Contains(err.Error(), envReviewSessionResumeEnabled.Name) {
			t.Fatalf("err = %v, want %s parse error", err, envReviewSessionResumeEnabled.Name)
		}
	})

	t.Run("invalid env duration", func(t *testing.T) {
		t.Setenv(envAgentTimeout.Name, "bad")
		_, err := ResolveRunConfig(RunFlags{}, config.LoadResult{})
		if err == nil || !strings.Contains(err.Error(), envAgentTimeout.Name) {
			t.Fatalf("err = %v, want %s parse error", err, envAgentTimeout.Name)
		}
	})

	t.Run("invalid config duration", func(t *testing.T) {
		_, err := ResolveRunConfig(RunFlags{}, config.LoadResult{Config: config.Config{AgentTimeout: stringPtr("bad")}})
		if err == nil || !strings.Contains(err.Error(), "invalid agent_timeout") {
			t.Fatalf("err = %v, want invalid agent_timeout", err)
		}
	})

	t.Run("reasoning effort validation", func(t *testing.T) {
		_, err := ResolveRunConfig(RunFlags{TaskReasoningEffort: "invalid"}, config.LoadResult{})
		if err == nil || !strings.Contains(err.Error(), "task-reasoning-effort") {
			t.Fatalf("err = %v, want reasoning effort error", err)
		}
	})
}

func TestResolveRepoDefaults_ConfigPathAndRendering(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	testutil.WriteFile(t, repoRoot+"/.mato.yml", "branch: main\ndocker_image: custom:latest\n")
	resolved, err := ResolveRepoDefaults(repoRoot)
	if err != nil {
		t.Fatalf("ResolveRepoDefaults: %v", err)
	}
	if !resolved.ConfigExists || !strings.HasSuffix(resolved.ConfigPath, ".mato.yml") {
		t.Fatalf("config metadata = exists=%v path=%q", resolved.ConfigExists, resolved.ConfigPath)
	}
	var text bytes.Buffer
	if err := RenderText(&text, resolved); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	out := text.String()
	if !strings.Contains(out, "Config file: "+resolved.ConfigPath) || !strings.Contains(out, "branch") || !strings.Contains(out, "(config)") {
		t.Fatalf("unexpected text output:\n%s", out)
	}
	var json bytes.Buffer
	if err := RenderJSON(&json, resolved); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(json.String(), "\"config_path\"") || !strings.Contains(json.String(), "\"agent_timeout\": {") {
		t.Fatalf("unexpected json output:\n%s", json.String())
	}
}

func TestResolveDoctorDockerImage(t *testing.T) {
	t.Run("env override still validates config", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "docker_image: from-config\n")
		t.Setenv(envDockerImage.Name, "from-env")
		resolved, err := ResolveDoctorDockerImage(repoRoot)
		if err != nil {
			t.Fatalf("ResolveDoctorDockerImage: %v", err)
		}
		if resolved.Value != "from-env" || resolved.Source != SourceEnv {
			t.Fatalf("resolved = %+v", resolved)
		}
	})

	t.Run("docker image resolution ignores unrelated invalid run settings", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "task_reasoning_effort: nope\nagent_timeout: bad\n")
		resolved, err := ResolveDoctorDockerImage(repoRoot)
		if err != nil {
			t.Fatalf("ResolveDoctorDockerImage: %v", err)
		}
		if resolved.Value != runner.DefaultDockerImage || resolved.Source != SourceDefault {
			t.Fatalf("resolved = %+v", resolved)
		}
	})

	t.Run("unknown repo root falls back to env or default", func(t *testing.T) {
		resolved, err := ResolveDoctorDockerImage("")
		if err != nil {
			t.Fatalf("ResolveDoctorDockerImage: %v", err)
		}
		if resolved.Value != runner.DefaultDockerImage || resolved.Source != SourceDefault {
			t.Fatalf("resolved = %+v", resolved)
		}
	})
}
