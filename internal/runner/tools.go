package runner

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Test seams for controlling tool lookups in tests.
//
// NOTE: These package-level mutable variables prevent t.Parallel() within
// this package. Struct-based dependency injection would be needed for true
// parallel test safety.
var lookPathFn = exec.LookPath
var statFn = os.Stat
var mkdirAllFn = os.MkdirAll
var userHomeDirFn = os.UserHomeDir

//nolint:staticcheck // Compatibility fallback for standalone mato binaries when 'go' is unavailable on PATH.
var runtimeGOROOTFn = runtime.GOROOT
var goEnvGOROOTFn = func() (string, error) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
var gitExecPathFn = func() (string, error) {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// hostTools holds the resolved paths and availability of host binaries
// and directories that are bind-mounted into Docker agent containers.
type hostTools struct {
	copilotPath        string
	copilotConfigDir   string
	copilotCacheDir    string
	vscodeNodePath     string
	gitPath            string
	gitUploadPackPath  string
	gitReceivePackPath string
	ghPath             string
	goplsPath          string
	goRoot             string
	gitTemplatesDir    string
	hasGitTemplates    bool
	systemCertsDir     string
	hasSystemCerts     bool
	homeDir            string
	ghConfigDir        string
	hasGhConfig        bool
}

// vscodeNodeRe matches quoted paths to the VS Code bundled node binary
// in copilot wrapper scripts. The binary path ends with /node and is
// enclosed in double quotes within the shell script.
var vscodeNodeRe = regexp.MustCompile(`"(/[^"]+/node)"`)

// readFileFn wraps os.ReadFile for test injection.
var readFileFn = os.ReadFile

// resolveVscodeNodePath reads the copilot wrapper script and extracts the
// path to the VS Code-bundled node binary. The copilot CLI installed by
// VS Code is a thin shell script that invokes node via an absolute path
// like /vscode/bin/linux-x64/<commit>/node. When mato bind-mounts this
// script into a Docker container, the node binary must also be mounted
// at the same path for the script to work.
//
// Returns the resolved path and true if found and the binary exists on
// disk, or empty string and false otherwise. This is best-effort: a
// missing node path is not fatal because standalone copilot binaries
// that do not depend on a separate node installation also exist.
func resolveVscodeNodePath(copilotPath string) (string, bool) {
	data, err := readFileFn(copilotPath)
	if err != nil {
		return "", false
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		m := vscodeNodeRe.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		nodePath := m[1]
		if info, err := statFn(nodePath); err == nil && !info.IsDir() {
			return nodePath, true
		}
	}
	return "", false
}

// discoverHostTools locates all host binaries and directories required
// for Docker agent containers. It fails fast if a required tool is missing.
func discoverHostTools() (hostTools, error) {
	copilotPath, err := lookPathFn("copilot")
	if err != nil {
		return hostTools{}, fmt.Errorf("find copilot CLI: %w\n  Install: see docs/configuration.md or https://docs.github.com/en/copilot", err)
	}
	gitPath, err := lookPathFn("git")
	if err != nil {
		return hostTools{}, fmt.Errorf("find git CLI: %w\n  Install: see https://git-scm.com/downloads", err)
	}
	gitUploadPackPath, err := findGitHelper("git-upload-pack")
	if err != nil {
		return hostTools{}, err
	}
	gitReceivePackPath, err := findGitHelper("git-receive-pack")
	if err != nil {
		return hostTools{}, err
	}

	ghPath := "/usr/bin/gh"
	if info, statErr := statFn(ghPath); statErr != nil || info.IsDir() {
		ghPath, err = lookPathFn("gh")
		if err != nil {
			return hostTools{}, fmt.Errorf("find gh CLI: %w\n  Install: see https://cli.github.com/", err)
		}
	}
	goplsPath, _ := lookPathFn("gopls")
	goRoot, err := goEnvGOROOTFn()
	if err != nil || strings.TrimSpace(goRoot) == "" {
		goRoot = strings.TrimSpace(runtimeGOROOTFn())
	}
	if goRoot == "" {
		return hostTools{}, fmt.Errorf("resolve GOROOT: neither 'go env GOROOT' nor runtime.GOROOT() returned a value")
	}

	gitTemplatesDir := "/usr/share/git-core/templates"
	hasGitTemplates := false
	if info, statErr := statFn(gitTemplatesDir); statErr == nil && info.IsDir() {
		hasGitTemplates = true
	}

	systemCertsDir := "/etc/ssl/certs"
	hasSystemCerts := false
	if info, statErr := statFn(systemCertsDir); statErr == nil && info.IsDir() {
		hasSystemCerts = true
	}

	homeDir, err := userHomeDirFn()
	if err != nil {
		return hostTools{}, fmt.Errorf("resolve home directory: %w", err)
	}

	copilotConfigDir := filepath.Join(homeDir, ".copilot")
	info, statErr := statFn(copilotConfigDir)
	if statErr != nil {
		return hostTools{}, fmt.Errorf("~/.copilot directory not found at %s: %w", copilotConfigDir, statErr)
	}
	if !info.IsDir() {
		return hostTools{}, fmt.Errorf("~/.copilot path %s exists but is not a directory", copilotConfigDir)
	}

	copilotCacheDir := filepath.Join(homeDir, ".cache", "copilot")
	if err := mkdirAllFn(copilotCacheDir, 0o755); err != nil {
		return hostTools{}, fmt.Errorf("create copilot cache directory %s: %w", copilotCacheDir, err)
	}

	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasGhConfig := false
	if info, statErr := statFn(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	vscodeNodePath, _ := resolveVscodeNodePath(copilotPath)

	return hostTools{
		copilotPath:        copilotPath,
		copilotConfigDir:   copilotConfigDir,
		copilotCacheDir:    copilotCacheDir,
		vscodeNodePath:     vscodeNodePath,
		gitPath:            gitPath,
		gitUploadPackPath:  gitUploadPackPath,
		gitReceivePackPath: gitReceivePackPath,
		ghPath:             ghPath,
		goplsPath:          goplsPath,
		goRoot:             goRoot,
		gitTemplatesDir:    gitTemplatesDir,
		hasGitTemplates:    hasGitTemplates,
		systemCertsDir:     systemCertsDir,
		hasSystemCerts:     hasSystemCerts,
		homeDir:            homeDir,
		ghConfigDir:        ghConfigDir,
		hasGhConfig:        hasGhConfig,
	}, nil
}

// ToolFinding describes a single tool or directory inspection result.
type ToolFinding struct {
	Name     string
	Path     string // resolved path, empty if not found
	Required bool
	Found    bool
	Message  string
}

// ToolReport collects all tool inspection findings.
type ToolReport struct {
	Findings []ToolFinding
}

// InspectHostTools probes all host tools and directories, returning
// structured findings for every item regardless of success or failure.
// This is a parallel implementation that mirrors the checks in
// discoverHostTools() but collects findings instead of failing fast.
// discoverHostTools() is NOT modified.
func InspectHostTools() ToolReport {
	var r ToolReport
	var copilotPathFound string

	// Required tools.
	for _, tool := range []struct {
		name     string
		lookup   func() (string, error)
		required bool
	}{
		{"copilot", func() (string, error) { return lookPathFn("copilot") }, true},
		{"git", func() (string, error) { return lookPathFn("git") }, true},
		{"git-upload-pack", func() (string, error) { return findGitHelper("git-upload-pack") }, true},
		{"git-receive-pack", func() (string, error) { return findGitHelper("git-receive-pack") }, true},
		{"gh", func() (string, error) {
			ghPath := "/usr/bin/gh"
			if info, err := statFn(ghPath); err == nil && !info.IsDir() {
				return ghPath, nil
			}
			return lookPathFn("gh")
		}, true},
	} {
		path, err := tool.lookup()
		if err != nil {
			r.Findings = append(r.Findings, ToolFinding{
				Name:     tool.name,
				Required: tool.required,
				Found:    false,
				Message:  fmt.Sprintf("%s not found", tool.name),
			})
		} else {
			if tool.name == "copilot" {
				copilotPathFound = path
			}
			r.Findings = append(r.Findings, ToolFinding{
				Name:     tool.name,
				Path:     path,
				Required: tool.required,
				Found:    true,
				Message:  fmt.Sprintf("%s: %s", tool.name, path),
			})
		}
	}

	// VS Code bundled node binary (required by the copilot wrapper script).
	if copilotPathFound != "" {
		if nodePath, ok := resolveVscodeNodePath(copilotPathFound); ok {
			r.Findings = append(r.Findings, ToolFinding{
				Name:    "vscode node",
				Path:    nodePath,
				Found:   true,
				Message: fmt.Sprintf("vscode node: %s", nodePath),
			})
		}
	}

	for _, tool := range []struct {
		name   string
		lookup func() (string, error)
	}{
		{"gopls", func() (string, error) { return lookPathFn("gopls") }},
	} {
		path, err := tool.lookup()
		if err != nil {
			r.Findings = append(r.Findings, ToolFinding{
				Name:     tool.name,
				Required: false,
				Found:    false,
				Message:  "gopls not found; Go LSP features will be unavailable in Docker agent containers",
			})
		} else {
			r.Findings = append(r.Findings, ToolFinding{
				Name:     tool.name,
				Path:     path,
				Required: false,
				Found:    true,
				Message:  fmt.Sprintf("%s: %s", tool.name, path),
			})
		}
	}

	// ~/.copilot directory (bind-mounted unconditionally by Docker args).
	homeDir, homeErr := userHomeDirFn()
	if homeErr == nil {
		copilotDir := filepath.Join(homeDir, ".copilot")
		if info, err := statFn(copilotDir); err == nil && info.IsDir() {
			r.Findings = append(r.Findings, ToolFinding{
				Name:     ".copilot",
				Path:     copilotDir,
				Required: true,
				Found:    true,
				Message:  fmt.Sprintf("~/.copilot: %s", copilotDir),
			})
		} else {
			r.Findings = append(r.Findings, ToolFinding{
				Name:     ".copilot",
				Path:     copilotDir,
				Required: true,
				Found:    false,
				Message:  fmt.Sprintf("~/.copilot not found at %s", copilotDir),
			})
		}
	} else {
		r.Findings = append(r.Findings, ToolFinding{
			Name:     ".copilot",
			Required: true,
			Found:    false,
			Message:  "~/.copilot not found (cannot resolve home directory)",
		})
	}

	// Optional directories.
	type optDir struct {
		name string
		path string
	}
	var optDirs []optDir

	if homeErr == nil {
		copilotCacheDir := filepath.Join(homeDir, ".cache", "copilot")
		optDirs = append(optDirs, optDir{"copilot cache dir", copilotCacheDir})
	}

	gitTemplatesDir := "/usr/share/git-core/templates"
	optDirs = append(optDirs, optDir{"git templates dir", gitTemplatesDir})

	systemCertsDir := "/etc/ssl/certs"
	optDirs = append(optDirs, optDir{"system certs dir", systemCertsDir})

	if homeErr == nil {
		ghConfigDir := filepath.Join(homeDir, ".config", "gh")
		optDirs = append(optDirs, optDir{"gh config dir", ghConfigDir})
	}

	for _, od := range optDirs {
		if info, err := statFn(od.path); err == nil && info.IsDir() {
			r.Findings = append(r.Findings, ToolFinding{
				Name:    od.name,
				Path:    od.path,
				Found:   true,
				Message: fmt.Sprintf("%s: %s", od.name, od.path),
			})
		} else {
			r.Findings = append(r.Findings, ToolFinding{
				Name:    od.name,
				Path:    od.path,
				Found:   false,
				Message: fmt.Sprintf("%s not found at %s", od.name, od.path),
			})
		}
	}

	return r
}

// findGitHelper locates a git helper binary (e.g. "git-upload-pack") by
// checking PATH first, then falling back to git's exec-path.
func findGitHelper(name string) (string, error) {
	if path, err := lookPathFn(name); err == nil {
		return path, nil
	}
	execPath, err := gitExecPathFn()
	if err != nil {
		return "", fmt.Errorf("find %s: git --exec-path failed: %w", name, err)
	}
	candidate := filepath.Join(execPath, name)
	if _, err := statFn(candidate); err != nil {
		return "", fmt.Errorf("find %s: not in PATH or %s: %w", name, candidate, err)
	}
	return candidate, nil
}
