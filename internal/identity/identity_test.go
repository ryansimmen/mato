package identity

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"mato/internal/process"
	"mato/internal/testutil"
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

func TestIsAgentActive_RejectsAgentIDWithPathSeparator(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(filepath.Join(locksDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(locksDir, "nested", "agent.pid"), []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "escape.pid"), []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, agentID := range []string{"nested/agent", "../escape"} {
		if IsAgentActive(tasksDir, agentID) {
			t.Fatalf("expected false for agent ID with path separator %q", agentID)
		}
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

func TestIsAgentActive_LegacyPIDOnly(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Legacy format: PID only (no start time)
	if err := os.WriteFile(filepath.Join(locksDir, "legacy.pid"), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsAgentActive(tasksDir, "legacy") {
		t.Fatal("legacy PID-only lock file for live process should be active")
	}

	if err := os.WriteFile(filepath.Join(locksDir, "legacy-dead.pid"), []byte("2147483647"), 0o644); err != nil {
		t.Fatal(err)
	}
	if IsAgentActive(tasksDir, "legacy-dead") {
		t.Fatal("legacy PID-only lock file for dead process should not be active")
	}
}

func TestIsAgentActive_PIDReuseDetected(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("test requires /proc filesystem (Linux)")
	}
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a lock with the current PID but a fabricated start time that
	// does not match the actual process start time. This simulates PID reuse.
	fakeIdentity := fmt.Sprintf("%d:99999999999", os.Getpid())
	if err := os.WriteFile(filepath.Join(locksDir, "reused.pid"), []byte(fakeIdentity), 0o644); err != nil {
		t.Fatal(err)
	}

	if IsAgentActive(tasksDir, "reused") {
		t.Fatal("lock with mismatched start time should detect PID reuse and return false")
	}
}

func TestIsAgentActive_CorruptedPIDFile(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "corrupted.pid"), []byte("not-a-number"), 0o644); err != nil {
		t.Fatal(err)
	}

	if IsAgentActive(tasksDir, "corrupted") {
		t.Fatal("corrupted pid file should not be considered active")
	}
}

func TestIsAgentActive_NegativePID(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "negative.pid"), []byte("-1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if IsAgentActive(tasksDir, "negative") {
		t.Fatal("negative pid should not be considered active")
	}
}

func TestIsAgentActive_EPERMTreatedAsAlive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user (PID 1 returns EPERM only for non-root)")
	}
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// PID 1 (init/systemd) belongs to root; Signal(0) returns EPERM for non-root callers.
	// Use legacy PID-only format since we can't read PID 1's start time as non-root.
	if err := os.WriteFile(filepath.Join(locksDir, "other-user.pid"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !IsAgentActive(tasksDir, "other-user") {
		t.Fatal("PID 1 should be considered active (EPERM means process exists)")
	}
}

// --- CheckAgentActive tests ---

func TestCheckAgentActive_LiveProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "liveagent.pid"), []byte(process.LockIdentity(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	active, err := CheckAgentActive(tasksDir, "liveagent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !active {
		t.Fatal("expected true for live process lock file")
	}
}

func TestCheckAgentActive_DeadProcess(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locksDir, "deadagent.pid"), []byte(fmt.Sprintf("%d:99999", 2147483647)), 0o644); err != nil {
		t.Fatal(err)
	}

	active, err := CheckAgentActive(tasksDir, "deadagent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active {
		t.Fatal("expected false for dead process lock file")
	}
}

func TestCheckAgentActive_MissingLock(t *testing.T) {
	tasksDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tasksDir, ".locks"), 0o755); err != nil {
		t.Fatal(err)
	}

	active, err := CheckAgentActive(tasksDir, "missing")
	if err != nil {
		t.Fatalf("unexpected error for missing lock: %v", err)
	}
	if active {
		t.Fatal("expected false for missing lock file")
	}
}

func TestCheckAgentActive_UnreadableLock(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(locksDir, "unreadable.pid")
	testutil.MakeUnreadablePath(t, lockPath)

	active, err := CheckAgentActive(tasksDir, "unreadable")
	if err == nil {
		t.Fatal("expected error for unreadable lock file")
	}
	if active {
		t.Fatal("expected false for unreadable lock file")
	}
}

func TestDescribeAgentActivity_UnreadableLock(t *testing.T) {
	tasksDir := t.TempDir()
	locksDir := filepath.Join(tasksDir, ".locks")
	if err := os.MkdirAll(locksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(locksDir, "unreadable.pid")
	testutil.MakeUnreadablePath(t, lockPath)

	status, err := DescribeAgentActivity(tasksDir, "unreadable")
	if err == nil {
		t.Fatal("expected error for unreadable lock")
	}
	if status != AgentUnknown {
		t.Fatalf("status = %v, want AgentUnknown", status)
	}
}

func TestCheckAgentActive_EmptyAgentID(t *testing.T) {
	active, err := CheckAgentActive(t.TempDir(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active {
		t.Fatal("expected false for empty agent ID")
	}
}

func TestCheckAgentActive_PathSeparator(t *testing.T) {
	active, err := CheckAgentActive(t.TempDir(), "nested/agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active {
		t.Fatal("expected false for agent ID with path separator")
	}
}
