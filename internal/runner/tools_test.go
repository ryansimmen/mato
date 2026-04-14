package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDiscoverHostTools_ValidCopilotDir(t *testing.T) {
	home := "/fake/home"
	copilotDir := filepath.Join(home, ".copilot")
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh": {name: "gh", isDir: false},
			copilotDir:    {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools should succeed with valid ~/.copilot: %v", err)
	}
	if tools.copilotConfigDir != copilotDir {
		t.Fatalf("copilotConfigDir = %q, want %q", tools.copilotConfigDir, copilotDir)
	}
	wantCache := filepath.Join(home, ".cache", "copilot")
	if tools.copilotCacheDir != wantCache {
		t.Fatalf("copilotCacheDir = %q, want %q", tools.copilotCacheDir, wantCache)
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

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.isDir }
func (f fakeFileInfo) Sys() any           { return nil }

// setTestSeams replaces package-level function variables with test doubles
// and restores originals via t.Cleanup (using the generic setHook helper).
func setTestSeams(t *testing.T, lp func(string) (string, error), st func(string) (os.FileInfo, error), home func() (string, error), gep func() (string, error)) {
	t.Helper()
	setHook(t, &mkdirAllFn, func(string, os.FileMode) error { return nil })
	setHook(t, &goEnvGOROOTFn, func() (string, error) { return "/usr/local/go", nil })
	setHook(t, &runtimeGOROOTFn, func() string { return "/runtime/go" })
	if lp != nil {
		setHook(t, &lookPathFn, lp)
	}
	if st != nil {
		setHook(t, &statFn, st)
	}
	if home != nil {
		setHook(t, &userHomeDirFn, home)
	}
	if gep != nil {
		setHook(t, &gitExecPathFn, gep)
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

func TestFindGitHelper_SeamInPath(t *testing.T) {
	setTestSeams(t,
		makeLookPathFn(map[string]string{
			"git-upload-pack": "/usr/bin/git-upload-pack",
		}),
		nil, nil, nil,
	)

	path, err := findGitHelper("git-upload-pack")
	if err != nil {
		t.Fatalf("findGitHelper should find tool in PATH: %v", err)
	}
	if path != "/usr/bin/git-upload-pack" {
		t.Errorf("path = %q, want /usr/bin/git-upload-pack", path)
	}
}

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
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
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
			"/usr/bin/gh":                   {name: "gh", isDir: true},
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
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
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
			"/usr/bin/gh":                        {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):      {name: ".copilot", isDir: true},
			"/usr/share/git-core/templates":      {name: "templates", isDir: true},
			"/etc/ssl/certs":                     {name: "certs", isDir: true},
			filepath.Join(home, ".config", "gh"): {name: "gh", isDir: true},
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

func TestDiscoverHostTools_GoplsOptionalWhenPresent(t *testing.T) {
	home := "/fake/home"
	tools := allRequiredTools()
	tools["gopls"] = "/home/test/go/bin/gopls"
	setTestSeams(t,
		makeLookPathFn(tools),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	found, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if found.goplsPath != "/home/test/go/bin/gopls" {
		t.Fatalf("goplsPath = %q, want %q", found.goplsPath, "/home/test/go/bin/gopls")
	}
}

func TestDiscoverHostTools_GoplsOptionalWhenMissing(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	found, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if found.goplsPath != "" {
		t.Fatalf("goplsPath = %q, want empty when gopls is missing", found.goplsPath)
	}
}

func TestDiscoverHostTools_GOROOTFallsBackToRuntimeWhenGoEnvFails(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)
	setHook(t, &goEnvGOROOTFn, func() (string, error) { return "", exec.ErrNotFound })
	setHook(t, &runtimeGOROOTFn, func() string { return "/runtime/go" })

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if tools.goRoot != "/runtime/go" {
		t.Fatalf("goRoot = %q, want %q", tools.goRoot, "/runtime/go")
	}
}

func TestDiscoverHostTools_GOROOTFallsBackToRuntimeWhenGoEnvEmpty(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)
	setHook(t, &goEnvGOROOTFn, func() (string, error) { return "   ", nil })
	setHook(t, &runtimeGOROOTFn, func() string { return "/runtime/go" })

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools() error: %v", err)
	}
	if tools.goRoot != "/runtime/go" {
		t.Fatalf("goRoot = %q, want %q", tools.goRoot, "/runtime/go")
	}
}

func TestDiscoverHostTools_GOROOTFailsWhenNoSourceReturnsValue(t *testing.T) {
	home := "/fake/home"
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)
	setHook(t, &goEnvGOROOTFn, func() (string, error) { return "", exec.ErrNotFound })
	setHook(t, &runtimeGOROOTFn, func() string { return "" })

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when neither go env nor runtime GOROOT returns a value")
	}
	if !strings.Contains(err.Error(), "neither 'go env GOROOT' nor runtime.GOROOT() returned a value") {
		t.Fatalf("error = %v, want missing GOROOT message", err)
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
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: false},
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
// discoverHostTools — tool-not-found installation hints
// ---------------------------------------------------------------------------

func TestDiscoverHostTools_CopilotNotFoundIncludesHint(t *testing.T) {
	home := "/fake/home"
	tools := allRequiredTools()
	delete(tools, "copilot")
	setTestSeams(t,
		makeLookPathFn(tools),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when copilot is missing")
	}
	if !strings.Contains(err.Error(), "docs/configuration.md") {
		t.Errorf("error should contain installation hint, got: %v", err)
	}
}

func TestDiscoverHostTools_GitNotFoundIncludesHint(t *testing.T) {
	home := "/fake/home"
	tools := allRequiredTools()
	delete(tools, "git")
	setTestSeams(t,
		makeLookPathFn(tools),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when git is missing")
	}
	if !strings.Contains(err.Error(), "https://git-scm.com/downloads") {
		t.Errorf("error should contain installation hint, got: %v", err)
	}
}

func TestDiscoverHostTools_GhNotFoundIncludesHint(t *testing.T) {
	home := "/fake/home"
	tools := allRequiredTools()
	delete(tools, "gh")
	setTestSeams(t,
		makeLookPathFn(tools),
		makeStatFn(map[string]fakeFileInfo{
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	_, err := discoverHostTools()
	if err == nil {
		t.Fatal("discoverHostTools should fail when gh is missing")
	}
	if !strings.Contains(err.Error(), "https://cli.github.com/") {
		t.Errorf("error should contain installation hint, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// InspectHostTools — required vs optional classification
// ---------------------------------------------------------------------------

func TestInspectHostTools_RequiredVsOptionalClassification(t *testing.T) {
	home := "/fake/home"
	tools := allRequiredTools()
	tools["gopls"] = "/home/test/go/bin/gopls"
	setTestSeams(t,
		makeLookPathFn(tools),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                            {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):          {name: ".copilot", isDir: true},
			filepath.Join(home, ".cache", "copilot"): {name: "copilot", isDir: true},
			"/usr/share/git-core/templates":          {name: "templates", isDir: true},
			"/etc/ssl/certs":                         {name: "certs", isDir: true},
			filepath.Join(home, ".config", "gh"):     {name: "gh", isDir: true},
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
		"gopls": true, "copilot cache dir": true, "git templates dir": true, "system certs dir": true, "gh config dir": true,
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
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)

	report := InspectHostTools()
	optionalNames := map[string]bool{
		"gopls": true, "copilot cache dir": true, "git templates dir": true, "system certs dir": true, "gh config dir": true,
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
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
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
			"/usr/bin/gh":                   {name: "gh", isDir: true},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
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

func TestResolveVscodeNodePath_Found(t *testing.T) {
	script := `#!/bin/sh
unset NODE_OPTIONS
ELECTRON_RUN_AS_NODE=1 "/vscode/bin/linux-x64/abc123/node" "/some/copilot.js"
`
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return []byte(script), nil
	})
	setHook(t, &statFn, makeStatFn(map[string]fakeFileInfo{
		"/vscode/bin/linux-x64/abc123/node": {name: "node"},
	}))

	nodePath, ok := resolveVscodeNodePath("/usr/local/bin/copilot")
	if !ok {
		t.Fatal("expected resolveVscodeNodePath to find the node binary")
	}
	if nodePath != "/vscode/bin/linux-x64/abc123/node" {
		t.Fatalf("nodePath = %q, want /vscode/bin/linux-x64/abc123/node", nodePath)
	}
}

func TestResolveVscodeNodePath_NotScript(t *testing.T) {
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return nil, os.ErrNotExist
	})

	_, ok := resolveVscodeNodePath("/usr/local/bin/copilot")
	if ok {
		t.Fatal("expected resolveVscodeNodePath to return false for unreadable file")
	}
}

func TestResolveVscodeNodePath_NodeBinaryMissing(t *testing.T) {
	script := `#!/bin/sh
ELECTRON_RUN_AS_NODE=1 "/vscode/bin/linux-x64/abc123/node" "/some/copilot.js"
`
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return []byte(script), nil
	})
	setHook(t, &statFn, makeStatFn(map[string]fakeFileInfo{}))

	_, ok := resolveVscodeNodePath("/usr/local/bin/copilot")
	if ok {
		t.Fatal("expected resolveVscodeNodePath to return false when node binary is missing")
	}
}

func TestResolveVscodeNodePath_NoNodeReference(t *testing.T) {
	script := `#!/bin/sh
exec /usr/local/bin/copilot-real "$@"
`
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return []byte(script), nil
	})
	setHook(t, &statFn, makeStatFn(map[string]fakeFileInfo{}))

	_, ok := resolveVscodeNodePath("/usr/local/bin/copilot")
	if ok {
		t.Fatal("expected resolveVscodeNodePath to return false for a script without a node reference")
	}
}

func TestDiscoverHostTools_VscodeNodePath(t *testing.T) {
	home := "/fake/home"
	script := `#!/bin/sh
ELECTRON_RUN_AS_NODE=1 "/vscode/bin/linux-x64/abc123/node" "/some/copilot.js"
`
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                          {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):         {name: ".copilot", isDir: true},
			"/vscode/bin/linux-x64/abc123/node":     {name: "node"},
		}),
		func() (string, error) { return home, nil },
		nil,
	)
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return []byte(script), nil
	})

	tools, err := discoverHostTools()
	if err != nil {
		t.Fatalf("discoverHostTools failed: %v", err)
	}
	if tools.vscodeNodePath != "/vscode/bin/linux-x64/abc123/node" {
		t.Fatalf("vscodeNodePath = %q, want /vscode/bin/linux-x64/abc123/node", tools.vscodeNodePath)
	}
}

func TestInspectHostTools_VscodeNodeFound(t *testing.T) {
	home := "/fake/home"
	script := `#!/bin/sh
ELECTRON_RUN_AS_NODE=1 "/vscode/bin/linux-x64/abc123/node" "/some/copilot.js"
`
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                          {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"):         {name: ".copilot", isDir: true},
			"/vscode/bin/linux-x64/abc123/node":     {name: "node"},
		}),
		func() (string, error) { return home, nil },
		nil,
	)
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return []byte(script), nil
	})

	report := InspectHostTools()
	for _, f := range report.Findings {
		if f.Name == "vscode node" {
			if !f.Found {
				t.Fatal("expected vscode node finding to be Found=true")
			}
			if f.Path != "/vscode/bin/linux-x64/abc123/node" {
				t.Fatalf("vscode node path = %q, want /vscode/bin/linux-x64/abc123/node", f.Path)
			}
			return
		}
	}
	t.Fatal("expected a vscode node finding in report")
}

func TestInspectHostTools_VscodeNodeMissingIsOmitted(t *testing.T) {
	home := "/fake/home"
	script := `#!/bin/sh
exec /usr/local/bin/copilot-real "$@"
`
	setTestSeams(t,
		makeLookPathFn(allRequiredTools()),
		makeStatFn(map[string]fakeFileInfo{
			"/usr/bin/gh":                   {name: "gh", isDir: false},
			filepath.Join(home, ".copilot"): {name: ".copilot", isDir: true},
		}),
		func() (string, error) { return home, nil },
		nil,
	)
	setHook(t, &readFileFn, func(path string) ([]byte, error) {
		return []byte(script), nil
	})

	report := InspectHostTools()
	for _, f := range report.Findings {
		if f.Name == "vscode node" {
			t.Fatal("expected missing best-effort vscode node finding to be omitted")
		}
	}
}
