package runtimecleanup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/sessionmeta"
	"mato/internal/taskstate"
)

// captureStderr redirects os.Stderr to a pipe, runs fn, and returns whatever
// was written. Tests that use this must not call t.Parallel because os.Stderr
// is process-global.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	return buf.String()
}

// replaceWithFile removes path (dir or file) and creates a regular file in
// its place so that child path lookups fail with ENOTDIR.
func replaceWithFile(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("RemoveAll %s: %v", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestDeleteAll_Success(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	// Seed taskstate.
	if err := taskstate.Update(tasksDir, filename, func(s *taskstate.TaskState) {
		s.LastOutcome = "test"
	}); err != nil {
		t.Fatalf("setup taskstate: %v", err)
	}

	// Seed sessionmeta (both work and review).
	for _, kind := range []string{sessionmeta.KindWork, sessionmeta.KindReview} {
		if _, err := sessionmeta.LoadOrCreate(tasksDir, kind, filename, "task/task"); err != nil {
			t.Fatalf("setup %s session: %v", kind, err)
		}
	}

	output := captureStderr(t, func() {
		DeleteAll(tasksDir, filename)
	})

	if output != "" {
		t.Fatalf("expected no stderr output, got: %q", output)
	}

	// Verify taskstate is gone.
	state, err := taskstate.Load(tasksDir, filename)
	if err != nil {
		t.Fatalf("Load taskstate: %v", err)
	}
	if state != nil {
		t.Fatal("taskstate should have been deleted")
	}

	// Verify both session files are gone.
	for _, kind := range []string{sessionmeta.KindWork, sessionmeta.KindReview} {
		session, err := sessionmeta.Load(tasksDir, kind, filename)
		if err != nil {
			t.Fatalf("Load %s session: %v", kind, err)
		}
		if session != nil {
			t.Fatalf("%s session should have been deleted", kind)
		}
	}
}

func TestDeleteAll_TaskStateError(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	// Replace the taskstate runtime directory with a regular file so that
	// os.Remove on child paths fails with ENOTDIR (not swallowed by
	// os.IsNotExist).
	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "taskstate"))

	// sessionmeta runtime dir does not exist → deletes return os.ErrNotExist
	// which is silently ignored, so no sessionmeta warning.

	output := captureStderr(t, func() {
		DeleteAll(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete taskstate for "+filename) {
		t.Fatalf("expected taskstate warning, got: %q", output)
	}
	// The warning must also surface the underlying OS error so operators can
	// diagnose the root cause (task requirement: "containing the filename
	// and error").
	if !strings.Contains(output, "not a directory") {
		t.Fatalf("expected underlying error in taskstate warning, got: %q", output)
	}
	if strings.Contains(output, "sessionmeta") {
		t.Fatalf("unexpected sessionmeta warning in output: %q", output)
	}
}

func TestDeleteAll_SessionMetaError(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	// Replace the sessionmeta runtime directory with a regular file.
	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "sessionmeta"))

	// taskstate runtime dir does not exist → delete returns os.ErrNotExist
	// which is silently ignored.

	output := captureStderr(t, func() {
		DeleteAll(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete sessionmeta for "+filename) {
		t.Fatalf("expected sessionmeta warning, got: %q", output)
	}
	// Verify the underlying error is included in the warning output.
	if !strings.Contains(output, "not a directory") {
		t.Fatalf("expected underlying error in sessionmeta warning, got: %q", output)
	}
	if strings.Contains(output, "taskstate") {
		t.Fatalf("unexpected taskstate warning in output: %q", output)
	}
}

func TestDeleteAll_BothFail(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	// Replace both runtime directories with regular files.
	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "taskstate"))
	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "sessionmeta"))

	output := captureStderr(t, func() {
		DeleteAll(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete taskstate for "+filename) {
		t.Fatalf("expected taskstate warning, got: %q", output)
	}
	if !strings.Contains(output, "warning: could not delete sessionmeta for "+filename) {
		t.Fatalf("expected sessionmeta warning, got: %q", output)
	}
}
