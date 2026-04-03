package taskfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadVerdictRejection_ValidReject(t *testing.T) {
	tasksDir := t.TempDir()
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	verdict := map[string]string{"verdict": "reject", "reason": "missing tests"}
	data, _ := json.Marshal(verdict)
	if err := os.WriteFile(filepath.Join(msgDir, "verdict-task.md.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	vr, ok := ReadVerdictRejection(tasksDir, "task.md")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if vr.Reason != "missing tests" {
		t.Fatalf("Reason = %q, want %q", vr.Reason, "missing tests")
	}
	if vr.Timestamp.IsZero() {
		t.Fatal("expected non-zero timestamp from file mod time")
	}
}

func TestReadVerdictRejection_ApproveReturnsNotOK(t *testing.T) {
	tasksDir := t.TempDir()
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	verdict := map[string]string{"verdict": "approve", "reason": ""}
	data, _ := json.Marshal(verdict)
	if err := os.WriteFile(filepath.Join(msgDir, "verdict-task.md.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, ok := ReadVerdictRejection(tasksDir, "task.md")
	if ok {
		t.Fatal("expected ok=false for approve verdict")
	}
}

func TestReadVerdictRejection_MissingFile(t *testing.T) {
	_, ok := ReadVerdictRejection(t.TempDir(), "task.md")
	if ok {
		t.Fatal("expected ok=false for missing verdict file")
	}
}

func TestReadVerdictRejection_InvalidJSON(t *testing.T) {
	tasksDir := t.TempDir()
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(msgDir, "verdict-task.md.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, ok := ReadVerdictRejection(tasksDir, "task.md")
	if ok {
		t.Fatal("expected ok=false for invalid JSON")
	}
}

func TestReadVerdictRejection_EmptyReason(t *testing.T) {
	tasksDir := t.TempDir()
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	verdict := map[string]string{"verdict": "reject", "reason": ""}
	data, _ := json.Marshal(verdict)
	if err := os.WriteFile(filepath.Join(msgDir, "verdict-task.md.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, ok := ReadVerdictRejection(tasksDir, "task.md")
	if ok {
		t.Fatal("expected ok=false for empty reason")
	}
}

func TestReadVerdictRejection_CaseInsensitiveVerdict(t *testing.T) {
	tasksDir := t.TempDir()
	msgDir := filepath.Join(tasksDir, "messages")
	if err := os.MkdirAll(msgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	verdict := map[string]string{"verdict": "REJECT", "reason": "bad code"}
	data, _ := json.Marshal(verdict)
	if err := os.WriteFile(filepath.Join(msgDir, "verdict-task.md.json"), data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	vr, ok := ReadVerdictRejection(tasksDir, "task.md")
	if !ok {
		t.Fatal("expected ok=true for case-insensitive REJECT")
	}
	if vr.Reason != "bad code" {
		t.Fatalf("Reason = %q, want %q", vr.Reason, "bad code")
	}
}
