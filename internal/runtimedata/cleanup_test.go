package runtimedata

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mato/internal/taskfile"
)

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

func seedVerdict(t *testing.T, tasksDir, filename string) {
	t.Helper()
	verdictDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(verdictDir, 0o755); err != nil {
		t.Fatalf("MkdirAll messages: %v", err)
	}
	payload := map[string]string{"verdict": "reject", "reason": "needs work"}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if err := os.WriteFile(taskfile.VerdictPath(tasksDir, filename), data, 0o644); err != nil {
		t.Fatalf("WriteFile verdict: %v", err)
	}
}

func TestDeleteRuntimeArtifacts_Success(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	if err := UpdateTaskState(tasksDir, filename, func(s *TaskState) {
		s.LastOutcome = "test"
	}); err != nil {
		t.Fatalf("setup taskstate: %v", err)
	}

	for _, kind := range []string{KindWork, KindReview} {
		if _, err := LoadOrCreateSession(tasksDir, kind, filename, "task/task"); err != nil {
			t.Fatalf("setup %s session: %v", kind, err)
		}
	}

	seedVerdict(t, tasksDir, filename)

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if output != "" {
		t.Fatalf("expected no stderr output, got: %q", output)
	}

	state, err := LoadTaskState(tasksDir, filename)
	if err != nil {
		t.Fatalf("LoadTaskState: %v", err)
	}
	if state != nil {
		t.Fatal("taskstate should have been deleted")
	}

	for _, kind := range []string{KindWork, KindReview} {
		session, err := LoadSession(tasksDir, kind, filename)
		if err != nil {
			t.Fatalf("LoadSession %s: %v", kind, err)
		}
		if session != nil {
			t.Fatalf("%s session should have been deleted", kind)
		}
	}

	if _, err := os.Stat(taskfile.VerdictPath(tasksDir, filename)); !os.IsNotExist(err) {
		t.Fatal("verdict file should have been deleted")
	}
}

func TestDeleteRuntimeArtifacts_TaskStateError(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "taskstate"))

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete taskstate for "+filename) {
		t.Fatalf("expected taskstate warning, got: %q", output)
	}
	if !strings.Contains(output, "not a directory") {
		t.Fatalf("expected underlying error in taskstate warning, got: %q", output)
	}
	if strings.Contains(output, "sessionmeta") {
		t.Fatalf("unexpected sessionmeta warning in output: %q", output)
	}
}

func TestDeleteRuntimeArtifacts_SessionMetaError(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "sessionmeta"))

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete sessionmeta for "+filename) {
		t.Fatalf("expected sessionmeta warning, got: %q", output)
	}
	if !strings.Contains(output, "not a directory") {
		t.Fatalf("expected underlying error in sessionmeta warning, got: %q", output)
	}
	if strings.Contains(output, "taskstate") {
		t.Fatalf("unexpected taskstate warning in output: %q", output)
	}
}

func TestDeleteRuntimeArtifacts_BothFail(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "taskstate"))
	replaceWithFile(t, filepath.Join(tasksDir, "runtime", "sessionmeta"))

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete taskstate for "+filename) {
		t.Fatalf("expected taskstate warning, got: %q", output)
	}
	if !strings.Contains(output, "warning: could not delete sessionmeta for "+filename) {
		t.Fatalf("expected sessionmeta warning, got: %q", output)
	}
}

func TestDeleteRuntimeArtifacts_VerdictError(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	replaceWithFile(t, filepath.Join(tasksDir, "messages"))

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if !strings.Contains(output, "warning: could not delete verdict for "+filename) {
		t.Fatalf("expected verdict warning, got: %q", output)
	}
	if !strings.Contains(output, "not a directory") {
		t.Fatalf("expected underlying error in verdict warning, got: %q", output)
	}
	if strings.Contains(output, "taskstate") {
		t.Fatalf("unexpected taskstate warning in output: %q", output)
	}
	if strings.Contains(output, "sessionmeta") {
		t.Fatalf("unexpected sessionmeta warning in output: %q", output)
	}
}

func TestDeleteRuntimeArtifacts_VerdictMissing_NoWarning(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if output != "" {
		t.Fatalf("expected no stderr output for missing verdict, got: %q", output)
	}
}

func TestDeleteRuntimeArtifacts_StaleVerdictCleanedOnReset(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	seedVerdict(t, tasksDir, filename)

	if _, ok := taskfile.ReadVerdictRejection(tasksDir, filename); !ok {
		t.Fatal("expected verdict to be readable before cleanup")
	}

	output := captureStderr(t, func() {
		DeleteRuntimeArtifacts(tasksDir, filename)
	})

	if output != "" {
		t.Fatalf("expected no stderr output, got: %q", output)
	}

	if _, ok := taskfile.ReadVerdictRejection(tasksDir, filename); ok {
		t.Fatal("stale verdict should not be readable after cleanup")
	}
}

func TestDeleteRuntimeArtifactsPreservingVerdict_KeepsVerdictFile(t *testing.T) {
	tasksDir := t.TempDir()
	filename := "task.md"

	if err := UpdateTaskState(tasksDir, filename, func(s *TaskState) {
		s.LastOutcome = "test"
	}); err != nil {
		t.Fatalf("setup taskstate: %v", err)
	}

	for _, kind := range []string{KindWork, KindReview} {
		if _, err := LoadOrCreateSession(tasksDir, kind, filename, "task/task"); err != nil {
			t.Fatalf("setup %s session: %v", kind, err)
		}
	}

	seedVerdict(t, tasksDir, filename)

	output := captureStderr(t, func() {
		DeleteRuntimeArtifactsPreservingVerdict(tasksDir, filename)
	})

	if output != "" {
		t.Fatalf("expected no stderr output, got: %q", output)
	}

	state, err := LoadTaskState(tasksDir, filename)
	if err != nil {
		t.Fatalf("LoadTaskState: %v", err)
	}
	if state != nil {
		t.Fatal("taskstate should have been deleted")
	}

	for _, kind := range []string{KindWork, KindReview} {
		session, err := LoadSession(tasksDir, kind, filename)
		if err != nil {
			t.Fatalf("LoadSession %s: %v", kind, err)
		}
		if session != nil {
			t.Fatalf("%s session should have been deleted", kind)
		}
	}

	if _, ok := taskfile.ReadVerdictRejection(tasksDir, filename); !ok {
		t.Fatal("verdict file should be preserved by DeleteRuntimeArtifactsPreservingVerdict")
	}
}
