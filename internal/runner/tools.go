package runner

import (
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
var evalSymlinksFn = filepath.EvalSymlinks

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
	copilotRuntimeRoot string
	copilotBinDir      string
	copilotConfigDir   string
	copilotCacheDir    string
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

// vscodeCopilotShimRe matches the VS Code shim path embedded in the wrapper
// script shipped by the Copilot Chat extension.
var vscodeCopilotShimRe = regexp.MustCompile(`"(/[^"]+/copilotCLIShim\.js)"`)

// readFileFn wraps os.ReadFile for test injection.
var readFileFn = os.ReadFile

func isVscodeCopilotWrapper(copilotPath string) bool {
	data, err := readFileFn(copilotPath)
	if err != nil {
		return false
	}
	return vscodeCopilotShimRe.Match(data)
}

func isWithinDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != "..")
}

func resolveCopilotRuntimeMount(copilotPath string) (string, string, bool) {
	if strings.TrimSpace(copilotPath) == "" {
		return "", "", false
	}
	binDir := filepath.Dir(copilotPath)
	if filepath.Base(binDir) != "bin" {
		return "", "", false
	}
	runtimeRoot := filepath.Dir(binDir)
	if info, err := statFn(filepath.Join(runtimeRoot, "bin", "node")); err != nil || info.IsDir() {
		return "", "", false
	}
	pkgRoot := filepath.Join(runtimeRoot, "lib", "node_modules", "@github", "copilot")
	if info, err := statFn(pkgRoot); err == nil && info.IsDir() {
		return runtimeRoot, binDir, true
	}
	resolvedPath, err := evalSymlinksFn(copilotPath)
	if err != nil {
		return "", "", false
	}
	if !isWithinDir(pkgRoot, resolvedPath) {
		return "", "", false
	}
	return runtimeRoot, binDir, true
}

func findFallbackCopilot(currentPath string) (string, bool) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		candidate := filepath.Join(dir, "copilot")
		if filepath.Clean(candidate) == filepath.Clean(currentPath) {
			continue
		}
		if info, err := statFn(candidate); err != nil || info.IsDir() {
			continue
		}
		if isVscodeCopilotWrapper(candidate) {
			continue
		}
		return candidate, true
	}
	return "", false
}

type copilotTool struct {
	path        string
	runtimeRoot string
	binDir      string
	wrapperPath string
}

func resolveCopilotTool() (copilotTool, error) {
	copilotPath, err := lookPathFn("copilot")
	if err != nil {
		return copilotTool{}, fmt.Errorf("find copilot CLI: %w\n  Install: see docs/configuration.md or https://docs.github.com/en/copilot", err)
	}
	tool := copilotTool{path: copilotPath}
	if isVscodeCopilotWrapper(copilotPath) {
		fallbackPath, ok := findFallbackCopilot(copilotPath)
		if !ok {
			return copilotTool{}, fmt.Errorf("find copilot CLI: found VS Code wrapper at %s but no non-wrapper copilot executable later on PATH; install the CLI or move it ahead of the VS Code wrapper", copilotPath)
		}
		tool.path = fallbackPath
		tool.wrapperPath = copilotPath
	}
	tool.runtimeRoot, tool.binDir, _ = resolveCopilotRuntimeMount(tool.path)
	return tool, nil
}

// discoverHostTools locates all host binaries and directories required
// for Docker agent containers. It fails fast if a required tool is missing.
func discoverHostTools() (hostTools, error) {
	copilot, err := resolveCopilotTool()
	if err != nil {
		return hostTools{}, err
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

	return hostTools{
		copilotPath:        copilot.path,
		copilotRuntimeRoot: copilot.runtimeRoot,
		copilotBinDir:      copilot.binDir,
		copilotConfigDir:   copilotConfigDir,
		copilotCacheDir:    copilotCacheDir,
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

	if copilot, err := resolveCopilotTool(); err != nil {
		r.Findings = append(r.Findings, ToolFinding{
			Name:     "copilot",
			Required: true,
			Found:    false,
			Message:  err.Error(),
		})
	} else {
		message := fmt.Sprintf("copilot: %s", copilot.path)
		if copilot.wrapperPath != "" {
			message = fmt.Sprintf("copilot: %s (selected after skipping VS Code wrapper %s)", copilot.path, copilot.wrapperPath)
		}
		r.Findings = append(r.Findings, ToolFinding{
			Name:     "copilot",
			Path:     copilot.path,
			Required: true,
			Found:    true,
			Message:  message,
		})
	}

	// Required tools.
	for _, tool := range []struct {
		name     string
		lookup   func() (string, error)
		required bool
	}{
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
