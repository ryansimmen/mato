// Package frontmatter parses YAML frontmatter and task metadata from markdown files.
package frontmatter

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"gopkg.in/yaml.v3"
)

var (
	branchUnsafeRe = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	branchMultiRe  = regexp.MustCompile(`-{2,}`)
)

// StrippedAffect records an affects entry that was removed during
// sanitization, along with the reason it was considered unsafe.
type StrippedAffect struct {
	Entry  string
	Reason string
}

type TaskMeta struct {
	ID         string   `yaml:"id"`
	Priority   int      `yaml:"priority"`
	DependsOn  []string `yaml:"depends_on"`
	Affects    []string `yaml:"affects"`
	MaxRetries int      `yaml:"max_retries"`
	// StrippedAffects records affects entries that were removed during
	// sanitization (e.g. absolute paths, path traversal). Not serialized
	// to YAML; populated at parse time for diagnostic reporting.
	StrippedAffects []StrippedAffect `yaml:"-"`
}

// ParseTaskFile reads a task file from disk and parses its YAML frontmatter
// and body. It is a convenience wrapper around ParseTaskData.
func ParseTaskFile(path string) (TaskMeta, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskMeta{}, "", fmt.Errorf("read task file %s: %w", path, err)
	}
	return ParseTaskData(data, path)
}

// ParseTaskData parses YAML frontmatter and body from raw file bytes. The path
// argument is used only for the default task ID (filename stem) and error
// messages. This allows callers that already hold the file contents (e.g.
// PollIndex) to avoid a redundant os.ReadFile.
func ParseTaskData(data []byte, path string) (TaskMeta, string, error) {
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	meta := TaskMeta{
		ID:         TaskFileStem(path),
		Priority:   50,
		MaxRetries: 3,
	}
	body := content

	lines := strings.Split(content, "\n")

	// Skip leading scheduler-managed HTML comment lines (e.g. <!-- claimed-by: ... -->)
	// and blank lines before checking for frontmatter, since claim.go prepends
	// these before the --- delimiter. User-authored HTML comments are NOT skipped
	// so they are preserved in the returned body.
	startLine := 0
	for startLine < len(lines) {
		trimmed := strings.TrimSpace(lines[startLine])
		if trimmed == "" || isManagedComment(trimmed) {
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
		blockKeys := map[string]struct{}{}
		if strings.TrimSpace(block) != "" {
			if err := yaml.Unmarshal([]byte(block), &meta); err != nil {
				return TaskMeta{}, "", fmt.Errorf("parse frontmatter in %s: %w", path, err)
			}
			blockKeys = topLevelKeys(block)
		}
		// Restore defaults for zero-value fields that weren't set
		if meta.ID == "" {
			meta.ID = TaskFileStem(path)
		}
		if meta.Priority == 0 {
			if _, ok := blockKeys["priority"]; !ok {
				meta.Priority = 50
			}
		}
		if meta.MaxRetries == 0 {
			if _, ok := blockKeys["max_retries"]; !ok {
				meta.MaxRetries = 3
			}
		}
		if meta.MaxRetries < 0 {
			return TaskMeta{}, "", fmt.Errorf("invalid max_retries in %s: value %d is negative", path, meta.MaxRetries)
		}
		// Filter empty strings from arrays (YAML can produce them from ["", x])
		meta.DependsOn = filterEmpty(meta.DependsOn)
		meta.Affects, meta.StrippedAffects = sanitizeAffects(filterEmpty(meta.Affects))
		body = strings.Join(lines[end+1:], "\n")
	}

	return meta, stripHTMLCommentLines(body), nil
}

func topLevelKeys(block string) map[string]struct{} {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		return nil
	}
	if len(doc.Content) == 0 {
		return nil
	}
	mapping := doc.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	keys := make(map[string]struct{}, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode := mapping.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		keys[keyNode.Value] = struct{}{}
	}
	return keys
}

func TaskFileStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// managedCommentPrefixes lists the HTML comment markers that the queue
// scheduler writes at runtime. Only these are stripped from the task body;
// all other HTML comments are preserved so task authors can use them freely.
var managedCommentPrefixes = []string{
	"<!-- claimed-by:",
	"<!-- branch:",
	"<!-- failure:",
	"<!-- review-failure:",
	"<!-- review-rejection:",
	"<!-- reviewed:",
	"<!-- cancelled:",
	"<!-- cycle-failure:",
	"<!-- terminal-failure:",
	"<!-- merged:",
}

// isManagedComment reports whether trimmed (a whitespace-trimmed line) is a
// full-line HTML comment that matches one of the queue-managed marker prefixes.
func isManagedComment(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "<!--") || !strings.HasSuffix(trimmed, "-->") {
		return false
	}
	for _, prefix := range managedCommentPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func stripHTMLCommentLines(body string) string {
	lines := strings.Split(body, "\n")
	filtered := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inFence && !isFenceLine(trimmed) && isManagedComment(trimmed) {
			continue
		}
		filtered = append(filtered, line)
		if isFenceLine(trimmed) {
			inFence = !inFence
		}
	}
	return strings.Join(filtered, "\n")
}

// isFenceLine uses the same toggle logic as taskfile.isFenceLine so fence
// handling stays consistent across packages.
func isFenceLine(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	if strings.HasPrefix(trimmed, "```") {
		return true
	}
	if strings.HasPrefix(trimmed, "~~~") {
		return true
	}
	return false
}

// ExtractTitle returns the first non-empty, non-HTML-comment line from the
// body, stripping leading markdown heading markers (#). Leading full-line HTML
// comments (<!-- ... -->) are skipped so that user-authored comments don't
// become the displayed title. Falls back to TaskFileStem(filename).
func ExtractTitle(filename, body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->") {
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

// IsGlob reports whether s contains glob metacharacters.
// Checks for *, ?, [, and { because doublestar supports brace expansion.
func IsGlob(s string) bool {
	return strings.ContainsAny(s, "*?[{")
}

// ValidateAffectsGlobs checks glob entries in an affects list for semantic
// errors: trailing "/" combined with glob syntax (ambiguous intent) and
// invalid glob syntax that doublestar cannot compile. Returns nil if all
// entries are valid or non-glob.
func ValidateAffectsGlobs(affects []string) error {
	for _, af := range affects {
		if !IsGlob(af) {
			continue
		}
		if strings.HasSuffix(af, "/") {
			return fmt.Errorf("affects %q combines glob syntax with trailing /; use a glob pattern without trailing / or a plain directory prefix", af)
		}
		if _, err := doublestar.Match(af, ""); err != nil {
			return fmt.Errorf("invalid glob in affects %q: %w", af, err)
		}
	}
	return nil
}

// sanitizeAffects validates affects entries against path traversal. Entries
// containing ".." path components that escape the repository root or absolute
// paths are stripped and recorded as StrippedAffect values so callers can
// report the problem explicitly. Non-glob, non-directory-prefix entries are
// cleaned via filepath.Clean so redundant components like
// "internal/../internal/foo.go" resolve to "internal/foo.go".
func sanitizeAffects(affects []string) ([]string, []StrippedAffect) {
	if affects == nil {
		return nil, nil
	}
	out := make([]string, 0, len(affects))
	var stripped []StrippedAffect
	for _, af := range affects {
		cleaned := filepath.Clean(af)
		if filepath.IsAbs(cleaned) {
			stripped = append(stripped, StrippedAffect{Entry: af, Reason: "absolute path"})
			continue
		}
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			stripped = append(stripped, StrippedAffect{Entry: af, Reason: "path traversal"})
			continue
		}
		// Preserve original value for globs and directory prefixes so their
		// semantic meaning is retained. Clean plain paths only.
		if !IsGlob(af) && !strings.HasSuffix(af, "/") {
			out = append(out, cleaned)
		} else {
			out = append(out, af)
		}
	}
	if len(out) == 0 {
		out = nil
	}
	return out, stripped
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

// BranchDisambiguator returns a short suffix derived from the SHA-256 hash of
// the filename. The suffix is 6 hex characters, providing enough uniqueness to
// avoid branch name collisions when multiple tasks sanitize to the same branch.
func BranchDisambiguator(filename string) string {
	h := sha256.Sum256([]byte(filename))
	return fmt.Sprintf("%x", h[:3])
}
