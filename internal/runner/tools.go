package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// hostTools holds the resolved paths and availability of host binaries
// and directories that are bind-mounted into Docker agent containers.
type hostTools struct {
	copilotPath        string
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
	copilotPath, err := exec.LookPath("copilot")
	if err != nil {
		return hostTools{}, fmt.Errorf("find copilot CLI: %w", err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return hostTools{}, fmt.Errorf("find git CLI: %w", err)
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
	if info, statErr := os.Stat(ghPath); statErr != nil || info.IsDir() {
		ghPath, err = exec.LookPath("gh")
		if err != nil {
			return hostTools{}, fmt.Errorf("find gh CLI: %w", err)
		}
	}

	gitTemplatesDir := "/usr/share/git-core/templates"
	hasGitTemplates := false
	if info, statErr := os.Stat(gitTemplatesDir); statErr == nil && info.IsDir() {
		hasGitTemplates = true
	}

	systemCertsDir := "/etc/ssl/certs"
	hasSystemCerts := false
	if info, statErr := os.Stat(systemCertsDir); statErr == nil && info.IsDir() {
		hasSystemCerts = true
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return hostTools{}, fmt.Errorf("resolve home directory: %w", err)
	}
	ghConfigDir := filepath.Join(homeDir, ".config", "gh")
	hasGhConfig := false
	if info, statErr := os.Stat(ghConfigDir); statErr == nil && info.IsDir() {
		hasGhConfig = true
	}

	return hostTools{
		copilotPath:        copilotPath,
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

// findGitHelper locates a git helper binary (e.g. "git-upload-pack") by
// checking PATH first, then falling back to git's exec-path.
func findGitHelper(name string) (string, error) {
	if path, err := exec.LookPath(name); err == nil {
		return path, nil
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "", fmt.Errorf("find %s: git --exec-path failed: %w", name, err)
	}
	candidate := filepath.Join(strings.TrimSpace(string(out)), name)
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("find %s: not in PATH or %s: %w", name, candidate, err)
	}
	return candidate, nil
}
