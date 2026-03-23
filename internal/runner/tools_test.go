package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFindGitHelper_InPath(t *testing.T) {
	// "git" itself should always be findable in PATH during tests.
	path, err := findGitHelper("git")
	if err != nil {
		t.Fatalf("findGitHelper(\"git\") returned error: %v", err)
	}
	if path == "" {
		t.Fatal("findGitHelper(\"git\") returned empty path")
	}
}

func TestFindGitHelper_FallbackToExecPath(t *testing.T) {
	// git-upload-pack may not be in PATH but should be in git --exec-path.
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	execPath := strings.TrimSpace(string(out))
	candidate := filepath.Join(execPath, "git-upload-pack")
	if _, statErr := os.Stat(candidate); statErr != nil {
		t.Skipf("git-upload-pack not found at %s, skipping fallback test", candidate)
	}

	path, err := findGitHelper("git-upload-pack")
	if err != nil {
		t.Fatalf("findGitHelper(\"git-upload-pack\") returned error: %v", err)
	}
	if path == "" {
		t.Fatal("findGitHelper(\"git-upload-pack\") returned empty path")
	}
}

func TestFindGitHelper_NotFound(t *testing.T) {
	_, err := findGitHelper("nonexistent-git-tool-xyz")
	if err == nil {
		t.Fatal("findGitHelper should return error for nonexistent tool")
	}
}

func TestDiscoverHostTools_MissingCopilotDir(t *testing.T) {
	// Override HOME to a temp dir without a .copilot directory.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when ~/.copilot is missing")
	}
	if !strings.Contains(err.Error(), ".copilot") {
		t.Fatalf("error should mention .copilot, got: %v", err)
	}
}

func TestDiscoverHostTools_CopilotPathIsFile(t *testing.T) {
	// Override HOME to a temp dir where .copilot is a regular file.
	tmpHome := t.TempDir()
	copilotFile := filepath.Join(tmpHome, ".copilot")
	if err := os.WriteFile(copilotFile, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("create .copilot file: %v", err)
	}
	t.Setenv("HOME", tmpHome)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when ~/.copilot is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error should mention 'not a directory', got: %v", err)
	}
}

func TestDiscoverHostTools_ValidCopilotDir(t *testing.T) {
	// Override HOME to a temp dir with a valid .copilot directory.
	tmpHome := t.TempDir()
	copilotDir := filepath.Join(tmpHome, ".copilot")
	if err := os.Mkdir(copilotDir, 0o755); err != nil {
		t.Fatalf("create .copilot dir: %v", err)
	}
	t.Setenv("HOME", tmpHome)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools should succeed with valid ~/.copilot: %v", err)
	}
	if tools.copilotConfigDir != copilotDir {
		t.Fatalf("copilotConfigDir = %q, want %q", tools.copilotConfigDir, copilotDir)
	}
}

// ---------------------------------------------------------------------------
// Test helpers for controlling function-variable seams
// ---------------------------------------------------------------------------

// fakeFileInfo implements os.FileInfo for tests.
type fakeFileInfo struct {
	name  string
	isDir bool
}

func (f fakeFileInfo) Name() string      { return f.name }
func (f fakeFileInfo) Size() int64       { return 0 }
func (f fakeFileInfo) Mode() os.FileMode { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool       { return f.isDir }
func (f fakeFileInfo) Sys() any          { return nil }

// setTestSeams replaces package-level function variables with test doubles
// and restores originals via t.Cleanup.
func setTestSeams(t *testing.T, lp func(string) (string, error), st func(string) (os.FileInfo, error), home func() (string, error), gep func() (string, error)) {
	t.Helper()
	origLP, origSt, origHome, origGEP := lookPathFn, statFn, userHomeDirFn, gitExecPathFn
	t.Cleanup(func() {
		lookPathFn = origLP
		statFn = origSt
		userHomeDirFn = origHome
		gitExecPathFn = origGEP
	})
	if lp != nil {
		lookPathFn = lp
	}
	if st != nil {
		statFn = st
	}
	if home != nil {
		userHomeDirFn = home
	}
	if gep != nil {
		gitExecPathFn = gep
	}
}

// makeLookPathFn builds a fake exec.LookPath from a name→path map.
func makeLookPathFn(tools map[string]string) func(string) (string, error) {
	return func(name string) (string, error) {
		if p, ok := tools[name]; ok {
			return p, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
}

// makeStatFn builds a fake os.Stat from a path→fakeFileInfo map.
func makeStatFn(entries map[string]fakeFileInfo) func(string) (os.FileInfo, error) {
	return func(path string) (os.FileInfo, error) {
		if info, ok := entries[path]; ok {
			return info, nil
		}
		return nil, os.ErrNotExist
	}
}

// allRequiredTools returns a lookPath map with every required CLI tool.
func allRequiredTools() map[string]string {
	return map[string]string{
		"copilot":          "/usr/local/bin/copilot",
		"git":              "/usr/bin/git",
		"git-upload-pack":  "/usr/bin/git-upload-pack",
		"git-receive-pack": "/usr/bin/git-receive-pack",
		"gh":               "/usr/local/bin/gh",
	}
}

// ---------------------------------------------------------------------------
// findGitHelper — seam-based tests
// ---------------------------------------------------------------------------

func TestFindGitHelper_SeamFallbackToExecPath(t *testing.T) {
	setTestSeams(t,
		makeLookPathFn(map[string]string{}),
		makeStatFn(map[string]fakeFileInfo{
			"/fake/exec/git-upload-pack": {name: "git-upload-pack"},
		}),
		nil,
		func() (string, error) { return "/fake/exec", nil },
	)

	path, err := findGitHelper("git-upload-pack")
	if err != nil {
		t.Fatalf("findGitHelper should find via exec-path fallback: %v", err)
	}
	if path != "/fake/exec/git-upload-pack" {
		t.Errorf("path = %q, want /fake/exec/git-upload-pack", path)
	}
}

func TestFindGitHelper_SeamNotFoundAnywhere(t *testing.T) {
	setTestSeams(t,
		makeLookPathFn(map[string]string{}),
		makeStatFn(map[string]fakeFileInfo{}),
		nil,
		func() (string, error) { return "/fake/exec", nil },
	)

	_, err := findGitHelper("git-upload-pack")
	if err == nil {
		t.Fatal("findGitHelper should fail when tool not in PATH or exec-path")
	}
	if !strings.Contains(err.Error(), "not in PATH") {
		t.Errorf("error should mention 'not in PATH', got: %v", err)
	}
}

func TestFindGitHelper_SeamExecPathFails(t *testing.T) {
	setTestSeams(t,
		makeLookPathFn(map[string]string{}),
		nil,
		nil,
		func() (string, error) { return "", exec.ErrNotFound },
	)

	_, err := findGitHelper("git-upload-pack")
	if err == nil {
		t.Fatal("findGitHelper should fail when exec-path lookup fails")
	}
	if !strings.Contains(err.Error(), "git --exec-path failed") {
		t.Errorf("error should mention exec-path failure, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// discoverHostTools — gh path preference
// ---------------------------------------------------------------------------

func TestDiscoverHostTools_GhPrefersUsrBinWhenFile(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if tools.ghPath != "/usr/bin/gh" {
		t.Errorf("ghPath = %q, want /usr/bin/gh", tools.ghPath)
	}
}

func TestDiscoverHostTools_GhFallsBackWhenUsrBinIsDir(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: true},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if tools.ghPath != "/usr/local/bin/gh" {
		t.Errorf("ghPath = %q, want /usr/local/bin/gh (PATH fallback)", tools.ghPath)
	}
}

func TestDiscoverHostTools_GhFallsBackWhenUsrBinMissing(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if tools.ghPath != "/usr/local/bin/gh" {
		t.Errorf("ghPath = %q, want /usr/local/bin/gh (PATH fallback)", tools.ghPath)
	}
}

// ---------------------------------------------------------------------------
// discoverHostTools — optional directories
// ---------------------------------------------------------------------------

func TestDiscoverHostTools_OptionalDirsMissing(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools should succeed when optional dirs are missing: %v", err)
	}
	if tools.hasGitTemplates {
		t.Error("hasGitTemplates should be false when dir is missing")
	}
	if tools.hasSystemCerts {
		t.Error("hasSystemCerts should be false when dir is missing")
	}
	if tools.hasGhConfig {
		t.Error("hasGhConfig should be false when dir is missing")
	}
}

func TestDiscoverHostTools_OptionalDirsPresent(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
			"/usr/share/git-core/templates":            {name: "templates", isDir: true},
			"/etc/ssl/certs":                           {name: "certs", isDir: true},
			filepath.Join(home, ".config", "gh"):       {name: "gh", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if !tools.hasGitTemplates {
		t.Error("hasGitTemplates should be true")
	}
	if !tools.hasSystemCerts {
		t.Error("hasSystemCerts should be true")
	}
	if !tools.hasGhConfig {
		t.Error("hasGhConfig should be true")
	}
}

// ---------------------------------------------------------------------------
// discoverHostTools — .copilot validation via seams
// ---------------------------------------------------------------------------

func TestDiscoverHostTools_SeamMissingCopilotDir(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh": {name: "gh", isDir: false},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when ~/.copilot is missing")
	}
	if !strings.Contains(err.Error(), ".copilot") {
		t.Errorf("error should mention .copilot, got: %v", err)
	}
}

func TestDiscoverHostTools_SeamCopilotIsFile(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: false},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when ~/.copilot is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error should mention 'not a directory', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// InspectHostTools — required vs optional classification
// ---------------------------------------------------------------------------

func TestInspectHostTools_RequiredVsOptionalClassification(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
			"/usr/share/git-core/templates":            {name: "templates", isDir: true},
			"/etc/ssl/certs":                           {name: "certs", isDir: true},
			filepath.Join(home, ".config", "gh"):       {name: "gh", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	report := InspectHostTools()

	requiredNames := map[string]bool{
		"copilot": true, "git": true, "git-upload-pack": true,
		"git-receive-pack": true, "gh": true, ".copilot": true,
	}
	optionalNames := map[string]bool{
		"git templates dir": true, "system certs dir": true, "gh config dir": true,
	}

	for _, f := range report.Findings {
		if requiredNames[f.Name] {
			if !f.Required {
				t.Errorf("%q should be Required", f.Name)
			}
			if !f.Found {
				t.Errorf("%q should be Found", f.Name)
			}
		} else if optionalNames[f.Name] {
			if f.Required {
				t.Errorf("%q should not be Required", f.Name)
			}
			if !f.Found {
				t.Errorf("%q should be Found", f.Name)
			}
		} else {
			t.Errorf("unexpected finding: %q", f.Name)
		}
	}

	// Verify we got all expected findings.
	seen := make(map[string]bool)
	for _, f := range report.Findings {
		seen[f.Name] = true
	}
	for name := range requiredNames {
		if !seen[name] {
			t.Errorf("missing required finding %q", name)
		}
	}
	for name := range optionalNames {
		if !seen[name] {
			t.Errorf("missing optional finding %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// InspectHostTools — missing .copilot reporting
// ---------------------------------------------------------------------------

func TestInspectHostTools_MissingCopilotDir(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh": {name: "gh", isDir: false},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	report := InspectHostTools()
	for _, f := range report.Findings {
		if f.Name == ".copilot" {
			if f.Found {
				t.Error(".copilot should not be found")
			}
			if !f.Required {
				t.Error(".copilot should be marked required")
			}
			if !strings.Contains(f.Message, ".copilot not found") {
				t.Errorf("message should mention '.copilot not found', got: %q", f.Message)
			}
			return
		}
	}
	t.Fatal("expected a .copilot finding in report")
}

// ---------------------------------------------------------------------------
// InspectHostTools — optional dirs missing do not fail
// ---------------------------------------------------------------------------

func TestInspectHostTools_OptionalDirsMissing(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	report := InspectHostTools()
	optionalNames := map[string]bool{
		"git templates dir": true, "system certs dir": true, "gh config dir": true,
	}
	for _, f := range report.Findings {
		if optionalNames[f.Name] {
			if f.Found {
				t.Errorf("optional %q should not be found when missing", f.Name)
			}
			if f.Required {
				t.Errorf("optional %q should not be Required", f.Name)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// InspectHostTools — gh path preference
// ---------------------------------------------------------------------------

func TestInspectHostTools_GhPrefersUsrBinWhenFile(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	report := InspectHostTools()
	for _, f := range report.Findings {
		if f.Name == "gh" {
			if f.Path != "/usr/bin/gh" {
				t.Errorf("gh path = %q, want /usr/bin/gh", f.Path)
			}
			return
		}
	}
	t.Fatal("expected a gh finding in report")
}

func TestInspectHostTools_GhFallsBackToPATH(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                              {name: "gh", isDir: true},
			filepath.Join(home, ".copilot"):            {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	report := InspectHostTools()
	for _, f := range report.Findings {
		if f.Name == "gh" {
			if f.Path != "/usr/local/bin/gh" {
				t.Errorf("gh path = %q, want /usr/local/bin/gh (PATH fallback)", f.Path)
			}
			return
		}
	}
	t.Fatal("expected a gh finding in report")
}
