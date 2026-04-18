// Package configresolve resolves repository configuration with source
// attribution across flags, environment variables, repo-local config, and
// built-in defaults.
package configresolve

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/config"
	"github.com/ryansimmen/mato/internal/ui"
)

type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceConfig  Source = "config"
	SourceDefault Source = "default"
)

type Resolved[T any] struct {
	Value  T      `json:"value"`
	Source Source `json:"source"`
	EnvVar string `json:"env_var,omitempty"`
}

type RepoDefaults struct {
	RepoRoot     string `json:"repo_root"`
	ConfigPath   string `json:"config_path,omitempty"`
	ConfigExists bool   `json:"config_exists"`

	Branch                     Resolved[string] `json:"branch"`
	DockerImage                Resolved[string] `json:"docker_image"`
	TaskModel                  Resolved[string] `json:"task_model"`
	ReviewModel                Resolved[string] `json:"review_model"`
	ReviewSessionResumeEnabled Resolved[bool]   `json:"review_session_resume_enabled"`
	TaskReasoningEffort        Resolved[string] `json:"task_reasoning_effort"`
	ReviewReasoningEffort      Resolved[string] `json:"review_reasoning_effort"`
	AgentTimeout               Resolved[string] `json:"agent_timeout"`
	RetryCooldown              Resolved[string] `json:"retry_cooldown"`
}

type RunFlags struct {
	TaskModel             string
	ReviewModel           string
	TaskReasoningEffort   string
	ReviewReasoningEffort string
}

type RunConfig struct {
	DockerImage                Resolved[string]
	TaskModel                  Resolved[string]
	ReviewModel                Resolved[string]
	ReviewSessionResumeEnabled Resolved[bool]
	TaskReasoningEffort        Resolved[string]
	ReviewReasoningEffort      Resolved[string]
	AgentTimeout               Resolved[time.Duration]
	RetryCooldown              Resolved[time.Duration]
}

type DoctorDockerConfig struct {
	DockerImage Resolved[string]
}

type envVarMeta struct {
	Name string
}

var (
	envBranch                     = envVarMeta{Name: "MATO_BRANCH"}
	envDockerImage                = envVarMeta{Name: "MATO_DOCKER_IMAGE"}
	envTaskModel                  = envVarMeta{Name: "MATO_TASK_MODEL"}
	envReviewModel                = envVarMeta{Name: "MATO_REVIEW_MODEL"}
	envReviewSessionResumeEnabled = envVarMeta{Name: "MATO_REVIEW_SESSION_RESUME_ENABLED"}
	envTaskReasoningEffort        = envVarMeta{Name: "MATO_TASK_REASONING_EFFORT"}
	envReviewReasoningEffort      = envVarMeta{Name: "MATO_REVIEW_REASONING_EFFORT"}
	envAgentTimeout               = envVarMeta{Name: "MATO_AGENT_TIMEOUT"}
	envRetryCooldown              = envVarMeta{Name: "MATO_RETRY_COOLDOWN"}
)

var validReasoningEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
}

func ResolveRepoDefaults(repoRoot string) (*RepoDefaults, error) {
	load, err := config.Load(repoRoot)
	if err != nil {
		return nil, err
	}
	branch, err := ResolveBranch(load, "")
	if err != nil {
		return nil, err
	}
	runCfg, err := ResolveRunConfig(RunFlags{}, load)
	if err != nil {
		return nil, err
	}
	view := &RepoDefaults{
		RepoRoot:                   repoRoot,
		ConfigPath:                 load.Path,
		ConfigExists:               load.Exists,
		Branch:                     branch,
		DockerImage:                runCfg.DockerImage,
		TaskModel:                  runCfg.TaskModel,
		ReviewModel:                runCfg.ReviewModel,
		ReviewSessionResumeEnabled: runCfg.ReviewSessionResumeEnabled,
		TaskReasoningEffort:        runCfg.TaskReasoningEffort,
		ReviewReasoningEffort:      runCfg.ReviewReasoningEffort,
		AgentTimeout:               formatDurationResolved(runCfg.AgentTimeout),
		RetryCooldown:              formatDurationResolved(runCfg.RetryCooldown),
	}
	return view, nil
}

func ResolveBranch(load config.LoadResult, flagValue string) (Resolved[string], error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return Resolved[string]{Value: v, Source: SourceFlag}, nil
	}
	envValue, ok, err := resolveEnvString(envBranch, true)
	if err != nil {
		return Resolved[string]{}, ui.WithHint(err, "set MATO_BRANCH to a valid git ref name such as mato or feature/my-change, or unset it to use the default")
	}
	if ok {
		return Resolved[string]{Value: envValue, Source: SourceEnv, EnvVar: envBranch.Name}, nil
	}
	if load.Config.Branch != nil {
		return Resolved[string]{Value: *load.Config.Branch, Source: SourceConfig}, nil
	}
	return Resolved[string]{Value: config.DefaultBranch, Source: SourceDefault}, nil
}

func ResolveRunConfig(flags RunFlags, load config.LoadResult) (RunConfig, error) {
	cfg := load.Config
	resolved := RunConfig{
		DockerImage:           resolveStringValue("", envDockerImage, cfg.DockerImage, config.DefaultDockerImage),
		TaskModel:             resolveStringValue(flags.TaskModel, envTaskModel, cfg.TaskModel, config.DefaultTaskModel),
		ReviewModel:           resolveStringValue(flags.ReviewModel, envReviewModel, cfg.ReviewModel, config.DefaultReviewModel),
		TaskReasoningEffort:   resolveStringValue(flags.TaskReasoningEffort, envTaskReasoningEffort, cfg.TaskReasoningEffort, config.DefaultReasoningEffort),
		ReviewReasoningEffort: resolveStringValue(flags.ReviewReasoningEffort, envReviewReasoningEffort, cfg.ReviewReasoningEffort, config.DefaultReasoningEffort),
	}

	resumeEnabled, err := resolveBoolValue(envReviewSessionResumeEnabled, cfg.ReviewSessionResume, true)
	if err != nil {
		return RunConfig{}, err
	}
	resolved.ReviewSessionResumeEnabled = resumeEnabled

	agentTimeout, err := resolveDurationValue(envAgentTimeout, cfg.AgentTimeout, "agent_timeout", config.DefaultAgentTimeout)
	if err != nil {
		return RunConfig{}, err
	}
	resolved.AgentTimeout = agentTimeout

	retryCooldown, err := resolveDurationValue(envRetryCooldown, cfg.RetryCooldown, "retry_cooldown", config.DefaultRetryCooldown)
	if err != nil {
		return RunConfig{}, err
	}
	resolved.RetryCooldown = retryCooldown

	if err := validateResolvedReasoningEffort(resolved.TaskReasoningEffort, "task-reasoning-effort", "task_reasoning_effort"); err != nil {
		return RunConfig{}, err
	}
	if err := validateResolvedReasoningEffort(resolved.ReviewReasoningEffort, "review-reasoning-effort", "review_reasoning_effort"); err != nil {
		return RunConfig{}, err
	}

	return resolved, nil
}

func ResolveDoctorDockerImage(repoRoot string) (Resolved[string], error) {
	if v, ok, err := resolveEnvString(envDockerImage, false); err != nil {
		return Resolved[string]{}, err
	} else if ok {
		if repoRoot != "" {
			if _, err := config.Load(repoRoot); err != nil {
				return Resolved[string]{}, err
			}
		}
		return Resolved[string]{Value: v, Source: SourceEnv, EnvVar: envDockerImage.Name}, nil
	}
	if repoRoot == "" {
		return Resolved[string]{Value: config.DefaultDockerImage, Source: SourceDefault}, nil
	}
	load, err := config.Load(repoRoot)
	if err != nil {
		return Resolved[string]{}, err
	}
	if load.Config.DockerImage != nil {
		return Resolved[string]{Value: *load.Config.DockerImage, Source: SourceConfig}, nil
	}
	return Resolved[string]{Value: config.DefaultDockerImage, Source: SourceDefault}, nil
}

func RenderText(w io.Writer, repoDefaults *RepoDefaults) error {
	if _, err := fmt.Fprintf(w, "Repo: %s\n", repoDefaults.RepoRoot); err != nil {
		return err
	}
	configPath := "none"
	if repoDefaults.ConfigExists {
		configPath = repoDefaults.ConfigPath
	}
	if _, err := fmt.Fprintf(w, "Config file: %s\n\n", configPath); err != nil {
		return err
	}
	rows := []struct {
		label  string
		value  string
		source Source
		envVar string
	}{
		{"branch", repoDefaults.Branch.Value, repoDefaults.Branch.Source, repoDefaults.Branch.EnvVar},
		{"docker_image", repoDefaults.DockerImage.Value, repoDefaults.DockerImage.Source, repoDefaults.DockerImage.EnvVar},
		{"task_model", repoDefaults.TaskModel.Value, repoDefaults.TaskModel.Source, repoDefaults.TaskModel.EnvVar},
		{"review_model", repoDefaults.ReviewModel.Value, repoDefaults.ReviewModel.Source, repoDefaults.ReviewModel.EnvVar},
		{"review_session_resume_enabled", fmt.Sprintf("%t", repoDefaults.ReviewSessionResumeEnabled.Value), repoDefaults.ReviewSessionResumeEnabled.Source, repoDefaults.ReviewSessionResumeEnabled.EnvVar},
		{"task_reasoning_effort", repoDefaults.TaskReasoningEffort.Value, repoDefaults.TaskReasoningEffort.Source, repoDefaults.TaskReasoningEffort.EnvVar},
		{"review_reasoning_effort", repoDefaults.ReviewReasoningEffort.Value, repoDefaults.ReviewReasoningEffort.Source, repoDefaults.ReviewReasoningEffort.EnvVar},
		{"agent_timeout", repoDefaults.AgentTimeout.Value, repoDefaults.AgentTimeout.Source, repoDefaults.AgentTimeout.EnvVar},
		{"retry_cooldown", repoDefaults.RetryCooldown.Value, repoDefaults.RetryCooldown.Source, repoDefaults.RetryCooldown.EnvVar},
	}
	maxLabel := 0
	maxValue := 0
	for _, row := range rows {
		label := row.label + ":"
		if len(label) > maxLabel {
			maxLabel = len(label)
		}
		if len(row.value) > maxValue {
			maxValue = len(row.value)
		}
	}
	for _, row := range rows {
		label := row.label + ":"
		if _, err := fmt.Fprintf(w, "%-*s %-*s (%s)\n", maxLabel, label, maxValue, row.value, formatSource(row.source, row.envVar)); err != nil {
			return err
		}
	}
	return nil
}

func RenderJSON(w io.Writer, repoDefaults *RepoDefaults) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(repoDefaults)
}

func resolveStringValue(flagValue string, envMeta envVarMeta, configVal *string, defaultVal string) Resolved[string] {
	if v := strings.TrimSpace(flagValue); v != "" {
		return Resolved[string]{Value: v, Source: SourceFlag}
	}
	if v, ok, _ := resolveEnvString(envMeta, false); ok {
		return Resolved[string]{Value: v, Source: SourceEnv, EnvVar: envMeta.Name}
	}
	if configVal != nil {
		return Resolved[string]{Value: *configVal, Source: SourceConfig}
	}
	return Resolved[string]{Value: defaultVal, Source: SourceDefault}
}

func resolveBoolValue(envMeta envVarMeta, configVal *bool, defaultVal bool) (Resolved[bool], error) {
	raw, ok := os.LookupEnv(envMeta.Name)
	if ok && strings.TrimSpace(raw) != "" {
		parsed, ok := parseEnvBool(raw)
		if !ok {
			return Resolved[bool]{}, fmt.Errorf("parse %s %q: must be true or false", envMeta.Name, raw)
		}
		return Resolved[bool]{Value: parsed, Source: SourceEnv, EnvVar: envMeta.Name}, nil
	}
	if configVal != nil {
		return Resolved[bool]{Value: *configVal, Source: SourceConfig}, nil
	}
	return Resolved[bool]{Value: defaultVal, Source: SourceDefault}, nil
}

func parseEnvBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func resolveDurationValue(envMeta envVarMeta, configVal *string, name string, defaultVal time.Duration) (Resolved[time.Duration], error) {
	if v := os.Getenv(envMeta.Name); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Resolved[time.Duration]{}, ui.WithHint(fmt.Errorf("parse %s %q: %w", envMeta.Name, v, err), durationHint(SourceEnv, envMeta.Name, name, defaultVal))
		}
		if d <= 0 {
			return Resolved[time.Duration]{}, ui.WithHint(fmt.Errorf("%s must be positive, got %v", envMeta.Name, d), durationHint(SourceEnv, envMeta.Name, name, defaultVal))
		}
		return Resolved[time.Duration]{Value: d, Source: SourceEnv, EnvVar: envMeta.Name}, nil
	}
	if configVal != nil {
		d, err := time.ParseDuration(*configVal)
		if err != nil {
			return Resolved[time.Duration]{}, ui.WithHint(fmt.Errorf("invalid %s %q in .mato.yaml: %w", name, *configVal, err), durationHint(SourceConfig, envMeta.Name, name, defaultVal))
		}
		if d <= 0 {
			return Resolved[time.Duration]{}, ui.WithHint(fmt.Errorf("%s in .mato.yaml must be positive, got %v", name, d), durationHint(SourceConfig, envMeta.Name, name, defaultVal))
		}
		return Resolved[time.Duration]{Value: d, Source: SourceConfig}, nil
	}
	return Resolved[time.Duration]{Value: defaultVal, Source: SourceDefault}, nil
}

func resolveEnvString(meta envVarMeta, rejectWhitespace bool) (string, bool, error) {
	raw, ok := os.LookupEnv(meta.Name)
	if !ok || raw == "" {
		return "", false, nil
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if rejectWhitespace {
			return "", false, fmt.Errorf("%s must not be whitespace-only", meta.Name)
		}
		return "", false, nil
	}
	return trimmed, true, nil
}

func formatDurationResolved(resolved Resolved[time.Duration]) Resolved[string] {
	return Resolved[string]{
		Value:  resolved.Value.String(),
		Source: resolved.Source,
		EnvVar: resolved.EnvVar,
	}
}

func formatSource(source Source, envVar string) string {
	if source == SourceEnv {
		return fmt.Sprintf("env: %s", envVar)
	}
	return string(source)
}

func validateResolvedReasoningEffort(resolved Resolved[string], flagName, settingName string) error {
	if validReasoningEfforts[resolved.Value] {
		return nil
	}
	message := fmt.Sprintf("invalid %s %q: must be one of low, medium, high, xhigh", flagName, resolved.Value)
	switch resolved.Source {
	case SourceEnv:
		message = fmt.Sprintf("invalid %s %q: must be one of low, medium, high, xhigh", resolved.EnvVar, resolved.Value)
	case SourceConfig:
		message = fmt.Sprintf("invalid %s %q in .mato.yaml: must be one of low, medium, high, xhigh", settingName, resolved.Value)
	case SourceDefault:
		message = fmt.Sprintf("invalid %s %q: must be one of low, medium, high, xhigh", settingName, resolved.Value)
	}
	return ui.WithHint(fmt.Errorf("%s", message), reasoningEffortHint(resolved, flagName, settingName))
}

func reasoningEffortHint(resolved Resolved[string], flagName, settingName string) string {
	switch resolved.Source {
	case SourceFlag:
		return fmt.Sprintf("set --%s to one of low, medium, high, or xhigh", flagName)
	case SourceEnv:
		return fmt.Sprintf("set %s to one of low, medium, high, or xhigh, or unset it to use the default", resolved.EnvVar)
	case SourceConfig:
		return fmt.Sprintf("set %s in .mato.yaml to one of low, medium, high, or xhigh", settingName)
	default:
		return fmt.Sprintf("set %s to one of low, medium, high, or xhigh", settingName)
	}
}

func durationHint(source Source, envVar, settingName string, defaultVal time.Duration) string {
	example := defaultVal.String()
	switch source {
	case SourceEnv:
		return fmt.Sprintf("set %s to a positive duration like %s, or unset it to fall back to .mato.yaml or the default", envVar, example)
	case SourceConfig:
		return fmt.Sprintf("set %s in .mato.yaml to a positive duration like %s, or remove it to use the default", settingName, example)
	default:
		return fmt.Sprintf("set %s to a positive duration like %s", settingName, example)
	}
}
