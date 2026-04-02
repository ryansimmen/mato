package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNewColorSet_AllFieldsNonNil(t *testing.T) {
	c := NewColorSet()
	if c.Bold == nil {
		t.Error("Bold is nil")
	}
	if c.Green == nil {
		t.Error("Green is nil")
	}
	if c.Red == nil {
		t.Error("Red is nil")
	}
	if c.Yellow == nil {
		t.Error("Yellow is nil")
	}
	if c.Cyan == nil {
		t.Error("Cyan is nil")
	}
	if c.Dim == nil {
		t.Error("Dim is nil")
	}
}

func TestNewColorSet_PlainOutput(t *testing.T) {
	// fatih/color disables ANSI when stdout is not a TTY (as in tests),
	// so we can verify that the functions return the plain text.
	c := NewColorSet()
	if got := c.Green("ok"); got != "ok" {
		t.Errorf("Green(ok) = %q, want %q", got, "ok")
	}
	if got := c.Red("err"); got != "err" {
		t.Errorf("Red(err) = %q, want %q", got, "err")
	}
}

func TestWarnf_ExplicitWriterOverridesStderr(t *testing.T) {
	var buf bytes.Buffer
	prev := SetWarningWriter(&buf)
	defer SetWarningWriter(prev)

	Warnf("warning: %s\n", "something broke")

	got := buf.String()
	want := "warning: something broke\n"
	if got != want {
		t.Errorf("Warnf output = %q, want %q", got, want)
	}
}

func TestWarnf_NilWriterUsesStderr(t *testing.T) {
	// When warningWriter is nil, Warnf falls back to os.Stderr.
	// We redirect os.Stderr to verify output arrives there.
	prev := SetWarningWriter(nil)
	defer SetWarningWriter(prev)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	Warnf("warning: fallback\n")
	w.Close()

	var buf bytes.Buffer
	buf.ReadFrom(r)
	r.Close()
	if got := buf.String(); got != "warning: fallback\n" {
		t.Errorf("Warnf via os.Stderr = %q, want %q", got, "warning: fallback\n")
	}
}

func TestSetWarningWriter_ReturnsPrevious(t *testing.T) {
	var buf bytes.Buffer
	prev := SetWarningWriter(&buf)
	if prev != nil {
		t.Errorf("initial warningWriter should be nil (default to os.Stderr at call time)")
	}
	restored := SetWarningWriter(prev)
	if restored != &buf {
		t.Errorf("SetWarningWriter should return previous writer")
	}
}

func TestValidateFormat_Allowed(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		allowed []string
	}{
		{"text in text/json", "text", []string{"text", "json"}},
		{"json in text/json", "json", []string{"text", "json"}},
		{"dot in text/dot/json", "dot", []string{"text", "dot", "json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateFormat(tt.format, tt.allowed); err != nil {
				t.Errorf("ValidateFormat(%q, %v) = %v, want nil", tt.format, tt.allowed, err)
			}
		})
	}
}

func TestValidateFormat_Rejected(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		allowed []string
	}{
		{"yaml not in text/json", "yaml", []string{"text", "json"}},
		{"dot not in text/json", "dot", []string{"text", "json"}},
		{"empty not in text/json", "", []string{"text", "json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateFormat(tt.format, tt.allowed); err == nil {
				t.Errorf("ValidateFormat(%q, %v) = nil, want error", tt.format, tt.allowed)
			}
		})
	}
}

func TestRequireTasksDir_ExistingDir(t *testing.T) {
	dir := t.TempDir()
	if err := RequireTasksDir(dir); err != nil {
		t.Errorf("RequireTasksDir(%q) = %v, want nil", dir, err)
	}
}

func TestRequireTasksDir_NotExist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	err := RequireTasksDir(dir)
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
	want := ".mato/ directory not found"
	if got := err.Error(); !contains(got, want) {
		t.Errorf("error = %q, want to contain %q", got, want)
	}
}

func TestRequireTasksDir_NotADir(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RequireTasksDir(file)
	if err == nil {
		t.Fatal("expected error for non-directory")
	}
	want := "exists but is not a directory"
	if got := err.Error(); !contains(got, want) {
		t.Errorf("error = %q, want to contain %q", got, want)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsAt(s, substr)
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
