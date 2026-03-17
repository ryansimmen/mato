package frontmatter

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	branchUnsafeRe = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	branchMultiRe  = regexp.MustCompile(`-{2,}`)
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

	// Skip leading HTML comment lines (e.g. <!-- claimed-by: ... -->) before
	// checking for frontmatter, since claim.go prepends these before the --- delimiter.
	startLine := 0
	for startLine < len(lines) {
		trimmed := strings.TrimSpace(lines[startLine])
		if trimmed == "" || (strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->")) {
			startLine++
			continue
		}
		break
	}

	if startLine < len(lines) && strings.TrimSpace(lines[startLine]) == "---" {
		end := -1
		for i := startLine + 1; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == "---" {
				end = i
				break
			}
		}
		if end == -1 {
			return TaskMeta{}, "", fmt.Errorf("unterminated frontmatter in %s", path)
		}
		block := strings.Join(lines[startLine+1:end], "\n")
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

// ExtractTitle returns the first non-empty line from the body, stripping
// leading markdown heading markers (#). Falls back to TaskFileStem(filename).
func ExtractTitle(filename, body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
		if trimmed != "" {
			return trimmed
		}
	}
	return TaskFileStem(filename)
}

// SanitizeBranchName converts a task filename into a safe git branch name by
// stripping the .md extension, replacing non-alphanumeric characters with
// dashes, collapsing consecutive dashes, and trimming edge dashes.
func SanitizeBranchName(name string) string {
	name = strings.TrimSuffix(name, ".md")
	name = branchUnsafeRe.ReplaceAllString(name, "-")
	name = branchMultiRe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "unnamed"
	}
	return name
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
