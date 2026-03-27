package pause

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRead_MissingFile(t *testing.T) {
	tasksDir := t.TempDir()
	state, err := Read(tasksDir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if state.Active {
		t.Fatalf("Active = true, want false")
	}
}

func TestRead_ValidSentinel(t *testing.T) {
	tasksDir := t.TempDir()
	fixed := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte(fixed.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state, err := Read(tasksDir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !state.Active {
		t.Fatal("Active = false, want true")
	}
	if state.ProblemKind != ProblemNone {
		t.Fatalf("ProblemKind = %v, want %v", state.ProblemKind, ProblemNone)
	}
	if !state.Since.Equal(fixed) {
		t.Fatalf("Since = %v, want %v", state.Since, fixed)
	}
}

func TestRead_UnreadableInjected(t *testing.T) {
	tasksDir := t.TempDir()
	path := filepath.Join(tasksDir, ".paused")
	if err := os.WriteFile(path, []byte("2026-03-23T10:00:00Z\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	orig := readFileFn
	readFileFn = func(string) ([]byte, error) { return nil, errors.New("boom") }
	defer func() { readFileFn = orig }()

	state, err := Read(tasksDir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if state.ProblemKind != ProblemUnreadable {
		t.Fatalf("ProblemKind = %v, want %v", state.ProblemKind, ProblemUnreadable)
	}
}

func TestRead_Malformed(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("nope\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	state, err := Read(tasksDir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if state.ProblemKind != ProblemMalformed {
		t.Fatalf("ProblemKind = %v, want %v", state.ProblemKind, ProblemMalformed)
	}
}

func TestRead_StatError(t *testing.T) {
	orig := statFn
	statFn = func(string) (os.FileInfo, error) { return nil, errors.New("stat boom") }
	defer func() { statFn = orig }()

	_, err := Read(t.TempDir())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPause_MissingCreatesSentinel(t *testing.T) {
	tasksDir := t.TempDir()
	fixed := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)

	result, err := Pause(tasksDir, fixed)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if result.AlreadyPaused || result.Repaired {
		t.Fatalf("unexpected result: %#v", result)
	}
	if !result.Since.Equal(fixed) {
		t.Fatalf("Since = %v, want %v", result.Since, fixed)
	}
	data, err := os.ReadFile(filepath.Join(tasksDir, ".paused"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != fixed.Format(time.RFC3339)+"\n" {
		t.Fatalf("content = %q", string(data))
	}
}

func TestPause_AlreadyPausedPreservesOriginal(t *testing.T) {
	tasksDir := t.TempDir()
	original := time.Date(2026, 3, 20, 9, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte(original.Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result, err := Pause(tasksDir, time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !result.AlreadyPaused {
		t.Fatalf("AlreadyPaused = false, want true")
	}
	if !result.Since.Equal(original) {
		t.Fatalf("Since = %v, want %v", result.Since, original)
	}
}

func TestPause_MalformedRepairs(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("bad\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	fixed := time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC)

	result, err := Pause(tasksDir, fixed)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !result.Repaired {
		t.Fatalf("Repaired = false, want true")
	}
	if !result.Since.Equal(fixed) {
		t.Fatalf("Since = %v, want %v", result.Since, fixed)
	}
}

func TestPause_MalformedWriteFailure(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("bad\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	orig := writeFileFn
	writeFileFn = func(string, []byte) error { return errors.New("write boom") }
	defer func() { writeFileFn = orig }()

	_, err := Pause(tasksDir, time.Date(2026, 3, 23, 10, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "malformed sentinel") || !strings.Contains(err.Error(), "write boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResume_RemovesSentinel(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tasksDir, ".paused"), []byte("2026-03-23T10:00:00Z\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	result, err := Resume(tasksDir)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !result.WasActive {
		t.Fatal("WasActive = false, want true")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, ".paused")); !os.IsNotExist(err) {
		t.Fatalf("expected sentinel removed, stat err = %v", err)
	}
}

func TestResume_MissingSentinel(t *testing.T) {
	result, err := Resume(t.TempDir())
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.WasActive {
		t.Fatal("WasActive = true, want false")
	}
}
