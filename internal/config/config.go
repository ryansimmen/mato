// Package config loads repository-local .mato.yaml settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const configFileName = ".mato.yaml"

// Config represents the settings from a .mato.yaml file.
// All fields are pointers to distinguish "not set" from "zero value".
type Config struct {
	Branch        *string `yaml:"branch"`
	DockerImage   *string `yaml:"docker_image"`
	DefaultModel  *string `yaml:"default_model"`
	AgentTimeout  *string `yaml:"agent_timeout"`
	RetryCooldown *string `yaml:"retry_cooldown"`
}

// Load reads .mato.yaml from dir. Returns a zero Config when the file does not
// exist.
func Load(dir string) (Config, error) {
	return LoadFile(filepath.Join(dir, configFileName))
}

// LoadFile parses a specific config file path. Returns a zero Config when the
// file does not exist.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}

	normalize(&cfg)
	return cfg, nil
}

func normalize(cfg *Config) {
	cfg.Branch = normalizeString(cfg.Branch)
	cfg.DockerImage = normalizeString(cfg.DockerImage)
	cfg.DefaultModel = normalizeString(cfg.DefaultModel)
	cfg.AgentTimeout = normalizeString(cfg.AgentTimeout)
	cfg.RetryCooldown = normalizeString(cfg.RetryCooldown)
}

func normalizeString(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
