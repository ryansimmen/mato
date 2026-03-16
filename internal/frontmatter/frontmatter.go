package frontmatter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
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
		block := strings.Join(lines[1:end], "\n")
		if strings.TrimSpace(block) != "" {
			if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
				return TaskMeta{}, "", fmt.Errorf("parse frontmatter in %s: %w", path, err)
			}
		}
		// Restore defaults for zero-value fields that weren't set
		if meta.ID == "" {
			meta.ID = TaskFileStem(path)
		}
		if meta.Priority == 0 && !strings.Contains(block, "priority:") {
			meta.Priority = 50
		}
		if meta.MaxRetries == 0 && !strings.Contains(block, "max_retries:") {
			meta.MaxRetries = 3
		}
		// Filter empty strings from arrays (YAML can produce them from ["", x])
		meta.DependsOn = filterEmpty(meta.DependsOn)
		meta.Affects = filterEmpty(meta.Affects)
		meta.Tags = filterEmpty(meta.Tags)
		body = strings.Join(lines[end+1:], "\n")
	}

	return meta, StripHTMLCommentLines(body), nil
}

func TaskFileStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
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

func filterEmpty(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
