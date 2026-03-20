package identity

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mato/internal/process"
)

func TestGenerateAgentID_Format(t *testing.T) {
	id, err := GenerateAgentID()
	if err != nil {
		t.Fatalf("GenerateAgentID() error: %v", err)
	}
	if len(id) != 8 {
		t.Fatalf("expected 8-char hex string, got %q (len %d)", id, len(id))
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Fatalf("expected valid hex string, got %q: %v", id, err)
	}
}

func TestGenerateAgentID_Unique(t *testing.T) {
	id1, err := GenerateAgentID()
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	id2, err := GenerateAgentID()
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two consecutive calls returned the same ID: %q", id1)
	}
}

func TestIsAgentActive_NonexistentLockFile(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if IsAgentActive(tasksDir, "nonexistent") {
		t.Fatal("expected false for non-existent lock file")
	}
}

func TestIsAgentActive_EmptyAgentID(t *testing.T) {
	if IsAgentActive(t.TempDir(), "") {
		t.Fatal("expected false for empty agent ID")
	}
}

func TestIsAgentActive_LiveProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use our own PID — guaranteed to be alive.
	pid := os.Getpid()
	identity := process.LockIdentity(pid)
	lockFile := filepath.Join(locksDir, "liveagent.pid")
	if err := os.WriteFile(lockFile, []byte(identity), 0o644); err != nil {
		t.Fatal(err)
	}

	if !IsAgentActive(tasksDir, "liveagent") {
		t.Fatal("expected true for live process lock file")
	}
}

func TestIsAgentActive_DeadProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// PID 0 is never a valid user process and will fail the signal check.
	// Use a high PID that almost certainly doesn't exist.
	deadIdentity := fmt.Sprintf("%d:99999", 2147483647)
	lockFile := filepath.Join(locksDir, "deadagent.pid")
	if err := os.WriteFile(lockFile, []byte(deadIdentity), 0o644); err != nil {
		t.Fatal(err)
	}

	if IsAgentActive(tasksDir, "deadagent") {
		t.Fatal("expected false for dead process lock file")
	}
}

func TestIsAgentActive_NoLocksDir(t *testing.T) {
	// tasksDir exists but has no .locks subdirectory
	tasksDir := t.TempDir()
	if IsAgentActive(tasksDir, "anyagent") {
		t.Fatal("expected false when .locks directory does not exist")
	}
}
