package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Test seams for controlling tool lookups in tests.
var lookPathFn = exec.LookPath
var statFn = os.Stat
var userHomeDirFn = os.UserHomeDir
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
	gitPath            string
	gitUploadPackPath  string
	gitReceivePackPath string
	ghPath             string
	goRoot             string
	gitTemplatesDir    string
	hasGitTemplates    bool
	systemCertsDir     string
	hasSystemCerts     bool
	homeDir            string
	ghConfigDir        string
	hasGhConfig        bool
}

// discoverHostTools locates all host binaries and directories required
// for Docker agent containers. It fails fast if a required tool is missing.
func discoverHostTools() (hostTools, error) {
	copilotPath, err := lookPathFn("copilot")
	if err != nil {
		return hostTools{}, fmt.Errorf("find copilot CLI: %w\n  Install: npm install -g @githubnext/github-copilot-cli", err)
	}
	gitPath, err := lookPathFn("git")
	if err != nil {
		return hostTools{}, fmt.Errorf("find git CLI: %w\n  Install: apt install git (Linux) or brew install git (macOS)", err)
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

	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasGhConfig := false
	if info, statErr := statFn(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	return hostTools{
		copilotPath:        copilotPath,
		copilotConfigDir:   copilotConfigDir,
		gitPath:            gitPath,
		gitUploadPackPath:  gitUploadPackPath,
		gitReceivePackPath: gitReceivePackPath,
		ghPath:             ghPath,
		goRoot:             runtime.GOROOT(),
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
			r.Findings = append(r.Findings, ToolFinding{
				Name:     tool.name,
				Path:     path,
				Required: tool.required,
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
