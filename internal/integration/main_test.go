package integration_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

var matoBinaryPath string

func integrationModuleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func buildMatoBinary() (string, error) {
	tempDir, err := os.MkdirTemp("", "mato-integration-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir for test binary: %w", err)
	}

	binPath := filepath.Join(tempDir, "mato-test")
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/mato")
	cmd.Dir = integrationModuleRoot()
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return "", fmt.Errorf("build test binary: %w\n%s", err, string(out))
	}

	return binPath, nil
}

func TestMain(m *testing.M) {
	binPath, err := buildMatoBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	matoBinaryPath = binPath

	code := m.Run()
	_ = os.RemoveAll(filepath.Dir(binPath))
	os.Exit(code)
}
