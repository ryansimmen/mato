package configresolve

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ryansimmen/mato/internal/config"
	"github.com/ryansimmen/mato/internal/git"
)

// ValidationIssue describes an invalid effective repository-default setting.
type ValidationIssue struct {
	Code       string
	Setting    string
	Message    string
	Source     Source
	EnvVar     string
	ConfigPath string
}

// RepoValidationResult contains resolved defaults plus any validation issues.
type RepoValidationResult struct {
	Resolved RepoDefaults
	Issues   []ValidationIssue
}

// ValidateRepoDefaults resolves repo defaults and returns all invalid effective
// settings it can discover in one pass. Config discovery and parse failures are
// returned as fatal errors because effective settings cannot be determined.
func ValidateRepoDefaults(repoRoot string) (*RepoValidationResult, error) {
	load, err := config.Load(repoRoot)
	if err != nil {
		return nil, err
	}

	result := &RepoValidationResult{
		Resolved: RepoDefaults{
			RepoRoot:              repoRoot,
			ConfigPath:            load.Path,
			ConfigExists:          load.Exists,
			DockerImage:           resolveStringValue("", envDockerImage, load.Config.DockerImage, config.DefaultDockerImage),
			TaskModel:             resolveStringValue("", envTaskModel, load.Config.TaskModel, config.DefaultTaskModel),
			ReviewModel:           resolveStringValue("", envReviewModel, load.Config.ReviewModel, config.DefaultReviewModel),
			TaskReasoningEffort:   resolveStringValue("", envTaskReasoningEffort, load.Config.TaskReasoningEffort, config.DefaultReasoningEffort),
			ReviewReasoningEffort: resolveStringValue("", envReviewReasoningEffort, load.Config.ReviewReasoningEffort, config.DefaultReasoningEffort),
			AgentTimeout:          resolveDurationValueForValidation(envAgentTimeout, load.Config.AgentTimeout, config.DefaultAgentTimeout),
			RetryCooldown:         resolveDurationValueForValidation(envRetryCooldown, load.Config.RetryCooldown, config.DefaultRetryCooldown),
		},
	}

	branch, branchIssue := resolveBranchForValidation(load)
	if branch.Value != "" || branch.Source != "" || branch.EnvVar != "" {
		result.Resolved.Branch = branch
	}
	if branchIssue != nil {
		result.Issues = append(result.Issues, *branchIssue)
	}

	reviewResume, boolIssue := resolveBoolValueForValidation(envReviewSessionResumeEnabled, load.Config.ReviewSessionResume, true, "review_session_resume_enabled", load.Path)
	result.Resolved.ReviewSessionResumeEnabled = reviewResume
	if boolIssue != nil {
		result.Issues = append(result.Issues, *boolIssue)
	}

	if issue := validateReasoningEffortForValidation(result.Resolved.TaskReasoningEffort, "task_reasoning_effort", load.Path); issue != nil {
		result.Issues = append(result.Issues, *issue)
	}
	if issue := validateReasoningEffortForValidation(result.Resolved.ReviewReasoningEffort, "review_reasoning_effort", load.Path); issue != nil {
		result.Issues = append(result.Issues, *issue)
	}
	if issue := validateDurationResolved(result.Resolved.AgentTimeout, "agent_timeout", load.Path); issue != nil {
		result.Issues = append(result.Issues, *issue)
	}
	if issue := validateDurationResolved(result.Resolved.RetryCooldown, "retry_cooldown", load.Path); issue != nil {
		result.Issues = append(result.Issues, *issue)
	}

	return result, nil
}

func resolveBranchForValidation(load config.LoadResult) (Resolved[string], *ValidationIssue) {
	raw, ok := os.LookupEnv(envBranch.Name)
	if ok && raw != "" {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return Resolved[string]{Source: SourceEnv, EnvVar: envBranch.Name}, &ValidationIssue{
				Code:    "config.invalid_branch",
				Setting: "branch",
				Message: fmt.Sprintf("invalid %s %q: must not be whitespace-only", envBranch.Name, raw),
				Source:  SourceEnv,
				EnvVar:  envBranch.Name,
			}
		}
		resolved := Resolved[string]{Value: trimmed, Source: SourceEnv, EnvVar: envBranch.Name}
		if err := git.ValidateBranch(trimmed); err != nil {
			return resolved, &ValidationIssue{
				Code:    "config.invalid_branch",
				Setting: "branch",
				Message: fmt.Sprintf("invalid %s %q: %s", envBranch.Name, trimmed, branchValidationDetail(trimmed, err)),
				Source:  SourceEnv,
				EnvVar:  envBranch.Name,
			}
		}
		return resolved, nil
	}

	if load.Config.Branch != nil {
		resolved := Resolved[string]{Value: *load.Config.Branch, Source: SourceConfig}
		if err := git.ValidateBranch(resolved.Value); err != nil {
			return resolved, &ValidationIssue{
				Code:       "config.invalid_branch",
				Setting:    "branch",
				Message:    fmt.Sprintf("invalid branch %q: %s", resolved.Value, branchValidationDetail(resolved.Value, err)),
				Source:     SourceConfig,
				ConfigPath: load.Path,
			}
		}
		return resolved, nil
	}

	resolved := Resolved[string]{Value: config.DefaultBranch, Source: SourceDefault}
	if err := git.ValidateBranch(resolved.Value); err != nil {
		return resolved, &ValidationIssue{
			Code:    "config.invalid_branch",
			Setting: "branch",
			Message: fmt.Sprintf("invalid branch %q: %s", resolved.Value, branchValidationDetail(resolved.Value, err)),
			Source:  SourceDefault,
		}
	}
	return resolved, nil
}

func resolveBoolValueForValidation(envMeta envVarMeta, configVal *bool, defaultVal bool, settingName, configPath string) (Resolved[bool], *ValidationIssue) {
	raw, ok := os.LookupEnv(envMeta.Name)
	if ok && strings.TrimSpace(raw) != "" {
		parsed, ok := parseEnvBool(raw)
		if !ok {
			return Resolved[bool]{Source: SourceEnv, EnvVar: envMeta.Name}, &ValidationIssue{
				Code:    "config.invalid_bool",
				Setting: settingName,
				Message: fmt.Sprintf("invalid %s %q: must be true or false", envMeta.Name, raw),
				Source:  SourceEnv,
				EnvVar:  envMeta.Name,
			}
		}
		return Resolved[bool]{Value: parsed, Source: SourceEnv, EnvVar: envMeta.Name}, nil
	}
	if configVal != nil {
		return Resolved[bool]{Value: *configVal, Source: SourceConfig}, nil
	}
	return Resolved[bool]{Value: defaultVal, Source: SourceDefault}, nil
}

func resolveDurationValueForValidation(envMeta envVarMeta, configVal *string, defaultVal time.Duration) Resolved[string] {
	if v := os.Getenv(envMeta.Name); v != "" {
		return Resolved[string]{Value: v, Source: SourceEnv, EnvVar: envMeta.Name}
	}
	if configVal != nil {
		return Resolved[string]{Value: *configVal, Source: SourceConfig}
	}
	return Resolved[string]{Value: defaultVal.String(), Source: SourceDefault}
}

func validateReasoningEffortForValidation(resolved Resolved[string], settingName, configPath string) *ValidationIssue {
	if validReasoningEfforts[resolved.Value] {
		return nil
	}
	message := fmt.Sprintf("invalid %s %q: must be one of low, medium, high, xhigh", settingName, resolved.Value)
	issue := ValidationIssue{
		Code:    "config.invalid_reasoning_effort",
		Setting: settingName,
		Message: message,
		Source:  resolved.Source,
		EnvVar:  resolved.EnvVar,
	}
	if resolved.Source == SourceEnv {
		issue.Message = fmt.Sprintf("invalid %s %q: must be one of low, medium, high, xhigh", resolved.EnvVar, resolved.Value)
	}
	if resolved.Source == SourceConfig {
		issue.ConfigPath = configPath
	}
	return &issue
}

func validateDurationResolved(resolved Resolved[string], settingName, configPath string) *ValidationIssue {
	if resolved.Value == "" {
		return nil
	}
	d, err := time.ParseDuration(resolved.Value)
	if err == nil && d > 0 {
		return nil
	}
	message := ""
	if err != nil {
		if resolved.Source == SourceEnv {
			message = fmt.Sprintf("invalid %s %q: %v", resolved.EnvVar, resolved.Value, err)
		} else {
			message = fmt.Sprintf("invalid %s %q: %v", settingName, resolved.Value, err)
		}
	} else if resolved.Source == SourceEnv {
		message = fmt.Sprintf("invalid %s %q: must be positive, got %v", resolved.EnvVar, resolved.Value, d)
	} else {
		message = fmt.Sprintf("invalid %s %q: must be positive, got %v", settingName, resolved.Value, d)
	}
	issue := ValidationIssue{
		Code:    "config.invalid_duration",
		Setting: settingName,
		Message: message,
		Source:  resolved.Source,
		EnvVar:  resolved.EnvVar,
	}
	if resolved.Source == SourceConfig {
		issue.ConfigPath = configPath
	}
	return &issue
}

func branchValidationDetail(branch string, err error) string {
	prefix := fmt.Sprintf("invalid branch name %q: ", branch)
	message := err.Error()
	if strings.HasPrefix(message, prefix) {
		return strings.TrimPrefix(message, prefix)
	}
	return message
}
