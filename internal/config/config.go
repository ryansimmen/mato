// Package config loads repository-local .mato.yaml settings.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const configFileName = ".mato.yaml"

// Config represents the settings from a .mato.yaml file.
// All fields are pointers to distinguish "not set" from "zero value".
type Config struct {
	Branch                *string `yaml:"branch"`
	DockerImage           *string `yaml:"docker_image"`
	TaskModel             *string `yaml:"task_model"`
	ReviewModel           *string `yaml:"review_model"`
	ReviewSessionResume   *bool   `yaml:"review_session_resume_enabled"`
	TaskReasoningEffort   *string `yaml:"task_reasoning_effort"`
	ReviewReasoningEffort *string `yaml:"review_reasoning_effort"`
	AgentTimeout          *string `yaml:"agent_timeout"`
	RetryCooldown         *string `yaml:"retry_cooldown"`
}

// Load reads .mato.yaml from dir. Returns a zero Config when the file does not
// exist.
func Load(dir string) (Config, error) {
	return LoadFile(filepath.Join(dir, configFileName))
}

// LoadFile parses a specific config file path. Returns a zero Config when the
// file does not exist. Returns an error if the file contains unknown keys.
func LoadFile(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if err == io.EOF {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}

	// Reject multi-document YAML: a second Decode must return io.EOF.
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("parse config file %s: contains multiple YAML documents", path)
	}

	normalize(&cfg)
	return cfg, nil
}

func normalize(cfg *Config) {
	cfg.Branch = normalizeString(cfg.Branch)
	cfg.DockerImage = normalizeString(cfg.DockerImage)
	cfg.TaskModel = normalizeString(cfg.TaskModel)
	cfg.ReviewModel = normalizeString(cfg.ReviewModel)
	cfg.TaskReasoningEffort = normalizeString(cfg.TaskReasoningEffort)
	cfg.ReviewReasoningEffort = normalizeString(cfg.ReviewReasoningEffort)
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
