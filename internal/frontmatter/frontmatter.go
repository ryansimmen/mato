package frontmatter

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type TaskMeta struct {
	ID                  string   `yaml:"id"`
	Priority            int      `yaml:"priority"`
	DependsOn           []string `yaml:"depends_on"`
	Affects             []string `yaml:"affects"`
	Tags                []string `yaml:"tags"`
	EstimatedComplexity string   `yaml:"estimated_complexity"`
	MaxRetries          int      `yaml:"max_retries"`
}

func ParseTaskFile(path string) (TaskMeta, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskMeta{}, "", err
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	meta := TaskMeta{
		ID:         TaskFileStem(path),
		Priority:   50,
		MaxRetries: 3,
	}
	body := content

	lines := strings.Split(content, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		end := -1
		for i := 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end == -1 {
			return TaskMeta{}, "", fmt.Errorf("unterminated frontmatter in %s", path)
		}
		if err := parseFrontmatterBlock(strings.Join(lines[1:end], "\n"), &meta); err != nil {
			return TaskMeta{}, "", fmt.Errorf("parse frontmatter in %s: %w", path, err)
		}
		body = strings.Join(lines[end+1:], "\n")
	}

	return meta, StripHTMLCommentLines(body), nil
}

func TaskFileStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func parseFrontmatterBlock(block string, meta *TaskMeta) error {
	var currentArray string

	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "- ") {
			if currentArray == "" {
				return fmt.Errorf("unexpected list item %q", trimmed)
			}
			appendArrayValue(meta, currentArray, trimYAMLString(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
			continue
		}

		currentArray = ""
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid frontmatter line %q", trimmed)
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		switch key {
		case "id":
			if value != "" {
				meta.ID = trimYAMLString(value)
			}
		case "priority":
			if value == "" {
				continue
			}
			priority, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("priority: %w", err)
			}
			meta.Priority = priority
		case "depends_on", "affects", "tags":
			values, isBlock, err := parseArrayValue(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			setArrayValue(meta, key, values)
			if isBlock {
				currentArray = key
			}
		case "estimated_complexity":
			meta.EstimatedComplexity = trimYAMLString(value)
		case "max_retries":
			if value == "" {
				continue
			}
			retries, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("max_retries: %w", err)
			}
			meta.MaxRetries = retries
		}
	}

	return nil
}

func parseArrayValue(value string) ([]string, bool, error) {
	if value == "" {
		return nil, true, nil
	}
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return []string{trimYAMLString(value)}, false, nil
	}

	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return nil, false, nil
	}

	parts := strings.Split(inner, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		item := trimYAMLString(strings.TrimSpace(part))
		if item != "" {
			values = append(values, item)
		}
	}
	return values, false, nil
}

func setArrayValue(meta *TaskMeta, key string, values []string) {
	switch key {
	case "depends_on":
		meta.DependsOn = values
	case "affects":
		meta.Affects = values
	case "tags":
		meta.Tags = values
	}
}

func appendArrayValue(meta *TaskMeta, key, value string) {
	if value == "" {
		return
	}
	if key == "depends_on" {
		meta.DependsOn = append(meta.DependsOn, value)
		return
	}
	if key == "affects" {
		meta.Affects = append(meta.Affects, value)
		return
	}
	if key == "tags" {
		meta.Tags = append(meta.Tags, value)
	}
}

func trimYAMLString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if value[0] == '\'' && value[len(value)-1] == '\'' {
			return value[1 : len(value)-1]
		}
		if value[0] == '"' && value[len(value)-1] == '"' {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func StripHTMLCommentLines(body string) string {
	lines := strings.Split(body, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}
