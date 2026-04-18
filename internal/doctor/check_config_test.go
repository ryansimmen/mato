package doctor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryansimmen/mato/internal/testutil"
)

func TestCheckConfig_NoRepo_GitSelected(t *testing.T) {
	// When the git check is also selected, config should not emit its own
	// no_repo finding (the git check already covers it).
	cc := &checkContext{
		ctx:       context.Background(),
		repoInput: "/tmp/nonexistent",
		repoErr:   errors.New("not a repo"),
		selectedChecks: map[string]bool{
			"git":    true,
			"config": true,
		},
	}

	cr := checkConfig(cc)

	if cr.Name != "config" {
		t.Fatalf("name = %q, want %q", cr.Name, "config")
	}
	// Should produce no findings because the git check is selected.
	if len(cr.Findings) != 0 {
		t.Errorf("len(findings) = %d, want 0; findings: %v", len(cr.Findings), cr.Findings)
	}
}

func TestCheckConfig_NoRepo_ConfigOnly(t *testing.T) {
	// When running only config (git not selected), config emits its own
	// config.no_repo finding.
	cc := &checkContext{
		ctx:       context.Background(),
		repoInput: "/tmp/nonexistent",
		repoErr:   errors.New("git rev-parse: exit status 128 (fatal: not a git repository)"),
		selectedChecks: map[string]bool{
			"config": true,
		},
	}

	cr := checkConfig(cc)

	if len(cr.Findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
	}
	f := cr.Findings[0]
	if f.Code != "config.no_repo" {
		t.Errorf("code = %q, want %q", f.Code, "config.no_repo")
	}
	if f.Severity != SeverityError {
		t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
	}
}

func TestCheckConfig_NoRepo_NilError(t *testing.T) {
	// When repoErr is nil but repoRoot is empty, the message should
	// indicate no valid git repository without an error detail suffix.
	cc := &checkContext{
		ctx:       context.Background(),
		repoInput: "/tmp/nonexistent",
		selectedChecks: map[string]bool{
			"config": true,
		},
	}

	cr := checkConfig(cc)

	if len(cr.Findings) != 1 {
		t.Fatalf("len(findings) = %d, want 1", len(cr.Findings))
	}
	f := cr.Findings[0]
	if f.Code != "config.no_repo" {
		t.Errorf("code = %q, want %q", f.Code, "config.no_repo")
	}
	if !strings.Contains(f.Message, "no valid git repository") {
		t.Errorf("message = %q, want it to contain 'no valid git repository'", f.Message)
	}
}

func TestCheckConfig_ParseError(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a malformed .mato.yaml.
	configPath := filepath.Join(repoRoot, ".mato.yaml")
	testutil.WriteFile(t, configPath, ":\n  bad: [unbalanced\n")

	cc := &checkContext{
		ctx:      context.Background(),
		repoRoot: repoRoot,
		selectedChecks: map[string]bool{
			"config": true,
		},
	}

	cr := checkConfig(cc)

	foundParseError := false
	for _, f := range cr.Findings {
		if f.Code == "config.parse_error" {
			foundParseError = true
			if f.Severity != SeverityError {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
			}
			// The path should point to the malformed .mato.yaml file.
			if f.Path != configPath {
				t.Errorf("path = %q, want %q", f.Path, configPath)
			}
			// The message should contain the YAML parse error detail.
			if f.Message == "" {
				t.Error("expected non-empty message for config.parse_error")
			}
			if !strings.Contains(f.Message, ".mato.yaml") {
				t.Errorf("message = %q, want it to reference .mato.yaml", f.Message)
			}
		}
	}
	if !foundParseError {
		t.Error("expected config.parse_error finding")
	}

	// configValidationFatal should be set.
	if !cc.configValidationFatal {
		t.Error("expected configValidationFatal to be true")
	}
}

func TestCheckConfig_ValidationIssueMapping(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	// Write a config with an invalid branch pattern.
	t.Setenv("MATO_BRANCH", "")
	testutil.WriteFile(t, filepath.Join(repoRoot, ".mato.yaml"),
		"branch: foo..bar\n")

	cc := &checkContext{
		ctx:      context.Background(),
		repoRoot: repoRoot,
		selectedChecks: map[string]bool{
			"config": true,
		},
	}

	cr := checkConfig(cc)

	foundIssue := false
	for _, f := range cr.Findings {
		if f.Code == "config.invalid_branch" {
			foundIssue = true
			if f.Severity != SeverityError {
				t.Errorf("severity = %q, want %q", f.Severity, SeverityError)
			}
		}
	}
	if !foundIssue {
		t.Error("expected config.invalid_branch finding")
	}
}

func TestCheckConfig_HealthyConfig(t *testing.T) {
	repoRoot, _ := testutil.SetupRepoWithTasks(t)
	allOK(t)

	cc := &checkContext{
		ctx:      context.Background(),
		repoRoot: repoRoot,
		selectedChecks: map[string]bool{
			"config": true,
		},
	}

	cr := checkConfig(cc)

	// No config file means no findings (config is optional).
	for _, f := range cr.Findings {
		if f.Severity == SeverityError {
			t.Errorf("unexpected error finding: %v", f)
		}
	}
}

func TestConfigPathFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "standard parse error",
			err:  errors.New("parse config file /tmp/repo/.mato.yaml: yaml: line 2: did not find expected key"),
			want: "/tmp/repo/.mato.yaml",
		},
		{
			name: "no prefix match",
			err:  errors.New("some other error"),
			want: "",
		},
		{
			name: "prefix match but no colon",
			err:  errors.New("parse config file path-only"),
			want: "",
		},
		{
			name: "prefix match with colon",
			err:  errors.New("parse config file /a/b/c.yaml: problem"),
			want: "/a/b/c.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configPathFromError(tt.err)
			if got != tt.want {
				t.Errorf("configPathFromError(%q) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
