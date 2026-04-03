// Package config loads repository-local .mato.yaml (or .mato.yml) settings.
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

var configFileNames = []string{".mato.yaml", ".mato.yml"}

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

// Load reads .mato.yaml or .mato.yml from dir. It checks for both filenames
// and returns an error if both exist. Returns a zero Config when neither file
// exists.
func Load(dir string) (Config, error) {
	var found []string
	for _, name := range configFileNames {
		path := filepath.Join(dir, name)
		_, err := os.Stat(path)
		if err == nil {
			found = append(found, path)
			continue
		}
		if !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("stat config file %s: %w", path, err)
		}
	}
	if len(found) > 1 {
		return Config{}, fmt.Errorf("found both .mato.yaml and .mato.yml; remove one to avoid ambiguity")
	}
	if len(found) == 0 {
		return Config{}, nil
	}
	return LoadFile(found[0])
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
