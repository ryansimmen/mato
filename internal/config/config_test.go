package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_AllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("branch: main\ndocker_image: ubuntu:24.04\ndefault_model: claude-sonnet-4\nagent_timeout: 45m\nretry_cooldown: 5m\n"), 0o644); err != nil {
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
	if cfg.DefaultModel == nil || *cfg.DefaultModel != "claude-sonnet-4" {
		t.Fatalf("DefaultModel = %v, want %q", cfg.DefaultModel, "claude-sonnet-4")
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
	if cfg.DockerImage != nil {
		t.Fatalf("DockerImage = %v, want nil", cfg.DockerImage)
	}
	if cfg.DefaultModel != nil {
		t.Fatalf("DefaultModel = %v, want nil", cfg.DefaultModel)
	}
	if cfg.AgentTimeout != nil {
		t.Fatalf("AgentTimeout = %v, want nil", cfg.AgentTimeout)
	}
	if cfg.RetryCooldown != nil {
		t.Fatalf("RetryCooldown = %v, want nil", cfg.RetryCooldown)
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
	if err := os.WriteFile(path, []byte("branch: \"\"\ndefault_model: \"\"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Branch != nil || cfg.DefaultModel != nil {
		t.Fatalf("cfg = %#v, want normalized nil string fields", cfg)
	}
}

func TestLoad_WhitespaceValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("branch: \"   \"\nagent_timeout: \" \t \"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Branch != nil || cfg.AgentTimeout != nil {
		t.Fatalf("cfg = %#v, want normalized nil string fields", cfg)
	}
}

func TestLoad_UnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configFileName)
	if err := os.WriteFile(path, []byte("future_setting: true\nbranch: mato\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Branch == nil || *cfg.Branch != "mato" {
		t.Fatalf("Branch = %v, want %q", cfg.Branch, "mato")
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
