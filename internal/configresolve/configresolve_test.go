package configresolve

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ryansimmen/mato/internal/config"
	"github.com/ryansimmen/mato/internal/testutil"
	"github.com/ryansimmen/mato/internal/ui"
)

func TestResolveRepoDefaults_DefaultsOnly(t *testing.T) {
	repoRoot := testutil.SetupRepo(t)
	t.Setenv(envBranch.Name, "")
	resolved, err := ResolveRepoDefaults(repoRoot)
	if err != nil {
		t.Fatalf("ResolveRepoDefaults: %v", err)
	}
	if resolved.Branch.Value != config.DefaultBranch || resolved.Branch.Source != SourceDefault {
		t.Fatalf("Branch = %+v, want default mato", resolved.Branch)
	}
	if resolved.DockerImage.Value != config.DefaultDockerImage || resolved.DockerImage.Source != SourceDefault {
		t.Fatalf("DockerImage = %+v", resolved.DockerImage)
	}
	if resolved.AgentTimeout.Value != config.DefaultAgentTimeout.String() || resolved.AgentTimeout.Source != SourceDefault {
		t.Fatalf("AgentTimeout = %+v", resolved.AgentTimeout)
	}
	if resolved.RetryCooldown.Value != config.DefaultRetryCooldown.String() || resolved.RetryCooldown.Source != SourceDefault {
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
		if resolved.DockerImage.Value != "custom:latest" || resolved.TaskModel.Value != "claude-sonnet-4" || resolved.AgentTimeout.Value != 45*time.Minute || resolved.RetryCooldown.Value != 5*time.Minute {
			t.Fatalf("resolved = %+v", resolved)
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
		hint, ok := ui.ErrorHint(err)
		if !ok || !strings.Contains(hint, "positive duration like") || !strings.Contains(hint, envAgentTimeout.Name) {
			t.Fatalf("hint = %q, want env duration hint", hint)
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
		hint, ok := ui.ErrorHint(err)
		if !ok || !strings.Contains(hint, "--task-reasoning-effort") {
			t.Fatalf("hint = %q, want task reasoning hint", hint)
		}
	})

	t.Run("branch whitespace env includes hint", func(t *testing.T) {
		t.Setenv(envBranch.Name, "   ")
		_, err := ResolveBranch(config.LoadResult{}, "")
		if err == nil || !strings.Contains(err.Error(), envBranch.Name) {
			t.Fatalf("err = %v, want branch env error", err)
		}
		hint, ok := ui.ErrorHint(err)
		if !ok || !strings.Contains(hint, envBranch.Name) || !strings.Contains(hint, "valid git ref name") {
			t.Fatalf("hint = %q, want branch hint", hint)
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
		if resolved.Value != config.DefaultDockerImage || resolved.Source != SourceDefault {
			t.Fatalf("resolved = %+v", resolved)
		}
	})

	t.Run("unknown repo root falls back to env or default", func(t *testing.T) {
		resolved, err := ResolveDoctorDockerImage("")
		if err != nil {
			t.Fatalf("ResolveDoctorDockerImage: %v", err)
		}
		if resolved.Value != config.DefaultDockerImage || resolved.Source != SourceDefault {
			t.Fatalf("resolved = %+v", resolved)
		}
	})
}

func TestValidateRepoDefaults(t *testing.T) {
	t.Run("valid effective config has no issues", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "branch: main\ndocker_image: custom:latest\nagent_timeout: 45m\nretry_cooldown: 5m\nreview_session_resume_enabled: false\ntask_reasoning_effort: medium\nreview_reasoning_effort: xhigh\n")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 0 {
			t.Fatalf("Issues = %+v, want none", result.Issues)
		}
		if result.Resolved.Branch.Value != "main" || result.Resolved.DockerImage.Value != "custom:latest" {
			t.Fatalf("Resolved = %+v", result.Resolved)
		}
	})

	t.Run("invalid branch from config", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "branch: foo..bar\n")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 1 || result.Issues[0].Code != "config.invalid_branch" || result.Issues[0].ConfigPath == "" {
			t.Fatalf("Issues = %+v", result.Issues)
		}
	})

	t.Run("invalid branch from env", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "foo..bar")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 1 || result.Issues[0].EnvVar != envBranch.Name {
			t.Fatalf("Issues = %+v", result.Issues)
		}
	})

	t.Run("invalid env bool", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		t.Setenv(envReviewSessionResumeEnabled.Name, "maybe")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 1 || result.Issues[0].Code != "config.invalid_bool" {
			t.Fatalf("Issues = %+v", result.Issues)
		}
	})

	t.Run("invalid env duration", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		t.Setenv(envAgentTimeout.Name, "bad")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 1 || result.Issues[0].Code != "config.invalid_duration" || result.Issues[0].EnvVar != envAgentTimeout.Name {
			t.Fatalf("Issues = %+v", result.Issues)
		}
	})

	t.Run("invalid config durations and reasoning efforts accumulate", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "task_reasoning_effort: nope\nreview_reasoning_effort: wrong\nagent_timeout: bad\nretry_cooldown: 0s\n")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 4 {
			t.Fatalf("len(Issues) = %d, want 4: %+v", len(result.Issues), result.Issues)
		}
	})

	t.Run("fatal ambiguity error returned", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "branch: main\n")
		testutil.WriteFile(t, repoRoot+"/.mato.yml", "branch: main\n")

		_, err := ValidateRepoDefaults(repoRoot)
		if err == nil || !strings.Contains(err.Error(), "found both .mato.yaml and .mato.yml") {
			t.Fatalf("err = %v, want ambiguity error", err)
		}
	})

	t.Run("fatal parse error returned", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", ":\n  bad yaml: [unbalanced\n")

		_, err := ValidateRepoDefaults(repoRoot)
		if err == nil || !strings.Contains(err.Error(), "parse config file") {
			t.Fatalf("err = %v, want parse error", err)
		}
	})

	t.Run("branch validation uses shared git rules", func(t *testing.T) {
		repoRoot := testutil.SetupRepo(t)
		t.Setenv(envBranch.Name, "")
		testutil.WriteFile(t, repoRoot+"/.mato.yaml", "branch: foo..bar\n")

		result, err := ValidateRepoDefaults(repoRoot)
		if err != nil {
			t.Fatalf("ValidateRepoDefaults: %v", err)
		}
		if len(result.Issues) != 1 || result.Issues[0].Code != "config.invalid_branch" {
			t.Fatalf("Issues = %+v, want one invalid branch issue", result.Issues)
		}
	})
}
