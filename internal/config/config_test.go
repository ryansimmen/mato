package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_AllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	content := strings.Join([]string{
		"branch: main",
		"docker_image: ubuntu:24.04",
		"task_model: claude-sonnet-4",
		"review_model: gpt-5.4",
		"review_session_resume_enabled: false",
		"task_reasoning_effort: high",
		"review_reasoning_effort: medium",
		"agent_timeout: 45m",
		"retry_cooldown: 5m",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Branch == nil || *cfg.Branch != "main" {
		t.Fatalf("Branch = %v, want %q", cfg.Branch, "main")
	}
	if cfg.DockerImage == nil || *cfg.DockerImage != "ubuntu:24.04" {
		t.Fatalf("DockerImage = %v, want %q", cfg.DockerImage, "ubuntu:24.04")
	}
	if cfg.TaskModel == nil || *cfg.TaskModel != "claude-sonnet-4" {
		t.Fatalf("TaskModel = %v, want %q", cfg.TaskModel, "claude-sonnet-4")
	}
	if cfg.ReviewModel == nil || *cfg.ReviewModel != "gpt-5.4" {
		t.Fatalf("ReviewModel = %v, want %q", cfg.ReviewModel, "gpt-5.4")
	}
	if cfg.ReviewSessionResume == nil || *cfg.ReviewSessionResume {
		t.Fatalf("ReviewSessionResume = %v, want false", cfg.ReviewSessionResume)
	}
	if cfg.TaskReasoningEffort == nil || *cfg.TaskReasoningEffort != "high" {
		t.Fatalf("TaskReasoningEffort = %v, want %q", cfg.TaskReasoningEffort, "high")
	}
	if cfg.ReviewReasoningEffort == nil || *cfg.ReviewReasoningEffort != "medium" {
		t.Fatalf("ReviewReasoningEffort = %v, want %q", cfg.ReviewReasoningEffort, "medium")
	}
	if cfg.AgentTimeout == nil || *cfg.AgentTimeout != "45m" {
		t.Fatalf("AgentTimeout = %v, want %q", cfg.AgentTimeout, "45m")
	}
	if cfg.RetryCooldown == nil || *cfg.RetryCooldown != "5m" {
		t.Fatalf("RetryCooldown = %v, want %q", cfg.RetryCooldown, "5m")
	}
}

func TestLoad_PartialFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("branch: develop\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Branch == nil || *cfg.Branch != "develop" {
		t.Fatalf("Branch = %v, want %q", cfg.Branch, "develop")
	}
	if cfg.DockerImage != nil || cfg.TaskModel != nil || cfg.ReviewModel != nil || cfg.ReviewSessionResume != nil || cfg.TaskReasoningEffort != nil || cfg.ReviewReasoningEffort != nil || cfg.AgentTimeout != nil || cfg.RetryCooldown != nil {
		t.Fatalf("unexpected non-nil fields: %#v", cfg)
	}
}

func TestLoad_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("cfg = %#v, want zero Config", cfg)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("cfg = %#v, want zero Config", cfg)
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("branch: [\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoad_EmptyStringValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	content := strings.Join([]string{
		"branch: \"\"",
		"task_model: \"\"",
		"review_model: \"\"",
		"review_session_resume_enabled: true",
		"task_reasoning_effort: \"\"",
		"review_reasoning_effort: \"\"",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Branch != nil || cfg.TaskModel != nil || cfg.ReviewModel != nil || cfg.ReviewSessionResume == nil || !*cfg.ReviewSessionResume || cfg.TaskReasoningEffort != nil || cfg.ReviewReasoningEffort != nil {
		t.Fatalf("cfg = %#v, want normalized nil string fields", cfg)
	}
}

func TestLoad_WhitespaceValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	content := strings.Join([]string{
		"branch: \"   \"",
		"task_model: \" \t \"",
		"review_model: \"   \"",
		"task_reasoning_effort: \" \t \"",
		"review_reasoning_effort: \"   \"",
		"agent_timeout: \" \t \"",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Branch != nil || cfg.TaskModel != nil || cfg.ReviewModel != nil || cfg.TaskReasoningEffort != nil || cfg.ReviewReasoningEffort != nil || cfg.AgentTimeout != nil {
		t.Fatalf("cfg = %#v, want normalized nil string fields", cfg)
	}
}

func TestLoad_UnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("future_setting: true\nbranch: mato\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "future_setting") {
		t.Fatalf("error should mention unknown key name, got: %v", err)
	}
}

func TestLoad_DefaultModelRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("default_model: claude-sonnet-4\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for default_model, got nil")
	}
	if !strings.Contains(err.Error(), "default_model") {
		t.Fatalf("error should mention default_model, got: %v", err)
	}
}

func TestLoadFile_ValidPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.yaml")
	if err := os.WriteFile(path, []byte("docker_image: custom:latest\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.DockerImage == nil || *cfg.DockerImage != "custom:latest" {
		t.Fatalf("DockerImage = %v, want %q", cfg.DockerImage, "custom:latest")
	}
}

func TestLoadFile_InvalidPath(t *testing.T) {
	_, err := LoadFile(t.TempDir())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoadFile_MultiDocumentRejected(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			"two documents",
			"branch: main\n---\nbranch: develop\n",
		},
		{
			"second document empty",
			"branch: main\n---\n",
		},
		{
			"three documents",
			"branch: main\n---\nbranch: develop\n---\nbranch: staging\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "multi.yaml")
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("os.WriteFile: %v", err)
			}
			_, err := LoadFile(path)
			if err == nil {
				t.Fatal("expected error for multi-document YAML, got nil")
			}
			if !strings.Contains(err.Error(), "multiple YAML documents") {
				t.Errorf("error = %q, want mention of multiple YAML documents", err.Error())
			}
		})
	}
}

func TestLoadFile_SingleDocumentWithTrailingNewlines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "single.yaml")
	if err := os.WriteFile(path, []byte("branch: main\n\n\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.Branch == nil || *cfg.Branch != "main" {
		t.Fatalf("Branch = %v, want %q", cfg.Branch, "main")
	}
}
